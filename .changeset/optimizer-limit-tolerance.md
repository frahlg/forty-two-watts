---
"ftw": patch
---

A plan that rides the site's grid limit exactly is no longer rejected
for solver float noise: the external-plan validator's grid-limits
check gains the same ±2 W tolerance every other power check already
had. Rejection discarded the whole plan and silently degraded the
site to the fallback planner — observed in the field as
"slot 34 grid_w 11040.000 violates grid limits" on an 11 040 W fuse.
