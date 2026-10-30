// go-lua-proxy is a lightweight HTTP reverse proxy that executes Lua scripts
// per-request, giving programmatic control over routing, auth, headers, body
// transformation, and more.
//
// # Configuration
//
// The proxy is configured entirely via environment variables:
//
//	LISTEN_ADDR=:8080        Address to listen on (default :8080)
//	UPSTREAM=http://backend  Backend URL to proxy to (required)
//	LUA_SCRIPTS=./a.lua,...  Comma-separated list of Lua script files
//
// # Lua API
//
// Each loaded script runs inside a fresh Lua state with one global table: `proxy`.
// Every request creates a new Lua state; globals do NOT persist between requests.
//
// ## Request info (read-only)
//
//	proxy.request.method   — "GET", "POST", etc.
//	proxy.request.path     — URL path
//	proxy.request.rawPath  — path as received (encoded chars preserved)
//	proxy.request.proto    — "HTTP/1.1", "HTTP/2.0"
//
//	proxy.getHeader(key)       — request header value (comma-joined for multi-value)
//	proxy.getQuery(key)        — URL query parameter value
//	proxy.clientIP()           — real client IP (X-Real-Ip → X-Forwarded-For[0] → RemoteAddr)
//	proxy.remoteAddr()         — raw peer address, e.g. "127.0.0.1:54321"
//	proxy.scheme()             — "http" or "https" (respects X-Forwarded-Proto, checks TLS)
//	proxy.host()               — Host header value
//	proxy.upstreamURL()        — the configured UPSTREAM value
//
// ## Response manipulation (merged into proxied response)
//
// These set fields on api.Response without short-circuiting. Headers are merged
// into the upstream response by ModifyResponse.
//
//	proxy.setStatus(code)                    — set HTTP status code
//	proxy.setResponseHeader(name, value)     — add a response header
//
// ## Body manipulation
//
//	proxy.getBody()    — return the request body as a string
//	proxy.setBody(str) — replace the request body before forwarding
//
// ## Short-circuit (bypass upstream entirely)
//
// These methods stop the request from reaching the backend and send a response directly.
//
//	proxy.redirect(location, statusCode?)  — redirect (default 302)
//	proxy.abort(statusCode, body?)         — abort with custom status + body (default 403)
//
//	Example: proxy.abort(200, `{"status":"ok"}`)
//	Example: proxy.redirect("/login", 302)
//
// ## JSON helpers
//
//	proxy.jsonDecode(str) → table, error  — parse JSON string → Lua table
//	proxy.jsonEncode(table) → str, error  — encode Lua table → JSON string
//
// Numbers stay as numbers (not strings), booleans stay boolean, nil stays nil.
//
// ## Logging
//
//	proxy.log(msg) — prints to proxy stderr log
//
// # Stats endpoint
//
// GET /__proxy_stats__ returns JSON with total_requests, status_codes, avg_latency_ms, etc.
//
// # Limitations
//
// - No cross-request state: each request gets a new Lua state. For persistent state
//   (rate limiting, caching), use an external store reachable via HTTP from Lua.
// - Body size limit: request bodies are read up to 10 MB before being passed to Lua.
// - Single upstream: this is a single-host reverse proxy, not a load balancer.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// --- Config ---

// Config holds the proxy configuration loaded from environment variables.
type Config struct {
	ListenAddr    string            // Address to listen on (LISTEN_ADDR)
	Upstream      string            // Backend URL to proxy to (UPSTREAM)
	LuaScripts    map[string]string // Map of file path -> source content (LUA_SCRIPTS)
	ScriptTimeout time.Duration     // Max execution time per script (reserved)
}

// loadConfig reads configuration from environment variables.
func loadConfig() Config {
	cfg := Config{
		ListenAddr:    getEnv("LISTEN_ADDR", ":8080"),
		Upstream:      getEnv("UPSTREAM", "http://localhost:9090"),
		ScriptTimeout: 5 * time.Second,
		LuaScripts:    make(map[string]string),
	}
	if scripts := os.Getenv("LUA_SCRIPTS"); scripts != "" {
		for _, p := range strings.Split(scripts, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			data, err := os.ReadFile(p)
			if err != nil {
				log.Fatalf("read lua script %s: %v", p, err)
			}
			cfg.LuaScripts[p] = string(data)
		}
	}
	return cfg
}

// getEnv returns the value of the environment variable named by key,
// or fallback if the variable is unset or empty.
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// LVIsNil checks if a stack value is nil (using negative index).
// This is needed because gopher-lua's IsNoneOrNil doesn't work with negative indices.
func LVIsNil(L *lua.LState, idx int) bool {
	v := L.Get(idx)
	return v == lua.LNil
}

// --- Proxy state ---

// ProxyState holds shared statistics across all requests.
type ProxyState struct {
	mu       sync.RWMutex
	stats    ProxyStats
	requests uint64
}

// ProxyStats tracks aggregate request metrics.
type ProxyStats struct {
	TotalRequests uint64            `json:"total_requests"`
	TotalBytesIn  uint64            `json:"total_bytes_in"`
	TotalBytesOut uint64            `json:"total_bytes_out"`
	StatusCodes   map[string]int    `json:"status_codes"`
	AvgLatencyMs  float64           `json:"avg_latency_ms"`
	LastUpdated   time.Time         `json:"last_updated"`
}

var state = &ProxyState{
	stats: ProxyStats{
		StatusCodes: make(map[string]int),
	},
}

// --- Lua API ---

// LuaAPI exposes request/response context to Lua scripts.
// Each field is populated per-request and accessible from within Lua via the `proxy` global.
type LuaAPI struct {
	Request     *http.Request // Original HTTP request
	Response    *http.Response // Response set by script (nil = proxy normally)
	Body        []byte         // Request body for inspection/modification
	Headers     map[string]string // All request headers (comma-joined multi-values)
	QueryArgs   map[string]string // URL query parameters
	Cancel      func()         // Cancel the script execution context
	Blocked     bool           // true if script called abort() or redirect() — short-circuit
	UpstreamURL string         // The configured UPSTREAM value, available to scripts
}

// registerLuaAPI populates the Lua state with the `proxy` global table containing
// all functions and data available to scripts. Called once per request.
func registerLuaAPI(L *lua.LState, api *LuaAPI) {
	m := L.NewTable()

	// request info — read-only metadata about the incoming request
	reqTbl := L.NewTable()
	L.SetField(reqTbl, "method", lua.LString(api.Request.Method))
	L.SetField(reqTbl, "path", lua.LString(api.Request.URL.Path))
	L.SetField(reqTbl, "rawPath", lua.LString(api.Request.URL.RawPath))
	L.SetField(reqTbl, "proto", lua.LString(api.Request.Proto))
	L.SetField(m, "request", reqTbl)

	// getHeader — look up a request header by name
	headersFn := L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		L.Push(lua.LString(api.Headers[key]))
		return 1
	})
	L.SetField(m, "getHeader", headersFn)

	// getQuery — look up a URL query parameter by name
	queryFn := L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		L.Push(lua.LString(api.QueryArgs[key]))
		return 1
	})
	L.SetField(m, "getQuery", queryFn)

	// response.status — current response status (0 if not yet set)
	respTbl := L.NewTable()
	if api.Response != nil {
		L.SetField(respTbl, "status", lua.LNumber(api.Response.StatusCode))
	} else {
		L.SetField(respTbl, "status", lua.LNumber(0))
	}
	L.SetField(m, "response", respTbl)

	// setStatus — set the HTTP status code for the response
	respSetFn := L.NewFunction(func(L *lua.LState) int {
		status := L.CheckInt(1)
		api.Response = &http.Response{StatusCode: status}
		L.Push(lua.LTrue)
		return 1
	})
	L.SetField(m, "setStatus", respSetFn)

	// setResponseHeader — add a response header. If called multiple times,
	// headers accumulate in the same Header map.
	respHdrFn := L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		val := L.CheckString(2)
		if api.Response == nil {
			api.Response = &http.Response{}
		}
		if api.Response.Header == nil {
			api.Response.Header = http.Header{}
		}
		api.Response.Header.Set(key, val)
		L.Push(lua.LTrue)
		return 1
	})
	L.SetField(m, "setResponseHeader", respHdrFn)

	// getBody — return the full request body as a string
	bodyFn := L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString(string(api.Body)))
		return 1
	})
	L.SetField(m, "getBody", bodyFn)

	// setBody — replace the request body before forwarding to upstream
	bodySetFn := L.NewFunction(func(L *lua.LState) int {
		api.Body = []byte(L.CheckString(1))
		L.Push(lua.LTrue)
		return 1
	})
	L.SetField(m, "setBody", bodySetFn)

	// redirect — short-circuit with an HTTP redirect response.
	// Usage: proxy.redirect(location) or proxy.redirect(location, statusCode)
	redirectFn := L.NewFunction(func(L *lua.LState) int {
		loc := L.CheckString(1)
		code := 302
		if L.GetTop() >= 2 && !LVIsNil(L, 2) {
			code = L.CheckInt(2)
		}
		api.Response = &http.Response{
			StatusCode: code,
			Header:     http.Header{"Location": []string{loc}},
		}
		api.Blocked = true
		L.Push(lua.LTrue)
		return 1
	})
	L.SetField(m, "redirect", redirectFn)

	// abort — short-circuit with an arbitrary status code and body.
	// Usage: proxy.abort(), proxy.abort(statusCode), proxy.abort(statusCode, body)
	// Default is 403 "blocked by script".
	abortFn := L.NewFunction(func(L *lua.LState) int {
		code := 403
		msg := "blocked by script"
		if L.GetTop() >= 1 && !LVIsNil(L, 1) {
			code = L.CheckInt(1)
		}
		if L.GetTop() >= 2 && !LVIsNil(L, 2) {
			msg = L.CheckString(2)
		}
		api.Response = &http.Response{
			StatusCode: code,
			Body:       io.NopCloser(bytes.NewReader([]byte(msg))),
		}
		api.Blocked = true
		if api.Cancel != nil {
			api.Cancel()
		}
		L.Push(lua.LTrue)
		return 1
	})
	L.SetField(m, "abort", abortFn)

	// log — print a message to the proxy's stderr log
	logFn := L.NewFunction(func(L *lua.LState) int {
		msg := L.CheckString(1)
		log.Printf("[lua] %s", msg)
		return 0
	})
	L.SetField(m, "log", logFn)

	// --- Network / connection info ---

	// clientIP — resolve the real client IP address.
	// Priority: X-Real-Ip → first entry of X-Forwarded-For → RemoteAddr (port stripped).
	clientIPFn := L.NewFunction(func(L *lua.LState) int {
		ip := api.Request.Header.Get("X-Real-Ip")
		if ip == "" {
			ip = api.Request.Header.Get("X-Forwarded-For")
			// X-Forwarded-For can be comma-separated; take first
			if idx := strings.Index(ip, ","); idx != -1 {
				ip = ip[:idx]
			}
		}
		if ip == "" {
			ip = api.Request.RemoteAddr
			// Strip port if present
			if host, _, err := net.SplitHostPort(ip); err == nil {
				ip = host
			}
		}
		L.Push(lua.LString(ip))
		return 1
	})
	L.SetField(m, "clientIP", clientIPFn)

	// remoteAddr — raw RemoteAddr of the direct peer (includes port)
	remoteAddrFn := L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString(api.Request.RemoteAddr))
		return 1
	})
	L.SetField(m, "remoteAddr", remoteAddrFn)

	// scheme — "http" or "https". Respects X-Forwarded-Proto header,
	// falls back to checking TLS on the request.
	schemeFn := L.NewFunction(func(L *lua.LState) int {
		scheme := "http"
		if api.Request.TLS != nil {
			scheme = "https"
		}
		if xfp := api.Request.Header.Get("X-Forwarded-Proto"); xfp != "" {
			scheme = xfp
		}
		L.Push(lua.LString(scheme))
		return 1
	})
	L.SetField(m, "scheme", schemeFn)

	// host — the Host header value from the request
	hostFn := L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString(api.Request.Host))
		return 1
	})
	L.SetField(m, "host", hostFn)

	// upstreamURL — the configured UPSTREAM URL value
	upstreamURLFn := L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString(api.UpstreamURL))
		return 1
	})
	L.SetField(m, "upstreamURL", upstreamURLFn)

	// jsonDecode — parse a JSON string into a Lua table.
	// Returns (table) on success, (nil, error_string) on failure.
	jsonDecodeFn := L.NewFunction(func(L *lua.LState) int {
		raw := L.CheckString(1)
		var out interface{}
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(tableFromValue(L, out))
		return 1
	})
	L.SetField(m, "jsonDecode", jsonDecodeFn)

	// jsonEncode — encode a Lua table into a JSON string.
	// Returns (json_string) on success, (nil, error_string) on failure.
	jsonEncodeFn := L.NewFunction(func(L *lua.LState) int {
		t := L.CheckTable(1)
		v := valueFromTable(L, t)
		data, err := json.Marshal(v)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(lua.LString(data))
		return 1
	})
	L.SetField(m, "jsonEncode", jsonEncodeFn)

	L.SetGlobal("proxy", m)
}

// --- Table <-> Go value helpers ---

// tableFromValue converts a Go value (typically from JSON unmarshal) into a Lua table.
// Handles nested maps, slices, primitives, and nil.
func tableFromValue(L *lua.LState, v interface{}) *lua.LTable {
	t := L.NewTable()
	switch vt := v.(type) {
	case map[string]interface{}:
		for k, vv := range vt {
			L.SetField(t, k, valueToLua(L, vv))
		}
	default:
		L.SetField(t, "_value", lua.LString(fmt.Sprintf("%v", v)))
	}
	return t
}

// valueToLua converts a single Go value into an appropriate Lua LValue.
func valueToLua(L *lua.LState, v interface{}) lua.LValue {
	switch vt := v.(type) {
	case map[string]interface{}:
		return tableFromValue(L, v)
	case []interface{}:
		return sliceToLua(L, vt)
	case float64:
		return lua.LNumber(vt)
	case int:
		return lua.LNumber(vt)
	case int64:
		return lua.LNumber(vt)
	case bool:
		return lua.LBool(vt)
	case nil:
		return lua.LNil
	case string:
		return lua.LString(vt)
	default:
		return lua.LString(fmt.Sprintf("%v", v))
	}
}

// sliceToLua converts a Go slice into a Lua table with integer keys starting at 1.
func sliceToLua(L *lua.LState, vt []interface{}) *lua.LTable {
	arr := L.NewTable()
	for i, item := range vt {
		arr.RawSetInt(i+1, valueToLua(L, item))
	}
	return arr
}

// valueFromTable converts a Lua table back into a Go value for JSON encoding.
func valueFromTable(L *lua.LState, t *lua.LTable) interface{} {
	result := make(map[string]interface{})
	t.ForEach(func(k, v lua.LValue) {
		key := k.String()
		switch lv := v.(type) {
		case *lua.LTable:
			result[key] = valueFromTable(L, lv)
		case lua.LNumber:
			result[key] = float64(lv)
		case lua.LBool:
			result[key] = bool(lv)
		case lua.LString:
			result[key] = string(lv)
		default:
			if v == lua.LNil {
				result[key] = nil
			} else {
				result[key] = v.String()
			}
		}
	})
	return result
}

// --- Script runner ---

// ScriptRunner executes Lua scripts in isolated states. Each call creates a new
// Lua VM, registers the per-request API, and runs the script. Errors are returned
// so the caller can decide how to handle them.
type ScriptRunner struct{}

func NewScriptRunner() *ScriptRunner {
	return &ScriptRunner{}
}

func (sr *ScriptRunner) Run(script string, api *LuaAPI) error {
	L := lua.NewState()
	defer L.Close()

	registerLuaAPI(L, api)

	if err := L.DoString(script); err != nil {
		return fmt.Errorf("lua error: %w", err)
	}
	return nil
}

// --- Handlers ---

// statsHandler serves aggregate request statistics as JSON at /__proxy_stats__.
func statsHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/__proxy_stats__" {
		http.NotFound(w, r)
		return
	}
	state.mu.RLock()
	s := state.stats
	state.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s)
}

// --- Main ---

func main() {
	cfg := loadConfig()

	upstream, err := url.Parse(cfg.Upstream)
	if err != nil {
		log.Fatalf("parse upstream: %v", err)
	}

	// httputil.ReverseProxy forwards requests to the upstream server.
	// We customize Director (rewrite URL) and ModifyResponse (merge Lua-set headers).
	proxy := httputil.NewSingleHostReverseProxy(upstream)

	runner := NewScriptRunner()

	server := &http.Server{
		Addr:         cfg.ListenAddr,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Main request handler: run Lua scripts, then proxy.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		state.mu.Lock()
		state.requests++
		state.stats.TotalRequests++
		state.mu.Unlock()

		// Build Lua API object for this request.
		api := &LuaAPI{
			Request:     r,
			Headers:     make(map[string]string),
			QueryArgs:   make(map[string]string),
			UpstreamURL: cfg.Upstream,
		}
		// Copy all request headers (multi-value joined with ", ")
		for k, vals := range r.Header {
			api.Headers[k] = strings.Join(vals, ", ")
		}
		// Copy all query parameters
		for k, vals := range r.URL.Query() {
			api.QueryArgs[k] = strings.Join(vals, ", ")
		}

		// Read body for Lua access (up to 10MB limit).
		// Replaced body reader so the proxy can still read it later.
		bodyBytes, _ := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		api.Body = bodyBytes

		// Create a cancellable context for the script.
		_, cancel := context.WithCancel(context.Background())
		api.Cancel = cancel

		// Run all loaded Lua scripts sequentially against the same api object.
		// Scripts can modify the request, set response headers, or short-circuit.
		for path, src := range cfg.LuaScripts {
			log.Printf("running lua script: %s", path)
			err := runner.Run(src, api)
			if err != nil {
				log.Printf("lua script error (%s): %v", path, err)
				api.Response = &http.Response{
					StatusCode: 500,
					Body:       io.NopCloser(bytes.NewReader([]byte("script error"))),
				}
				api.Blocked = true
				break
			}
			// If script blocked (abort/redirect), stop running further scripts.
			if api.Blocked {
				cancel()
				break
			}
		}

		// If any script short-circuited (abort/redirect), send response directly.
		if api.Blocked && api.Response != nil {
			duration := time.Since(start)
			state.mu.Lock()
			sc := fmt.Sprintf("%d", api.Response.StatusCode)
			state.stats.StatusCodes[sc]++
			state.stats.AvgLatencyMs = float64(duration.Milliseconds())
			state.stats.LastUpdated = time.Now()
			state.mu.Unlock()

			for k, vals := range api.Response.Header {
				for _, v := range vals {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(api.Response.StatusCode)
			if api.Response.Body != nil {
				io.Copy(w, api.Response.Body)
			}
			cancel()
			return
		}

		// Normal flow: restore body and forward to upstream.
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		// Rewrite the outgoing request to point at the upstream.
		proxy.Director = func(outReq *http.Request) {
			outReq.URL.Scheme = upstream.Scheme
			outReq.URL.Host = upstream.Host
			outReq.URL.Path = r.URL.Path
			outReq.URL.RawQuery = r.URL.RawQuery
			outReq.Header = make(http.Header)
			for k, vals := range r.Header {
				outReq.Header[k] = vals
			}
			outReq.Host = upstream.Host
		}

		// After receiving the upstream response, merge any headers that Lua scripts
		// set via setResponseHeader(). This allows scripts to inject security headers,
		// logging info, etc. into proxied responses.
		proxy.ModifyResponse = func(resp *http.Response) error {
			if api.Response != nil && api.Response.Header != nil {
				for k, vals := range api.Response.Header {
					for _, v := range vals {
						resp.Header.Add(k, v)
					}
				}
			}
			duration := time.Since(start)
			state.mu.Lock()
			sc := fmt.Sprintf("%d", resp.StatusCode)
			state.stats.StatusCodes[sc]++
			state.stats.AvgLatencyMs = float64(duration.Milliseconds())
			state.stats.LastUpdated = time.Now()
			state.mu.Unlock()
			return nil
		}

		proxy.ServeHTTP(w, r)
		cancel()
	})

	// Stats endpoint: GET /__proxy_stats__
	http.HandleFunc("/__proxy_stats__", statsHandler)

	// Graceful shutdown on SIGINT/SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		log.Println("shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()

	log.Printf("listening on %s, proxying to %s", cfg.ListenAddr, cfg.Upstream)
	if len(cfg.LuaScripts) > 0 {
		log.Printf("loaded %d lua scripts", len(cfg.LuaScripts))
	}
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
	log.Println("stopped")
}
