---
"ftw": minor
---

`ftw-plant` ships as an independent optional container: the multi-rack plant controller behind the `/v1` loopback contract, with its own Dockerfile, an opt-in (commented) compose service, a `plant.example.yaml`, and a beta→stable release workflow modeled on the optimizer's. Stopping the container is safe by construction — core's plant driver goes stale into its autonomous default and the racks' setpoint leases expire to zero.
