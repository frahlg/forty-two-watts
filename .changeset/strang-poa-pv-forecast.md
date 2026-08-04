---
"ftw": minor
---

PV forecasts are now orientation-aware. When per-plane geometry is configured
(the Weather tab's PV arrays: tilt/azimuth/kWp), a radiation-bearing forecast
provider's global horizontal irradiance is projected onto each panel plane via
the physics `sunpos` model and summed, instead of the previous flat
`rated × (W/m² / 1000)` estimate that ignored panel orientation. Sites with no
arrays configured keep the existing behaviour, and providers that already return
site-calibrated watts (Forecast.Solar) are left untouched.

Providers that publish only global horizontal irradiance get an Erbs correlation
to split it into direct and diffuse components before projection, so a
south-facing 35° roof and a flat one no longer receive the same forecast.
