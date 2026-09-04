---
"ftw": patch
---

`GET /api/config` masks driver config keys whose names say credential (password, secret, token, api key, private key) even when the installed driver's catalog entry does not list them under `config_secrets`, and `POST /api/config` restores the stored value when the client sends the mask or a blank back. The installed copy of a driver can lag its source: a box served myuplink's `client_secret` and `refresh_token` in clear text over the LAN.
