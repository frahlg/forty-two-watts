---
"ftw": patch
---

FTW now holds at most one Modbus TCP connection per device. Many inverters accept a single session and drop the old one on every new connect, so a second driver on the same gateway, a driver test, or a fingerprint probe used to knock the live driver's session out mid-control; they now share the one session, each with its own unit id, and the socket closes only when the last user is gone. When something outside FTW keeps taking the device's only session, the box now says so — a rate-limited warning names the likely cause instead of flooding the log with a reconnect line per poll.
