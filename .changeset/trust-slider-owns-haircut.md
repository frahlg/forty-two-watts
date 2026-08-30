---
"ftw": minor
---

The Plan card's forecast-trust slider always works now. An explicit
`pv_forecast_safety_k` in config.yaml used to win over it and render it
permanently disabled with a "config.yaml wins" note; the field is now a
first-boot seed only — it maps to the nearest trust step once when no
preference is stored, and the slider owns the live value from then on,
the same stored-wins contract `forecast_trust` already had.
