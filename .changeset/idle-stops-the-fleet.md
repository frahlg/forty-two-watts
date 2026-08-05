---
"ftw": patch
---

Idle mode now stops the batteries instead of stopping the commands.

Selecting idle used to issue no command at all. A battery holds the last
setpoint it accepted until it is given another one, so switching to idle while
a battery ran at 5 kW left it running at 5 kW, and what happened after that was
the vendor's decision rather than ours: a Ferroamp EnergyHub's forced mode
expires, and on 2026-06-10 one reverted to its own behaviour and charged 2.6 kW
from the grid while FTW believed it was idling, while a Sungrow holds until
told otherwise. Idle now commands 0 W to every battery it may command and
re-sends it on every control tick, so the hold is ours and cannot decay.

The mode key stays `idle` — the `/api/modes` and Home Assistant contract is
unchanged, and so is every automation built on it. The button is now labelled
"Stop batteries", and its tooltip says what the mode does: the fuse-saver still
overrides the hold and discharges a battery when the site is about to trip its
main fuse, and EV charging and PV curtailment are unaffected and keep their own
controls. To leave a battery to another controller entirely, use the per-driver
`observe_only` setting, which excludes it from dispatch altogether.
