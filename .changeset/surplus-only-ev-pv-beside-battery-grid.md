---
"ftw": patch
---

A surplus-only EV can take leftover PV while the home battery buys from the grid. Surplus-only is an EV policy, not a site-wide import ban: the car still cannot import, and the home battery still cannot feed the car.

Core DP includes this change. Sites using the optional Python/HiGHS planner need an updated optimizer to produce the same allocation. Core DP does not require a new optimizer image.
