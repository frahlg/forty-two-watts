---
"ftw": patch
---

Bundle easee_cloud 1.2.0 (srcfl/device-drivers#103): the driver now
emits `request_active`, so Core can tell "the car has stopped
requesting current" (Easee reason 50 / charging completed) from "the
box paused it". This turns on three existing protections for Easee
sites: the session-completion latch stops the planner allocating
energy to a full car, a manual Start hold auto-releases instead of
offering power all night, and the charging-interrupted notification
stops firing on the car's own renegotiation bursts. The driver's HTTP
transport is also pcall-hardened.
