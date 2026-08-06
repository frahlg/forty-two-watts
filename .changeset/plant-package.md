---
"ftw": minor
---

New `go/internal/plant` package: N PCS/rack units presented as one logical battery. Per-unit Modbus polling with offline detection, an SoC-balancing allocator (proportional to direction headroom, biased toward fleet balance, full/empty/faulted units excluded, residual redistribution so the written setpoints always sum to the clamped aggregate), setpoint leases whose expiry ramps every unit to zero, and a versioned `/v1` HTTP contract (`status` + `setpoint`) for the upcoming `ftw_plant` driver. Aggregate reporting counts only reachable units so headroom is never overstated.
