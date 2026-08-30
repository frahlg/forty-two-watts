---
"ftw": minor
---

The EV modal names the exact clamp behind a paused charger instead of
the generic "paused by the box": main-fuse protection (with automatic
resume), waiting for PV surplus, stale site-meter safety hold — and an
ongoing charge says when the main fuse is limiting its rate. Every
dispatch branch now records why it chose the commanded watts, exposed
as `commanded_reason` on GET /api/loadpoints ("plan", "no_plan_budget",
"pv_surplus", "pv_surplus_pause", "fuse_limit", "fuse_cooldown",
"site_meter_stale", "manual_hold", "wake_kick"). Prompted by a field
report where the plan showed charging while the box sent 0 A and the
operator spent the evening debugging cable and charger.
