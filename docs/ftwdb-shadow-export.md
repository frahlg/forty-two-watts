# FTWDB shadow export

`ftw-shadow-export` is an optional process that copies settled FTW data to a
local FTWDB sidecar. FTW Core remains authoritative. If the exporter or
sidecar stops, control, telemetry storage, the API, and the app keep running.
The feature is off when the exporter process is not running.

The exporter opens `state.db` and the optional sibling `cache.db` in SQLite
read-only and query-only modes. It loads the existing `nova.key` with
`nova.LoadExistingIdentity`; it never creates or changes that key. It does not
run SQL writes, migrations, healing, or compaction, and it does not keep its
own durable cursor or outbox. SQLite may still maintain shared WAL state while
Core writes. Measure the full storage write count on each target box.

## Data and durability

The fast stream exports site power, long telemetry samples, planner
diagnostics, driver command results, prices, and forecasts. The ledger stream
exports energy ledger entries. Each stream derives its own stable source ID
from the FTW public key and uses its own FTWDB durable cursor. One process runs
the two streams in turn, so no more than one client owns the Unix socket.

For each batch the exporter:

1. reads the sidecar's durable watermark;
2. reads a bounded source window older than `-settle-delay`;
3. closes the SQLite read transaction;
4. maps and encodes one commit frame;
5. keeps those exact bytes in memory until FTWDB reports that sequence as
   durable.

An accepted but non-durable response does not advance the source cursor. If a
connection drops after a send, the exporter reconnects and checks the durable
watermark. It either drops the in-memory retry when FTWDB proves it durable or
resends the same bytes and commit ID. Retry delay grows to `-retry-max`. A
degraded or unavailable health response pauses reads and commits.

If the exporter itself restarts while FTWDB reports an accepted cursor ahead
of its durable cursor, it asks FTWDB to flush through the accepted cursor. It
does not reread or remap mutable SQLite rows. Only a durable flush proof moves
the source cursor. The current sidecar uses its safe always-durable mode, but
this path also keeps grouped durability safe for a later release.

The sidecar currently syncs every durable batch. The default fast poll is 30
seconds and the command rejects values below 10 seconds. A shorter interval
would cause more `fsync` calls and more storage writes. The ledger query lacks
an index in the current Core schema, so its cursor and five-minute minimum poll
stay separate from the fast stream. The default batch limit is 4096 source
rows. A larger group that shares one source millisecond cannot be split across
cursor commits and stops with a clear error instead of dropping part of it.

The v1 source cursor is the greatest source timestamp in a settled batch. It
does not yet form complete change-data capture: a late insert, delete, clock
rollback, or upsert at or behind a durable timestamp can escape the next
`> cursor` read. Keep the exporter off for beta control or production reads
until Core supplies a monotonic writer-owned change sequence in the same
transaction as each source change. An overlap scan or timestamp tie-breaker
does not close this gap.

Current rows do not carry every stable link needed for a full hardware audit.
The exporter uses `devices.device_id` only when the current driver-name match
is unique. A missing or reused name stays a `configured_driver`, not claimed
hardware. Core does not keep old driver-name aliases, so a row written before
a rename may still fall back to its name-based identity. Energy assets keep
their own type and use a stable device as parent only when that match is safe.

Each planner slot has a decision run with its reason and EMS mode, and its plan
points cite that decision. `driver_command_results` still lack a writer-saved
`plan_id` and slot-decision ID. The exporter therefore records the hardware
outcome but does not claim that a given candidate plan or decision caused it.
Core must write those IDs with the command result before v1 can prove that
causal link.

## Run it

Build and install the separate binary without changing the Core binary:

```sh
cd go
go build -o ../bin/ftw-shadow-export ./cmd/ftw-shadow-export
```

Start the FTWDB sidecar first, then run:

```sh
./bin/ftw-shadow-export \
  -state /var/lib/ftw/state.db \
  -identity-key /var/lib/ftw/nova.key \
  -socket /run/ftwdb-shadow/ftwdb-shadow.sock \
  -stream all \
  -backfill 24h \
  -poll 30s \
  -ledger-poll 5m \
  -settle-delay 5s
```

`-once` exports one fixed settled view and exits. `-stream fast` and `-stream
ledger` help with checks; production should use one `-stream all` process so
the two streams cannot compete for the single-client sidecar.

[`deploy/ftw-shadow-export.service`](../deploy/ftw-shadow-export.service) is a
hardened systemd example. It expects both processes to run as the `ftw` user,
which matches the sidecar peer-UID check. Review paths and systemd support on
each target before enabling it. Its mount rules keep the databases, WAL files,
identity key, and state directory read-only. Only existing SQLite `-shm` files
receive write access for live WAL reads.

## Produce reconcile input

`-dump-dir` skips the socket and writes each exact commit request frame as
lowercase hex plus a final newline for one fixed backfill view. It reads the
source through the same read-only path and publishes each `.hex` file with mode
`0600` in a real, current-user-owned directory that grants no group or other
access. Secure dumps fail closed on Windows because v1 cannot prove the Unix
owner and directory-sync rules there:

```sh
./bin/ftw-shadow-export \
  -state /var/lib/ftw/state.db \
  -identity-key /var/lib/ftw/nova.key \
  -stream all \
  -backfill 24h \
  -dump-dir /var/lib/ftw-shadow-check
```

The FTWDB reconcile command can compare those expected frames with a stopped
or copied shadow store. A repeated dump accepts an existing byte-identical
file and refuses to replace different data.

```sh
ftwdb-shadow-reconcile /var/lib/ftwdb-shadow /var/lib/ftw-shadow-check/*.hex
```

The `FTWDB shadow contract` workflow checks the frozen fixtures and runs a real
exchange against a pinned FTWDB commit whenever this adapter changes. Update
that pin only with a reviewed FTWDB contract change. Run the same gate from
`go/` before a paired release. It starts that exact sidecar binary, exports
from a real migrated FTW store with an active WAL, and requires a durable
watermark:

```sh
FTWDB_SHADOW_BIN=/path/to/ftwdb-shadow \
  go test ./cmd/ftw-shadow-export -run TestRustSidecarInterop -count=1
```

## Beta checks

Before enabling beta boxes, verify all of these on the target hardware:

- FTW control timing does not change with the sidecar absent, stalled, full,
  corrupt, or killed.
- SIGTERM stops the exporter and sidecar within their service limits.
- A power cut after send and before acknowledgement resumes from the durable
  watermark without a gap or duplicate data.
- Late rows, same-timestamp upserts, deletes, and a wall-clock rollback either
  pass a writer-sequence test or keep the exporter disabled.
- Renamed and reused driver names preserve the expected stable device identity,
  and command results carry writer-saved plan and decision IDs before any
  plan-to-hardware audit claim is enabled.
- Reconcile reports matching source sequences, commit IDs, records, and
  points for fast and ledger streams.
- At least 72 hours of storage counters show the added bytes and `fsync` rate
  at 30-second and five-minute polls.
- A schema from the oldest supported beta release either exports correctly or
  fails with a clear error while Core keeps running.
- The service user can read the state and key, and another UID cannot connect
  to the sidecar socket.

Keep the exporter disabled on a box that has not passed these checks.
