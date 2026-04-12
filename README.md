# forty-two-watts

Unified Home Energy Management System. Coordinates multiple battery/inverter systems on a shared grid connection to prevent oscillation and optimize self-consumption. Runs as a single Rust binary, loads Lua drivers compatible with the Sourceful Zap gateway, and exposes a REST API plus MQTT integration for Home Assistant.

## Architecture

```
                          config.yaml
                              |
                        +-----v------+
                        |            |
                        |  forty-two-watts  |       REST API :8080
                        |  (Rust)    |-----> GET /api/status
                        |            |       GET /api/health
                        +--+-+-+-+---+       POST /api/mode
                           | | | |           POST /api/target
                           | | | |
            +--------------+ | | +--------------+
            |                | |                |
    +-------v--------+ +----v-v----+   +--------v--------+
    | Driver Thread   | | Control  |   | HA MQTT Bridge   |
    | (Lua sandbox)   | |  Loop    |   | (autodiscovery)  |
    |                 | | (5s)     |   |                  |
    | driver_poll()   | |          |   | fortytwo/status/* |
    | host.emit()     | | compute  |   | fortytwo/command/*|
    |   |             | | dispatch |   +------------------+
    |   v             | |   |      |
    | TelemetryStore <--+   |      |
    | (Arc<Mutex>)    |     v      |
    |                 | driver_command()
    +---------+-------+            |
              |                    |
    +---------v---------+  +------v---------+
    | Modbus TCP Client |  | MQTT Client    |
    | (auto-connected)  |  | (auto-connected|
    +------|------------+  +------|----------+
           |                      |
    +------v------+        +------v------+
    |  Inverter   |        |  Inverter   |
    |  (Sungrow)  |        | (Ferroamp)  |
    +-------------+        +-------------+
```

## Quick Start

### Install

```bash
git clone <repo-url> && cd forty-two-watts
cargo build --release
```

The binary is at `target/release/forty-two-watts`. No system dependencies required -- Lua 5.4 is vendored via `mlua`.

### Configure

Copy the example config and edit it for your setup:

```bash
cp config.example.yaml config.yaml
```

At minimum, configure your driver(s) with the correct IP addresses and credentials. See [Configuration Reference](#configuration-reference) below.

### Run

```bash
# From the project directory (so driver paths resolve correctly)
./target/release/forty-two-watts -c config.yaml

# Or with debug logging
RUST_LOG=debug ./target/release/forty-two-watts -c config.yaml

# Or during development
cargo run -- -c config.yaml
```

The web UI is available at `http://localhost:8080` and the API at `http://localhost:8080/api/`.

## Configuration Reference

```yaml
site:
  name: "Home"                    # Site name (for display)
  control_interval_s: 5           # Control loop interval in seconds (default: 5)
  grid_target_w: 0                # Grid power target in watts (default: 0 = self-consumption)
  grid_tolerance_w: 50            # Deadband: don't adjust if within +/- this value (default: 50)
  watchdog_timeout_s: 60          # Revert to autonomous if driver stalls (default: 60)
  smoothing_alpha: 0.3            # EMA smoothing factor, 0-1 (default: 0.3, lower = smoother)

fuse:
  max_amps: 16                    # Main fuse rating in amps
  phases: 3                       # Number of phases (default: 3)
  voltage: 230                    # Nominal voltage per phase (default: 230)

drivers:
  - name: ferroamp                # Unique driver name
    lua: drivers/ferroamp.lua     # Path to Lua driver (relative to config file)
    is_site_meter: true           # This driver provides the site grid meter (at least one required)
    battery_capacity_wh: 15200    # Battery capacity in Wh (for proportional dispatch)
    mqtt:                         # MQTT connection (for MQTT-based drivers)
      host: 192.168.1.153
      port: 1883                  # Default: 1883
      username: user              # Optional
      password: pass              # Optional

  - name: sungrow
    lua: drivers/sungrow.lua
    battery_capacity_wh: 9600
    modbus:                       # Modbus TCP connection (for Modbus-based drivers)
      host: 192.168.1.10
      port: 502                   # Default: 502
      unit_id: 1                  # Modbus slave ID (default: 1)

api:
  port: 8080                      # REST API port (default: 8080)

homeassistant:                    # Optional: Home Assistant MQTT integration
  enabled: true                   # Default: true
  broker: 192.168.1.1             # HA Mosquitto broker IP
  port: 1883                     # Default: 1883
  username: fortytwo               # Optional
  password: fortytwo               # Optional
  publish_interval_s: 5           # Sensor update interval (default: 5)

state:
  path: state.redb                # Persistent state file (default: state.redb)
```

### Validation Rules

- At least one driver must be configured
- At least one driver must have `is_site_meter: true`
- Each driver must have either `mqtt` or `modbus` config
- `smoothing_alpha` must be between 0 (exclusive) and 1 (inclusive)

## API Reference

### `GET /api/health`

System health check.

```json
{
  "status": "ok",
  "drivers_ok": 2,
  "drivers_degraded": 0,
  "drivers_offline": 0
}
```

`status` is `"ok"` when all drivers are online, `"degraded"` when any driver is offline.

### `GET /api/status`

Full system status including aggregated telemetry, per-driver details, and dispatch targets.

```json
{
  "mode": "self_consumption",
  "grid_w": 150.0,
  "pv_w": -3200.0,
  "bat_w": 1500.0,
  "bat_soc": 0.72,
  "grid_target_w": 0.0,
  "drivers": {
    "ferroamp": {
      "status": "Ok",
      "meter_w": 150.0,
      "pv_w": -2000.0,
      "bat_w": 1000.0,
      "bat_soc": 0.75,
      "consecutive_errors": 0,
      "tick_count": 1234
    },
    "sungrow": {
      "status": "Ok",
      "pv_w": -1200.0,
      "bat_w": 500.0,
      "bat_soc": 0.68,
      "consecutive_errors": 0,
      "tick_count": 1230
    }
  },
  "dispatch": [
    {"driver": "ferroamp", "target_w": 1000.0, "clamped": false},
    {"driver": "sungrow", "target_w": 500.0, "clamped": false}
  ]
}
```

### `GET /api/mode`

Current operating mode and grid target.

```json
{
  "mode": "self_consumption",
  "grid_target_w": 0.0
}
```

### `POST /api/mode`

Set operating mode and optional dispatch parameters.

```bash
# Set mode
curl -X POST http://localhost:8080/api/mode \
  -d '{"mode": "self_consumption"}'

# Set priority order
curl -X POST http://localhost:8080/api/mode \
  -d '{"mode": "priority", "priority_order": ["ferroamp", "sungrow"]}'

# Set custom weights
curl -X POST http://localhost:8080/api/mode \
  -d '{"mode": "weighted", "weights": {"ferroamp": 0.7, "sungrow": 0.3}}'
```

### `POST /api/target`

Set the grid power target.

```bash
# Self-consumption (target 0W grid)
curl -X POST http://localhost:8080/api/target \
  -d '{"grid_target_w": 0}'

# Allow 200W import from grid
curl -X POST http://localhost:8080/api/target \
  -d '{"grid_target_w": 200}'
```

### `GET /api/drivers`

List all drivers with health status.

```json
[
  {
    "name": "ferroamp",
    "status": "Ok",
    "consecutive_errors": 0,
    "tick_count": 1234,
    "last_error": null
  }
]
```

## Dispatch Modes

### Idle

No dispatch commands are sent. All battery systems run autonomously in whatever mode they were left in. Use this to temporarily disable EMS control without stopping the service.

### Self-Consumption (default)

Targets `grid_target_w` (default 0W = zero grid exchange). The error between actual grid power and the target is split **proportionally by battery capacity** across all online batteries.

Example with two batteries (15 kWh and 10 kWh) and 2500W excess PV:
- Ferroamp (15 kWh): receives 60% of the error = 1500W charge
- Sungrow (10 kWh): receives 40% of the error = 1000W charge

### Charge

Forces all online batteries to charge at maximum power (5 kW target, clamped by the inverter's own limits). Useful for grid charging during cheap tariff periods.

### Priority

One battery handles all the error first. The secondary battery only receives the overflow when the primary is saturated (e.g., full or at max power). Configure via the API:

```bash
curl -X POST http://localhost:8080/api/mode \
  -d '{"mode": "priority", "priority_order": ["ferroamp", "sungrow"]}'
```

### Weighted

Custom weights instead of capacity-proportional dispatch. Configure via the API:

```bash
curl -X POST http://localhost:8080/api/mode \
  -d '{"mode": "weighted", "weights": {"ferroamp": 0.8, "sungrow": 0.2}}'
```

## Fuse Guard

The fuse guard prevents total generation (PV + battery discharge) from exceeding the main fuse rating. This protects the physical breaker on the grid connection.

**Calculation:** `fuse_max_w = max_amps * voltage * phases`

Example: 16A 3-phase at 230V = 11,040W maximum.

If total PV generation plus battery discharge exceeds this limit, all discharge targets are proportionally scaled down. Charge targets are not affected (charging reduces current on the fuse).

The fuse guard only limits instantaneous power. It does not account for per-phase current imbalance -- that is the responsibility of individual inverters.

## Building from Source

### Requirements

- Rust 1.85+ (2024 edition)
- No system Lua required (Lua 5.4 is vendored)

### Build

```bash
cargo build --release
```

### Run Tests

```bash
cargo test
```

### Dependencies

| Crate               | Purpose                                   |
|----------------------|-------------------------------------------|
| `mlua`               | Lua 5.4 runtime (vendored, no system dep) |
| `redb`               | Embedded key-value DB for state persistence |
| `tiny_http`          | HTTP server for REST API and web UI       |
| `serde` / `serde_json` / `serde_yaml` | Serialization           |
| `tracing`            | Structured logging                        |
| `ctrlc`              | Graceful shutdown on SIGINT/SIGTERM       |

No tokio, no async runtime. The architecture uses synchronous OS threads with `Arc<Mutex<>>` for shared state.

### Project Layout

```
forty-two-watts/
  src/
    main.rs          # Entry point, driver threads, control loop
    config.rs        # YAML config parsing and validation
    control.rs       # Dispatch modes (self-consumption, priority, etc.)
    telemetry.rs     # Shared telemetry store with EMA smoothing
    api.rs           # REST API server
    ha.rs            # Home Assistant MQTT bridge
    state.rs         # Persistent state (redb)
    lua/
      mod.rs         # Lua module root
      driver.rs      # Driver lifecycle (load, init, poll, command)
      runtime.rs     # Lua VM wrapper with memory limits
      sandbox.rs     # Sandbox: blocked globals, isolated environments
      json.rs        # Lua <-> JSON conversion
      host_api/
        mod.rs       # Host API registration
        core.rs      # host.log, host.millis, host.set_make, host.set_sn
        decode.rs    # host.decode_*, host.scale, host.json_*
        modbus.rs    # host.modbus_read, host.modbus_write
        mqtt.rs      # host.mqtt_subscribe, host.mqtt_messages, host.mqtt_publish
        telemetry.rs # host.emit
    modbus/
      mod.rs         # Modbus client (FC 0x03, 0x04, 0x06, 0x10)
      tcp.rs         # Modbus TCP transport (MBAP framing)
    mqtt/
      client.rs      # Minimal sync MQTT 3.1.1 client
  drivers/
    ferroamp.lua     # Ferroamp EnergyHub driver (MQTT)
    sungrow.lua      # Sungrow SH Hybrid driver (Modbus TCP)
  web/
    index.html       # Web UI
  docs/
    lua-drivers.md   # Driver authoring guide
    host-api.md      # Host API reference
    ha-integration.md # Home Assistant setup guide
  config.example.yaml
```

## License

MIT
