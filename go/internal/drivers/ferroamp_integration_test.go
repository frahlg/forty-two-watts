package drivers

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	mqttserver "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/frahlg/forty-two-watts/go/cmd/sim-ferroamp/ferroamp"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// findFerroampWASM walks up from this test file to find drivers-wasm/ferroamp.wasm.
// Skips the test if the file isn't built yet (expects `make wasm` to have run).
func findFerroampWASM(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Dir(file)
	// walk up looking for drivers-wasm/
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "drivers-wasm", "ferroamp.wasm")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("drivers-wasm/ferroamp.wasm not found — run `make wasm` first")
	return ""
}

func pickFreePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil { t.Fatal(err) }
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// ---- Real MQTT capability that talks to an embedded broker ----

type realMQTT struct {
	client    mqtt.Client
	mu        sync.Mutex
	incoming  []MQTTMessage
}

func newRealMQTT(t *testing.T, brokerPort string, clientID string) *realMQTT {
	t.Helper()
	r := &realMQTT{}
	opts := mqtt.NewClientOptions().
		AddBroker("tcp://127.0.0.1:" + brokerPort).
		SetClientID(clientID).
		SetDefaultPublishHandler(func(_ mqtt.Client, m mqtt.Message) {
			r.mu.Lock()
			r.incoming = append(r.incoming, MQTTMessage{
				Topic:   m.Topic(),
				Payload: string(m.Payload()),
			})
			r.mu.Unlock()
		})
	r.client = mqtt.NewClient(opts)
	if tok := r.client.Connect(); tok.WaitTimeout(3*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	return r
}

func (r *realMQTT) Subscribe(topic string) error {
	tok := r.client.Subscribe(topic, 0, nil)
	tok.WaitTimeout(2 * time.Second)
	return tok.Error()
}

func (r *realMQTT) Publish(topic string, payload []byte) error {
	tok := r.client.Publish(topic, 0, false, payload)
	tok.WaitTimeout(2 * time.Second)
	return tok.Error()
}

func (r *realMQTT) PopMessages() []MQTTMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.incoming
	r.incoming = nil
	return out
}

// ---- The actual integration test ----

func TestIntegration_FerroampWasmDriverAgainstSimulator(t *testing.T) {
	wasmPath := findFerroampWASM(t)

	// 1. Start embedded MQTT broker + Ferroamp physics simulator.
	brokerPort := pickFreePort(t)
	s := mqttserver.New(&mqttserver.Options{InlineClient: true})
	if err := s.AddHook(new(auth.AllowHook), nil); err != nil { t.Fatal(err) }
	tcp := listeners.NewTCP(listeners.Config{ID: "t1", Address: "127.0.0.1:" + brokerPort})
	if err := s.AddListener(tcp); err != nil { t.Fatal(err) }
	go func() { _ = s.Serve() }()
	defer s.Close()
	time.Sleep(150 * time.Millisecond)

	cfg := ferroamp.Default()
	cfg.ResponseTauS = 0.3
	cfg.PVPeakW = 2000 // constant so we have stable PV to verify
	sim := ferroamp.New(cfg)

	// Subscribe inline client to route commands into the sim
	var cmdCount atomic.Int32
	_ = s.Subscribe("extapi/control/request", 1, func(_ *mqttserver.Client, _ packets.Subscription, pk packets.Packet) {
		cmdCount.Add(1)
		handleFerroampCommand(s, sim, pk.Payload)
	})

	// Start the physics publish loop
	stopSim := make(chan struct{})
	var simWg sync.WaitGroup
	simWg.Add(1)
	go func() {
		defer simWg.Done()
		tk := time.NewTicker(100 * time.Millisecond)
		defer tk.Stop()
		last := time.Now()
		for {
			select {
			case <-stopSim: return
			case now := <-tk.C:
				dt := now.Sub(last); last = now
				snap := sim.Tick(dt)
				publishFerroampTelemetry(s, snap)
			}
		}
	}()
	defer func() { close(stopSim); simWg.Wait() }()

	// 2. Spin up WASM driver via our runtime, pointing it at the same broker.
	ctx := context.Background()
	rt := NewRuntime(ctx)
	defer rt.Close(ctx)

	tel := telemetry.NewStore()
	env := NewHostEnv("ferroamp", tel).WithMQTT(newRealMQTT(t, brokerPort, "wasm-driver"))

	drv, err := rt.Load(ctx, wasmPath, env)
	if err != nil { t.Fatal(err) }
	defer drv.Cleanup(ctx)

	// 3. Init the driver. It subscribes to extapi/data/* and publishes extapiversion.
	if err := drv.Init(ctx, []byte(`{}`)); err != nil {
		t.Fatalf("driver_init: %v", err)
	}
	// Identity should have been set
	if make, _ := env.Identity(); make != "Ferroamp" {
		t.Errorf("make not set: %q", make)
	}

	// 4. Poll loop: run it a handful of times, giving the sim time to publish
	// and the driver time to consume+emit.
	time.Sleep(400 * time.Millisecond) // let initial messages arrive
	for i := 0; i < 10; i++ {
		if _, err := drv.Poll(ctx); err != nil {
			t.Fatalf("poll: %v", err)
		}
		time.Sleep(150 * time.Millisecond)
	}

	// 5. Verify the driver emitted real telemetry into the store.
	meter := tel.Get("ferroamp", telemetry.DerMeter)
	pv := tel.Get("ferroamp", telemetry.DerPV)
	bat := tel.Get("ferroamp", telemetry.DerBattery)

	if meter == nil { t.Fatal("no meter reading — driver didn't emit?") }
	if pv == nil { t.Fatal("no pv reading") }
	if bat == nil { t.Fatal("no battery reading") }

	t.Logf("meter: grid=%.1fW (smoothed=%.1f)", meter.RawW, meter.SmoothedW)
	t.Logf("pv:    %.1fW (negative = generation)", pv.RawW)
	if bat.SoC != nil {
		t.Logf("bat:   %.1fW SoC=%.3f", bat.RawW, *bat.SoC)
	}

	// PV should be negative (generation) around -2000W (our constant sim value)
	if pv.RawW > -500 {
		t.Errorf("PV should be strongly negative (-2000ish), got %.1f", pv.RawW)
	}
	// Battery SoC should be set (~0.5 default)
	if bat.SoC == nil {
		t.Error("battery SoC not extracted from ESO")
	} else if *bat.SoC < 0.3 || *bat.SoC > 0.7 {
		t.Errorf("SoC out of expected range: %f", *bat.SoC)
	}

	// 6. Issue a command through the driver — should result in MQTT publish to
	//    extapi/control/request, which the simulator's inline subscriber counts.
	beforeCmds := cmdCount.Load()
	if err := drv.Command(ctx, []byte(`{"action":"battery","power_w":-1500}`)); err != nil {
		t.Fatalf("command: %v", err)
	}
	// Give MQTT time to round-trip
	time.Sleep(300 * time.Millisecond)
	if cmdCount.Load() <= beforeCmds {
		t.Errorf("driver command didn't trigger MQTT publish (before=%d, after=%d)",
			beforeCmds, cmdCount.Load())
	}

	// 7. Poll more cycles and verify the battery actually responded to the discharge command
	for i := 0; i < 20; i++ {
		_, _ = drv.Poll(ctx)
		time.Sleep(150 * time.Millisecond)
	}
	bat = tel.Get("ferroamp", telemetry.DerBattery)
	if bat == nil { t.Fatal("no battery reading after dispatch") }
	t.Logf("after -1500W discharge command: bat.w=%.1fW (expect negative, discharging)", bat.RawW)
	if bat.RawW > -200 {
		t.Errorf("expected battery to be discharging after command, got %.1fW", bat.RawW)
	}

	// 8. Driver health should be Ok with many ticks
	h := tel.DriverHealth("ferroamp")
	if h == nil || h.Status != telemetry.StatusOk {
		t.Errorf("driver health: %+v", h)
	}
	if h.TickCount < 10 {
		t.Errorf("expected many ticks, got %d", h.TickCount)
	}
	t.Logf("final health: %s (ticks=%d)", h.Status, h.TickCount)
}

// ---- Helpers: replicate what cmd/sim-ferroamp/main.go does to publish + handle cmds ----
//
// We can't just import main from cmd/sim-ferroamp, so we copy the small bit
// of logic here. The physics itself is the same instance used by the real
// simulator binary — we import the ferroamp package.

func publishFerroampTelemetry(s *mqttserver.Server, snap ferroamp.Snapshot) {
	// Same formatting as cmd/sim-ferroamp/main.go
	ehub := []byte(
		`{"pext":{"L1":"` + ftoa(snap.GridW/3) + `","L2":"` + ftoa(snap.GridW/3) + `","L3":"` + ftoa(snap.GridW/3) + `"},` +
			`"ul":{"L1":"230.0","L2":"230.0","L3":"230.0"},` +
			`"gridfreq":{"val":"50.00"},` +
			`"ppv":{"val":"` + ftoa(snap.PVW) + `"},` +
			`"pbat":{"val":"` + ftoa(-snap.ActualBatW) + `"},` + // Ferroamp: +=discharge
			`"wextconsq3p":{"val":"` + ftoa(snap.ImportWh*3_600_000) + `"},` +
			`"wextprodq3p":{"val":"` + ftoa(snap.ExportWh*3_600_000) + `"}}`)
	_ = s.Publish("extapi/data/ehub", ehub, false, 0)

	eso := []byte(
		`{"soc":{"val":"` + ftoa(snap.SoC*100) + `"},` +
			`"ubat":{"val":"48.0"},"ibat":{"val":"` + ftoa(-snap.ActualBatW/48) + `"},` +
			`"wbatprod":{"val":"` + ftoa(snap.BatDischargeWh*3_600_000) + `"},` +
			`"wbatcons":{"val":"` + ftoa(snap.BatChargeWh*3_600_000) + `"}}`)
	_ = s.Publish("extapi/data/eso", eso, false, 0)
}

func ftoa(f float64) string {
	return strconv.FormatFloat(f, 'f', 3, 64)
}

func handleFerroampCommand(s *mqttserver.Server, sim *ferroamp.Simulator, payload []byte) {
	// Minimal parser that understands {"transId":"x","cmd":{"name":"charge|discharge|auto","arg":N}}
	// We just forward to the sim — doesn't need to be elegant.
	type cmd struct {
		TransID string `json:"transId"`
		Cmd     struct {
			Name string  `json:"name"`
			Arg  float64 `json:"arg"`
		} `json:"cmd"`
	}
	var c cmd
	// lightweight JSON — use encoding/json from stdlib, which we're already linking
	if err := decodeJSON(payload, &c); err != nil { return }
	switch c.Cmd.Name {
	case "charge":
		sim.SetMode(ferroamp.ModeCharge, c.Cmd.Arg)
	case "discharge":
		sim.SetMode(ferroamp.ModeDischarge, c.Cmd.Arg)
	case "auto":
		sim.SetMode(ferroamp.ModeAuto, 0)
	}
	result := []byte(`{"transId":"` + c.TransID + `","status":"ack"}`)
	_ = s.Publish("extapi/result", result, false, 0)
}

func decodeJSON(b []byte, v any) error {
	return jsonUnmarshal(b, v)
}
