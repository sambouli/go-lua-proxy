package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// --- Config tests ---

func TestLoadConfigDefaults(t *testing.T) {
	os.Unsetenv("LISTEN_ADDR")
	os.Unsetenv("UPSTREAM")
	os.Unsetenv("LUA_SCRIPTS")
	cfg := loadConfig()
	if cfg.ListenAddr != ":8080" {
		t.Errorf("expected :8080, got %s", cfg.ListenAddr)
	}
	if cfg.Upstream != "http://localhost:9090" {
		t.Errorf("expected http://localhost:9090, got %s", cfg.Upstream)
	}
	if cfg.ScriptTimeout != 5*time.Second {
		t.Errorf("expected 5s, got %v", cfg.ScriptTimeout)
	}
}

func TestLoadConfigEnvVars(t *testing.T) {
	os.Setenv("LISTEN_ADDR", ":9999")
	os.Setenv("UPSTREAM", "http://example.com:3000")
	defer func() {
		os.Unsetenv("LISTEN_ADDR")
		os.Unsetenv("UPSTREAM")
	}()
	cfg := loadConfig()
	if cfg.ListenAddr != ":9999" {
		t.Errorf("expected :9999, got %s", cfg.ListenAddr)
	}
	if cfg.Upstream != "http://example.com:3000" {
		t.Errorf("expected http://example.com:3000, got %s", cfg.Upstream)
	}
}

func TestGetEnvFallback(t *testing.T) {
	os.Unsetenv("NONEXISTENT_KEY_12345")
	v := getEnv("NONEXISTENT_KEY_12345", "fallback_val")
	if v != "fallback_val" {
		t.Errorf("expected fallback_val, got %s", v)
	}
}

func TestGetEnvOverride(t *testing.T) {
	os.Setenv("TEST_ENV_VAR_XYZ", "actual_value")
	defer os.Unsetenv("TEST_ENV_VAR_XYZ")
	v := getEnv("TEST_ENV_VAR_XYZ", "fallback")
	if v != "actual_value" {
		t.Errorf("expected actual_value, got %s", v)
	}
}

// --- Lua API tests ---

func TestRegisterLuaAPI_BasicFields(t *testing.T) {
	req, _ := http.NewRequest("POST", "/test/path?foo=bar&baz=qux", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Custom", "custom-val")

	api := &LuaAPI{
		Request:   req,
		Headers:   make(map[string]string),
		QueryArgs: make(map[string]string),
	}
	for k, vals := range req.Header {
		api.Headers[k] = strings.Join(vals, ", ")
	}
	for k, vals := range req.URL.Query() {
		api.QueryArgs[k] = strings.Join(vals, ", ")
	}

	L := newTestLuaState()
	registerLuaAPI(L, api)

	// Check proxy.request.method
	m := L.GetGlobal("proxy")
	reqTbl := L.GetField(m, "request").(*lua.LTable)
	method := L.GetField(reqTbl, "method").(lua.LString)
	if string(method) != "POST" {
		t.Errorf("expected POST, got %s", method)
	}

	path := L.GetField(reqTbl, "path").(lua.LString)
	if string(path) != "/test/path" {
		t.Errorf("expected /test/path, got %s", path)
	}
}

func TestLuaAPI_GetHeader(t *testing.T) {
	req, _ := http.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer token123")

	api := &LuaAPI{
		Request: req,
		Headers: map[string]string{
			"Authorization": "Bearer token123",
		},
	}

	L := newTestLuaState()
	registerLuaAPI(L, api)

	// Call proxy.getHeader("Authorization")
	getHdrFn := L.GetField(L.GetGlobal("proxy"), "getHeader").(*lua.LFunction)
	L.Push(getHdrFn)
	L.Push(lua.LString("Authorization"))
	if err := L.PCall(1, lua.MultRet, nil); err != nil {
		t.Fatal(err)
	}
	result := L.ToString(-1)
	if result != "Bearer token123" {
		t.Errorf("expected 'Bearer token123', got '%s'", result)
	}
	L.Pop(1)
}

func TestLuaAPI_GetQuery(t *testing.T) {
	req, _ := http.NewRequest("GET", "/search?q=hello&page=2", nil)

	api := &LuaAPI{
		Request:   req,
		QueryArgs: map[string]string{"q": "hello", "page": "2"},
	}

	L := newTestLuaState()
	registerLuaAPI(L, api)

	qFn := L.GetField(L.GetGlobal("proxy"), "getQuery").(*lua.LFunction)
	L.Push(qFn)
	L.Push(lua.LString("q"))
	if err := L.PCall(1, lua.MultRet, nil); err != nil {
		t.Fatal(err)
	}
	result := L.ToString(-1)
	if result != "hello" {
		t.Errorf("expected 'hello', got '%s'", result)
	}
	L.Pop(1)
}

func TestLuaAPI_SetStatus(t *testing.T) {
	api := &LuaAPI{Request: httptest.NewRequest("GET", "/", nil)}

	L := newTestLuaState()
	registerLuaAPI(L, api)

	setFn := L.GetField(L.GetGlobal("proxy"), "setStatus").(*lua.LFunction)
	L.Push(setFn)
	L.Push(lua.LNumber(201))
	if err := L.PCall(1, 1, nil); err != nil {
		t.Fatal(err)
	}
	L.Pop(1)

	if api.Response == nil {
		t.Fatal("expected Response to be set")
	}
	if api.Response.StatusCode != 201 {
		t.Errorf("expected 201, got %d", api.Response.StatusCode)
	}
}

func TestLuaAPI_SetResponseHeader(t *testing.T) {
	api := newTestAPI()

	L := newTestLuaState()
	registerLuaAPI(L, api)

	setFn := L.GetField(L.GetGlobal("proxy"), "setResponseHeader").(*lua.LFunction)
	L.Push(setFn)
	L.Push(lua.LString("X-Proxy"))
	L.Push(lua.LString("filtered"))
	if err := L.PCall(2, 1, nil); err != nil {
		t.Fatal(err)
	}
	L.Pop(1)

	if api.Response == nil || api.Response.Header == nil {
		t.Fatal("expected Header to be set")
	}
	if api.Response.Header.Get("X-Proxy") != "filtered" {
		t.Errorf("expected X-Proxy=filtered, got %s", api.Response.Header.Get("X-Proxy"))
	}
}

func TestLuaAPI_GetBody(t *testing.T) {
	api := newTestAPI()
	api.Body = []byte(`{"key":"value"}`)

	L := newTestLuaState()
	registerLuaAPI(L, api)

	bodyFn := L.GetField(L.GetGlobal("proxy"), "getBody").(*lua.LFunction)
	L.Push(bodyFn)
	if err := L.PCall(0, 1, nil); err != nil {
		t.Fatal(err)
	}
	result := L.ToString(-1)
	if result != `{"key":"value"}` {
		t.Errorf("expected '{\"key\":\"value\"}', got '%s'", result)
	}
	L.Pop(1)
}

func TestLuaAPI_SetBody(t *testing.T) {
	api := newTestAPI()
	api.Body = []byte("original")

	L := newTestLuaState()
	registerLuaAPI(L, api)

	setFn := L.GetField(L.GetGlobal("proxy"), "setBody").(*lua.LFunction)
	L.Push(setFn)
	L.Push(lua.LString("modified body"))
	if err := L.PCall(1, 1, nil); err != nil {
		t.Fatal(err)
	}
	L.Pop(1)

	if !bytes.Equal(api.Body, []byte("modified body")) {
		t.Errorf("expected 'modified body', got %s", api.Body)
	}
}

func TestLuaAPI_Redirect_Default(t *testing.T) {
	api := newTestAPI()

	L := newTestLuaState()
	registerLuaAPI(L, api)

	fn := L.GetField(L.GetGlobal("proxy"), "redirect").(*lua.LFunction)
	L.Push(fn)
	L.Push(lua.LString("http://new.example.com"))
	if err := L.PCall(1, 1, nil); err != nil {
		t.Fatal(err)
	}
	L.Pop(1)

	if api.Response == nil || api.Response.StatusCode != 302 {
		t.Errorf("expected status 302, got %v", api.Response)
	}
	if api.Response.Header.Get("Location") != "http://new.example.com" {
		t.Errorf("expected Location header, got %s", api.Response.Header.Get("Location"))
	}
}

func TestLuaAPI_Redirect_CustomCode(t *testing.T) {
	api := newTestAPI()

	L := newTestLuaState()
	registerLuaAPI(L, api)

	fn := L.GetField(L.GetGlobal("proxy"), "redirect").(*lua.LFunction)
	L.Push(fn)
	L.Push(lua.LString("http://new.example.com"))
	L.Push(lua.LNumber(301))
	if err := L.PCall(2, 1, nil); err != nil {
		t.Fatal(err)
	}
	L.Pop(1)

	if api.Response.StatusCode != 301 {
		t.Errorf("expected 301, got %d", api.Response.StatusCode)
	}
}

func TestLuaAPI_Abort_Default(t *testing.T) {
	api := newTestAPI()

	L := newTestLuaState()
	registerLuaAPI(L, api)

	fn := L.GetField(L.GetGlobal("proxy"), "abort").(*lua.LFunction)
	L.Push(fn)
	if err := L.PCall(0, 1, nil); err != nil {
		t.Fatal(err)
	}
	L.Pop(1)

	if api.Response == nil {
		t.Fatal("expected Response on abort")
	}
	if api.Response.StatusCode != 403 {
		t.Errorf("expected 403, got %d", api.Response.StatusCode)
	}
}

func TestLuaAPI_Abort_Custom(t *testing.T) {
	api := newTestAPI()

	L := newTestLuaState()
	registerLuaAPI(L, api)

	fn := L.GetField(L.GetGlobal("proxy"), "abort").(*lua.LFunction)
	L.Push(fn)
	L.Push(lua.LNumber(404)) // code
	L.Push(lua.LString("not found")) // msg
	if err := L.PCall(2, 1, nil); err != nil {
		t.Fatal(err)
	}
	L.Pop(1)

	if api.Response.StatusCode != 404 {
		t.Errorf("expected 404, got %d", api.Response.StatusCode)
	}
}

func TestLuaAPI_Log(t *testing.T) {
	api := newTestAPI()
	L := newTestLuaState()
	registerLuaAPI(L, api)

	logFn := L.GetField(L.GetGlobal("proxy"), "log").(*lua.LFunction)
	L.Push(logFn)
	L.Push(lua.LString("test log message"))
	if err := L.PCall(1, 0, nil); err != nil {
		t.Fatal(err)
	}
}

func TestLuaAPI_JSONDecode(t *testing.T) {
	api := newTestAPI()
	L := newTestLuaState()
	registerLuaAPI(L, api)

	fn := L.GetField(L.GetGlobal("proxy"), "jsonDecode").(*lua.LFunction)
	L.Push(fn)
	L.Push(lua.LString(`{"name":"test","count":42,"items":[1,2,3]}`))
	if err := L.PCall(1, 1, nil); err != nil {
		t.Fatal(err)
	}

	result := L.ToTable(-1)
	nameVal := L.GetField(result, "name")
	if nameVal.String() != "test" {
		t.Errorf("expected 'test', got '%s'", nameVal)
	}
	countVal := L.GetField(result, "count")
	if float64(countVal.(lua.LNumber)) != 42 {
		t.Errorf("expected 42, got %v", countVal)
	}
	itemsArr := L.GetField(result, "items").(*lua.LTable)
	first := itemsArr.RawGetInt(1).(lua.LNumber)
	if float64(first) != 1 {
		t.Errorf("expected items[1]=1, got %v", first)
	}
	L.Pop(1)
}

func TestLuaAPI_JSONDecodeError(t *testing.T) {
	api := newTestAPI()
	L := newTestLuaState()
	registerLuaAPI(L, api)

	fn := L.GetField(L.GetGlobal("proxy"), "jsonDecode").(*lua.LFunction)
	L.Push(fn)
	L.Push(lua.LString("not json"))
	if err := L.PCall(1, 2, nil); err != nil {
		t.Fatal(err)
	}
	errVal := L.ToString(-1)
	if errVal == "" {
		t.Error("expected error message for invalid JSON")
	}
	L.Pop(2)
}

func TestLuaAPI_JSONEncode(t *testing.T) {
	api := newTestAPI()
	L := newTestLuaState()
	registerLuaAPI(L, api)

	fn := L.GetField(L.GetGlobal("proxy"), "jsonEncode").(*lua.LFunction)
	L.Push(fn)

	// Build a table to encode
	tbl := L.NewTable()
	L.SetField(tbl, "name", lua.LString("alice"))
	L.SetField(tbl, "active", lua.LBool(true))
	L.SetField(tbl, "score", lua.LNumber(9.5))
	L.Push(tbl)

	if err := L.PCall(1, 1, nil); err != nil {
		t.Fatal(err)
	}

	result := L.ToString(-1)
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatalf("invalid JSON output: %v", err)
	}
	if decoded["name"] != "alice" {
		t.Errorf("expected alice, got %v", decoded["name"])
	}
	L.Pop(1)
}

// --- ScriptRunner tests ---

func TestScriptRunner_RunSimple(t *testing.T) {
	api := &LuaAPI{
		Request: httptest.NewRequest("GET", "/test", nil),
		Body:    []byte("hello"),
	}
	runner := NewScriptRunner()
	err := runner.Run(`
		proxy.log("hello from lua")
		proxy.setStatus(200)
	`, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if api.Response == nil || api.Response.StatusCode != 200 {
		t.Error("expected status 200 from script")
	}
}

func TestScriptRunner_RunSyntaxError(t *testing.T) {
	api := &LuaAPI{Request: httptest.NewRequest("GET", "/", nil)}
	runner := NewScriptRunner()
	err := runner.Run(`this is not valid lua {{{`, api)
	if err == nil {
		t.Fatal("expected error for invalid lua syntax")
	}
}

func TestScriptRunner_RedirectFromScript(t *testing.T) {
	api := &LuaAPI{Request: httptest.NewRequest("GET", "/old", nil)}
	runner := NewScriptRunner()
	err := runner.Run(`
		proxy.redirect("/new", 301)
	`, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if api.Response == nil || api.Response.StatusCode != 301 {
		t.Error("expected redirect 301")
	}
	if api.Response.Header.Get("Location") != "/new" {
		t.Errorf("expected Location=/new, got %s", api.Response.Header.Get("Location"))
	}
}

func TestScriptRunner_ModifyBody(t *testing.T) {
	api := &LuaAPI{
		Request: httptest.NewRequest("POST", "/echo", bytes.NewReader([]byte("orig"))),
		Body:    []byte("orig"),
	}
	runner := NewScriptRunner()
	err := runner.Run(`
		local body = proxy.getBody()
		proxy.setBody(body .. "_appended")
	`, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(api.Body, []byte("orig_appended")) {
		t.Errorf("expected 'orig_appended', got %s", api.Body)
	}
}

func TestScriptRunner_JSONRoundTrip(t *testing.T) {
	api := &LuaAPI{Request: httptest.NewRequest("GET", "/", nil)}
	runner := NewScriptRunner()
	err := runner.Run(`
		local data = proxy.jsonDecode('{"a":1,"b":[10,20]}')
		data.c = true
		local encoded = proxy.jsonEncode(data)
	`, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- Stats handler tests ---

func TestStatsHandler(t *testing.T) {
	// Pre-seed state
	state.mu.Lock()
	state.stats.TotalRequests = 5
	state.stats.StatusCodes["200"] = 3
	state.stats.StatusCodes["404"] = 2
	state.mu.Unlock()

	req := httptest.NewRequest("GET", "/__proxy_stats__", nil)
	rec := httptest.NewRecorder()
	statsHandler(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	var stats ProxyStats
	if err := json.NewDecoder(rec.Body).Decode(&stats); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if stats.TotalRequests != 5 {
		t.Errorf("expected 5 requests, got %d", stats.TotalRequests)
	}
	if stats.StatusCodes["200"] != 3 {
		t.Errorf("expected 3 x 200, got %d", stats.StatusCodes["200"])
	}
}

func TestStatsHandler_NotFound(t *testing.T) {
	req := httptest.NewRequest("GET", "/some_other_path", nil)
	rec := httptest.NewRecorder()
	statsHandler(rec, req)
	if rec.Code != 404 {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

// --- Table conversion tests ---

func TestTableFromValue_Map(t *testing.T) {
	L := newTestLuaState()
	input := map[string]interface{}{"name": "test", "count": 42}
	tbl := tableFromValue(L, input)

	name := L.GetField(tbl, "name").(lua.LString)
	if string(name) != "test" {
		t.Errorf("expected 'test', got '%s'", name)
	}
	count := L.GetField(tbl, "count").(lua.LNumber)
	if float64(count) != 42 {
		t.Errorf("expected 42, got %v", count)
	}
}

func TestTableFromValue_Slice(t *testing.T) {
	L := newTestLuaState()
	input := []interface{}{"a", "b", "c"}
	tbl := sliceToLua(L, input)

	for i, expected := range []string{"a", "b", "c"} {
		val := tbl.RawGetInt(i + 1).(lua.LString)
		if string(val) != expected {
			t.Errorf("index %d: expected %s, got %s", i+1, expected, val)
		}
	}
}

func TestTableFromValue_NestedMap(t *testing.T) {
	L := newTestLuaState()
	input := map[string]interface{}{
		"user": map[string]interface{}{
			"name": "bob",
			"tags": []interface{}{"admin", "verified"},
		},
	}
	tbl := tableFromValue(L, input)

	userTbl := L.GetField(tbl, "user").(*lua.LTable)
	name := L.GetField(userTbl, "name").(lua.LString)
	if string(name) != "bob" {
		t.Errorf("expected 'bob', got '%s'", name)
	}
}

func TestValueFromTable(t *testing.T) {
	L := newTestLuaState()
	tbl := L.NewTable()
	L.SetField(tbl, "str", lua.LString("hello"))
	L.SetField(tbl, "num", lua.LNumber(42))
	L.SetField(tbl, "bool", lua.LBool(true))
	L.SetField(tbl, "nil_val", lua.LNil)

	result := valueFromTable(L, tbl).(map[string]interface{})

	if result["str"] != "hello" {
		t.Errorf("str: expected 'hello', got %v", result["str"])
	}
	if result["num"] != float64(42) {
		t.Errorf("num: expected 42, got %v", result["num"])
	}
	if result["bool"] != true {
		t.Errorf("bool: expected true, got %v", result["bool"])
	}
	if result["nil_val"] != nil {
		t.Errorf("nil_val: expected nil, got %v", result["nil_val"])
	}
}

// --- Full integration: proxy with Lua script ---

func TestFullProxy_NoScript(t *testing.T) {
	// Start a fake upstream
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("upstream response"))
	}))
	defer upstream.Close()

	// Temporarily override config loading for test
	origUpstream := UpstreamForTest
	origScripts := LuaScriptsForTest
	UpstreamForTest = upstream.URL
	LuaScriptsForTest = map[string]string{}
	defer func() {
		UpstreamForTest = origUpstream
		LuaScriptsForTest = origScripts
	}()

	handler := buildTestHandler()

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "upstream response" {
		t.Errorf("expected 'upstream response', got '%s'", rec.Body.String())
	}
}

func TestFullProxy_WithRedirectScript(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("should not reach upstream"))
	}))
	defer upstream.Close()

	origUpstream := UpstreamForTest
	origScripts := LuaScriptsForTest
	UpstreamForTest = upstream.URL
	LuaScriptsForTest = map[string]string{
		"test.lua": `proxy.redirect("/blocked", 403)`,
	}
	defer func() {
		UpstreamForTest = origUpstream
		LuaScriptsForTest = origScripts
	}()

	handler := buildTestHandler()

	req := httptest.NewRequest("GET", "/anything", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 403 {
		t.Errorf("expected 403, got %d", rec.Code)
	}
	// Should NOT have reached upstream
}

func TestFullProxy_BodyModification(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(200)
		w.Write([]byte("received: " + string(body)))
	}))
	defer upstream.Close()

	origUpstream := UpstreamForTest
	origScripts := LuaScriptsForTest
	UpstreamForTest = upstream.URL
	LuaScriptsForTest = map[string]string{
		"test.lua": `
			local body = proxy.getBody()
			proxy.setBody("[filtered] " .. body)
		`,
	}
	defer func() {
		UpstreamForTest = origUpstream
		LuaScriptsForTest = origScripts
	}()

	handler := buildTestHandler()

	req := httptest.NewRequest("POST", "/submit", bytes.NewReader([]byte("secret")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	expected := "received: [filtered] secret"
	if rec.Body.String() != expected {
		t.Errorf("expected '%s', got '%s'", expected, rec.Body.String())
	}
}

func TestFullProxy_LuaErrorReturns500(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("upstream"))
	}))
	defer upstream.Close()

	origUpstream := UpstreamForTest
	origScripts := LuaScriptsForTest
	UpstreamForTest = upstream.URL
	LuaScriptsForTest = map[string]string{
		"bad.lua": `syntax error here {{{`,
	}
	defer func() {
		UpstreamForTest = origUpstream
		LuaScriptsForTest = origScripts
	}()

	handler := buildTestHandler()

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 500 {
		t.Errorf("expected 500 on lua error, got %d", rec.Code)
	}
}

// --- Integration: client IP / network info API ---

func TestLuaAPI_ClientIP(t *testing.T) {
	api := newTestAPI()

	L := newTestLuaState()
	registerLuaAPI(L, api)

	fn := L.GetField(L.GetGlobal("proxy"), "clientIP").(*lua.LFunction)
	L.Push(fn)
	if err := L.PCall(0, 1, nil); err != nil {
		t.Fatal(err)
	}
	result := L.ToString(-1)
	// Without forwarded headers, should be empty (httptest.NewRequest has no RemoteAddr)
	if result == "" {
		t.Log("clientIP returned empty — expected when no X-Forwarded-For/X-Real-Ip/RemoteAddr")
	}
	L.Pop(1)
}

func TestLuaAPI_ClientIP_WithForwarded(t *testing.T) {
	api := &LuaAPI{
		Request:     httptest.NewRequest("GET", "/test", nil),
		Headers:     make(map[string]string),
		QueryArgs:   make(map[string]string),
		UpstreamURL: "http://localhost:9090",
	}
	// Simulate X-Forwarded-For from a real proxy
	api.Request.Header.Set("X-Forwarded-For", "203.0.113.42, 10.0.0.1")

	L := newTestLuaState()
	registerLuaAPI(L, api)

	fn := L.GetField(L.GetGlobal("proxy"), "clientIP").(*lua.LFunction)
	L.Push(fn)
	if err := L.PCall(0, 1, nil); err != nil {
		t.Fatal(err)
	}
	result := L.ToString(-1)
	if result != "203.0.113.42" {
		t.Errorf("expected '203.0.113.42', got '%s'", result)
	}
	L.Pop(1)
}

func TestLuaAPI_ClientIP_XRealIp(t *testing.T) {
	api := &LuaAPI{
		Request:     httptest.NewRequest("GET", "/test", nil),
		Headers:     make(map[string]string),
		QueryArgs:   make(map[string]string),
		UpstreamURL: "http://localhost:9090",
	}
	api.Request.Header.Set("X-Real-Ip", "198.51.100.7")

	L := newTestLuaState()
	registerLuaAPI(L, api)

	fn := L.GetField(L.GetGlobal("proxy"), "clientIP").(*lua.LFunction)
	L.Push(fn)
	if err := L.PCall(0, 1, nil); err != nil {
		t.Fatal(err)
	}
	result := L.ToString(-1)
	if result != "198.51.100.7" {
		t.Errorf("expected '198.51.100.7', got '%s'", result)
	}
	L.Pop(1)
}

func TestLuaAPI_RemoteAddr(t *testing.T) {
	api := &LuaAPI{
		Request:     httptest.NewRequest("GET", "/test", nil),
		Headers:     make(map[string]string),
		QueryArgs:   make(map[string]string),
		UpstreamURL: "http://localhost:9090",
	}
	api.Request.RemoteAddr = "127.0.0.1:54321"

	L := newTestLuaState()
	registerLuaAPI(L, api)

	fn := L.GetField(L.GetGlobal("proxy"), "remoteAddr").(*lua.LFunction)
	L.Push(fn)
	if err := L.PCall(0, 1, nil); err != nil {
		t.Fatal(err)
	}
	result := L.ToString(-1)
	if result != "127.0.0.1:54321" {
		t.Errorf("expected '127.0.0.1:54321', got '%s'", result)
	}
	L.Pop(1)
}

func TestLuaAPI_Scheme_HTTPS(t *testing.T) {
	api := &LuaAPI{
		Request:     httptest.NewRequest("GET", "/test", nil),
		Headers:     make(map[string]string),
		QueryArgs:   make(map[string]string),
		UpstreamURL: "http://localhost:9090",
	}
	api.Request.TLS = &tls.ConnectionState{} // simulates HTTPS

	L := newTestLuaState()
	registerLuaAPI(L, api)

	fn := L.GetField(L.GetGlobal("proxy"), "scheme").(*lua.LFunction)
	L.Push(fn)
	if err := L.PCall(0, 1, nil); err != nil {
		t.Fatal(err)
	}
	result := L.ToString(-1)
	if result != "https" {
		t.Errorf("expected 'https', got '%s'", result)
	}
	L.Pop(1)
}

func TestLuaAPI_Scheme_ForwardedProto(t *testing.T) {
	api := &LuaAPI{
		Request:     httptest.NewRequest("GET", "/test", nil),
		Headers:     make(map[string]string),
		QueryArgs:   make(map[string]string),
		UpstreamURL: "http://localhost:9090",
	}
	api.Request.Header.Set("X-Forwarded-Proto", "https")

	L := newTestLuaState()
	registerLuaAPI(L, api)

	fn := L.GetField(L.GetGlobal("proxy"), "scheme").(*lua.LFunction)
	L.Push(fn)
	if err := L.PCall(0, 1, nil); err != nil {
		t.Fatal(err)
	}
	result := L.ToString(-1)
	if result != "https" {
		t.Errorf("expected 'https' from X-Forwarded-Proto, got '%s'", result)
	}
	L.Pop(1)
}

func TestLuaAPI_Host(t *testing.T) {
	api := &LuaAPI{
		Request:     httptest.NewRequest("GET", "/test", nil),
		Headers:     make(map[string]string),
		QueryArgs:   make(map[string]string),
		UpstreamURL: "http://localhost:9090",
	}
	api.Request.Host = "api.example.com"

	L := newTestLuaState()
	registerLuaAPI(L, api)

	fn := L.GetField(L.GetGlobal("proxy"), "host").(*lua.LFunction)
	L.Push(fn)
	if err := L.PCall(0, 1, nil); err != nil {
		t.Fatal(err)
	}
	result := L.ToString(-1)
	if result != "api.example.com" {
		t.Errorf("expected 'api.example.com', got '%s'", result)
	}
	L.Pop(1)
}

func TestLuaAPI_UpstreamURL(t *testing.T) {
	api := &LuaAPI{
		Request:     httptest.NewRequest("GET", "/test", nil),
		Headers:     make(map[string]string),
		QueryArgs:   make(map[string]string),
		UpstreamURL: "http://backend.internal:8080",
	}

	L := newTestLuaState()
	registerLuaAPI(L, api)

	fn := L.GetField(L.GetGlobal("proxy"), "upstreamURL").(*lua.LFunction)
	L.Push(fn)
	if err := L.PCall(0, 1, nil); err != nil {
		t.Fatal(err)
	}
	result := L.ToString(-1)
	if result != "http://backend.internal:8080" {
		t.Errorf("expected 'http://backend.internal:8080', got '%s'", result)
	}
	L.Pop(1)
}

func TestIntegration_ClientIPInResponse(t *testing.T) {
	// End-to-end: request with X-Forwarded-For goes through proxy,
	// Lua script injects X-Client-IP header from proxy.clientIP()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	origUpstream := UpstreamForTest
	origScripts := LuaScriptsForTest
	UpstreamForTest = upstream.URL
	LuaScriptsForTest = map[string]string{
		"test.lua": `
			local ip = proxy.clientIP()
			proxy.setResponseHeader("X-Client-IP", ip)
			proxy.setResponseHeader("X-Scheme", proxy.scheme())
			proxy.setResponseHeader("X-Host", proxy.host())
		`,
	}
	defer func() {
		UpstreamForTest = origUpstream
		LuaScriptsForTest = origScripts
	}()

	handler := buildTestHandler()

	req := httptest.NewRequest("GET", "/api", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.42")
	req.Host = "myapp.example.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Check injected headers
	if rec.Header().Get("X-Client-IP") != "203.0.113.42" {
		t.Errorf("expected X-Client-IP=203.0.113.42, got %s", rec.Header().Get("X-Client-IP"))
	}
	if rec.Header().Get("X-Scheme") != "http" {
		t.Errorf("expected X-Scheme=http, got %s", rec.Header().Get("X-Scheme"))
	}
	if rec.Header().Get("X-Host") != "myapp.example.com" {
		t.Errorf("expected X-Host=myapp.example.com, got %s", rec.Header().Get("X-Host"))
	}
}

// --- Helper functions ---

func newTestLuaState() *lua.LState {
	L := lua.NewState()
	return L
}

func newTestAPI() *LuaAPI {
	return &LuaAPI{
		Request:     httptest.NewRequest("GET", "/test", nil),
		Headers:     make(map[string]string),
		QueryArgs:   make(map[string]string),
		UpstreamURL: "http://localhost:9090",
	}
}

// These variables allow tests to override config without env vars.
var (
	UpstreamForTest    string
	LuaScriptsForTest  map[string]string
)

func buildTestHandler() http.Handler {
	upstream, _ := url.Parse(UpstreamForTest)
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	runner := NewScriptRunner()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api := &LuaAPI{
			Request:     r,
			Headers:     make(map[string]string),
			QueryArgs:   make(map[string]string),
			UpstreamURL: upstream.String(),
		}
		for k, vals := range r.Header {
			api.Headers[k] = strings.Join(vals, ", ")
		}
		for k, vals := range r.URL.Query() {
			api.QueryArgs[k] = strings.Join(vals, ", ")
		}

		bodyBytes, _ := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		api.Body = bodyBytes

		_, cancel := context.WithCancel(context.Background())
		api.Cancel = cancel

		for _, src := range LuaScriptsForTest {
			err := runner.Run(src, api)
			if err != nil {
				api.Response = &http.Response{
					StatusCode: 500,
					Body:       io.NopCloser(bytes.NewReader([]byte("script error"))),
				}
				api.Blocked = true
				break
			}
			if api.Blocked {
				cancel()
				break
			}
		}

		if api.Blocked && api.Response != nil {
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

		r.Body = io.NopCloser(bytes.NewReader(api.Body))
		r.ContentLength = int64(len(api.Body))
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
		// Merge Lua-set response headers into the proxied response
		if api.Response != nil && api.Response.Header != nil {
			for k, vals := range api.Response.Header {
				for _, v := range vals {
					w.Header().Add(k, v)
				}
			}
		}
		proxy.ServeHTTP(w, r)
		cancel()
	})
}
