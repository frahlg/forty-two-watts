---
"ftw": minor
---

Optional Modbus TCP proxy. Drivers share one socket per host:port, and other LAN integrations can talk to that same session through FTW. Off by default; writes stay blocked unless you opt in.
