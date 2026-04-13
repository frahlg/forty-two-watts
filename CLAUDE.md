# forty-two-watts

Unified Home Energy Management System. Coordinates multiple battery/inverter systems on a shared grid connection to prevent oscillation and optimize self-consumption.

## Architecture

Rust binary that loads Lua drivers (from Sourceful's srcful-device-support registry), runs a 5s control loop, and dispatches battery power targets. Exposes REST API + MQTT for Home Assistant integration.

## Key concepts

- **Lua drivers**: Self-contained scripts implementing `driver_init/poll/command/cleanup`. Same drivers that run on the Sourceful Zap gateway.
- **Host API**: The `host.*` namespace exposed to Lua drivers (MQTT, Modbus, decode helpers, telemetry emit)
- **Telemetry Store**: Central shared state. Kalman-filtered per-signal smoothing (auto-adaptive noise), separate slow filter for load.
- **Control Loop**: configurable interval (default 5s) reads telemetry, runs PI controller (Kp=0.5, Ki=0.1, anti-windup at ±3000W), applies slew limit, dispatches.
- **Battery Models** (`battery_model.rs`): Per-battery ARX(1) model learned online via RLS. Provides τ (time constant), steady-state gain, saturation curves per SoC, deadband, hardware health score. See [docs/battery-models.md](docs/battery-models.md).
- **Cascade controller** (in `control.rs`): When models present, each battery gets its own inner PI loop (auto-tuned from learned τ) + saturation clamp + inverse-model command transformation. Falls back to direct command when models missing.
- **Self-tune** (`self_tune.rs`): Manual calibration sequence (3 min/battery) — drives each battery through known steps, fits ARX(1) from response, writes as baseline for health drift detection.
- **Fuse Guard**: Ensures total generation (PV + battery discharge) never exceeds the shared breaker limit.
- **Energy Accumulator**: Wh integrated from W on every cycle. Today / total split, day rollover at UTC midnight, persisted to redb.
- **DriverRegistry**: Manages driver thread lifecycle. `add()`, `remove()`, `reload()` (diffs configs and applies). All hot — no restart.
- **Config Reload**: File watcher (`notify` crate) on `config.yaml`. Diff vs current, apply per-subsystem. Settings UI writes to the same yaml via `save_atomic` (tmp + rename) → file watcher picks it up. Round-trip path: GET `/api/config` → edit → POST `/api/config` → save yaml → diff + apply → swap `Arc<RwLock<Config>>`.
- **Tiered History**: redb-backed. Hot tier: 30 days at 5s. Warm: 12 months at 15min buckets. Cold: forever at 1d buckets. Auto-aggregation on prune.

## Hot-reload boundaries

What hot-reloads cleanly:
- **Control tuning**: `grid_target_w`, `grid_tolerance_w`, `slew_rate_w`, `min_dispatch_interval_s` — applied to `ControlState` in-place
- **Drivers**: add/remove/restart via `DriverRegistry::reload()`. Diff compares `lua` path, `is_site_meter`, `battery_capacity_wh`, MQTT host/port/auth, Modbus host/port/unit_id. Any change → restart that driver thread.
- **Per-cycle config**: `fuse.max_amps/voltage/phases`, `price.*`, `weather.*`, `batteries.*` — read fresh each control cycle from `current_config: Arc<RwLock<Config>>`

What requires restart:
- `homeassistant.*` (MQTT broker reconnect not yet implemented)
- `api.port` (socket bind happens at startup)
- `state.path` (redb file is opened once)

## Building & testing

```bash
cargo build --release
cargo test --release           # 104+ inline tests, no external deps
cargo run -- config.yaml
```

Tests live inline as `#[cfg(test)] mod tests` in each module. No `tests/` directory — keeps unit + integration close to the code. `tempfile` is the only dev-dep.

## Release & deploy

```bash
./scripts/release.sh v0.X.Y    # Builds arm64 + amd64 musl statics via Docker, creates GitHub release
./scripts/deploy.sh homelab-rpi # Pulls latest, replaces binary + drivers + web, restarts
```

## Project layout

- `src/main.rs` — Entry: load config, spawn drivers via `DriverRegistry`, start API + HA + file watcher, run control loop
- `src/config.rs` — YAML schema + validation. All types `Clone + Serialize + Deserialize` for round-trip via UI.
- `src/driver_registry.rs` — Dynamic driver lifecycle. `add()`/`remove()`/`reload()` + `diff_drivers()` pure helper.
- `src/config_reload.rs` — File watcher (`notify`) + `reload()` + `save_atomic()` (tmp + rename).
- `src/control.rs` — Site PI controller + dispatch modes + fuse guard + slew rate + cascade (per-battery inner PI + inverse model + saturation clamp).
- `src/battery_model.rs` — Per-battery RLS estimator (ARX(1)), saturation curve tracking, hardware-health drift detection.
- `src/self_tune.rs` — Self-tune state machine: step-response sequence + first-order fit + writes baseline.
- `src/telemetry.rs` — `TelemetryStore` + `KalmanFilter1D` + `DriverHealth`.
- `src/energy.rs` — `EnergyAccumulator` (Wh integration) + day rollover.
- `src/state.rs` — `StateStore` (redb): config, telemetry snapshots, events (ms-keyed), tiered history with auto-aggregation.
- `src/api.rs` — REST. `apply_config_update()` is the testable core for POST /api/config.
- `src/ha.rs` — Home Assistant MQTT autodiscovery + publish + command subscribe.
- `src/lua/` — Lua runtime, sandbox, host API (decode/modbus/mqtt/telemetry).
- `drivers/` — Lua drivers (ferroamp.lua, sungrow.lua)
- `web/index.html`, `style.css`, `app.js` — Dashboard
- `web/settings.js` — 6-tab settings modal (Control / Devices / Price / Weather / Batteries / Home Assistant). GETs `/api/config`, edits in place, POSTs back.
- `web/models.js` — Battery Models panel + self-tune modal. Polls `/api/battery_models` every 3s, drives `/api/self_tune/*`.
- `docs/` — `lua-drivers.md`, `host-api.md`, `ha-integration.md`, `configuration.md`, `battery-models.md`
- `config.example.yaml` — Example configuration

## Dependencies

- `mlua` — Lua 5.4 runtime (vendored, no system Lua needed)
- `redb` — Embedded key-value DB for state persistence (pure Rust, zero deps)
- `tiny_http` — HTTP server for REST API + web UI
- `notify` — File watcher for hot config reload
- `pid` — PI/PID controller with anti-windup
- `serde` / `serde_json` / `serde_yaml` — Serialization
- `tracing` — Structured logging

No tokio, no async. Sync threads with `Arc<Mutex<>>` and `Arc<RwLock<Config>>` as the single source of truth for config.

## Code reuse

Lua runtime, sandbox, Modbus client, and host API decode helpers are ported from the Sourceful zap-os repository (`crates/zap-core/src/`).
