---
"ftw": minor
---

The forecast-trust slider becomes a real dial: 41 positions (0–2 in
0.05 steps) instead of three, stored as the numeric safety factor the
planner actually uses. With the per-slot PV hedge this is now a
tangible control — each notch changes the share of every slot's own
forecast uncertainty the plan holds in reserve, the hedge line updates
live while dragging, and releasing the slider replans immediately.
Existing three-step choices and the enum API field keep working; old
clients read the nearest step.
