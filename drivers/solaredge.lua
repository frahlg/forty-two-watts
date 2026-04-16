-- solaredge.lua
-- SolarEdge inverter + meter driver (SunSpec over Modbus TCP).
-- Emits: PV, Meter. READ-ONLY. Tested on HD-Wave and StorEdge.
--
-- Ported from sourceful-hugin/device-support/drivers/lua/solaredge.lua.
-- Differences vs hugin source:
--   * Uses 42W v2.1 host idiom (host.log(level,msg), decode_u32_be,
--     host.emit_metric).
--   * SunSpec scale factor + pow10 applied inline in Lua (host.scale is
--     not available in v2.1).
--   * Adds SunSpec common-block SN read (register 40052, 16 regs ASCII)
--     so device identity resolves to make:serial.
--   * Diagnostics (MPPT, heatsink, grid Hz, per-phase) routed through
--     host.emit_metric into the long-format TS DB.

DRIVER = {
  id           = "solaredge",
  name         = "SolarEdge inverter + meter",
  manufacturer = "SolarEdge",
  version      = "1.0.0",
  protocols    = { "modbus" },
  capabilities = { "meter", "pv" },
  description  = "SolarEdge HD-Wave / StorEdge via Modbus TCP (SunSpec).",
  homepage     = "https://www.solaredge.com",
  authors      = { "forty-two-watts contributors" },
  tested_models = { "HD-Wave", "StorEdge" },
}
--
-- SunSpec register map (FC 0x04 / "input" on SolarEdge; they intentionally
-- mirror the SunSpec common + inverter + meter blocks there):
--
--   Common block (device identity):
--     40052-40067  SN (16 regs, ASCII, null-padded)
--
--   Inverter model (101/102/103):
--     40083        AC power (I16)        * 10^ac_power_sf
--     40084        AC power SF (I16)
--     40085        Frequency (U16)       * 10^hz_sf
--     40086        Frequency SF (I16)
--     40093-40094  Lifetime Wh (U32 BE)  * 10^energy_sf
--     40095        Energy SF (I16 via i16 decode; datasheet calls it U16)
--     40103        Heat-sink °C (I16)    * 10^temp_sf
--     40106        Temp SF (I16)
--     40123        MPPT current SF (I16)
--     40124        MPPT voltage SF (I16)
--     40140-40141  MPPT1 A/V (U16 each)
--     40160-40161  MPPT2 A/V (U16 each)
--
--   Meter model (203 — 3-phase wye):
--     40100        Total W (I16)         * 10^meter_w_sf
--     40101        Meter W SF (I16)
--     40191-40193  Per-phase A (I16)     * 10^meter_a_sf
--     40194        Meter A SF (I16)
--     40196-40198  Per-phase V (I16)     * 10^meter_v_sf
--     40203        Meter V SF (I16)
--     40207-40209  Per-phase W (I16)     * 10^phase_w_sf
--     40210        Phase W SF (I16)
--     40226-40227  Export Wh (U32 BE)    * 10^meter_energy_sf
--     40234-40235  Import Wh (U32 BE)    * 10^meter_energy_sf
--     40242        Meter energy SF (I16)
--
-- Sign translation to site convention (positive = into site):
--   AC power out of the inverter = generation → PV w = -ac_w.
--   SolarEdge meter reports with utility-meter convention inverted
--   (+ = export in their datasheet), so we negate W and A to match
--   site convention (+ = import).

PROTOCOL = "modbus"

-- Cached per-device metadata. Scale factors are factory-set constants
-- (SunSpec guarantees they never change during a session), so we read
-- them once and cache. That cuts 11 Modbus round trips per poll.
-- However, if the first read attempt fails (returns zeros), we retry on
-- subsequent polls until all SFs are non-zero or we exhaust retries.
local sn_read = false
local sf_cache = nil
local sf_retries = 0
local SF_MAX_RETRIES = 5

----------------------------------------------------------------------------
-- SunSpec helpers
----------------------------------------------------------------------------

-- Raw integer power of ten — avoids math.pow (Lua 5.1 still has it, but
-- 5.3+ removed it and we prefer portable code). Scale factors are small
-- integers, typically -3..+3, so a fixed table is fastest and clearest.
local POW10 = {
    [-6] = 1e-6, [-5] = 1e-5, [-4] = 1e-4, [-3] = 1e-3,
    [-2] = 1e-2, [-1] = 1e-1, [0] = 1,
    [1] = 10, [2] = 100, [3] = 1000, [4] = 10000, [5] = 100000, [6] = 1e6,
}

-- SunSpec scale factors use 0x8000 (= -32768 after i16 decode) as a
-- "not implemented" sentinel. Treat that, and any out-of-range sf, as 0
-- (i.e. don't scale) — better to report a raw register than NaN out.
local function pow10(sf)
    if sf == -32768 then return 1 end
    local p = POW10[sf]
    if p ~= nil then return p end
    return 1
end

-- Apply a SunSpec scale factor inline:  value * 10^sf.
-- value may be any lua number (already decoded i16/u16/u32/i32).
local function scale(value, sf)
    return value * pow10(sf)
end

-- Read a single register and return it as an i16 (signed) scale factor.
-- Returns 0 on read failure so downstream scaling becomes a no-op (the
-- caller will get raw register values until the next retry).
local function read_sf(addr)
    local ok, regs = pcall(host.modbus_read, addr, 1, "input")
    if ok and regs then
        return host.decode_i16(regs[1])
    end
    return 0
end

-- Populate sf_cache with every SunSpec scale factor we need. Returns the
-- table whether or not every read succeeded — a failed read just leaves
-- that SF at 0 until we retry (see load_scale_factors call site).
local function load_scale_factors()
    return {
        ac_power     = read_sf(40084),
        hz           = read_sf(40086),
        energy       = read_sf(40095),
        temp         = read_sf(40106),
        mppt_a       = read_sf(40123),
        mppt_v       = read_sf(40124),
        meter_w      = read_sf(40101),
        meter_a      = read_sf(40194),
        meter_v      = read_sf(40203),
        phase_w      = read_sf(40210),
        meter_energy = read_sf(40242),
    }
end

-- Decode a null-/space-padded ASCII string from a run of registers
-- (SunSpec common-block strings — high byte first inside each reg).
local function decode_ascii(regs, n)
    local s = ""
    for i = 1, n do
        local r = regs[i] or 0
        local hi = math.floor(r / 256)
        local lo = r % 256
        if hi > 32 and hi < 127 then s = s .. string.char(hi) end
        if lo > 32 and lo < 127 then s = s .. string.char(lo) end
    end
    return s
end

----------------------------------------------------------------------------
-- Lifecycle
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("SolarEdge")
end

function driver_poll()
    -- ---- Serial number (SunSpec common block, one-shot) ----
    if not sn_read then
        local ok_sn, sn_regs = pcall(host.modbus_read, 40052, 16, "input")
        if ok_sn and sn_regs then
            local sn = decode_ascii(sn_regs, 16)
            if #sn > 0 then
                host.set_sn(sn)
                sn_read = true
            end
        end
    end

    -- ---- Scale factors (cached with retry on zero reads) ----
    -- A failed modbus read returns 0 from read_sf(), which would cause raw
    -- register values to be emitted unscaled — wrong by orders of magnitude.
    -- Re-read all SFs until none are 0 or we exhaust retries.
    local need_sf_read = (sf_cache == nil)
    if not need_sf_read and sf_retries < SF_MAX_RETRIES then
        for _, v in pairs(sf_cache) do
            if v == 0 then need_sf_read = true; break end
        end
    end
    if need_sf_read then
        sf_cache = load_scale_factors()
        sf_retries = sf_retries + 1
        if sf_retries >= SF_MAX_RETRIES then
            host.log("warn", "SolarEdge: accepting scale factors after "
                .. tostring(SF_MAX_RETRIES) .. " retries (some may be 0)")
        end
    end
    local sf = sf_cache

    -- ---- Inverter AC ----

    -- AC power: 40083, I16
    local ok_acw, acw_regs = pcall(host.modbus_read, 40083, 1, "input")
    local ac_w = 0
    if ok_acw and acw_regs then
        ac_w = scale(host.decode_i16(acw_regs[1]), sf.ac_power)
    end

    -- Frequency: 40085, U16
    local ok_hz, hz_regs = pcall(host.modbus_read, 40085, 1, "input")
    local hz = 0
    if ok_hz and hz_regs then
        hz = scale(hz_regs[1], sf.hz)
    end

    -- Lifetime energy: 40093-40094, U32 BE (Wh once scaled)
    local ok_le, le_regs = pcall(host.modbus_read, 40093, 2, "input")
    local lifetime_wh = 0
    if ok_le and le_regs then
        lifetime_wh = scale(host.decode_u32_be(le_regs[1], le_regs[2]), sf.energy)
    end

    -- Heat-sink temperature: 40103, I16
    local ok_temp, temp_regs = pcall(host.modbus_read, 40103, 1, "input")
    local temp_c = 0
    if ok_temp and temp_regs then
        temp_c = scale(host.decode_i16(temp_regs[1]), sf.temp)
    end

    -- MPPT1 A/V: 40140-40141, U16 each
    local ok_m1, m1_regs = pcall(host.modbus_read, 40140, 2, "input")
    local mppt1_a, mppt1_v = 0, 0
    if ok_m1 and m1_regs then
        mppt1_a = scale(m1_regs[1], sf.mppt_a)
        mppt1_v = scale(m1_regs[2], sf.mppt_v)
    end

    -- MPPT2 A/V: 40160-40161, U16 each
    local ok_m2, m2_regs = pcall(host.modbus_read, 40160, 2, "input")
    local mppt2_a, mppt2_v = 0, 0
    if ok_m2 and m2_regs then
        mppt2_a = scale(m2_regs[1], sf.mppt_a)
        mppt2_v = scale(m2_regs[2], sf.mppt_v)
    end

    -- Emit PV (site convention: generation is negative W)
    host.emit("pv", {
        w           = -ac_w,
        mppt1_v     = mppt1_v,
        mppt1_a     = mppt1_a,
        mppt2_v     = mppt2_v,
        mppt2_a     = mppt2_a,
        lifetime_wh = lifetime_wh,
        temp_c      = temp_c,
    })
    host.emit_metric("pv_mppt1_v",      mppt1_v)
    host.emit_metric("pv_mppt1_a",      mppt1_a)
    host.emit_metric("pv_mppt2_v",      mppt2_v)
    host.emit_metric("pv_mppt2_a",      mppt2_a)
    host.emit_metric("inverter_temp_c", temp_c)
    host.emit_metric("grid_hz",         hz)

    -- ---- Meter ----

    -- Total W: 40100, I16
    local ok_mw, mw_regs = pcall(host.modbus_read, 40100, 1, "input")
    local meter_w = 0
    if ok_mw and mw_regs then
        meter_w = scale(host.decode_i16(mw_regs[1]), sf.meter_w)
    end

    -- Per-phase current: 40191-40193, I16 each
    local ok_la, la_regs = pcall(host.modbus_read, 40191, 3, "input")
    local l1_a, l2_a, l3_a = 0, 0, 0
    if ok_la and la_regs then
        l1_a = scale(host.decode_i16(la_regs[1]), sf.meter_a)
        l2_a = scale(host.decode_i16(la_regs[2]), sf.meter_a)
        l3_a = scale(host.decode_i16(la_regs[3]), sf.meter_a)
    end

    -- Per-phase voltage: 40196-40198, I16 each
    local ok_lv, lv_regs = pcall(host.modbus_read, 40196, 3, "input")
    local l1_v, l2_v, l3_v = 0, 0, 0
    if ok_lv and lv_regs then
        l1_v = scale(host.decode_i16(lv_regs[1]), sf.meter_v)
        l2_v = scale(host.decode_i16(lv_regs[2]), sf.meter_v)
        l3_v = scale(host.decode_i16(lv_regs[3]), sf.meter_v)
    end

    -- Per-phase power: 40207-40209, I16 each
    local ok_lw, lw_regs = pcall(host.modbus_read, 40207, 3, "input")
    local l1_w, l2_w, l3_w = 0, 0, 0
    if ok_lw and lw_regs then
        l1_w = scale(host.decode_i16(lw_regs[1]), sf.phase_w)
        l2_w = scale(host.decode_i16(lw_regs[2]), sf.phase_w)
        l3_w = scale(host.decode_i16(lw_regs[3]), sf.phase_w)
    end

    -- Export energy: 40226-40227, U32 BE
    local ok_exp, exp_regs = pcall(host.modbus_read, 40226, 2, "input")
    local export_wh = 0
    if ok_exp and exp_regs then
        export_wh = scale(host.decode_u32_be(exp_regs[1], exp_regs[2]), sf.meter_energy)
    end

    -- Import energy: 40234-40235, U32 BE
    local ok_imp, imp_regs = pcall(host.modbus_read, 40234, 2, "input")
    local import_wh = 0
    if ok_imp and imp_regs then
        import_wh = scale(host.decode_u32_be(imp_regs[1], imp_regs[2]), sf.meter_energy)
    end

    -- SolarEdge meter reports with sign inverted vs site convention.
    -- Site: + = into site (import). SolarEdge: + = out (export).
    -- So negate W and A to flip to site convention. V and Hz are unsigned.
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
        hz        = hz,
        import_wh = import_wh,
        export_wh = export_wh,
    })
    host.emit_metric("meter_l1_w", -l1_w)
    host.emit_metric("meter_l2_w", -l2_w)
    host.emit_metric("meter_l3_w", -l3_w)
    host.emit_metric("meter_l1_v", l1_v)
    host.emit_metric("meter_l2_v", l2_v)
    host.emit_metric("meter_l3_v", l3_v)
    host.emit_metric("meter_l1_a", -l1_a)
    host.emit_metric("meter_l2_a", -l2_a)
    host.emit_metric("meter_l3_a", -l3_a)

    return 5000
end

----------------------------------------------------------------------------
-- Control (read-only driver — command stubs)
----------------------------------------------------------------------------

function driver_command(action, power_w, cmd)
    -- debug level on purpose: the controller may probe every cycle.
    host.log("debug", "SolarEdge: read-only driver, ignoring action=" .. tostring(action))
    return false
end

function driver_default_mode()
    -- Read-only — nothing to revert.
end

function driver_cleanup()
    -- Read-only — nothing to clean up.
end
