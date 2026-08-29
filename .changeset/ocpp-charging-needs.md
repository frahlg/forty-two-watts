---
"ftw": minor
---

FTW now listens to what the car itself asks for. On an ISO 15118 session an
OCPP 2.0.1 charger forwards the vehicle's own `NotifyEVChargingNeeds` —
the energy it wants, when it expects to leave, and on DC its battery capacity
and present state of charge. Core takes that as the session's truth: the
reported capacity replaces the configured `vehicle_capacity_wh` (measured beats
an operator's estimate of the car that usually parks here), the reported SoC
re-anchors the session estimate, and the two together with the requested energy
derive the target the planner sizes on. A departure time the car states becomes
the loadpoint's target time, and one it does not state never erases the
operator's own. Everything is session-scoped and reverts on plug-out, like an
identified vehicle profile. The report is visible on `GET /api/ocpp/chargers`
as `charging_needs`, and quarantine still applies — a pending charge point's
needs are shown but never reach a loadpoint.

An AC session states energy without a battery size, so no target fraction is
derived from it; guessing one would feed the planner a number the car never
claimed.
