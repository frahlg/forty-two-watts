-- solaredge_pv.lua
-- SolarEdge inverter driver, PV-only variant (SunSpec over Modbus TCP).
-- Emits: PV. READ-ONLY.
--
-- Clone of drivers/solaredge.lua with the SunSpec Model 203 meter block
-- stripped out — use this when the grid meter comes from a different
-- driver (e.g. the Pixii PowerShaper via its Model 203 chain) and you
-- only want PV generation from the SolarEdge inverter.

DRIVER = {
  id           = "solaredge-pv",
  name         = "SolarEdge inverter (PV only)",
  manufacturer = "SolarEdge",
  version      = "1.0.0",
  protocols    = { "modbus" },
  capabilities = { "pv" },
  description  = "SolarEdge HD-Wave / StorEdge PV-only via Modbus TCP (SunSpec).",
  homepage     = "https://www.solaredge.com",
  authors      = { "forty-two-watts contributors" },
  tested_models = { "HD-Wave", "StorEdge" },
}
--
-- SunSpec register map. This SolarEdge gateway serves FC 0x03 (holding)
-- only; FC 0x04 (input) times out, so every read uses "holding".
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
--     40095        Energy SF (I16)
--     40103        Heat-sink °C (I16)    * 10^temp_sf
--     40106        Temp SF (I16)
--     40123        MPPT current SF (I16)
--     40124        MPPT voltage SF (I16)
--     40140-40141  MPPT1 A/V (U16 each)
--     40160-40161  MPPT2 A/V (U16 each)
--
-- Sign translation to site convention (positive = into site):
--   AC power out of the inverter = generation → PV w = -ac_w.

PROTOCOL = "modbus"

-- Cached per-device metadata. Scale factors are factory-set constants
-- (SunSpec guarantees they never change during a session), so we read
-- them once and cache.
local sn_read = false
local sf_cache = nil

----------------------------------------------------------------------------
-- SunSpec helpers
----------------------------------------------------------------------------

local POW10 = {
    [-6] = 1e-6, [-5] = 1e-5, [-4] = 1e-4, [-3] = 1e-3,
    [-2] = 1e-2, [-1] = 1e-1, [0] = 1,
    [1] = 10, [2] = 100, [3] = 1000, [4] = 10000, [5] = 100000, [6] = 1e6,
}

-- SunSpec scale factors use 0x8000 (= -32768 after i16 decode) as a
-- "not implemented" sentinel. Treat that as 0 (don't scale).
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

function driver_poll()
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

    -- ---- Scale factors (cached one-shot) ----
    if sf_cache == nil then
        sf_cache = load_scale_factors()
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

    -- Lifetime energy: 40093-40094, U32 BE (Wh once scaled)
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

    -- MPPT2 A/V: 40160-40161, U16 each
    local ok_m2, m2_regs = pcall(host.modbus_read, 40160, 2, "holding")
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

    return 5000
end

----------------------------------------------------------------------------
-- Control (read-only driver — command stubs)
----------------------------------------------------------------------------

function driver_command(action, power_w, cmd)
    host.log("debug", "SolarEdge: read-only driver, ignoring action=" .. tostring(action))
    return false
end

function driver_default_mode()
end

function driver_cleanup()
end
