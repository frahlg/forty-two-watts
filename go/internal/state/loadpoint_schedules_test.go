package state

import (
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestUpsertPrimaryLoadpointSchedule covers the path the legacy
// POST /api/loadpoints/{id}/target endpoint drives: first call
// INSERTs, second call UPDATEs the same row.
func TestUpsertPrimaryLoadpointSchedule(t *testing.T) {
	st := newTestStore(t)

	sched, err := st.UpsertPrimaryLoadpointSchedule("garage", 80, 1776960000000)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	firstID := sched.ID
	if firstID == 0 {
		t.Error("expected non-zero ID after insert")
	}
	if sched.Name != "primary" {
		t.Errorf("default name = %q, want primary", sched.Name)
	}
	if !sched.Enabled || !sched.AllowGrid || !sched.AllowBatterySupport {
		t.Errorf("default policy not permissive: %+v", sched)
	}
	if sched.OnlySurplus {
		t.Errorf("default only_surplus should be false")
	}

	// Second upsert — same loadpoint, different soc.
	sched2, err := st.UpsertPrimaryLoadpointSchedule("garage", 90, 1776960000000)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if sched2.ID != firstID {
		t.Errorf("upsert changed ID: %d → %d", firstID, sched2.ID)
	}
	if sched2.TargetSoCPct != 90 {
		t.Errorf("soc not updated: %f", sched2.TargetSoCPct)
	}
}

// TestSaveLoadpointScheduleWithFlags round-trips the energy-source
// policy flags (AllowGrid / AllowBatterySupport / OnlySurplus).
func TestSaveLoadpointScheduleWithFlags(t *testing.T) {
	st := newTestStore(t)

	sched := &LoadpointSchedule{
		LoadpointID:         "garage",
		Name:                "primary",
		TargetSoCPct:        80,
		TargetTimeMs:        1776960000000,
		Enabled:             true,
		AllowGrid:           false,
		AllowBatterySupport: false,
		OnlySurplus:         true,
	}
	if _, err := st.SaveLoadpointSchedule(sched); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := st.ListLoadpointSchedules("garage")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d schedules, want 1", len(got))
	}
	g := got[0]
	if g.AllowGrid || g.AllowBatterySupport {
		t.Errorf("false flags did not round-trip: %+v", g)
	}
	if !g.OnlySurplus {
		t.Errorf("only_surplus=true did not round-trip: %+v", g)
	}
}

// TestAllLoadpointSchedulesReturnsEveryLoadpoint — the startup
// restore path queries every row in one shot.
func TestAllLoadpointSchedulesReturnsEveryLoadpoint(t *testing.T) {
	st := newTestStore(t)
	_, _ = st.UpsertPrimaryLoadpointSchedule("garage", 80, 1)
	_, _ = st.UpsertPrimaryLoadpointSchedule("street", 50, 2)

	all, err := st.AllLoadpointSchedules()
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d, want 2: %+v", len(all), all)
	}
	ids := map[string]bool{}
	for _, s := range all {
		ids[s.LoadpointID] = true
	}
	if !ids["garage"] || !ids["street"] {
		t.Errorf("missing loadpoint: %+v", ids)
	}
}

// TestDeleteLoadpointSchedule — removal works by ID, not lp.
func TestDeleteLoadpointSchedule(t *testing.T) {
	st := newTestStore(t)
	sched, _ := st.UpsertPrimaryLoadpointSchedule("garage", 80, 0)
	if err := st.DeleteLoadpointSchedule(sched.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, _ := st.ListLoadpointSchedules("garage")
	if len(got) != 0 {
		t.Errorf("expected 0 schedules after delete, got %d", len(got))
	}
}
