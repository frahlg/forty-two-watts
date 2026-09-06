---
"ftw": patch
---

Ask OCPP chargers for fresh hardware identity after reconnecting, with bounded retries for missing replies and a safe fallback when the request is unsupported. Pause an older manual Start when hardware identity is lost, keep an explicit Pause, and allow a new Start to bind to the next verified identity. Keep cable status unknown during a network interruption, and preserve the reconnect boundary even when it falls between control ticks.
