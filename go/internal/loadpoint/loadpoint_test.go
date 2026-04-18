package loadpoint

import (
	"testing"
	"time"
)

func TestLoadPopulatesAndPreservesOrder(t *testing.T) {
	m := NewManager()
	m.Load([]Config{
		{ID: "garage", DriverName: "easee-cloud", MaxChargeW: 11000},
		{ID: "street", DriverName: "zap", MaxChargeW: 7400},
	})
	if ids := m.IDs(); len(ids) != 2 || ids[0] != "garage" || ids[1] != "street" {
		t.Errorf("IDs not insertion-ordered: %v", ids)
	}
}

func TestLoadSkipsBlankID(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "", DriverName: "ghost"}, {ID: "real"}})
	if len(m.IDs()) != 1 || m.IDs()[0] != "real" {
		t.Errorf("blank ID should be skipped; got %v", m.IDs())
	}
}

func TestReloadPreservesObservedState(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee-cloud"}})
	m.Observe("garage", true, 55, 7400, 1200)
	target := time.Date(2026, 4, 18, 6, 0, 0, 0, time.UTC)
	m.SetTarget("garage", 80, target)

	// Reload with same ID — state should persist.
	m.Load([]Config{{ID: "garage", DriverName: "easee-cloud", MaxChargeW: 11000}})
	st, ok := m.State("garage")
	if !ok {
		t.Fatal("state missing after reload")
	}
	if !st.PluggedIn || st.CurrentSoCPct != 55 || st.CurrentPowerW != 7400 {
		t.Errorf("observed state lost: %+v", st)
	}
	if st.TargetSoCPct != 80 || !st.TargetTime.Equal(target) {
		t.Errorf("target lost: %+v", st)
	}
	// But config should update.
	if st.MaxChargeW != 11000 {
		t.Errorf("config not updated: MaxChargeW=%f", st.MaxChargeW)
	}
}

func TestReloadDropsRemovedIDs(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a"}, {ID: "b"}})
	m.Load([]Config{{ID: "b"}})
	if ids := m.IDs(); len(ids) != 1 || ids[0] != "b" {
		t.Errorf("removed ID should be dropped; got %v", ids)
	}
}

func TestObserveOnUnknownIsNoop(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "real"}})
	m.Observe("ghost", true, 80, 7400, 0) // must not panic
	if _, ok := m.State("ghost"); ok {
		t.Error("ghost state should not exist")
	}
}

func TestSetTargetClamp(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a"}})
	m.SetTarget("a", 250, time.Time{})
	st, _ := m.State("a")
	if st.TargetSoCPct != 100 {
		t.Errorf("should clamp to 100; got %f", st.TargetSoCPct)
	}
	m.SetTarget("a", -10, time.Time{})
	st, _ = m.State("a")
	if st.TargetSoCPct != 0 {
		t.Errorf("should clamp to 0; got %f", st.TargetSoCPct)
	}
}

func TestStatesReturnsAllInOrder(t *testing.T) {
	m := NewManager()
	m.Load([]Config{
		{ID: "garage", MaxChargeW: 11000},
		{ID: "street", MaxChargeW: 7400},
	})
	m.Observe("garage", true, 40, 11000, 500)
	states := m.States()
	if len(states) != 2 {
		t.Fatalf("expected 2 states, got %d", len(states))
	}
	if states[0].ID != "garage" || states[1].ID != "street" {
		t.Errorf("wrong ordering: %v, %v", states[0].ID, states[1].ID)
	}
	if !states[0].PluggedIn {
		t.Error("garage should be plugged in")
	}
	if states[1].PluggedIn {
		t.Error("street should not be plugged in")
	}
}
