# forty-two-watts

<img src="web/logo.jpg" alt="42W" width="120" align="right">

> Home energy management that actually works. Coordinates solar, batteries, grid, and EV chargers so your house runs on its own power.

**Status: v0.4.0-alpha** — running in production on real hardware, but the API and config format may still change. [Join the Discord](https://discord.gg/z7FxpQnk) to follow development and share feedback.

## What it does

42W is a single Go binary that runs on a Raspberry Pi (or any Linux box) and manages your home energy system in real time:

- **Self-consumption** — batteries discharge to cover household load, charge from PV surplus, grid stays near zero.
- **Smart scheduling** — an MPC planner looks 48 hours ahead using electricity prices + weather forecasts and decides when to charge, discharge, or hold.
- **EV-aware** — when your car is charging, home batteries automatically stop discharging into it (no energy round-tripping through two battery systems).
- **Multi-device** — runs multiple inverters + batteries + meters simultaneously, each with its own Lua driver. Tested with Ferroamp + Sungrow on the same site.

## Supported devices

19 Lua drivers ship today. Each is a single `.lua` file — no compilation, no toolchain, hot-reloadable on the device.

| Category | Manufacturers |
|----------|--------------|
| **Hybrid inverters** | Sungrow, Solis, Huawei, Deye, SMA, Fronius, SolarEdge, Kostal, GoodWe, Growatt, Sofar, Victron |
| **Batteries** | Ferroamp (MQTT + Modbus), Pixii |
| **Meters** | Eastron SDM630, Fronius Smart Meter |
| **EV chargers** | Easee (Cloud API) |

Adding a new device? See [Writing a driver](docs/writing-a-driver.md) or the [Claude Code recipe](docs/writing-a-driver-with-claude-code.md) to generate one from a register map.

## Quick start

### Option A — one-shot Docker installer (recommended for Raspberry Pi)

Works on a freshly-installed Raspberry Pi OS (arm64) and most Debian/Ubuntu
machines. Installs Docker + compose, pulls the multi-arch image from GHCR,
and starts the container. Idempotent — re-run to upgrade.

```bash
curl -fsSL https://raw.githubusercontent.com/frahlg/forty-two-watts/master/scripts/install.sh | bash
```

Then open `http://<your-pi>:8080/setup` to run the first-time wizard.

### Option B — build from source

**Prerequisites:** Go 1.25+, a Raspberry Pi (or any `linux/arm64` machine), and at least one supported inverter/battery on your LAN.

```bash
git clone https://github.com/frahlg/forty-two-watts
cd forty-two-watts

# Try it locally with simulators
make dev          # starts sim-ferroamp + sim-sungrow + the app
open http://localhost:8080

# Build for your Pi
make build-arm64
scp bin/forty-two-watts-linux-arm64 pi@<your-pi>:~/42w/
scp -r drivers/ web/ config.example.yaml pi@<your-pi>:~/42w/
```

Copy `config.example.yaml` to `config.yaml` and fill in your device IPs. The web UI at `:8080` lets you configure everything else.

## How it works

Three layers in one binary:

1. **Control loop** (every 5 s) — PI controller + slew rate + fuse guard + SoC clamps. Reads the site meter, computes battery targets, dispatches to drivers. This is the part that keeps the lights on.

2. **MPC planner** (every 15 min) — dynamic programming over a discretized SoC grid. Three strategies:
   - **Self-consumption** — never import to charge, never export from battery. Just cover your own load.
   - **Cheap charging** — charge from grid when prices are low, otherwise self-consume.
   - **Arbitrage** — full price optimization: charge cheap, discharge expensive, export when profitable.

3. **Digital twins** (every 60 s) — online machine learning models that observe your system and learn:
   - **PV twin** — learns your roof's orientation, shading, and soiling from clear-sky physics + cloud cover.
   - **Load twin** — learns your household's hourly consumption pattern + heating coefficient.
   - **Price twin** — fills in electricity prices beyond the day-ahead publication window.

## Web UI

The dashboard shows real-time power flow, battery SoC, energy totals, the planner's 48-hour schedule, and per-driver health. Everything is configurable from Settings — devices, strategies, EV charger credentials, Home Assistant integration.

## Home Assistant

Built-in MQTT autodiscovery. Enable it in Settings → Home Assistant, point it at your Mosquitto broker, and sensors + controls appear in HA automatically.

## EV charging

Configure your Easee charger in Settings → EV Charger (email + password). The driver polls the Easee Cloud API every 5 seconds. When the car charges, the dispatch clamp prevents home batteries from discharging into the car.

OCPP 1.6J Central System is also built in (port 8887) for chargers that support direct WebSocket connections.

## Architecture

```
config.yaml
    ↓
┌─────────────────────────────────────────┐
│  Lua drivers (one goroutine per device) │
│  ferroamp.lua · sungrow.lua · easee.lua │
└────────────┬────────────────────────────┘
             ↓ host.emit("meter"|"pv"|"battery"|"ev")
┌─────────────────────────────────────────┐
│  Telemetry store (Kalman-smoothed)      │
└────────────┬────────────────────────────┘
             ↓
┌─────────────────────────────────────────┐
│  Control loop (PI + dispatch + clamps)  │
│  ← MPC planner (DP, 48h horizon)       │
│  ← Digital twins (PV, load, price)     │
└────────────┬────────────────────────────┘
             ↓ driver_command(action, power_w)
┌─────────────────────────────────────────┐
│  Lua drivers → Modbus / MQTT / HTTP     │
└─────────────────────────────────────────┘
```

No cloud dependency for core operation. Everything runs locally on the Pi. Weather forecasts (met.no) and electricity prices (Elpriset Just Nu / ENTSO-E) are fetched periodically but the system degrades gracefully without them.

## Development

```bash
make test         # full Go test suite
make e2e          # end-to-end with simulators
make dev          # live dev with hot-reload
make build-arm64  # cross-compile for Pi
```

Driver development:
- [Writing a Lua driver](docs/writing-a-driver.md) — full walkthrough
- [Using Claude Code](docs/writing-a-driver-with-claude-code.md) — AI-assisted driver generation
- [Testing drivers live](docs/testing-drivers-live.md) — sim + Pi workflow

## Community

- **Discord**: [discord.gg/z7FxpQnk](https://discord.gg/z7FxpQnk) — discuss development, share your setup, report issues
- **Issues**: [github.com/frahlg/forty-two-watts/issues](https://github.com/frahlg/forty-two-watts/issues)

## Roadmap

- [ ] New-user onboarding flow (guided setup wizard)
- [ ] Network scanning for auto-discovery of devices
- [ ] Driver marketplace with version history and compatibility info
- [ ] EV smart charging (PV-surplus preferred, departure-time aware)
- [ ] Multi-charger load balancing with fuse coordination
- [ ] OCPP 2.0.1 support

## License

MIT
