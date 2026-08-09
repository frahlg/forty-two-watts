---
"ftw": patch
---

The contract registry now names the command operations. `site.mode.set` and `battery.hold` were hand-written twice — here beside the dispatcher, and in the app's simulator — with the scope each demands written twice more, and nothing compared the four. The registry's new `ops` block is the one place the pair is written down: the generator renders it as `RegistryOps`, and a test holds `defaultOps()` to it, the same arrangement mode tiers already have. Containment rather than equality, deliberately — the registry may name an op ahead of the box, and `battery.hold` is exactly that today: an app may say it, this box rejects it with `E_UNKNOWN_OP`, and nothing acts.

The registry also takes in the last of the app's own error codes. Its header has always said every app-raised code has a home there; three did not — `E_NO_ACK`, `E_NO_ANSWER` and `E_BAD_BODY` sat in the app alone, outside the check that keeps prose and retry rules honest. They now sit in `client_errors` beside `E_RESPONSE_TOO_LARGE`. The box generates nothing from that block and must never send one of them; the copy here exists because the file is one file in two repositories, byte for byte.
