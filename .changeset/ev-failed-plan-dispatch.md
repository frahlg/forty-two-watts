---
"ftw": patch
---

Stop dispatching an old plan when its replacement fails, so a removed charging goal cannot keep charging the car. Keep the current plan during normal recalculation, require a successful new plan after failure, and preserve manual Start and Pause with the usual safety limits.
