-- CTEK Chargestorm EV Charger Driver (MQTT, telemetry-only)
-- Emits: EV (read-only — no control surface)
-- Protocol: MQTT
--
-- Hardware + firmware:
--   Requires a CTEK Chargestorm Connected 2 or Connected 3 running
--   CSOS (Chargestorm OS) firmware 3.11.15.1 / 3.12.6 or later. The
--   Modbus charging-limit register (0x1200) is NOT available over
--   MQTT — CTEK's MQTT interface is strictly read-only. Use this
--   driver to get rich, per-phase telemetry (power, current, voltage,
--   lifetime energy, EVSE state code) plus the "assigned" current the
--   charger reports. For control (set current, pause, start, resume)
--   pair this with drivers/ctek.lua or drivers/ctek_v2.lua — those
--   drive register 0x1200 / 0x2200 over Modbus/TCP, using the SAME
--   station. Run both drivers against the same charger; the clamp +
--   dispatch layers key off the EV reading from whichever driver is
--   emitting DerEV first. If you only run this MQTT driver, the EMS
--   will see accurate telemetry but won't be able to adjust the
--   charging setpoint.
--
--   On the CCU, under Automation → MQTT:
--     MqttEnabled = true
--     MqttServer  = <broker-ip>   (empty → internal broker on charger)
--     MqttPort    = 1883
--     MqttBaseTopic = CTEK        (default; override if you customised it)
--     MqttLogin / MqttPassword = <broker credentials, if required>
--
-- Topic layout (source: CTEK "Automation interface" v1.0, rev 6b4af7):
--
--     <base>/<CBID>/evse1/em            energy meter JSON, per-outlet
--     <base>/<CBID>/evse2/em            (dual-outlet stations only)
--     <base>/<CBID>/evse1/status        EVSE state + assigned current
--     <base>/<CBID>/evse1/meterinfo     serial number of the meter
--     <base>/<CBID>/mainMeter/em        whole-station meter (not used here)
--
--   <base>   — MqttBaseTopic on the CCU (default: "CTEK").
--   <CBID>   — the charger's serial / board ID, e.g. 91728M03W4010406.
--
--   em payload:
--     { "current":[A_L1, A_L2, A_L3],
--       "voltage":[V_L1, V_L2, V_L3],
--       "power":   <W>,
--       "energy":  <Wh>,
--       "frequency": <Hz>,
--       "timestamp": "ISO8601" }
--
--   status payload:
--     { "assigned": <A>,       # current offered to the EV
--       "state":    "CHRG",    # 4-letter code (see table below)
--       "timestamp": "..." }
--
--     State codes (PDF §2.4):
--       AVAL Available, no EV connected
--       PAUS Pause, charging disallowed by station
--       EVRD EV is ready (plug + auth done, ramp pending)
--       CHRG Charging
--       FLTY Fault
--       DSBL Disabled
--       CONN EV connected, waiting for authentication
--       NCRQ No charging requested by EV
--       AUTH Authenticated, waiting for EV to connect
--       INVL Invalid
--       GONE EV disappeared
--       DONE Session finished
--       SUHT Transient → pause
--       STHT Transient → stop
--
-- Sign convention (SITE = positive W flows INTO the site):
--   ev.w: always positive when charging — an EVSE is a one-way load;
--   there's no vehicle-to-grid path on Chargestorm. The em.power value
--   from CTEK is already positive on draw, so we pass it through.
--
-- Config example (config.yaml):
--   drivers:
--     - name: ctek-telemetry
--       lua: drivers/ctek_mqtt.lua
--       capabilities:
--         mqtt:
--           host: 192.168.1.190   # or the address of your shared broker
--           port: 1883
--           username: ""
--           password: ""
--       config:
--         base_topic: "CTEK"      # match MqttBaseTopic on the CCU
--         cbid:       ""          # optional: pin to one charger's serial;
--                                 # leave empty to accept the first one seen
--         outlet:     1           # 1 = EVSE1, 2 = EVSE2 (dual-outlet)
--         phases:     3
--         min_a:      6           # IEC 61851 floor (clamped ≥ 6)
--         max_a:      16          # fuse-limited ceiling; used as ev.max_a
--                                 # when no EV session is active (the CTEK
--                                 # MQTT namespace doesn't publish
--                                 # FuseRating, only CCU/.../Configuration
--                                 # does — and this driver stays out of the
--                                 # CCU tree).

DRIVER = {
  id           = "ctek-chargestorm-mqtt",
  name         = "CTEK Chargestorm (MQTT telemetry)",
  manufacturer = "CTEK",
  version      = "0.1.0",
  protocols    = { "mqtt" },
  capabilities = { "ev" },
  description  = "CTEK Chargestorm Connected 2/3 telemetry-only driver via MQTT (CSOS ≥ 3.11.15.1). Per-phase power/current/voltage + EVSE state code. Pair with drivers/ctek.lua (Modbus) for control.",
  homepage     = "https://www.ctek.com",
  authors      = { "forty-two-watts contributors" },
  tested_models = { "Chargestorm Connected 2", "Chargestorm Connected 3" },
  verification_status = "beta",
  verification_notes = "Topic layout and payload shapes per CTEK 'Automation interface' PDF v1.0 rev 6b4af7. Telemetry-only — control MUST come from the Modbus driver.",
  connection_defaults = {
    port = 1883,
  },
}

PROTOCOL = "mqtt"

----------------------------------------------------------------------------
-- Runtime config (overridden from config.yaml in driver_init)
----------------------------------------------------------------------------
local base_topic = "CTEK"
local cbid       = nil   -- snap to first seen CBID when not configured
local outlet     = 1
local phases     = 3
-- Fuse-limited maximum current the charger is allowed to deliver at this
-- site. The CTEK topic namespace doesn't publish this (FuseRating lives
-- only under the CCU/NG-V1/... tree, which this driver stays out of by
-- design), so we take it from config and fall back on it when no EV is
-- plugged — otherwise ev.max_a reads 0 whenever the station is idle and
-- the UI shows "0 A max" which looks broken. Matches the Modbus driver's
-- max_a config key exactly so a site running both drivers has the same
-- ceiling.
local max_a      = 16
local min_a      = 6

-- Cached last known values — emitted every poll even if no fresh MQTT
-- message arrived, so the dispatch + Kalman layers see a steady stream.
-- CTEK publishes em at a low rate (typically ~30 s) so polling faster
-- than that would otherwise report stale-=-zero.
local last_em     = nil    -- decoded em JSON table
local last_status = nil    -- decoded status JSON table
local meter_sn    = nil

-- Per-state connected / charging derivation. Keyed on the 4-letter code
-- CTEK puts in status.state. "connected" is true whenever the EV is
-- physically plugged AND the charger isn't in a fault/unavailable state;
-- "charging" is strictly the CHRG state. Unknown codes default to
-- "connected unknown, not charging" so the dispatch clamp stays
-- conservative.
local STATE_FLAGS = {
    AVAL = { connected = false, charging = false },
    PAUS = { connected = true,  charging = false },
    EVRD = { connected = true,  charging = false },
    CHRG = { connected = true,  charging = true  },
    CONN = { connected = true,  charging = false },
    NCRQ = { connected = true,  charging = false },
    AUTH = { connected = false, charging = false },   -- waiting for EV
    DONE = { connected = true,  charging = false },
    GONE = { connected = false, charging = false },
    SUHT = { connected = true,  charging = false },   -- transient → pause
    STHT = { connected = true,  charging = false },   -- transient → stop
    FLTY = { connected = false, charging = false },
    DSBL = { connected = false, charging = false },
    INVL = { connected = false, charging = false },
}

----------------------------------------------------------------------------
-- Helpers
----------------------------------------------------------------------------

-- Parse "<base>/<CBID>/evse<N>/<leaf>" and return (CBID, outlet, leaf).
-- Returns nil if the topic doesn't match our expected shape so we can
-- ignore stray subscriptions without spamming warnings.
local function parse_topic(topic)
    local base, id, evse, leaf = topic:match("^([^/]+)/([^/]+)/([^/]+)/([^/]+)$")
    if not base or base ~= base_topic then return nil end
    local n = evse:match("^evse(%d+)$")
    if not n then return nil end
    return id, tonumber(n), leaf
end

local function safe_num(v)
    if v == nil then return nil end
    local n = tonumber(v)
    if n ~= n then return nil end   -- nan guard
    return n
end

-- em.current / em.voltage come as 3-element arrays. Lua arrays from
-- host.json_decode are 1-indexed tables, so [1]/[2]/[3] = L1/L2/L3.
local function arr3(t, i)
    if type(t) ~= "table" then return 0 end
    return safe_num(t[i]) or 0
end

----------------------------------------------------------------------------
-- Driver interface
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("CTEK")

    if config then
        if type(config.base_topic) == "string" and #config.base_topic > 0 then
            base_topic = config.base_topic
        end
        if type(config.cbid) == "string" and #config.cbid > 0 then
            cbid = config.cbid
            host.set_sn(cbid)
        end
        if tonumber(config.outlet) then
            local o = math.floor(tonumber(config.outlet))
            if o == 1 or o == 2 then outlet = o end
        end
        if tonumber(config.phases) then
            local p = math.floor(tonumber(config.phases))
            if p == 1 or p == 3 then phases = p end
        end
        if tonumber(config.max_a) then max_a = math.floor(tonumber(config.max_a)) end
        if tonumber(config.min_a) then min_a = math.floor(tonumber(config.min_a)) end
    end
    if min_a < 6 then min_a = 6 end        -- IEC 61851 floor
    if max_a < min_a then max_a = min_a end

    -- Wildcard on the CBID segment so we don't force the operator to
    -- hard-code the charger's serial. If they pinned one in config we
    -- still subscribe broadly (cheap) and filter on receive.
    local em_topic       = string.format("%s/+/evse%d/em",        base_topic, outlet)
    local status_topic   = string.format("%s/+/evse%d/status",    base_topic, outlet)
    local meterinfo_topic = string.format("%s/+/evse%d/meterinfo", base_topic, outlet)
    host.mqtt_subscribe(em_topic)
    host.mqtt_subscribe(status_topic)
    host.mqtt_subscribe(meterinfo_topic)

    host.log("info", string.format(
        "CTEK-MQTT: subscribed to %s/%s/evse%d/{em,status,meterinfo} (phases=%d, cbid=%s)",
        base_topic, cbid or "+", outlet, phases, cbid or "<auto>"))
end

function driver_poll()
    local messages = host.mqtt_messages()
    if messages then
        for _, msg in ipairs(messages) do
            local msg_cbid, msg_outlet, leaf = parse_topic(msg.topic)
            if msg_cbid and msg_outlet == outlet then
                -- Pin to first seen CBID if config didn't specify one.
                -- Anchoring device_id to the CTEK serial means battery /
                -- model state keyed on device_id survives a driver rename.
                if not cbid then
                    cbid = msg_cbid
                    host.set_sn(cbid)
                    host.log("info", "CTEK-MQTT: locked onto CBID " .. cbid)
                elseif msg_cbid ~= cbid then
                    goto continue
                end

                local ok, data = pcall(host.json_decode, msg.payload)
                if ok and data then
                    if leaf == "em" then
                        last_em = data
                    elseif leaf == "status" then
                        last_status = data
                    elseif leaf == "meterinfo" then
                        if type(data.serialno) == "string" then
                            meter_sn = data.serialno
                        end
                    end
                end
                ::continue::
            end
        end
    end

    -- Derive the EV reading from whatever we have cached. A fresh
    -- subscriber may have not received retained messages yet — emit
    -- a conservative placeholder in that case so the UI shows up.
    local ev = {
        w         = 0,
        connected = false,
        charging  = false,
        max_a     = 0,
        phases    = phases,
    }

    if last_em then
        ev.w    = safe_num(last_em.power) or 0
        ev.l1_a = arr3(last_em.current, 1)
        ev.l2_a = arr3(last_em.current, 2)
        ev.l3_a = arr3(last_em.current, 3)
        ev.l1_v = arr3(last_em.voltage, 1)
        ev.l2_v = arr3(last_em.voltage, 2)
        ev.l3_v = arr3(last_em.voltage, 3)
        ev.lifetime_wh = safe_num(last_em.energy) or 0
        local hz = safe_num(last_em.frequency)
        if hz then ev.hz = hz end
    end

    local state_code = nil
    if last_status then
        -- `assigned` in the CTEK status payload is the current the
        -- station is currently offering to an EV session — zero when
        -- no car is plugged (state=AVAL) or paused (state=PAUS). The
        -- UI's "max current" field expects the ceiling the charger is
        -- *willing* to deliver right now, so report the configured
        -- fuse-limited max whenever assigned is unhelpful (0 / nil) —
        -- a plain mirror of `assigned` reads "0 A max" in the idle
        -- state which looks like a config error.
        local assigned = safe_num(last_status.assigned) or 0
        ev.max_a = assigned > 0 and assigned or max_a
        state_code = last_status.state
        local flags = state_code and STATE_FLAGS[state_code]
        if flags then
            ev.connected = flags.connected
            ev.charging  = flags.charging
        else
            -- Fall back to deriving from power if the state code is
            -- unknown / missing (e.g. empty first-poll window).
            ev.charging  = (ev.w or 0) > 100
            ev.connected = ev.charging
        end
        if state_code then ev.state_label = state_code end
    elseif last_em then
        ev.charging  = (ev.w or 0) > 100
        ev.connected = ev.charging
        ev.max_a     = max_a
    else
        ev.max_a = max_a
    end

    host.emit("ev", ev)

    host.emit_metric("ev_power_w",     ev.w or 0)
    host.emit_metric("ev_assigned_a",  ev.max_a or 0)
    host.emit_metric("ev_l1_a",        ev.l1_a or 0)
    host.emit_metric("ev_l2_a",        ev.l2_a or 0)
    host.emit_metric("ev_l3_a",        ev.l3_a or 0)
    host.emit_metric("ev_l1_v",        ev.l1_v or 0)
    host.emit_metric("ev_l2_v",        ev.l2_v or 0)
    host.emit_metric("ev_l3_v",        ev.l3_v or 0)
    if ev.lifetime_wh then host.emit_metric("ev_lifetime_wh", ev.lifetime_wh) end
    if ev.hz           then host.emit_metric("grid_hz",       ev.hz)          end

    return 5000
end

function driver_command(action, power_w, cmd)
    if action == "init" or action == "deinit" then
        return true
    end
    -- Telemetry-only driver — the CTEK MQTT interface is read-only per
    -- CTEK's "Automation interface" documentation. Control commands
    -- must be sent via the Modbus driver (drivers/ctek.lua or
    -- drivers/ctek_v2.lua).
    host.log("warn", "CTEK-MQTT: received " .. tostring(action)
        .. " but MQTT is read-only; use the Modbus driver for control")
    return false
end

function driver_default_mode()
    -- No control surface — nothing to reset to a safe state.
end

function driver_cleanup()
    -- No-op.
end
