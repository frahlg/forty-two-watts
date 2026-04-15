# telemetry — in-memory DER store, Kalman smoothing, driver health, sample buffer

## What it does

Central sink for DER readings. Drivers call `Update` once per poll; the store applies a scalar Kalman filter per `(driver, der_type)` signal, preserves last-known SoC across gaps, and records a `DriverHealth` entry so the watchdog can flip drivers offline. Readings, health, and a `pending` metric buffer all live in RAM — the control loop drains the buffer once per cycle and forwards it to `state.RecordSamples`. Every W value is site-signed (see `../../../docs/site-convention.md`).

## Key types

| Type | Purpose |
|---|---|
| `Store` | Thread-safe DER + health + buffer. One per site. |
| `DerReading` | Raw + smoothed W, optional SoC, raw JSON, update time. |
| `DerType` | `DerMeter`, `DerPV`, `DerBattery`. |
| `DriverHealth` | Status + consecutive errors + last success time + tick count. |
| `DriverStatus` | `StatusOk`, `StatusDegraded`, `StatusOffline`. |
| `MetricSample` | Buffered `(driver, metric, ts_ms, value)` awaiting flush. |
| `WatchdogTransition` | Emitted when a driver's online state flips. |
| `KalmanFilter1D` | 1-D Kalman used per signal; also for the separate load filter. |

## Public API surface

- `NewStore()` — construct with default Kalman params (process 100 W, meas 50 W) and a slow load filter (20/500).
- `Update(driver, type, rawW, soc, data)` — driver hot path; smooths, stores, and auto-buffers `{type}_w` + `{type}_soc` as samples.
- `Get(driver, type)` / `ReadingsByType(type)` / `ReadingsByDriver(driver)` — read-side accessors used by control + api.
- `IsStale(driver, type, timeout)` — freshness check.
- `DriverHealth(name)` / `DriverHealthMut(name)` / `AllHealth()` — health lookup; `Mut` creates-if-missing.
- `WatchdogScan(timeout)` — called once per control cycle; returns the list of flipped drivers so the caller can kick them back to autonomous mode.
- `EmitMetric(driver, name, value)` — arbitrary scalar into the pending buffer (temperatures, voltages, etc.).
- `FlushSamples()` — drain + clear the pending buffer; the control loop forwards the result to `state.Store.RecordSamples`.
- `UpdateLoad(rawLoad)` — runs the separate slow filter for the computed `load = grid − pv − bat` channel.
- `ParseDerType(s)` / `(DerType).String()` — serialization helpers.
- `NewKalman(process, measurement)` / `(*KalmanFilter1D).Update` — exposed for tests and the load filter.

## How it talks to neighbors

**Leaf for writes, source for reads.** Drivers (`internal/drivers`) call `Update` / `EmitMetric` / `DriverHealthMut`. The control loop (`internal/control`) reads via `Get` + `ReadingsByType` + `DriverHealth` and drains via `FlushSamples`. The HTTP API (`internal/api`) and HA bridge (`internal/ha`) read for presentation. `telemetry` itself imports nothing from other internal packages — it does not know about `state` or SQL at all.

## What to read first

`store.go` — type declarations, `Update`, and the `pending*` buffer mechanics are all in one file. `kalman.go` is ~50 lines and self-contained; read it to understand why smoothed values track raw so responsively.

## What NOT to do

- **Do NOT persist from `Update`.** The DB writer lives in `state` and is called from the control loop via `FlushSamples`. Keeping `Update` allocation-light is a hot-path invariant — drivers call it at their poll rate.
- **Do NOT store smoothed W in the TS DB.** `Update` buffers `rawW` on purpose (store.go:201) — consumers smooth as they like; ground truth stays untouched.
- **Do NOT clear SoC on a missing emit.** `Update` falls back to the previous SoC when the new sample has `soc == nil` (store.go:180–184). Ferroamp ESO and similar publish SoC less frequently than power-flow telemetry; dropping it here produces phantom 0 % dips.
- **Do NOT share `*DriverHealth` pointers across goroutines.** `DriverHealthMut` returns the raw pointer with no lock held after return — only the watchdog and the driver's own loop should write to it.
- **Do NOT forget site convention.** `rawW` / `SmoothedW` are site-signed; the sign flip for raw device protocol values happens in the driver, never here.
