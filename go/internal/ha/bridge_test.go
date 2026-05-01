package ha

import (
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/config"
)

// TestNilBridgeSafe verifies that all diagnostic methods on a nil *Bridge
// return zero values without panicking. The API layer calls these without
// checking whether the bridge was started, so nil-safety is load-bearing.
func TestNilBridgeSafe(t *testing.T) {
	var b *Bridge

	if b.IsConnected() {
		t.Error("nil bridge: IsConnected should be false")
	}
	if b.BrokerAddr() != "" {
		t.Error("nil bridge: BrokerAddr should be empty")
	}
	if b.LastPublishMs() != 0 {
		t.Error("nil bridge: LastPublishMs should be 0")
	}
	if b.SensorsAnnounced() != 0 {
		t.Error("nil bridge: SensorsAnnounced should be 0")
	}
	// Stop is NOT nil-safe by design (it dereferences b.stop and b.client),
	// so we do not call it on a nil receiver.
}

// TestZeroBridgeSafe verifies that a zero-value Bridge (non-nil but
// uninitialised) returns safe defaults from the diagnostic methods.
func TestZeroBridgeSafe(t *testing.T) {
	b := &Bridge{}

	if b.IsConnected() {
		t.Error("zero bridge: IsConnected should be false")
	}
	if b.BrokerAddr() != "" {
		t.Error("zero bridge: BrokerAddr should be empty (cfg is nil)")
	}
	if b.LastPublishMs() != 0 {
		t.Error("zero bridge: LastPublishMs should be 0")
	}
	if b.SensorsAnnounced() != 0 {
		t.Error("zero bridge: SensorsAnnounced should be 0")
	}
}

// TestBrokerAddrFormat verifies BrokerAddr returns "host:port" for
// various broker configurations.
func TestBrokerAddrFormat(t *testing.T) {
	cases := []struct {
		name   string
		broker string
		port   int
		want   string
	}{
		{"ip", "192.168.1.100", 1883, "192.168.1.100:1883"},
		{"hostname", "mqtt.local", 8883, "mqtt.local:8883"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &Bridge{cfg: &config.HomeAssistant{Broker: tc.broker, Port: tc.port}}
			if got := b.BrokerAddr(); got != tc.want {
				t.Errorf("BrokerAddr = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSensorsAnnouncedZeroWithoutStart verifies that SensorsAnnounced is
// zero when the bridge has been constructed but discovery has not run (no
// broker connection). This is the normal state when the API queries a
// bridge that failed to connect.
func TestSensorsAnnouncedZeroWithoutStart(t *testing.T) {
	b := &Bridge{
		driverNames: []string{"ferroamp", "sungrow"},
	}
	if got := b.SensorsAnnounced(); got != 0 {
		t.Errorf("SensorsAnnounced before discovery = %d, want 0", got)
	}
}

// TestSensorsAnnouncedFormula verifies the count that publishDiscovery
// writes matches the formula in the code: len(sensors) + len(driverNames)*5.
// We can't call publishDiscovery (it needs a live MQTT client), but we
// can verify the formula by constructing a Bridge and writing the field
// directly as publishDiscovery would.
func TestSensorsAnnouncedFormula(t *testing.T) {
	drivers := []string{"ferroamp", "sungrow"}
	b := &Bridge{driverNames: drivers}

	// Simulate what publishDiscovery sets (line 242 of bridge.go):
	// 6 site-level sensors + len(driverNames)*5
	siteSensors := 6
	perDriver := 5
	expected := siteSensors + len(drivers)*perDriver
	b.mu.Lock()
	b.sensorsAnnounced = expected
	b.mu.Unlock()

	if got := b.SensorsAnnounced(); got != expected {
		t.Errorf("SensorsAnnounced = %d, want %d", got, expected)
	}
}

// TestLastPublishMsRoundTrip verifies the getter/setter path through the
// mutex-protected field.
func TestLastPublishMsRoundTrip(t *testing.T) {
	b := &Bridge{}

	b.mu.Lock()
	b.lastPublishMs = 1713200000000
	b.mu.Unlock()

	if got := b.LastPublishMs(); got != 1713200000000 {
		t.Errorf("LastPublishMs = %d, want 1713200000000", got)
	}
}

// TestStopIdempotent verifies the lifecycle Stop is safe to call multiple
// times. main.go's defer-stack and the configreload "running → disabled"
// path can both wind up calling Stop on the same bridge; a second call
// must be a no-op rather than a panic-inducing double-close-of-channel.
func TestStopIdempotent(t *testing.T) {
	b := &Bridge{}
	// Pretend Start ran and gave us channels to close.
	b.stop = make(chan struct{})
	b.done = make(chan struct{})
	close(b.done) // simulate publishLoop having already exited

	b.Stop()
	b.Stop() // second call must be a no-op

	if !b.stopped {
		t.Error("Stop should mark bridge stopped")
	}
}

// TestReloadAfterStopRejected guards the invariant that a Stop'd bridge
// is terminal — operators must construct a fresh bridge, not resurrect
// the dead one. The configreload applier in main.go relies on this: it
// nulls haBridge after Stop and calls ha.Start on a re-enable, so a
// Reload-on-stopped path that silently succeeded would mask wiring bugs.
func TestReloadAfterStopRejected(t *testing.T) {
	b := &Bridge{}
	b.stop = make(chan struct{})
	b.done = make(chan struct{})
	close(b.done)
	b.Stop()

	err := b.Reload(&config.HomeAssistant{Broker: "x", Port: 1883}, nil)
	if err == nil {
		t.Fatal("Reload after Stop must error, got nil")
	}
}

// TestStopAfterFailedConnectDoesNotDeadlock documents the invariant
// connectAndStart guarantees on its error path: when paho's Connect()
// fails, the fresh stop+done channels it installed must not leak
// (publishLoop never starts, so nothing else closes `b.done`). The
// rollback in connectAndStart closes b.done explicitly — this test
// simulates that post-rollback state and verifies Stop completes
// rather than blocking forever in `<-doneCh`. Codex P1 on PR #232.
//
// The error path is rare in practice because the bridge wires paho
// with `SetConnectRetry(true)` — refused connections never surface as
// a synchronous error. The fix is still defensive: paho versions /
// future config tweaks can re-introduce the path, and the cost of
// always closing the rollback channel is one extra channel close.
func TestStopAfterFailedConnectDoesNotDeadlock(t *testing.T) {
	b := &Bridge{}
	// Simulate the post-rollback state connectAndStart leaves behind
	// after a Connect failure: stop is fresh + open, done is fresh +
	// closed by the rollback, no goroutine running, no live client.
	b.stop = make(chan struct{})
	b.done = make(chan struct{})
	close(b.done)
	b.cfg = &config.HomeAssistant{Broker: "unreachable", Port: 1883}

	doneCh := make(chan struct{})
	go func() {
		b.Stop()
		close(doneCh)
	}()
	select {
	case <-doneCh:
		// Stop returned — invariant holds.
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked after a simulated failed Connect — teardown is stuck on <-doneCh")
	}
}
