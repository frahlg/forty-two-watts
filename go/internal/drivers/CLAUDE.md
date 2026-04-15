# drivers — Lua/WASM driver host with capability-gated I/O

## What it does

Spawns one goroutine per device driver, running either a Lua 5.1 script (via `yuin/gopher-lua`) or a WASM module (via `tetratelabs/wazero`). Exposes a fixed lifecycle (`driver_init` → `driver_poll` loop + `driver_command`/`driver_default` → `driver_cleanup`) plus a capability-gated host API (log, emit, MQTT, Modbus, identity). Drivers are FAT: all protocol parsing, state machines, and retries live in the driver; the host only provides I/O and time.

## Key types

| Type | Purpose |
|---|---|
| `Registry` | Owns running drivers, runs per-driver `runLoop`, handles add/remove/reload. |
| `driverRuntime` (unexported interface) | Shared shape over Lua + WASM so `runLoop` is runtime-agnostic (`registry.go:46`). |
| `LuaDriver` | gopher-lua VM bound to one `HostEnv` (`lua.go:47`). |
| `Driver` | wazero module instance bound to one `HostEnv` (`runtime.go:35`). |
| `Runtime` | Shared wazero runtime across WASM drivers (`runtime.go:18`). |
| `HostEnv` | Per-driver context: capabilities, telemetry store, identity (`host.go:43`). |
| `MQTTCap` / `ModbusCap` | Capability interfaces implemented by `../mqtt` and `../modbus` (`host.go:20`, `host.go:35`). |
| `MQTTMessage` | Inbound message `{topic, payload}` drained via `PopMessages` (`host.go:29`). |
| `CatalogEntry` | Metadata scraped from the `DRIVER={…}` block at the top of each `.lua` file (`catalog.go:15`). |

## Public API surface

- `NewRegistry(rt, tel)` + `Add / Remove / Reload / Send / SendDefault / ShutdownAll / Names / Env`.
- `NewRuntime(ctx)` / `Runtime.Load(ctx, path, env)` / `Runtime.LoadBytes` for WASM.
- `NewLuaDriver(path, env)` for Lua.
- `NewHostEnv(name, tel)` + `WithMQTT / WithModbus / SetEndpoint / SetMAC`.
- `HostEnv.Identity() / FullIdentity()` for `state.RegisterDevice` wiring.
- `LoadCatalog(dir)` walks `.lua` files and extracts the DRIVER metadata table.
- ABI constants: `StatusOk/Error/Unsupported`, `LogTrace…LogError`, `ModbusCoil…ModbusInput`, `ABIVersionMajor/Minor` (`abi.go`).

## How it talks to neighbors

`Registry.Add` resolves capabilities via the injected `MQTTFactory` / `ModbusFactory` / `ARPLookup` (wired in `cmd/forty-two-watts/main.go`). MAC resolution comes from `../arp`; endpoint is set from the MQTT/Modbus config. The HostEnv owns a pointer to `../telemetry.Store` — `emitTelemetry` routes structured pv/battery/meter readings through `Store.Update`, `emitMetric` routes scalar diagnostics through `Store.EmitMetric`, and each successful poll records a health tick via `DriverHealthMut`. The Lua backend adapts a `map[string]any` config at the boundary (`registry.go:70`), while WASM passes raw JSON (`runtime.go:248`). See `docs/lua-drivers.md` and `docs/host-api.md`.

## What to read first

1. `registry.go` — `Add`, `runLoop`, `Reload`, and the `driverRuntime` adapter layer.
2. `host.go` — HostEnv + capability interfaces + identity fields.
3. `lua.go` — registration of the `host.*` global and the gopher-lua bridge.
4. `runtime.go` — wazero host module builder, lifecycle dispatch, WASI stdout→slog.
5. `abi.go` — the stable WASM ABI (exports, imports, status codes).
6. `catalog.go` — how the UI discovers available drivers.

## What NOT to do

- **Do NOT reuse an MQTT `clientID` across drivers.** `main.go:133` prefixes with `ftw-<name>` for a reason — brokers disconnect duplicates.
- **Do NOT register a new driver backend by touching `runLoop` directly.** Add a new `driverRuntime` adapter alongside `wasmRuntime` / `luaRuntime` (`registry.go:55-79`) and let `Add` pick it by extension. Lua is the recommended path for new drivers.
- **Do NOT share a wazero module across drivers.** `LoadBytes` closes the existing `host` module each time (`runtime.go:70`) — a shared runtime would need name-prefixed host modules.
- **Do NOT call into a nil capability.** Gate every MQTT/Modbus access with the `env.MQTT != nil` / `env.Modbus != nil` check (the host proxies already do — see `host.go:197+`). Drivers get `ErrNoCapability` back.
- **Do NOT assume ARP lookup succeeds.** Cross-VLAN devices return `("", false)`; identity must fall back to the endpoint hash (`registry.go:119-134`).
- **Do NOT bypass the command channel.** All driver mutations go through `rd.cmdCh` so they serialize with `Poll` on the same goroutine. Calling `Command` directly from another goroutine is a data race against gopher-lua's single-threaded VM.
