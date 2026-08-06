package drivers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A legacy (v1) driver whose poll and command spin forever, like a driver
// stuck in a retry loop against a dead device.
const spinningDriverSrc = `
function driver_init(config) end
function driver_poll()
    while true do end
end
function driver_command(action, w, cmd)
    while true do end
end
function driver_default_mode()
    while true do end
end
`

func newSpinningDriver(t *testing.T, timeout time.Duration) *LuaDriver {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spin.lua")
	if err := os.WriteFile(path, []byte(spinningDriverSrc), 0644); err != nil {
		t.Fatal(err)
	}
	d, err := NewLuaDriver(path, NewHostEnv("spin", telemetry.NewStore()))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	d.ExecTimeout = timeout
	return d
}

func TestLegacyDriverPollTimesOut(t *testing.T) {
	d := newSpinningDriver(t, 200*time.Millisecond)
	defer d.Cleanup()
	done := make(chan error, 1)
	go func() {
		_, err := d.Poll(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("spinning poll returned nil error, want deadline failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("spinning poll never returned; legacy drivers are unbounded")
	}
}

func TestLegacyDriverCommandTimesOut(t *testing.T) {
	d := newSpinningDriver(t, 200*time.Millisecond)
	defer d.Cleanup()
	done := make(chan error, 1)
	go func() {
		done <- d.Command(context.Background(), []byte(`{"action":"battery","power_w":0}`))
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("spinning command returned nil error, want deadline failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("spinning command never returned")
	}
}

func TestLegacyDriverDefaultModeTimesOut(t *testing.T) {
	d := newSpinningDriver(t, 200*time.Millisecond)
	defer d.Cleanup()
	done := make(chan error, 1)
	go func() {
		done <- d.DefaultMode()
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("spinning default_mode returned nil error, want deadline failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("spinning default_mode never returned")
	}
}

// A well-behaved driver must keep working after another entrypoint hit its
// deadline: the abort must not poison the LState for subsequent calls.
const recoveringDriverSrc = `
slow = true
function driver_init(config) end
function driver_poll()
    if slow then
        slow = false
        while true do end
    end
    host.emit("meter", { w = 42 })
    return 1000
end
`

func TestLegacyDriverRecoversAfterTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recover.lua")
	if err := os.WriteFile(path, []byte(recoveringDriverSrc), 0644); err != nil {
		t.Fatal(err)
	}
	d, err := NewLuaDriver(path, NewHostEnv("recover", telemetry.NewStore()))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	d.ExecTimeout = 200 * time.Millisecond
	defer d.Cleanup()

	if _, err := d.Poll(context.Background()); err == nil {
		t.Fatal("first poll should hit the deadline")
	}
	next, err := d.Poll(context.Background())
	if err != nil {
		t.Fatalf("second poll after timeout: %v", err)
	}
	if next != time.Second {
		t.Fatalf("second poll interval = %v, want 1s", next)
	}
}

func TestLegacyDriverZeroTimeoutStaysUnbounded(t *testing.T) {
	// ExecTimeout 0 must not install any context: a normal driver runs
	// exactly as before. (We can't wait forever to prove unboundedness;
	// instead prove that a normal poll works and no context is present.)
	path := filepath.Join(t.TempDir(), "plain.lua")
	if err := os.WriteFile(path, []byte(testDriverSrc), 0644); err != nil {
		t.Fatal(err)
	}
	env := NewHostEnv("plain", telemetry.NewStore())
	env.BatteryCapacityWh = 9600
	d, err := NewLuaDriver(path, env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	d.ExecTimeout = 0
	defer d.Cleanup()
	if err := d.Init(context.Background(), map[string]any{"foo": "bar"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if d.L.Context() != nil {
		t.Fatal("no lifecycle context should remain installed")
	}
}
