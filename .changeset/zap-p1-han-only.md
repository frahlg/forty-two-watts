---
"ftw": major
---

**BREAKING CHANGE:** Sourceful Zap in FTW is now the P1/HAN site meter only. The driver no longer ingests PV, battery or V2X from devices attached to Zap. If Zap lists an inverter, battery or charger, add that device in FTW with its own driver. Sites that used Zap as a proxy for those resources will lose that telemetry until they do.
