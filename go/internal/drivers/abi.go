// Package drivers hosts WASM driver modules via wazero. Each driver is a
// user-supplied Rust/Go/Zig/... program compiled to wasm32-wasip1 that
// imports a fixed set of host functions and exports a fixed lifecycle.
//
// Design principle: drivers are FAT. The host provides only capabilities
// (I/O, time, logging). All protocol parsing, state machines, retries,
// and command translation live in the driver.
//
// This file documents the ABI — the exact function names and signatures
// that drivers must export and that the host provides. See wasm-drivers/
// for reference driver implementations in Rust.
package drivers

// ============================================================================
// Driver exports (must be implemented by every driver)
// ============================================================================
//
//   // Memory management — drivers export an allocator so host can pass strings/bytes in.
//   (func (export "wasm_alloc")   (param i32) (result i32))   // size -> ptr
//   (func (export "wasm_dealloc") (param i32 i32))             // ptr, size
//
//   // Lifecycle — called in order by the host.
//   (func (export "driver_init")     (param i32 i32) (result i32))  // config_json_ptr, len -> status
//   (func (export "driver_poll")     (result i32))                   // -> next_poll_interval_ms
//   (func (export "driver_command")  (param i32 i32) (result i32))   // cmd_json_ptr, len -> status
//   (func (export "driver_default")  (result i32))                   // revert to autonomous
//   (func (export "driver_cleanup")  (result i32))                   // graceful shutdown
//
// Status codes:
//   0 = success
//   1 = generic error
//   2 = not supported
//   (>2 reserved for future)
//
// All byte-slice/string parameters use the "pointer + length in module memory"
// pattern. Drivers allocate their own memory for incoming data via wasm_alloc;
// host writes into that memory then calls the lifecycle function.

// StatusOk is returned by driver functions that succeeded.
const (
	StatusOk         int32 = 0
	StatusError      int32 = 1
	StatusUnsupported int32 = 2
)

// ============================================================================
// Host imports (driver -> host)
// ============================================================================
//
// Namespace: "host".
// All string / byte-slice params are (ptr: i32, len: i32) pairs into module memory.
// Values returned to the driver via output buffers: (out_ptr: i32, out_cap: i32) -> i32
//   returns bytes_written, or -1 on overflow (caller should retry with larger buf).
//
//   host.log(level: i32, msg_ptr, msg_len)
//     level: 0=trace, 1=debug, 2=info, 3=warn, 4=error
//
//   host.millis() -> i64
//     monotonic milliseconds since host startup (for deadlines/timers)
//
//   host.set_poll_interval(ms: i32)
//     hint to host about desired next poll interval (driver can also return from driver_poll)
//
//   host.emit_telemetry(json_ptr, json_len) -> i32
//     json encodes one DER reading: {"type": "meter"|"pv"|"battery", "w": 123,
//     "soc": 0.5, "...": ...} — status 0 = accepted
//
//   host.set_sn(ptr, len) -> i32                 — device serial number
//   host.set_make(ptr, len) -> i32               — device manufacturer
//
//   // MQTT (only available if driver was granted mqtt capability)
//   host.mqtt_subscribe(topic_ptr, topic_len) -> i32
//   host.mqtt_publish(topic_ptr, topic_len, payload_ptr, payload_len) -> i32
//   host.mqtt_poll_messages(out_ptr, out_cap) -> i32
//       writes pending messages as JSON array [{"topic":"...","payload":"..."}]
//       returns bytes written or -1 if buffer too small
//
//   // Modbus (only available if driver was granted modbus capability)
//   host.modbus_read(addr, count, kind, out_ptr, out_cap) -> i32
//       kind: 0=coils, 1=discrete, 2=holding, 3=input
//       writes count × u16 as little-endian bytes to out_ptr
//       returns bytes written (= count*2) or negative error
//   host.modbus_write_single(addr, value) -> i32
//   host.modbus_write_multi(addr, values_ptr, count) -> i32
//       values encoded as count × u16 little-endian bytes
//
// Stable across driver reloads; additions may happen but no removals without
// a bump to the MAJOR_ABI_VERSION (see Version below).

// ABI versioning. Drivers should export an `abi_version` global as i32 (or
// check via their manifest) — runtimes reject loads where MAJOR differs.
const (
	ABIVersionMajor int32 = 1
	ABIVersionMinor int32 = 0
)

// LogLevel codes passed to host.log.
const (
	LogTrace int32 = 0
	LogDebug int32 = 1
	LogInfo  int32 = 2
	LogWarn  int32 = 3
	LogError int32 = 4
)

// ModbusKind codes passed as the 3rd arg of host.modbus_read.
const (
	ModbusCoil     int32 = 0
	ModbusDiscrete int32 = 1
	ModbusHolding  int32 = 2
	ModbusInput    int32 = 3
)
