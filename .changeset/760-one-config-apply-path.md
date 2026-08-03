---
"ftw": patch
---

Saving a config through the API now applies it exactly like a file edit. Previously POST /api/config hot-applied only a hand-picked subset of control fields and swapped the shared config pointer itself, which blinded the config watcher's own diff — so a site meter set for the first time (the setup wizard's normal path) never reached the running controller: the dashboard showed Grid 0 W and an inflated Load, and dispatch had no site boundary until a process restart. Both paths now run one shared apply, so hot-reload of the site meter, slew enable, DC-link protection, inverter groups, fuse parameters and the mpc/loadmodel sync all work from the UI too.
