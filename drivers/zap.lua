-- zap.lua
-- Sourceful Zap — multi-device energy gateway with P1 meter, Modbus TCP
-- inverters/batteries and local JSON API. This driver reads the P1 grid
-- meter exposed over /api/devices/<p1-sn>/data/json and emits it as the
-- site meter, and aggregates PV generation from any inverter the Zap is
-- talking to (device_type=="inverter" with an enabled pv DER).
-- Emits: Meter, PV (read-only)
-- Protocol: HTTP (local JSON API on the Zap gateway)
--
-- Discovery:
--   The Zap advertises itself via mDNS as `zap.local`. On Linux the OS
--   resolver handles that transparently as long as nss-mdns / avahi is
--   installed (it is on RPi OS). If zap.local doesn't resolve on the
--   operator's network — router blocks mDNS, DNS rebinding filter, or
--   the EMS is on a different VLAN — set `host` to the device's LAN IP.
--
--   On the first poll the driver walks GET /api/devices once and picks:
--     - the first `p1_uart` entry as the P1 meter (site meter)
--     - every `device_type=="inverter"` entry with an enabled `pv` DER
--       as a PV source; W values across them are summed.
--   Override the P1 via config.serial if you have several; the PV set is
--   always auto-detected (add/remove an inverter → restart the driver).
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
--   pv.w   : negative = generating (source).  Zap reports generation as
--            positive W, so the driver negates at the boundary.
-- The Zap P1 meter already reports import-positive, so no meter flip.

DRIVER = {
  id           = "sourceful-zap",
  name         = "Sourceful Zap",
  manufacturer = "Sourceful",
  version      = "1.1.0",
  protocols    = { "http" },
  capabilities = { "meter", "pv" },
  description  = "Sourceful Zap local-JSON gateway: P1 grid meter + PV from any attached inverter.",
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
local pv_serials = {}   -- list of inverter serials that carry an enabled pv DER

-- Backoff state for discovery / data failures: no point hammering the
-- gateway when it's unreachable.
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

-- Clamp obviously-bogus readings. The Zap passes through raw overflow
-- sentinels when a downstream device is offline (u16=65535, i16=32768/
-- -32768, u32=4294967295, and a couple of *100-scaled lookalikes for
-- currents). Treat any |v| above `max_abs` as "not available" → nil.
local function sane(v, max_abs)
    local n = tonumber(v)
    if n == nil then return nil end
    if max_abs and math.abs(n) > max_abs then return nil end
    return n
end

-- Walk GET /api/devices and return:
--   (p1_serial, pv_serials[], err)
-- p1_serial is the first `p1_uart` device; pv_serials is every inverter
-- with at least one enabled `pv` DER, in the order the Zap returns them.
-- Does not log — the caller owns that so success and failure messages
-- stay co-located with the backoff / retry logic.
local function discover_devices()
    local url = base_url() .. "/api/devices"
    local body, err = host.http_get(url)
    if err then
        return nil, nil, err
    end
    local data = host.json_decode(body)
    if not data or not data.devices then
        return nil, nil, "unexpected payload (no `devices` array)"
    end
    local p1 = nil
    local pvs = {}
    for _, dev in ipairs(data.devices) do
        if not dev.sn then
            -- nothing to key on; skip
        elseif dev.type == "p1_uart" and not p1 then
            p1 = dev.sn
        elseif dev.device_type == "inverter" and type(dev.ders) == "table" then
            for _, der in ipairs(dev.ders) do
                if der.type == "pv" and der.enabled then
                    pvs[#pvs + 1] = dev.sn
                    break
                end
            end
        end
    end
    if not p1 then
        return nil, nil, "no p1_uart device found"
    end
    return p1, pvs, nil
end

-- Bump the discovery backoff exponentially (cap at MAX). Called whenever
-- discovery or the site-meter data poll fails so we don't spam the network.
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

-- Fetch /api/devices/<sn>/data/json, return the decoded table or (nil, err).
local function fetch_device(sn)
    local url = base_url() .. "/api/devices/" .. sn .. "/data/json"
    local body, err = host.http_get(url)
    if err then return nil, err end
    local data = host.json_decode(body)
    if not data then return nil, "json decode failed" end
    return data, nil
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
    -- Phase 1: discover P1 + PV inverters if we don't know them yet.
    if not p1_serial or #pv_serials == 0 then
        -- Skip if the user pinned config.serial and we already have it;
        -- otherwise run discovery whenever p1 is missing OR we haven't
        -- enumerated inverters yet.
        local need_discovery = (not p1_serial) or (p1_serial and #pv_serials == 0)
        if need_discovery then
            if in_backoff() then
                return 1000
            end
            local p1, pvs, err = discover_devices()
            if err then
                bump_backoff()
                host.log("warn", "Zap: discovery failed: " .. tostring(err)
                    .. " (retry in " .. discovery_backoff_ms .. "ms)")
                return 1000
            end
            if not p1_serial then
                p1_serial = p1
                host.set_sn(p1_serial)
                host.log("info", "Zap: discovered P1 meter " .. p1_serial)
            end
            pv_serials = pvs
            if #pv_serials > 0 then
                host.log("info", "Zap: discovered " .. #pv_serials
                    .. " PV inverter(s): " .. table.concat(pv_serials, ","))
            else
                host.log("info", "Zap: no PV inverters found")
            end
            clear_backoff()
        end
    end

    -- Phase 2: poll the P1 meter.
    local data, err = fetch_device(p1_serial)
    if err then
        bump_backoff()
        host.log("warn", "Zap: P1 data fetch failed: " .. tostring(err)
            .. " (retry in " .. discovery_backoff_ms .. "ms)")
        -- If the Zap was rebooted the serial may have rotated (unlikely
        -- on real hardware but possible on dev gateways). Force re-discovery
        -- on persistent failure.
        if discovery_backoff_ms >= DISCOVERY_BACKOFF_MAX then
            host.log("warn", "Zap: repeated failures — will re-discover devices")
            p1_serial = nil
            pv_serials = {}
        end
        return 1000
    end
    if not data.meter then
        bump_backoff()
        host.log("warn", "Zap: P1 payload missing `meter` block")
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
    host.emit_metric("meter_l1_w", meter.l1_w)
    host.emit_metric("meter_l2_w", meter.l2_w)
    host.emit_metric("meter_l3_w", meter.l3_w)
    host.emit_metric("meter_l1_v", meter.l1_v)
    host.emit_metric("meter_l2_v", meter.l2_v)
    host.emit_metric("meter_l3_v", meter.l3_v)
    host.emit_metric("meter_l1_a", meter.l1_a)
    host.emit_metric("meter_l2_a", meter.l2_a)
    host.emit_metric("meter_l3_a", meter.l3_a)

    -- Phase 3: aggregate PV across every inverter the Zap exposes.
    -- Each inverter has an independent data endpoint; we sum W and
    -- generation energy, and emit diagnostics per inverter (the structured
    -- `pv` reading on the telemetry store is a single slot, so combining
    -- them is the only way both inverters show up). PV fetch failures
    -- don't reset the meter's backoff — the site meter is load-bearing,
    -- PV is nice-to-have; we just log debug and skip that inverter.
    if #pv_serials > 0 then
        local total_w = 0
        local total_gen_wh = 0
        local any = false
        for _, sn in ipairs(pv_serials) do
            local pvdata, perr = fetch_device(sn)
            if perr or not pvdata or not pvdata.pv then
                host.log("debug", "Zap: PV fetch failed for "
                    .. sn .. ": " .. tostring(perr or "missing pv block"))
            else
                any = true
                local pv = pvdata.pv
                -- Rated cap gives us a sanity bound for the W reading.
                -- The Zap emits overflow sentinels when the inverter is
                -- offline (i16 32768, u32 huge values); anything above
                -- 10× rated or otherwise clearly bogus → treat as 0.
                local rated = tonumber(pv.rated_power_W) or 0
                local cap = rated > 0 and (rated * 10) or 1e6
                local w = sane(pv.W, cap) or 0
                total_w = total_w + w
                local gen = sane(pv.total_generation_Wh, 1e12) or 0
                total_gen_wh = total_gen_wh + gen

                -- Per-inverter diagnostics into the TS DB. For multi-inverter
                -- setups tag the serial onto each metric name so they don't
                -- collide; for single-inverter sites the outer aggregate
                -- emission below covers `pv_w` and we only need the extra
                -- MPPT / heatsink detail here.
                local multi = (#pv_serials > 1)
                local tag = multi and ("_" .. sn) or ""
                if multi then host.emit_metric("pv_w" .. tag, w) end
                local hs = sane(pv.heatsink_C, 150)
                if hs then host.emit_metric("pv_heatsink_c" .. tag, hs) end
                local m1v = sane(pv.mppt1_V, 1500)
                if m1v then host.emit_metric("pv_mppt1_v" .. tag, m1v) end
                local m1a = sane(pv.mppt1_A, 50)
                if m1a then host.emit_metric("pv_mppt1_a" .. tag, m1a) end
                local m2v = sane(pv.mppt2_V, 1500)
                if m2v then host.emit_metric("pv_mppt2_v" .. tag, m2v) end
                local m2a = sane(pv.mppt2_A, 50)
                if m2a then host.emit_metric("pv_mppt2_a" .. tag, m2a) end
            end
        end
        if any then
            -- Site convention: PV generation is a source → negative W.
            host.emit("pv", { w = -total_w, lifetime_wh = total_gen_wh })
            host.emit_metric("pv_w", -total_w)
        end
    end

    return 1000
end

----------------------------------------------------------------------------
-- Control (READ-ONLY — Zap exposes no writable endpoint)
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
    pv_serials = {}
    clear_backoff()
end
