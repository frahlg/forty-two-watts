# Sourceful Zap

FTW reads Sourceful Zap as the **P1/HAN site meter**. That is the only
role Zap has in FTW. If Zap also lists an inverter, battery or charger,
add that device in FTW with its own driver. Do not attach those resources
to Zap and pull them in through this integration.

The driver is telemetry-only. It talks to Zap's local API and stays up
without Sourceful cloud.

## Configuration

```yaml
drivers:
  - name: sourceful-zap
    lua: drivers/zap.lua
    is_site_meter: true
    capabilities:
      http:
        allowed_hosts: ["zap.local"]
    config:
      host: zap.local
```

Use the Zap's LAN IP in both places when mDNS does not cross the network.

With several meters, `meter_serial` pins the site meter; otherwise the first
P1/HAN device is preferred.

## Data and identity

The driver refreshes Zap devices without restarting and emits the selected
meter's power, phases, voltage/current/frequency and energy totals.

If Zap also lists a PV inverter, battery or charger, the driver logs that
and records an `other_resources` metric. It does not ingest those readings.
Add the matching native driver instead.

The FTW device identity is based on Zap's gateway serial from `/api/crypto`,
with a lower-confidence meter serial fallback for older firmware.

Zap's per-DER `enabled` flag controls its Nova publishing, not whether local
FTW may read the P1/HAN meter.

## Safety

The driver converts every value to FTW's site convention and does not invent
zero when a required reading is absent. Silence lets the watchdog and
stale-site-meter guard act.

`driver_default_mode` performs no write because the driver is read-only.

## Verification

```bash
cd go
go test ./internal/drivers -run 'Zap|zap'
```

## Troubleshooting

- not found: confirm Zap is on Wi-Fi and reachable at
  `http://zap.local/api/system` from the FTW host;
- no meter: inspect Zap's `/api/devices` and pin `meter_serial` when needed;
- inverter, battery or charger listed on Zap: add that device in FTW with
  its own driver. This Zap driver will not read it.
