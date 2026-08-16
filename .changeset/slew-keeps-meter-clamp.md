---
"ftw": patch
---

Keep battery slew from restoring charge or discharge that the live-meter
clamp removed, while mixed battery fleets stay on the aggregate meter target.
A battery that ignores commands can no longer pin dispatch on the wrong side
of the controller's target.
