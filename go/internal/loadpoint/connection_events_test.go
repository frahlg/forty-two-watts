package loadpoint

import (
	"github.com/srcfl/ftw/go/internal/events"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"testing"
	"time"
)

func TestConnectionEventNeedsFreshEdgeAndSurvivesReload(t *testing.T) {
	m := NewManager()
	cfg := []Config{{ID: "garage", DriverName: "easee"}}
	m.Load(cfg)
	now := time.Unix(1700000000, 0)
	m.SetNowFn(func() time.Time { return now })
	b := events.NewBus()
	m.SetBus(b)
	count := 0
	b.Subscribe(events.KindChargingConnected, func(e events.Event) {
		count++
		if e.(events.ChargingConnected).LoadpointID != "garage" {
			t.Fatal(e)
		}
		m.State("garage")
	})
	health := func(status telemetry.DriverStatus) {
		b.Publish(events.HealthTick{Health: map[string]telemetry.DriverHealth{"easee": {Status: status}}, Now: now})
	}
	tick := func(connected bool) { m.Observe("garage", connected, 0, 0, true); now = now.Add(3 * time.Second) }
	health(telemetry.StatusOk)
	tick(true)
	tick(true)
	if count != 0 {
		t.Fatal("startup is not a new plug-in")
	}
	tick(false)
	tick(true)
	tick(true)
	if count != 1 {
		t.Fatalf("got %d, want one plug event", count)
	}
	m.Load(cfg)
	tick(true)
	if count != 1 {
		t.Fatal("reload repeated the plug event")
	}
	health(telemetry.StatusOffline)
	tick(false)
	health(telemetry.StatusOk)
	tick(true)
	if count != 1 {
		t.Fatal("recovery claimed a new plug-in")
	}
	tick(false)
	tick(true)
	if count != 2 {
		t.Fatal("next real plug-in was lost")
	}
}
