---
"ftw": patch
---

Map `rated_W`, `rated_power_W` and `rated_w` onto `rated_power_w`, the name `nova.DerTelemetry` reads. None of the three was mapped, so a device's rated AC power has never reached Nova — including from our own `zap.lua`, which emits `rated_power_W`.
