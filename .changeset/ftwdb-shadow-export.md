---
"ftw": minor
---

Add an optional, read-only FTWDB shadow exporter. It copies settled telemetry,
plans, command outcomes, prices, forecasts, and energy ledger data through a
local Unix socket without SQL writes, migrations, or its own durable outbox. It
resumes only from a durable sidecar cursor, retries the same encoded frame after
an uncertain acknowledgement, and keeps Core independent when the sidecar is
unavailable.
