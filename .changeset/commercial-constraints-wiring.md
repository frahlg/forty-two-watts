---
"ftw": minor
---

The planner now sends `commercial_constraints_v1` to the optimizer on tariff-configured C&I sites: the demand charge (kVA rate and billing peak converted to real power via the assumed power factor), per-slot demand-window flags from the tariff schedule, and the load-shedding backup energy floor. The block is included only when the optimizer's handshake advertises the feature — an older optimizer degrades to TOU-only planning with a warning, never a failure. `ValidatePlan` independently rejects any plan that drains stored energy below the backup reserve, using the same non-worsening recovery semantics as the SoC bounds.
