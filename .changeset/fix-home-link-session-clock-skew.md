---
"ftw": patch
---

Fix Home Link sessions being rejected over a few milliseconds of clock difference between the gateway and the browser. A gateway now issues a session lifetime below the verifier ceiling, so remote passkey access works instead of failing with "Could not reach this home". Reads also reopen a bounded session instead of leaving the page dead until a reload, and a failed session or read now states its cause.
