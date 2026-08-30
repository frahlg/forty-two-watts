---
"ftw": minor
---

The planner now runs inside Core by default. The Go solver — measured
within öre of the external MILP on real site snapshots and structurally
immune to the relaxation failure modes the external stack needed guard
rails for — plans against the per-slot PV downside at 201×401
resolution. The Python/HiGHS optimizer is no longer the champion: with
planner.shadow_python (default on) it runs after each replan as a
comparison shadow on identical inputs, and every replan logs and
records the terminal-corrected cost difference — the field evidence
for its scheduled removal. Set planner.engine: python to keep the old
arrangement during the transition.

A battery that has drifted outside soc_min…soc_max no longer stops
Core from planning. The planner starts from the nearest band edge,
warns with the real reading and the difference in Wh, and records the
unclamped value on the diagnostic as initial_soc_unclamped; the
dispatch clamp and the driver's own floor still bound what any plan
can ask the hardware for. A reading that is not physically possible —
outside 0–1 — is still refused, and the previous plan stands.
