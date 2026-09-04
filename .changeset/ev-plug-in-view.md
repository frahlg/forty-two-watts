---
"ftw": minor
---

EV modal: the plan is visible at plug-in. Above the tabs the modal now draws the planned charge windows for the car on a 24 h track, with the energy in them, under the usual one-line status. Right below sits the car's current charge, which the plan is built from: drag it to the real value and let go, and the box replans and redraws the plan. The "Set current charge" button and the SoC editor in the Scheduled tab are gone. `GET /api/loadpoints` carries the windows as `plan_windows`, and `POST /api/loadpoints/{id}/soc` replans before it answers.

Two fixes underneath, both seen on a real site: setting the car's charge level now clears the "session complete" latch instead of snapping back to the target on the next tick, and ten minutes of steady charging after that latch releases it, so the estimate follows the energy going in instead of sitting at the target while kilowatt-hours flow. `soc_source` reports `completed` when the latch is what pinned the value.
