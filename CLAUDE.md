# forty-two-watts

Unified Home Energy Management System. Coordinates multiple battery/inverter systems on a shared grid connection to prevent oscillation and optimize self-consumption.

## Architecture

Rust binary that loads Lua drivers (from Sourceful's srcful-device-support registry), runs a 5s control loop, and dispatches battery power targets. Exposes REST API + MQTT for Home Assistant integration.

## Key concepts

- **Lua drivers**: Self-contained scripts implementing `driver_init/poll/command/cleanup`. Same drivers that run on the Sourceful Zap gateway.
- **Host API**: The `host.*` namespace exposed to Lua drivers (MQTT, Modbus, decode helpers, telemetry emit)
- **Telemetry Store**: Central shared state with EMA smoothing and staleness tracking
- **Control Loop**: 5s cycle reads telemetry, computes targets, dispatches via `driver_command()`
- **Fuse Guard**: Ensures total current never exceeds the shared breaker limit

## Building

```bash
cargo build
cargo run -- -c config.yaml
```

## Project layout

- `src/` — Rust source
- `drivers/` — Lua drivers (ferroamp.lua, sungrow.lua)
- `web/` — Static web UI (plain HTML/CSS/JS, no build step)
- `docs/` — Driver authoring guide, host API reference, HA integration guide
- `config.example.yaml` — Example configuration

## Dependencies

- `mlua` — Lua 5.4 runtime (vendored, no system Lua needed)
- `redb` — Embedded key-value DB for state persistence (pure Rust, zero deps)
- `tiny_http` — HTTP server for REST API + web UI
- `serde` / `serde_json` / `serde_yaml` — Serialization
- `tracing` — Structured logging

No tokio, no async. Sync threads with Arc<Mutex<>>.

## Code reuse

Lua runtime, sandbox, Modbus client, and host API decode helpers are ported from the Sourceful zap-os repository (`crates/zap-core/src/`).
