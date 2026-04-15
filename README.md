# forty-two-watts 🐬

> *"The Answer to the Ultimate Question of Life, the Universe, and Grid Balancing is... 42 watts."*

Unified Home Energy Management System. Coordinates multiple batteries + PV +
grid + loads on a shared grid connection so they don't oscillate or blow the
main fuse.

Three layers in one binary:

1. **Inner control loop** (5 s) — PI + cascade + fuse + SoC clamps,
   executes grid-power targets no matter where they come from.
2. **MPC planner** (15 min) — dynamic programming over a discretized
   SoC grid, 48-hour horizon, three strategies (self-consumption /
   cheap charging / arbitrage), confidence-weighted when prices are
   ML-forecasted.
3. **Digital twins** (1 min) — online RLS / bucket models that learn
   the system's PV curve, household load profile, and spot-price
   pattern from its own telemetry. Feed slot-by-slot forecasts into
   the MPC.

The Go port is now **mainline on `master`**. The previous Rust
implementation lives alongside in `src/` + `Cargo.toml` and is frozen
(see `MIGRATION_PLAN.md` for historical context).

## What's new in v2.1+

- **Lua drivers are now the primary path.** Drop a `.lua` file in
  `drivers/`, no build step. WASM drivers still load via the same
  registry and capability ABI, but Lua is recommended for anything new.
  See [`docs/architecture.md`](docs/architecture.md) for the current
  driver host and [`docs/writing-a-driver.md`](docs/writing-a-driver.md)
  for the walkthrough.
- **Long-format TSDB.** `ts_drivers` / `ts_metrics` / `ts_samples`
  (WITHOUT ROWID, STRICT) in SQLite, with automatic daily Parquet
  rolloff past 14 days. Drivers push arbitrary scalar diagnostics with
  `host.emit_metric(name, value)`. Details in
  [`docs/tsdb.md`](docs/tsdb.md).
- **Hardware-stable device identity.** Every device gets a `device_id`
  resolved as `make:serial` > `mac:<arp-resolved>` > `ep:<endpoint>`.
  Battery models and other persistent state are keyed on `device_id`,
  so renaming a driver no longer orphans training data. See
  [`docs/device-identity.md`](docs/device-identity.md).
- **`sunpos` package.** Physics-only solar position (Spencer 1971),
  used as a prior for the data-driven PV twin and as groundwork for
  upcoming auto-PV.
- **Watchdog safety.** `tel.WatchdogScan` flips stale drivers offline
  and reverts them to autonomous mode; a stale site-meter
  short-circuits the dispatch cycle.

Start with [`docs/architecture.md`](docs/architecture.md) for the
top-of-funnel overview.

---

## Supported devices

17 Lua drivers covering 15 manufacturers — Ferroamp, Sungrow, Solis,
Huawei, Deye, SMA, Fronius, SolarEdge, Eastron SDM, Pixii, Kostal,
GoodWe, Growatt, Sofar, Victron. Six drivers participate in EMS
dispatch control; the rest are read-only telemetry.

See [`docs/driver-catalog.md`](docs/driver-catalog.md) for the full
table (protocols, capabilities, control, tested models) and
[`docs/writing-a-driver.md`](docs/writing-a-driver.md) if your device
isn't listed.

---

## Architecture in one sentence

A Go binary runs the control loop, the HTTP API, and the Home Assistant
bridge; Lua driver modules (one `.lua` file per device type, WASM still
supported) do all protocol work — MQTT, Modbus, JSON parsing, bit
twiddling — inside a capability-scoped sandbox with a tiny host API.

```
┌────────────────────────────────────────────────────────────────┐
│                   forty-two-watts (Go)                          │
│                                                                 │
│  ┌──────────┐  ┌──────────┐   Lua drivers (gopher-lua)         │
│  │ferroamp  │  │ sungrow  │   — fat. all protocol logic here.  │
│  │  .lua    │  │  .lua    │   (legacy .wasm still supported    │
│  └────┬─────┘  └────┬─────┘    via the same Registry/ABI)      │
│       │              │                                          │
│       ▼              ▼                                          │
│  ┌──────────────────────────┐                                  │
│  │    Telemetry store        │  (site sign convention —        │
│  │    + Kalman smoothing     │   + = import to site,           │
│  │    + driver health        │   PV − (generation),            │
│  └──────────────┬───────────┘   bat + charge / − discharge)    │
│                 │                                               │
│  ┌──────────────▼────────────┐  ┌────────────────────────┐    │
│  │  Control loop              │  │  HTTP API + web UI     │    │
│  │  PI + cascade + self-tune  │  │  :8080                 │    │
│  │  + ARX(1) RLS per battery  │  └────────────────────────┘    │
│  │  + watchdog / stale-meter  │                                 │
│  └──────────────┬────────────┘                                  │
│                 │                 ┌────────────────────────┐    │
│                 └────────────────▶│  HA MQTT bridge         │    │
│                                   │  (autodiscovery)        │    │
│                                   └────────────────────────┘    │
│  ┌──────────────────────────┐                                  │
│  │  SQLite state DB         │  config, events, devices,        │
│  │  + tiered history        │  battery models (keyed on        │
│  │  + long-format TS (14d)  │  device_id), history hot/warm/   │
│  │  + Parquet cold (>14d)   │  cold tiers, prices, forecasts   │
│  └──────────────────────────┘                                  │
│                                                                 │
│  ┌──────────────────────────┐   ┌────────────────────────┐     │
│  │  MPC planner (15 min)    │◀──│  Digital twins (1 min) │     │
│  │  • DP over SoC grid      │   │  • pvmodel (RLS)       │     │
│  │  • 48 h horizon          │   │  • loadmodel (buckets) │     │
│  │  • three strategies      │   │  • priceforecast       │     │
│  │  • confidence blending   │   │  • sunpos prior        │     │
│  │  • per-slot reasons      │   │    (physics-only)      │     │
│  └────────────┬─────────────┘   └────────────────────────┘     │
│               │  grid_target_w per slot                         │
│               └─────▶ consumed by the control loop above        │
└────────────────────────────────────────────────────────────────┘
```

Architecture walkthrough: [`docs/architecture.md`](docs/architecture.md)
Sign convention (critical): [`docs/site-convention.md`](docs/site-convention.md)
Historical migration rationale: [`MIGRATION_PLAN.md`](MIGRATION_PLAN.md)

## Quick start

Prereqs: Go 1.22+, `make`. Rust + `wasm32-wasip1` only needed if you
want to build legacy WASM drivers — the default Lua drivers need
nothing more.

```bash
# Build Go binaries + (if present) WASM drivers, run the full local stack with simulators
make dev

# Open the UI
open http://localhost:8080
```

That's it — no real hardware, no external MQTT broker needed. Two
simulators stand in for a Ferroamp EnergyHub (MQTT) and a Sungrow
SH10RT (Modbus TCP) with realistic first-order battery response.

## Driver development

Writing a new driver is the usual way to add device support. Drop a
`.lua` file in `drivers/`, wire it into `config.yaml`, restart. No
build step.

- [`docs/writing-a-driver.md`](docs/writing-a-driver.md) — canonical
  walkthrough: DRIVER table, lifecycle functions, host API reference,
  sign convention, pitfalls.
- [`docs/writing-a-driver-with-claude-code.md`](docs/writing-a-driver-with-claude-code.md)
  — hands-on recipe for using Claude Code to bootstrap a driver from
  a vendor register map: prompts, porting checklist, common mistakes,
  iteration loop.
- [`docs/testing-drivers-live.md`](docs/testing-drivers-live.md) —
  runbook for iterating against the bundled simulators and against
  real hardware on a Raspberry Pi, with verified `curl` + `make`
  commands for every common loop.
- `drivers/sungrow.lua` — full Modbus TCP reference implementation.
- `drivers/ferroamp.lua` — full MQTT reference implementation.

## Features

- **PI controller** with anti-windup, 6 dispatch modes (idle,
  self_consumption, peak_shaving, charge, priority, weighted)
- **Cascade control** — per-battery inner PI tuned from the learned
  τ, plus inverse-gain compensation so commands actually land
- **Online learning** — ARX(1) model per battery via RLS with forgetting
  factor, capability-aware saturation curves, hardware-health drift
- **Self-tune** — 3-minute step-response calibration per battery to set
  a clean baseline; safety-gated by confidence
- **Lua drivers** — FAT drivers. The host provides only capabilities
  (MQTT, Modbus, time, logging, TS metric emit). Each driver does its
  own protocol parsing, state management, command translation. WASM
  drivers still load through the same Registry for anyone shipping a
  `.wasm` binary from v2.0.
- **Hardware-stable device identity** — `make:serial` >
  `mac:<arp-resolved>` > `ep:<endpoint>`. Persistent state (battery
  models, calibration history) survives driver renames.
- **Watchdog** — stale drivers flip offline and revert to autonomous
  mode; stale site-meter short-circuits the dispatch cycle.
- **Hot-reload** — `config.yaml` + settings UI round-trip, file watcher
  applies changes live for 99% of settings
- **Long-format TSDB** — `host.emit_metric(name, value)` lands
  diagnostics in an interned SQLite schema (`ts_samples`,
  WITHOUT ROWID, STRICT). Past 14 days rolls off to daily Parquet
  files for long-term retention.
- **Tiered history** — 30d at 5s, 12mo at 15min buckets, forever at 1d
  buckets. Pure SQL aggregation (SQLite, no CGo).
- **Home Assistant MQTT** — autodiscovery publishes sensors for grid,
  PV, battery, load, SoC, per-driver + mode/target/peak/EV commands

## Deploy to a Raspberry Pi

```bash
# One-time (only needed if you ship WASM drivers): wasm32-wasip1 toolchain
rustup target add wasm32-wasip1

# Build the release tarballs (arm64 + amd64)
make release

# Push a release via GitHub (if you want to deploy from CI later)
gh release create v1.0.0 release/*.tar.gz --generate-notes

# Or deploy directly
./scripts/deploy-go.sh homelab-rpi
```

## Library choices (all pure Go)

| Need | Choice | Why |
|---|---|---|
| Lua runtime | [gopher-lua](https://github.com/yuin/gopher-lua) | Pure Go Lua 5.1, zero CGo |
| WASM runtime | [wazero](https://wazero.io) | Zero CGo, zero deps, prod-ready |
| State DB | [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) | Pure Go SQLite, SQL queries for history |
| Parquet (cold tier) | [parquet-go/parquet-go](https://github.com/parquet-go/parquet-go) | Pure Go, zstd + dictionary encoding |
| MQTT client | eclipse/paho.mqtt.golang | Battle-tested |
| MQTT broker (tests/sim) | mochi-mqtt/server | Embeddable |
| Modbus TCP | simonvetter/modbus | Both client and server |
| File watcher | fsnotify/fsnotify | Cross-platform |
| YAML | gopkg.in/yaml.v3 | Standard |
| HTTP | stdlib `net/http` | Go 1.22+ method-scoped routing |
| Logging | stdlib `log/slog` | Structured, inbuilt |

Nothing exotic. Nothing that requires a C toolchain. One `go build`
produces a static binary that drops onto a Pi.

## Repo layout

```
forty-two-watts/
├── go/
│   ├── cmd/
│   │   ├── forty-two-watts/   # main binary
│   │   ├── sim-ferroamp/      # embedded MQTT broker + Ferroamp fake
│   │   └── sim-sungrow/       # Modbus TCP Sungrow fake
│   ├── internal/
│   │   ├── api/               # HTTP handlers
│   │   ├── arp/               # L2 MAC resolver (linux/darwin)
│   │   ├── battery/           # ARX(1) + RLS + cascade
│   │   ├── config/            # YAML + validation
│   │   ├── configreload/      # fsnotify watcher
│   │   ├── control/           # PI + dispatch modes + fuse guard
│   │   ├── drivers/           # Lua host + wazero host + registry
│   │   ├── ha/                # Home Assistant MQTT bridge
│   │   ├── loadmodel/         # household load twin
│   │   ├── mpc/               # MPC planner (DP over SoC grid)
│   │   ├── mqtt/              # paho wrapper
│   │   ├── modbus/            # simonvetter wrapper
│   │   ├── priceforecast/     # price twin (fills past day-ahead)
│   │   ├── pvmodel/           # PV twin (RLS over sunpos/cloud)
│   │   ├── selftune/          # step-response calibration
│   │   ├── state/             # SQLite + tiered history + long-format TS + Parquet + devices
│   │   ├── sunpos/            # solar position (Spencer 1971)
│   │   └── telemetry/         # DER store + Kalman + health + watchdog
│   └── test/e2e/              # full-stack integration test
├── drivers/                   # Lua drivers (ferroamp.lua, sungrow.lua, …)
├── wasm-drivers/
│   ├── ferroamp/              # legacy Rust → wasm32-wasip1
│   └── sungrow/               #     (~280 LOC each)
├── drivers-wasm/              # compiled .wasm modules (gitignored)
├── web/                       # static UI (HTML/CSS/JS)
├── docs/                      # architecture + operator docs
├── config.example.yaml        # sample config
├── Makefile                   # build orchestration
└── MIGRATION_PLAN.md          # historical Rust→Go migration rationale
```

## Testing

```bash
make test        # all unit + integration tests (Go + Rust)
make e2e         # full-stack end-to-end test (simulates real hardware)
```

The e2e test stands up both simulators, loads the compiled drivers,
runs the control loop, and verifies:
- Drivers load, initialize, emit telemetry
- Site sign convention holds (PV −, grid + for import, bat + for charge)
- Control loop responds to grid-target changes in the correct direction
- Mode switching persists through the state DB
- Battery models accumulate samples through RLS
- Per-model reset wipes them cleanly
- Settings round-trip through the HTTP API

## Documentation

- [`docs/site-convention.md`](docs/site-convention.md) — THE sign convention, enforced at driver boundary
- [`docs/battery-models.md`](docs/battery-models.md) — ARX(1), RLS, cascade, self-tune
- [`docs/clamping.md`](docs/clamping.md) — the seven clamps and why each matters
- [`docs/configuration.md`](docs/configuration.md) — full YAML schema reference
- [`docs/mpc-planner.md`](docs/mpc-planner.md) — MPC strategies, confidence blending, decision reasons
- [`docs/ml-twins.md`](docs/ml-twins.md) — PV + load + price digital twins (older, being superseded)
- [`docs/ha-integration.md`](docs/ha-integration.md) — Home Assistant MQTT bridge
- [`docs/host-api.md`](docs/host-api.md) — legacy WASM driver ABI
- [`docs/lua-drivers.md`](docs/lua-drivers.md) — earlier Lua driver notes
- [`MIGRATION_PLAN.md`](MIGRATION_PLAN.md) — historical: why Go + WASM, library evaluations

These docs are being added by parallel work and will resolve once the
sibling PRs land — the `docs/architecture.md` and `docs/writing-a-driver.md`
links referenced at the top of this README are part of that set:
`architecture.md`, `ml-models.md`, `tsdb.md`, `device-identity.md`,
`safety.md`, `writing-a-driver.md`, `api.md`, `operations.md`, `testing.md`.

---

*So long, and thanks for all the watts.* 🐬
