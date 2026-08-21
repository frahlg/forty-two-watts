---
"ftw": patch
---

A hostname with no dot is no longer treated as local. If the driver catalog cannot be read, or a configured driver is missing from it, config secrets stay hidden. Driver test and fingerprint refuse loopback, localhost, and link-local targets on MQTT, Modbus, HTTP, WebSocket and TCP.
