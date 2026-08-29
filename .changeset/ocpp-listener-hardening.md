---
"ftw": minor
---

The OCPP listener can now be pinned to one interface, served over TLS, and
given a credential per charger.

`ocpp.bind` finally does something. The library builds its listen address from
the port alone, so the socket is unavoidably open on every interface; FTW now
refuses the WebSocket handshake for a connection that arrived on any other
address. That is an access control rather than a smaller attack surface — the
port still answers a scan — and the docs say so.

`ocpp.tls` serves `wss://` instead of `ws://`, ending the plaintext basic auth
anyone on the LAN could sniff. `client_ca_file` additionally requires every
charge point to present a certificate signed by that CA (OCPP 2.0.1 security
profile 3). Half a TLS section is refused at startup rather than quietly
serving plaintext.

`ocpp.chargers` gives a named charge point a password of its own. On OCPP the
basic-auth username is the charge point identity, so a listed charger must
present both, and the shared password stops being enough to connect under its
name — the impersonation hole the pending-charger quarantine could not close.
It is opt-in per charger; anything unlisted keeps using the shared credential.
Per-charger passwords are masked out of `GET /api/config` and survive a
settings save, matched by charger id rather than position.
