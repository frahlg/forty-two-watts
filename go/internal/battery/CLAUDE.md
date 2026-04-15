# battery — per-battery ARX(1) online identification

## What it does

RLS identification of a first-order battery model `y(t+1) = a·y(t) + b·u(t)`
fed one (command, actual) pair per control cycle. Tracks a per-SoC
saturation envelope from observed peaks. Drives the cascade controller: at
confidence > 0.5 the outer PI's command is inverted through the model to
produce the setpoint; below 0.5 the PI drives the plant directly.

## Math

ARX(1) prediction + residual (`model.go:234-238`):

```
phi  = [y_prev, u_prev]
pred = a*y_prev + b*u_prev
err  = y_actual - pred
```

RLS with forgetting `lambda = 0.99` (~100-sample window, `model.go:20`):

```
K       = P*phi / (lambda + phi^T*P*phi)
[a, b] += K * err                     (clamped: a in [0.1, 0.99], b in [-1.5, 1.5])
P       = (P - K * phi^T * P) / lambda
```

Derived quantities (`model.go:98-132`):

```
steady_state_gain = b / (1 - a)           (display-clamped to [0.3, 1.5])
time_constant_s   = -dt / ln(a)           (clamped to [0.05, 60] s)
confidence        = min(n/200, 1) * (1 - varEMA/10000)
```

Outlier rejection: after 20 samples skip when `|err| > 5*sigma`
(`model.go:240-247`, `OutlierSigma = 5`).

Saturation curve: per-SoC bucket (5% wide) track max observed |actual|;
slow decay `0.9999` per update so old peaks fade
(`model.go:322-335`). New buckets only seed when `value >= MinSatSeedW =
1000 W` (`model.go:338-350`) — guards against a self-reinforcing clamp loop
inherited from the Rust port.

Inverse (cascade use): `inverse(target) = target / gain` when
`|gain| ∈ [0.3, 2.0]`, else pass-through (`model.go:181-188`).

Health baseline: `SetBaseline(gain, tau)` stores a self-tune snapshot.
`HealthScore = 1 - 2 * |gain - baseline| / |baseline|` (clamped
[0, 1], `model.go:146-155`). `HealthDriftPerDay` is linear regression slope
over `GainHistory` in gain/day (`model.go:159-177`).

## Inputs / outputs

Per cycle: `Update(command, actual, soc, dtS, nowMs)` where `command` and
`actual` are site-sign W (+ charge, − discharge), `soc` is fraction in
[0, 1], `dtS` is the control period. Returns `true` when RLS actually
moved (sample was informative).

Reads:

- `SteadyStateGain()`, `TimeConstantS(dt)`, `Confidence()`, `HealthScore()`,
  `HealthDriftPerDay()`.
- `Inverse(target)`, `ClampToSaturation(target, soc) (clamped, wasClamped)`.

## Training cadence + persistence

Cadence: one `Update` per control cycle (typically 5 s). Low-signal gate
skips `|u| < 100 W` or, after warm-up, `|Δy| < 20 W` (`model.go:222-232`).

Persistence: `state.Store.SaveBatteryModel(device_id, json)` where
`device_id` is the resolved hardware identifier — NOT `driver_name`
(store.go:261-270). Legacy `driver_name`-keyed rows are upgraded by
`state.Store.MigrateBatteryModelKeys()` on boot (`state/devices.go:111`).
Save cadence: every ~60 s from the control loop (models are serialized via
`json.Marshal` and passed into `SaveBatteryModel`, see `api.go:533`).

`LoadAllBatteryModels` returns a map keyed by current `driver_name`
(joined through the `devices` table) so renames don't orphan trained
state.

## Public API surface

- `New(name) *Model`
- `(*Model).Update(cmd, actual, soc, dtS, nowMs) bool`
- `(*Model).Inverse(target) float64`
- `(*Model).ClampToSaturation(target, soc) (float64, bool)`
- `(*Model).SteadyStateGain() / .SteadyStateGainRaw() / .TimeConstantS(dt)`
- `(*Model).Confidence() / .HealthScore() / .HealthDriftPerDay()`
- `(*Model).SetBaseline(gain, tau, nowMs)` — written by `selftune`.
- `(*Model).SetFromStepFit(gain, tauS, dtS)` — overwrites ARX with a
  clean step fit and resets covariance to indicate high confidence.
- Types `Model`, `SoCPoint`, `GainPoint`; constants
  `DefaultForgetting`, `InitialCov`, `SoCBucket`, `SatDecay`,
  `MinCommandForRLS`, `MinDeltaForRLS`, `OutlierSigma`, `GainHistoryLen`,
  `MinSatSeedW`.

## How it talks to neighbors

- `control` cascade calls `Inverse` + `ClampToSaturation` each cycle, then
  feeds `(command, actual)` back via `Update`.
- `selftune` writes cleanly-fit baselines via `SetFromStepFit` +
  `SetBaseline`, and the health endpoints read them back.
- State store: `SaveBatteryModel` + `LoadAllBatteryModels` (`state/store.go`).
- Operator-facing docs: `docs/battery-models.md`.

## What NOT to do

- Do not store under `driver_name` — always resolve to `device_id` first
  (see `batteryModelKey` in `state/store.go`). Renames in config will
  otherwise lose weeks of training.
- Do not lower `OutlierSigma` below ~3; brief transients during PV clipping
  otherwise bias `a` downward.
- Do not seed new saturation buckets below `MinSatSeedW = 1000 W`. A small
  first observation at a new SoC becomes a clamp ceiling the battery can
  never exceed.
- Do not run RLS when `|u| < MinCommandForRLS = 100 W`. At that magnitude
  `b` is dominated by noise.
