---
"ftw": patch
---

The app can now correct the car's charge level and turn PV-only charging on or off over the session. Two new command operations, `loadpoint.soc.set` and `loadpoint.surplus_only.set`, do what the box's own page does through the same code path, and the matching HTTP routes name them when the passthrough refuses them.
