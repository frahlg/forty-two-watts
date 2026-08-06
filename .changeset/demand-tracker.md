---
"ftw": minor
---

Billing-demand tracking for tariff-configured C&I sites: apparent power is integrated over clock-aligned utility windows (30 min default) with sample-and-hold weighting, classified against the tariff's demand bands, and the highest counted interval per billing cycle becomes the demand-charge peak. Intervals and the peak persist in state.db (`billing_demand` / `billing_peak`) so a restart mid-cycle keeps the peak-so-far. Observation only runs while the site meter is fresh — a stale meter leaves a coverage gap, never fabricated demand. New `GET /api/demand` returns the running window, billing peak, NMD and recent intervals; residential sites without a `tariff:` block are untouched (404).
