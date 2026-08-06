---
"ftw": minor
---

New `site.profile` declaration (`residential` default, `commercial`) and `site.max_command_w`: a site-level override for the 5 kW fallback per-battery command cap applied when a driver has no `max_charge_w`/`max_discharge_w`. Values above 5000 W require `profile: commercial`, so a typo can never lift a home site's clamp. Per-driver limits still win over the site default; existing configs behave identically. Hot-reloadable.
