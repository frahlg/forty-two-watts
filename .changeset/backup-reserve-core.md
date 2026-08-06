---
"ftw": minor
---

Core-side C&I enforcement, independent of the planner: the load-shedding backup reserve becomes a per-battery SoC discharge floor in the dispatch clamp (no plan, manual hold or fallback can drain through it), and the Notified Maximum Demand becomes a third grid-import ceiling alongside the fuse and the manual peak limit. The reactive fuse-saver honors a strict hierarchy — a genuine fuse/breaker overage may draw the fleet through the reserve, a billing-only (NMD/peak) overage may not. The Go DP fallback planner raises its SoC lower bound to the reserve and plans imports under NMD with per-slot feasibility guards, so safe-fallback behavior matches the champion's constraints.
