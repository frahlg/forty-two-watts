# selftune — closed-loop battery step-response calibration

## What it does

Drives each battery through a scripted step pattern, fits a first-order
response to each step, and writes the aggregated (gain, τ) into the
battery model as a trusted baseline. Used by the health endpoints to
measure drift over time. One battery at a time, behind a
coordinator-owned override that the control loop respects.

## Math

Step pattern (per battery, `selftune.go:56-73`):

```
stabilize → +1000 W → settle → -1000 W → settle
          → +3000 W → settle → -3000 W → settle → fit → done
```

Sample collection runs only during the four active step_up/step_down
phases (`Step.Collecting`).

First-order fit over samples within one step (`fitStepResponse`,
`selftune.go:129-174`):

```
y_ss    = mean of last 50% of samples
delta   = y_ss - y_initial
gain    = y_ss / u            (clamped to [0.3, 1.5])
tau_s   = ElapsedS at which y crosses y_initial + 0.632*delta
                              (clamped to [0.5, 30])
```

Fit rejected when `|u| < 100 W` or `|delta| < 50 W` — the response was
too small to distinguish from noise.

Aggregate (`selftune.go:302-324`): mean of valid fits across the 4 steps
→ `battery.Model.SetFromStepFit(gain, tau, dtS)` (overwrites ARX + resets
covariance) and `SetBaseline(gain, tau, nowMs)` (records it as the
reference point for future drift detection).

## Inputs / outputs

Input: list of battery names + the current `dtS` + actual-lookup callback
returning `(actualW, soc, ok)`.

Output side effects on each `battery.Model`:

- `A`, `B` overwritten from step fit (high-confidence covariance reset).
- `BaselineGain`, `BaselineTauS`, `LastCalibrated` stored.
- `ModelSnapshot` before/after held in memory for the UI.

## Training cadence + persistence

Triggered only by `POST /api/self_tune/start` (operator-initiated, never
automatic — `api.go:111`). Cancel via `POST /api/self_tune/cancel`
(`api.go:113`); status polled via `GET /api/self_tune/status`
(`api.go:112`).

Sequenced per-battery: when `CurrentIdx` increments past
`len(Batteries)`, `Active = false` and the override lifts
(`selftune.go:327-341`). Typical total runtime ≈ 2 min per battery
(sum of `DurationS()` across the state machine).

Persistence: none directly. The updated `battery.Model` is saved on the
next normal save cycle via `state.Store.SaveBatteryModel`.

## Public API surface

- `NewCoordinator() *Coordinator`
- `(*Coordinator).Start(names, models, dtS) error`
- `(*Coordinator).Cancel()`
- `(*Coordinator).Tick(actualLookup, models, dtS, nowMs)`
- `(*Coordinator).CurrentCommand() (name, commandW, active)`
- `(*Coordinator).IsTuning(name) bool`
- `(*Coordinator).Status() Status`
- Types `Step`, `Sample`, `stepFit`, `ModelSnapshot`, `Status`.
- Step constants `StepStabilize`, `StepUpSmall`, `StepSettleUp`,
  `StepDownSmall`, `StepSettleDown`, `StepUpLarge`, `StepSettleHighUp`,
  `StepDownLarge`, `StepSettleHighDown`, `StepFit`, `StepDone`.

## How it talks to neighbors

- `control` dispatch calls `Coordinator.CurrentCommand()` each cycle; if
  active for a given battery, it overrides the normal dispatch for that
  one battery only (others keep following the MPC plan).
- Writes into `battery.Model.SetFromStepFit` and `SetBaseline`.
- API endpoints in `internal/api/api.go` (`/api/self_tune/*`).
- `actualLookup` callback is wired to `telemetry.Store` reads.

## What NOT to do

- Do not drive two batteries in parallel — the coordinator sequences
  deliberately. Simultaneous steps would couple through the shared
  grid/PV baseline and corrupt the gain fits.
- Do not feed the step samples into the normal RLS path; `SetFromStepFit`
  bypasses the online learner because step data is qualitatively
  different (clean input, unlike typical noisy operation).
- Do not run self-tune below ~20% SoC or above ~80% — saturation curves
  clip the response and the fit collapses to `gain = 1.0`. Operator UI
  should gate this, but don't rely on it here.
- Do not skip the `stabilize` opening step. The ARX fit assumes the
  battery is at rest when `t = 0`.
