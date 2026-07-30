---
"ftw": minor
---

Lua host: `host.http_patch(url, body, headers)` — the verb REST device APIs
use for state-changing writes, added for the NIBE Solar PV surplus feed
(srcfl/ftw#537). It is gated by a new, explicit `capabilities.http.allow_write`
beyond the plain `capabilities.http` grant, so granting HTTP for telemetry
never implicitly grants the ability to mutate a device. Scope is exactly
PATCH: `http_get` stays a read and `http_post` stays under the plain grant
unchanged (existing drivers POST to query-style APIs), so no existing HTTP
driver changes behaviour. Without the grant a driver gets an error string and
the request never leaves the host.

The verb shares the allowlist, TLS-pinning, 1 MB response cap and managed-write
accounting of the other verbs, and — unlike `http_post` — refuses to follow
redirects: Go re-issues a redirected `PATCH` (301/302/303) as a body-less GET,
which would otherwise report success for a device write that never landed.
