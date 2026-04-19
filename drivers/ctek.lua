-- CTEK Chargestorm EV Charger Driver (Automation API v1, hybrid MQTT + Modbus)
-- Emits: EV
-- Protocols: Modbus/TCP (control + telemetry) and MQTT (telemetry, optional)
--
-- Hardware + firmware:
--   Requires a CTEK Chargestorm Connected 2 or Connected 3 running CSOS
--   (Chargestorm OS) firmware 4.9.3 or later for the Modbus charging-limit
--   register; MQTT telemetry works from CSOS 3.11.15.1 / 3.12.6.
--
--   Modbus side (control + Modbus telemetry):
--     Automation → ModbusTCPEnable                  = true
--     Automation → modbus_tcp_automation_api_version = 1
--   Use drivers/ctek_v2.lua if you've set the API version to 2 instead.
--   The two variants differ ONLY in the base register address (0x1xxx vs
--   0x2xxx); semantics, units and function codes are identical.
--
--   MQTT side (optional richer telemetry):
--     Automation → MQTT
--       MqttEnabled    = true
--       MqttServer     = <broker-ip>
--       MqttPort       = 1883
--       MqttBaseTopic  = CTEK
--       MqttLogin / MqttPassword = <broker credentials, if required>
--   The MQTT side is strictly read-only on CTEK's interface, so even with
--   it configured this driver still drives the charging limit over Modbus.
--   When MQTT is configured the EVSE state code ("CHRG", "PAUS", "EVRD",
--   …), per-phase telemetry, lifetime energy counter and grid frequency
--   come from the broker — Modbus is then only used for control + as a
--   cold-start fallback while we wait for the first MQTT message.
--
-- Unit identifier selects the outlet on dual-outlet stations:
--   unit_id = 1 → EVSE1 (left outlet, or single-outlet station)
--   unit_id = 2 → EVSE2 (right outlet)
--
-- Modbus register map (source: CTEK "Automation interface" v1.0, rev 6b4af7):
--
--   Identity / meter type:
--     0x1000         API version       (u16)
--     0x1001         API status, 0=OK  (u16)
--     0x1002         EnergyMeterType   (u16 enum)
--     0x1003..0x1008 Serial (12 ASCII chars, 2 per register, big-endian bytes)
--
--   Telemetry (one contiguous read 0x1100..0x1108 = 9 regs):
--     0x1100..0x1101 Lifetime energy (Wh, u32 big-endian, high word first)
--     0x1102..0x1104 Per-phase current, L1/L2/L3 (u16 × 10⁻³ A, i.e. mA)
--     0x1105..0x1107 Per-phase voltage L1-N/L2-N/L3-N (u16 × 10⁻¹ V)
--     0x1108         Total active power (u16 W)
--
--   Control:
--     0x1200         Charging limit (u16 A, read/write) — 0 disables
--                    charging; values 1..5 are treated as 0 by the
--                    charger (IEC 61851 minimum is 6 A). Setpoint is
--                    lost on charger restart.
--     0x1201         Maximum assignment (u16 A, read-only) — upper
--                    bound the charger will accept given current
--                    de-rating, schedules, NANOGRID™ curtailment, etc.
--
-- MQTT topic layout (same source PDF, §2.2):
--     <base>/<CBID>/evse1/em            energy meter JSON, per-outlet
--     <base>/<CBID>/evse2/em            (dual-outlet stations only)
--     <base>/<CBID>/evse1/status        EVSE state + assigned current
--     <base>/<CBID>/evse1/meterinfo     serial number of the meter
--   <base>   — MqttBaseTopic on the CCU (default "CTEK").
--   <CBID>   — the charger's serial / board ID, e.g. 91728M03W4010406.
--
--   em payload:
--     { "current":[A_L1,A_L2,A_L3], "voltage":[V_L1,V_L2,V_L3],
--       "power":<W>, "energy":<Wh>, "frequency":<Hz>, "timestamp":"..." }
--
--   status payload:
--     { "assigned":<A>, "state":"CHRG", "timestamp":"..." }
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
--   there's no vehicle-to-grid path on Chargestorm.
--
-- Config example (config.yaml):
--   drivers:
--     - name: ctek
--       lua: drivers/ctek.lua
--       capabilities:
--         modbus:                       # required for control
--           host: 192.168.1.190
--           port: 502
--           unit_id: 1                  # 1 = EVSE1, 2 = EVSE2
--         mqtt:                         # optional; richer telemetry + state
--           host: 192.168.1.190         # CCU runs an internal broker
--           port: 1883
--           username: ""
--           password: ""
--       config:
--         phases:     3                 # 1 or 3; default 3
--         min_a:      6                 # minimum charge current (A); default 6
--         max_a:      16                # fuse-limited max (A); default 16
--         voltage_v:  230               # nominal per-phase voltage; default 230
--         base_topic: "CTEK"            # MQTT base topic; matches CCU default
--         cbid:       ""                # optional: pin to one charger's serial
--         outlet:     1                 # MQTT outlet selector (mirror unit_id)

DRIVER = {
  id           = "ctek-chargestorm",
  name         = "CTEK Chargestorm (API v1)",
  manufacturer = "CTEK",
  version      = "0.3.0",
  protocols    = { "modbus", "mqtt" },
  capabilities = { "ev" },
  description  = "CTEK Chargestorm Connected 2/3, hybrid driver: Modbus/TCP Automation API v1 (CSOS ≥ 4.9.3) for control, MQTT for richer telemetry + EVSE state code.",
  homepage     = "https://www.ctek.com",
  authors      = { "forty-two-watts contributors" },
  tested_models = { "Chargestorm Connected 2", "Chargestorm Connected 3" },
  verification_status = "beta",
  verification_notes = "Register map per CTEK Automation interface v1.0; MQTT topic layout per the same PDF rev 6b4af7. Charging-limit write verified against CSOS 4.9.x.",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}

PROTOCOL = "modbus"

----------------------------------------------------------------------------
-- Register addresses — API v1.
-- The v2 variant (drivers/ctek_v2.lua) differs ONLY in the base offsets
-- (0x2xxx instead of 0x1xxx); keep the two files in sync when register
-- semantics change.
----------------------------------------------------------------------------
local REG_API_VERSION   = 0x1000
local REG_API_STATUS    = 0x1001
local REG_METER_TYPE    = 0x1002
local REG_SERIAL_BASE   = 0x1003   -- 6 regs → 12 ASCII chars
local REG_TELEMETRY     = 0x1100   -- 9 regs: energy(2) + I(3) + V(3) + W(1)
local REG_CHARGE_LIMIT  = 0x1200   -- r/w
local REG_MAX_ASSIGN    = 0x1201   -- r/o

local API_VERSION_EXPECTED = 1
local OTHER_DRIVER_HINT    = "drivers/ctek_v2.lua"

----------------------------------------------------------------------------
-- Runtime config (overridden from config.yaml in driver_init)
----------------------------------------------------------------------------
local phases     = 3
local min_a      = 6
local max_a      = 16
local voltage_v  = 230
local base_topic = "CTEK"
local cbid       = nil   -- snap to first seen CBID when not configured
local outlet     = 1

-- Last setpoint we successfully wrote. Used as the resume target when
-- ev_start / ev_resume come in without a specific current.
local last_set_a = 0
local sn_read    = false

-- Cached MQTT state. CTEK publishes em at a low rate (~30 s) so we keep
-- the last decoded payload around and emit on every poll regardless.
local last_em        = nil   -- decoded em JSON table
local last_status    = nil   -- decoded status JSON table
local mqtt_enabled   = false -- subscribed successfully → trust MQTT data

-- Per-state connected / charging derivation. Keyed on the 4-letter code
-- CTEK puts in status.state. "connected" is true whenever the EV is
-- physically plugged AND the charger isn't in a fault/unavailable state;
-- "charging" is strictly the CHRG state.
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

local function clamp_amps(a)
    if a == nil then return 0 end
    a = math.floor(a + 0.5)
    if a <= 0 then return 0 end
    if a < min_a then return 0 end     -- below minimum → pause
    if a > max_a then return max_a end
    return a
end

local function watts_to_amps(power_w)
    if not power_w or power_w <= 0 then return 0 end
    return math.floor((power_w / voltage_v / phases) + 0.5)
end

-- Decode a SunSpec/CTEK-style ASCII block where each u16 register packs
-- two bytes of the string in big-endian order. The charger pads the
-- tail with NUL bytes; stop at the first one to get a clean serial.
local function decode_ascii(regs, n)
    local s = ""
    for i = 1, n do
        local r = regs[i] or 0
        local hi = math.floor(r / 256)
        local lo = r % 256
        if hi == 0 then break end
        if hi >= 32 and hi < 127 then s = s .. string.char(hi) end
        if lo == 0 then break end
        if lo >= 32 and lo < 127 then s = s .. string.char(lo) end
    end
    return s
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

local function write_setpoint(amps)
    local ok, err = pcall(host.modbus_write, REG_CHARGE_LIMIT, amps)
    if not ok then
        host.log("warn", "CTEK: write charging limit failed: " .. tostring(err))
        return false
    end
    last_set_a = amps
    return true
end

local function read_setpoint()
    local ok, regs = pcall(host.modbus_read, REG_CHARGE_LIMIT, 1, "holding")
    if not ok or not regs or not regs[1] then return nil end
    return regs[1]
end

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

local function drain_mqtt()
    local messages = host.mqtt_messages()
    if not messages then return end
    for _, msg in ipairs(messages) do
        local msg_cbid, msg_outlet, leaf = parse_topic(msg.topic)
        if msg_cbid and msg_outlet == outlet then
            -- Pin to first seen CBID if config didn't specify one.
            -- Anchoring device_id to the CTEK serial means battery /
            -- model state keyed on device_id survives a driver rename.
            if not cbid then
                cbid = msg_cbid
                host.set_sn(cbid)
                sn_read = true
                host.log("info", "CTEK: locked onto CBID " .. cbid .. " from MQTT")
            end
            if msg_cbid == cbid then
                local ok, data = pcall(host.json_decode, msg.payload)
                if ok and data then
                    if leaf == "em" then
                        last_em = data
                    elseif leaf == "status" then
                        last_status = data
                    end
                end
            end
        end
    end
end

----------------------------------------------------------------------------
-- Driver interface
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("CTEK")

    if config then
        if tonumber(config.phases) then
            local p = math.floor(tonumber(config.phases))
            if p == 1 or p == 3 then phases = p end
        end
        if tonumber(config.min_a)     then min_a     = math.floor(tonumber(config.min_a))     end
        if tonumber(config.max_a)     then max_a     = math.floor(tonumber(config.max_a))     end
        if tonumber(config.voltage_v) then voltage_v = tonumber(config.voltage_v)             end
        if type(config.base_topic) == "string" and #config.base_topic > 0 then
            base_topic = config.base_topic
        end
        if type(config.cbid) == "string" and #config.cbid > 0 then
            cbid = config.cbid
            host.set_sn(cbid)
            sn_read = true
        end
        if tonumber(config.outlet) then
            local o = math.floor(tonumber(config.outlet))
            if o == 1 or o == 2 then outlet = o end
        end
    end

    if min_a < 6 then min_a = 6 end         -- IEC 61851 floor
    if max_a < min_a then max_a = min_a end

    -- MQTT side: optional. Subscribe with a wildcard on CBID so we don't
    -- force the operator to hard-code the charger's serial. If the host
    -- wasn't granted an MQTT capability the subscribe call returns an
    -- error string and we just stay Modbus-only.
    local em_topic        = string.format("%s/+/evse%d/em",        base_topic, outlet)
    local status_topic    = string.format("%s/+/evse%d/status",    base_topic, outlet)
    local meterinfo_topic = string.format("%s/+/evse%d/meterinfo", base_topic, outlet)
    local sub_ok = pcall(function()
        host.mqtt_subscribe(em_topic)
        host.mqtt_subscribe(status_topic)
        host.mqtt_subscribe(meterinfo_topic)
    end)
    if sub_ok then
        mqtt_enabled = true
        host.log("info", string.format(
            "CTEK: MQTT subscribed to %s/%s/evse%d/{em,status,meterinfo}",
            base_topic, cbid or "+", outlet))
    end

    -- Modbus side: sanity-check the API version. A mismatch means the
    -- operator enabled v2 instead — suggest the v2 driver rather than
    -- silently reading back garbage. Skipped silently if Modbus isn't
    -- wired (MQTT-only deployments).
    local ok, api_regs = pcall(host.modbus_read, REG_API_VERSION, 2, "holding")
    if ok and api_regs and api_regs[1] then
        local api_ver    = api_regs[1]
        local api_status = api_regs[2] or 0
        host.log("info", string.format(
            "CTEK: Modbus API v%d, status %d (expected v%d; use %s for the other version)",
            api_ver, api_status, API_VERSION_EXPECTED, OTHER_DRIVER_HINT))
    end

    host.log("info", string.format(
        "CTEK: driver initialized (phases=%d, min=%dA, max=%dA, V=%.0f, mqtt=%s)",
        phases, min_a, max_a, voltage_v, mqtt_enabled and "on" or "off"))

    local cur = read_setpoint()
    if cur then
        last_set_a = cur
        host.log("info", "CTEK: charge limit readback = " .. tostring(cur) .. "A")
    end
end

function driver_poll()
    if mqtt_enabled then drain_mqtt() end

    -- One-shot serial read off Modbus when we don't already have a CBID
    -- from MQTT. Keys device identity to the EVSE serial so battery /
    -- other models keyed on device_id stay stable across driver renames.
    if not sn_read then
        local ok_sn, sn_regs = pcall(host.modbus_read, REG_SERIAL_BASE, 6, "holding")
        if ok_sn and sn_regs then
            local sn = decode_ascii(sn_regs, 6)
            if #sn > 0 then
                host.set_sn(sn)
                sn_read = true
            end
        end
    end

    -- Modbus telemetry block — always read so we have a live source even
    -- if MQTT hasn't delivered yet (cold boot, broker reconnect). 9 regs
    -- in one transaction so the energy counter, per-phase current +
    -- voltage, and total power all come from a consistent snapshot.
    local ok_tel, tel = pcall(host.modbus_read, REG_TELEMETRY, 9, "holding")
    local mb_w  = 0
    local mb_il = { 0, 0, 0 }
    local mb_vl = { 0, 0, 0 }
    local mb_lifetime = 0
    if ok_tel and tel then
        mb_lifetime = host.decode_u32_be(tel[1], tel[2])
        mb_il[1]    = (tel[3] or 0) / 1000
        mb_il[2]    = (tel[4] or 0) / 1000
        mb_il[3]    = (tel[5] or 0) / 1000
        mb_vl[1]    = (tel[6] or 0) / 10
        mb_vl[2]    = (tel[7] or 0) / 10
        mb_vl[3]    = (tel[8] or 0) / 10
        mb_w        = tel[9] or 0
    end

    -- Modbus control state: charging limit (current target) + max
    -- assignment (upper bound the charger will honour).
    local limit, max_assign = last_set_a, max_a
    local ok_ctl, ctl = pcall(host.modbus_read, REG_CHARGE_LIMIT, 2, "holding")
    if ok_ctl and ctl then
        limit       = ctl[1] or last_set_a
        max_assign  = ctl[2] or max_a
        last_set_a  = limit
    end

    -- Pick the source for emitted telemetry. MQTT is preferred when we've
    -- received an em payload — it carries the EVSE state code and tends
    -- to refresh on session events faster than our 5 s Modbus poll. Fall
    -- back to Modbus for cold-start coverage.
    local ev_w, i_l1, i_l2, i_l3, v_l1, v_l2, v_l3, lifetime_wh, hz, state_code
    if last_em then
        ev_w        = safe_num(last_em.power) or mb_w
        i_l1        = arr3(last_em.current, 1)
        i_l2        = arr3(last_em.current, 2)
        i_l3        = arr3(last_em.current, 3)
        v_l1        = arr3(last_em.voltage, 1)
        v_l2        = arr3(last_em.voltage, 2)
        v_l3        = arr3(last_em.voltage, 3)
        lifetime_wh = safe_num(last_em.energy) or mb_lifetime
        hz          = safe_num(last_em.frequency)
    else
        ev_w        = mb_w
        i_l1, i_l2, i_l3 = mb_il[1], mb_il[2], mb_il[3]
        v_l1, v_l2, v_l3 = mb_vl[1], mb_vl[2], mb_vl[3]
        lifetime_wh = mb_lifetime
    end

    -- State derivation: prefer the explicit MQTT state code; otherwise
    -- derive conservatively from current + power. The Chargestorm
    -- exposes no "connector state" flag on Modbus, so the derived
    -- variant is necessarily approximate.
    local charging, connected
    if last_status then
        state_code = last_status.state
        local flags = state_code and STATE_FLAGS[state_code]
        if flags then
            connected = flags.connected
            charging  = flags.charging
        end
    end
    if charging == nil then
        local max_phase_a = math.max(i_l1, i_l2, i_l3)
        charging = (ev_w > 100) or (max_phase_a > 1.0)
        -- "Connected" is noisy over Modbus alone: when the car is plugged
        -- but not yet authenticated or ramping, current + power both read
        -- zero. Report connected whenever the charger is actively
        -- delivering OR whenever a non-zero limit is in effect — the
        -- latter covers "plan has scheduled a charging window; cable
        -- plugged".
        connected = charging or (limit >= min_a and max_assign > 0)
    end

    local ev = {
        w           = ev_w,
        connected   = connected,
        charging    = charging,
        max_a       = limit,
        phases      = phases,
        l1_v        = v_l1, l2_v = v_l2, l3_v = v_l3,
        l1_a        = i_l1, l2_a = i_l2, l3_a = i_l3,
        lifetime_wh = lifetime_wh,
    }
    if hz then ev.hz = hz end
    if state_code then ev.state_label = state_code end

    host.emit("ev", ev)

    host.emit_metric("ev_set_current_a",  limit)
    host.emit_metric("ev_max_assign_a",   max_assign)
    host.emit_metric("ev_l1_a",           i_l1)
    host.emit_metric("ev_l2_a",           i_l2)
    host.emit_metric("ev_l3_a",           i_l3)
    host.emit_metric("ev_l1_v",           v_l1)
    host.emit_metric("ev_l2_v",           v_l2)
    host.emit_metric("ev_l3_v",           v_l3)
    host.emit_metric("ev_power_w",        ev_w)
    host.emit_metric("ev_lifetime_wh",    lifetime_wh)
    if hz then host.emit_metric("grid_hz", hz) end

    return 5000
end

function driver_command(action, power_w, cmd)
    if action == "init" or action == "deinit" then
        return true
    end

    if action == "ev_set_current" then
        local amps = clamp_amps(watts_to_amps(power_w))
        host.log("debug", "CTEK: ev_set_current "
            .. tostring(power_w) .. "W → " .. tostring(amps) .. "A")
        return write_setpoint(amps)
    end

    if action == "ev_pause" then
        return write_setpoint(0)
    end

    if action == "ev_start" or action == "ev_resume" then
        -- Without a target current in the command, resume at whatever
        -- setpoint was in effect before the pause. If we've never
        -- written one (first start after boot), fall back to max_a so
        -- the car actually draws current instead of sitting idle.
        local amps = (last_set_a and last_set_a >= min_a) and last_set_a or max_a
        return write_setpoint(amps)
    end

    host.log("warn", "CTEK: unknown action " .. tostring(action))
    return false
end

function driver_default_mode()
    -- EV chargers have no "autonomous self-consumption" equivalent.
    -- The Chargestorm will continue at the last-written current limit
    -- until the operator changes it or the car unplugs, which is the
    -- right behaviour when the EMS loses contact — the user still
    -- gets their car charged. Matches the Easee driver's stance.
end

function driver_cleanup()
    -- No-op: leave the setpoint where it is so a service restart
    -- doesn't interrupt an active charging session.
end
