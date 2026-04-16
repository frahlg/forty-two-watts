-- ferroamp.lua
-- Ferroamp EnergyHub MQTT driver
-- Emits: PV, Battery, Meter telemetry

DRIVER = {
  id           = "ferroamp",
  name         = "Ferroamp EnergyHub",
  manufacturer = "Ferroamp",
  version      = "1.0.0",
  protocols    = { "mqtt" },
  capabilities = { "meter", "pv", "battery" },
  description  = "Ferroamp EnergyHub with ESO battery + SSO solar strings (3-phase).",
  homepage     = "https://ferroamp.com",
  authors      = { "forty-two-watts contributors" },
  connection_defaults = {
    port     = 1883,
    username = "extapi",
    password = "ferroampExtApi",
  },
}
--
-- Subscribes to:
--   extapi/data/ehub  - main hub data (grid, frequency, energy counters, PV summary)
--   extapi/data/eso   - battery storage object (SoC, battery power, voltage, current)
--   extapi/data/sso   - solar string object (per-string PV power)
--
-- Ferroamp payload format: {"key": {"val": value}} or {"key": {"L1": v1, "L2": v2, "L3": v3}}
-- Energy counters are in mJ (millijoules); convert to Wh: mJ / 3,600,000
--
-- Sign convention:
--   PV w:      always negative (generation)
--   Battery w: positive = charging, negative = discharging
--   Meter w:   positive = import, negative = export

PROTOCOL = "mqtt"

-- Cached state from each topic
local ehub_data = nil
local eso_data = nil
local sso_data = nil

----------------------------------------------------------------------------
-- Helpers
----------------------------------------------------------------------------

-- Extract a value from Ferroamp's {"key": {"val": v}} structure.
-- Returns the raw val (string/number), or the field table if no "val" key.
local function extract_val(obj, key)
    if not obj then return nil end
    local field = obj[key]
    if not field then return nil end
    if type(field) == "table" and field.val ~= nil then
        return field.val
    end
    return field
end

-- Sum L1+L2+L3 from a phase table {"L1":..,"L2":..,"L3":..}, or return scalar.
-- Also handles numeric arrays for backwards compatibility.
local function sum_phases(val)
    if val == nil then return 0 end
    if type(val) == "number" then return val end
    if type(val) == "string" then return tonumber(val) or 0 end
    if type(val) == "table" then
        -- Try named keys first (current Ferroamp format)
        if val.L1 or val.L2 or val.L3 then
            return (tonumber(val.L1) or 0) + (tonumber(val.L2) or 0) + (tonumber(val.L3) or 0)
        end
        -- Fall back to numeric array
        local s = 0
        for _, v in ipairs(val) do
            s = s + (tonumber(v) or 0)
        end
        return s
    end
    return 0
end

-- Get a specific phase value from {"L1":..,"L2":..,"L3":..} or array [1,2,3].
local function phase_val(val, phase)
    if val == nil then return 0 end
    if type(val) ~= "table" then return 0 end
    -- Named key (e.g. "L1")
    if val[phase] then return tonumber(val[phase]) or 0 end
    -- Numeric index fallback (L1=1, L2=2, L3=3)
    local idx = ({L1=1, L2=2, L3=3})[phase]
    if idx and val[idx] then return tonumber(val[idx]) or 0 end
    return 0
end

-- Convert Ferroamp mJ counter to Wh
local function mj_to_wh(mj_val)
    local mj = tonumber(mj_val) or 0
    return mj / 3600000
end

----------------------------------------------------------------------------
-- Driver interface
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("Ferroamp")

    -- Subscribe to telemetry topics
    host.mqtt_subscribe("extapi/data/ehub")
    host.mqtt_subscribe("extapi/data/eso")
    host.mqtt_subscribe("extapi/data/sso")

    -- Subscribe to control response topic to verify commands are received
    host.mqtt_subscribe("extapi/result")

    -- Query API version to verify connectivity and external API access
    local version_cmd = '{"transId":"init","cmd":{"name":"extapiversion"}}'
    host.mqtt_publish("extapi/control/request", version_cmd)
    host.log("info", "Ferroamp: sent extapiversion query")

    -- Ensure we start in auto mode (clean state)
    host.mqtt_publish("extapi/control/request", '{"transId":"init","cmd":{"name":"auto"}}')
    host.log("info", "Ferroamp: set auto mode on init")
end

function driver_poll()
    local messages = host.mqtt_messages()
    if not messages then return 1000 end

    -- Process incoming messages and cache data
    for _, msg in ipairs(messages) do
        local ok, data = pcall(host.json_decode, msg.payload)
        if ok and data then
            if msg.topic == "extapi/data/ehub" then
                ehub_data = data
            elseif msg.topic == "extapi/data/eso" then
                eso_data = data
            elseif msg.topic == "extapi/data/sso" then
                sso_data = data
            end
        end
    end

    --------------------------------------------------------------------------
    -- Meter (grid connection point)
    --------------------------------------------------------------------------
    if ehub_data then
        local pext     = extract_val(ehub_data, "pext")     -- per-phase grid power (W)
        local gridfreq = extract_val(ehub_data, "gridfreq") -- grid frequency (Hz)
        local ul       = extract_val(ehub_data, "ul")       -- per-phase voltage (V)
        local il       = extract_val(ehub_data, "il")       -- per-phase current (A)
        -- 3-phase energy totals in mJ
        local wextconsq3p = extract_val(ehub_data, "wextconsq3p") -- total import mJ
        local wextprodq3p = extract_val(ehub_data, "wextprodq3p") -- total export mJ

        local meter = {}

        -- Grid power: negative = exporting, positive = importing
        meter.w    = sum_phases(pext)
        meter.l1_w = phase_val(pext, "L1")
        meter.l2_w = phase_val(pext, "L2")
        meter.l3_w = phase_val(pext, "L3")

        -- Grid frequency
        if gridfreq then
            meter.hz = tonumber(gridfreq) or 0
        end

        -- Per-phase voltage
        meter.l1_v = phase_val(ul, "L1")
        meter.l2_v = phase_val(ul, "L2")
        meter.l3_v = phase_val(ul, "L3")

        -- Per-phase current
        meter.l1_a = phase_val(il, "L1")
        meter.l2_a = phase_val(il, "L2")
        meter.l3_a = phase_val(il, "L3")

        -- Energy counters (mJ → Wh)
        if wextconsq3p then
            meter.import_wh = mj_to_wh(wextconsq3p)
        end
        if wextprodq3p then
            meter.export_wh = mj_to_wh(wextprodq3p)
        end

        host.emit("meter", meter)
        -- Diagnostics: long-format TS DB
        if meter.l1_w then host.emit_metric("meter_l1_w", meter.l1_w) end
        if meter.l2_w then host.emit_metric("meter_l2_w", meter.l2_w) end
        if meter.l3_w then host.emit_metric("meter_l3_w", meter.l3_w) end
        if meter.l1_v then host.emit_metric("meter_l1_v", meter.l1_v) end
        if meter.l2_v then host.emit_metric("meter_l2_v", meter.l2_v) end
        if meter.l3_v then host.emit_metric("meter_l3_v", meter.l3_v) end
        if meter.l1_a then host.emit_metric("meter_l1_a", meter.l1_a) end
        if meter.l2_a then host.emit_metric("meter_l2_a", meter.l2_a) end
        if meter.l3_a then host.emit_metric("meter_l3_a", meter.l3_a) end
        if meter.hz   then host.emit_metric("grid_hz",    meter.hz)   end
    end

    --------------------------------------------------------------------------
    -- PV (solar generation)
    --------------------------------------------------------------------------
    if ehub_data then
        local ppv = extract_val(ehub_data, "ppv")
        if ppv then
            -- Negate: Ferroamp reports PV as positive, convention requires negative
            host.emit("pv", { w = -(tonumber(ppv) or 0) })
        end
    end

    --------------------------------------------------------------------------
    -- Battery
    --------------------------------------------------------------------------
    if ehub_data then
        local pbat = extract_val(ehub_data, "pbat")
        if pbat then
            local battery = {}
            -- Ferroamp: positive pbat = discharging, negate for convention
            -- Convention: positive = charging, negative = discharging
            battery.w = -(tonumber(pbat) or 0)

            -- Enrich with ESO data (battery-specific telemetry)
            if eso_data then
                local soc = extract_val(eso_data, "soc")
                if soc then
                    local soc_val = tonumber(soc) or 0
                    -- Ferroamp reports SoC as 0-100%, convert to 0.0-1.0 fraction
                    if soc_val > 1 then soc_val = soc_val / 100 end
                    battery.soc = soc_val
                end

                local ubat = extract_val(eso_data, "ubat")
                if ubat then battery.v = tonumber(ubat) or 0 end

                local ibat = extract_val(eso_data, "ibat")
                if ibat then battery.a = tonumber(ibat) or 0 end

                -- Battery energy counters (mJ → Wh)
                local wbatprod = extract_val(eso_data, "wbatprod")
                local wbatcons = extract_val(eso_data, "wbatcons")
                if wbatprod then battery.discharge_wh = mj_to_wh(wbatprod) end
                if wbatcons then battery.charge_wh    = mj_to_wh(wbatcons) end
            end

            host.emit("battery", battery)
            if battery.v then host.emit_metric("battery_dc_v", battery.v) end
            if battery.a then host.emit_metric("battery_dc_a", battery.a) end
        end
    end

    return 1000
end

----------------------------------------------------------------------------
-- Control
----------------------------------------------------------------------------

-- Control: Ferroamp External API
-- Reference: https://github.com/henricm/ha-ferroamp
-- Topic: extapi/control/request
-- Commands:
--   {"transId":"...","cmd":{"name":"charge","arg":<watts>}}    — force charge (arg always positive)
--   {"transId":"...","cmd":{"name":"discharge","arg":<watts>}} — force discharge (arg always positive)
--   {"transId":"...","cmd":{"name":"auto"}}                    — return to auto mode
-- EMS convention: positive power_w = charge, negative = discharge
function driver_command(action, power_w, cmd)
    if action == "init" then
        return true
    elseif action == "battery" then
        local tid = "ems-" .. tostring(host.millis())
        if power_w > 0 then
            -- Charge: use "charge" command with positive watts
            local payload = string.format(
                '{"transId":"%s","cmd":{"name":"charge","arg":%d}}',
                tid, math.floor(power_w)
            )
            return host.mqtt_publish("extapi/control/request", payload)
        elseif power_w < 0 then
            -- Discharge: use "discharge" command with positive watts
            local payload = string.format(
                '{"transId":"%s","cmd":{"name":"discharge","arg":%d}}',
                tid, math.floor(math.abs(power_w))
            )
            return host.mqtt_publish("extapi/control/request", payload)
        else
            -- Zero: return to auto mode
            return host.mqtt_publish("extapi/control/request",
                string.format('{"transId":"%s","cmd":{"name":"auto"}}', tid))
        end
    elseif action == "curtail" then
        local payload = string.format(
            '{"transId":"ems","cmd":{"name":"pplim","arg":%d}}',
            math.floor(math.abs(power_w))
        )
        return host.mqtt_publish("extapi/control/request", payload)
    elseif action == "curtail_disable" then
        return host.mqtt_publish("extapi/control/request",
            '{"transId":"ems","cmd":{"name":"pplim","arg":0}}')
    elseif action == "deinit" then
        return host.mqtt_publish("extapi/control/request",
            '{"transId":"ems","cmd":{"name":"auto"}}')
    end
    return false
end

function driver_default_mode()
    host.mqtt_publish("extapi/control/request",
        '{"transId":"watchdog","cmd":{"name":"auto"}}')
end

function driver_cleanup()
    ehub_data = nil
    eso_data = nil
    sso_data = nil
end
