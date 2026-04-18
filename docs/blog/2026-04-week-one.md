# Week one of forty-two-watts

*Published 2026-04-18 · by Fredrik Ahlgren*

Seven days ago this repo didn't exist. Today it runs on a Raspberry Pi
in my garage, co-optimizing a home battery, PV array, and EV charger
from a 48-hour price forecast, with a time-travel UI that lets me
scrub through every decision the planner has ever made. Two
contributors, 276 commits, 25 tagged releases from `v0.4.0` to
`v0.18.0`, and one unexpected Rust→Go rewrite in the middle. Here's
how that happened.

---

## Sunday, April 12 — the birth

First commit lands at `ad603f0`: *"Initial scaffold: config, telemetry
store, Lua drivers."* Rust + a WASM driver runtime + a `redb` key-value
store + a web UI with a live power chart. By end-of-day there's a PI
controller, a Kalman filter, a Dockerfile, an anti-oscillation scheme
(gain, slew, holdoff), and the first Raspberry Pi deployment.

Twenty-six commits in one day. The core thesis is already in the code:
one thermodynamic boundary, one control loop, sub-second dispatch.

I run it on my own house that same evening. It doesn't break
anything. That's the entire bar.

## Monday, April 13 — it learns

`v0.2.0`: hot-reload, dynamic drivers, Settings UI, 104-test suite.
Then `v0.3.0` a few hours later: per-battery online learning, cascade
control, self-tune, hardware health. Tiered history retention — 30
days live at 1 s, 12 months at 15 min, forever at daily.

I sit at the laptop and watch the chart. The battery is learning the
round-trip efficiency of its own inverter by commanding itself and
observing the result. I did not build that; I just wrote the
bookkeeping and told it to converge. The emergence felt disturbing
in a nice way.

## Tuesday, April 14 — Rust → Go, in a day

xorath (Erik Arenhill) is shipping in parallel — semantic-release +
GitHub Actions for versioned Docker images, amd64 and arm64. He slots
conventional-commit rules into `CLAUDE.md` so the changelog bot gets
the right MAJOR/MINOR/PATCH bumps automatically. I make him admin;
the `#dev-core` Discord channel is three hours old.

Meanwhile the `MIGRATION_PLAN.md` that's been hanging over the
project since Saturday turns into execution: **phases 0 through 13 in
one day.** Scaffold, WASM driver runtime, Ferroamp-in-Rust as the
reference implementation, Sungrow over Modbus, control loop, battery
model, self-tune, HTTP API, HA bridge, spot prices, weather forecast,
PV estimation, MPC planner.

Per-slot export pricing lands the same day — so arbitrage can finally
see the *spread* between morning peak and midday trough instead of a
flat horizon average.

## Wednesday, April 15 — the driver ecosystem opens up

`DerEV` telemetry type, EV clamp in the dispatch, Pixii +
SolarEdge drivers, Ferroamp-Modbus as an alternative transport to
the MQTT driver. A curated Lua driver catalog. A driver-authoring
guide written as a runbook for Claude Code. Docs for testing drivers
against sims or on a real Pi.

Legend wrap, nice-tick y-axis, cleaner chart labels. The Rust UI
still works; somebody has to keep it pretty while the Go port
matures underneath.

## Thursday, April 16 — clean-room, Zap, Easee, and the first charge

The big cut: `2fc4a0e Remove all Rust/WASM code — pure Go + Lua from
here on.` The WASM era ends. Every driver from here on runs in
embedded Lua 5.1 via `gopher-lua`. One static binary, no CGo.

I try to get local OCPP running on my Easee charger. The firmware
refuses the config — I follow the docs, nothing lights up. I give
up and write a clean-room REST client against the Easee cloud API
instead. The principle I wrote into the channel that morning is the
principle I hold all week:

> "noga att alltid köra 'clean room' när man tittar på andra projekt."

By 09:01 the Easee Lua driver is live. 300 LOC of Lua. Sandboxed —
the driver can only reach the host capabilities (HTTP + MQTT) its
config grants it. A bug in the Easee driver cannot touch the Modbus
bus.

That afternoon Erik and I agree the shape of the thing: an SD-card
image people can flash, a first-boot wizard, and only drivers as the
per-device extension point. Everything dangerous lives behind a
narrow host API. Damo (Damon) joins the channel and takes a
UI-refactor PR. Leitet (Johan) signs up to test on his FoxEss setup.

Erik ships dynamic per-driver config fields — YAML values become web-
form inputs automatically. I ship a `/setup` wizard for fresh
installs. Scanner + driver catalog from my earlier Hugin project
gets ported over. Codex flags three P1s on the Zap + Easee work; we
fix them same-day (#74, #73).

That evening: **the first successful EV charge.** Real charger, real
car, 0 W drawn from the home battery, the session shows up in the UI
with the right state labels and correct session-Wh. I write
"laddning fungerar!" in the channel at 18:48. Erik replies
"Toppen". Leitet does not reply because he's on dinner duty.

## Friday, April 17 — the twin grows up

The PV digital twin has been running for four days now and it's
getting opinionated. We teach it to hot-reload rated kW + lat/lon
without a service restart, anchor near-future predictions to live
telemetry so a cloud patch over the house doesn't linger in the
forecast, and stand up two new weather providers — **Open-Meteo** for
free-tier public access, **Forecast.Solar** for multi-array setups
where a south and a west array need their own sky models.

A twin-card reset button (because RLS occasionally drifts and an
operator needs an escape hatch). Multi-array PV cold-start via the
Settings UI. Fuse-capacity clamp on the MPC's power ask (#81) —
Erik spotted the planner asking for 41 kW on a 16 A fuse.

The stability fix of the week: `#95 — driver restart closes MQTT /
Modbus caps + records health on Add`. Restarting the Ferroamp
driver used to silently leave a dangling MQTT client ID that the
broker disconnected on reconnect. Gone.

I drop the wall-clock version back from `v0.5.x` to the `v0.4.x`
track briefly to set expectation: this thing is a week old, stop
treating the version number like maturity. Erik saw me do it and
laughed — he'd been about to ask.

Late Friday, Erik pings me with the screenshot that set up the whole
weekend. His planner charged the battery from the grid at 21:15.
Price was peaking. Plan said discharge. The EMS did the opposite.

> "hmm jag funderar på om den inte löser DST, utan kör på UTC tider"

He was right. But we didn't know how right until Saturday morning.

## Saturday, April 18 — the planner overhaul

I open the laptop at 07:04 CEST. The core observation from
Erik's debugging: every hour-of-week bucket the planner indexes is
a local-time bucket, but the day-ahead prices are timestamped in
UTC. Twice a year the bucket drifts. Silently. For months.

The morning lands as **five PRs in six minutes** — `#97..#101`:

- **#97 DST / timezone audit.** Every predictor that indexed
  hour-of-week or month buckets now forces UTC at the leaf.
- **#98 `/api/mpc/diagnose`.** A per-slot audit endpoint: price,
  spot, confidence, PV, load joined with the DP's battery, grid,
  SoC, cost, reason. First real observability.
- **#99 PowerLimits + Loadpoint skeleton.** Per-slot grid caps on
  the MPC. A new `loadpoint` package modeling an EV charger as a
  first-class entity the planner can reason about.
- **#100 EV in the DP.** State: `(t, batt_soc, ev_soc)`. Action:
  `(batt_action, ev_action)`. Bilinear interpolation across both
  SoC dimensions. Discrete EV action set so the 1-phase ↔ 3-phase
  minimum-current gap gets handled natively without MILP.
- **#101 EV dispatch wiring.** Every 5 s the control loop computes
  `remaining_wh / remaining_s → W`, snaps to the charger's allowed
  steps, sends `ev_set_current` to the existing `easee_cloud.lua`.

Codex flags eight more issues within minutes. We bundle them into
**#103** (missing 0 W command when a slot has no EV budget,
`plugin_soc_pct` YAML no-op, hard-coded 15-min-slot assumption,
`lastLoadpointID` race, infeasible-state fallback picking full
discharge, model-state invalidation on the UTC switch, config
hot-reload missing loadpoints, session-anchor preservation). Ship
in one hotfix.

The chain goes from master to the Pi at roughly 10:00 CEST. Real
charger, real battery, real kilowatts.

Erik's next question lands within the hour: *can we see what the
planner was thinking retroactively?* The in-memory `/api/mpc/diagnose`
shows only the CURRENT plan. The one that caused his 21:00 grid-
charge is already gone.

So we build **#104** — `planner_diagnostics` SQLite table, every
replan persists the full Diagnostic (`~18 KB × 100/day`), 30-day hot
retention with daily Parquet rolloff to cold storage, two new
endpoints: `/history` for the timeline list and `/at?ts=<ms>` with
closest-earlier semantics. Ask for 02:07, get the 02:00 snapshot —
the plan that was actually driving the EMS at that moment.

Then **#105** — a new Diagnose tab in the web UI. Hash-routed
(`#diagnose/<ts>`) so snapshots are deep-linkable; paste a link in
a bug report and the reviewer lands on the exact replan.
Color-coded timeline, stacked canvas chart, per-slot table showing
the DP's reason string for every slot.

Codex follows up with four more issues on #104/#105 (a replan-race
on the persistence hook, a CSS rule I forgot so the Live view
ghosted behind Diagnose). Ship as **#106**. Final deploy: 10:20
CEST. `v0.18.0-2-g9296c93` on the Pi, seven diagnostic snapshots
already in SQLite.

Erik's still asleep at this point. He'll wake up to a Pi that no
longer charges at 21:00 on expensive price signals, an EV charger
the planner now actually controls, and a browser tab he can use to
reconstruct any decision the planner has ever made.

That's one week.

---

## What we have, today

### Core control

- 5-second dispatch loop with site-signed grid flow, PI + cascade
  across multiple batteries, slew + fuse + SoC clamps, fuse guard
  that trips everything to zero before a breaker pops.
- Driver watchdog. Any driver whose telemetry goes stale flips
  offline and reverts to its autonomous self-consumption default.
- Per-battery ARX(1) self-learning + RLS + self-tune step-response
  coordinator. Models are keyed by hardware-stable `device_id`
  (`make:serial` > `mac:<arp>` > `ep:<endpoint>`) so renaming a
  driver in YAML never orphans trained state.

### Drivers (Lua, sandboxed, hot-editable)

- **Ferroamp** (MQTT) + **Ferroamp Modbus** (alt transport)
- **Sungrow SH** (Modbus TCP)
- **Pixii** (Modbus)
- **SolarEdge** (with and without meter)
- **Sourceful Zap** — our own meter + PV-aggregator driver
- **Easee Cloud** — EV charger via REST, clean-room, with a
  charger-list "Connect" flow that confirms credentials work
  before you commit the config
- **Solis** + **Deye** — aligned with the Zap reference patterns

Every driver ships with a `DRIVER = {…}` metadata block. The UI
reads them for a catalog + verification-status badge (production /
beta / experimental, with `verified_by` + `verified_at` fields for
production claims). Write a new one in an afternoon by copying
`ferroamp.lua` and filling in the blanks.

### Digital twins

- **PV twin** — 7-feature RLS (clear-sky + cloud response + 1st+2nd
  time-of-day harmonics), 50-sample warmup blended with a physics
  prior, output capped at 105 % of rated. Anchors near-future
  predictions to live telemetry so the forecast doesn't linger
  after a cloud passes. Hot-reload of rated-kW + lat/lon.
- **Load twin** — 168 hour-of-week EMA buckets + operator-set
  heating coefficient. Blends with a baked Swedish-home prior for
  cold-boot sanity.
- **Price forecaster** — 168-bucket EMA + monthly multiplier, baked
  Nordic prior (morning peak 07-09, midday trough, evening peak
  17-20, winter-heavier), Bayesian-blended so sparse history
  doesn't wipe the shape. Fills in future slots beyond the day-
  ahead publication window so overnight-arbitrage planning works
  at 22:00 instead of stalling until 13:00 the next day.
- **Multi-array PV support** — configure tilt + azimuth + kWp per
  array; Forecast.Solar gets a proper orientation-aware prediction
  instead of pretending your west array is south-facing.

All twin-learned state is persisted to SQLite, versioned against
coordinate-space changes (e.g. `pvmodel/state_utc` after the UTC
switch invalidated local-zone-indexed β coefficients).

### Planner (MPC)

Pure-Go dynamic programming over a discretized state space. Backward
Bellman recursion, <10 ms for battery-only, ~1 s with an EV
loadpoint plugged in.

**State**: `(t, battery_soc, ev_soc)`.
**Action**: `(battery_action, ev_action)` — discrete on both sides.
**Horizon**: 48 hours rolling, 15-min slots by default (Nordic
day-ahead went quarterly in 2025).

- **Three strategies**: self-consumption / cheap-charge / arbitrage.
  Per-slot export pricing — arbitrage sees the spread.
- **Confidence blending** — forecasted slots pulled toward the
  horizon mean so the DP doesn't over-commit to an ML guess.
- **Per-slot reason strings** — every decision self-documents.
- **Deadline-aware EV charging** — target + target-time maps to a
  horizon slot; missing the target costs `missed_kwh × mean_price
  × 4`, so the DP commits when feasible and gracefully degrades
  (max-delivered-energy) when infeasible.
- **PowerLimits per slot** — import/export caps for DSO curtailment
  signals, dynamic-capacity tariff windows, service-entrance fuse
  limits.
- **Infeasibility-safe** — when mode + limits eliminate every
  action, forward-sim falls back to the closest-to-idle action
  instead of accidentally full-discharging.
- **Reactive replan** — 15-min half-life leaky integral on
  (actual − predicted) PV and load. When the gap crosses 500 / 400
  Wh, trigger an off-schedule replan. Divergence events surface in
  the UI as orange timeline rows.

### Energy-allocation dispatch

The planner no longer publishes a power target. It publishes a **Wh
budget per slot**. The EMS converts to instantaneous power in real
time via `remaining_wh × 3600 / remaining_s`. Grid is residual; no
PI chases a grid target. Works uniformly for battery and EV.

### Observability — the Diagnose tab

- **`/api/mpc/plan`** — current plan snapshot.
- **`/api/mpc/diagnose`** — in-memory per-slot audit of the
  currently-active plan.
- **`/api/mpc/diagnose/history`** — persisted summaries for the
  timeline UI.
- **`/api/mpc/diagnose/at?ts=<ms>`** — full diagnostic active at any
  past instant. Falls through to Parquet when the query extends
  past the 30-day SQLite retention.
- Long-format time-series DB for every `host.emit_metric` call,
  rolling off to daily Parquet under `<dataDir>/cold/YYYY/MM/DD.parquet`.

The Diagnose UI tab renders all of this with a color-coded timeline,
a stacked canvas chart (price → power → SoC), and a per-slot
table. Hash-routed URLs so you can send someone a link to a
specific past replan.

### UI

- **Live tab** — live power chart, 5-card summary, today's kWh
  totals, strategy picker, inline driver cards with per-driver
  battery-model readout.
- **Diagnose tab** — described above. New this Saturday.
- **Settings modal** — full YAML-backed config editor with
  dynamic driver-specific fields (every new Lua driver gets a
  form without Go code touching the UI).
- **First-boot wizard** (`setup.html`) — scanner, driver catalog,
  device auto-detection, so a fresh Pi never hits raw YAML.

### Home Assistant

MQTT autodiscovery bridge. Every driver emits entities; HA sees
them natively.

### CI / release

- semantic-release with conventional-commit rules wired to GitHub
  Actions.
- Test-gated releases (#81) — tests must pass before a release gets
  cut.
- Versioned Docker images (`amd64` + `arm64`) published to GHCR.
- One-shot Docker installer for Raspberry Pi OS (#82) — script
  pulls the latest image, writes a compose file, registers a
  systemd service. Flash Raspbian, paste one line, you're running.

### Deployment

- Single static Go binary. No CGo.
- Docker Compose as the canonical deploy path.
- Raspberry Pi 4 / 5 as reference hardware.
- `homelab-rpi` (mine) runs v0.18.0 and has been live continuously
  since Sunday.

---

## The meta-observation

Two humans, several agents, one week. The things that broke were
exactly the things you'd predict two humans working in parallel with
"100 % Claude" would break: force-pushes over each other's PRs, a
commit that rebased someone else's in-progress work, a merge conflict
in `main.go` that took five minutes to resolve because three branches
all rewired the same Deps struct.

The things that worked were also predictable: integration rate is
bounded by human attention, not typing speed. Erik and I spent more
time reading each other's code than writing our own. That's the right
ratio for something that controls a 16 A fuse on a real house.

What I don't know yet — and what next week is for — is what happens
when ten operators are running this at once. Erik's bug at 21:00
Friday was one signal from one site. The planner wouldn't have
caught the DST edge without him running it in production and getting
annoyed enough to screenshot it.

Software is dead. Building software is not dead. Running software
where physics can punish you turns out to be the part nobody can
automate away.

---

*If you're running your own battery + PV + EV on a Pi and want to
kick the tires, the latest image is at `ghcr.io/frahlg/forty-two-watts`
and the Raspberry-Pi installer is [here](../operations.md). Bugs,
feedback, or a zone we haven't tested against are welcome in
[GitHub issues](https://github.com/frahlg/forty-two-watts/issues).*
