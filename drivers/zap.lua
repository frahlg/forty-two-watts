-- zap.lua
-- Sourceful Zap — multi-device energy gateway with P1 meter, Modbus TCP
-- inverters/batteries and local JSON API. This driver reads the P1 grid
-- meter exposed over /api/devices/<p1-sn>/data/json and emits it as the
-- site meter.
-- Emits: Meter (read-only)
-- Protocol: HTTP (local JSON API on the Zap gateway)
--
-- Discovery:
--   The Zap advertises itself via mDNS as `zap.local`. On Linux the OS
--   resolver handles that transparently as long as nss-mdns / avahi is
--   installed (it is on RPi OS). If zap.local doesn't resolve on the
--   operator's network — router blocks mDNS, DNS rebinding filter, or
--   the EMS is on a different VLAN — set `host` to the device's LAN IP.
--
--   The P1 meter's serial number is auto-discovered on the first poll by
--   walking GET /api/devices and picking the entry with type=="p1_uart".
--   Override by setting `serial` in config if you have more than one P1
--   device on the same Zap and want to pin a specific one.
--
-- Config example (config.yaml):
--   drivers:
--     - name: zap
--       lua: drivers/zap.lua
--       is_site_meter: true
--       capabilities:
--         http:
--           allowed_hosts: ["zap.local"]  # or the LAN IP
--       config:
--         host: "zap.local"          # default; override with IP if mDNS fails
--         # serial: "p1m-xxxxxxxx"   # optional; auto-detected when omitted
--
-- Sign convention (SITE = positive W flows INTO the site):
--   meter.w: positive = importing from grid, negative = exporting
-- The Zap P1 meter already reports import-positive, so no sign flip.

DRIVER = {
  id           = "sourceful-zap",
  name         = "Sourceful Zap",
  manufacturer = "Sourceful",
  version      = "1.0.0",
  protocols    = { "http" },
  capabilities = { "meter" },
  description  = "Sourceful Zap local-JSON gateway reading the P1 grid meter (zap.local or LAN IP).",
  homepage     = "https://sourceful.energy",
  authors      = { "forty-two-watts contributors" },
  http_hosts   = { "zap.local" },
  connection_defaults = {
    host = "zap.local",
  },
}

PROTOCOL = "http"

local zap_host = "zap.local"
local p1_serial = nil

-- Backoff state for discovery failures: no point hammering /api/devices
-- every second when the gateway is unreachable.
local discovery_last_attempt = 0
local discovery_backoff_ms   = 0
local DISCOVERY_BACKOFF_MIN  = 2000
local DISCOVERY_BACKOFF_MAX  = 60000

local function base_url()
    -- Accept both "zap.local" and "http://zap.local" styles.
    if string.sub(zap_host, 1, 7) == "http://" or string.sub(zap_host, 1, 8) == "https://" then
        return zap_host
    end
    return "http://" .. zap_host
end

-- Walk GET /api/devices and pick the first `p1_uart` entry's serial.
-- Returns (serial, err). Does not log — the caller owns that so success
-- and failure messages stay co-located with the backoff / retry logic.
local function discover_p1_serial()
    local url = base_url() .. "/api/devices"
    local body, err = host.http_get(url)
    if err then
        return nil, err
    end
    local data = host.json_decode(body)
    if not data or not data.devices then
        return nil, "unexpected payload (no `devices` array)"
    end
    for _, dev in ipairs(data.devices) do
        if dev.type == "p1_uart" and dev.sn then
            return dev.sn, nil
        end
    end
    return nil, "no p1_uart device found"
end

-- Bump the discovery backoff exponentially (cap at MAX). Called whenever
-- discovery or a data poll fails so we don't spam the network.
local function bump_backoff()
    if discovery_backoff_ms == 0 then
        discovery_backoff_ms = DISCOVERY_BACKOFF_MIN
    else
        discovery_backoff_ms = math.min(discovery_backoff_ms * 2, DISCOVERY_BACKOFF_MAX)
    end
    discovery_last_attempt = host.millis()
end

local function clear_backoff()
    discovery_backoff_ms = 0
    discovery_last_attempt = 0
end

local function in_backoff()
    if discovery_backoff_ms == 0 then return false end
    return (host.millis() - discovery_last_attempt) < discovery_backoff_ms
end

----------------------------------------------------------------------------
-- Driver interface
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("Sourceful")

    if config and type(config.host) == "string" and config.host ~= "" then
        zap_host = config.host
    end
    if config and type(config.serial) == "string" and config.serial ~= "" then
        p1_serial = config.serial
        host.set_sn(p1_serial)
        host.log("info", "Zap: using pinned P1 serial " .. p1_serial)
    end

    host.log("info", "Zap: driver initialized (host=" .. zap_host .. ")")
end

function driver_poll()
    -- Phase 1: discover the P1 serial if we don't know it yet.
    if not p1_serial then
        if in_backoff() then
            return 1000
        end
        local sn, err = discover_p1_serial()
        if err or not sn then
            bump_backoff()
            host.log("warn", "Zap: P1 discovery failed: " .. tostring(err)
                .. " (retry in " .. discovery_backoff_ms .. "ms)")
            return 1000
        end
        p1_serial = sn
        host.set_sn(p1_serial)
        clear_backoff()
        host.log("info", "Zap: discovered P1 meter " .. p1_serial)
    end

    -- Phase 2: poll meter data.
    local url = base_url() .. "/api/devices/" .. p1_serial .. "/data/json"
    local body, err = host.http_get(url)
    if err then
        bump_backoff()
        host.log("warn", "Zap: data fetch failed: " .. tostring(err)
            .. " (retry in " .. discovery_backoff_ms .. "ms)")
        -- If the Zap was rebooted the serial may have rotated (unlikely
        -- on real hardware but possible on dev gateways). Force re-discovery
        -- on persistent failure.
        if discovery_backoff_ms >= DISCOVERY_BACKOFF_MAX then
            host.log("warn", "Zap: repeated failures — will re-discover P1 serial")
            p1_serial = nil
        end
        return 1000
    end

    local data = host.json_decode(body)
    if not data or not data.meter then
        bump_backoff()
        host.log("warn", "Zap: payload missing `meter` block")
        return 1000
    end
    clear_backoff()

    local m = data.meter
    local meter = {
        w         = tonumber(m.W)    or 0,
        l1_w      = tonumber(m.L1_W) or 0,
        l2_w      = tonumber(m.L2_W) or 0,
        l3_w      = tonumber(m.L3_W) or 0,
        l1_v      = tonumber(m.L1_V) or 0,
        l2_v      = tonumber(m.L2_V) or 0,
        l3_v      = tonumber(m.L3_V) or 0,
        l1_a      = tonumber(m.L1_A) or 0,
        l2_a      = tonumber(m.L2_A) or 0,
        l3_a      = tonumber(m.L3_A) or 0,
        import_wh = tonumber(m.total_import_Wh) or 0,
        export_wh = tonumber(m.total_export_Wh) or 0,
    }

    host.emit("meter", meter)
    -- Diagnostics: long-format TS DB
    host.emit_metric("meter_l1_w", meter.l1_w)
    host.emit_metric("meter_l2_w", meter.l2_w)
    host.emit_metric("meter_l3_w", meter.l3_w)
    host.emit_metric("meter_l1_v", meter.l1_v)
    host.emit_metric("meter_l2_v", meter.l2_v)
    host.emit_metric("meter_l3_v", meter.l3_v)
    host.emit_metric("meter_l1_a", meter.l1_a)
    host.emit_metric("meter_l2_a", meter.l2_a)
    host.emit_metric("meter_l3_a", meter.l3_a)

    return 1000
end

----------------------------------------------------------------------------
-- Control (READ-ONLY — P1 meter exposes no writable endpoint)
----------------------------------------------------------------------------

function driver_command(action, power_w, cmd)
    if action == "init" or action == "deinit" then
        return true
    end
    host.log("warn", "Zap: read-only driver, ignoring action=" .. tostring(action))
    return false
end

function driver_default_mode()
    -- Read-only: nothing to revert.
end

function driver_cleanup()
    p1_serial = nil
    clear_backoff()
end
