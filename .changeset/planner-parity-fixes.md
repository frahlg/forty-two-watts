---
"ftw": patch
---

Planner parity fixes ported from the MILP formulation (#1020): the
strict self-consumption bias clamps at zero price instead of
inverting into an import bonus on negative-price slots; the PV-charge
bonus applies in every mode (still bounded by live PV surplus);
horizon mean prices are length-weighted for mixed slot lengths; the
simulated plan starts at the battery's real state of charge instead
of the nearest grid point; and replan diagnostics persist the
arbitrage spread and PV-uncertainty inputs so a snapshot re-solves
under the exact economics the replan used.
