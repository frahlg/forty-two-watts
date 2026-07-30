---
"ftw": patch
---

Give HTTP drivers a hardware-stable device identity. An HTTP driver's MAC was
never resolved, so a LAN device that had not yet reported a serial fell back to
no identity at all and its persistent state (battery model, RLS twin,
calibration history) was orphaned whenever its address changed. The registry now
ARP-resolves the driver's own configured address, exactly as it already does for
MQTT and Modbus.

Only the driver's own `host`/`url` is probed, never the merged HTTP allowlist,
so vendor cloud endpoints are never treated as identity candidates. ARP lookups
are also now skipped for addresses that provably cannot appear in an ARP table
(loopback, carrier-grade-NAT/tunnel, multicast and broadcast), saving 150 ms of
pointless dialing. Public addresses stay eligible, so devices on a segment with
an ISP-routed block keep the MAC-derived identity they have today.
