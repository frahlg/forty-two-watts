---
"ftw": patch
---

A battery can no longer keep charging against a charge block that the site has already closed. The reactive control arm stops early when grid power is close enough to its target, and it issued no command when it did — but a battery holds its last setpoint until it gets another one, so an earlier charge command kept running. The trap is that the charge itself is what keeps the meter near target: on a planner slot that forbids charging, a battery absorbing a 2 kW solar surplus holds the meter at −50 W, the error stays inside the deadband, and the tick that would have stopped the charge walks away for as long as the sun holds. The surplus the slot exists to export goes into the pack instead. The same silence kept a battery charging after its own driver reported that it cannot charge. Such a tick now commands zero, and a deadband tick with nothing blocked still commands nothing.
