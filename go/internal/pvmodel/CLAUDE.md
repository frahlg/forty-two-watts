# pvmodel — self-learning PV digital twin

## What it does

Recursive-least-squares regression over 7 time-of-day features that maps
clear-sky irradiance + cloud cover to AC output. Captures orientation,
shading, and degradation the generic physics prior misses. Runs one RLS
update per 60s sample during daylight; the MPC calls `Predict` to get the
system-specific forecast for any future slot.

## Math

Feature vector (`model.go:75-95`, 7 slots, 1st + 2nd time-of-day harmonic):

```
x = [ 0,                                       ← dead slot (see below)
      clearsky_w,
      clearsky_w * (1 - cloud/100)^1.5,
      clearsky_w * sin(h),   clearsky_w * cos(h),
      clearsky_w * sin(2h),  clearsky_w * cos(2h) ]
      where h = 2*pi*hour_of_day/24
```

Slot 0 is held at 0 on purpose — PV physics pass through the origin
(no sun → no output), so an RLS intercept has no physical basis. Left
free, Beta[0] drifted during daytime training and projected phantom
generation into night slots (issue #133/#134). Kept as a dead slot
(instead of dropping NFeat to 6) to preserve on-disk Beta persistence;
Update() also re-zeros Beta[0] each tick so drifted persisted models
self-heal, and NewService() zeros Beta[0] on load.

RLS update (`model.go:183-217`):

```
K   = P x / (lambda + x^T P x)
beta = beta + K * (actual - beta.x)
P   = (P - K x^T P) / lambda
```

Forgetting factor `lambda = 0.995` (~200-sample effective window).
Cold-start: `beta = [0, 0, rated/1000, 0, 0, 0, 0]` reproduces the physics
prior, so day-one predictions already behave (`model.go:66-70`).

Cold-start blend (`model.go:129-133`):

```
trust = min(samples / 50, 1)
y     = trust * learned + (1 - trust) * prior
prior = rated * (clearsky/1000) * (1-cloud/100)^1.5
```

Output sanity envelope: if `y > 1.05 * rated` → return the physics prior
instead (`model.go:140-143`). A wild RLS coefficient can't escape into the
plan.

Predict gate (`model.go:109-117`):

- `clearSkyW < 50` → return 0. Physics: no sun → no PV. Mirrors the
  Update-side gate; without this the intercept term (`x[0]=1`) projects
  any non-zero `Beta[0]` into every night slot (issue #133).

Input outlier rejection (`model.go:149-180`):

- `clearSkyW < 50` → skip (night, no signal).
- `actualPVW < 0` → skip.
- `actualPVW > 1.2 * rated` → skip (sensor glitch).
- `|yHat| > 2 * rated` before fit → skip (prevents cascading bad samples).
- After 50 samples: reject `|err| > max(10*MAE, 200 W)`.

## Inputs / outputs

Per sample: `(clearSkyW [W/m²], cloudPct [0..100], t, actualPVW [W,
non-negative])`. Converted from telemetry inside `Service.sample`
(PV telemetry is site-sign so `pvW = -SmoothedW`, `service.go:165-171`).

`Predict(t, cloudPct)` → expected AC output in W, non-negative, capped.

## Training cadence + persistence

Sample + update every `SampleInterval = 60s` (`service.go:54`). Skip when
`clearSky < 50 W/m²` (night) or telemetry shows `pvW < 1 W` (driver outage,
`service.go:173-177`).

Persistence: JSON-encoded `Model` saved via
`state.Store.SaveConfig("pvmodel/state", …)` (constant in `service.go:15`).
Loaded on boot via `LoadConfig` (`service.go:60-68`). Persisted every
`PersistEvery = 10` samples and on shutdown.

Reset: `POST /api/pvmodel/reset` → re-seeds with the physics prior.

## Public API surface

Model (`model.go`):

- `NewModel(ratedW) *Model`, `Features(clearSkyW, cloudPct, t) [7]float64`
- `(Model).Predict(clearSkyW, cloudPct, t) float64`
- `(*Model).Update(clearSkyW, cloudPct, t, actualPVW) bool`
- `(Model).Quality() float64`
- Constants `NFeat = 7`, `WarmupSamples = 50`.

Service (`service.go`):

- `NewService(st, tel, clearSkyFn, cloudFn, ratedW) *Service`
- `(*Service).Start(ctx)` / `.Stop()` / `.Reset()`
- `(*Service).Predict(t, cloudPct) float64`, `.PredictNow()`, `.Model() Model`
- Injected func types `ClearSkyFunc`, `CloudFunc` (decouples from
  `forecast` and `sunpos`).

## How it talks to neighbors

- `main.go` wires `ClearSkyFunc` to `sunpos.ClearSkyW(t, lat, lon)` (or
  equivalent) and `CloudFunc` to the forecast cache.
- MPC consumes `Service.Predict` via the `mpc.PVPredictor` func type
  (`mpc/service.go:18`, wired in `main.go`).
- Reads live PV power from `telemetry.Store.ReadingsByType(DerPV)`.
- UI overlays predicted vs actual via `PredictNow`.

## What NOT to do

- Never feed a night-time sample — `sample()` already guards with
  `cs < 50`; keep that check if you refactor.
- Do not drop the `1.2 * rated` input guard or the `1.05 * rated` output
  envelope; an inverter restart transient of 15 kW on a 10 kW plant will
  otherwise poison `beta`.
- Do not persist on every sample — SQLite writes are not free. Keep the
  `PersistEvery` batching.
- Do not change `Beta[2] = rated/1000` on cold start. That coefficient is
  the naive baseline and keeps day-one usable.
