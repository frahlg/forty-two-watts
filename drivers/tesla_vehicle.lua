-- Tesla Vehicle Driver (read-only, via TeslaBLEProxy on local LAN)
-- Emits: Vehicle (DerVehicle)
-- Protocol: HTTP (Tesla Owner API shape — /api/1/vehicles/{VIN}/vehicle_data)
--
-- Fetches the vehicle's own SoC and charge_limit so forty-two-watts can
-- show the real "24 / 50 %" in the EV bubble and let the loadpoint
-- manager prefer the truth over its delivered-Wh inference. Designed
-- to talk to TeslaBLEProxy running on the same LAN
-- (https://github.com/wimaha/TeslaBleHttpProxy), which translates
-- HTTP/JSON to BLE under the hood. No Tesla cloud credentials, no
-- OAuth token, no internet round-trip — the proxy IS the key to the
-- vehicle.
--
-- Config: two fields, that's it.
--
--   drivers:
--     - name: tesla-garage
--       lua: drivers/tesla_vehicle.lua
--       capabilities:
--         http:
--           allowed_hosts: ["192.168.1.50"]   # IP of the proxy
--       config:
--         ip:  "192.168.1.50"
--         vin: "5YJ3E1EA1KF000000"

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

-- Runtime state. base_url is built from the `ip` config field.
-- TeslaBLEProxy listens on :8080 by default — hardcoded since that's
-- what the project ships. Poll + staleness are tuned in-driver;
-- operators touch only `ip` + `vin` in YAML.
local PROXY_PORT        = 8080
local POLL_INTERVAL_MS  = 60000
local STALE_AFTER_MS    = 900000

local base_url = nil
local vin = nil

-- Cached last-known reading so we can keep publishing a value while
-- the vehicle is asleep. Tesla returns 408 "vehicle unavailable" when
-- the car is in deep sleep; we treat that as "use last-known" until
-- STALE_AFTER_MS elapses.
local last = {
  ts_ms          = 0,
  soc            = nil,
  charge_limit   = nil,
  charging_state = nil,
  time_to_full   = nil,
}

local function auth_headers()
  -- TeslaBLEProxy on the LAN doesn't require a bearer token; it
  -- authenticates against the car via BLE itself. Plain JSON Accept
  -- header is all we need.
  return { ["Accept"] = "application/json" }
end

local function safe_http_err(err)
  if err == nil then return "ok" end
  return tostring(err):match("^(HTTP %d+)") or "request failed"
end

function driver_init(config)
  if not config then
    host.log("error", "tesla: config required (ip + vin)")
    return
  end
  local ip = config.ip
  vin = config.vin

  if not ip or ip == "" then
    host.log("error", "tesla: `ip` required (LAN address of TeslaBLEProxy)")
    return
  end
  if not vin or vin == "" then
    host.log("error", "tesla: `vin` required (vehicle VIN the proxy is paired to)")
    return
  end

  base_url = "http://" .. ip .. ":" .. tostring(PROXY_PORT)

  host.set_make("Tesla")
  host.set_sn(tostring(vin))
  host.log("info", "tesla: driver initialized vin=" .. tostring(vin) ..
                   " proxy=" .. base_url)
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
  local stale = age > STALE_AFTER_MS
  host.emit("vehicle", {
    soc              = last.soc,
    charge_limit_pct = last.charge_limit,
    charging_state   = last.charging_state,
    time_to_full_min = last.time_to_full,
    stale            = stale,
  })
end

function driver_poll()
  if not base_url or not vin then
    return 10000
  end

  -- endpoints=charge_state narrows the response to just what we
  -- care about (SoC + limit + charging_state + time_to_full). The
  -- wakeup=true flag asks the proxy to wake the car over BLE if it
  -- is sleeping, so the data returned reflects the current state
  -- rather than a cached snapshot. TeslaBLEProxy's README calls
  -- this out explicitly for "receive data frequently" use cases.
  local url = base_url .. "/api/1/vehicles/" .. vin ..
              "/vehicle_data?endpoints=charge_state&wakeup=true"
  local resp, err = host.http_get(url, auth_headers())
  if err ~= nil then
    -- Tesla proxies return 408 for "vehicle asleep" — treat as
    -- "cached data still valid" rather than a hard error.
    host.log("debug", "tesla: poll error: " .. safe_http_err(err))
    emit_last()
    return POLL_INTERVAL_MS
  end

  local body = resp and resp.body
  if not body or body == "" then
    emit_last()
    return POLL_INTERVAL_MS
  end

  local decoded, derr = host.json_decode(body)
  if derr or not decoded then
    host.log("warn", "tesla: json decode failed")
    emit_last()
    return POLL_INTERVAL_MS
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
    return POLL_INTERVAL_MS
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

  return POLL_INTERVAL_MS
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
