-- Sungrow SH Hybrid Inverter Driver
-- Emits: PV, Battery, Meter
-- Register type: INPUT
-- Byte order: Little-Endian for multi-register values

PROTOCOL = "modbus"

local sn_read = false

function driver_init(config)
    host.set_make("Sungrow")
end

function driver_poll()
    -- Read serial number once on first successful poll (Modbus connected by now)
    if not sn_read then
        local ok, sn_regs = pcall(host.modbus_read, 4990, 10, "input")
        if ok and sn_regs then
            local sn = ""
            for i = 1, 10 do
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

    -- Read status flags to determine battery direction
    local ok_status, status_regs = pcall(host.modbus_read, 13000, 1, "input")
    local status = 0
    if ok_status and status_regs then
        status = status_regs[1]
    end
    -- Lua 5.1 compatible bit check: bit 2 (0x0004) set means discharging
    local is_discharging = (math.floor(status / 4) % 2) == 1

    -- PV power: 5016-5017, U32 LE, watts
    local ok_pv, pv_regs = pcall(host.modbus_read, 5016, 2, "input")
    local pv_w = 0
    if ok_pv and pv_regs then
        pv_w = host.decode_u32_le(pv_regs[1], pv_regs[2])
    end

    -- PV MPPT voltages and currents: 5010-5013
    local ok_mppt, mppt_regs = pcall(host.modbus_read, 5010, 4, "input")
    local mppt1_v, mppt1_a, mppt2_v, mppt2_a = 0, 0, 0, 0
    if ok_mppt and mppt_regs then
        mppt1_v = mppt_regs[1] * 0.1
        mppt1_a = mppt_regs[2] * 0.1
        mppt2_v = mppt_regs[3] * 0.1
        mppt2_a = mppt_regs[4] * 0.1
    end

    -- PV generation energy: 13002-13003, U32 LE × 0.1 kWh
    local ok_pvgen, pvgen_regs = pcall(host.modbus_read, 13002, 2, "input")
    local pv_gen_wh = 0
    if ok_pvgen and pvgen_regs then
        pv_gen_wh = host.decode_u32_le(pvgen_regs[1], pvgen_regs[2]) * 0.1 * 1000
    end

    -- Rated power: 5000, U16 × 0.1 kW
    local ok_rated, rated_regs = pcall(host.modbus_read, 5000, 1, "input")
    local rated_w = 0
    if ok_rated and rated_regs then
        rated_w = rated_regs[1] * 0.1 * 1000
    end

    -- Heatsink temp: 5007, I16 × 0.1 C
    local ok_temp, temp_regs = pcall(host.modbus_read, 5007, 1, "input")
    local heatsink_c = 0
    if ok_temp and temp_regs then
        heatsink_c = host.decode_i16(temp_regs[1]) * 0.1
    end

    -- Frequency: 5241, U16 × 0.01 Hz
    local ok_hz, hz_regs = pcall(host.modbus_read, 5241, 1, "input")
    local hz = 0
    if ok_hz and hz_regs then
        hz = hz_regs[1] * 0.01
    end

    -- Emit PV telemetry (W always negative for generation)
    host.emit("pv", {
        w           = -pv_w,
        mppt1_v     = mppt1_v,
        mppt1_a     = mppt1_a,
        mppt2_v     = mppt2_v,
        mppt2_a     = mppt2_a,
        lifetime_wh = pv_gen_wh,
        rated_w     = rated_w,
        temp_c      = heatsink_c,
    })

    -- Battery registers: 13019-13022
    local ok_bat, bat_regs = pcall(host.modbus_read, 13019, 4, "input")
    local bat_v, bat_a, bat_w, bat_soc = 0, 0, 0, 0
    if ok_bat and bat_regs then
        bat_v   = bat_regs[1] * 0.1
        bat_a   = bat_regs[2] * 0.1
        bat_w   = bat_regs[3]
        bat_soc = bat_regs[4] * 0.1 / 100  -- percent to fraction
    end

    -- Negate battery W if discharging
    if is_discharging then
        bat_w = -bat_w
    end

    -- Battery charge energy: 13040-13041, U32 LE × 0.1 kWh
    local ok_bchg, bchg_regs = pcall(host.modbus_read, 13040, 2, "input")
    local bat_charge_wh = 0
    if ok_bchg and bchg_regs then
        bat_charge_wh = host.decode_u32_le(bchg_regs[1], bchg_regs[2]) * 0.1 * 1000
    end

    -- Battery discharge energy: 13026-13027, U32 LE × 0.1 kWh
    local ok_bdis, bdis_regs = pcall(host.modbus_read, 13026, 2, "input")
    local bat_discharge_wh = 0
    if ok_bdis and bdis_regs then
        bat_discharge_wh = host.decode_u32_le(bdis_regs[1], bdis_regs[2]) * 0.1 * 1000
    end

    -- Emit Battery telemetry
    host.emit("battery", {
        w            = bat_w,
        v            = bat_v,
        a            = bat_a,
        soc          = bat_soc,
        charge_wh    = bat_charge_wh,
        discharge_wh = bat_discharge_wh,
    })

    -- Meter power: 5600-5601, I32 LE, watts
    local ok_mw, mw_regs = pcall(host.modbus_read, 5600, 2, "input")
    local meter_w = 0
    if ok_mw and mw_regs then
        meter_w = host.decode_i32_le(mw_regs[1], mw_regs[2])
    end

    -- Per-phase meter power: 5602-5607, I32 LE each pair
    local ok_mp, mp_regs = pcall(host.modbus_read, 5602, 6, "input")
    local l1_w, l2_w, l3_w = 0, 0, 0
    if ok_mp and mp_regs then
        l1_w = host.decode_i32_le(mp_regs[1], mp_regs[2])
        l2_w = host.decode_i32_le(mp_regs[3], mp_regs[4])
        l3_w = host.decode_i32_le(mp_regs[5], mp_regs[6])
    end

    -- Per-phase voltage: 5740-5742, U16 × 0.1 each
    local ok_mv, mv_regs = pcall(host.modbus_read, 5740, 3, "input")
    local l1_v, l2_v, l3_v = 0, 0, 0
    if ok_mv and mv_regs then
        l1_v = mv_regs[1] * 0.1
        l2_v = mv_regs[2] * 0.1
        l3_v = mv_regs[3] * 0.1
    end

    -- Per-phase current: 5743-5745, U16 × 0.01 each
    local ok_ma, ma_regs = pcall(host.modbus_read, 5743, 3, "input")
    local l1_a, l2_a, l3_a = 0, 0, 0
    if ok_ma and ma_regs then
        l1_a = ma_regs[1] * 0.01
        l2_a = ma_regs[2] * 0.01
        l3_a = ma_regs[3] * 0.01
    end

    -- Import energy: 13036-13037, U32 LE × 0.1 kWh
    local ok_imp, imp_regs = pcall(host.modbus_read, 13036, 2, "input")
    local import_wh = 0
    if ok_imp and imp_regs then
        import_wh = host.decode_u32_le(imp_regs[1], imp_regs[2]) * 0.1 * 1000
    end

    -- Export energy: 13045-13046, U32 LE × 0.1 kWh
    local ok_exp, exp_regs = pcall(host.modbus_read, 13045, 2, "input")
    local export_wh = 0
    if ok_exp and exp_regs then
        export_wh = host.decode_u32_le(exp_regs[1], exp_regs[2]) * 0.1 * 1000
    end

    -- Emit Meter telemetry
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

    return 5000
end

-- Control command handler for Sungrow battery
-- Reference: https://github.com/mkaiser/Sungrow-SHx-Inverter-Modbus-Home-Assistant
-- Holding registers (address = register - 1):
--   13049 (reg 13050): EMS mode: 0=self-consumption, 2=forced/compulsory, 3=external EMS
--   13050 (reg 13051): Force charge/discharge cmd: 0xAA=charge, 0xBB=discharge, 0xCC=stop
--   13051 (reg 13052): Force charge/discharge power: 0-5000 W
-- EMS convention: positive power_w = charge, negative = discharge
function driver_command(action, power_w, cmd)
    if action == "init" then
        return true
    elseif action == "battery" then
        if power_w > 0 then
            -- Force charge
            host.modbus_write(13051, math.floor(math.min(power_w, 5000)))  -- power limit
            host.modbus_write(13050, 0xAA)                                 -- force charge cmd
            host.modbus_write(13049, 2)                                    -- forced mode
        elseif power_w < 0 then
            -- Force discharge
            host.modbus_write(13051, math.floor(math.min(math.abs(power_w), 5000)))  -- power limit
            host.modbus_write(13050, 0xBB)                                             -- force discharge cmd
            host.modbus_write(13049, 2)                                                -- forced mode
        else
            -- Stop forced mode, return to self-consumption
            host.modbus_write(13050, 0xCC)  -- stop
            host.modbus_write(13049, 0)     -- self-consumption mode
        end
        return true
    elseif action == "curtail" then
        -- Limit export power
        host.modbus_write(13073, math.floor(math.abs(power_w)))
        return true
    elseif action == "curtail_disable" or action == "deinit" then
        host.modbus_write(13050, 0xCC)  -- stop forced
        host.modbus_write(13049, 0)     -- self-consumption
        return true
    end
    return false
end

function driver_default_mode()
    host.modbus_write(13050, 0xCC)  -- stop forced charge/discharge
    host.modbus_write(13049, 0)     -- self-consumption mode
end

function driver_cleanup()
end
