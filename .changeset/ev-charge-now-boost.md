---
"ftw": minor
---

The EV modal's Start button becomes "Charge now → target": the manual
hold charges at the slider's amps and releases itself once the car's
estimated state of charge reaches the schedule's target (80 % when no
schedule is set), falling straight back to planned dispatch — pressing
Start no longer overrides the planner for the rest of the session. The
release target survives restarts with the hold, holds without a target
keep the old pin-until-Stop-or-unplug contract, and
POST /api/loadpoints/{id}/manual_hold accepts the new
`release_at_soc_pct` field.
