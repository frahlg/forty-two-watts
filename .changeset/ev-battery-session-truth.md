---
"ftw": patch
---

Keep a confirmed EV battery level across restart only when fresh charger telemetry identifies the same hardware and charging session. Show when the level cannot be retained or the disk write failed. Changing battery capacity preserves the current level and its confidence.

Treat a car declining current as a separate charging status. It no longer changes the estimated battery level to the target or sends a completed notification. Completion needs a fresh matched vehicle battery reading.
