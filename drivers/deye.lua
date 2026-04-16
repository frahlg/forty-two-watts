-- Deye Hybrid Inverter Driver
-- Ported from sourceful-hugin/device-support/drivers/lua/deye.lua
-- Emits: PV, Battery, Meter telemetry + battery control
-- Protocol: Modbus TCP (holding registers throughout)
-- Byte order: Little-Endian for multi-register U32 values

DRIVER = {
  id           = "deye",
  name         = "Deye hybrid inverter",
  manufacturer = "Deye",
  version      = "1.0.0",
  protocols    = { "modbus" },
  capabilities = { "meter", "pv", "battery" },
  description  = "Deye SUN-SG series hybrid inverters via Modbus. Auto-detects LV vs HV battery at init.",
  homepage     = "https://www.deyeinverter.com",
  authors      = { "forty-two-watts contributors" },
  tested_models = { "SUN-SG03LP1", "SUN-SG04LP3" },
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}
--
-- Register conventions (all holding, FC 0x03/0x06):
--   0        : device type code. High byte == 6 → HV battery variant.
--   3-7      : serial number, packed 2 bytes/register, big-endian within each word.
--   20-21    : rated power, U32 LE × 0.1 kW
--   141      : energy management mode (0=Selling First, 1=Zero Export To Load,
--              2=Zero Export To CT, 3=External EMS / forced)
--   143      : grid-charge enable (bit 0)
--   144      : power limit for forced charge/discharge (W)
--   217      : battery temperature, U16, actual C = (val - 1000) / 10
--   516-519  : battery charge/discharge energy counters, U32 LE × 0.1 kWh
--   522-525  : grid import/export energy, U32 LE × 0.1 kWh
--   534-535  : total PV generation, U32 LE × 0.1 kWh
--   541      : heatsink temperature, U16 × 0.1 C
--   587-591  : battery V/SoC/power/current
--   598-612  : per-phase V/current, grid frequency
--   619, 622 : total + per-phase meter power (I16, W)
--   672-679  : PV power + MPPT V/A
--
-- Sign convention (site view, see docs/site-convention.md):
--   pv.w      : always negative (generation flowing into the site)
--   battery.w : positive = charging, negative = discharging
--   meter.w   : positive = importing, negative = exporting

PROTOCOL = "modbus"

-- Cached across polls
local is_hv = false
local sn_read = false

----------------------------------------------------------------------------
-- Initialization
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("Deye")

    -- Detect HV vs LV up-front so the first poll already uses the right scales.
    local ok, mode_regs = pcall(host.modbus_read, 0, 1, "holding")
    if ok and mode_regs then
        local val = mode_regs[1]
        -- Lua 5.1: no bitwise ops; check high byte via floor div.
        is_hv = (val == 6) or (math.floor(val / 256) == 6)
        host.log("info", "Deye: device type 0x" .. string.format("%04x", val)
            .. " (" .. (is_hv and "HV" or "LV") .. " battery)")
    else
        host.log("warn", "Deye: device type read failed; assuming LV")
    end
end

----------------------------------------------------------------------------
-- Telemetry polling
----------------------------------------------------------------------------

function driver_poll()
    -- Read serial number once.  Registers 3..7 hold 10 ASCII bytes
    -- (2 chars per register, big-endian within each word).
    if not sn_read then
        local ok, sn_regs = pcall(host.modbus_read, 3, 5, "holding")
        if ok and sn_regs then
            local sn = ""
            for i = 1, 5 do
                local hi = math.floor(sn_regs[i] / 256)
                local lo = sn_regs[i] % 256
                if hi > 32 and hi < 127 then sn = sn .. string.char(hi) end
                if lo > 32 and lo < 127 then sn = sn .. string.char(lo) end
            end
            if #sn > 0 then
                host.set_sn(sn)
                sn_read = true
            end
        end
    end

    -- ---- PV ----

    -- PV1-PV4 power: 672-675, U16 each (×1 LV, ×10 HV)
    local ok_pvw, pvw_regs = pcall(host.modbus_read, 672, 4, "holding")
    local pv_total_w = 0
    if ok_pvw and pvw_regs then
        local pv_scale = is_hv and 10 or 1
        for i = 1, 4 do
            pv_total_w = pv_total_w + pvw_regs[i] * pv_scale
        end
    end

    -- MPPT1 V/A: 676-677, U16 × 0.1 each
    local ok_m1, m1_regs = pcall(host.modbus_read, 676, 2, "holding")
    local mppt1_v, mppt1_a = 0, 0
    if ok_m1 and m1_regs then
        mppt1_v = m1_regs[1] * 0.1
        mppt1_a = m1_regs[2] * 0.1
    end

    -- MPPT2 V/A: 678-679, U16 × 0.1 each
    local ok_m2, m2_regs = pcall(host.modbus_read, 678, 2, "holding")
    local mppt2_v, mppt2_a = 0, 0
    if ok_m2 and m2_regs then
        mppt2_v = m2_regs[1] * 0.1
        mppt2_a = m2_regs[2] * 0.1
    end

    -- Total generation: 534-535, U32 LE × 0.1 kWh
    local ok_gen, gen_regs = pcall(host.modbus_read, 534, 2, "holding")
    local pv_gen_wh = 0
    if ok_gen and gen_regs then
        pv_gen_wh = host.decode_u32_le(gen_regs[1], gen_regs[2]) * 0.1 * 1000
    end

    -- Rated power: 20-21, U32 LE × 0.1 kW
    local ok_rated, rated_regs = pcall(host.modbus_read, 20, 2, "holding")
    local rated_w = 0
    if ok_rated and rated_regs then
        rated_w = host.decode_u32_le(rated_regs[1], rated_regs[2]) * 0.1 * 1000
    end

    -- Heatsink temperature: 541, U16 × 0.1 C
    local ok_temp, temp_regs = pcall(host.modbus_read, 541, 1, "holding")
    local heatsink_c = 0
    if ok_temp and temp_regs then
        heatsink_c = temp_regs[1] * 0.1
    end

    host.emit("pv", {
        w           = -pv_total_w,  -- negate: PV generation is negative in site convention
        mppt1_v     = mppt1_v,
        mppt1_a     = mppt1_a,
        mppt2_v     = mppt2_v,
        mppt2_a     = mppt2_a,
        lifetime_wh = pv_gen_wh,
        rated_w     = rated_w,
        temp_c      = heatsink_c,
    })
    host.emit_metric("pv_mppt1_v",      mppt1_v)
    host.emit_metric("pv_mppt1_a",      mppt1_a)
    host.emit_metric("pv_mppt2_v",      mppt2_v)
    host.emit_metric("pv_mppt2_a",      mppt2_a)
    host.emit_metric("inverter_temp_c", heatsink_c)

    -- ---- Battery ----

    -- Battery voltage: 587, U16 (×0.01 LV, ×0.1 HV)
    local ok_bv, bv_regs = pcall(host.modbus_read, 587, 1, "holding")
    local bat_v = 0
    if ok_bv and bv_regs then
        bat_v = bv_regs[1] * (is_hv and 0.1 or 0.01)
    end

    -- Battery SoC: 588, U16 percent → 0..1 fraction
    local ok_bsoc, bsoc_regs = pcall(host.modbus_read, 588, 1, "holding")
    local bat_soc = 0
    if ok_bsoc and bsoc_regs then
        bat_soc = bsoc_regs[1] / 100
    end

    -- Battery power: 590, I16 (×1 LV, ×10 HV).  Deye native: positive = charging.
    local ok_bw, bw_regs = pcall(host.modbus_read, 590, 1, "holding")
    local bat_w = 0
    if ok_bw and bw_regs then
        local bat_scale = is_hv and 10 or 1
        bat_w = host.decode_i16(bw_regs[1]) * bat_scale
    end

    -- Battery current: 591, I16 × 0.01 A
    local ok_ba, ba_regs = pcall(host.modbus_read, 591, 1, "holding")
    local bat_a = 0
    if ok_ba and ba_regs then
        bat_a = host.decode_i16(ba_regs[1]) * 0.01
    end

    -- Battery temperature: 217, U16 offset-encoded (actual = (val-1000)/10)
    local ok_btemp, btemp_regs = pcall(host.modbus_read, 217, 1, "holding")
    local bat_temp = 0
    if ok_btemp and btemp_regs then
        bat_temp = (btemp_regs[1] - 1000) / 10
    end

    -- Battery charge energy: 516-517, U32 LE × 0.1 kWh
    local ok_bchg, bchg_regs = pcall(host.modbus_read, 516, 2, "holding")
    local bat_charge_wh = 0
    if ok_bchg and bchg_regs then
        bat_charge_wh = host.decode_u32_le(bchg_regs[1], bchg_regs[2]) * 0.1 * 1000
    end

    -- Battery discharge energy: 518-519, U32 LE × 0.1 kWh
    local ok_bdis, bdis_regs = pcall(host.modbus_read, 518, 2, "holding")
    local bat_discharge_wh = 0
    if ok_bdis and bdis_regs then
        bat_discharge_wh = host.decode_u32_le(bdis_regs[1], bdis_regs[2]) * 0.1 * 1000
    end

    -- Deye reports positive = charging already, which matches site convention.
    host.emit("battery", {
        w            = bat_w,
        v            = bat_v,
        a            = bat_a,
        soc          = bat_soc,
        temp_c       = bat_temp,
        charge_wh    = bat_charge_wh,
        discharge_wh = bat_discharge_wh,
    })
    host.emit_metric("battery_dc_v",   bat_v)
    host.emit_metric("battery_dc_a",   bat_a)
    host.emit_metric("battery_temp_c", bat_temp)

    -- ---- Meter ----

    -- Per-phase voltage: 598-600, U16 × 0.1 V
    local ok_lv, lv_regs = pcall(host.modbus_read, 598, 3, "holding")
    local l1_v, l2_v, l3_v = 0, 0, 0
    if ok_lv and lv_regs then
        l1_v = lv_regs[1] * 0.1
        l2_v = lv_regs[2] * 0.1
        l3_v = lv_regs[3] * 0.1
    end

    -- Grid frequency: 609, U16 × 0.01 Hz
    local ok_hz, hz_regs = pcall(host.modbus_read, 609, 1, "holding")
    local hz = 0
    if ok_hz and hz_regs then
        hz = hz_regs[1] * 0.01
    end

    -- Per-phase current: 610-612, I16 × 0.01 A
    local ok_la, la_regs = pcall(host.modbus_read, 610, 3, "holding")
    local l1_a, l2_a, l3_a = 0, 0, 0
    if ok_la and la_regs then
        l1_a = host.decode_i16(la_regs[1]) * 0.01
        l2_a = host.decode_i16(la_regs[2]) * 0.01
        l3_a = host.decode_i16(la_regs[3]) * 0.01
    end

    -- Total meter power: 619, I16 W (positive = import, negative = export)
    local ok_tw, tw_regs = pcall(host.modbus_read, 619, 1, "holding")
    local meter_w = 0
    if ok_tw and tw_regs then
        meter_w = host.decode_i16(tw_regs[1])
    end

    -- Per-phase power: 622-624, I16 W
    local ok_lw, lw_regs = pcall(host.modbus_read, 622, 3, "holding")
    local l1_w, l2_w, l3_w = 0, 0, 0
    if ok_lw and lw_regs then
        l1_w = host.decode_i16(lw_regs[1])
        l2_w = host.decode_i16(lw_regs[2])
        l3_w = host.decode_i16(lw_regs[3])
    end

    -- Import energy: 522-523, U32 LE × 0.1 kWh
    local ok_imp, imp_regs = pcall(host.modbus_read, 522, 2, "holding")
    local import_wh = 0
    if ok_imp and imp_regs then
        import_wh = host.decode_u32_le(imp_regs[1], imp_regs[2]) * 0.1 * 1000
    end

    -- Export energy: 524-525, U32 LE × 0.1 kWh
    local ok_exp, exp_regs = pcall(host.modbus_read, 524, 2, "holding")
    local export_wh = 0
    if ok_exp and exp_regs then
        export_wh = host.decode_u32_le(exp_regs[1], exp_regs[2]) * 0.1 * 1000
    end

    host.emit("meter", {
        w         = meter_w,
        l1_w      = l1_w,
        l2_w      = l2_w,
        l3_w      = l3_w,
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
    host.emit_metric("meter_l1_w", l1_w)
    host.emit_metric("meter_l2_w", l2_w)
    host.emit_metric("meter_l3_w", l3_w)
    host.emit_metric("meter_l1_v", l1_v)
    host.emit_metric("meter_l2_v", l2_v)
    host.emit_metric("meter_l3_v", l3_v)
    host.emit_metric("meter_l1_a", l1_a)
    host.emit_metric("meter_l2_a", l2_a)
    host.emit_metric("meter_l3_a", l3_a)
    host.emit_metric("grid_hz",    hz)

    return 5000
end

----------------------------------------------------------------------------
-- Battery control
----------------------------------------------------------------------------
--
-- Deye's control model:
--   141 : Energy Management Mode
--           0 = Selling First        (default autonomous self-consumption)
--           1 = Zero Export To Load
--           2 = Zero Export To CT
--           3 = External EMS / Forced
--   143 : Grid-charge enable        (0 = off, 1 = on)
--   144 : Forced power limit (W)     — interpretation depends on mode 3
--
-- In external-EMS mode (141=3) the inverter treats reg 144 as an absolute
-- power target and reg 143 as the direction hint (1 → pull from grid to
-- charge; 0 → push to load/grid to discharge).  Going back to mode 0
-- restores the inverter's native self-consumption behaviour.

local REG_EMS_MODE    = 141
local REG_GRID_CHARGE = 143
local REG_POWER_LIMIT = 144

local MODE_SELLING_FIRST = 0
local MODE_EXTERNAL_EMS  = 3

-- Return to the inverter's native self-consumption mode.
local function set_self_consumption()
    local ok1 = pcall(host.modbus_write, REG_GRID_CHARGE, 0)
    local ok2 = pcall(host.modbus_write, REG_EMS_MODE,    MODE_SELLING_FIRST)
    if not (ok1 and ok2) then
        host.log("warn", "Deye: self-consumption write failed")
        return false
    end
    host.log("debug", "Deye: self-consumption mode")
    return true
end

-- Set battery to a specific charge/discharge power (site convention).
-- power_w > 0 : charge at power_w W
-- power_w < 0 : discharge at |power_w| W
-- power_w = 0 : revert to self-consumption
local function set_battery_power(power_w)
    if power_w == 0 then
        return set_self_consumption()
    end

    local watts = math.floor(math.min(math.abs(power_w), 12000))
    local charge_dir = (power_w > 0) and 1 or 0

    local ok1 = pcall(host.modbus_write, REG_POWER_LIMIT, watts)
    local ok2 = pcall(host.modbus_write, REG_GRID_CHARGE, charge_dir)
    local ok3 = pcall(host.modbus_write, REG_EMS_MODE,    MODE_EXTERNAL_EMS)

    if not (ok1 and ok2 and ok3) then
        host.log("warn", "Deye: battery control write failed")
        return false
    end

    host.log("debug", string.format("Deye: %s %dW",
        charge_dir == 1 and "force charge" or "force discharge", watts))
    return true
end

function driver_command(action, power_w, cmd)
    if action == "init" then
        return true
    elseif action == "battery" then
        return set_battery_power(power_w)
    elseif action == "curtail_disable" or action == "deinit" then
        return set_self_consumption()
    end
    host.log("debug", "Deye: unsupported action: " .. tostring(action))
    return false
end

-- Watchdog fallback: always revert to autonomous self-consumption so the
-- device doesn't get stuck in a forced mode when the EMS goes offline.
function driver_default_mode()
    host.log("info", "Deye: watchdog → reverting to self-consumption")
    set_self_consumption()
end

function driver_cleanup()
    set_self_consumption()
    -- Reset cached state so a reload starts from a clean slate.
    is_hv = false
    sn_read = false
end
