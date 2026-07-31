---
"ftw": minor
---

A read-only driver may now sign in before it reads.

A driver that reads a vendor cloud cannot read anything until it has exchanged
a token, and it exchanges one with a POST issued from `driver_init` or
`driver_poll` — the phases the write scope refuses, correctly, since nothing
there carries a command lease. Such a driver could therefore only be published
control-capable, and its catalog entry then claimed a control path it does not
have.

`RuntimePolicy` now carries `auth_post_path` from the signed driver manifest,
and `host.http_post` skips the write scope only for a URL whose path equals it.
The exemption requires a read-only policy, a declared path, `http.post` still
granted, and an exact path match, and it does not consume the write budget — a
token refresh is driven by expiry rather than by a caller. A driver that
declares nothing behaves exactly as before.
