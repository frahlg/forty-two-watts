# EV loadpoint policy v1 — progress notes

Session: 2026-04-22. Branch: `feat/ev-loadpoint-policy-v1-backend`. Companion docs: `2026-04-22-ev-loadpoint-policy-v1-design.md` (spec), `2026-04-22-ev-loadpoint-policy-v1.md` (plan).

## Done (16 commits, all deployed to dev Pi)

Originally planned:

- **Task 1** — `control.FloorSnapChargeW` helper + tests. (`1e4d2d5`)
- **Task 2** — `loadpoint.TargetPolicy` + origin-carrying `SetTarget`. (`b49a254`)
- **Task 3** — Persistent per-loadpoint Settings + LastPolicy via `state.Store` KV. (`ceb6646`)
- **Task 4** — `events.EVSurplusStarved` + notifications rule. (`4eab998`, gofmt fixup `fee5e8a`)
- **Task 5** — `loadpoint.DispatchState` pure state machine (hysteresis / starvation / op-mode). (`da492b9`)
- **Task 6** — `control.State.BatteryCoversEV` surface (verification-only, no code).
- **Task 7** — `main.go` dispatch block rewrite: policy-aware surplus clamp with forward-only hysteresis, auto-clear on 3→4 or unplug, aggregated `BatteryCoversEV`, `events.EVSurplusStarved` publish. (`6f73502`)
- **Task 8** — Handlers extracted into `api_loadpoint_policy.go` + new `/settings` endpoint. (`f247107`, gofmt fixup `1c64903`)
- **Task 10** — Event bus wiring (already present pre-spec; verified).
- **Task 15** — Frontend: three policy controls on the EV popup, smart Start/Resume wrapper, last-policy pre-fill. (`d678d44`)

Additions during implementation (not in original plan, but pragmatic):

- **MPC probe fix** — `mpcSvc.Loadpoint` only emits a spec when `TargetSoCPct > 0`, so plugging in no longer causes opportunistic EV planning. (`99f3064`)
- **Surplus includes home-battery charging** — EV surplus calc adds aggregate `DerBattery` charging readings; the control PI slews the home battery down as EV ramps up. (`5bc0b94`)
- **Dynamic 1φ/3φ phase switching** (Task 15b, ad-hoc) — `ev_set_phases` driver command, 1-phase config fields on `Loadpoint`, `DispatchState.ObservePhases` / `.ShouldSwitchPhase` with 120 s cooldown, dispatch chooses phase from surplus band. (`72daed3`, `ecd132a`, `d55a46d`)
- **`ev_set_phases` on `/api/ev/command`** — allowlist entry + phases payload plumbing so the feature is manually testable. (`718acab`)

## Pending

- **Task 9** — API handler tests (`api_loadpoint_policy_test.go`). Handlers work in production; tests are the formal guard.
- **Task 11** — Smoke-test on Pi (ad-hoc validation has been happening throughout; needs a formal checklist).
- **Task 12** — E2E test: surplus clamp under sim cloud transient (`go/test/e2e/loadpoint_policy_test.go`).
- **Task 13** — E2E test: auto-clear on op-mode 3→4.
- **Task 14** — Open PR on GitHub. Branch is pushed as `feat/ev-loadpoint-policy-v1-backend`; holding the PR open per operator request.
- **Task 16** — Manual QA checklist against the new UI (PR #2 formality; UI is live on Pi).

## Known open questions

- **Grid top-up mode** — user asked for a third `GridMode` value (`"topup"`) between `"none"` and `"any"`: allow grid only up to the charger's minimum step, no higher. Design drafted but not yet implemented. Would replace `AllowGrid` bool with a 3-valued enum and add a segmented UI control.
- **Easee 1-phase physical constraint** — 1φ max depends on the installation; default config set to 7360 W (32 A × 230 V) but user capped to 2300 W (10 A) in their config for their setup.
- **Float-validation stylistic asymmetry** in `loadpoint.Manager.UpdateSettings` — `SurplusHysteresisW` runs range-check unconditionally while integer fields gate on `!= 0`. Functionally equivalent (zero passes the range check) but inconsistent; reviewer flagged as "Important" nit on Task 3.

## Deployment state

- Dev Pi (`192.168.1.139`) runs the dev-binary override (`~/ftw-dev/bin/forty-two-watts`) at version `v0.43.0-20-g718acab`. Easee driver + web dir also bind-mounted from `~/ftw-dev/`.
- Config hot-reloaded to include 1-phase steps capped at 10 A (`allowed_steps_1phase_w: [0, 1380, 1610, 1840, 2070, 2300]`, `max_charge_1phase_w: 2300`).
- No migration needed; `state.db` KV keys created on first write.

## Mode-related operator notes

- **`planner_arbitrage`** will schedule battery export/import based on price signals regardless of EV policy. If the intent is "battery absorbs PV first", `planner_self` (self-consumption) is the right mode. Our dispatch's `BatteryCoversEV` flag governs whether batteries cover EV load when present; it does not override arbitrage scheduling.
- Fast EV→0 transitions can produce 1–2 ticks (≤ 10 s) of transient export because the control PI has a 500 W/tick slew limit on battery targets. Not a bug, physics.
