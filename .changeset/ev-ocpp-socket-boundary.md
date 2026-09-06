---
"ftw": patch
---

Keep OCPP messages and command replies bound to the connection they came from. A delayed status or BootNotification from an older connection can no longer clear Pause or replace the current charger's identity. Check capabilities again after reconnecting.
