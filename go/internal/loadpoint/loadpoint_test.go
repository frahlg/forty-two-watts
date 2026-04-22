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
	m.SetTarget("garage", 80, target, "manual", TargetPolicy{})

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
	m.SetTarget("a", 250, time.Time{}, "manual", TargetPolicy{})
	st, _ := m.State("a")
	if st.TargetSoCPct != 100 {
		t.Errorf("should clamp to 100; got %f", st.TargetSoCPct)
	}
	m.SetTarget("a", -10, time.Time{}, "manual", TargetPolicy{})
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

// TestSetCurrentSoCReAnchors — operator corrects the inferred SoC
// mid-session. After SetCurrentSoC, current_soc equals the provided
// value, and any further delivered Wh advance from that anchor.
// Chargers can't read vehicle BMS, so this is the only way the
// operator can correct drift.
func TestSetCurrentSoCReAnchors(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoCPct: 25}})
	// Plug in, deliver 9 kWh → naive estimate = 25 + 9000/60000*100 = 40 %.
	m.Observe("a", true, 7400, 9000)
	if st, _ := m.State("a"); st.CurrentSoCPct < 39 || st.CurrentSoCPct > 41 {
		t.Fatalf("pre-correction SoC: got %.2f want ~40", st.CurrentSoCPct)
	}
	// Operator looks at their dashboard: car is actually 60 %.
	if !m.SetCurrentSoC("a", 60) {
		t.Fatal("SetCurrentSoC returned false on plugged-in loadpoint")
	}
	st, _ := m.State("a")
	if st.CurrentSoCPct < 59 || st.CurrentSoCPct > 61 {
		t.Errorf("post-correction SoC: got %.2f want ~60", st.CurrentSoCPct)
	}
	// Deliver another 3 kWh → should be ~65 % (60 + 3000/60000*100).
	m.Observe("a", true, 7400, 12000)
	st, _ = m.State("a")
	if st.CurrentSoCPct < 64 || st.CurrentSoCPct > 66 {
		t.Errorf("after more delivery SoC: got %.2f want ~65", st.CurrentSoCPct)
	}
}

// TestSetCurrentSoCRejectsUnplugged — SoC is meaningless without an
// active session. Chargers may cling to the last known vehicle state
// briefly after unplug; blocking this avoids anchoring against noise.
func TestSetCurrentSoCRejectsUnplugged(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000}})
	if m.SetCurrentSoC("a", 55) {
		t.Error("should reject SetCurrentSoC on never-plugged loadpoint")
	}
	m.Observe("a", true, 0, 0)
	m.Observe("a", false, 0, 0)
	if m.SetCurrentSoC("a", 55) {
		t.Error("should reject SetCurrentSoC after unplug")
	}
}

func TestSetCurrentSoCClampsRange(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000}})
	m.Observe("a", true, 0, 0)
	m.SetCurrentSoC("a", 150)
	if st, _ := m.State("a"); st.CurrentSoCPct != 100 {
		t.Errorf("clamp high: got %.2f", st.CurrentSoCPct)
	}
	m.SetCurrentSoC("a", -10)
	if st, _ := m.State("a"); st.CurrentSoCPct != 0 {
		t.Errorf("clamp low: got %.2f", st.CurrentSoCPct)
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

func TestSetTargetWithPolicyRoundtrip(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee"}})
	deadline := time.Now().Add(8 * time.Hour)
	ok := m.SetTarget("garage", 80, deadline, "manual", TargetPolicy{
		AllowGrid:           false,
		AllowBatterySupport: true,
	})
	if !ok {
		t.Fatalf("SetTarget returned false for known id")
	}
	st, ok := m.State("garage")
	if !ok {
		t.Fatalf("State not found")
	}
	if st.TargetSoCPct != 80 {
		t.Errorf("target SoC = %v, want 80", st.TargetSoCPct)
	}
	if st.TargetOrigin != "manual" {
		t.Errorf("origin = %q, want manual", st.TargetOrigin)
	}
	if st.TargetPolicy.AllowGrid {
		t.Errorf("AllowGrid = true, want false")
	}
	if !st.TargetPolicy.AllowBatterySupport {
		t.Errorf("AllowBatterySupport = false, want true")
	}
}

func TestSetTargetUnknownID(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage"}})
	if m.SetTarget("unknown", 80, time.Now(), "manual", TargetPolicy{}) {
		t.Errorf("expected false for unknown id")
	}
}

// fakeKV implements the tiny interface loadpoint needs for persistence.
type fakeKV struct{ m map[string]string }

func (k *fakeKV) SaveConfig(key, value string) error {
	if k.m == nil {
		k.m = map[string]string{}
	}
	k.m[key] = value
	return nil
}
func (k *fakeKV) LoadConfig(key string) (string, bool) {
	v, ok := k.m[key]
	return v, ok
}

func TestLoadpointSettingsDefaults(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage"}})
	m.BindKV(&fakeKV{})
	s := m.Settings("garage")
	if s.ChargeDurationH != 8 {
		t.Errorf("ChargeDurationH default = %v, want 8", s.ChargeDurationH)
	}
	if s.SurplusHysteresisW != 500 {
		t.Errorf("SurplusHysteresisW default = %v, want 500", s.SurplusHysteresisW)
	}
	if s.SurplusHysteresisS != 300 {
		t.Errorf("SurplusHysteresisS default = %v, want 300", s.SurplusHysteresisS)
	}
	if s.SurplusStarvationS != 1800 {
		t.Errorf("SurplusStarvationS default = %v, want 1800", s.SurplusStarvationS)
	}
}

func TestLoadpointSettingsRoundtrip(t *testing.T) {
	kv := &fakeKV{}
	m := NewManager()
	m.Load([]Config{{ID: "garage"}})
	m.BindKV(kv)
	err := m.UpdateSettings("garage", Settings{
		ChargeDurationH:    4,
		SurplusHysteresisW: 300,
		SurplusHysteresisS: 120,
		SurplusStarvationS: 900,
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	// Fresh manager, same KV → settings restore.
	m2 := NewManager()
	m2.Load([]Config{{ID: "garage"}})
	m2.BindKV(kv)
	s := m2.Settings("garage")
	if s.ChargeDurationH != 4 || s.SurplusHysteresisW != 300 ||
		s.SurplusHysteresisS != 120 || s.SurplusStarvationS != 900 {
		t.Errorf("settings not restored: %+v", s)
	}
}

func TestLoadpointLastPolicyRoundtrip(t *testing.T) {
	kv := &fakeKV{}
	m := NewManager()
	m.Load([]Config{{ID: "garage"}})
	m.BindKV(kv)
	m.SaveLastPolicy("garage", TargetPolicy{AllowGrid: true, AllowBatterySupport: false})
	m2 := NewManager()
	m2.Load([]Config{{ID: "garage"}})
	m2.BindKV(kv)
	p := m2.LastPolicy("garage")
	if !p.AllowGrid || p.AllowBatterySupport {
		t.Errorf("last policy not restored: %+v", p)
	}
}
