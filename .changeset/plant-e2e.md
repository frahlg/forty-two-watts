---
"ftw": patch
---

Full-stack plant e2e scenario: sim-pcs racks ← ftw-plant controller ← ftw_plant driver ← core driver registry. Verifies aggregate telemetry, dispatch convergence on the racks, headroom derating on a rack fault, and — with the module killed — that the racks ramp themselves to zero on lease expiry while the driver goes stale into core's watchdog path.
