# go-lua-proxy

> **Provenance.** This is one half of an A/B comparison: the same brief — "build a
> Go reverse proxy with a per-request Lua scripting API" — was given to two models.
> This repo holds the result from **DeepSeek V4 Flash**; the sibling
> [go-lua-proxy-fn](https://github.com/sambouli/go-lua-proxy-fn) holds the result
> from **QFN (Qwen3.8-Flash-Next)**. They are independent implementations of the
> same task, kept apart so the outcomes can be compared directly.

A lightweight HTTP reverse proxy written in Go that executes **Lua scripts** per-request, giving you programmatic control over routing, auth, headers, body transformation, and more — without leaving the Lua ecosystem.

## Quick start

```bash
# Build
go build -o go-lua-proxy .

# Run (env config)
LISTEN_ADDR=:8080 \
UPSTREAM=http://backend:9090 \
LUA_SCRIPTS=./auth.lua,./rate-limit.lua \
./go-lua-proxy
```

### Environment variables

| Variable | Default | Description |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | Address to listen on |
| `UPSTREAM` | `http://localhost:9090` | Backend URL to proxy to |
| `LUA_SCRIPTS` | *(none)* | Comma-separated list of `.lua` files to load |

## Lua API

Each loaded script runs inside a fresh Lua state with one global table: `proxy`.

### Request info

```lua
-- Read-only request metadata
print(proxy.request.method)   -- "GET", "POST", ...
print(proxy.request.path)     -- "/api/users"
print(proxy.request.rawPath)  -- path as received (includes encoded chars)
print(proxy.request.proto)    -- "HTTP/1.1", "HTTP/2.0"

-- Query parameters
local limit = tonumber(proxy.getQuery("limit")) or 10
local offset = tonumber(proxy.getQuery("offset")) or 0

-- Headers (comma-joined for multi-value headers)
local contentType = proxy.getHeader("Content-Type")

-- Connection / network info
local ip        = proxy.clientIP()      -- real client IP (X-Real-Ip → X-Forwarded-For[0] → RemoteAddr)
local addr      = proxy.remoteAddr()    -- raw peer address, e.g. "127.0.0.1:54321"
local scheme    = proxy.scheme()        -- "http" or "https" (respects X-Forwarded-Proto, checks TLS)
local host      = proxy.host()          -- Host header value
local upstream  = proxy.upstreamURL()   -- configured UPSTREAM value
```

### Response manipulation (non-short-circuiting)

Headers set here are merged into the proxied response from upstream:

```lua
proxy.setStatus(200)
proxy.setResponseHeader("X-Custom-Header", "value")
proxy.setResponseHeader("X-Security", "nosniff")
```

### Body manipulation

```lua
-- Read the request body
local body = proxy.getBody()

-- Modify the request body before forwarding
proxy.setBody(newBodyString)
```

### Short-circuit (bypass upstream entirely)

These methods stop the request from reaching the backend:

```lua
-- Redirect to another URL (default 302)
proxy.redirect("/login", 302)

-- Abort with a custom status + body
proxy.abort(401, '{"error":"unauthorized"}')
proxy.abort(403, "blocked", 403)  -- code, message, explicit code

-- Or combine setStatus + abort for a full custom response
proxy.setStatus(200)
proxy.setBody('{"status":"ok"}')
proxy.abort(200, '{"status":"ok"}')
```

### JSON helpers

```lua
-- Decode JSON string → Lua table
local data, err = proxy.jsonDecode('{"name":"test","count":42}')
if data then
    print(data.name)       -- "test"
    print(type(data.count)) -- number (not string!)
end

-- Encode Lua table → JSON string
local jsonStr, err = proxy.jsonEncode({key = "value", num = 123})
```

### Logging

```lua
proxy.log("debug message")  -- prints to proxy stderr log
```

## Example: auth + security headers

```lua
local proxy = proxy

-- 1. Resolve client IP
local ip = proxy.clientIP()
proxy.setResponseHeader("X-Client-IP", ip)

-- 2. Auth check
local apiKey = proxy.getHeader("X-Api-Key")
if apiKey ~= "" then
    local validKeys = {["sk-test-001"] = true, ["sk-prod-abc"] = true}
    if not validKeys[apiKey] then
        proxy.abort(401, '{"error":"invalid api key"}')
        return
    end
end

-- 3. Add security headers to every proxied response
proxy.setResponseHeader("X-Content-Type-Options", "nosniff")
proxy.setResponseHeader("X-Frame-Options", "DENY")

-- 4. Block dangerous actions in POST bodies
if proxy.request.method == "POST" then
    local body = proxy.getBody()
    local contentType = proxy.getHeader("Content-Type")
    if contentType and contentType:find("application/json") then
        local data = proxy.jsonDecode(body)
        if data and data.action == "delete_all" then
            proxy.abort(403, '{"error":"action blocked"}')
            return
        end
    end
end

-- 5. Path-based short-circuit
if proxy.request.path == "/health" then
    proxy.abort(200, '{"status":"ok"}')
end
```

## Stats endpoint

`GET /__proxy_stats__` returns JSON:

```json
{
  "total_requests": 150,
  "total_bytes_in": 4096,
  "total_bytes_out": 8192,
  "status_codes": {"200": 140, "401": 5, "403": 3, "502": 2},
  "avg_latency_ms": 23.5,
  "last_updated": "2026-10-30T00:00:00Z"
}
```

## How it works

1. Each incoming request creates a fresh Lua state.
2. All loaded scripts run sequentially against the same `proxy` object.
3. Scripts can read/modify the request, set response headers, or short-circuit.
4. If a script calls `abort()` or `redirect()`, the request never reaches the upstream.
5. Otherwise the request is forwarded to `UPSTREAM`, and any headers set via `setResponseHeader` are merged into the response.

## Limitations

- **No cross-request state**: each request gets a new Lua state, so globals like `_RATE_LIMITS` reset. For persistent state (rate limiting, caching), use an external store (DB, Redis) reachable via HTTP from Lua.
- **Body size limit**: request bodies are read up to 10 MB before being passed to Lua.
- **Single upstream**: this is a single-host reverse proxy, not a load balancer.
