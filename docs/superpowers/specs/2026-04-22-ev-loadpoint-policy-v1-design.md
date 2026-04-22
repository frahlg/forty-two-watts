# EV loadpoint charging-policy v1

**Date:** 2026-04-22
**Status:** Design — pending implementation plan
**Scope:** sub-project 1 of 4 (policy primitives + manual start flow). Schedule windows (sub-project 2), price-effective auto-decisions (sub-project 3), and timeline UI (sub-project 4) are separate specs.

## Motivation

Today's loadpoint dispatch (`go/cmd/forty-two-watts/main.go:1083-1138`) unconditionally forwards the MPC's per-slot EV energy budget to the Easee. The plan is the only policy surface. Concrete observed failure modes:

- Without a `target_soc_pct` the DP allocates zero energy to the loadpoint, dispatch sends `dynamicChargerCurrent = 0 A`, and Easee auto-pauses any session the user tries to start. The user cannot charge even with massive PV surplus.
- When a target is set and PV is weaker than forecast, the DP still honours the plan's absolute watts and the dispatch imports from the grid — violating the user's "PV surplus only" intent.
- There is no way to express per-session intent ("charge now from whatever is cheap" vs "charge now from PV only" vs "let the battery help") — the entire system runs on one global planner mode.

The user's mental model is the opposite: they press Start, pick how to charge (surplus / grid / battery), the system honours that until the car is full or they unplug. This spec describes that system.

## Goals

- Pressing Start/Resume from the EV popup is always sufficient to start a session with a clear 8-hour window.
- The user picks, per session, whether grid import and battery support are allowed. "Surplus only" is the derived default.
- Dispatch never imports from the grid when the active session's policy forbids it, with a small tolerance for transients to avoid pause-flapping.
- When surplus is too weak to honour an active surplus-only session for a sustained period, the user is notified.
- When the car reports it is full (or is unplugged) mid-session, the system stops trying to charge and replans.

## Non-goals

- Time-of-day schedule windows (sub-project 2).
- Price-driven auto-decisions — "charge when spot is cheap", "don't divert surplus when export price is high" (sub-project 3).
- A timeline / schedule-editor UI (sub-project 4).
- Multi-loadpoint aggregation of `allow_battery_support` (deferred; v1 targets a single-loadpoint home).
- V2G or bidirectional EV charging (not on roadmap).

## Design

### Model — the target carries the policy

Each EV session is represented by a **target**. A target today is `{soc_pct, target_time}`. We extend it to carry the session's policy snapshot:

```go
type Target struct {
    SoCPct       float64
    TargetTime   time.Time
    Origin       string         // "manual" (v1) | "schedule" (sub-project 2)
    Policy       TargetPolicy
}

type TargetPolicy struct {
    AllowGrid            bool   // false → dispatch clamps cmdW to live surplus
    AllowBatterySupport  bool   // true → BatteryCoversEV flipped while target active
}
```

Derived: `surplus_only = !AllowGrid && !AllowBatterySupport`. The UI surfaces `surplus_only` as a read-only indicator, not as a stored field.

### Smart Start button

The EV popup's existing Start / Resume buttons are wrapped. Clicking either:

1. Sends the driver command (`ev_start` or `ev_resume`) as today.
2. POSTs `/api/loadpoints/{id}/target` with:
   - `soc_pct: 100`
   - `target_time_ms: now + ChargeDurationH × 3600_000` (default 8 h, per-loadpoint override)
   - `origin: "manual"`
   - `policy: { allow_grid, allow_battery_support }` from the checkboxes shown above the button

Pressing Start again mid-session re-arms the target from `now`. The policy is re-read from the current checkbox state.

### Session checkboxes on the EV popup

Three controls rendered above Start / Resume:

- `[ ] Allow grid import` — session-level `policy.allow_grid`.
- `[ ] Allow battery support` — session-level `policy.allow_battery_support`.
- `[ ] Surplus only` — read-only, shown checked when both above are unchecked.

Checkbox values persist per-loadpoint as `last_policy` in `state.db`, so each new session seeds from the last one. Initial default for a fresh install: both unchecked (= surplus only).

### Dispatch loop changes (`go/cmd/forty-two-watts/main.go:1083-1138`)

Per tick, per loadpoint, after resolving the existing `wantW` from the MPC plan:

1. Read the active target. If none, proceed as today (sends `cmdW=0` when no budget — Easee paused).
2. If `target.Policy.AllowBatterySupport != currentBatteryCoversEV`, flip `control.State.BatteryCoversEV` to match. When the target is cleared, restore to `false`.
3. If `!target.Policy.AllowGrid`, compute `surplusW = max(0, -gridR.SmoothedW + evR.SmoothedW)` and pick `wantW` from one of three branches:

   - **Grid-meter stale** (site-meter watchdog fired): force `wantW = 0`. No tolerance without a meter.
   - **Surplus comfortable** (`surplusW ≥ MinChargeW`): `wantW = min(wantW, surplusW)`, reset `surplusBelowMinSinceMs = 0`.
   - **Surplus low** (`surplusW < MinChargeW`):
     - If the gap is larger than the watt cap (`MinChargeW − surplusW > SurplusHysteresisW`): `wantW = 0` immediately, no grace. Reset `surplusBelowMinSinceMs = 0`.
     - Else the gap is within the cap: hold at min-step for a grace window. If `surplusBelowMinSinceMs == 0`, set it to `now`. If `now − surplusBelowMinSinceMs ≥ SurplusHysteresisS`, grace expired → `wantW = 0`. Otherwise `wantW = MinChargeW` (deliberate small import, bounded by `SurplusHysteresisW`).
   - Then `cmdW = control.FloorSnapChargeW(wantW, MinChargeW, MaxChargeW, AllowedStepsW)` — new variant that snaps down to the largest step ≤ want.
4. If `target.Policy.AllowGrid`:
   - `cmdW = control.SnapChargeW(wantW, MinChargeW, MaxChargeW, AllowedStepsW)` — existing nearest-snap.
5. **Starvation detector:** maintain `starvedSinceMs`. When `target` active AND `!AllowGrid` AND `cmdW == 0` continuously for `SurplusStarvationS`, emit an `ev_surplus_starved` event (notifications subsystem). Respect `cooldown_s` between emissions. Reset `starvedSinceMs` whenever `cmdW > 0`.
6. **Auto-clear target:** observe transitions:
   - `prevOpMode == 3` AND `curOpMode == 4` (charging → completed): `lpMgr.SetTarget(id, 0, zeroTime, "")`; `go mpcSvc.Replan(ctx)`.
   - `plugged_in` transition `true → false`: same.
   - Surplus-origin pauses (`prevOpMode == 3`, `curOpMode == 2`, our `lastCmdW == 0`) never clear the target.
7. **Expired target:** if `target.TargetTime != zero && target.TargetTime < now`, clear target, no replan needed (DP already finished allocating past the deadline).
8. **EV driver offline:** skip `ev_set_current` send this tick; keep all tracked state. The driver's own `DefaultMode` has already run under the watchdog.

Per-loadpoint transient state (`prevOpMode`, `lastCmdW`, `surplusBelowMinSinceMs`, `starvedSinceMs`, `starvedLastEmitMs`) lives in a local map owned by the dispatch goroutine — not exported.

### Config surface — loadpoint prefs

Persisted per-loadpoint in `state.db` via the existing settings pattern:

| Key | Default | Meaning |
|---|---|---|
| `loadpoint.<id>.charge_duration_h` | 8 | Target window length when user presses Start. |
| `loadpoint.<id>.surplus_hysteresis_w` | 500 | Max deliberate grid import (W) during the grace window. Surplus gaps larger than this skip the grace window and pause immediately. |
| `loadpoint.<id>.surplus_hysteresis_s` | 300 | Grace duration (s) for small surplus gaps before pausing. |
| `loadpoint.<id>.surplus_starvation_s` | 1800 | Continuous-cmd=0 duration before notifying. |
| `loadpoint.<id>.last_policy` | `{false, false}` | Seeds the EV popup's checkboxes for the next session. |

No changes to `config.yaml`.

### API surface

Handlers land in a **new file** `go/internal/api/api_loadpoint_policy.go`, following the existing split pattern (compare `api_selfupdate.go`). `api.go` only gains three lines in `routes()` registering the new paths; all handler bodies, request/response structs, and validation live in the new file.

- `POST /api/loadpoints/{id}/target` — existing handler moves to `api_loadpoint_policy.go` and gains optional `origin` (string, default `"manual"`) and `policy` (object `{allow_grid, allow_battery_support}`, default both `false`) in the JSON body. Backward-compatible: callers that omit these get v0 behaviour.
- `POST /api/loadpoints/{id}/settings` — new. Body `{charge_duration_h?, surplus_hysteresis_w?, surplus_hysteresis_s?, surplus_starvation_s?}`. Any subset; values validated (positive, sensible ranges).
- `GET /api/loadpoints` — existing handler moves to the new file; response extended with `target.origin`, `target.policy`, `last_policy`, and the four settings fields.
- `POST /api/loadpoints/{id}/soc` — the existing SoC-anchor handler lives alongside the other loadpoint endpoints by topic; moves to the new file too so the package split is by feature, not by age.

Tests for the new handlers live in `go/internal/api/api_loadpoint_policy_test.go`.

`BatteryCoversEV` is no longer operator-controllable from the UI in v1 — it's set by an active target's policy. If a global "force battery to always cover EVs" affordance is needed later, that's its own spec.

### Notifications

New event type added to the existing notifications config:

```yaml
- type: ev_surplus_starved
  enabled: true
  threshold_s: 1800    # default matches SurplusStarvationS
  priority: 3
  cooldown_s: 3600
```

Handler lives alongside `driver_offline` in `go/internal/notifications/`. The event's payload carries the loadpoint id + the target's remaining time so the user's ntfy push can say "garage has been starved for 30 min; deadline 02:15".

### Helper additions

`go/internal/control/ev_dispatch.go` gains one function:

```go
// FloorSnapChargeW returns the largest step ≤ want (not the nearest).
// Zero if want < min. Used when the caller must not overshoot (e.g.
// surplus-only dispatch — any step above live surplus imports grid).
func FloorSnapChargeW(want, min, max float64, steps []float64) float64
```

Existing `SnapChargeW` (nearest) stays for the `AllowGrid == true` path.

### Loadpoint state struct extensions (`go/internal/loadpoint/loadpoint.go`)

`SetTarget` signature becomes `SetTarget(id string, socPct float64, targetTime time.Time, origin string, policy TargetPolicy) bool`. Existing call sites without policy pass the zero-value (both false) — equivalent to pre-v1 behaviour for schedule / API callers that haven't opted in.

## Edge cases (catalogued)

- **Grid meter stale + `!AllowGrid`:** force `cmdW = 0`; better to under-charge than to silently import.
- **EV driver offline:** skip `ev_set_current` sends; resume on next successful tick.
- **User-initiated pause (op_mode → 2 while our cmdW > 0):** target stays, starvation counter does not accrue.
- **Car completes mid-window (3 → 4):** auto-clear target; replan.
- **Cable unplugged (`plugged_in` true → false):** auto-clear target; replan.
- **Target deadline passes without completion:** auto-clear; no replan (DP's shortfall penalty handled the deadline already).
- **Re-arm mid-session:** new `target_time = now + ChargeDurationH`; policy re-read from current checkbox state.
- **`BatteryCoversEV` restore:** on target clear, restore to `false` (v1 has no persistent-true state).
- **Charger ramp-up at session start:** surplus math briefly over-estimates; self-corrects within the hysteresis window. Not mitigated in v1.
- **Floor-snap overshoot into export:** surplus 10 kW, floor-snap to 7.4 kW → 2.6 kW exported. Accepted — exporting natural surplus is not a bug.

## Failure modes accepted

- Brief grid import bounded by `SurplusHysteresisW × SurplusHysteresisS` (≤ 42 Wh per hysteresis cycle at defaults). Visible in energy log; not billable in any meaningful way.
- Session start latency up to one dispatch tick (5 s).
- Starvation notification delivered up to `cooldown_s` late after a prior event.

## Testing

### Unit tests

- `go/internal/control/ev_dispatch_test.go`
  - `TestFloorSnapChargeW` — exact-match, below-min, above-max, between-steps, empty-steps cases.
  - `TestSurplusHysteresisDebounceForwardOnly` — synthetic surplus time-series; commit-to-0 only after threshold; reset on first above-min tick.
- `go/internal/loadpoint/loadpoint_test.go`
  - `TestSetTargetWithPolicyPersists` — origin + policy round-trip through state.db.
  - `TestAutoClearOnCompleted` — op_mode 3→4 clears target.
  - `TestAutoClearOnUnplug` — `plugged_in` transition clears target.
  - `TestAutoClearIgnoresSurplusPause` — 3→2 with last cmdW=0 does NOT clear.
  - `TestAutoClearExpiredDeadline` — past deadline clears, no replan.
- `go/internal/notifications/*_test.go`
  - `TestEVSurplusStarvedFiresOnce` — cmdW=0 for threshold duration fires once; cooldown suppresses second.

### Integration tests (`go/test/e2e/`)

- `TestSurplusClampUnderCloudTransient` — sim PV drops then recovers; verify cmdW ramps down with hysteresis, not instantly.
- `TestOpModeCompletedClearsTarget` — sim op_mode 3→4; verify target cleared and replan count incremented within one tick.
- `TestExtendedLowSurplusEmitsEvent` — sim low surplus for 30+ sim-minutes; verify one `ev_surplus_starved` event emitted.

### Manual QA (pre-merge, documented checklist in the PR)

- Checkboxes persist last-used across page reloads.
- Start with all checkboxes off during a known-cloudy window → starvation notification arrives.
- Start with both "allow" checkboxes on → car charges immediately, battery discharge covers EV load, restores to no-coverage after session ends.
- Re-arm mid-session → new 8 h window applied, ongoing current not disrupted.

## Rollout

1. Backend + API + unit tests land first, behind no feature flag — all new fields default to v0 behaviour.
2. Frontend checkboxes + smart Start wrapper land in a follow-up PR.
3. Manual QA per checklist.
4. Default `charge_duration_h` and hysteresis values stay at the spec defaults. Operators with edge-case setups (small fuses, 1-phase chargers) override via `/api/loadpoints/{id}/settings`.

No migration needed — new state.db keys are created on first write, absence implies defaults.

## Future sub-projects (pointers, not commitments)

- **Sub-project 2 — schedule windows.** Ordered list of time-of-day windows with per-window `TargetPolicy` + target SoC + duration. Window activation posts a target with `origin: "schedule"`. Manual targets outrank schedule targets while active (manual re-arm extends the window past a schedule trigger).
- **Sub-project 3 — price-effective triggers.** A window or standing rule that says "when spot is below X öre/kWh for the next Y hours, post a schedule target with allow_grid=true". Implemented as a rule engine on top of sub-project 2's window mechanism.
- **Sub-project 4 — timeline UI.** Schedule editor + plan overlay + 24 h PV/price/EV forecast visualisation.
