# MPC planner

The Model-Predictive-Control planner is the outer, slow loop that decides
*what the battery should do over the next 48 hours*. It runs every
15 minutes, emits a schedule of grid-power targets per 15-minute slot,
and feeds those targets into the inner 5-second control loop.

The planner never bypasses safety — fuse, SoC bounds, and slew limits
are enforced by the inner loop regardless of what the plan says.

---

## Strategies

Three user-selectable strategies. All respect SoC + power + efficiency
limits; the difference is **what the planner is allowed to do with the
grid connection**.

| Strategy | Grid-charge? | Battery export? | When to pick |
|---|---|---|---|
| **Self-consumption** | no | no | Safest. Battery only covers local load or absorbs PV surplus. |
| **Cheap charging** | yes (when cheap) | no | Good when export tariffs are low. Top up overnight, use during peaks. |
| **Arbitrage** | yes | yes | Biggest savings on volatile days. Charges cheap, discharges into expensive hours. |

The currently-selected strategy is shown in the `Strategy` control panel.
A one-sentence description refreshes every 5 seconds.

Legacy modes (`idle` / manual `self_consumption` / `peak_shaving` /
`charge`) remain available under the `Manual…` toggle.

---

## How it thinks

For each slot in the 48-hour horizon, the planner has five inputs:

- **price** — consumer öre/kWh (spot + grid tariff + VAT)
- **PV forecast** — expected generation W (digital twin)
- **load forecast** — expected household consumption W (digital twin)
- **SoC at start of slot** — propagated from the previous slot's decision
- **confidence** — 1.0 if day-ahead real, ~0.6 if ML-forecasted

Dynamic programming over a discretized SoC grid (51 levels × 21 action
levels × 193 slots ≈ 200k evaluations) finds the cost-minimizing
schedule in ~0.6 ms.

### Cost function

```
slot_cost = import_cost − export_revenue
          = price × max(grid_kWh, 0) − export_price × max(−grid_kWh, 0)
```

where `grid = load + pv + battery`, all site-signed (import positive,
PV negative, battery positive = charging). Export revenue defaults to
`mean_spot + export_bonus − export_fee`.

### Confidence blending (the "don't bet the farm on a guess" lens)

Predicted slots are less trustworthy than real day-ahead prices. The DP
doesn't ignore them — it pulls them toward the horizon mean:

```
effective_price[t] = confidence × real_price + (1 − confidence) × horizon_mean
```

With `confidence = 0.6`, a predicted 200 öre peak is weighted as if it
were only about 160 öre for the decision, while a predicted 20 öre trough
gets pulled up to about 60 öre. Result: the planner still commits when
the ML forecaster *and* the hour-of-week pattern agree, but it doesn't
lock in expensive arbitrage trades on a prediction that may be off.

The reported `cost_ore` on each action is the **raw** (un-blended) value
— what you'd actually pay if prices hold. Blending is a decision lens
only.

### Per-slot decision reasons

Every slot comes with a short human-readable `reason` string, surfaced
in the UI hover tooltip:

- `absorb PV surplus` — battery charging, PV-surplus baseline
- `charge — price below horizon mean` — DP picked up on a cheap slot
- `discharge — cover local load` — battery covering house consumption
- `discharge — price above horizon mean` — DP discharging into a peak
- `idle — import to cover load`
- `idle — export PV surplus`

Predicted slots have `(predicted)` appended.

---

## Horizon

Default: **48 hours**. Day-ahead auctions usually publish tomorrow's
prices around 13:00 CET; before that, the price-forecaster fills the
gap so the horizon is always 48 h regardless of when you look.

Configurable via `planner.horizon_hours`.

---

## Staleness guard

If the most recent plan is older than `MaxPlanAge = 30 min`, the control
loop falls back to `self_consumption` with `grid_target_w = 0`. The
status endpoint exposes `plan_stale: true` so the UI can warn.

A single missed replan is absorbed without fallback (the window is
double the replan interval). Only sustained outage trips it.

---

## Dispatch path

```
15-min cadence:
  MPC.replan() → Plan cached on Service
                   ↑
                 price forecaster + PV twin + load twin

5-second cadence:
  Control.dispatch():
    if Mode.IsPlannerMode():
        grid_target_w = plan.GridTargetAt(now)   // current-slot lookup
    PI controller chases grid_target_w
    Cascade splits across batteries (proportional / weighted / priority)
    Fuse + SoC clamps
```

The control loop keeps running `self_consumption` logic when no
planner strategy is selected — the MPC only writes `grid_target_w`
when the operator has chosen one.

---

## API surface

- `GET  /api/mpc/plan` — latest cached plan (mode, horizon, per-slot actions with reasons)
- `POST /api/mpc/replan` — force an immediate replan
- `POST /api/mode {"mode":"planner_arbitrage"}` — activates a strategy AND forces an MPC replan so targets take effect within one control cycle

---

## Tuning knobs

| Config | Default | Effect |
|---|---|---|
| `planner.mode` | `self_consumption` | Starting strategy |
| `planner.horizon_hours` | 48 | How far ahead to plan |
| `planner.interval_min` | 15 | Replan cadence |
| `planner.soc_min_pct` / `soc_max_pct` | 10 / 95 | SoC bounds the DP respects |
| `planner.charge_efficiency` / `discharge_efficiency` | 0.95 / 0.95 | Round-trip = 0.9025 |
| `planner.export_ore_per_kwh` | mean spot | Export revenue per kWh |

The DP internally uses 51 SoC levels × 21 action levels; this is fixed
and gives ≈0.6 ms solve time over a 193-slot horizon.

---

## When arbitrage won't help

Flat-price days give the DP nothing to work with; all three strategies
return near-identical schedules. See `go/internal/mpc/stress_test.go`
for the scenario-comparison benchmark that illustrates this.

---

## Solar curtailment

Post-processing flags each plan slot with a recommended PV cap
(`pv_limit_w`) when all three conditions hold:

1. the slot is exporting (`grid_w < 0`)
2. export revenue is non-positive (spot ≤ 0 or fees ≥ revenue)
3. local consumption (load + battery charge) cannot absorb the PV

In that regime, exporting costs money and curtailing the PV is a pure
win. The recommended limit equals `load_w + max(battery_w, 0)` — just
enough to cover on-site absorption.

The MPC cost function isn't changed by curtailment; the annotation is
a dispatch-time suggestion. A driver that advertises
`supports_pv_curtail` can pick up `action.pv_limit_w` on each control
cycle and send a setpoint to the inverter. Drivers without this
capability ignore the field; curtailment then just doesn't happen.

---

## Savings — what to expect

The stress test (`go test -run AnnualSavings ./internal/mpc/`) projects
annual SEK savings across six scenario types weighted by prevalence:

| Scenario | Weight | Self-cons | Cheap-charge | Arbitrage |
|---|---:|---:|---:|---:|
| sunny_mild | 25% | 430 | 438 | 479 |
| cloudy | 30% | 596 | 1 038 | 1 086 |
| price_spike | 5% | 753 | 2 239 | 2 391 |
| flat_prices | 10% | 106 | 106 | 111 |
| cheap_night | 15% | 613 | 1 312 | 1 640 |
| solar_surplus | 15% | 112 | 112 | 131 |
| **TOTAL SEK/yr** | | **2 610** | **5 245** | **5 839** |

Figures based on a 15 kWh / 5 kW battery and typical Nordic-ish prices
(see scenario generators in `stress_test.go`). Arbitrage adds
~3 200 SEK/yr over self-consumption but depends heavily on price
volatility — a stable market year narrows the gap.
