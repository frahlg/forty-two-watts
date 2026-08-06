---
"ftw": patch
---

Legacy (v1) Lua drivers now run `driver_poll`, `driver_command`, `driver_default_mode` and `driver_cleanup` under an execution deadline (default 10 s), where previously only signed control-v2 drivers were bounded and a spinning driver could wedge its goroutine forever. A deadline abort is treated as a normal driver failure — restart and autonomous default mode. Per-driver override: `command_timeout_s` in the driver's YAML block (`0` restores the old unbounded behavior). `driver_init` stays unbounded for legacy drivers, since slow discovery at startup is legitimate.
