package drivers

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Runtime holds a shared wazero runtime. Multiple Drivers can share one
// runtime; each has its own module instance.
type Runtime struct {
	r wazero.Runtime
}

// NewRuntime creates a wazero runtime with WASI preview 1 pre-registered.
func NewRuntime(ctx context.Context) *Runtime {
	r := wazero.NewRuntime(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, r)
	return &Runtime{r: r}
}

// Close tears down the runtime and all its modules.
func (r *Runtime) Close(ctx context.Context) error {
	return r.r.Close(ctx)
}

// Driver is one loaded + instantiated WASM module bound to a HostEnv.
type Driver struct {
	runtime *Runtime
	env     *HostEnv

	mod     api.Module
	alloc   api.Function
	dealloc api.Function

	initFn    api.Function
	pollFn    api.Function
	cmdFn     api.Function
	defaultFn api.Function
	cleanupFn api.Function
}

// Load reads a .wasm file from disk, instantiates it, and binds the host
// functions from env.
func (r *Runtime) Load(ctx context.Context, wasmPath string, env *HostEnv) (*Driver, error) {
	bin, err := os.ReadFile(wasmPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", wasmPath, err)
	}
	return r.LoadBytes(ctx, bin, env)
}

// LoadBytes instantiates WASM directly from a byte slice (used by tests).
func (r *Runtime) LoadBytes(ctx context.Context, wasmBytes []byte, env *HostEnv) (*Driver, error) {
	d := &Driver{runtime: r, env: env}

	// Each driver gets its own Runtime instance (see per-driver Load), so we
	// can register a fresh "host" module bound to this env without affecting
	// other drivers. If a shared runtime is desired later, we'd prefix the
	// host module name with the driver name.
	hostModName := "host"
	if existing := r.r.Module(hostModName); existing != nil {
		_ = existing.Close(ctx)
	}
	hostBuilder := r.r.NewHostModuleBuilder(hostModName)

	// ---- Core ----
	hostBuilder.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, level, ptr, n int32) {
			s, _ := readString(m, ptr, n)
			env.log(level, s)
		}).
		Export("log")

	hostBuilder.NewFunctionBuilder().
		WithFunc(func() int64 { return env.millis() }).
		Export("millis")

	hostBuilder.NewFunctionBuilder().
		WithFunc(func(ms int32) { env.setPollInterval(ms) }).
		Export("set_poll_interval")

	hostBuilder.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, ptr, n int32) int32 {
			b, _ := readBytes(m, ptr, n)
			if err := env.emitTelemetry(b); err != nil {
				env.Logger.Warn("emit_telemetry failed", "err", err)
				return StatusError
			}
			return StatusOk
		}).
		Export("emit_telemetry")

	hostBuilder.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, ptr, n int32) int32 {
			s, _ := readString(m, ptr, n)
			env.setSN(s); return StatusOk
		}).
		Export("set_sn")

	hostBuilder.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, ptr, n int32) int32 {
			s, _ := readString(m, ptr, n)
			env.setMake(s); return StatusOk
		}).
		Export("set_make")

	// ---- MQTT ----
	hostBuilder.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, ptr, n int32) int32 {
			s, _ := readString(m, ptr, n)
			if err := env.mqttSubscribe(ctx, s); err != nil {
				env.Logger.Warn("mqtt_subscribe failed", "topic", s, "err", err)
				return StatusError
			}
			return StatusOk
		}).
		Export("mqtt_subscribe")

	hostBuilder.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, topicPtr, topicLen, payPtr, payLen int32) int32 {
			topic, _ := readString(m, topicPtr, topicLen)
			pay, _ := readBytes(m, payPtr, payLen)
			if err := env.mqttPublish(ctx, topic, pay); err != nil {
				env.Logger.Warn("mqtt_publish failed", "topic", topic, "err", err)
				return StatusError
			}
			return StatusOk
		}).
		Export("mqtt_publish")

	hostBuilder.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, outPtr, outCap int32) int32 {
			msgs, err := env.mqttPollMessages()
			if err != nil { return -1 }
			// Encode as compact JSON array
			buf := encodeMessagesJSON(msgs)
			if int32(len(buf)) > outCap { return -1 }
			if !m.Memory().Write(uint32(outPtr), buf) { return -1 }
			return int32(len(buf))
		}).
		Export("mqtt_poll_messages")

	// ---- Modbus ----
	hostBuilder.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, addr, count, kind, outPtr, outCap int32) int32 {
			regs, err := env.modbusRead(uint16(addr), uint16(count), kind)
			if err != nil {
				env.Logger.Warn("modbus_read failed", "addr", addr, "count", count, "err", err)
				return -1
			}
			need := int32(len(regs)) * 2
			if need > outCap { return -1 }
			buf := make([]byte, need)
			for i, r := range regs {
				binary.LittleEndian.PutUint16(buf[i*2:], r)
			}
			if !m.Memory().Write(uint32(outPtr), buf) { return -1 }
			return need
		}).
		Export("modbus_read")

	hostBuilder.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, addr, value int32) int32 {
			if err := env.modbusWriteSingle(uint16(addr), uint16(value)); err != nil {
				env.Logger.Warn("modbus_write_single failed", "addr", addr, "err", err)
				return StatusError
			}
			return StatusOk
		}).
		Export("modbus_write_single")

	hostBuilder.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, addr, valuesPtr, count int32) int32 {
			buf, ok := readBytes(m, valuesPtr, count*2)
			if !ok { return StatusError }
			vals := make([]uint16, count)
			for i := int32(0); i < count; i++ {
				vals[i] = binary.LittleEndian.Uint16(buf[i*2:])
			}
			if err := env.modbusWriteMulti(uint16(addr), vals); err != nil {
				env.Logger.Warn("modbus_write_multi failed", "addr", addr, "err", err)
				return StatusError
			}
			return StatusOk
		}).
		Export("modbus_write_multi")

	if _, err := hostBuilder.Instantiate(ctx); err != nil {
		return nil, fmt.Errorf("instantiate host module: %w", err)
	}

	// Configure module: enable stdout/stderr as logs
	cfg := wazero.NewModuleConfig().
		WithStdout(&slogWriter{logger: env.Logger, level: "info"}).
		WithStderr(&slogWriter{logger: env.Logger, level: "error"}).
		WithName(env.DriverName)
	mod, err := r.r.InstantiateWithConfig(ctx, wasmBytes, cfg)
	if err != nil {
		return nil, fmt.Errorf("instantiate driver: %w", err)
	}
	d.mod = mod

	// Bind exported lifecycle functions
	d.alloc = mod.ExportedFunction("wasm_alloc")
	d.dealloc = mod.ExportedFunction("wasm_dealloc")
	d.initFn = mod.ExportedFunction("driver_init")
	d.pollFn = mod.ExportedFunction("driver_poll")
	d.cmdFn = mod.ExportedFunction("driver_command")
	d.defaultFn = mod.ExportedFunction("driver_default")
	d.cleanupFn = mod.ExportedFunction("driver_cleanup")
	if d.alloc == nil || d.pollFn == nil || d.initFn == nil {
		return nil, errors.New("driver missing required exports (wasm_alloc / driver_init / driver_poll)")
	}
	return d, nil
}

// passBytes allocates module memory via wasm_alloc, writes bytes into it,
// and returns (ptr, len). Caller is responsible for freeing via dealloc if
// the module needs long-term ownership — for short-lived lifecycle calls
// the module can free internally.
func (d *Driver) passBytes(ctx context.Context, data []byte) (int32, int32, error) {
	if d.alloc == nil {
		return 0, 0, errors.New("driver missing wasm_alloc export")
	}
	if len(data) == 0 {
		return 0, 0, nil
	}
	res, err := d.alloc.Call(ctx, uint64(len(data)))
	if err != nil {
		return 0, 0, fmt.Errorf("wasm_alloc: %w", err)
	}
	ptr := int32(res[0])
	if !d.mod.Memory().Write(uint32(ptr), data) {
		return 0, 0, errors.New("write to wasm memory failed")
	}
	return ptr, int32(len(data)), nil
}

// Init calls driver_init with the given config JSON. Returns driver status.
func (d *Driver) Init(ctx context.Context, configJSON []byte) error {
	ptr, n, err := d.passBytes(ctx, configJSON)
	if err != nil { return err }
	res, err := d.initFn.Call(ctx, uint64(uint32(ptr)), uint64(uint32(n)))
	if err != nil {
		return fmt.Errorf("driver_init: %w", err)
	}
	if int32(res[0]) != StatusOk {
		return fmt.Errorf("driver_init returned status %d", int32(res[0]))
	}
	return nil
}

// Poll calls driver_poll; returns the driver's requested next interval.
func (d *Driver) Poll(ctx context.Context) (time.Duration, error) {
	res, err := d.pollFn.Call(ctx)
	if err != nil {
		return 0, fmt.Errorf("driver_poll: %w", err)
	}
	ms := int32(res[0])
	if ms <= 0 {
		return d.env.PollInterval(), nil
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// Command sends a JSON command to the driver.
func (d *Driver) Command(ctx context.Context, cmdJSON []byte) error {
	if d.cmdFn == nil {
		return errors.New("driver does not export driver_command")
	}
	ptr, n, err := d.passBytes(ctx, cmdJSON)
	if err != nil { return err }
	res, err := d.cmdFn.Call(ctx, uint64(uint32(ptr)), uint64(uint32(n)))
	if err != nil {
		return fmt.Errorf("driver_command: %w", err)
	}
	if int32(res[0]) != StatusOk {
		return fmt.Errorf("driver_command status %d", int32(res[0]))
	}
	return nil
}

// DefaultMode calls driver_default — the driver should revert to autonomous behavior.
func (d *Driver) DefaultMode(ctx context.Context) error {
	if d.defaultFn == nil { return nil }
	_, err := d.defaultFn.Call(ctx)
	return err
}

// Cleanup calls driver_cleanup and then closes the module.
func (d *Driver) Cleanup(ctx context.Context) error {
	if d.cleanupFn != nil {
		_, _ = d.cleanupFn.Call(ctx)
	}
	return d.mod.Close(ctx)
}

// Env returns the driver's host environment (for inspection in tests).
func (d *Driver) Env() *HostEnv { return d.env }

// ---- Helpers ----

func readBytes(m api.Module, ptr, n int32) ([]byte, bool) {
	if n == 0 { return nil, true }
	b, ok := m.Memory().Read(uint32(ptr), uint32(n))
	if !ok { return nil, false }
	// Copy because wazero memory may be reused
	out := make([]byte, len(b))
	copy(out, b)
	return out, true
}

func readString(m api.Module, ptr, n int32) (string, bool) {
	b, ok := readBytes(m, ptr, n)
	if !ok { return "", false }
	return string(b), true
}

func encodeMessagesJSON(msgs []MQTTMessage) []byte {
	// Small hand-rolled JSON to avoid unnecessary allocation — we control the structure.
	// Falls back to encoding/json on edge cases.
	// Simplest: just use encoding/json.
	if len(msgs) == 0 {
		return []byte("[]")
	}
	buf, err := marshalMessages(msgs)
	if err != nil {
		return []byte("[]")
	}
	return buf
}

// slogWriter routes WASI stdout/stderr lines into the driver's slog logger.
type slogWriter struct {
	logger interface{ Info(msg string, args ...any); Error(msg string, args ...any) }
	level  string
}

func (w *slogWriter) Write(p []byte) (int, error) {
	s := string(p)
	if len(s) > 0 && s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	if w.level == "error" {
		w.logger.Error("wasm stderr", "msg", s)
	} else {
		w.logger.Info("wasm stdout", "msg", s)
	}
	return len(p), nil
}
