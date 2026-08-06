---
"ftw": patch
---

Config validation now rejects `fuse.phases` above 3. Dispatch only reads three phase currents while the aggregate power limit used every configured phase, so a larger value overstated the usable fuse budget. It also rejects more than one driver with `is_site_meter: true`; the first match previously won without warning. Existing invalid configs must be corrected before the next restart: set `fuse.phases` to 1, 2 or 3 and keep exactly one site meter.
