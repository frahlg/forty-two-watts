---
"ftw": minor
---

The EV modal now says why the charger is or is not charging, and when it
will: the next planned charge window from the active plan ("Charging
planned 02:15–06:30, ~18 kWh"), an explicit "waiting for tomorrow's
prices — PV surplus only until then" state when grid-funded planning is
deferred past the published price horizon, "charger offers X kW but the
car isn't drawing" with the charger's own reason when the vehicle
declines, and a plain warning when nothing (schedule, PV-only, Start)
will ever start a charge. GET /api/loadpoints carries the new fields:
`plan_next_start_ms` / `plan_next_end_ms` / `plan_next_wh` /
`plan_total_wh`, `grid_deferred`, and `commanded_w` / `commanded_known`.
