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

type Config struct {
	ListenAddr    string
	Upstream      string
	LuaScripts    map[string]string // path -> source
	ScriptTimeout time.Duration
}

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

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// LVIsNil checks if a stack value is nil (using negative index).
func LVIsNil(L *lua.LState, idx int) bool {
	v := L.Get(idx)
	return v == lua.LNil
}

// --- Proxy state ---

type ProxyState struct {
	mu       sync.RWMutex
	stats    ProxyStats
	requests uint64
}

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

type LuaAPI struct {
	Request     *http.Request
	Response    *http.Response
	Body        []byte
	Headers     map[string]string
	QueryArgs   map[string]string
	Cancel      func()
	Blocked     bool // true if script called abort or redirect
	UpstreamURL string // configured upstream URL
}

func registerLuaAPI(L *lua.LState, api *LuaAPI) {
	m := L.NewTable()

	// request info
	reqTbl := L.NewTable()
	L.SetField(reqTbl, "method", lua.LString(api.Request.Method))
	L.SetField(reqTbl, "path", lua.LString(api.Request.URL.Path))
	L.SetField(reqTbl, "rawPath", lua.LString(api.Request.URL.RawPath))
	L.SetField(reqTbl, "proto", lua.LString(api.Request.Proto))
	L.SetField(m, "request", reqTbl)

	// headers helpers
	headersFn := L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		L.Push(lua.LString(api.Headers[key]))
		return 1
	})
	L.SetField(m, "getHeader", headersFn)

	queryFn := L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		L.Push(lua.LString(api.QueryArgs[key]))
		return 1
	})
	L.SetField(m, "getQuery", queryFn)

	// response helpers
	respTbl := L.NewTable()
	if api.Response != nil {
		L.SetField(respTbl, "status", lua.LNumber(api.Response.StatusCode))
	} else {
		L.SetField(respTbl, "status", lua.LNumber(0))
	}
	L.SetField(m, "response", respTbl)

	respSetFn := L.NewFunction(func(L *lua.LState) int {
		status := L.CheckInt(1)
		api.Response = &http.Response{StatusCode: status}
		L.Push(lua.LTrue)
		return 1
	})
	L.SetField(m, "setStatus", respSetFn)

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

	// body manipulation
	bodyFn := L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString(string(api.Body)))
		return 1
	})
	L.SetField(m, "getBody", bodyFn)

	bodySetFn := L.NewFunction(func(L *lua.LState) int {
		api.Body = []byte(L.CheckString(1))
		L.Push(lua.LTrue)
		return 1
	})
	L.SetField(m, "setBody", bodySetFn)

	// redirect
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

	// abort / error — usage: proxy.abort(code, msg)
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

	// log helper
	logFn := L.NewFunction(func(L *lua.LState) int {
		msg := L.CheckString(1)
		log.Printf("[lua] %s", msg)
		return 0
	})
	L.SetField(m, "log", logFn)

	// --- Network / connection info ---

	// proxy.clientIP() — resolves real client IP from X-Forwarded-For / X-Real-Ip / RemoteAddr
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

	// proxy.remoteAddr() — raw RemoteAddr of the direct peer
	remoteAddrFn := L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString(api.Request.RemoteAddr))
		return 1
	})
	L.SetField(m, "remoteAddr", remoteAddrFn)

	// proxy.scheme() — "http" or "https"
	schemeFn := L.NewFunction(func(L *lua.LState) int {
		scheme := "http"
		if api.Request.TLS != nil {
			scheme = "https"
		}
		// Respect X-Forwarded-Proto if present
		if xfp := api.Request.Header.Get("X-Forwarded-Proto"); xfp != "" {
			scheme = xfp
		}
		L.Push(lua.LString(scheme))
		return 1
	})
	L.SetField(m, "scheme", schemeFn)

	// proxy.host() — the Host header value
	hostFn := L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString(api.Request.Host))
		return 1
	})
	L.SetField(m, "host", hostFn)

	// proxy.upstreamURL() — the configured upstream URL
	upstreamURLFn := L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString(api.UpstreamURL))
		return 1
	})
	L.SetField(m, "upstreamURL", upstreamURLFn)

	// proxy.setBodyStream(reader) — set body to an io.ReadCloser (advanced)
	bodyStreamFn := L.NewFunction(func(L *lua.LState) int {
		ud := L.CheckUserData(1)
		if reader, ok := ud.Value.(io.ReadCloser); ok {
			buf, err := io.ReadAll(reader)
			reader.Close()
			if err != nil {
				L.Push(lua.LNil)
				L.Push(lua.LString(err.Error()))
				return 2
			}
			api.Body = buf
		}
		L.Push(lua.LTrue)
		return 1
	})
	L.SetField(m, "setBodyStream", bodyStreamFn)

	// json decode
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

	// json encode
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

// valueToLua converts a Go value (from JSON unmarshal) into an LValue.
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

// sliceToLua converts a []interface{} (from JSON unmarshal) into a Lua table.
func sliceToLua(L *lua.LState, vt []interface{}) *lua.LTable {
	arr := L.NewTable()
	for i, item := range vt {
		arr.RawSetInt(i+1, valueToLua(L, item))
	}
	return arr
}

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

	proxy := httputil.NewSingleHostReverseProxy(upstream)

	runner := NewScriptRunner()

	server := &http.Server{
		Addr:         cfg.ListenAddr,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		state.mu.Lock()
		state.requests++
		state.stats.TotalRequests++
		state.mu.Unlock()

		// Build Lua API for this request
		api := &LuaAPI{
			Request:     r,
			Headers:     make(map[string]string),
			QueryArgs:   make(map[string]string),
			UpstreamURL: cfg.Upstream,
		}
		for k, vals := range r.Header {
			api.Headers[k] = strings.Join(vals, ", ")
		}
		for k, vals := range r.URL.Query() {
			api.QueryArgs[k] = strings.Join(vals, ", ")
		}

		// Read body for Lua access
		bodyBytes, _ := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		api.Body = bodyBytes

		// Create a cancellable context for the script
		_, cancel := context.WithCancel(context.Background())
		api.Cancel = cancel

		// Run Lua scripts if any are loaded
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
			// If script blocked (abort/redirect), short-circuit
			if api.Blocked {
				cancel()
				break
			}
		}

		// If script produced a blocked response, send it directly
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

		// Normal proxy: restore body and forward
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

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

		proxy.ModifyResponse = func(resp *http.Response) error {
			// Merge headers set by Lua scripts (even when not short-circuiting)
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

	http.HandleFunc("/__proxy_stats__", statsHandler)

	// Graceful shutdown
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
