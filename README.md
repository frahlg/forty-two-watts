# forty-two-watts 🐬

> *"The Answer to the Ultimate Question of Life, the Universe, and Grid Balancing is... 42 watts."*

A unified Home Energy Management System that coordinates multiple battery systems on a shared grid connection. Because having two independent battery controllers fight over the same meter is about as productive as a Vogon poetry reading.

## The Problem

You have two energy systems (say, a Ferroamp EnergyHub and a Sungrow hybrid inverter) both running self-consumption mode, both watching the same grid meter, both convinced *they* should be the one to zero it out. The result? They oscillate. They overshoot. They chase each other's tails like a pair of mattresses from Sqornshellous Zeta.

## The Answer

**forty-two-watts** sits above both systems as a single coordinating intelligence. It reads telemetry from all devices, applies a PI controller with Kalman-filtered inputs, and dispatches power targets to each battery — proportionally, by priority, or with custom weights. The default deadband? 42 watts, naturally.

When your grid power is within 42W of the target, the system logs `Don't Panic` and holds steady.

```
┌──────────────────────────────────────────────────────────┐
│              forty-two-watts process 🐬                   │
│                                                           │
│  ┌──────────┐  ┌──────────┐  Lua driver threads          │
│  │ Ferroamp │  │ Sungrow  │  (poll every 1-5s)           │
│  │   🔌 MQTT │  │  🔌 Modbus│                             │
│  └────┬─────┘  └────┬─────┘                              │
│       │              │                                    │
│       ▼              ▼                                    │
│  ┌─────────────────────────────┐                         │
│  │    Kalman-filtered State    │  (auto-adaptive noise)  │
│  └──────────────┬──────────────┘                         │
│                 │                                         │
│  ┌──────────────▼──────────────┐  ┌───────────────────┐  │
│  │    PI Controller + Fuse     │  │  REST API + Web   │  │
│  │    Guard + Slew Limiter     │  │  :8080            │  │
│  └──────────────┬──────────────┘  └───────────────────┘  │
│                 │                  ┌───────────────────┐  │
│                 └─────────────────▶  HA MQTT bridge    │  │
│                                    │  (autodiscovery)  │  │
│                                    └───────────────────┘  │
└──────────────────────────────────────────────────────────┘
```

## Features

- **PI Controller** with anti-windup — because proportional-only control is for mostly harmless systems
- **Per-Battery Online Learning** — RLS estimates each battery's response model (τ, gain, saturation curve) continuously. Cascade controller uses the model to compensate for individual quirks. See [docs/battery-models.md](docs/battery-models.md)
- **Self-tune** — manual calibration: 3-min step response per battery → fits ARX(1) model → sets baseline for hardware health
- **Hardware health detection** — gain drift over time signals battery aging before it becomes catastrophic
- **1D Kalman Filter** per signal — auto-adapts to noise. Like the Babel Fish, but for watts
- **Lua Driver System** — same drivers that run on the Sourceful Zap gateway. Drop in a `.lua` file, get a new device
- **6 Dispatch Modes**: idle, self_consumption, peak_shaving, charge, priority, weighted
- **Fuse Guard** — respects your breaker limits, because tripping a fuse is the grid equivalent of destroying Earth to build a hyperspace bypass
- **Slew Rate Limiter** — smooth power ramps, no step changes
- **Home Assistant MQTT** — autodiscovery, sensor publishing, mode control via commands
- **Web Dashboard** with **Settings UI** — real-time chart, plus a 6-tab settings modal that edits all config live
- **Hot Reload** — change *anything* (control tuning, drivers, HA, price, weather) without restarting. Edit the yaml, the file watcher picks it up. Or use the UI — same path
- **Dynamic Drivers** — add/remove devices via the Settings UI. Threads spin up/down on the fly
- **Tiered History** — 30 days at 5s, 12 months at 15min, forever at 1d. Auto-aggregated
- **Energy as Primary** — Wh integrated and persisted; W is derived for control
- **Crash Recovery** — redb state persistence, resumes where it left off

## Quick Start

```bash
# Download
curl -sL https://github.com/frahlg/forty-two-watts/releases/latest/download/forty-two-watts-linux-arm64.tar.gz | tar xz

# Configure
cp config.example.yaml config.yaml
# Edit config.yaml with your device IPs

# Don't Panic
./forty-two-watts-linux-arm64 config.yaml

# Open the Hitchhiker's Guide to your Grid
open http://localhost:8080
```

## Configuration

Two ways to configure: **the Settings UI** (preferred) or **edit `config.yaml` directly**. Both are equivalent — the yaml is canonical and human-readable, the UI just edits it. Either way, changes hot-apply without a restart. See [docs/configuration.md](docs/configuration.md) for the full reference.

```yaml
site:
  name: "Heart of Gold"
  control_interval_s: 5           # how often Deep Thought thinks
  grid_target_w: 0                # The Question
  grid_tolerance_w: 42            # The Answer (naturally)
  watchdog_timeout_s: 60

fuse:
  max_amps: 16                    # don't blow up Earth
  phases: 3
  voltage: 230

drivers:
  - name: ferroamp
    lua: drivers/ferroamp.lua
    is_site_meter: true
    battery_capacity_wh: 15200
    mqtt:
      host: 192.168.1.153
      port: 1883
      username: extapi
      password: ferroampExtApi

  - name: sungrow
    lua: drivers/sungrow.lua
    battery_capacity_wh: 9600
    modbus:
      host: 192.168.1.10
      port: 502
      unit_id: 1

# Optional sections — all hot-reloadable
price:
  provider: elprisetjustnu      # or entsoe (needs api_key) or none
  zone: SE3                     # SE1-SE4
  grid_tariff_ore_kwh: 50
  vat_percent: 25

weather:
  provider: met_no              # or openweather or none
  latitude: 59.3293
  longitude: 18.0686

batteries:                      # per-battery overrides (optional)
  ferroamp:
    weight: 2.0                 # for weighted mode
    soc_min: 0.1                # override BMS floor
```

### Hot Reload

Both `config.yaml` and the Settings UI write to the same file. A file watcher picks up external edits within 500ms. The runtime diffs new vs current and applies per-subsystem:

| Change | Effect |
|--------|--------|
| `site.grid_target_w`, `slew_rate_w`, `min_dispatch_interval_s` | Live-update PI controller |
| `drivers[]` add/remove/change | Spawn/stop/restart driver threads |
| `price`, `weather`, `batteries` | Pick up next cycle |
| `fuse` | Live-update fuse guard limit |
| `homeassistant`, `api.port` | Restart required (MQTT / socket bind) |

## Dispatch Modes

| Mode | What it does |
|------|-------------|
| `idle` | Don't Panic. Both systems run autonomously. |
| `self_consumption` | Target 0W grid. The Answer is always 42W away. |
| `peak_shaving` | Cap grid import at `peak_limit_w`. Charge from any export. |
| `charge` | Force charge. For when electricity is cheap and life is good. |
| `priority` | One battery does the work. Like Zaphod's two heads. |
| `weighted` | Custom split. Not all batteries are created equal. |

## API

```bash
curl http://localhost:8080/api/status           # The Guide
curl http://localhost:8080/api/config           # Full current config (UI source)
curl -X POST http://localhost:8080/api/config \
  -H 'Content-Type: application/json' \
  -d @new-config.json                           # Replace config + hot-apply + save yaml
curl -X POST http://localhost:8080/api/mode \
  -d '{"mode":"self_consumption"}'              # Set mode
curl -X POST http://localhost:8080/api/target \
  -d '{"grid_target_w": 0}'                    # The Question
```

## Testing

```bash
cargo test --release        # Full inline test suite (104+ tests)
```

Tests cover config validation/roundtrip, energy integration, Kalman/health tracking, control dispatch (all 6 modes + fuse guard + slew rate), driver registry diff logic, atomic save + reload, and API config endpoints.

## Sign Convention

| Signal | Positive | Negative |
|--------|----------|----------|
| Grid | importing | exporting |
| PV | — | generating |
| Battery | charging | discharging |
| Load | consuming | — |

## Building

```bash
cargo build --release
# Or: ./scripts/release.sh v0.42.0
```

## Deploy

```bash
./scripts/deploy.sh homelab-rpi
```

## Writing Drivers

See [docs/lua-drivers.md](docs/lua-drivers.md). Drivers are Lua scripts following the Sourceful driver contract.

---

*So long, and thanks for all the watts.* 🐬
