---
"ftw": patch
---

An MQTT driver that stops draining its subscription no longer grows the inbound queue without limit. The buffer is bounded at 1024 messages, dropping the oldest half on overflow — the same rule the websocket and TCP capabilities already follow — so a stalled driver on a busy broker can no longer exhaust memory on the box.
