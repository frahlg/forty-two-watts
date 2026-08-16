---
"ftw": patch
---

Self (manual) now includes EV charging when it drives the site meter toward
zero. It no longer needs `battery_covers_ev` to stop charging the home battery
while the full site imports. Other modes keep the setting. A surplus-only
loadpoint still blocks home-battery discharge into its EV without blocking
coverage of regular EVs.
