-- CTEK Chargestorm EV Charger Driver
-- Emits: EV
-- Protocol: Modbus TCP (port 502, unit ID 1 = EVSE1, 2 = EVSE2)
--
-- Control surface verified at time of writing:
--   Register 4608 (0x1200), holding uint16 — set charging current in
--   amperes. Minimum 6 A (below that the charger stops, IEC 61851
--   limit). Writing 0 pauses charging; values 1-5 are treated as 0 in
--   this driver to match the Easee behaviour (silently-below-minimum
--   leaves the station stuck "awaiting current").
--
-- Telemetry registers (power / voltage / session energy / connector
-- state) are NOT yet mapped here — CTEK ships register docs under NDA
-- and the layout differs between CCU firmware versions. This driver
-- currently emits a placeholder `ev` reading so the station shows up
-- in the UI with its current-limit setpoint; add structured telemetry
-- reads incrementally as you confirm register numbers against your
-- firmware. See the TODO block in driver_poll.
--
-- The automation ModbusTCP server on the CCU (disabled by default,
-- enable via Automation/ModbusTCPEnable=True in the web UI) exposes
-- the internal energy meter at 0x1000+ for EVSE1 and 0x1100+ for
-- EVSE2. API version differs between firmwares (v1: 0x1000..0x1008,
-- v2: 0x2000..0x2008) — wire that in once you've confirmed the
-- version on your unit.
--
-- Sign convention (SITE = positive W flows INTO the site):
--   ev.w: positive when the car is pulling from the grid (which is
--   always, for an EVSE — there's no vehicle-to-grid path here).
--
-- Config example (config.yaml):
--   drivers:
--     - name: ctek
--       lua: drivers/ctek.lua
--       modbus:
--         host: 192.168.1.50
--         port: 502
--         unit_id: 1            # 1 = EVSE1, 2 = EVSE2
--       config:
--         phases:    3          # 1 or 3; default 3
--         min_a:     6          # minimum charge current (A); default 6
--         max_a:     16         # fuse-limited max (A); default 16
--         voltage_v: 230        # nominal per-phase voltage; default 230

DRIVER = {
  id           = "ctek-chargestorm",
  name         = "CTEK Chargestorm",
  manufacturer = "CTEK",
  version      = "0.1.0",
  protocols    = { "modbus" },
  capabilities = { "ev" },
  description  = "CTEK Chargestorm Connected 2/3 via Modbus TCP. Current-limit control at register 4608; extended telemetry pending.",
  homepage     = "https://www.ctek.com",
  authors      = { "forty-two-watts contributors" },
  tested_models = { "Chargestorm Connected 2" },
  verification_status = "beta",
  verification_notes = "Register 4608 current-limit write verified; power/state telemetry registers not yet mapped.",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}

PROTOCOL = "modbus"

-- Register 4608 (0x1200), holding uint16 — charge current setpoint in A.
local REG_SET_CURRENT = 4608

local phases    = 3
local min_a     = 6
local max_a     = 16
local voltage_v = 230

-- Last setpoint we successfully wrote. Used as the resume target when
-- ev_start / ev_resume come in without a specific current.
local last_set_a = 0

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

local function write_setpoint(amps)
    local ok, err = pcall(host.modbus_write, REG_SET_CURRENT, amps)
    if not ok then
        host.log("warn", "CTEK: write register " .. REG_SET_CURRENT
            .. " failed: " .. tostring(err))
        return false
    end
    last_set_a = amps
    return true
end

local function read_setpoint()
    local ok, regs = pcall(host.modbus_read, REG_SET_CURRENT, 1, "holding")
    if not ok or not regs or not regs[1] then return nil end
    return regs[1]
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
    end

    if min_a < 6 then min_a = 6 end         -- IEC 61851 floor
    if max_a < min_a then max_a = min_a end

    host.log("info", string.format(
        "CTEK: driver initialized (phases=%d, min=%dA, max=%dA, V=%.0f)",
        phases, min_a, max_a, voltage_v))

    local cur = read_setpoint()
    if cur then
        last_set_a = cur
        host.log("info", "CTEK: current setpoint readback = " .. tostring(cur) .. "A")
    end
end

function driver_poll()
    -- Read back the current-limit setpoint so the UI reflects whatever
    -- the EMS (or a manual override) last wrote.
    local setpoint = read_setpoint()
    if setpoint then last_set_a = setpoint end

    -- TODO(ctek-telemetry): fill in once the CCU register map for your
    -- firmware is confirmed. Candidate sources:
    --   - CCU "Automation" ModbusTCP server, enabled via
    --     Automation/ModbusTCPEnable. API v1 exposes the per-outlet
    --     energy meter at 0x1000..0x1008 (EVSE1) / 0x1100..0x1108
    --     (EVSE2); v2 at 0x2000..0x2008 / 0x2100..0x2108.
    --   - OEM register doc (CTEK distributes under NDA).
    -- Until those reads land, we can't truthfully report `w`,
    -- `connected`, or `charging`, so emit conservative placeholders:
    --   w=0, connected=false, charging=false.
    -- The dispatch clamp that prevents home batteries from feeding the
    -- car keys off `connected`/`charging`, so reporting them false
    -- here means the clamp won't kick in for CTEK sites yet. Map the
    -- state register before running this driver alongside a battery.

    host.emit("ev", {
        w         = 0,
        connected = false,
        charging  = false,
        max_a     = last_set_a,
        phases    = phases,
    })
    host.emit_metric("ev_set_current_a", last_set_a)

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
