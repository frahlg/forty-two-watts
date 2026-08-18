---
"ftw": patch
---

A slew rate of 0 W/cycle no longer freezes battery dispatch. The limiter anchors on the battery's measured power, so a zero budget snapped every target back to whatever the battery was already doing and the site held that power until restart. Non-positive now means "no external ramp limit", the same as `slew_enabled: false`.
