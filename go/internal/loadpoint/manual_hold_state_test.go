package loadpoint

import (
	"testing"
	"time"
)

func TestManualHoldRestoreRequiresHardwareAndActiveSession(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	m := sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 4300, 1000, true, "easee:ABC", "session-1")
	if err := m.PersistManualHold("garage", ManualHold{PowerW: 4140, Persistent: true}, false); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		device, session, status string
		power                   float64
	}{
		{"easee:ABC", "session-1", "restored", 4140},
		{"easee:ABC", "session-2", "unconfirmed", 0},
		{"easee:ABC", "", "unconfirmed", 0},
		{"easee:OTHER", "session-1", "unconfirmed", 0},
	} {
		m := sessionManager(store, "garage", "charger")
		m.ObserveSession("garage", true, 0, 1000, true, tc.device, tc.session)
		h, status := m.RestoreManualHold("garage")
		if status != tc.status || h.PowerW != tc.power || !h.Persistent {
			t.Fatalf("%+v: got %+v %s", tc, h, status)
		}
	}
	if err := m.PersistManualHold("garage", ManualHold{PowerW: 0, Persistent: true}, false); err != nil {
		t.Fatal(err)
	}
	m = sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 0, 1000, true, "easee:ABC", "")
	if h, status := m.RestoreManualHold("garage"); status != "restored" || h.PowerW != 0 {
		t.Fatalf("pause failed: %+v %s", h, status)
	}
}

func TestManualHoldBeforeIdentityBindsOnlyAfterFreshReading(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	m := sessionManager(store, "garage", "charger")
	m.PersistManualHold("garage", ManualHold{PowerW: 4140, Persistent: true}, false)
	if len(store.data) != 0 {
		t.Fatal("unidentified device persisted")
	}
	m.ObserveSession("garage", true, 4300, 1000, true, "easee:ABC", "session-1")
	m = sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 4300, 1100, true, "easee:ABC", "session-1")
	if h, status := m.RestoreManualHold("garage"); status != "restored" || h.PowerW != 4140 {
		t.Fatalf("explicit start was lost: %+v %s", h, status)
	}
}

func TestControllerPausesPendingRestoreAndHonoursExplicitOverride(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	m := sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 4300, 1000, true, "easee:ABC", "session-1")
	m.PersistManualHold("garage", ManualHold{PowerW: 4140, Persistent: true}, false)
	m = sessionManager(store, "garage", "charger")
	c := NewController(m, nil, nil, nil)
	c.restoreManualHoldForSession("garage")
	if h, ok := c.GetManualHold("garage", time.Now()); !ok || h.PowerW != 0 {
		t.Fatalf("unknown session could use automatic charging: %+v %v", h, ok)
	}
	if s, _ := m.State("garage"); !s.ManualRestoreUnconfirmed {
		t.Fatal("pending restore not visible")
	}
	c.markManualExplicit("garage")
	c.SetManualHold("garage", ManualHold{PowerW: 5520, Persistent: true})
	m.ObserveSession("garage", true, 4300, 1100, true, "easee:ABC", "session-1")
	c.restoreManualHoldForSession("garage")
	if h, ok := c.GetManualHold("garage", time.Now()); !ok || h.PowerW != 5520 {
		t.Fatalf("recovery overwrote explicit choice: %+v %v", h, ok)
	}
	if s, _ := m.State("garage"); s.ManualRestoreUnconfirmed {
		t.Fatal("explicit action did not confirm restore")
	}
}

func TestLegacyManualHoldNeedsConfirmationAndClearCannotResurrectIt(t *testing.T) {
	store := &sessionMemory{data: map[string]string{"loadpoint_manual_hold:garage": `{"PowerW":11000,"Persistent":true}`}}
	m := sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 4300, 1000, true, "easee:ABC", "session-1")
	if h, status := m.RestoreManualHold("garage"); status != "unconfirmed" || h.PowerW != 0 {
		t.Fatalf("legacy hold resumed: %+v %s", h, status)
	}
	m.PersistManualHold("garage", ManualHold{}, true)
	m = sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 4300, 1100, true, "easee:ABC", "session-1")
	if _, status := m.RestoreManualHold("garage"); status != "none" {
		t.Fatalf("cleared hold resurrected: %s", status)
	}
}

func TestRuntimeManualHoldCannotFollowChangedDeviceOrSession(t *testing.T) {
	for _, change := range []string{"hardware", "session", "remove_readd"} {
		t.Run(change, func(t *testing.T) {
			store := &sessionMemory{data: map[string]string{}}
			m := sessionManager(store, "garage", "charger")
			m.ObserveSession("garage", true, 4300, 1000, true, "easee:A", "session-1")
			c := NewController(m, nil, nil, nil)
			c.SetManualHold("garage", ManualHold{PowerW: 4140, Persistent: true})
			device, session := "easee:A", "session-1"
			switch change {
			case "hardware":
				device = "easee:B"
			case "session":
				session = "session-2"
			case "remove_readd":
				m.Load(nil)
				m.Load([]Config{{ID: "garage", DriverName: "charger", VehicleCapacityWh: 60000}})
			}
			m.ObserveSession("garage", true, 4300, 1100, true, device, session)
			c.restoreManualHoldForSession("garage")
			if h, ok := c.GetManualHold("garage", time.Now()); !ok || h.PowerW != 0 {
				t.Fatalf("old power followed %s: %+v %v", change, h, ok)
			}
			if s, _ := m.State("garage"); !s.ManualRestoreUnconfirmed {
				t.Fatal("changed session was not shown")
			}
			// The safe pause also survives another restart on that hardware.
			m = sessionManager(store, "garage", "charger")
			m.ObserveSession("garage", true, 0, 1100, true, device, session)
			if h, status := m.RestoreManualHold("garage"); status != "restored" || h.PowerW != 0 {
				t.Fatalf("pause was not durable: %+v %s", h, status)
			}
		})
	}
}

func TestExplicitStartCanAcquireFirstSessionProof(t *testing.T) {
	for _, initialDevice := range []string{"", "easee:A"} {
		m := sessionManager(nil, "garage", "charger")
		if initialDevice != "" {
			m.ObserveSession("garage", true, 0, 0, true, initialDevice, "")
		}
		c := NewController(m, nil, nil, nil)
		c.SetManualHold("garage", ManualHold{PowerW: 4140, Persistent: true})
		m.ObserveSession("garage", true, 4300, 100, true, "easee:A", "session-1")
		c.restoreManualHoldForSession("garage")
		if h, ok := c.GetManualHold("garage", time.Now()); !ok || h.PowerW != 4140 {
			t.Fatalf("explicit start before proof was lost: %+v %v", h, ok)
		}
	}
}

func TestClearBeforeTelemetrySurvivesAnotherImmediateRestart(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	m := sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 4300, 1000, true, "easee:A", "session-1")
	m.PersistManualHold("garage", ManualHold{PowerW: 4140, Persistent: true}, false)
	store.data["loadpoint_manual_hold:garage"] = `{"PowerW":11000,"Persistent":true}`
	m = sessionManager(store, "garage", "charger")
	c := NewController(m, nil, nil, nil)
	c.SetManualHoldSaver(func(id string, h ManualHold, cleared bool) {
		if err := m.PersistManualHold(id, h, cleared); err != nil {
			t.Error(err)
		}
	})
	c.ClearManualHold("garage")
	m = sessionManager(store, "garage", "charger")
	c = NewController(m, nil, nil, nil)
	c.restoreManualHoldForSession("garage")
	m.ObserveSession("garage", true, 4300, 1000, true, "easee:A", "session-1")
	c.restoreManualHoldForSession("garage")
	if _, ok := c.GetManualHold("garage", time.Now()); ok {
		t.Fatal("cleared hold returned after reboot before telemetry")
	}
	if _, status := m.RestoreManualHold("garage"); status != "none" {
		t.Fatalf("record survived explicit clear: %s", status)
	}
}

func TestConcurrentSetAndClearPersistInControllerOrder(t *testing.T) {
	m := sessionManager(nil, "garage", "charger")
	c := NewController(m, nil, nil, nil)
	entered, release, setDone, clearDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var order []float64
	c.SetManualHoldSaver(func(_ string, h ManualHold, cleared bool) {
		if !cleared {
			close(entered)
			<-release
			order = append(order, h.PowerW)
		} else {
			order = append(order, 0)
		}
	})
	go func() { c.SetManualHold("garage", ManualHold{PowerW: 4140, Persistent: true}); close(setDone) }()
	<-entered
	go func() { c.ClearManualHold("garage"); close(clearDone) }()
	select {
	case <-clearDone:
		t.Fatal("Clear overtook an earlier pending disk write")
	case <-time.After(20 * time.Millisecond):
	}
	if h, ok := c.GetManualHold("garage", time.Now()); !ok || h.PowerW != 4140 {
		t.Fatalf("read was blocked or changed before ordered clear: %+v %v", h, ok)
	}
	close(release)
	<-setDone
	<-clearDone
	if len(order) != 2 || order[0] != 4140 || order[1] != 0 {
		t.Fatalf("persist order: %v", order)
	}
	if _, ok := c.GetManualHold("garage", time.Now()); ok {
		t.Fatal("clear lost to earlier save")
	}
}
