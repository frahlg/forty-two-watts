---
"ftw": patch
---

Enabling the OCPP server from the Chargers panel now works on the first
try: the username field carries the real default ("ftw") instead of a
placeholder that validation then rejected, and saving an OCPP change
honestly reports that a restart is required — the central system
listener only starts at boot, so the previous "no restart needed"
answer left the port silently closed after an apparently successful
save.
