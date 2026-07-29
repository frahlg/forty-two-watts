-- Growatt Hybrid Inverter Driver
-- Emits: PV, Battery, Meter (READ-ONLY)
-- Protocol: Modbus TCP, INPUT registers (FC 0x04), Big-Endian word order
--
-- Ported from sourceful-hugin/device-support/drivers/lua/growatt.lua
-- for FTW Lua host v2.1.
--
-- Sign convention (site convention — positive W = INTO the site):
--   pv.w:       always negative (generation)
--   battery.w:  positive = charging, negative = discharging
--   meter.w:    positive = import from grid, negative = export
--
-- Growatt reports separate charge / discharge power registers; we
-- combine them into a signed site-convention value at the boundary.
-- The native meter register is already positive=import, so it maps
-- to site convention without a flip.

DRIVER = {
  host_api_min = 1,
  host_api_max = 1,
  id           = "growatt",
  name         = "Growatt hybrid inverter",
  manufacturer = "Growatt",
  version      = "2.1.1",
  protocols    = { "modbus" },
  capabilities = { "meter", "pv", "battery" },
  description  = "Growatt SPH / MOD hybrid inverters via Modbus TCP.",
  homepage     = "https://www.growatt.com",
  authors      = { "FTW contributors" },
  tested_models = { "SPH", "MOD" },
  verification_status = "experimental",
  verification_notes = "Ported from a reference implementation. Not yet verified against live hardware on a FTW site.",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}

PROTOCOL = "modbus"

----------------------------------------------------------------------------
-- Absent registers
----------------------------------------------------------------------------

-- Registers this device has stopped answering.
--
-- The host counts every failed host.modbus_read against the poll whether or
-- not this driver caught the error — "driver_poll: N of M modbus reads
-- failed". So a register retried on every poll costs a failed poll on every
-- poll, and the stale-telemetry watchdog takes the driver offline. The site
-- then reports nothing at all, which is worse than reporting one field less.
--
-- Three attempts absorb a transient blip; after that we stop asking. A
-- restart re-probes, so firmware that gains the register is picked up.
local GIVE_UP_AFTER = 3
local read_failures = {}

local function probe_read(addr, count, kind)
    if (read_failures[addr] or 0) >= GIVE_UP_AFTER then return nil end
    local ok, regs = pcall(host.modbus_read, addr, count, kind)
    if ok and regs and regs[1] ~= nil then
        read_failures[addr] = nil
        return regs
    end
    local failures = (read_failures[addr] or 0) + 1
    read_failures[addr] = failures
    if failures == GIVE_UP_AFTER then
        host.log("info", string.format(
            "Growatt: register %d did not answer %d times; leaving it alone " ..
            "until restart", addr, GIVE_UP_AFTER))
    end
    return nil
end

----------------------------------------------------------------------------
-- Initialization
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("Growatt")
    host.log("info", "Growatt: driver_init")
end

----------------------------------------------------------------------------
-- Telemetry polling
----------------------------------------------------------------------------

function driver_poll()
    -- ---- PV ----

    -- PV total power: 1-2, U32 BE × 0.1 W
    local pvw_regs = probe_read(1, 2, "input")
    local pv_w = 0
    if pvw_regs then
        pv_w = host.decode_u32_be(pvw_regs[1], pvw_regs[2]) * 0.1
    end

    -- PV1 V/A: 3-4, U16 × 0.1 V, U16 × 0.1 A
    local m1_regs = probe_read(3, 2, "input")
    local mppt1_v, mppt1_a = 0, 0
    if m1_regs then
        mppt1_v = m1_regs[1] * 0.1
        mppt1_a = m1_regs[2] * 0.1
    end

    -- PV2 V/A: 7-8, U16 × 0.1 V, U16 × 0.1 A
    local m2_regs = probe_read(7, 2, "input")
    local mppt2_v, mppt2_a = 0, 0
    if m2_regs then
        mppt2_v = m2_regs[1] * 0.1
        mppt2_a = m2_regs[2] * 0.1
    end

    -- Grid frequency: 37, U16 × 0.01 Hz
    local hz_regs = probe_read(37, 1, "input")
    local hz = 0
    if hz_regs then
        hz = hz_regs[1] * 0.01
    end

    -- Total PV lifetime energy: 91-92, U32 BE × 0.1 kWh → × 1000 for Wh
    local pvgen_regs = probe_read(91, 2, "input")
    local pv_gen_wh = 0
    if pvgen_regs then
        pv_gen_wh = host.decode_u32_be(pvgen_regs[1], pvgen_regs[2]) * 0.1 * 1000
    end

    -- Emit PV telemetry (site convention: generation is negative).
    host.emit("pv", {
        w           = -pv_w,
        mppt1_v     = mppt1_v,
        mppt1_a     = mppt1_a,
        mppt2_v     = mppt2_v,
        mppt2_a     = mppt2_a,
        lifetime_wh = pv_gen_wh,
    })
    host.emit_metric("pv_mppt1_v", mppt1_v)
    host.emit_metric("pv_mppt1_a", mppt1_a)
    host.emit_metric("pv_mppt2_v", mppt2_v)
    host.emit_metric("pv_mppt2_a", mppt2_a)
    host.emit_metric("grid_hz",    hz)

    -- ---- Battery ----

    -- Battery charge power: 1009-1010, U32 BE × 0.1 W
    local bchgw_regs = probe_read(1009, 2, "input")
    local bat_charge_w = 0
    if bchgw_regs then
        bat_charge_w = host.decode_u32_be(bchgw_regs[1], bchgw_regs[2]) * 0.1
    end

    -- Battery discharge power: 1011-1012, U32 BE × 0.1 W
    local bdisw_regs = probe_read(1011, 2, "input")
    local bat_discharge_w = 0
    if bdisw_regs then
        bat_discharge_w = host.decode_u32_be(bdisw_regs[1], bdisw_regs[2]) * 0.1
    end

    -- Net battery power: positive = charging, negative = discharging (site convention).
    local bat_w = bat_charge_w - bat_discharge_w

    -- Battery voltage: 1013, U16 × 0.1 V
    local bv_regs = probe_read(1013, 1, "input")
    local bat_v = 0
    if bv_regs then
        bat_v = bv_regs[1] * 0.1
    end

    -- Battery SoC: 1014, U16, percent → 0-1 fraction
    local bsoc_regs = probe_read(1014, 1, "input")
    local bat_soc = 0
    if bsoc_regs then
        bat_soc = bsoc_regs[1] / 100
    end

    -- Battery temperature: 1040, U16 × 0.1 °C
    local btemp_regs = probe_read(1040, 1, "input")
    local bat_temp = 0
    if btemp_regs then
        bat_temp = btemp_regs[1] * 0.1
    end

    host.emit("battery", {
        w      = bat_w,
        v      = bat_v,
        soc    = bat_soc,
        temp_c = bat_temp,
    })
    host.emit_metric("battery_dc_v",   bat_v)
    host.emit_metric("battery_temp_c", bat_temp)

    -- ---- Meter ----

    -- Meter total power: 1015-1016, I32 BE × 0.1 W
    -- Growatt sign: positive = import (matches site convention).
    local mw_regs = probe_read(1015, 2, "input")
    local meter_w = 0
    if mw_regs then
        meter_w = host.decode_i32_be(mw_regs[1], mw_regs[2]) * 0.1
    end

    -- Per-phase voltages: L1=38, L2=42, L3=46, U16 × 0.1 V
    local lv1_regs = probe_read(38, 1, "input")
    local l1_v = 0
    if lv1_regs then
        l1_v = lv1_regs[1] * 0.1
    end

    local lv2_regs = probe_read(42, 1, "input")
    local l2_v = 0
    if lv2_regs then
        l2_v = lv2_regs[1] * 0.1
    end

    local lv3_regs = probe_read(46, 1, "input")
    local l3_v = 0
    if lv3_regs then
        l3_v = lv3_regs[1] * 0.1
    end

    -- Per-phase currents: L1=39, L2=43, L3=47, U16 × 0.1 A
    local la1_regs = probe_read(39, 1, "input")
    local l1_a = 0
    if la1_regs then
        l1_a = la1_regs[1] * 0.1
    end

    local la2_regs = probe_read(43, 1, "input")
    local l2_a = 0
    if la2_regs then
        l2_a = la2_regs[1] * 0.1
    end

    local la3_regs = probe_read(47, 1, "input")
    local l3_a = 0
    if la3_regs then
        l3_a = la3_regs[1] * 0.1
    end

    -- Total import energy: 1021-1022, U32 BE × 0.1 kWh → × 1000 for Wh
    local imp_regs = probe_read(1021, 2, "input")
    local import_wh = 0
    if imp_regs then
        import_wh = host.decode_u32_be(imp_regs[1], imp_regs[2]) * 0.1 * 1000
    end

    -- Total export energy: 1029-1030, U32 BE × 0.1 kWh → × 1000 for Wh
    local exp_regs = probe_read(1029, 2, "input")
    local export_wh = 0
    if exp_regs then
        export_wh = host.decode_u32_be(exp_regs[1], exp_regs[2]) * 0.1 * 1000
    end

    host.emit("meter", {
        w         = meter_w,
        l1_v      = l1_v,
        l2_v      = l2_v,
        l3_v      = l3_v,
        l1_a      = l1_a,
        l2_a      = l2_a,
        l3_a      = l3_a,
        hz        = hz,
        import_wh = import_wh,
        export_wh = export_wh,
    })
    host.emit_metric("meter_l1_v", l1_v)
    host.emit_metric("meter_l2_v", l2_v)
    host.emit_metric("meter_l3_v", l3_v)
    host.emit_metric("meter_l1_a", l1_a)
    host.emit_metric("meter_l2_a", l2_a)
    host.emit_metric("meter_l3_a", l3_a)

    return 5000
end

----------------------------------------------------------------------------
-- Control (read-only driver — no battery control implemented)
----------------------------------------------------------------------------

-- Read-only driver: control is not implemented. Returning false lets the
-- control loop register the no-op without flooding the log — the EMS
-- sends a battery dispatch every tick even when power_w = 0.
function driver_command(action, power_w, cmd)
    return false
end

function driver_default_mode()
    -- Read-only driver: device manages itself autonomously.
end

function driver_cleanup()
    -- Nothing to clean up.
end
