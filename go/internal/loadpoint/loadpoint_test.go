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
	m.Load([]Config{{
		ID: "garage", DriverName: "easee-cloud",
		VehicleCapacityWh: 60000, PluginSoCPct: 40,
	}})
	m.Observe("garage", true, 7400, 1200) // 1.2 kWh into session
	target := time.Date(2026, 4, 18, 6, 0, 0, 0, time.UTC)
	m.SetTarget("garage", 80, target)

	// Reload with same ID — state should persist.
	m.Load([]Config{{
		ID: "garage", DriverName: "easee-cloud", MaxChargeW: 11000,
		VehicleCapacityWh: 60000, PluginSoCPct: 40,
	}})
	st, ok := m.State("garage")
	if !ok {
		t.Fatal("state missing after reload")
	}
	// SoC = 40 + 1200/60000*100 = 42
	if !st.PluggedIn || st.CurrentPowerW != 7400 {
		t.Errorf("observed state lost: %+v", st)
	}
	if got := st.CurrentSoCPct; got < 41.5 || got > 42.5 {
		t.Errorf("SoC estimate: got %.2f, want ~42", got)
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
	m.Observe("ghost", true, 7400, 0) // must not panic
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

// TestObserveUnpluggedClearsSoCEstimate — when the car is disconnected
// we can't meaningfully estimate its SoC, so the manager clears it.
// Otherwise a stale 42% would hang on the display after the car drove
// away.
func TestObserveUnpluggedClearsSoCEstimate(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoCPct: 30}})
	m.Observe("a", true, 7400, 1800) // charging → SoC = 30 + 3 = 33
	if st, _ := m.State("a"); st.CurrentSoCPct < 32.5 || st.CurrentSoCPct > 33.5 {
		t.Fatalf("expected ~33 %% while plugged in, got %.2f", st.CurrentSoCPct)
	}
	m.Observe("a", false, 0, 0)
	if st, _ := m.State("a"); st.CurrentSoCPct != 0 || st.PluggedIn {
		t.Errorf("expected cleared state when unplugged, got %+v", st)
	}
}

// TestObserveNewSessionAnchor — on plug-in the anchor resets to
// Config.PluginSoCPct. This prevents residual session_wh from a
// previous session leaking into the new one.
func TestObserveNewSessionAnchor(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoCPct: 50}})
	m.Observe("a", true, 0, 0)
	if st, _ := m.State("a"); st.CurrentSoCPct < 49 || st.CurrentSoCPct > 51 {
		t.Errorf("fresh plug-in should show 50 %%, got %.2f", st.CurrentSoCPct)
	}
	// Disconnect.
	m.Observe("a", false, 0, 0)
	// Re-plug — session delivered counter starts fresh.
	m.Observe("a", true, 0, 0)
	if st, _ := m.State("a"); st.CurrentSoCPct < 49 || st.CurrentSoCPct > 51 {
		t.Errorf("re-plug should re-anchor at 50 %%, got %.2f", st.CurrentSoCPct)
	}
}

func TestStatesReturnsAllInOrder(t *testing.T) {
	m := NewManager()
	m.Load([]Config{
		{ID: "garage", MaxChargeW: 11000, VehicleCapacityWh: 60000},
		{ID: "street", MaxChargeW: 7400, VehicleCapacityWh: 60000},
	})
	m.Observe("garage", true, 11000, 500)
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
