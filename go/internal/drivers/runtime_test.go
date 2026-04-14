package drivers

import (
	"context"
	_ "embed"
	"errors"
	"sync"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

//go:embed testdata/hello-driver.wasm
var helloDriver []byte

// ---- Host capability stubs used by driver tests ----

type fakeMQTT struct {
	mu         sync.Mutex
	subs       []string
	published  []struct{ Topic string; Payload []byte }
	incoming   []MQTTMessage
	subErr     error
	publishErr error
}

func (f *fakeMQTT) Subscribe(topic string) error {
	f.mu.Lock(); defer f.mu.Unlock()
	f.subs = append(f.subs, topic)
	return f.subErr
}
func (f *fakeMQTT) Publish(topic string, payload []byte) error {
	f.mu.Lock(); defer f.mu.Unlock()
	f.published = append(f.published, struct{ Topic string; Payload []byte }{topic, append([]byte{}, payload...)})
	return f.publishErr
}
func (f *fakeMQTT) PopMessages() []MQTTMessage {
	f.mu.Lock(); defer f.mu.Unlock()
	out := f.incoming
	f.incoming = nil
	return out
}
func (f *fakeMQTT) AddIncoming(m MQTTMessage) {
	f.mu.Lock(); defer f.mu.Unlock()
	f.incoming = append(f.incoming, m)
}

type fakeModbus struct {
	mu         sync.Mutex
	values     map[uint16]uint16
	readCalls  []struct{ Addr, Count uint16; Kind int32 }
	writeCalls []struct{ Addr uint16; Values []uint16 }
	readErr    error
}

func newFakeModbus() *fakeModbus {
	return &fakeModbus{values: map[uint16]uint16{}}
}
func (f *fakeModbus) Read(addr, count uint16, kind int32) ([]uint16, error) {
	f.mu.Lock(); defer f.mu.Unlock()
	f.readCalls = append(f.readCalls, struct{ Addr, Count uint16; Kind int32 }{addr, count, kind})
	if f.readErr != nil { return nil, f.readErr }
	out := make([]uint16, count)
	for i := uint16(0); i < count; i++ { out[i] = f.values[addr+i] }
	return out, nil
}
func (f *fakeModbus) WriteSingle(addr, value uint16) error {
	f.mu.Lock(); defer f.mu.Unlock()
	f.writeCalls = append(f.writeCalls, struct{ Addr uint16; Values []uint16 }{addr, []uint16{value}})
	f.values[addr] = value
	return nil
}
func (f *fakeModbus) WriteMulti(addr uint16, values []uint16) error {
	f.mu.Lock(); defer f.mu.Unlock()
	f.writeCalls = append(f.writeCalls, struct{ Addr uint16; Values []uint16 }{addr, append([]uint16{}, values...)})
	for i, v := range values { f.values[addr+uint16(i)] = v }
	return nil
}

// ---- Runtime lifecycle tests ----

func TestLoadHelloDriver(t *testing.T) {
	ctx := context.Background()
	rt := NewRuntime(ctx)
	defer rt.Close(ctx)

	tel := telemetry.NewStore()
	env := NewHostEnv("hello", tel)
	drv, err := rt.LoadBytes(ctx, helloDriver, env)
	if err != nil {
		t.Fatal(err)
	}
	defer drv.Cleanup(ctx)

	// init
	if err := drv.Init(ctx, []byte(`{}`)); err != nil {
		t.Fatalf("init: %v", err)
	}
	// driver_init should have called host.set_poll_interval(1000)
	if env.PollInterval().Milliseconds() != 1000 {
		t.Errorf("expected poll interval 1000ms, got %v", env.PollInterval())
	}

	// poll — should emit telemetry and return 1000ms
	next, err := drv.Poll(ctx)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if next.Milliseconds() != 1000 {
		t.Errorf("expected next=1000ms, got %v", next)
	}

	// Verify telemetry made it into the store
	r := tel.Get("hello", telemetry.DerMeter)
	if r == nil {
		t.Fatal("no meter reading recorded")
	}
	if r.RawW != 1234.5 {
		t.Errorf("meter reading W: got %f, want 1234.5", r.RawW)
	}

	// Driver health should reflect the successful emit
	h := tel.DriverHealth("hello")
	if h == nil || h.Status != telemetry.StatusOk {
		t.Errorf("driver health: %+v", h)
	}
	if h.TickCount == 0 {
		t.Error("tick count should have advanced")
	}

	// command — no-op in hello-driver but should not error
	if err := drv.Command(ctx, []byte(`{"test":true}`)); err != nil {
		t.Errorf("command: %v", err)
	}
}

func TestMissingExportRejected(t *testing.T) {
	// Minimal WASM with memory but no wasm_alloc / driver_init (empty module)
	empty := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, // magic + version
	}
	ctx := context.Background()
	rt := NewRuntime(ctx)
	defer rt.Close(ctx)
	env := NewHostEnv("empty", telemetry.NewStore())
	_, err := rt.LoadBytes(ctx, empty, env)
	if err == nil {
		t.Fatal("expected error loading empty module (missing exports)")
	}
}

// ---- Host MQTT capability ----

func TestMQTTCapabilityDeniedByDefault(t *testing.T) {
	env := NewHostEnv("x", telemetry.NewStore())
	if err := env.mqttSubscribe(context.Background(), "t"); !errors.Is(err, ErrNoCapability) {
		t.Errorf("expected ErrNoCapability, got %v", err)
	}
	if err := env.mqttPublish(context.Background(), "t", nil); !errors.Is(err, ErrNoCapability) {
		t.Errorf("expected ErrNoCapability, got %v", err)
	}
}

func TestMQTTCapabilityGranted(t *testing.T) {
	m := &fakeMQTT{}
	env := NewHostEnv("x", telemetry.NewStore()).WithMQTT(m)
	if err := env.mqttSubscribe(context.Background(), "topic/a"); err != nil {
		t.Fatal(err)
	}
	if err := env.mqttPublish(context.Background(), "topic/b", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if len(m.subs) != 1 || m.subs[0] != "topic/a" {
		t.Errorf("subs: %v", m.subs)
	}
	if len(m.published) != 1 || string(m.published[0].Payload) != "hi" {
		t.Errorf("published: %v", m.published)
	}
}

// ---- Host Modbus capability ----

func TestModbusCapabilityDeniedByDefault(t *testing.T) {
	env := NewHostEnv("x", telemetry.NewStore())
	if _, err := env.modbusRead(100, 2, ModbusHolding); !errors.Is(err, ErrNoCapability) {
		t.Errorf("expected denial, got %v", err)
	}
	if err := env.modbusWriteSingle(100, 1); !errors.Is(err, ErrNoCapability) {
		t.Errorf("expected denial, got %v", err)
	}
}

func TestModbusCapabilityReadWrite(t *testing.T) {
	m := newFakeModbus()
	m.values[100] = 42
	env := NewHostEnv("x", telemetry.NewStore()).WithModbus(m)

	regs, err := env.modbusRead(100, 1, ModbusHolding)
	if err != nil { t.Fatal(err) }
	if regs[0] != 42 {
		t.Errorf("read: got %d", regs[0])
	}

	if err := env.modbusWriteSingle(200, 99); err != nil { t.Fatal(err) }
	if m.values[200] != 99 {
		t.Error("write_single did not land")
	}

	if err := env.modbusWriteMulti(300, []uint16{10, 20, 30}); err != nil { t.Fatal(err) }
	if m.values[300] != 10 || m.values[301] != 20 || m.values[302] != 30 {
		t.Errorf("write_multi values: %v", m.values)
	}
}

// ---- emit_telemetry decoding ----

func TestEmitTelemetryMeter(t *testing.T) {
	tel := telemetry.NewStore()
	env := NewHostEnv("x", tel)
	err := env.emitTelemetry([]byte(`{"type":"meter","w":1500.5}`))
	if err != nil { t.Fatal(err) }
	r := tel.Get("x", telemetry.DerMeter)
	if r == nil || r.RawW != 1500.5 {
		t.Fatalf("meter reading: %+v", r)
	}
	h := tel.DriverHealth("x")
	if h.Status != telemetry.StatusOk || h.TickCount == 0 {
		t.Errorf("health: %+v", h)
	}
}

func TestEmitTelemetryBatteryWithSoC(t *testing.T) {
	tel := telemetry.NewStore()
	env := NewHostEnv("x", tel)
	err := env.emitTelemetry([]byte(`{"type":"battery","w":-800,"soc":0.42}`))
	if err != nil { t.Fatal(err) }
	r := tel.Get("x", telemetry.DerBattery)
	if r == nil {
		t.Fatal("no battery reading")
	}
	if r.SoC == nil || *r.SoC != 0.42 {
		t.Errorf("SoC: %v", r.SoC)
	}
	if r.RawW != -800 {
		t.Errorf("W: %f", r.RawW)
	}
}

func TestEmitTelemetryBadJSON(t *testing.T) {
	env := NewHostEnv("x", telemetry.NewStore())
	if err := env.emitTelemetry([]byte(`not json`)); err == nil {
		t.Error("expected parse error")
	}
}

func TestEmitTelemetryUnknownType(t *testing.T) {
	env := NewHostEnv("x", telemetry.NewStore())
	if err := env.emitTelemetry([]byte(`{"type":"lightsaber","w":1}`)); err == nil {
		t.Error("expected unknown type error")
	}
}

// ---- Identity ----

func TestSetMakeAndSN(t *testing.T) {
	env := NewHostEnv("x", telemetry.NewStore())
	env.setMake("Ferroamp")
	env.setSN("FA-EXT-12345")
	m, sn := env.Identity()
	if m != "Ferroamp" || sn != "FA-EXT-12345" {
		t.Errorf("identity: %q %q", m, sn)
	}
}

// ---- Poll interval ----

func TestPollIntervalDefault(t *testing.T) {
	env := NewHostEnv("x", telemetry.NewStore())
	if env.PollInterval().Milliseconds() != 5000 {
		t.Errorf("default: %v", env.PollInterval())
	}
}

func TestSetPollInterval(t *testing.T) {
	env := NewHostEnv("x", telemetry.NewStore())
	env.setPollInterval(250)
	if env.PollInterval().Milliseconds() != 250 {
		t.Errorf("after set: %v", env.PollInterval())
	}
}

// ---- millis monotonic ----

func TestMillisMonotonic(t *testing.T) {
	env := NewHostEnv("x", telemetry.NewStore())
	m1 := env.millis()
	m2 := env.millis()
	if m2 < m1 {
		t.Errorf("millis went backwards: %d → %d", m1, m2)
	}
}
