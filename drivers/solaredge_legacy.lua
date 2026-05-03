-- solaredge_legacy.lua
-- SolarEdge legacy K-series inverter driver (SunSpec over Modbus TCP).
-- Emits: PV. READ-ONLY. Targets older display-era inverters.
--
-- Cloned from drivers/solaredge.lua. Differences vs the HD-Wave driver:
--
--   * Reads use FC 0x03 (holding), not FC 0x04 (input). Older SolarEdge
--     firmware mirrors the SunSpec block only under holding registers;
--     issuing FC 0x04 against a K-series inverter (SE7K / SE10K / SE17K /
--     SE25K — the ones with the LCD on the inverter housing) times out
--     silently. Same diagnosis as drivers/solaredge_pv.lua, just for the
--     full inverter case.
--
--   * Meter block (SunSpec Model 203) is intentionally OMITTED in v1.
--     Newer HD-Wave firmware places the meter at 40190+, but K-series
--     firmware places it at 40121+ — and we don't yet have a verified
--     legacy-meter offset to ship. Operators who need grid-meter data
--     on a legacy install should pair this driver with a separate
--     meter driver (Pixii's Model 203 chain, an SDM630, etc.). Once
--     we have a confirmed K-series meter map we'll either extend
--     this driver or fold both into a unified driver with a
--     `function_code` config knob.
--
--   * driver_init runs a one-shot SunSpec ID probe on 40000-40003 to
--     verify the device is actually SunSpec-speaking before we trust
--     the rest of the register map. Pre-SunSpec firmware (very old
--     installations) won't reply to this and we log a clear failure
--     instead of silently emitting zeros forever.

DRIVER = {
  id           = "solaredge-legacy",
  name         = "SolarEdge legacy (K-series with display)",
  manufacturer = "SolarEdge",
  version      = "0.1.0",
  protocols    = { "modbus" },
  capabilities = { "pv" },
  description  = "SolarEdge K-series (SE7K / SE10K / SE17K / SE25K) PV inverter via Modbus TCP — uses FC 0x03 holding (legacy firmware doesn't mirror under FC 0x04 input).",
  homepage     = "https://www.solaredge.com",
  authors      = { "forty-two-watts contributors" },
  tested_models = { "SE17K (display, legacy firmware)" },
  verification_status = "experimental",
  verification_notes = "First-cut clone of solaredge.lua targeting K-series display inverters. Awaits field verification — 17K install reported can-connect / can't-read on the HD-Wave driver, this variant uses holding registers to address that.",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}

PROTOCOL = "modbus"

-- Cached per-device metadata. Scale factors are factory-set constants
-- (SunSpec guarantees they never change during a session), so we read
-- them once and cache. That cuts ~6 Modbus round trips per poll.
-- However, if the first read attempt fails (returns zeros), we retry on
-- subsequent polls until all SFs are non-zero or we exhaust retries.
local sn_read       = false
local sunspec_ok    = nil  -- true / false / nil (= not yet probed)
local sf_cache      = nil
local sf_retries    = 0
local SF_MAX_RETRIES = 5

----------------------------------------------------------------------------
-- SunSpec helpers
----------------------------------------------------------------------------

local POW10 = {
    [-6] = 1e-6, [-5] = 1e-5, [-4] = 1e-4, [-3] = 1e-3,
    [-2] = 1e-2, [-1] = 1e-1, [0] = 1,
    [1] = 10, [2] = 100, [3] = 1000, [4] = 10000, [5] = 100000, [6] = 1e6,
}

local function pow10(sf)
    if sf == -32768 then return 1 end
    local p = POW10[sf]
    if p ~= nil then return p end
    return 1
end

local function scale(value, sf)
    return value * pow10(sf)
end

local function read_sf(addr)
    local ok, regs = pcall(host.modbus_read, addr, 1, "holding")
    if ok and regs then
        return host.decode_i16(regs[1])
    end
    return 0
end

-- Subset of solaredge.lua's load_scale_factors — only the inverter-side
-- factors we actually use (the meter block is omitted in v1).
local function load_scale_factors()
    return {
        ac_power = read_sf(40084),
        hz       = read_sf(40086),
        energy   = read_sf(40095),
        temp     = read_sf(40106),
        mppt_a   = read_sf(40123),
        mppt_v   = read_sf(40124),
    }
end

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

-- One-shot SunSpec ID probe. SunSpec-compliant devices place the magic
-- bytes "SunS" (0x53756e53) at registers 40000-40001. Without them, the
-- rest of the register map we trust is meaningless — the inverter is
-- either pre-SunSpec firmware or a totally different device behind the
-- same TCP port (e.g. a meter-only gateway). Returns true once we've
-- confirmed SunSpec; subsequent calls short-circuit. Failure is sticky
-- *for this poll* but we re-probe on later polls in case the link was
-- transiently slow during the first attempt.
local function probe_sunspec()
    if sunspec_ok == true then return true end
    local ok, regs = pcall(host.modbus_read, 40000, 2, "holding")
    if not ok or not regs or #regs < 2 then
        sunspec_ok = false
        host.log("warn", "SolarEdge-legacy: SunSpec probe failed (read 40000-40001 returned nothing) — check unit_id and that the device speaks SunSpec over Modbus TCP")
        return false
    end
    -- "SunS" = 0x5375, 0x6e53 — SunSpec magic.
    local hi, lo = regs[1], regs[2]
    if hi == 0x5375 and lo == 0x6e53 then
        sunspec_ok = true
        host.log("info", "SolarEdge-legacy: SunSpec ID confirmed at 40000-40001")
        return true
    end
    sunspec_ok = false
    host.log("warn", string.format(
        "SolarEdge-legacy: SunSpec probe got 0x%04X 0x%04X (expected 0x5375 0x6e53) — device may be pre-SunSpec firmware or a non-SolarEdge gateway",
        hi, lo))
    return false
end

function driver_poll()
    -- ---- SunSpec sanity gate ----
    -- Refuse to emit anything until we've verified this is actually a
    -- SunSpec-speaking device. Otherwise a wrong unit_id or wrong host
    -- would silently produce a stream of "0 W generation" readings that
    -- the dashboard treats as legitimate.
    if not probe_sunspec() then
        return 30000  -- back off; SunSpec probe re-runs on next poll
    end

    -- ---- Serial number (SunSpec common block, one-shot) ----
    if not sn_read then
        local ok_sn, sn_regs = pcall(host.modbus_read, 40052, 16, "holding")
        if ok_sn and sn_regs then
            local sn = decode_ascii(sn_regs, 16)
            if #sn > 0 then
                host.set_sn(sn)
                sn_read = true
            end
        end
    end

    -- ---- Scale factors (cached with retry on zero reads) ----
    local need_sf_read = (sf_cache == nil)
    if not need_sf_read and sf_retries < SF_MAX_RETRIES then
        for _, v in pairs(sf_cache) do
            if v == 0 then need_sf_read = true; break end
        end
    end
    if need_sf_read then
        local fresh = load_scale_factors()
        if sf_cache == nil then
            sf_cache = fresh
        else
            for k, v in pairs(fresh) do
                if v ~= 0 then sf_cache[k] = v end
            end
        end
        sf_retries = sf_retries + 1
        if sf_retries >= SF_MAX_RETRIES then
            host.log("warn", "SolarEdge-legacy: accepting scale factors after "
                .. tostring(SF_MAX_RETRIES) .. " retries (some may be 0)")
        end
    end
    local sf = sf_cache

    -- ---- Inverter AC ----

    -- AC power: 40083, I16
    local ok_acw, acw_regs = pcall(host.modbus_read, 40083, 1, "holding")
    local ac_w = 0
    if ok_acw and acw_regs then
        ac_w = scale(host.decode_i16(acw_regs[1]), sf.ac_power)
    end

    -- Frequency: 40085, U16
    local ok_hz, hz_regs = pcall(host.modbus_read, 40085, 1, "holding")
    local hz = 0
    if ok_hz and hz_regs then
        hz = scale(hz_regs[1], sf.hz)
    end

    -- Lifetime energy: 40093-40094, U32 BE
    local ok_le, le_regs = pcall(host.modbus_read, 40093, 2, "holding")
    local lifetime_wh = 0
    if ok_le and le_regs then
        lifetime_wh = scale(host.decode_u32_be(le_regs[1], le_regs[2]), sf.energy)
    end

    -- Heat-sink temperature: 40103, I16
    local ok_temp, temp_regs = pcall(host.modbus_read, 40103, 1, "holding")
    local temp_c = 0
    if ok_temp and temp_regs then
        temp_c = scale(host.decode_i16(temp_regs[1]), sf.temp)
    end

    -- MPPT1 A/V: 40140-40141, U16 each
    local ok_m1, m1_regs = pcall(host.modbus_read, 40140, 2, "holding")
    local mppt1_a, mppt1_v = 0, 0
    if ok_m1 and m1_regs then
        mppt1_a = scale(m1_regs[1], sf.mppt_a)
        mppt1_v = scale(m1_regs[2], sf.mppt_v)
    end

    -- MPPT2 A/V: 40160-40161, U16 each. Larger K-series have two strings;
    -- single-string units will return zeros here, which is correct.
    local ok_m2, m2_regs = pcall(host.modbus_read, 40160, 2, "holding")
    local mppt2_a, mppt2_v = 0, 0
    if ok_m2 and m2_regs then
        mppt2_a = scale(m2_regs[1], sf.mppt_a)
        mppt2_v = scale(m2_regs[2], sf.mppt_v)
    end

    -- Site convention: generation is negative W.
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

    return 5000
end

----------------------------------------------------------------------------
-- Control (read-only driver — command stubs)
----------------------------------------------------------------------------

function driver_command(action, power_w, cmd)
    host.log("debug", "SolarEdge-legacy: read-only driver, ignoring action=" .. tostring(action))
    return false
end

function driver_default_mode()
    -- Read-only — nothing to revert.
end

function driver_cleanup()
    -- Read-only — nothing to clean up.
end
