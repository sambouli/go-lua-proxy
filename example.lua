-- Real-world example: auth + rate-limiting + response transformation
local proxy = proxy

-- 1. Read request info
local method = proxy.request.method
local path = proxy.request.path
local ip = proxy.clientIP()
local scheme = proxy.scheme()
local host = proxy.host()

proxy.log("request: " .. method .. " " .. path .. " from " .. ip)

-- 2. Auth check — require X-API-Key header
local apiKey = proxy.getHeader("X-Api-Key")
if apiKey ~= "" then
    -- Whitelist of valid keys (in production, this would be a DB/API call)
    local validKeys = {
        ["sk-test-001"] = true,
        ["sk-prod-abc"] = true,
    }
    if not validKeys[apiKey] then
        proxy.abort(401, '{"error":"invalid api key"}')
        return
    end
end

-- 3. Rate limiting — simple per-IP counter using os.time bucket
local now = os.time()
local bucket = math.floor(now / 60)  -- 1-minute buckets
local key = ip .. ":" .. tostring(bucket)

-- Use a global table to track requests per bucket
if not _RATE_LIMITS then
    _RATE_LIMITS = {}
end

local count = (_RATE_LIMITS[key] or 0) + 1
_RATE_LIMITS[key] = count

if count > 10 then
    proxy.abort(429, '{"error":"rate limit exceeded","retry_after":' .. (60 - (now % 60)) .. '}')
    return
end

-- 4. Query param extraction
local limit = tonumber(proxy.getQuery("limit")) or 10
local offset = tonumber(proxy.getQuery("offset")) or 0
proxy.log("pagination: limit=" .. limit .. " offset=" .. offset)

-- 5. Response modification — add security headers
proxy.setResponseHeader("X-Content-Type-Options", "nosniff")
proxy.setResponseHeader("X-RateLimit-Remaining", tostring(math.max(0, 10 - count)))
proxy.setResponseHeader("X-RateLimit-Reset", tostring((bucket + 1) * 60))

-- 6. Request body inspection (for POST/PUT)
if method == "POST" or method == "PUT" then
    local body = proxy.getBody()
    if #body > 1024 * 1024 then  -- 1MB limit
        proxy.abort(413, '{"error":"payload too large"}')
        return
    end
    
    -- Parse JSON body if content-type is application/json
    local contentType = proxy.getHeader("Content-Type")
    if contentType and contentType:find("application/json") then
        local data, err = proxy.jsonDecode(body)
        if data and data.action then
            proxy.log("action detected: " .. tostring(data.action))
            -- Block specific actions
            if data.action == "delete_all" then
                proxy.abort(403, '{"error":"action blocked: delete_all"}')
                return
            end
        end
    end
end

-- 7. Path-based routing decisions
if path == "/health" then
    -- Short-circuit: set status + body directly (not redirect)
    proxy.setStatus(200)
    proxy.setBody('{"status":"ok"}')
    proxy.abort(200, '{"status":"ok"}')
elseif path == "/admin" then
    -- Admin paths only from localhost
    if ip ~= "127.0.0.1" and ip ~= "::1" then
        proxy.abort(403, '{"error":"admin access denied"}')
        return
    end
end
