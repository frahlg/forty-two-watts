---
"ftw": patch
---

Three fixes from running the OCPP central system against Sourceful's device
simulator. Each one let FTW report a limit it had not actually imposed, or
refuse a control that should have worked.

**Charging profiles are sent as Relative, not Absolute.** FTW's schedule is a
single period at second 0 with no end — "hold this limit until I say
otherwise". Absolute expresses that only with a `startSchedule` timestamp, and
while the specification says an absolute schedule without one is relative to
the start of charging anyway, a charger that parses the missing timestamp
strictly finds no valid start, treats the profile as not yet active, and
answers **Accepted** while charging on at full rate. Relative carries no
timestamp, so there is nothing to misparse — and nothing that depends on the
charger's clock agreeing with ours.

**A charger that refuses a charge-point-wide profile is retried on connector
1.** OCPP 1.6 permits a `TxDefaultProfile` on connector 0 — it is how a profile
applies to every connector — but some chargers read the connector-0 rule as
`ChargePointMaxProfile`-only and reject it. Rejecting means no limit at all, so
one retry on the first connector is the difference between a charger FTW steers
and one it can only meter.

**Manual EV controls reach an OCPP charger.** Pause, Resume, Force start and
set-current posted to `/api/ev/command` went straight to the Lua driver
registry, which an OCPP charge point is not in — it dialled us rather than
being dialled. They failed with `driver "<id>" not found` while automatic
dispatch steered the same charger correctly.
