---
"ftw": patch
---

Core now keeps a driver out of control when the driver registry has blocked writes pending a safe default, tracks refused battery, PV, and EV actions separately on hybrid devices, and prevents a wallbox resume from crossing a newer command, default, exclusion, or driver shutdown. Charger sends now have a deadline so one slow device cannot hold the other loadpoints' control tick.
