---
"ftw": patch
---

The planner's DP grid resolution rises from 41 SoC × 81 action levels
to 201 × 401 (about 0.4 % SoC and 24 W steps), closing most of the
measured discretization gap to the external MILP; replans with an
active EV loadpoint automatically derate to 101 × 201 to keep the
extended state space near one second. Solve budgets were measured on
the snapshot replay bench before raising the defaults.
