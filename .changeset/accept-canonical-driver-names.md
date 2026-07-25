---
"ftw": minor
---

Accept the canonical driver spellings alongside the current ones, so srcfl/device-drivers can convert its catalog one driver at a time without any site losing telemetry. `host.emit` reads `W` and `SoC_nom_fract` when `w` and `soc` are absent, and `write`, `write_registers` and `now_ms` are registered as aliases of `modbus_write`, `modbus_write_multi` and `millis`. Nothing is removed; the older names keep working until the catalog has moved.
