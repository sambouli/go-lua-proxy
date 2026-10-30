-- Add X-Client-IP header from request
local ip = proxy.getHeader("X-Real-Ip")
if ip == "" then ip = proxy.getHeader("X-Forwarded-For") end
if ip == "" then ip = "unknown" end
proxy.setResponseHeader("X-Client-IP", ip)

-- Add X-Proxy-Day header with current day of week
local day = os.date("%A")
proxy.setResponseHeader("X-Proxy-Day", day)
