;; Minimal WASM "driver" used by drivers_test.go to validate the ABI.
;; Implements: wasm_alloc, driver_init, driver_poll, driver_command, driver_cleanup.
;; Calls host.log + host.emit_telemetry + host.set_poll_interval to exercise the host side.
;;
;; Compile:
;;   wat2wasm hello-driver.wat -o hello-driver.wasm
;;
;; (We hand-assemble the binary in driver_ops_test.go since wat2wasm may not be
;; installed; this file is canonical documentation.)
(module
  (import "host" "log"              (func $log (param i32 i32 i32)))
  (import "host" "set_poll_interval" (func $set_poll (param i32)))
  (import "host" "emit_telemetry"   (func $emit (param i32 i32) (result i32)))

  (memory (export "memory") 1)

  ;; A fixed region for our "message" and "telemetry JSON"
  (data (i32.const 0) "hello-driver")
  (data (i32.const 32) "{\"type\":\"meter\",\"w\":1234.5}")

  ;; Bump allocator — wasm_alloc returns ever-increasing pointers. Not production.
  (global $bump (mut i32) (i32.const 1024))

  (func (export "wasm_alloc") (param $n i32) (result i32)
    (local $p i32)
    (local.set $p (global.get $bump))
    (global.set $bump (i32.add (global.get $bump) (local.get $n)))
    (local.get $p))

  (func (export "wasm_dealloc") (param i32 i32)
    ;; no-op
    nop)

  (func (export "driver_init") (param i32 i32) (result i32)
    ;; log "hello-driver" at info level
    (call $log (i32.const 2) (i32.const 0) (i32.const 12))
    ;; request 1-second poll
    (call $set_poll (i32.const 1000))
    i32.const 0)

  (func (export "driver_poll") (result i32)
    ;; emit one meter reading and return next interval
    ;; JSON is 27 bytes: {"type":"meter","w":1234.5}
    (drop (call $emit (i32.const 32) (i32.const 27)))
    i32.const 1000)

  (func (export "driver_command") (param i32 i32) (result i32)
    i32.const 0)

  (func (export "driver_default") (result i32) i32.const 0)
  (func (export "driver_cleanup") (result i32) i32.const 0)
)
