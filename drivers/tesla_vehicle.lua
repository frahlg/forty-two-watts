-- Tesla Vehicle Driver (read-only, via TeslaBLEProxy or Tesla API)
-- Emits: Vehicle (DerVehicle)
-- Protocol: HTTPS / HTTP (Tesla Owner API shape — vehicle_data endpoint)
--
-- Fetches the vehicle's own SoC and charge_limit so forty-two-watts can
-- show the real "24 / 50 %" in the EV bubble and let the loadpoint
-- manager prefer the truth over its delivered-Wh inference. This
-- driver is transparent to the transport — it speaks plain HTTP/JSON
-- against anything that implements Tesla's vehicle_data shape. In the
-- typical deployment that's TeslaBLEProxy running on the same LAN
-- (https://github.com/wimaha/TeslaBleHttpProxy) which translates to
-- BLE under the hood — forty-two-watts never sees BLE.
--
-- Config example:
--   drivers:
--     - name: tesla-garage
--       lua: drivers/tesla_vehicle.lua
--       capabilities:
--         http:
--           allowed_hosts: ["192.168.1.50"]
--       config:
--         base_url: "http://192.168.1.50:8080"
--         vehicle_id: "1234567890"      # id or VIN, per proxy
--         access_token: ""              # optional Bearer token
--         poll_interval_s: 60           # default 60
--         stale_after_s: 900            # default 900 (15 min)

DRIVER = {
  id           = "tesla-vehicle",
  name         = "Tesla Vehicle (BLE Proxy)",
  manufacturer = "Tesla",
  version      = "0.1.0",
  protocols    = { "http" },
  capabilities = { "vehicle" },
  description  = "Read-only vehicle SoC + charge limit via Tesla API-compatible HTTP endpoint (e.g. TeslaBLEProxy).",
  homepage     = "https://github.com/wimaha/TeslaBleHttpProxy",
  authors      = { "forty-two-watts contributors" },
  tested_models = { "Model Y", "Model 3" },
  verification_status = "beta",
}

PROTOCOL = "http"

-- Runtime state
local base_url = nil
local vehicle_id = nil
local access_token = nil
local poll_interval_ms = 60000
local stale_after_ms = 900000

-- Cached last-known reading so we can keep publishing a value while
-- the vehicle is asleep. Tesla returns 408 "vehicle unavailable" when
-- the car is in deep sleep; we treat that as "use last-known" until
-- stale_after_ms elapses.
local last = {
  ts_ms          = 0,
  soc            = nil,
  charge_limit   = nil,
  charging_state = nil,
  time_to_full   = nil,
}

local function auth_headers()
  if access_token and access_token ~= "" then
    return { ["Authorization"] = "Bearer " .. access_token,
             ["Accept"] = "application/json" }
  end
  return { ["Accept"] = "application/json" }
end

local function safe_http_err(err)
  if err == nil then return "ok" end
  return tostring(err):match("^(HTTP %d+)") or "request failed"
end

function driver_init(config)
  if not config then
    host.log("error", "tesla: config required")
    return
  end
  base_url = config.base_url
  vehicle_id = config.vehicle_id or config.vin
  access_token = config.access_token

  if not base_url or base_url == "" then
    host.log("error", "tesla: base_url required")
    return
  end
  if not vehicle_id or vehicle_id == "" then
    host.log("error", "tesla: vehicle_id (or vin) required")
    return
  end

  if tonumber(config.poll_interval_s) then
    poll_interval_ms = math.max(15, math.floor(tonumber(config.poll_interval_s))) * 1000
  end
  if tonumber(config.stale_after_s) then
    stale_after_ms = math.max(60, math.floor(tonumber(config.stale_after_s))) * 1000
  end

  host.set_make("Tesla")
  host.set_sn(tostring(vehicle_id))
  host.log("info", "tesla: driver initialized for " .. tostring(vehicle_id))
end

-- emit_last sends the cached reading with a stale flag computed from
-- the cached timestamp. Called when the HTTP poll fails or the
-- vehicle is asleep, so the UI keeps showing a number instead of a
-- blank.
local function emit_last()
  if last.soc == nil then
    return
  end
  local age = host.millis() - last.ts_ms
  local stale = age > stale_after_ms
  host.emit("vehicle", {
    soc              = last.soc,
    charge_limit_pct = last.charge_limit,
    charging_state   = last.charging_state,
    time_to_full_min = last.time_to_full,
    stale            = stale,
  })
end

function driver_poll()
  if not base_url or not vehicle_id then
    return 10000
  end

  local url = base_url .. "/api/1/vehicles/" .. vehicle_id .. "/vehicle_data"
  local resp, err = host.http_get(url, auth_headers())
  if err ~= nil then
    -- Tesla proxies return 408 for "vehicle asleep" — treat as
    -- "cached data still valid" rather than a hard error.
    host.log("debug", "tesla: poll error: " .. safe_http_err(err))
    emit_last()
    return poll_interval_ms
  end

  local body = resp and resp.body
  if not body or body == "" then
    emit_last()
    return poll_interval_ms
  end

  local decoded, derr = host.json_decode(body)
  if derr or not decoded then
    host.log("warn", "tesla: json decode failed")
    emit_last()
    return poll_interval_ms
  end

  -- Tesla shape: { response: { charge_state: {...}, ... } } for Owner API.
  -- Some proxies omit the envelope and return charge_state at top level —
  -- handle both.
  local charge_state = nil
  if type(decoded) == "table" then
    if decoded.response and type(decoded.response) == "table" and
       decoded.response.charge_state then
      charge_state = decoded.response.charge_state
    elseif decoded.charge_state then
      charge_state = decoded.charge_state
    end
  end
  if not charge_state then
    host.log("debug", "tesla: no charge_state in response")
    emit_last()
    return poll_interval_ms
  end

  local soc = tonumber(charge_state.battery_level)
  local limit = tonumber(charge_state.charge_limit_soc)
  local ttf = tonumber(charge_state.time_to_full_charge)
  local cs = charge_state.charging_state
  if type(cs) ~= "string" then cs = nil end

  if soc ~= nil then
    last.soc            = soc
    last.charge_limit   = limit
    last.charging_state = cs
    -- Tesla API returns time_to_full_charge in hours (decimal); convert to minutes.
    last.time_to_full   = ttf and math.floor(ttf * 60 + 0.5) or nil
    last.ts_ms          = host.millis()

    host.emit("vehicle", {
      soc              = soc,
      charge_limit_pct = limit,
      charging_state   = cs,
      time_to_full_min = last.time_to_full,
      stale            = false,
    })
  else
    -- Malformed response (no battery_level) → keep last-known.
    emit_last()
  end

  return poll_interval_ms
end

function driver_command(action, _, _)
  -- Read-only driver. Commands are intentionally not supported —
  -- forty-two-watts does not try to wake / set_charge_limit /
  -- start_charging the vehicle. Operators manage that via their
  -- Tesla app or the proxy's own UI.
  host.log("debug", "tesla: command ignored: " .. tostring(action))
  return false
end

function driver_default_mode()
  -- Nothing to do — there's no output state to reset.
end

function driver_cleanup()
  last.soc = nil
end
