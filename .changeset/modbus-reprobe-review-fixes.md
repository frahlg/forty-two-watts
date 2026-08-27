---
"ftw": patch
---

Modbus give-up recovery no longer reload-loops a missing driver file or a device that never answered, and a failed `driver_init` during that reload keeps the previous VM so default-mode still works.
