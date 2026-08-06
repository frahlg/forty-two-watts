-- TEST FIXTURE — local copy of srcfl/device-drivers drivers/lua/ftw_plant.lua
-- (srcfl/device-drivers#77) used by plant_e2e_test.go until the bundled
-- driver snapshot pin picks it up. Do not edit here; edit the catalog
-- driver and re-copy.
-- ftw-plant — multi-rack battery plant module (aggregate battery)
-- Emits: battery
-- Protocol: HTTP (the ftw-plant module's /v1 loopback contract).
--
-- The plant module (ghcr.io/srcfl/ftw-plant) presents N PCS/rack units
-- as ONE logical battery: this driver is the thin bridge that lets FTW
-- core treat the whole plant as a single controllable battery. All
-- per-rack allocation, SoC balancing and fault derating happen inside
-- the module; core's safety pipeline (fuse guard, SoC clamps, slew,
-- staleness → default mode) applies unchanged to the aggregate.
--
-- Defense in depth: every setpoint this driver forwards carries a lease
-- (ttl_ms). If core stops dispatching — or this driver dies — the
-- module ramps every rack to zero on its own when the lease expires.
--
-- Config example (config.yaml):
--   drivers:
--     - name: plant
--       lua: drivers/ftw_plant.lua
--       battery_capacity_wh: 200000
--       max_charge_w: 100000
--       max_discharge_w: 100000
--       capabilities:
--         http:
--           allowed_hosts:
--             - 127.0.0.1
--       config:
--         host: "127.0.0.1"
--         port: 9200
--         lease_ttl_ms: 10000
--
-- Sign convention (site: positive W flows INTO the site):
--   battery w positive = plant charging. The module already speaks the
--   site convention, so values pass through unchanged in both
--   directions — no sign flip in this driver. SoC arrives from the
--   module as a 0..1 fraction and is emitted as soc_nom_fract.

DRIVER = {
  host_api_min = 1,
  host_api_max = 1,
  id           = "ftw_plant",
  name         = "FTW plant module (multi-rack battery)",
  manufacturer = "Sourceful",
  version      = "0.1.0",
  protocols    = { "http" },
  capabilities = { "battery" },
  description  = "Bridges the ftw-plant multi-rack battery module's /v1 contract into one controllable aggregate battery.",
  homepage     = "https://github.com/srcfl/ftw",
  authors      = { "FTW contributors" },
  http_hosts   = { "127.0.0.1" },
  verification_status = "experimental",
  verification_notes  = "Verified against sim-pcs and the ftw-plant module; no multi-rack hardware validation yet.",
  connection_defaults = {
    host = "127.0.0.1",
    port = 9200,
  },
}

PROTOCOL = "http"

local plant_host   = "127.0.0.1"
local plant_port   = 9200
local lease_ttl_ms = 10000

local base_url = "http://" .. plant_host .. ":" .. tostring(plant_port)

local function refresh_base_url()
    base_url = "http://" .. plant_host .. ":" .. tostring(plant_port)
end

-- Backoff so a stopped module doesn't get hammered: 2 s → 30 s. While
-- backed off we emit nothing, so core's watchdog sees the driver go
-- stale and applies the autonomous default — exactly the safe path.
local last_attempt = 0
local backoff_ms   = 0
local BACKOFF_MIN  =  2000
local BACKOFF_MAX  = 30000

local function bump_backoff()
    if backoff_ms == 0 then
        backoff_ms = BACKOFF_MIN
    else
        backoff_ms = math.min(backoff_ms * 2, BACKOFF_MAX)
    end
    last_attempt = host.millis()
end

local function clear_backoff()
    backoff_ms   = 0
    last_attempt = 0
end

local function in_backoff()
    if backoff_ms == 0 then return false end
    return (host.millis() - last_attempt) < backoff_ms
end

-- http_get_json: pcall-guarded GET + decode so a module restart mid-
-- response can never abort the poll with a Lua error.
local function http_get_json(path)
    local ok, body, err = pcall(host.http_get, base_url .. path)
    if not ok or not body then
        return nil, tostring(err or body)
    end
    local decoded, data = pcall(host.json_decode, body)
    if not decoded or type(data) ~= "table" then
        return nil, "malformed JSON"
    end
    return data
end

local function http_post_json(path, payload_table)
    local payload = host.json_encode(payload_table)
    local ok, body, err = pcall(host.http_post, base_url .. path, payload)
    if not ok or not body then
        return nil, tostring(err or body)
    end
    local decoded, data = pcall(host.json_decode, body)
    if not decoded or type(data) ~= "table" then
        return nil, "malformed JSON"
    end
    return data
end

function driver_init(config)
    host.set_make("Sourceful")
    host.set_model("ftw-plant")
    if config then
        if type(config.host) == "string" and config.host ~= "" then
            plant_host = config.host
        end
        if config.port then
            plant_port = tonumber(config.port) or 9200
        end
        if config.lease_ttl_ms then
            local n = tonumber(config.lease_ttl_ms)
            if n and n >= 1000 then lease_ttl_ms = n end
        end
    end
    refresh_base_url()
    host.log("info", "ftw_plant: module at " .. base_url)
end

function driver_poll()
    if in_backoff() then return 1000 end

    local st, err = http_get_json("/v1/status")
    if not st or type(st.aggregate) ~= "table" then
        host.log("warn", "ftw_plant: status failed: " .. tostring(err))
        bump_backoff()
        return 1000
    end
    clear_backoff()

    local agg = st.aggregate
    -- No online units = no reachable energy: stop emitting so core's
    -- watchdog stales the driver rather than dispatching into a plant
    -- that cannot respond.
    if (tonumber(agg.units_online) or 0) == 0 then
        host.log("warn", "ftw_plant: zero units online — suppressing telemetry")
        return 2000
    end

    -- Identity: synthesized from the fleet shape so re-siting the same
    -- module keeps the same device id.
    host.set_sn("plant-" .. tostring(agg.units_total) .. "x")
    host.set_rated_w(tonumber(agg.available_discharge_w) or 0)

    -- The module reports soc already as a fraction (0..1), so it maps
    -- straight onto soc_nom_fract with no percent division.
    local soc_already_fraction = tonumber(agg.soc)
    host.emit("battery", {
        w             = tonumber(agg.power_w) or 0,
        soc_nom_fract = soc_already_fraction,
    })
    host.emit_metric("plant_units_online", tonumber(agg.units_online) or 0, "", "", "Units online")
    host.emit_metric("plant_units_total", tonumber(agg.units_total) or 0, "", "", "Units total")
    host.emit_metric("plant_available_charge_w", tonumber(agg.available_charge_w) or 0, "W", "", "Charge headroom")
    host.emit_metric("plant_available_discharge_w", tonumber(agg.available_discharge_w) or 0, "W", "", "Discharge headroom")
    host.emit_metric("plant_usable_wh", tonumber(agg.usable_energy_wh) or 0, "Wh", "", "Usable energy")
    return 1000
end

local function send_setpoint(w)
    local res, err = http_post_json("/v1/setpoint", { power_w = w, ttl_ms = lease_ttl_ms })
    if not res then
        return false, "setpoint failed: " .. tostring(err)
    end
    if res.accepted ~= true then
        return false, "setpoint not accepted"
    end
    return true
end

function driver_command(action, power_w, cmd)
    if action ~= "battery" then
        return "unsupported action: " .. tostring(action)
    end
    local ok, err = send_setpoint(tonumber(power_w) or 0)
    if not ok then
        host.log("warn", "ftw_plant: " .. err)
        return err
    end
    return true
end

function driver_default_mode()
    -- Safe autonomous state for a plant is zero power; the racks' own
    -- BMS handles everything below that. Even if this write fails, the
    -- module's lease expiry ramps the fleet to zero by itself.
    local ok, err = send_setpoint(0)
    if not ok then
        host.log("warn", "ftw_plant: default-mode " .. err .. " (lease expiry covers this)")
        return err
    end
    return true
end

function driver_cleanup()
    -- Shutdown path: leave the plant leased-out; expiry ramps to zero.
    return true
end
