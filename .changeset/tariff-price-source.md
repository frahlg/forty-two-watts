---
"ftw": minor
---

On tariff-configured sites the planner's prices now come from the compiled TOU schedule instead of the day-ahead spot pipeline: deterministic rates across the full horizon (no forecast extension needed), full confidence, and a zero export basis so the planner never values battery-to-grid export on a site without an export agreement. Residential sites keep the existing spot + forecast path untouched.
