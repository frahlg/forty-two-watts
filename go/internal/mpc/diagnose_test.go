package mpc

import (
	"testing"
	"time"
)

// TestDiagnoseNilBeforeReplan asserts we return nil (not a garbage
// struct or panic) when Diagnose is called before any replan has
// completed. The UI handles nil as "no plan yet".
func TestDiagnoseNilBeforeReplan(t *testing.T) {
	s := &Service{Zone: "SE3"}
	if d := s.Diagnose(); d != nil {
		t.Errorf("Diagnose before first replan must be nil, got %+v", d)
	}
}

// TestDiagnoseJoinsSlotsAndActions is the core contract: the per-slot
// output row must carry BOTH the input context the DP saw (price, PV,
// load, confidence) and the decision it made (battery, grid, SoC,
// reason). Without the join, operators can't audit decisions.
func TestDiagnoseJoinsSlotsAndActions(t *testing.T) {
	start := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC).UnixMilli()
	// Two 15-min slots, both have positive price so the DP will opt to
	// idle (self-consumption mode). Exact decision doesn't matter —
	// we're testing the join shape.
	slots := []Slot{
		{StartMs: start, LenMin: 15, PriceOre: 100, SpotOre: 50,
			PVW: -200, LoadW: 400, Confidence: 1.0},
		{StartMs: start + 15*60*1000, LenMin: 15, PriceOre: 150,
			SpotOre: 80, PVW: -100, LoadW: 500, Confidence: 0.6},
	}
	p := Params{
		Mode:                ModeSelfConsumption,
		SoCLevels:           11,
		CapacityWh:          10000,
		SoCMinPct:           10,
		SoCMaxPct:           95,
		InitialSoCPct:       50,
		ActionLevels:        7,
		MaxChargeW:          5000,
		MaxDischargeW:       5000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    100,
	}
	plan := Optimize(slots, p)

	svc := &Service{
		Zone:         "SE3",
		last:         &plan,
		lastSlots:    slots,
		lastParams:   p,
		lastReplanAt: time.UnixMilli(plan.GeneratedAtMs),
		lastReason:   "unit-test",
	}
	d := svc.Diagnose()
	if d == nil {
		t.Fatal("Diagnose returned nil after a successful optimize")
	}
	if d.Zone != "SE3" {
		t.Errorf("Zone: got %q want SE3", d.Zone)
	}
	if d.Params.Mode != ModeSelfConsumption {
		t.Errorf("Params.Mode: got %q want self_consumption", d.Params.Mode)
	}
	if d.Params.InitialSoCPct != 50 {
		t.Errorf("Params.InitialSoCPct: got %.2f want 50", d.Params.InitialSoCPct)
	}
	if d.LastReason != "unit-test" {
		t.Errorf("LastReason: got %q want unit-test", d.LastReason)
	}
	if got := len(d.Slots); got != len(slots) {
		t.Fatalf("Slots length: got %d want %d", got, len(slots))
	}
	// Verify row 0 joined correctly: inputs match slots[0], outputs
	// match plan.Actions[0].
	row := d.Slots[0]
	if row.PriceOre != 100 {
		t.Errorf("row0 PriceOre: got %.1f want 100", row.PriceOre)
	}
	if row.SpotOre != 50 {
		t.Errorf("row0 SpotOre: got %.1f want 50", row.SpotOre)
	}
	if row.Confidence != 1.0 {
		t.Errorf("row0 Confidence: got %.2f want 1.0", row.Confidence)
	}
	if row.PVW != -200 {
		t.Errorf("row0 PVW: got %.1f want -200", row.PVW)
	}
	if row.LoadW != 400 {
		t.Errorf("row0 LoadW: got %.1f want 400", row.LoadW)
	}
	// Outputs come from the plan's action — we don't assert exact
	// values (that's what the mpc_test suite covers), just that they
	// were populated.
	if row.Reason == "" {
		t.Error("row0 Reason should be populated by the DP")
	}
	// Row 1 should carry the forecast confidence.
	if d.Slots[1].Confidence != 0.6 {
		t.Errorf("row1 Confidence: got %.2f want 0.6", d.Slots[1].Confidence)
	}
	if d.Slots[1].SlotStartMs != start+15*60*1000 {
		t.Errorf("row1 SlotStartMs: got %d want %d",
			d.Slots[1].SlotStartMs, start+15*60*1000)
	}
}

// TestDiagnoseHandlesLengthMismatch guards against a panic if slots
// and actions ever get out of sync (shouldn't happen in practice —
// Optimize returns len(actions) == len(slots) — but we round-trip
// into lastSlots in service code paths that could diverge).
func TestDiagnoseHandlesLengthMismatch(t *testing.T) {
	slots := []Slot{
		{StartMs: 1000, LenMin: 15, PriceOre: 100, Confidence: 1.0},
		{StartMs: 2000, LenMin: 15, PriceOre: 110, Confidence: 1.0},
	}
	plan := Plan{
		GeneratedAtMs: 123,
		Actions:       []Action{{SlotStartMs: 1000, SlotLenMin: 15}},
	}
	svc := &Service{
		Zone:      "SE3",
		last:      &plan,
		lastSlots: slots,
	}
	d := svc.Diagnose()
	if d == nil {
		t.Fatal("Diagnose should not be nil on mismatch — should truncate")
	}
	if len(d.Slots) != 1 {
		t.Errorf("should truncate to shorter side; got %d rows", len(d.Slots))
	}
}
