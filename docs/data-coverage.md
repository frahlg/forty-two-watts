# Geographic coverage of external data sources

FTW controls hardware anywhere, but it depends on external data for two
things that ship today: **spot prices** and **weather/PV forecasts**. Those
have very different geographic reach, and the difference decides how much of
FTW is useful at a given site.

Short version:

- **Weather and PV forecasting works worldwide.**
- **Price-driven planning works in Europe only.**
- There is **no manual or fixed-tariff price provider** to stand in outside
  Europe.

A site outside Europe can still run FTW for monitoring, safety and control —
but the economic optimisation that motivates most of the planner has no price
source to work from.

`GET /api/data-sources` answers this per site: it returns every source with
its coverage area and, when the site location is known, whether that source
reaches it. The Weather and Price settings tabs render the same data. This
file is the prose; [`go/internal/coverage`](../go/internal/coverage) is the
machine-readable source of truth, and the two are meant to stay in step.

> **Coverage bounds are advisory.** Each bounded source declares a lat/lon
> box. Treat `covers: false` as definitive and `covers: true` as "worth
> trying". The upstream API is always the final word.

## Spot prices — Europe only

Configured under `price.provider`.

| Provider | Coverage | API key | Notes |
|---|---|---|---|
| `sourceful` | European day-ahead markets | No | Default. Sourceful's cached ENTSO-E API. |
| `elprisetjustnu` | **Sweden only** — zones SE1–SE4 | No | 15-minute PTU since late 2025. |
| `entsoe` | ENTSO-E member markets (most of Europe) | Yes | Direct from the Transparency Platform. |
| `none` | — | — | Disables price fetching entirely. |

There is **no provider for any market outside Europe**. North America
(CAISO, ERCOT, PJM, ISO-NE, NYISO, MISO, SPP, AESO, IESO), Australia
(AEMO/NEM), Japan (JEPX) and everywhere else are unsupported, and there is
no manual or fixed-tariff provider to stand in for them.

Prices are stored in **minor units of the configured currency** per kWh
(öre, cent, øre, …). ENTSO-E figures that arrive in another currency are
converted with **ECB** daily FX rates.

> The Tibber driver (`drivers/tibber.lua`) is telemetry only — it reports
> meter readings, not prices, so it is not a fourth price source.

The zone picker is served from `GET /api/prices/zones`, the same table the
fetchers use. Country first, zone second. See
[`go/internal/prices/zones.go`](../go/internal/prices/zones.go).

## Weather and PV forecasts — worldwide

Configured under `weather.provider`. All four work at any latitude/longitude.

| Provider | Coverage | API key | Signal quality |
|---|---|---|---|
| `met_no` | Global | No | Cloud cover only — weakest PV signal. |
| `openweather` | Global | Yes | Cloud cover only. |
| `open_meteo` | Global | No | Shortwave radiation (GHI) — good. |
| `forecast_solar` | Global | No (free tier) | Site-calibrated watts from panel geometry — best. |

Accuracy varies by region because the underlying numerical weather models
do, but none of these are geographically gated. Prefer `open_meteo` or
`forecast_solar` when the site has array geometry: they carry an irradiance
signal, which is what the orientation-aware plane-of-array model needs.

## What is not in this tree yet

Two further data sources are regional and are **not shipped** on this
branch. They are listed so a non-European site is not surprised later:

| Capability | Planned source | Coverage when it lands |
|---|---|---|
| PV performance scoring / forecast calibration | SMHI STRÅNG historical irradiance ([#734](https://github.com/srcfl/ftw/pull/734), [#726](https://github.com/srcfl/ftw/issues/726)) | Nordic region only |
| Automatic roof geometry | Lantmäteriet via STAC ([#717](https://github.com/srcfl/ftw/discussions/717), [#735](https://github.com/srcfl/ftw/pull/735)) | Sweden by default; any conformant STAC catalog |

Until those land, a site anywhere still has manual array geometry on the
Weather tab, and the self-learning PV twin still calibrates from measured
production.

## What a non-European site loses today

| Capability | Works outside Europe? |
|---|---|
| Device control, safety, dispatch | Yes |
| Telemetry, history, dashboard | Yes |
| Weather + PV forecasting | Yes |
| Self-learning PV twin | Yes |
| Price-driven planning / optimisation | **No** — no price source |
| Manual array geometry | Yes |
