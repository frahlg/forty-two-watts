-- fronius.lua
-- Fronius Symo / Primo GEN24 hybrid inverter driver
-- Emits: PV, Battery, Meter
-- Protocol: Modbus TCP (SunSpec), ALL HOLDING registers
-- Ported from sourceful-hugin/device-support/drivers/lua/fronius.lua
-- Port notes (v2.1 API drift vs hugin):
--   host.log(msg)           → host.log("info", msg)
--   host.decode_u32/i32     → _be variants
--   host.decode_f32         → inline IEEE-754 (two u16, big-endian words)
--   host.scale(v, sf)       → local apply_sf(v, sf)

DRIVER = {
  host_api_min = 1,
  host_api_max = 1,
  id           = "fronius",
  name         = "Fronius GEN24",
  manufacturer = "Fronius",
  version      = "2.1.1",
  protocols    = { "modbus" },
  capabilities = { "pv", "battery" },
  description  = "Fronius Symo / Primo GEN24 hybrid inverters via Modbus TCP (SunSpec).",
  homepage     = "https://www.fronius.com",
  authors      = { "FTW contributors" },
  tested_models = { "Symo GEN24", "Primo GEN24" },
  verification_status = "experimental",
  verification_notes = "Ported from a reference implementation. Not yet verified against live hardware on a FTW site.",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}
--
-- Register conventions:
--   All registers read as FC 0x03 (holding).
--   Inverter scalar values (AC power, frequency, DC power, per-phase V/A)
--   are SunSpec IEEE 754 float32, two consecutive u16 registers, big-endian
--   word order (high word first).
--   MPPT, battery, and rated-power values are u16/i16 raw + a signed
--   int16 scale factor in a separate register.
--
-- Sign convention (site/EMS):
--   meter.w  : positive = importing from grid, negative = exporting
--   pv.w     : always <= 0 (generation pushes to site)
--   battery.w: positive = charging (into battery), negative = discharging

PROTOCOL = "modbus"

local sn_read = false

----------------------------------------------------------------------------
-- Local decoders (replace host.decode_f32 / host.scale from hugin v1.x)
----------------------------------------------------------------------------

-- Apply a SunSpec int16 scale factor: value * 10^sf.
-- sf is typically in the range [-3, 3]; split on sign to avoid 10^(-n)
-- rounding quirks.
local function apply_sf(v, sf)
    if sf >= 0 then
        return v * (10 ^ sf)
    else
        return v / (10 ^ -sf)
    end
end

-- Decode IEEE 754 single-precision float from two u16 registers,
-- big-endian word order: hi = first register, lo = second.
-- Treats NaN / ±Inf (SunSpec "not implemented" sentinel 0x7FC00000) as 0.
local function decode_f32_be(hi, lo)
    local bits = (hi % 0x10000) * 0x10000 + (lo % 0x10000)
    if bits == 0 then return 0 end
    local sign = (bits >= 0x80000000) and -1 or 1
    local exp = math.floor(bits / 0x800000) % 0x100
    local frac = bits % 0x800000
    if exp == 0xFF then return 0 end
    if exp == 0 then return sign * frac * 2 ^ -149 end
    return sign * (1 + frac / 0x800000) * 2 ^ (exp - 127)
end

-- Turn a Lua array of u16s into an ASCII string, trimming NULs and spaces.
local function regs_to_string(regs, count)
    if not regs then return "" end
    local s = ""
    for i = 1, count do
        local r = regs[i] or 0
        local hi = math.floor(r / 256)
        local lo = r % 256
        if hi > 32 and hi < 127 then s = s .. string.char(hi) end
        if lo > 32 and lo < 127 then s = s .. string.char(lo) end
    end
    return s
end

----------------------------------------------------------------------------
-- Initialization
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
            "Fronius: register %d did not answer %d times; leaving it alone " ..
            "until restart", addr, GIVE_UP_AFTER))
    end
    return nil
end

function driver_init(config)
    host.set_make("Fronius")
end

----------------------------------------------------------------------------
-- Telemetry polling
----------------------------------------------------------------------------

function driver_poll()
    -- Read SunSpec Common block serial number once (40052, 16 regs = 32 chars).
    -- Some gateways don't expose the common block; fall back silently.
    if not sn_read then
        local sn_regs = probe_read(40052, 16, "holding")
        if sn_regs then
            local sn = regs_to_string(sn_regs, 16)
            if #sn > 0 then
                host.set_sn(sn)
                sn_read = true
            end
        end
    end

    -- ---------------------------------------------------------------- SFs
    -- Scale factors live in separate i16 registers.

    local rated_w_sf = 0
    local rwsf_regs = probe_read(40135, 1, "holding")
    if rwsf_regs then rated_w_sf = host.decode_i16(rwsf_regs[1]) end

    local mppt_a_sf = 0
    local masf_regs = probe_read(40265, 1, "holding")
    if masf_regs then mppt_a_sf = host.decode_i16(masf_regs[1]) end

    local mppt_v_sf = 0
    local mvsf_regs = probe_read(40266, 1, "holding")
    if mvsf_regs then mppt_v_sf = host.decode_i16(mvsf_regs[1]) end

    local max_charge_sf = 0
    local mcsf_regs = probe_read(40331, 1, "holding")
    if mcsf_regs then max_charge_sf = host.decode_i16(mcsf_regs[1]) end

    local soc_sf = 0
    local socsf_regs = probe_read(40335, 1, "holding")
    if socsf_regs then soc_sf = host.decode_i16(socsf_regs[1]) end

    local bat_v_sf = 0
    local bvsf_regs = probe_read(40337, 1, "holding")
    if bvsf_regs then bat_v_sf = host.decode_i16(bvsf_regs[1]) end

    local charge_rate_sf = 0
    local crsf_regs = probe_read(40338, 1, "holding")
    if crsf_regs then charge_rate_sf = host.decode_i16(crsf_regs[1]) end

    -- ---------------------------------------------------------------- PV
    -- Inverter/AC values are SunSpec F32 BE pairs.

    local ac_w = 0
    local acw_regs = probe_read(40091, 2, "holding")
    if acw_regs then ac_w = decode_f32_be(acw_regs[1], acw_regs[2]) end

    local hz = 0
    local hz_regs = probe_read(40093, 2, "holding")
    if hz_regs then hz = decode_f32_be(hz_regs[1], hz_regs[2]) end

    local lifetime_wh = 0
    local le_regs = probe_read(40101, 2, "holding")
    if le_regs then lifetime_wh = decode_f32_be(le_regs[1], le_regs[2]) end

    local dc_w = 0
    local dcw_regs = probe_read(40107, 2, "holding")
    if dcw_regs then dc_w = decode_f32_be(dcw_regs[1], dcw_regs[2]) end

    local heatsink_c = 0
    local temp_regs = probe_read(40111, 2, "holding")
    if temp_regs then heatsink_c = decode_f32_be(temp_regs[1], temp_regs[2]) end

    -- Rated W: 40134, U16 raw × 10^rated_w_sf
    local rated_w = 0
    local rw_regs = probe_read(40134, 1, "holding")
    if rw_regs then rated_w = apply_sf(rw_regs[1], rated_w_sf) end

    -- MPPT1 A/V: 40282-40283, U16 each
    local mppt1_a, mppt1_v = 0, 0
    local m1_regs = probe_read(40282, 2, "holding")
    if m1_regs then
        mppt1_a = apply_sf(m1_regs[1], mppt_a_sf)
        mppt1_v = apply_sf(m1_regs[2], mppt_v_sf)
    end

    -- MPPT2 A/V: 40302-40303, U16 each
    local mppt2_a, mppt2_v = 0, 0
    local m2_regs = probe_read(40302, 2, "holding")
    if m2_regs then
        mppt2_a = apply_sf(m2_regs[1], mppt_a_sf)
        mppt2_v = apply_sf(m2_regs[2], mppt_v_sf)
    end

    -- Per-phase AC current: 40073, 40075, 40077 (F32 BE pairs)
    local l1_a, l2_a, l3_a = 0, 0, 0
    local l1a_regs = probe_read(40073, 2, "holding")
    if l1a_regs then l1_a = decode_f32_be(l1a_regs[1], l1a_regs[2]) end
    local l2a_regs = probe_read(40075, 2, "holding")
    if l2a_regs then l2_a = decode_f32_be(l2a_regs[1], l2a_regs[2]) end
    local l3a_regs = probe_read(40077, 2, "holding")
    if l3a_regs then l3_a = decode_f32_be(l3a_regs[1], l3a_regs[2]) end

    -- Per-phase AC voltage: 40085, 40087, 40089 (F32 BE pairs)
    local l1_v, l2_v, l3_v = 0, 0, 0
    local l1v_regs = probe_read(40085, 2, "holding")
    if l1v_regs then l1_v = decode_f32_be(l1v_regs[1], l1v_regs[2]) end
    local l2v_regs = probe_read(40087, 2, "holding")
    if l2v_regs then l2_v = decode_f32_be(l2v_regs[1], l2v_regs[2]) end
    local l3v_regs = probe_read(40089, 2, "holding")
    if l3v_regs then l3_v = decode_f32_be(l3v_regs[1], l3v_regs[2]) end

    -- Emit PV (dc_w magnitude → negative for generation, per site convention)
    host.emit("pv", {
        w           = -dc_w,
        mppt1_v     = mppt1_v,
        mppt1_a     = mppt1_a,
        mppt2_v     = mppt2_v,
        mppt2_a     = mppt2_a,
        lifetime_wh = lifetime_wh,
        temp_c      = heatsink_c,
        rated_w     = rated_w,
    })
    -- Diagnostics: long-format TS DB
    host.emit_metric("pv_mppt1_v",      mppt1_v)
    host.emit_metric("pv_mppt1_a",      mppt1_a)
    host.emit_metric("pv_mppt2_v",      mppt2_v)
    host.emit_metric("pv_mppt2_a",      mppt2_a)
    host.emit_metric("inverter_temp_c", heatsink_c)
    host.emit_metric("grid_hz",         hz)

    -- ----------------------------------------------------------- Battery

    -- Max charge power: 40315, U16 raw × 10^max_charge_sf
    local max_charge_w = 0
    local maxchg_regs = probe_read(40315, 1, "holding")
    if maxchg_regs then
        max_charge_w = apply_sf(maxchg_regs[1], max_charge_sf)
    end

    -- Battery SoC: 40321, U16 raw (percent), convert to 0..1 fraction
    local bat_soc = 0
    local bsoc_regs = probe_read(40321, 1, "holding")
    if bsoc_regs then
        bat_soc = apply_sf(bsoc_regs[1], soc_sf) / 100
    end

    -- Battery voltage: 40323, U16 raw × 10^bat_v_sf
    local bat_v = 0
    local batv_regs = probe_read(40323, 1, "holding")
    if batv_regs then
        bat_v = apply_sf(batv_regs[1], bat_v_sf)
    end

    -- Discharge rate % (I16): 40325
    local discharge_rate = 0
    local dis_regs = probe_read(40325, 1, "holding")
    if dis_regs then
        discharge_rate = apply_sf(host.decode_i16(dis_regs[1]), charge_rate_sf)
    end

    -- Charge rate % (I16): 40326
    local charge_rate = 0
    local chg_regs = probe_read(40326, 1, "holding")
    if chg_regs then
        charge_rate = apply_sf(host.decode_i16(chg_regs[1]), charge_rate_sf)
    end

    -- Site convention: positive W = charging, negative W = discharging.
    -- Fronius reports direction as two separate percent rates + max power.
    local bat_w = 0
    if discharge_rate > 0 then
        bat_w = -(discharge_rate / 100) * max_charge_w
    elseif charge_rate > 0 then
        bat_w = (charge_rate / 100) * max_charge_w
    end

    host.emit("battery", {
        w   = bat_w,
        v   = bat_v,
        soc = bat_soc,
    })
    host.emit_metric("battery_dc_v", bat_v)

    -- NOTE: This driver does NOT emit meter telemetry. A Fronius inverter is
    -- NOT a grid meter — its AC output represents inverter production, not the
    -- grid boundary. Operators need a separate meter driver (fronius_smart_meter.lua,
    -- sdm630.lua, p1_meter.lua, etc.) with `is_site_meter: true` in config.yaml
    -- for accurate grid power readings.

    return 5000
end

----------------------------------------------------------------------------
-- Control (READ-ONLY driver — Fronius GEN24 battery control via SunSpec
-- requires the Modbus-control licence key and is not implemented here.)
----------------------------------------------------------------------------

function driver_command(action, power_w, cmd)
    if action == "init" or action == "deinit" then
        return true
    end
    host.log("warn", "Fronius: read-only driver, ignoring action=" .. tostring(action))
    return false
end

function driver_default_mode()
    -- Read-only: nothing to revert. Inverter runs its own self-consumption.
end

function driver_cleanup()
    sn_read = false
end
