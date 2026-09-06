---
"ftw": patch
---

Add a charger without inventing an ID or pressing Save again. Charger settings apply on change, with errors and a retry beside the form. OCPP setup stays separate from cloud chargers.

Charging feedback separates the FTW request, the charger's reported limit and measured power. Old charger readings cannot claim current charging. Manual current changes apply on release; Return to plan names the action that ends a manual hold. Charge level and schedule writes run in order, and failed requests stay visible.
