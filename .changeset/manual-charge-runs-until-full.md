---
"ftw": patch
---

EV manual charge: "Charge now" runs until the car is full, Stop or unplug. It no longer stops at the schedule's target SoC. The SoC estimate is a guess on chargers that cannot read the car, and a Start that released itself the moment the guess sat at the target left the operator with no way to charge. The API also refuses a `release_at_soc_pct` the estimate already meets (409) instead of installing a hold that clears on the next tick.
