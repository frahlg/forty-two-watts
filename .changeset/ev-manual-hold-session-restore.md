---
"ftw": patch
---

Bind a saved manual charging request to its charger hardware and verified charging session. Keep a saved pause on the same charger. When a prior positive request cannot be verified, pause and ask the owner to confirm instead of resuming automatic charging. Preserve explicit Start or Clear actions that arrive before the first charger reading.

A running request also stops if the charger, session or loadpoint binding changes. A clear issued before telemetry survives another immediate restart, and concurrent Set/Clear writes preserve the order shown by the controller.
