---
"ftw": patch
---

Bound every control-loop driver command with a 2 s deadline. Battery dispatch, PV curtailment and loadpoint sends previously waited on `Registry.Send` with the long-lived loop context, so one driver wedged mid-poll could stall dispatch to every other driver indefinitely. A timed-out send is logged and recovery is left to the existing watchdog/staleness paths, matching how autonomous default commands are already bounded.
