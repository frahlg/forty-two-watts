-- Pixii PowerShaper Driver
-- Emits: Battery, Meter
-- Protocol: Modbus TCP — SunSpec-compliant commercial battery storage
-- Register type: ALL HOLDING (FC 0x03)
-- Uses SunSpec scale factors (signed i16 exponents → value * 10^sf)
--
-- Ported from sourceful-hugin/device-support/drivers/lua/pixii.lua
-- (read-only; control path not yet implemented).

DRIVER = {
  id           = "pixii",
  name         = "Pixii PowerShaper",
  manufacturer = "Pixii",
  version      = "1.0.0",
  protocols    = { "modbus" },
  capabilities = { "battery", "meter" },
  description  = "Pixii PowerShaper commercial battery storage via Modbus TCP.",
  homepage     = "https://pixii.com",
  authors      = { "forty-two-watts contributors" },
  tested_models = { "PowerShaper" },
}
--
-- Sign convention (SITE = positive W flows INTO the site):
--   Battery w: positive = charging  (load), negative = discharging (source)
--   Meter   w: positive = importing,        negative = exporting
--
-- Pixii native meter reports negative = import (utility-perspective), so we
-- negate meter power/current to line up with the site convention.

PROTOCOL = "modbus"

local sn_read = false

----------------------------------------------------------------------------
-- Helpers
----------------------------------------------------------------------------

-- SunSpec scale factor: value * 10^sf, where sf is a signed int16 exponent.
local function scale(v, sf)
    if sf == 0 then return v end
    return v * (10 ^ sf)
end

-- Read a single i16-typed scale factor register, returning 0 on error.
local function read_sf(addr)
    local ok, regs = pcall(host.modbus_read, addr, 1, "holding")
    if ok and regs then return host.decode_i16(regs[1]) end
    return 0
end

-- Decode a SunSpec ASCII string from a block of u16 registers. Stops at
-- the first NUL byte and strips trailing whitespace.
local function decode_ascii(regs, count)
    local s = ""
    for i = 1, count do
        local hi = math.floor(regs[i] / 256)
        local lo = regs[i] % 256
        if hi == 0 and lo == 0 then break end
        if hi > 32 and hi < 127 then s = s .. string.char(hi) end
        if lo > 32 and lo < 127 then s = s .. string.char(lo) end
    end
    return s
end

----------------------------------------------------------------------------
-- Initialization
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("Pixii")

    -- Verify SunSpec signature at 40000 ("SunS") as a sanity check.
    local ok, sig = pcall(host.modbus_read, 40000, 2, "holding")
    if ok and sig then
        local want = "SunS"
        local got = decode_ascii(sig, 2)
        if got ~= want then
            host.log("warn", "Pixii: unexpected SunSpec header '" .. got .. "' at 40000")
        end
    end
end

----------------------------------------------------------------------------
-- Telemetry polling
----------------------------------------------------------------------------

function driver_poll()
    -- Read serial number once from SunSpec Common Model (offset 52 from
    -- the common block → absolute 40052, 16 regs ASCII).
    if not sn_read then
        local ok, sn_regs = pcall(host.modbus_read, 40052, 16, "holding")
        if ok and sn_regs then
            local sn = decode_ascii(sn_regs, 16)
            if string.len(sn) > 0 then
                host.set_sn(sn)
                sn_read = true
            end
        end
    end

    -- ---- Scale Factors (all i16 exponents) ----
    local ac_w_sf        = read_sf(40084)
    local hz_sf          = read_sf(40086)
    local temp_sf        = read_sf(40106)
    local soc_sf         = read_sf(40177)
    local bat_v_sf       = read_sf(40180)
    local bat_a_sf       = read_sf(40182)
    local bat_w_sf       = read_sf(40184)
    local meter_a_sf     = read_sf(40240)
    local meter_v_sf     = read_sf(40249)
    local meter_hz_sf    = read_sf(40251)
    local meter_w_sf     = read_sf(40256)
    local meter_energy_sf = read_sf(40288)

    -- ---- Battery Values ----

    -- AC power (inverter): 40083, I16  (diagnostic only; bat_w below is DC)
    local ok_acw, acw_regs = pcall(host.modbus_read, 40083, 1, "holding")
    local ac_w = 0
    if ok_acw and acw_regs then
        ac_w = scale(host.decode_i16(acw_regs[1]), ac_w_sf)
    end

    -- Inverter frequency: 40085, U16
    local ok_hz, hz_regs = pcall(host.modbus_read, 40085, 1, "holding")
    local inv_hz = 0
    if ok_hz and hz_regs then
        inv_hz = scale(hz_regs[1], hz_sf)
    end

    -- Inverter temperature: 40102, I16 (°C)
    local ok_temp, temp_regs = pcall(host.modbus_read, 40102, 1, "holding")
    local temp_c = 0
    if ok_temp and temp_regs then
        temp_c = scale(host.decode_i16(temp_regs[1]), temp_sf)
    end

    -- Battery SoC: 40132, U16 (percent → fraction 0..1)
    local ok_soc, soc_regs = pcall(host.modbus_read, 40132, 1, "holding")
    local bat_soc = 0
    if ok_soc and soc_regs then
        bat_soc = scale(soc_regs[1], soc_sf) / 100
    end

    -- Battery voltage: 40155, I16
    local ok_bv, bv_regs = pcall(host.modbus_read, 40155, 1, "holding")
    local bat_v = 0
    if ok_bv and bv_regs then
        bat_v = scale(host.decode_i16(bv_regs[1]), bat_v_sf)
    end

    -- Battery current: 40165, I16
    local ok_ba, ba_regs = pcall(host.modbus_read, 40165, 1, "holding")
    local bat_a = 0
    if ok_ba and ba_regs then
        bat_a = scale(host.decode_i16(ba_regs[1]), bat_a_sf)
    end

    -- Battery DC power: 40168, I16  (SunSpec: positive = charge, so site-conv)
    local ok_bw, bw_regs = pcall(host.modbus_read, 40168, 1, "holding")
    local bat_w = 0
    if ok_bw and bw_regs then
        bat_w = scale(host.decode_i16(bw_regs[1]), bat_w_sf)
    end

    -- Cabinet charge/discharge energy: 39958-39961, two I32 BE pairs, kWh
    local ok_cab, cab_regs = pcall(host.modbus_read, 39958, 4, "holding")
    local bat_charge_wh, bat_discharge_wh = 0, 0
    if ok_cab and cab_regs then
        bat_charge_wh    = host.decode_i32_be(cab_regs[1], cab_regs[2]) * 1000
        bat_discharge_wh = host.decode_i32_be(cab_regs[3], cab_regs[4]) * 1000
    end

    host.emit("battery", {
        w            = bat_w,
        v            = bat_v,
        a            = bat_a,
        soc          = bat_soc,
        temp_c       = temp_c,
        charge_wh    = bat_charge_wh,
        discharge_wh = bat_discharge_wh,
    })
    -- Diagnostics: long-format TS DB
    host.emit_metric("battery_dc_v",      bat_v)
    host.emit_metric("battery_dc_a",      bat_a)
    host.emit_metric("battery_ac_w",      ac_w)
    host.emit_metric("inverter_temp_c",   temp_c)
    host.emit_metric("inverter_hz",       inv_hz)

    -- ---- Meter Values ----

    -- Per-phase current: 40237-40239, I16 each
    local ok_la, la_regs = pcall(host.modbus_read, 40237, 3, "holding")
    local l1_a, l2_a, l3_a = 0, 0, 0
    if ok_la and la_regs then
        l1_a = scale(host.decode_i16(la_regs[1]), meter_a_sf)
        l2_a = scale(host.decode_i16(la_regs[2]), meter_a_sf)
        l3_a = scale(host.decode_i16(la_regs[3]), meter_a_sf)
    end

    -- Per-phase voltage: 40242-40244, I16 each
    local ok_lv, lv_regs = pcall(host.modbus_read, 40242, 3, "holding")
    local l1_v, l2_v, l3_v = 0, 0, 0
    if ok_lv and lv_regs then
        l1_v = scale(host.decode_i16(lv_regs[1]), meter_v_sf)
        l2_v = scale(host.decode_i16(lv_regs[2]), meter_v_sf)
        l3_v = scale(host.decode_i16(lv_regs[3]), meter_v_sf)
    end

    -- Meter frequency: 40250, U16
    local ok_mhz, mhz_regs = pcall(host.modbus_read, 40250, 1, "holding")
    local meter_hz = 0
    if ok_mhz and mhz_regs then
        meter_hz = scale(mhz_regs[1], meter_hz_sf)
    end

    -- Total meter power: 40252, I16
    local ok_mw, mw_regs = pcall(host.modbus_read, 40252, 1, "holding")
    local meter_w = 0
    if ok_mw and mw_regs then
        meter_w = scale(host.decode_i16(mw_regs[1]), meter_w_sf)
    end

    -- Per-phase meter power: 40253-40255, I16 each
    local ok_lpw, lpw_regs = pcall(host.modbus_read, 40253, 3, "holding")
    local l1_w, l2_w, l3_w = 0, 0, 0
    if ok_lpw and lpw_regs then
        l1_w = scale(host.decode_i16(lpw_regs[1]), meter_w_sf)
        l2_w = scale(host.decode_i16(lpw_regs[2]), meter_w_sf)
        l3_w = scale(host.decode_i16(lpw_regs[3]), meter_w_sf)
    end

    -- Export energy: 40272-40275, U32 BE (two regs consumed for the value)
    local ok_exp, exp_regs = pcall(host.modbus_read, 40272, 4, "holding")
    local export_wh = 0
    if ok_exp and exp_regs then
        export_wh = scale(host.decode_u32_be(exp_regs[1], exp_regs[2]), meter_energy_sf)
    end

    -- Import energy: 40280-40283, U32 BE
    local ok_imp, imp_regs = pcall(host.modbus_read, 40280, 4, "holding")
    local import_wh = 0
    if ok_imp and imp_regs then
        import_wh = scale(host.decode_u32_be(imp_regs[1], imp_regs[2]), meter_energy_sf)
    end

    -- Pixii native: negative=import. Site convention: positive=import.
    -- Negate W and A (voltage is always positive magnitude, leave alone).
    host.emit("meter", {
        w         = -meter_w,
        l1_w      = -l1_w,
        l2_w      = -l2_w,
        l3_w      = -l3_w,
        l1_v      = l1_v,
        l2_v      = l2_v,
        l3_v      = l3_v,
        l1_a      = -l1_a,
        l2_a      = -l2_a,
        l3_a      = -l3_a,
        hz        = meter_hz,
        import_wh = import_wh,
        export_wh = export_wh,
    })
    host.emit_metric("meter_l1_w", -l1_w)
    host.emit_metric("meter_l2_w", -l2_w)
    host.emit_metric("meter_l3_w", -l3_w)
    host.emit_metric("meter_l1_v",  l1_v)
    host.emit_metric("meter_l2_v",  l2_v)
    host.emit_metric("meter_l3_v",  l3_v)
    host.emit_metric("meter_l1_a", -l1_a)
    host.emit_metric("meter_l2_a", -l2_a)
    host.emit_metric("meter_l3_a", -l3_a)
    host.emit_metric("grid_hz",     meter_hz)

    return 5000
end

----------------------------------------------------------------------------
-- Control (read-only — not implemented)
----------------------------------------------------------------------------

function driver_command(action, power_w, cmd)
    host.log("info", "Pixii control not yet implemented: " .. tostring(action))
    return false
end

function driver_default_mode()
    -- No-op: read-only driver, nothing to revert.
end

function driver_cleanup()
    -- Nothing to clean up.
end
