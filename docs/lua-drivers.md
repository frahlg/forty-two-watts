# Lua Driver Authoring Guide

## Overview

A Lua driver is a self-contained script that communicates with a single physical device (inverter, battery, meter) and translates its proprietary protocol into the standardized telemetry format that forty-two-watts understands. Drivers are the same scripts that run on the Sourceful Zap gateway -- they use the same `host.*` API and follow the same contract, so a driver written for forty-two-watts works on the Zap and vice versa.

Each driver runs in its own OS thread inside an isolated Lua 5.4 sandbox. The runtime automatically creates protocol connections (Modbus TCP, MQTT) based on the YAML config, so the driver code never deals with sockets or connection management directly.

```
+-----------+     +-----------+     +------------------+
|  Physical |     |   Lua     |     |   forty-two-watts       |
|  Device   |<--->|  Driver   |---->|  TelemetryStore  |
| (inverter)|     | (sandbox) |     |  (shared state)  |
+-----------+     +-----------+     +------------------+
                       |                     |
                  host.emit()           control loop
                  host.modbus_*         computes targets
                  host.mqtt_*           calls driver_command()
```

## Driver Contract

Every driver must declare one global variable and implement a set of lifecycle functions.

### Required Global

#### `PROTOCOL` (string)

Tells the runtime which transport client to create before `driver_init()` is called.

| Value          | Transport Created                                |
|----------------|--------------------------------------------------|
| `"modbus"`     | Modbus TCP connection (host, port, unit_id)      |
| `"mqtt"`       | MQTT 3.1.1 client (host, port, credentials)      |
| `"http"`       | HTTP client for local REST/RPC APIs              |
| `"serial"` / `"p1"` | Serial port (P1/HAN smart meter telegrams) |
| `"standalone"` | No client (test/demo drivers)                    |

### Required Functions

#### `driver_init(config)`

Called once after the Lua VM is created and the protocol client is connected.

**Parameters:**
- `config` -- JSON string with connection details. Parse it with `host.json_decode(config)`. Contains:

| Key                  | Type    | Description                              |
|----------------------|---------|------------------------------------------|
| `name`               | string  | Driver name from YAML config             |
| `protocol`           | string  | `"modbus"` or `"mqtt"`                   |
| `is_site_meter`      | boolean | Whether this driver provides the site meter |
| `battery_capacity_wh`| number  | Configured battery capacity in Wh        |
| `ip` / `port`        | string/number | Modbus host and port (if modbus)   |
| `unit_id`            | number  | Modbus slave ID (if modbus)              |
| `mqtt_host` / `mqtt_port` | string/number | MQTT broker (if mqtt)         |

**Typical usage:**

```lua
function driver_init(config)
    host.set_make("MyBrand")
    -- MQTT drivers subscribe to topics here:
    host.mqtt_subscribe("device/data/#")
end
```

#### `driver_poll()`

Called repeatedly by the runtime. This is where you read data from the device and emit telemetry.

**Must:**
1. Read data from the device (via `host.modbus_read`, `host.mqtt_messages`, etc.)
2. Emit telemetry via `host.emit("meter", data)`, `host.emit("pv", data)`, `host.emit("battery", data)`
3. Return the desired poll interval in milliseconds

**Return value:**
- Number > 0: next poll interval in ms (capped to 100ms minimum, 60s maximum)
- `nil` or 0: use default interval (1000ms)

**Error handling:** Wrap device reads in `pcall()` so a single failed register read does not crash the entire poll cycle.

```lua
function driver_poll()
    local ok, regs = pcall(host.modbus_read, 100, 10, "input")
    if not ok or not regs then return 5000 end

    host.emit("meter", { w = host.decode_i32(regs[1], regs[2]) })
    return 5000
end
```

#### `driver_cleanup()`

Called when the driver is stopped (shutdown or reload). Release any resources, clear cached state.

```lua
function driver_cleanup()
    cached_data = nil
end
```

### Optional Functions (Control)

#### `driver_command(action, power_w, cmd)`

Called when the EMS control loop dispatches a battery command.

**Parameters:**
- `action` (string) -- one of:
  - `"init"` -- EMS is taking control (acknowledge and return `true`)
  - `"battery"` -- set battery charge/discharge target
  - `"curtail"` -- limit PV output
  - `"curtail_disable"` -- remove PV curtailment
  - `"deinit"` -- EMS is releasing control (revert to autonomous)
- `power_w` (number) -- target power in watts. Signed:
  - Positive = charge the battery
  - Negative = discharge the battery
- `cmd` (string) -- JSON with metadata: `{"id": "ems"}`

**Return:** `true` on success, `false` on failure.

```lua
function driver_command(action, power_w, cmd)
    if action == "init" then
        return true
    elseif action == "battery" then
        if power_w > 0 then
            -- Set charge mode and power limit
            host.modbus_write(REG_MODE, MODE_CHARGE)
            host.modbus_write(REG_CHARGE_LIMIT, math.floor(power_w))
        elseif power_w < 0 then
            -- Set discharge mode and power limit
            host.modbus_write(REG_MODE, MODE_DISCHARGE)
            host.modbus_write(REG_DISCHARGE_LIMIT, math.floor(math.abs(power_w)))
        else
            host.modbus_write(REG_MODE, MODE_AUTO)
        end
        return true
    elseif action == "deinit" then
        host.modbus_write(REG_MODE, MODE_AUTO)
        return true
    end
    return false
end
```

#### `driver_default_mode()`

Called when the EMS heartbeat watchdog expires (default 60 seconds without a successful control cycle). The driver should revert the device to safe autonomous operation.

```lua
function driver_default_mode()
    host.modbus_write(REG_MODE, MODE_AUTO)
end
```

## Sign Conventions

Consistent sign conventions are critical. All drivers must follow these rules:

| DER Type | Field | Positive (+)         | Negative (-)         |
|----------|-------|----------------------|----------------------|
| Meter    | `w`   | Importing from grid  | Exporting to grid    |
| PV       | `w`   | (never positive)     | Generating power     |
| Battery  | `w`   | Charging             | Discharging          |

**Key points:**
- PV `w` is always negative or zero. If your device reports PV as a positive number, negate it before emitting.
- Battery `w` positive means the battery is consuming power (charging). Negative means it is producing power (discharging). If your device uses the opposite convention, negate accordingly.
- Meter `w` positive means the site is consuming from the grid (importing). Negative means the site is feeding into the grid (exporting).

## Telemetry Field Reference

### Meter

Emitted via `host.emit("meter", data)`.

| Field       | Unit | Description                            |
|-------------|------|----------------------------------------|
| `w`         | W    | Total grid power (+import / -export)   |
| `hz`        | Hz   | Grid frequency                         |
| `l1_w`      | W    | Phase 1 power                          |
| `l2_w`      | W    | Phase 2 power                          |
| `l3_w`      | W    | Phase 3 power                          |
| `l1_v`      | V    | Phase 1 voltage                        |
| `l2_v`      | V    | Phase 2 voltage                        |
| `l3_v`      | V    | Phase 3 voltage                        |
| `l1_a`      | A    | Phase 1 current                        |
| `l2_a`      | A    | Phase 2 current                        |
| `l3_a`      | A    | Phase 3 current                        |
| `import_wh` | Wh   | Lifetime energy imported from grid     |
| `export_wh` | Wh   | Lifetime energy exported to grid       |

### PV

Emitted via `host.emit("pv", data)`.

| Field          | Unit | Description                          |
|----------------|------|--------------------------------------|
| `w`            | W    | Total PV power (always negative)     |
| `rated_w`      | W    | Rated/nameplate capacity             |
| `mppt1_v`      | V    | MPPT 1 voltage                       |
| `mppt1_a`      | A    | MPPT 1 current                       |
| `mppt2_v`      | V    | MPPT 2 voltage                       |
| `mppt2_a`      | A    | MPPT 2 current                       |
| `temp_c`       | C    | Inverter/heatsink temperature        |
| `lifetime_wh`  | Wh   | Lifetime PV energy generated         |
| `lower_limit_w`| W    | Minimum curtailment limit            |
| `upper_limit_w`| W    | Maximum curtailment limit            |

### Battery

Emitted via `host.emit("battery", data)`.

| Field          | Unit     | Description                         |
|----------------|----------|-------------------------------------|
| `w`            | W        | Battery power (+charge / -discharge)|
| `v`            | V        | Battery voltage                     |
| `a`            | A        | Battery current                     |
| `soc`          | fraction | State of charge, 0.0 to 1.0         |
| `temp_c`       | C        | Battery temperature                 |
| `charge_wh`    | Wh       | Lifetime energy charged             |
| `discharge_wh` | Wh       | Lifetime energy discharged          |
| `upper_limit_w`| W        | Max charge power limit              |
| `lower_limit_w`| W        | Max discharge power limit           |

**SoC note:** The `soc` field must be a fraction between 0.0 and 1.0 (not a percentage). If your device reports 0-100%, divide by 100.

## How to Add a New Device Driver

### Step 1: Create the Lua file

Create `drivers/mydevice.lua` in the project root.

### Step 2: Declare the protocol

```lua
PROTOCOL = "modbus"  -- or "mqtt"
```

### Step 3: Implement driver_init

```lua
function driver_init(config)
    host.set_make("MyDevice")
    -- For MQTT: subscribe to topics
    -- For Modbus: nothing needed, connection is automatic
end
```

### Step 4: Implement driver_poll

Read device data, convert to standard units, apply sign conventions, and emit telemetry.

### Step 5: Implement driver_command (if the device supports control)

Handle `"init"`, `"battery"`, `"curtail"`, `"curtail_disable"`, and `"deinit"` actions.

### Step 6: Implement driver_default_mode

Revert the device to autonomous/auto mode.

### Step 7: Implement driver_cleanup

Clear any cached state.

### Step 8: Add config entry

Add the driver to `config.yaml`:

```yaml
drivers:
  - name: mydevice
    lua: drivers/mydevice.lua
    battery_capacity_wh: 10000
    modbus:
      host: 192.168.1.50
      port: 502
      unit_id: 1
```

If this device provides the site grid meter, add `is_site_meter: true`.

### Step 9: Test

Run forty-two-watts and check the logs:

```bash
RUST_LOG=debug cargo run -- -c config.yaml
```

Verify telemetry appears in the API:

```bash
curl http://localhost:8080/api/status | jq .
```

## Minimal Driver Template

### Modbus Driver

```lua
-- mydevice.lua
-- MyDevice Inverter Driver (Modbus TCP)
-- Emits: PV, Battery, Meter

PROTOCOL = "modbus"

function driver_init(config)
    host.set_make("MyDevice")
end

function driver_poll()
    -- Read grid power (example: register 100, signed 32-bit, 2 regs)
    local ok, regs = pcall(host.modbus_read, 100, 2, "input")
    if ok and regs then
        local grid_w = host.decode_i32(regs[1], regs[2])
        host.emit("meter", { w = grid_w })
    end

    -- Read PV power (example: register 200, unsigned 16-bit)
    local ok2, pv_regs = pcall(host.modbus_read, 200, 1, "input")
    if ok2 and pv_regs then
        local pv_w = pv_regs[1]
        host.emit("pv", { w = -pv_w })  -- negate: generation is negative
    end

    -- Read battery (example: registers 300-303)
    local ok3, bat_regs = pcall(host.modbus_read, 300, 4, "input")
    if ok3 and bat_regs then
        local bat_w = host.decode_i16(bat_regs[1])  -- already signed
        local bat_soc = bat_regs[2] * 0.1 / 100     -- percent to fraction
        host.emit("battery", { w = bat_w, soc = bat_soc })
    end

    return 5000  -- poll every 5 seconds
end

function driver_command(action, power_w, cmd)
    if action == "init" then return true end
    if action == "battery" then
        -- Write charge/discharge target to device
        -- (implement device-specific register writes here)
        return true
    end
    if action == "deinit" then
        -- Revert to auto mode
        return true
    end
    return false
end

function driver_default_mode()
    -- Revert to autonomous operation
end

function driver_cleanup()
end
```

### MQTT Driver

```lua
-- mydevice_mqtt.lua
-- MyDevice MQTT Driver
-- Emits: PV, Battery, Meter

PROTOCOL = "mqtt"

local cached_data = nil

function driver_init(config)
    host.set_make("MyDevice")
    host.mqtt_subscribe("mydevice/telemetry/#")
end

function driver_poll()
    local messages = host.mqtt_messages()
    if not messages then return 1000 end

    for _, msg in ipairs(messages) do
        local ok, data = pcall(host.json_decode, msg.payload)
        if ok and data then
            cached_data = data
        end
    end

    if cached_data then
        host.emit("meter", {
            w = cached_data.grid_power or 0,
        })
        host.emit("pv", {
            w = -(cached_data.pv_power or 0),
        })
        host.emit("battery", {
            w   = cached_data.battery_power or 0,
            soc = (cached_data.battery_soc or 0) / 100,
        })
    end

    return 1000  -- poll every 1 second (MQTT is event-driven)
end

function driver_command(action, power_w, cmd)
    if action == "init" then return true end
    if action == "battery" then
        local payload = string.format('{"power":%d}', math.floor(power_w))
        return host.mqtt_publish("mydevice/control/battery", payload)
    end
    if action == "deinit" then
        return host.mqtt_publish("mydevice/control/mode", '{"mode":"auto"}')
    end
    return false
end

function driver_default_mode()
    host.mqtt_publish("mydevice/control/mode", '{"mode":"auto"}')
end

function driver_cleanup()
    cached_data = nil
end
```

## Testing Tips

1. **Use `pcall` everywhere.** Every `host.modbus_read` or `host.json_decode` call should be wrapped in `pcall()`. A single communication error should not crash the entire poll cycle.

2. **Log liberally during development.** Use `host.log("debug", "register 100 = " .. tostring(val))` to trace values. Set `RUST_LOG=lua=debug` to see driver log output.

3. **Check the REST API.** After starting forty-two-watts, `curl http://localhost:8080/api/status` shows live telemetry for all drivers. This is the fastest way to verify your driver emits correct values.

4. **Watch sign conventions.** The most common bug is wrong signs. Check that:
   - Meter `w` is positive when importing, negative when exporting
   - PV `w` is always negative (generation)
   - Battery `w` is positive when charging, negative when discharging

5. **Handle SoC as a fraction.** Many devices report SoC as 0-100 (percent). Divide by 100 before emitting so the value is 0.0-1.0.

6. **Use decode helpers.** Do not manually combine register bytes. Use `host.decode_u32()`, `host.decode_i32()`, `host.decode_f32()`, etc. They handle byte order and signedness correctly.

7. **Test the watchdog.** Stop your device (or block the network) and verify that `driver_default_mode()` fires after the configured timeout (default 60s).

8. **Memory limit.** Each driver VM has a 4 MB memory limit. Avoid accumulating large tables across polls -- clear caches in `driver_cleanup()` and keep only the latest data.

9. **Instruction limit.** Each `driver_poll()` call is limited to 50 million Lua instructions. This prevents infinite loops from locking up the system. If your driver hits this limit, optimize or split the work across multiple polls.

10. **Exponential backoff.** The runtime automatically applies exponential backoff on consecutive poll errors (up to 60s). You do not need to implement retry logic yourself -- just return an error and the runtime handles it.
