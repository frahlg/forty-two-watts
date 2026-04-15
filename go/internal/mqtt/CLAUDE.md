# mqtt — per-driver MQTT capability built on paho.mqtt.golang

## What it does

Wraps one `paho.Client` per driver so each driver has its own broker connection, its own subscription set, and its own inbound message buffer. Implements `drivers.MQTTCap` (`Subscribe`, `Publish`, `PopMessages`). Auto-reconnect + 5 s connect-retry are on by default; inbound messages are buffered in a slice by the default publish handler and drained when the driver calls `host.mqtt_messages()`.

## Key types

| Type | Purpose |
|---|---|
| `Capability` | Wraps `paho.Client` + an inbound slice guarded by `sync.Mutex`. Implements `drivers.MQTTCap`. |

## Public API surface

- `Dial(host, port, username, password, clientID) (*Capability, error)` — connects, retries for 5 s, fails after 10 s total wait (`client.go:43`).
- `(*Capability).Close()` — disconnects with 250 ms quiesce.
- `(*Capability).Subscribe(topic string) error` — QoS 0, 5 s wait.
- `(*Capability).Publish(topic string, payload []byte) error` — QoS 0, non-retained, 5 s wait.
- `(*Capability).PopMessages() []drivers.MQTTMessage` — atomically drains and returns the inbound buffer.

## How it talks to neighbors

The `../drivers` registry holds an `MQTTFactory` function wired in `cmd/forty-two-watts/main.go:132-134` that calls `Dial(host, port, user, pass, "ftw-"+driverName)` for each driver that has an MQTT config. The returned `*Capability` is bound to the driver's `HostEnv` via `env.WithMQTT(cap)`; from then on the driver's Lua/WASM code calls `host.mqtt_subscribe` / `host.mqtt_publish` / `host.mqtt_messages`, which route through `drivers.MQTTCap`. The HA bridge (`../ha`) creates its own paho client — it does NOT go through this package.

## What to read first

`client.go` — the whole package is a single file. Pay attention to `SetDefaultPublishHandler` (`client.go:32`): every inbound message lands in `cap.incoming` regardless of topic, so make sure your driver only subscribes to topics it actually wants.

## What NOT to do

- **Do NOT reuse a `clientID` across drivers or processes.** Paho + broker behavior: the newer session kicks the older off. Always use the `ftw-<driver_name>` convention from `main.go`.
- **Do NOT share one `Capability` between two drivers.** Each driver gets its own instance so subscriptions don't leak and `PopMessages` doesn't steal from a sibling.
- **Do NOT add per-topic callbacks.** Messages are delivered through the default handler into the buffer, then pulled by the driver's poll loop — that's what keeps the Lua VM single-threaded (no callbacks into gopher-lua from paho goroutines).
- **Do NOT assume messages survive restarts.** QoS is 0 and publish is non-retained; if a driver needs persistence it has to publish retained or use QoS 1/2 itself.
- **Do NOT block the default publish handler.** It holds `cap.mu` while appending — keep the callback the cheap append it already is.
