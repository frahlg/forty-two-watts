---
"ftw": patch
---

Move the bundled-driver pin to device-drivers@76c968bd. A gateway booting
offline no longer retries the Sungrow EMS control block on an inverter that
has already refused it — an SG string inverter logged a failed
self-consumption reset once per watchdog tick for the life of the session.
Sungrow is the only bundled driver the move changes (1.5.6 → 1.5.7).
