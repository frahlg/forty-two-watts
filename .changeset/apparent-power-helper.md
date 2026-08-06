---
"ftw": patch
---

Apparent-power (VA) estimation for kVA demand tracking: `telemetry.SiteApparentPowerVA` derives site apparent power from the best available site-meter telemetry — per-phase V·I when all configured phases report fresh currents, √(P²+Q²) from reactive metrics (`meter_q_l{n}_var` or the DSMR import/export split), else |P|/`site.assumed_power_factor`. Stale metrics are ignored so they cannot fabricate demand. Metric-name convention documented in docs/site-convention.md.
