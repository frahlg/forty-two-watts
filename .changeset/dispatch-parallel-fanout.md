---
"ftw": patch
---

Fan control-loop dispatch out concurrently instead of serially. Battery targets and PV-curtail commands within a phase are now sent to their drivers in parallel (each driver already has its own command queue and goroutine), bounded by a 1.5 s per-phase budget so slow drivers cannot push dispatch past the 2 s control tick. Phase ordering is unchanged: EV first, then battery targets, then PV curtailment.
