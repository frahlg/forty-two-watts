# priceforecast — zone-aware spot-price forecaster

## What it does

168 hour-of-week EMA buckets per bidding zone + 12 monthly multipliers.
Bayesian blend with a baked Nordic prior so day-one predictions already
have the right morning/evening shape. Backfills spot prices for slots
beyond the day-ahead publication cutoff so the MPC can plan the overnight
arbitrage run even when tomorrow's prices aren't out yet.

## Math

Bucket index (`forecast.go:235-239`):

```
hourOfWeek(t) = ((weekday + 6) % 7) * 24 + hour    (UTC, Mon=0..Sun=6)
```

Bayesian blend per bucket (`forecast.go:192-204`):

```
posterior = (priorValue * PriorWeight + sum_observed) / (PriorWeight + n_observed)
```

With `PriorWeight = 8` (`forecast.go:163`) two real samples give
80% prior + 20% fitted; 40 samples give 17% prior + 83% fitted. Month
multipliers use the same blend but over normalized ratios
(`forecast.go:209-222`).

Prediction (`forecast.go:137-140`):

```
spot(t) = Bucket[hourOfWeek(t)] * Month[month(t)-1]
```

Baked prior shape (`forecast.go:79-121`): morning ramp 07–09 (×1.6), midday
trough 11–14 (×0.55), evening peak 17–20 (×1.85), overnight 00–05 (×0.65),
weekends softened. Base level 50–80 öre/kWh by zone
(SE3/SE4/DK1/DK2/DE = 80, NO2/FI = 70, SE1/SE2/NO1/NO3/NO4 = 50). Monthly
prior peaks in winter (Dec 1.40, Jan 1.35) and troughs in summer
(Jul 0.70).

## Inputs / outputs

Fit input: `[]state.PricePoint` (spot öre/kWh, slot timestamp, zone) pulled
from the last 90 days of `state.Store.LoadPrices` (`forecast.go:360-362`).

Per-zone output: `ZoneModel.Predict(t) float64` in öre/kWh (raw spot,
no tariff/VAT). MPC adds tariff + VAT before using it
(`mpc/service.go:442-443`).

## Training cadence + persistence

Refits from history every `RefitInterval = 6h` (`forecast.go:246`).
Needs ≥ 24 price rows to bother refitting (`forecast.go:369`).

Persistence: whole `map[zone]*ZoneModel` JSON-blobbed via
`state.Store.SaveConfig("pricefc/state", …)` (`forecast.go:243`). Restored
on startup.

Seed hook: `Service.SeedFromCSV(path)` ingests historical prices from a
CSV (`zone,slot_ts_ms,spot_ore_kwh[,slot_len_min]`) via SQLite upsert,
then kicks a refit. Idempotent — safe on every boot (`forecast.go:410-496`).

## Public API surface

Zone model (`forecast.go`):

- `NewZoneModel(zone) *ZoneModel`
- `(ZoneModel).Predict(t) float64`
- `(*ZoneModel).FitFromHistory(pts []state.PricePoint)`
- Constants `Buckets = 168`, `MinTrustSamples = 4`, `PriorWeight = 8`, `RefitInterval = 6h`.

Service (`forecast.go`):

- `NewService(st, zones []string) *Service`
- `(*Service).Start(ctx)` / `.Stop()`
- `(*Service).Predict(zone, t) float64`
- `(*Service).Model(zone) *ZoneModel`
- `(*Service).SeedFromCSV(path string) (int, error)`

## How it talks to neighbors

- MPC consumes `Service.Predict` via `mpc.PricePredictor`
  (`mpc/service.go:28`). `extendPricesWithForecast` synthesizes price rows
  for slots between the latest published end and the horizon end
  (`mpc/service.go:410-454`), tagging them `source = "forecast"` so the
  MPC sees `Confidence = 0.6` in `buildSlots` (`mpc/service.go:492-494`).
- Reads history via `state.Store.LoadPrices`.
- Effective price in the MPC: `effPrice = 0.6 * raw + 0.4 * mean` for
  forecast slots (`mpc/mpc.go:195-197`).

## How the Bayesian blend feels in practice

One bucket sees zero samples after 30 days → still returns the baked
prior. Two samples → 20% of the way toward observed. 40 samples → fitted
value dominates. This is why sparse SE1 history never destroys the shape.

## What NOT to do

- Do not persist the whole prices table here — that's `state.Store`'s
  job. This package only stores the fitted model.
- Do not treat `Predict` output as consumer-total price; it's raw spot.
  The MPC applies tariff + VAT before feeding to the optimizer.
- Do not lower `PriorWeight` below ~4; the shape will collapse on thin
  history and re-learn noise.
- Do not break CSV idempotency — `SeedFromCSV` runs every boot in some
  setups. Rely on SQLite upsert on `(zone, slot_ts_ms)`.
