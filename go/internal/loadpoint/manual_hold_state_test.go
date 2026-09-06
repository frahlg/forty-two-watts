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
