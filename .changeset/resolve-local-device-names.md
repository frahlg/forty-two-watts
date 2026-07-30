---
"ftw": minor
---

Resolve device `.local` names over mDNS so devices can be configured by name instead of a DHCP-assigned IP.

Go never resolves `.local` itself: it hands those names to libc only when cgo is
available, and FTW builds with `CGO_ENABLED=0`, so a configured `zap.local`
became a unicast DNS query to the site router and failed. That was true on every
base image and every libc. FTW now answers those names itself over multicast
DNS, and every driver transport uses it — Modbus TCP, MQTT (driver and Home
Assistant bridge), HTTP (including TLS-pinned clients), WebSocket and raw TCP.

Resolution happens per dial rather than once at startup, so a device that moves
to a new DHCP lease is found again on the next reconnect without a config edit.
Answers are cached for the record's TTL (clamped to 30–120 s) so reconnect loops
do not flood the LAN, and failures are cached briefly so a device that is still
booting is retried soon. A failed resolution now logs `mDNS resolution failed`
and names the mechanism, instead of surfacing as a generic dial error.

Only `.local` names take this path; literal IPs and ordinary DNS names dial
exactly as before. mDNS needs multicast reachability, which the Linux Compose
topology has via `network_mode: host`. Under `docker-compose.macos.yml` the
container is bridged, so configure devices by IP there.
