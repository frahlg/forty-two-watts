---
"ftw": minor
---

New scheduled-tariff engine (`go/internal/tariff`) and `tariff:` config block for C&I sites on time-of-use supply: seasonal peak/standard/off-peak bands per day class with holiday handling, energy rates in minor currency units per kWh, and a demand charge (cents/kVA on the billing-cycle peak over configurable integration windows). First target is South African Eskom Megaflex-style and municipal tariffs; the schedule is fully data-driven. Companion `site` declarations added: `currency`, `nmd_kva`, `assumed_power_factor` and `backup_reserve.min_usable_energy_wh`. This release adds the schema and interpreter; planner and dispatch integration follow separately.
