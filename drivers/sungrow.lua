-- Sungrow SH Hybrid Inverter Driver
-- Emits: PV, Battery, Meter
-- Protocol: Modbus TCP (port 502, unit ID 1)
-- Reference: https://github.com/mkaiser/Sungrow-SHx-Inverter-Modbus-Home-Assistant

DRIVER = {
  host_api_min = 1,
  host_api_max = 1,
  id           = "sungrow-shx",
  name         = "Sungrow SH Hybrid Inverter",
  manufacturer = "Sungrow",
  version      = "1.5.4",
  protocols    = { "modbus" },
  capabilities = { "meter", "pv", "battery", "pv-curtail" },
  description  = "Sungrow SH-series hybrid inverters with LFP battery, via Modbus TCP.",
  homepage     = "https://en.sungrowpower.com",
  authors      = { "FTW contributors" },
  tested_models = { "SH5.0RT", "SH6.0RT", "SH8.0RT", "SH10RT" },
  verification_status = "production",
  verified_by = { "frahlg@homelab-rpi:14d" },
  verified_at = "2026-04-18",
  verification_notes = "Battery control + telemetry in continuous use on homelab-rpi. Device type 0x0E0E (SH8.0RT-V112).",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}
--
-- Register conventions:
--   Input registers (FC 0x04): read-only telemetry
--   Holding registers (FC 0x03/0x06): read/write configuration & control
--   Multi-register values: Little-Endian word order (low word first)
--
-- Battery sign convention (EMS unified):
--   positive W = charging (grid → battery)
--   negative W = discharging (battery → grid)
--
-- Host addresses are zero-based (the Sungrow protocol's documented register
-- number minus one):
--   13049: EMS mode (documented 13050; 0=self, 2=forced, 3=external EMS)
--   13050: Force cmd (documented 13051; AA=charge, BB=discharge, CC=stop)
--   13051: Force power (documented 13052; watts)
--   13073: Feed-in limit value (documented 13074; watts)
--   13086: Feed-in limit enable (documented 13087; read-only to this driver)
--   13088: Active power limit enable (documented 13089; AA=on, 55=off)
--   13089: Active power limit ratio (documented 13090; 0.1%)
--   33046: Max charge power (documented 33047; scale 0.01 kW)
--   33047: Max discharge power (documented 33048; scale 0.01 kW)
--   13057: Max SoC (documented 13058; scale 0.1%)
--   13058: Min SoC (documented 13059; scale 0.1%)

PROTOCOL = "modbus"

local sn_read = false
local SN_ATTEMPTS = 3
local sn_tries = 0
local init_verified = false
local rated_ac_w = 0
local pv_curtail_control_enabled = false
local pv_curtail_active = false
local pv_curtail_method = "active_power"
local feed_in_release_w = 0
local latest_non_export_w = 0

local function first_write_error(a, b, c)
    if a ~= nil and a ~= "" then return tostring(a) end
    if b ~= nil and b ~= "" then return tostring(b) end
    if c ~= nil and c ~= "" then return tostring(c) end
    return nil
end

local function bit_is_set(value, bit)
    return math.floor(value / (2 ^ bit)) % 2 == 1
end

----------------------------------------------------------------------------
-- Model family
--
-- Sungrow ships two families behind one driver and they do not share a
-- register map. SH hybrids answer the 13xxx block: battery, running state,
-- energy counters. SG string inverters have no battery and do not answer any
-- of it.
--
-- The host counts every failed read against the poll and one failure fails the
-- whole poll, so reading the hybrid block on a string inverter does not merely
-- return nothing -- it takes the site offline. An SG12RT lost all telemetry
-- this way: 12 of 19 reads failed on every poll.
----------------------------------------------------------------------------

-- The device type code says which family this is. 0x0Dxx and 0x0Exx are the SH
-- hybrids; 0 and 0xFFFF are the "not present" sentinel and mean nothing.
-- Shared with driver_fingerprint so there is one classification, not two that
-- can drift apart.
local function classify_device_type(code)
    if code == nil or code == 0 or code == 0xFFFF then return nil end
    local family = math.floor(code / 256)
    if family == 0x0D or family == 0x0E then return "hybrid" end
    return "string"
end

local model_family = nil -- "hybrid", "string", "unknown", or nil while asking

-- Set once the battery registers have actually answered. The device type code
-- is the fast answer to "does this have a battery"; this is the slow one, and
-- it is the only one available when register 4999 never answers. The control
-- path needs it: `unknown` is a state the read path may probe its way out of,
-- because a failed read is bounded and self-correcting, while a write to a
-- register the model does not implement is neither.
local battery_confirmed = false

-- Detection has to give up. Retrying a register the inverter never answers
-- costs one failed read on every poll for the rest of the session, which is
-- the same outage by a slower route.
local DETECT_ATTEMPTS = 3
local detect_tries = 0

local function detect_model_family()
    detect_tries = detect_tries + 1
    local ok, regs = pcall(host.modbus_read, 4999, 1, "input")
    local family = nil
    if ok and regs ~= nil then family = classify_device_type(regs[1]) end
    if family then return family end
    if detect_tries >= DETECT_ATTEMPTS then
        host.log("info", "Sungrow: register 4999 never gave a usable device "
            .. "type; probing the hybrid block instead")
        return "unknown"
    end
    return nil
end

-- Whether the 13xxx block is worth reading. While detection is still running
-- we hold off rather than probe: it normally settles on the first poll, so a
-- hybrid loses nothing, and a string inverter is spared a burst of failed
-- reads it cannot afford. Only once detection has given up do we probe.
local function hybrid_block_worth_reading()
    return model_family == "hybrid" or model_family == "unknown"
end

-- A register that answers on one model and not another. Absence has to be
-- proved: a single timeout or a busy bus must not silence a register for the
-- rest of the session, so only a run of failures counts.
--
-- Every read driver_poll makes goes through here, including the 5xxx block
-- that every family is supposed to answer. "Supposed to" is not a guarantee:
-- a register the inverter never answers costs a failed read on every poll
-- forever, and the stale-telemetry watchdog then takes the whole site offline
-- while this driver is still reporting good numbers for everything else. The
-- control paths below stay on a plain pcall -- they run only when a command
-- arrives, so they cost the poll nothing, and a poll-time miss must not veto
-- a later command without trying.
local MISSES_BEFORE_SKIP = 3
local miss_counts = {}

local function optional_read(address, count, kind)
    if (miss_counts[address] or 0) >= MISSES_BEFORE_SKIP then
        return nil
    end
    local ok, registers = pcall(host.modbus_read, address, count, kind)
    -- Check every register asked for, not just the first. A short reply leaves
    -- the rest nil, and arithmetic on nil ends the whole poll.
    if ok and registers ~= nil then
        for i = 1, count do
            if registers[i] == nil then ok = false break end
        end
    end
    if ok and registers ~= nil then
        miss_counts[address] = nil
        return registers
    end
    local misses = (miss_counts[address] or 0) + 1
    miss_counts[address] = misses
    if misses >= MISSES_BEFORE_SKIP then
        host.log("info", "Sungrow: register " .. address .. " failed "
            .. misses .. " reads in a row; treating it as absent")
    end
    return nil
end

----------------------------------------------------------------------------
-- Fingerprint
----------------------------------------------------------------------------

-- driver_fingerprint() — passive probe used by /api/drivers/fingerprint to
-- auto-detect what's listening on a Modbus endpoint. Reads ONLY the
-- read-only device-type input register (5000 / addr 4999); never writes.
-- Tri-state:
--   true  → device-type code is in the SH hybrid family (0x0Dxx / 0x0Exx,
--           e.g. 0x0E0E = SH10RT)
--   false → answered Modbus with a clean code that isn't a Sungrow hybrid
--   nil    → couldn't read (wrong unit id, not Modbus, timeout) or the code
--           reads back as the 0 / 0xFFFF "not present" sentinel
function driver_fingerprint()
    local ok, regs = pcall(host.modbus_read, 4999, 1, "input")
    if not ok or regs == nil then
        return nil
    end
    local code = regs[1]
    local family = classify_device_type(code)
    if family == nil then
        return nil -- unreadable, or the 0 / 0xFFFF sentinel — inconclusive
    end
    if family == "hybrid" then
        return true, {
            make = "Sungrow",
            model = string.format("0x%04X", code),
            confidence = 0.9,
        }
    end
    return false -- responded, but not a Sungrow SH hybrid signature
end

----------------------------------------------------------------------------
-- Initialization
----------------------------------------------------------------------------

function driver_init(config)
    config = config or {}
    host.set_make("Sungrow")

    -- The generic supports_pv_curtail opt-in is injected by the host as a
    -- runtime-only key. It gives FTW ownership of the Active Power Limitation
    -- registers without touching a separately configured feed-in/export limit.
    pv_curtail_control_enabled = config._supports_pv_curtail == true
    if config.pv_curtail_method == "feed_in" then
        pv_curtail_method = "feed_in"
    end
    feed_in_release_w = tonumber(config.feed_in_release_w) or 0

    -- Read the device type once here and remember which family it is. This
    -- read already happened; it just threw the answer away after logging it,
    -- which is why an SG string inverter went on to be read with the hybrid
    -- map. driver_poll retries while the family is still unknown.
    detect_tries = detect_tries + 1
    local ok, dev = pcall(host.modbus_read, 4999, 1, "input")
    if ok and dev then
        host.log("info", "Device type code: " .. tostring(dev[1]))
        model_family = classify_device_type(dev[1])
        if model_family then
            host.log("info", "Sungrow: detected a " .. model_family .. " inverter")
        end
    end

    -- Cache rated AC power before the first poll so a manual curtail command
    -- can be converted from watts to Sungrow's 0.1% ratio immediately.
    local ok_rated, rated = pcall(host.modbus_read, 5000, 1, "input")
    if ok_rated and rated and rated[1] and rated[1] > 0 then
        rated_ac_w = rated[1] * 0.1 * 1000
    end

    -- A container update can terminate while the inverter still holds the
    -- last forced charge/discharge command. Always establish the safe native
    -- mode before the driver is admitted to dispatch, including a zeroed
    -- stale power register. Verification failures remain visible in logs and
    -- the normal watchdog still has a chance to retry default mode.
    if not set_self_consumption() then
        host.log("warn", "Sungrow: startup control-state reset did not verify")
    end

    -- Only operators who opted into PV curtail give FTW ownership of a limit.
    -- Clear a stale FTW limit after an abrupt restart. Active-power mode owns
    -- its enable+ratio pair; explicit feed-in mode restores only the configured
    -- absolute release value and never changes the installer's enable flag.
    if pv_curtail_control_enabled and not set_pv_curtail_disabled() then
        host.log("warn", "Sungrow: startup PV curtail release did not verify")
    end

    -- Verify and configure power limits for battery control
    configure_power_limits()

    -- Read current EMS mode and log it
    local ok_ems, ems = pcall(host.modbus_read, 13049, 3, "holding")
    if ok_ems and ems then
        host.log("info", "EMS state: mode=" .. tostring(ems[1])
            .. " cmd=0x" .. string.format("%04x", ems[2])
            .. " power=" .. tostring(ems[3]) .. "W")
    end
end

-- Ensure power limits allow full charge/discharge (5kW each)
-- Some Sungrow units ship with discharge capped at 100W
function configure_power_limits()
    -- Max charge power: register 33046, scale 0.01 kW
    local ok_chg, chg = pcall(host.modbus_read, 33046, 1, "holding")
    if ok_chg and chg then
        local chg_kw = chg[1] * 0.01
        host.log("info", "Max charge power: " .. string.format("%.2f", chg_kw) .. " kW")
        if chg[1] < 500 then
            host.log("info", "Setting max charge power to 5 kW")
            local err = host.modbus_write(33046, 500)
            if err ~= nil and err ~= "" then
                host.log("warn", "Sungrow: max charge power write failed: " .. tostring(err))
            end
        end
    end

    -- Max discharge power: register 33047, scale 0.01 kW
    local ok_dis, dis = pcall(host.modbus_read, 33047, 1, "holding")
    if ok_dis and dis then
        local dis_kw = dis[1] * 0.01
        host.log("info", "Max discharge power: " .. string.format("%.2f", dis_kw) .. " kW")
        if dis[1] < 500 then
            host.log("info", "Setting max discharge power to 5 kW")
            local err = host.modbus_write(33047, 500)
            if err ~= nil and err ~= "" then
                host.log("warn", "Sungrow: max discharge power write failed: " .. tostring(err))
            end
        end
    end

    -- Read SoC limits
    local ok_soc, soc_lim = pcall(host.modbus_read, 13057, 2, "holding")
    if ok_soc and soc_lim then
        host.log("info", "SoC limits: max=" .. string.format("%.1f", soc_lim[1] * 0.1)
            .. "% min=" .. string.format("%.1f", soc_lim[2] * 0.1) .. "%")
    end

    init_verified = true
end

----------------------------------------------------------------------------
-- Telemetry polling
----------------------------------------------------------------------------

function driver_poll()
    -- driver_init already asked once. Keep asking while the answer is unknown,
    -- up to DETECT_ATTEMPTS, then settle on probing.
    if model_family == nil and detect_tries < DETECT_ATTEMPTS then
        model_family = detect_model_family()
        if model_family then
            host.log("info", "Sungrow: detected a " .. model_family .. " inverter")
        end
    elseif model_family == nil then
        model_family = "unknown"
    end

    -- Read serial number once. Bounded like the device type: an inverter that
    -- never answers this would otherwise cost a failed read on every poll.
    if not sn_read and sn_tries < SN_ATTEMPTS then
        sn_tries = sn_tries + 1
        local ok, sn_regs = pcall(host.modbus_read, 4990, 10, "input")
        if ok and sn_regs then
            local sn = ""
            for i = 1, 10 do
                if sn_regs[i] == nil then break end
                local hi = math.floor(sn_regs[i] / 256)
                local lo = sn_regs[i] % 256
                if hi > 32 and hi < 127 then sn = sn .. string.char(hi) end
                if lo > 32 and lo < 127 then sn = sn .. string.char(lo) end
            end
            if string.len(sn) > 0 then
                host.set_sn(sn)
                sn_read = true
            end
        end
    end

    -- Sungrow running state is documented register 13000 (host address
    -- 12999). Power-flow status is documented 13001 (host address 13000):
    -- bit 2 = discharging, bit 1 = charging. Keep both as diagnostics so a
    -- reachable-but-stopped inverter is no longer misreported as merely 0 W.
    -- Hybrid only: an SG string inverter answers neither, and asking fails
    -- the whole poll.
    local running_state = 0
    local power_flow_status = 0
    local running_state_known = false
    if hybrid_block_worth_reading() then
        local running_regs = optional_read(12999, 1, "input")
        if running_regs then
            running_state = running_regs[1]
            running_state_known = true
        end

        local flow_regs = optional_read(13000, 1, "input")
        if flow_regs then power_flow_status = flow_regs[1] end
    end
    local is_discharging = (math.floor(power_flow_status / 4) % 2) == 1

    -- PV power: 5016-5017, U32 LE, watts
    -- Observed on at least one SH-series firmware: this register stays
    -- at 0 even while MPPT1/MPPT2 V×I clearly indicate generation. So we
    -- also read the MPPT voltage+current pairs separately and use their
    -- product as a fallback when the top-level register doesn't match.
    local pv_regs = optional_read(5016, 2, "input")
    local pv_w_primary = 0
    local pv_raw_u32 = 0
    if pv_regs then
        pv_w_primary = host.decode_u32_le(pv_regs[1], pv_regs[2])
        pv_raw_u32   = pv_w_primary
    end

    -- PV MPPT: 5010-5013 (V×0.1, A×0.1 per string)
    local mppt_regs = optional_read(5010, 4, "input")
    local mppt1_v, mppt1_a, mppt2_v, mppt2_a = 0, 0, 0, 0
    if mppt_regs then
        mppt1_v = mppt_regs[1] * 0.1
        mppt1_a = mppt_regs[2] * 0.1
        mppt2_v = mppt_regs[3] * 0.1
        mppt2_a = mppt_regs[4] * 0.1
    end
    local mppt1_w = mppt1_v * mppt1_a
    local mppt2_w = mppt2_v * mppt2_a
    local pv_w_mppt = mppt1_w + mppt2_w

    -- Resolve PV power with a fallback ladder:
    --   1. Trust 5016-5017 if it reports > 50 W (clearly live).
    --   2. Otherwise use MPPT sum if that reports > 50 W (primary
    --      register stuck / firmware quirk).
    --   3. Zero — panels aren't generating right now.
    -- 50 W threshold filters out noise-floor readings without swallowing
    -- genuine low-light output.
    local pv_w = 0
    local pv_source = "zero"
    if pv_w_primary > 50 then
        pv_w = pv_w_primary
        pv_source = "primary_reg"
    elseif pv_w_mppt > 50 then
        pv_w = pv_w_mppt
        pv_source = "mppt_sum"
    end

    -- PV lifetime energy: 13002-13003, U32 LE × 0.1 kWh
    local pv_gen_wh = 0
    if hybrid_block_worth_reading() then
        local pvgen_regs = optional_read(13002, 2, "input")
        if pvgen_regs then
            pv_gen_wh = host.decode_u32_le(pvgen_regs[1], pvgen_regs[2]) * 0.1 * 1000
        end
    end

    -- Rated power: 5000, U16 × 0.1 kW
    local rated_regs = optional_read(5000, 1, "input")
    local rated_w = rated_ac_w
    if rated_regs then
        rated_w = rated_regs[1] * 0.1 * 1000
        if rated_w > 0 then rated_ac_w = rated_w end
    end

    -- Heatsink temp: 5007, I16 × 0.1 C
    local temp_regs = optional_read(5007, 1, "input")
    local heatsink_c = 0
    if temp_regs then
        heatsink_c = host.decode_i16(temp_regs[1]) * 0.1
    end

    -- Grid frequency: 5241, U16 × 0.01 Hz
    local hz_regs = optional_read(5241, 1, "input")
    local hz = 0
    if hz_regs then
        hz = hz_regs[1] * 0.01
    end

    host.emit("pv", {
        w           = -pv_w,  -- negative = generation (EMS convention)
        mppt1_v     = mppt1_v,
        mppt1_a     = mppt1_a,
        mppt2_v     = mppt2_v,
        mppt2_a     = mppt2_a,
        mppt1_w     = mppt1_w,
        mppt2_w     = mppt2_w,
        pv_source   = pv_source,  -- "primary_reg" | "mppt_sum" | "zero"
        lifetime_wh = pv_gen_wh,
        rated_w     = rated_w,
        temp_c      = heatsink_c,
    })
    -- Diagnostics: long-format TS DB. Surface BOTH the primary-register
    -- reading and the MPPT-derived fallback so operators can see when
    -- the two disagree (= firmware register quirk) directly in the
    -- metric browser.
    host.emit_metric("pv_w_primary",     pv_w_primary)
    host.emit_metric("pv_w_mppt_sum",    pv_w_mppt)
    host.emit_metric("pv_raw_u32",       pv_raw_u32)
    host.emit_metric("pv_mppt1_v",       mppt1_v)
    host.emit_metric("pv_mppt1_a",       mppt1_a)
    host.emit_metric("pv_mppt1_w",       mppt1_w)
    host.emit_metric("pv_mppt2_v",       mppt2_v)
    host.emit_metric("pv_mppt2_a",       mppt2_a)
    host.emit_metric("pv_mppt2_w",       mppt2_w)
    host.emit_metric("inverter_temp_c",  heatsink_c)
    host.emit_metric("grid_hz",          hz)
    host.emit_metric("sungrow_running_state", running_state)
    host.emit_metric("sungrow_power_flow_status", power_flow_status)
    host.emit_metric("sungrow_pv_curtail_method", pv_curtail_method == "feed_in" and 2 or 1)

    -- Contiguous bitfields from documented input registers 13050-13079
    -- (zero-based host addresses 13049-13078). One read gives enough evidence
    -- to distinguish thermal, grid, PV/DC, battery and BMS shutdowns.
    local fault_regs = nil
    if hybrid_block_worth_reading() then
        fault_regs = optional_read(13049, 30, "input")
    end
    local fault_values = {}
    if fault_regs and #fault_regs >= 30 then
        local fault_names = {
            "sungrow_inverter_alarm_bits",
            "sungrow_grid_fault_bits",
            "sungrow_system_fault1_bits",
            "sungrow_system_fault2_bits",
            "sungrow_dc_fault_bits",
            "sungrow_permanent_fault_bits",
            "sungrow_bdc_fault_bits",
            "sungrow_bdc_permanent_fault_bits",
            "sungrow_battery_fault_bits",
            "sungrow_battery_alarm_bits",
            "sungrow_bms_alarm_bits",
            "sungrow_bms_protection_bits",
            "sungrow_bms_fault1_bits",
            "sungrow_bms_fault2_bits",
            "sungrow_bms_alarm2_bits",
        }
        for i, name in ipairs(fault_names) do
            local j = (i - 1) * 2 + 1
            local value = host.decode_u32_le(fault_regs[j], fault_regs[j + 1])
            fault_values[i] = value
            host.emit_metric(name, value)
        end
    end

    -- A faulted inverter can keep returning perfectly fresh Modbus telemetry,
    -- which previously made /api/status and Diagnose say "ok" while PV and
    -- battery actuation were physically unavailable. Surface Sungrow's actual
    -- running state through the host's device-fault channel. RecordSuccess does
    -- not clear this flag; only a later non-fault running-state poll does.
    if running_state == 0x5500 or running_state == 0x0100 then
        local reason = string.format("Sungrow fault (running state 0x%04X)", running_state)
        local system_fault2 = fault_values[4] or 0
        if bit_is_set(system_fault2, 1) then
            reason = string.format(
                "Sungrow fault: excessively high ambient temperature (%.1f C, state 0x%04X)",
                heatsink_c, running_state)
        end
        host.set_device_fault(true, reason)
    elseif running_state_known then
        -- Clear only on a running state we actually read. A string inverter
        -- never reads it, so it neither raises nor clears a fault here.
        host.set_device_fault(false, "")
    end

    -- Battery: 13019-13022 (voltage, current, power, SoC). Hybrid only -- an
    -- SG string inverter has no battery and answers none of this.
    local bat_v, bat_a, bat_w, bat_soc = 0, 0, 0, 0
    local bat_regs = nil
    if hybrid_block_worth_reading() then
        bat_regs = optional_read(13019, 4, "input")
    end
    if bat_regs then
        battery_confirmed = true
        bat_v   = bat_regs[1] * 0.1
        bat_a   = bat_regs[2] * 0.1
        bat_w   = bat_regs[3]
        bat_soc = bat_regs[4] * 0.1 / 100  -- 0-1 fraction

        -- Apply sign: Sungrow reports power as unsigned, direction from the
        -- status register. EMS convention: positive = charging.
        if is_discharging then
            bat_w = -bat_w
        end
    end

    if bat_regs then
        local battery = {
            w   = bat_w,
            v   = bat_v,
            a   = bat_a,
            soc = bat_soc,
        }

        -- Energy counters are separate reads. Add each only if it answered:
        -- a counter reported as zero would look like a reset meter.
        local bchg_regs = optional_read(13040, 2, "input")
        if bchg_regs then
            battery.charge_wh =
                host.decode_u32_le(bchg_regs[1], bchg_regs[2]) * 0.1 * 1000
        end

        local bdis_regs = optional_read(13026, 2, "input")
        if bdis_regs then
            battery.discharge_wh =
                host.decode_u32_le(bdis_regs[1], bdis_regs[2]) * 0.1 * 1000
        end

        host.emit("battery", battery)
        host.emit_metric("battery_dc_v", bat_v)
        host.emit_metric("battery_dc_a", bat_a)
    end

    -- EMS state diagnostics — what the inverter *actually* has latched
    -- in its control registers right now. With the #164 write-order fix
    -- these should track whatever the dispatcher sent last tick; any
    -- drift between target and ems_force_w points at external writers
    -- (iSolarCloud, HA integration, another EMS) racing the driver.
    local emsd = nil
    if hybrid_block_worth_reading() then
        emsd = optional_read(13049, 3, "holding")
    end
    if emsd then
        host.emit_metric("sungrow_ems_mode",  emsd[1]) -- 0=self, 2=forced, 3=ext
        host.emit_metric("sungrow_force_cmd", emsd[2]) -- 0xAA=170 chg, 0xBB=187 dis, 0xCC=204 stop
        host.emit_metric("sungrow_force_w",   emsd[3])
    end

    -- Grid meter power: 5600-5601, I32 LE, watts (positive=import, negative=export)
    local mw_regs = optional_read(5600, 2, "input")
    local meter_w = 0
    if mw_regs then
        meter_w = host.decode_i32_le(mw_regs[1], mw_regs[2])
    end

    -- Per-phase power: 5602-5607, I32 LE pairs
    local mp_regs = optional_read(5602, 6, "input")
    local l1_w, l2_w, l3_w = 0, 0, 0
    if mp_regs then
        l1_w = host.decode_i32_le(mp_regs[1], mp_regs[2])
        l2_w = host.decode_i32_le(mp_regs[3], mp_regs[4])
        l3_w = host.decode_i32_le(mp_regs[5], mp_regs[6])
    end

    -- Per-phase voltage: 5740-5742, U16 × 0.1 V
    local mv_regs = optional_read(5740, 3, "input")
    local l1_v, l2_v, l3_v = 0, 0, 0
    if mv_regs then
        l1_v = mv_regs[1] * 0.1
        l2_v = mv_regs[2] * 0.1
        l3_v = mv_regs[3] * 0.1
    end

    -- Per-phase current: 5743-5745, U16 × 0.01 A
    local ma_regs = optional_read(5743, 3, "input")
    local l1_a, l2_a, l3_a = 0, 0, 0
    if ma_regs then
        l1_a = ma_regs[1] * 0.01
        l2_a = ma_regs[2] * 0.01
        l3_a = ma_regs[3] * 0.01
    end

    -- Import and export energy: 13036-13037 and 13045-13046, U32 LE × 0.1 kWh.
    -- In the hybrid block, so an SG string inverter reports neither. Left out
    -- rather than zeroed when absent: a counter reading zero looks like a
    -- meter that was reset.
    local import_wh, export_wh = nil, nil
    if hybrid_block_worth_reading() then
        local imp_regs = optional_read(13036, 2, "input")
        if imp_regs then
            import_wh = host.decode_u32_le(imp_regs[1], imp_regs[2]) * 0.1 * 1000
        end
        local exp_regs = optional_read(13045, 2, "input")
        if exp_regs then
            export_wh = host.decode_u32_le(exp_regs[1], exp_regs[2]) * 0.1 * 1000
        end
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

    -- Feed-in fallback translation: a PV allowance is the power that may
    -- serve local load, charge the battery, or leave the site. Sungrow's
    -- legacy SHxRT register controls only the last term, so subtract current
    -- non-export absorption. This preserves useful self-consumption while
    -- preventing paid/negative-price export. A manual cap below live local
    -- absorption cannot force DC PV lower on this firmware; it safely floors
    -- the export allowance at zero instead.
    local load_w = meter_w - bat_w + pv_w
    latest_non_export_w = math.max(0, load_w + math.max(0, bat_w))

    return 5000
end

----------------------------------------------------------------------------
-- Battery control
----------------------------------------------------------------------------

-- Battery control command handler
-- EMS convention: positive power_w = charge, negative = discharge
-- Verified: charge 200W and discharge 200W both tested and confirmed
--
-- Do not remove the no-battery guard below. It shipped in 1.2.1 (#17),
-- was lost when #27 replaced this file with FTW's driver, and #29 restored
-- only the read half at 1.4.0. Between those, a battery command on a model
-- this driver had already classified as "string" wrote forced mode, a force
-- command and a setpoint into the 13xxx block the family does not implement,
-- and then reported success: the read-back that would have caught it fails on
-- such a device, and a failed read-back is treated as transient and assumed
-- good. The host recorded an applied setpoint, renewed the lease on it, and
-- the planner went on dispatching a battery that is not there.
--
-- Refusing follows the shape curtail already uses when rated power is missing:
-- a warn line naming the reason, and a result the host can report. The code is
-- the one packages/v1/sungrow/targets/ftw.lua returns, so the two control
-- paths refuse in the same words rather than drifting apart again.
--
-- Two ways to know this device takes a battery command: it named itself a
-- hybrid, or its battery registers have answered. Everything else is a guess,
-- and the guess used to be "yes" -- so an SG inverter whose device-type
-- register never answered still got the EMS writes. Note what this is not:
-- refusing everything the device type did not positively confirm. A hybrid
-- whose register 4999 is unreadable reads its battery every poll and reports
-- its SoC; denying control to a battery the driver is actively reading would
-- be an outage of its own, and detection giving up is common enough that
-- DETECT_ATTEMPTS exists for it.
function driver_command(action, power_w, cmd)
    if action == "init" then
        return true
    elseif action == "battery" then
        -- Zero is not a dispatch. It is the host handing the device back to
        -- itself: forced mode off, setpoint nought. Refusing to write "stop"
        -- is a different risk from refusing to write "charge" -- a device
        -- left in a forced state stays there, and the safe default is the
        -- one path that must never be gated on how much we know about the
        -- hardware. It also arrives before the first poll, from the
        -- lifecycle rather than from the planner, which is exactly when
        -- nothing has been confirmed yet.
        --
        -- On a genuine string inverter this writes to a register block the
        -- model does not implement. That write fails at the Modbus layer and
        -- costs nothing; the outage it protects against is a battery left
        -- charging with no way to say stop.
        if power_w ~= 0 and model_family ~= "hybrid" and not battery_confirmed then
            local why = model_family == "string"
                and "this model has no battery registers"
                or "no battery register has answered on this device"
            host.log("warn", "Sungrow: battery command refused — " .. why)
            return false, {
                status = "rejected",
                code = "no_battery",
                message = why,
                device_state = "unchanged",
            }
        end
        return set_battery_power(power_w)
    elseif action == "curtail" then
        return set_pv_curtail_limit(math.abs(power_w))
    elseif action == "curtail_disable" then
        return set_pv_curtail_disabled()
    elseif action == "deinit" then
        local pv_ok = true
        if pv_curtail_control_enabled or pv_curtail_active then
            pv_ok = set_pv_curtail_disabled()
        end
        return set_self_consumption() and pv_ok
    end
    return false
end

-- Set battery to a specific charge/discharge power
-- power_w > 0: charge at power_w watts
-- power_w < 0: discharge at |power_w| watts
-- power_w = 0: forced idle at 0W. Default/watchdog mode is the only
-- path that returns the inverter to autonomous self-consumption.
-- Write order matters. SH-series ignores the force_cmd (13050) and
-- force_power (13051) registers while EMS mode (13049) is still 0
-- (self-consumption). Writing power + cmd FIRST and mode LAST means
-- the cmd/power writes land in self-consumption mode and get buffered-
-- and-discarded or implicitly reset by the mode transition; the
-- inverter ends up in mode=2 with no forced command and the battery
-- sits idle despite a non-zero target. Reference implementations
-- (mkaiser/Sungrow-SHx Home Assistant, openWB) write mode → cmd →
-- power for exactly this reason. Issue #164.
function set_battery_power(power_w)
    local want_cmd, watts
    if power_w > 0 then
        watts    = math.floor(math.min(power_w, 5000))
        want_cmd = 0xAA -- force charge
    elseif power_w < 0 then
        watts    = math.floor(math.min(math.abs(power_w), 5000))
        want_cmd = 0xBB -- force discharge
    else
        return set_battery_idle()
    end

    -- Order: mode first (so the inverter is ready to latch cmd/power),
    -- then cmd (which register is honoured), then power setpoint.
    local mode_err = host.modbus_write(13049, 2)         -- 1. forced mode
    local cmd_err = host.modbus_write(13050, want_cmd)   -- 2. charge/discharge cmd
    local power_err = host.modbus_write(13051, watts)    -- 3. power setpoint
    local write_err = first_write_error(mode_err, cmd_err, power_err)
    if write_err then
        host.log("warn", "Sungrow: force command write failed: " .. write_err)
        return false
    end
    host.log("debug", string.format("Sungrow: force %s %dW",
        want_cmd == 0xAA and "charge" or "discharge", watts))

    -- Verify all three latched. Previous version only checked mode and
    -- logged success on mode=2 even when cmd was still 0 → the exact
    -- class of silent failure #164 describes. Emit mismatches as WARN
    -- so operators see the drift in logs without spamming on ok cases.
    local ok, ems = pcall(host.modbus_read, 13049, 3, "holding")
    if not ok or not ems then
        return true -- transient read failure; assume writes are good
    end
    if ems[1] ~= 2 then
        host.log("warn", "Sungrow: EMS mode not latched (got " .. tostring(ems[1]) .. " want 2)")
        return false
    end
    if ems[2] ~= want_cmd then
        host.log("warn", string.format("Sungrow: force_cmd not latched (got 0x%02x want 0x%02x)",
            ems[2], want_cmd))
        return false
    end
    if math.abs(ems[3] - watts) > 10 then -- observed firmware quantizes to 10 W
        host.log("warn", string.format("Sungrow: force_power not latched (got %dW want %dW)",
            ems[3], watts))
        return false
    end
    return true
end

-- Hold the battery at 0W without handing control back to the inverter's
-- autonomous self-consumption mode. Otherwise a 0W planner target can still
-- let Sungrow charge from PV surplus on its own.
function set_battery_idle()
    local mode_err = host.modbus_write(13049, 2)     -- forced mode
    local cmd_err = host.modbus_write(13050, 0xCC)   -- stop forced charge/discharge
    local power_err = host.modbus_write(13051, 0)    -- zero power setpoint
    local write_err = first_write_error(mode_err, cmd_err, power_err)
    if write_err then
        host.log("warn", "Sungrow: idle write failed: " .. write_err)
        return false
    end
    host.log("debug", "Sungrow: force idle 0W")

    local ok, ems = pcall(host.modbus_read, 13049, 3, "holding")
    if not ok or not ems then
        return true -- transient read failure; assume writes are good
    end
    if ems[1] ~= 2 then
        host.log("warn", "Sungrow: EMS idle mode not latched (got " .. tostring(ems[1]) .. " want 2)")
        return false
    end
    if math.abs(ems[3]) > 1 then -- 1W rounding tolerance
        host.log("warn", string.format("Sungrow: idle force_power not latched (got %dW want 0W)", ems[3]))
        return false
    end
    return true
end

-- Return to self-consumption mode (safe default)
function set_self_consumption()
    -- Stop first, clear the stale power setpoint, then hand control back to
    -- the inverter. This makes the post-restart state deterministic instead
    -- of leaving e.g. mode=0/cmd=CC/power=880W latched in diagnostics.
    local cmd_err = host.modbus_write(13050, 0xCC)
    local power_err = host.modbus_write(13051, 0)
    local mode_err = host.modbus_write(13049, 0)
    local write_err = first_write_error(cmd_err, power_err, mode_err)
    if write_err then
        host.log("warn", "Sungrow: self-consumption reset write failed: " .. write_err)
        return false
    end
    host.log("debug", "Sungrow: self-consumption mode")

    local ok, ems = pcall(host.modbus_read, 13049, 3, "holding")
    if not ok or not ems then return true end
    if ems[1] ~= 0 or ems[2] ~= 0xCC or ems[3] ~= 0 then
        host.log("warn", string.format(
            "Sungrow: self-consumption reset not latched (mode=%d cmd=0x%02x power=%dW)",
            ems[1] or -1, ems[2] or -1, ems[3] or -1))
        return false
    end
    return true
end

-- Cap inverter AC output using Sungrow's Active Power Limitation pair
-- (documented registers 13089/13090, zero-based addresses 13088/13089).
-- This is the preferred control point for FTW's PV power cap. SHxRT firmware
-- that does not expose this pair can explicitly select the feed-in method;
-- that path preserves self-consumption and restores a configured installer
-- ceiling verbatim instead of taking ownership of its enable flag.
function set_pv_curtail_limit(watts)
    if pv_curtail_method == "feed_in" then
        return set_feed_in_curtail_limit(watts)
    end

    if rated_ac_w <= 0 then
        local ok, rated = pcall(host.modbus_read, 5000, 1, "input")
        if ok and rated and rated[1] and rated[1] > 0 then
            rated_ac_w = rated[1] * 0.1 * 1000
        end
    end
    if rated_ac_w <= 0 then
        host.log("warn", "Sungrow: PV curtail refused — rated AC power unavailable")
        return false
    end

    local clamped_w = math.max(0, math.min(watts, rated_ac_w))
    local ratio_x10 = math.floor((clamped_w / rated_ac_w) * 1000 + 0.5)
    -- Use separate FC 0x06 writes because some Sungrow gateways reject a
    -- combined FC 0x10 write here. Set the inert ratio first and enable last:
    -- if either step fails, a stale low ratio cannot be enabled.
    local ratio_err = host.modbus_write(13089, ratio_x10)
    local enable_err = nil
    if ratio_err == nil or ratio_err == "" then
        enable_err = host.modbus_write(13088, 0xAA)
    end
    local write_err = first_write_error(ratio_err, enable_err, nil)
    if write_err then
        host.log("warn", "Sungrow: PV curtail write failed: " .. write_err)
        return false
    end

    local ok, regs = pcall(host.modbus_read, 13088, 2, "holding")
    if ok and regs and (regs[1] ~= 0xAA or regs[2] ~= ratio_x10) then
        host.log("warn", string.format(
            "Sungrow: PV curtail not latched (enable=0x%02x ratio=%d want=%d)",
            regs[1] or -1, regs[2] or -1, ratio_x10))
        return false
    end

    pv_curtail_active = true
    host.emit_metric("sungrow_pv_limit_w", clamped_w)
    host.emit_metric("sungrow_pv_limit_ratio_x10", ratio_x10)
    host.log("debug", string.format("Sungrow: PV limit %.0fW (%.1f%%)",
        clamped_w, ratio_x10 * 0.1))
    return true
end

function set_pv_curtail_disabled()
    if pv_curtail_method == "feed_in" then
        if feed_in_release_w <= 0 then
            host.log("warn", "Sungrow: feed-in curtail release refused — config.feed_in_release_w is required")
            return false
        end
        local err = host.modbus_write(13073, math.floor(feed_in_release_w))
        if err ~= nil and err ~= "" then
            host.log("warn", "Sungrow: feed-in curtail release failed: " .. tostring(err))
            return false
        end
        local ok, regs = pcall(host.modbus_read, 13073, 1, "holding")
        if ok and regs and regs[1] ~= math.floor(feed_in_release_w) then
            host.log("warn", string.format(
                "Sungrow: feed-in release not latched (got %dW want %.0fW)",
                regs[1] or -1, feed_in_release_w))
            return false
        end
        pv_curtail_active = false
        host.emit_metric("sungrow_feed_in_limit_w", feed_in_release_w)
        host.emit_metric("sungrow_pv_limit_w", rated_ac_w)
        host.log("debug", string.format(
            "Sungrow: feed-in limit restored to %.0fW", feed_in_release_w))
        return true
    end

    -- Disable first, then reset the inert ratio to 100%. This ordering is
    -- safe on the SHxRT firmwares that require separate FC 0x06 writes: an
    -- interrupted release cannot leave the low limit enabled.
    local disable_err = host.modbus_write(13088, 0x55)
    local ratio_err = nil
    if disable_err == nil or disable_err == "" then
        ratio_err = host.modbus_write(13089, 1000)
    end
    local write_err = first_write_error(disable_err, ratio_err, nil)
    if write_err then
        host.log("warn", "Sungrow: PV curtail release failed: " .. write_err)
        return false
    end

    local ok, regs = pcall(host.modbus_read, 13088, 2, "holding")
    if ok and regs and (regs[1] ~= 0x55 or regs[2] ~= 1000) then
        host.log("warn", string.format(
            "Sungrow: PV curtail release not latched (enable=0x%02x ratio=%d)",
            regs[1] or -1, regs[2] or -1))
        return false
    end

    pv_curtail_active = false
    host.emit_metric("sungrow_pv_limit_w", rated_ac_w)
    host.emit_metric("sungrow_pv_limit_ratio_x10", 1000)
    host.log("debug", "Sungrow: PV limit disabled")
    return true
end

function set_feed_in_curtail_limit(watts)
    if feed_in_release_w <= 0 then
        host.log("warn", "Sungrow: feed-in curtail refused — config.feed_in_release_w is required")
        return false
    end

    -- Never take ownership of an installer-disabled export control. This
    -- fallback changes only the already-enabled absolute limit value and
    -- restores the configured baseline verbatim on release.
    local ok_enabled, enabled = pcall(host.modbus_read, 13086, 1, "holding")
    if not ok_enabled or not enabled or enabled[1] ~= 0xAA then
        host.log("warn", "Sungrow: feed-in curtail refused — Feed-in Limitation is not enabled")
        return false
    end

    local export_limit_w = math.max(0, watts - latest_non_export_w)
    export_limit_w = math.min(export_limit_w, feed_in_release_w, 65535)
    export_limit_w = math.floor(export_limit_w + 0.5)
    local err = host.modbus_write(13073, export_limit_w)
    if err ~= nil and err ~= "" then
        host.log("warn", "Sungrow: feed-in curtail write failed: " .. tostring(err))
        return false
    end

    local ok, regs = pcall(host.modbus_read, 13073, 1, "holding")
    if ok and regs and math.abs(regs[1] - export_limit_w) > 10 then
        host.log("warn", string.format(
            "Sungrow: feed-in curtail not latched (got %dW want %dW)",
            regs[1] or -1, export_limit_w))
        return false
    end
    local latched_export_w = export_limit_w
    if ok and regs and regs[1] ~= nil then latched_export_w = regs[1] end

    pv_curtail_active = true
    host.emit_metric("sungrow_feed_in_limit_w", latched_export_w)
    host.emit_metric("sungrow_pv_limit_w", watts)
    host.log("debug", string.format(
        "Sungrow: PV allowance %.0fW → feed-in limit %dW (local absorption %.0fW)",
        watts, latched_export_w, latest_non_export_w))
    return true
end

-- Watchdog fallback: always revert to self-consumption
function driver_default_mode()
    host.log("info", "Sungrow: watchdog → reverting to self-consumption")
    return set_self_consumption()
end

function driver_cleanup()
    if pv_curtail_control_enabled or pv_curtail_active then
        set_pv_curtail_disabled()
    end
    set_self_consumption()
end
