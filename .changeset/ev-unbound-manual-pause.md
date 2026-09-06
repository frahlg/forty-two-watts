---
"ftw": patch
---

Keep an explicit charging pause across restart even before the charger reports hardware identity. A prior Start without matching session proof restores only a pause that needs confirmation. Use the current OCPP connection's boot identity, and retry failed saves when charger data returns.
