# Architecture

FTW is a local-first home energy management system. Its architecture has
three explicit modules: **core**, **drivers**, and **optimizer**. Core is the
safety boundary. Drivers translate hardware protocols. The optimizer proposes
plans. A failure or upgrade outside core must never stop local measurement or
make dispatch unsafe.

## Module boundaries

| Module | Source | Runtime | Responsibility |
|---|---|---|---|
| Core | [`go/cmd/ftw`](../go/cmd/ftw), [`go/internal`](../go/internal), [`web`](../web) | One Go binary | Configuration, telemetry, state, API/UI, safety, control and fallback planning |
| Drivers | Editable source in [`srcfl/device-drivers`](https://github.com/srcfl/device-drivers); bundled recovery in `drivers/*.lua`; host in [`go/internal/drivers`](../go/internal/drivers) | One sandboxed Lua VM per configured device | Vendor protocol, sign conversion and device commands |
| Optimizer | [`optimizer`](../optimizer), contract in [`go/internal/mpc`](../go/internal/mpc) | Optional Python service/process | Solve the long-horizon mathematical plan |

Core can run without the optimizer. Hardware cannot be accessed without a
driver, but one failed driver is isolated from the others. Optional
integrations such as Home Assistant, CalDAV, notifications and Nova attach at
core's API, state or telemetry boundaries; they do not own dispatch safety.

A future module belongs outside core only when it has:

- a small, explicit and versioned contract;
- independent failure and update semantics;
- no authority to bypass core's validation or safety limits;
- a useful fallback or a cleanly unavailable state.

## Power convention

Above the driver boundary, positive power flows into the site and negative
power flows out. Examples: grid import is positive, PV production is negative,
battery charge is positive and battery discharge is negative.

Only drivers convert vendor signs. Core, storage, API, UI and optimizer all use
the site convention. See [site-convention.md](site-convention.md) before
changing power math.

## Core

[`go/cmd/ftw/main.go`](../go/cmd/ftw/main.go) is the composition root. It wires
configuration, driver registry, telemetry, persistent state, control, planning,
API and integrations.
Packages under [`go/internal`](../go/internal) should stay cohesive and communicate through
narrow Go interfaces or data types instead of reaching into one another's
storage.

The main flow is:

```text
device
  ↕ vendor protocol
Lua driver                 optional optimizer
  ↕ site-convention data       ↓ proposed trajectory
telemetry → control/planner → core validation and safety → driver command
     ↘ SQLite/history       ↘ API/UI and integrations
```

The in-memory telemetry store owns latest readings and driver health. SQLite
owns durable configuration state, history, forecasts, prices, device identity
and learned model state. Database access stays in
[`go/internal/state`](../go/internal/state).

The control loop computes a site target, allocates it across capable assets,
applies safety constraints, then sends commands through the driver registry.
Planner output is an input to that loop, never a direct device command.

## Drivers

The public `srcfl/device-drivers` repo owns editable driver source, versions,
contracts, tests and FTW's signed release channel. FTW downloads only an
explicitly selected, content-addressed Lua asset after it verifies the signed
manifest. It never runs raw code from the repository branch. Device Support
may later consume an exact public commit for other products or a higher support
level.

Each Lua artifact still contains its own `DRIVER` metadata and implements the
FTW lifecycle. [`go/internal/drivers/lua.go`](../go/internal/drivers/lua.go) is
the source of truth for FTW's
host API and capability sandbox. Network and protocol capabilities must be
granted in configuration.

Drivers are the only hardware-specific layer. They must:

- translate telemetry and commands to the site sign convention;
- report stable make and serial identity when available;
- implement a safe autonomous default mode;
- avoid policy decisions that belong in core;
- remain independently testable and hot-editable.

Bundled drivers provide the offline recovery set. A signed distribution index
is discovery only; FTW independently verifies the selected package and
artifact, while activation remains explicit and atomic. See
[writing-a-driver.md](writing-a-driver.md) and
[device-repository.md](device-repository.md).

## Optimizer

The Python/CVXPY optimizer is optional and separately deployable. Core sends a
versioned planning request and accepts only a complete, valid trajectory. The
optimizer does not read hardware or issue commands.

If the socket/process fails, times out or returns invalid output, core falls
back to its Go planner. Optimizer deployment and dependency churn therefore do
not enlarge the safety-critical runtime.

## Versioning a module contract

Drivers and the optimizer release on their own schedules, so core cannot assume
the version on the other side of either contract. Both use the same rule.

Each side declares the **window** of contract versions it speaks — core in
[`go/internal/components`](../go/internal/components) and
[`go/internal/optimizercontract`](../go/internal/optimizercontract), a driver in its
`host_api_min`/`host_api_max` metadata, the optimizer in its handshake reply.
An overlap of one version is enough. Declaring a single version means a window
of one.

Grow the contract by adding **features** to the handshake, not by bumping the
version. A feature an old peer does not advertise costs nothing — core simply
does not ask it for what it cannot do — while a version bump makes every peer
outside the new window incompatible at once. That is the mistake the `champion`
requirement made: it landed in core before any optimizer image advertised it,
and every site that had not updated the optimizer silently fell back to the Go
planner.

When the framing or the request shape genuinely changes, bump the version and
**widen** the window rather than moving it, so sites that have not updated the
module keep working.

## Failure boundaries

Core enforces these invariants regardless of mode or module:

- stale site-meter data stops dispatch;
- stale or failed drivers are put in their autonomous default mode;
- configured power, fuse, SoC and slew limits are enforced after planning;
- incomplete or invalid optimizer output is rejected;
- external integrations fail soft and cannot block the control loop;
- persistent writes and activated driver artifacts are atomic.

The concise safety rationale is in [safety.md](safety.md). Tests next to the
relevant code are the detailed executable specification.

## Configuration and interfaces

[`config.example.yaml`](../config.example.yaml) and the structs plus validation
in [`go/internal/config`](../go/internal/config) define the configuration
schema. The handlers registered in
[`go/internal/api/api.go`](../go/internal/api/api.go) define the HTTP surface. Driver metadata defines
the device catalog. These sources replace manually duplicated reference docs.

Some startup bindings cannot be hot-reloaded, including state paths, API
listener and selected integration transports. Normal device and control
configuration is reloaded through
[`go/internal/configreload`](../go/internal/configreload).

## Remote access boundary

Remote access is the FTW app and nothing else. The box holds one outbound WSS
connection to `wss://relay.ftw.energy`; there is no inbound listener, no
NAT-traversal layer, no cloud account and no browser-managed site directory.
See [ADR 0006](adr/0006-app-uplink.md) for why Home Link was removed rather
than kept alongside it.

The path is four packages:

- [`go/internal/appenroll`](../go/internal/appenroll) — the Noise static key,
  the rotatable rendezvous secret, the single-use pairing code, the QR payload
  and the list of app keys that have been let in. All of it is boot-time
  material stored beside `nova.key`, not in SQLite;
- [`go/internal/appwire`](../go/internal/appwire) — frames, the Noise IK
  responder and the transport;
- [`go/internal/appproto`](../go/internal/appproto) — the message layer;
- [`go/internal/appuplink`](../go/internal/appuplink) — the outbound
  connection, the per-epoch rendezvous handle and session demultiplexing.

The properties that matter:

- the app's trust anchor arrives optically. The QR payload is a URL fragment,
  which is never sent in an HTTP request, so a hostile or compelled
  `app.ftw.energy` can deny service but cannot impersonate a box;
- the box is not a WebAuthn relying party and stores no user credential. It
  authorises a first connection by the pairing code inside the Noise handshake
  and later ones by the app's pinned static key;
- the relay forwards encrypted frames and holds no keys. The handle the box
  joins under is derived per epoch from the rendezvous secret, so it rotates
  hourly and gives the relay operator nothing stable to key a household on;
- the machine identity in [`go/internal/gatewayidentity`](../go/internal/gatewayidentity)
  is separate and does not authenticate this connection. It resolves to a
  hardware-protected P-256 key where the hardware exists and a bound software
  key with the same public-key and signature wire contract otherwise, with a
  deterministic adjective-color-animal display name derived from the stable
  18-hex gateway ID. The name is a label, never an authenticator;
- authority is unchanged by the transport. A command carries an expiry and
  preconditions, and core revalidates against fresh state before acting. Core
  remains authoritative for telemetry freshness, validation, clamps, planning
  and dispatch. An unavailable relay leaves local control and local recovery
  intact.

## Fleet ping

The box's other outbound path to Sourceful, and the only one that carries
readable content: once a day it posts its FTW version and channel, the driver
types in use, a battery-capacity bucket, its price zone and an install-age
bucket. It answers how many boxes exist and what they run, which is what sizes
engineering bets.

It goes straight to the endpoint over HTTPS and never through the relay,
because the relay's claim is that it holds nothing and routing a readable
message through it would make that false.

The constraint is the one above: Sourceful must not be able to follow a
household. So the message carries no gateway ID, no key, no serial, no site
name, no counter and no timestamp — nothing in it says which box sent it —
values are bucketed rather than reported, the version travels only when it is a
release tag so a developer's build reports as unknown, and the send time is
drawn fresh each day rather than sitting in one slot.

Driver names get their own rule, because a driver file's name is whatever the
thing that installed it called it, and a household can install its own. A name
travels only if it is on one of two lists, and neither list is the contents of
a directory:

- the drivers this build ships with, generated from
  `drivers/BUNDLED_SOURCE.json` into `fleetping.ShippedCatalogue` when the
  binary is built. Every box on a release carries the same list, and nothing
  on a running box can add to it;
- the box's own install history, asked per file: does the row `driverrepo`
  wrote when it installed this exact filename say the manifest verified
  against FTW's own signing key — the one compiled into the binary?

The first list used to be a scan of the directory the bundled drivers sit in,
which was wrong, because `device_repository.root_dir` says where installed
drivers are kept and a config can point it inside that directory. Discovering
the shipped catalogue at boot is asking the config what shipped; fixing it at
build time is not asking. The same reasoning is why the second list is asked
per file. The active directory is still listed, but only to drop records whose
file has gone: a listing can take a name off that list and never put one on it,
so where `root_dir` points does not matter, and an install record is not
something a config writes.

Everything else reports as `other`: a driver somebody wrote, a file copied into
place, a file renamed after it was installed, one from any repository but FTW's
however carefully it signs its own manifests, and one that was installed before
FTW started recording where drivers come from. The last two are what this
costs: a driver another publisher ships is counted but not named, and a box's
existing drivers go unnamed until they are next installed.

The rule is deliberately not asked of the config. The config belongs to the
household and can be rewritten after the fact — an entry can claim the id the
binary pins for the beta channel, be switched off, be deleted outright while
the file it installed stays where it is, or move `root_dir` under the bundled
drivers. What happened during the install is not theirs to rewrite, so that is
what the box records and what the ping reads. This is a rule about names, not
about code: it stops nobody from running the drivers they like, and the one way
left through it is writing an FTW-signed row into the box's own `state.db` by
hand, which this design does not claim to stop.

Two things this does not fix, and both are said on the Settings screen rather
than only here. The fields still describe a household, so the payload remains a
quasi-identifier: a beta box in a small price zone with a big battery may be
the only one like it, and coarse buckets with a small field set are what keep
that population large rather than a proof that it is. And the endpoint sees the
source IP. The design makes the payload useless as an identifier, not the
connection anonymous.

A failed send is forgotten, never retried. Settings → Fleet ping renders the
exact payload from the same call the sender uses, so the claim is checkable
rather than promised. See [`go/internal/fleetping`](../go/internal/fleetping).

## Releases

There are two channels:

- `beta`: every new release candidate, used for real-site validation;
- `stable`: promotion of the exact commit already published and tested as beta.

Core, Optimizer and signed Drivers may release independently, but all use the
same beta-to-stable progression. Core and its privileged updater remain a
paired control plane; optional components negotiate compatibility with Core.
There is no edge channel. See [self-update.md](self-update.md).

## Start reading

1. [site-convention.md](site-convention.md)
2. this document
3. [`go/cmd/ftw/main.go`](../go/cmd/ftw/main.go)
4. the package or driver being changed and its colocated tests
5. [writing-a-driver.md](writing-a-driver.md) for hardware support
6. [operations.md](operations.md) for deployment and recovery
