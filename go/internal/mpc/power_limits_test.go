package mpc

import "testing"

// TestPowerLimitsDefaultIsUnlimited asserts the zero value places no
// constraints on the DP — protecting backwards compatibility. Without
// this invariant, every existing Slot caller would suddenly face a
// zero-import / zero-export constraint.
func TestPowerLimitsDefaultIsUnlimited(t *testing.T) {
	var l PowerLimits
	for _, grid := range []float64{-10000, -100, 0, 100, 10000} {
		if !l.allowsImport(grid) {
			t.Errorf("default should allow import for gridW=%.0f", grid)
		}
		if !l.allowsExport(grid) {
			t.Errorf("default should allow export for gridW=%.0f", grid)
		}
	}
}

func TestPowerLimitsImportCap(t *testing.T) {
	l := PowerLimits{MaxImportW: 5000}
	cases := []struct {
		gridW  float64
		ok     bool
		reason string
	}{
		{-10000, true, "export ignores import cap"},
		{0, true, "zero flow allowed"},
		{4000, true, "below cap"},
		{5000, true, "at cap"},
		{5001, false, "above cap"},
		{10000, false, "well above cap"},
	}
	for _, tc := range cases {
		if got := l.allowsImport(tc.gridW); got != tc.ok {
			t.Errorf("%s: gridW=%.0f, got allowsImport=%v, want %v",
				tc.reason, tc.gridW, got, tc.ok)
		}
	}
}

func TestPowerLimitsExportCap(t *testing.T) {
	l := PowerLimits{MaxExportW: 3000}
	cases := []struct {
		gridW  float64
		ok     bool
		reason string
	}{
		{10000, true, "import ignores export cap"},
		{0, true, "zero flow allowed"},
		{-2000, true, "below cap (magnitude)"},
		{-3000, true, "at cap"},
		{-3001, false, "above cap"},
	}
	for _, tc := range cases {
		if got := l.allowsExport(tc.gridW); got != tc.ok {
			t.Errorf("%s: gridW=%.0f, got allowsExport=%v, want %v",
				tc.reason, tc.gridW, got, tc.ok)
		}
	}
}

// TestOptimizeRespectsImportCap is the end-to-end: if every cheap slot
// has an import cap, the DP should not schedule import over that cap.
// Without the cap, a cheap slot would tempt an unbounded charge.
func TestOptimizeRespectsImportCap(t *testing.T) {
	// 4 hourly slots, all cheap at 10 öre. Default battery params
	// would happily charge at full power in every slot; the cap on
	// slot 1 should force the DP to stay under 2000 W net import.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 10, SpotOre: 10,
			LoadW: 500, Confidence: 1.0},
		{StartMs: 3600_000, LenMin: 60, PriceOre: 10, SpotOre: 10,
			LoadW: 500, Confidence: 1.0,
			Limits: PowerLimits{MaxImportW: 2000}},
		{StartMs: 7200_000, LenMin: 60, PriceOre: 10, SpotOre: 10,
			LoadW: 500, Confidence: 1.0},
		{StartMs: 10800_000, LenMin: 60, PriceOre: 10, SpotOre: 10,
			LoadW: 500, Confidence: 1.0},
	}
	p := Params{
		Mode:                ModeCheapCharge,
		SoCLevels:           41,
		CapacityWh:          15000,
		SoCMinPct:           10,
		SoCMaxPct:           95,
		InitialSoCPct:       20,
		ActionLevels:        21,
		MaxChargeW:           5000,
		MaxDischargeW:        5000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    50,
	}
	plan := Optimize(slots, p)
	if len(plan.Actions) != len(slots) {
		t.Fatalf("got %d actions, want %d", len(plan.Actions), len(slots))
	}
	// Slot 1 has the cap. GridW there must not exceed 2000 W.
	g := plan.Actions[1].GridW
	if g > 2000+1e-6 {
		t.Errorf("capped slot GridW = %.1f, exceeds cap 2000 W", g)
	}
	// Uncapped slots should still be free to import more to make up
	// what was missed in the capped one — we don't assert a specific
	// bound, just that the DP didn't get stuck at a degenerate plan.
	if plan.Actions[0].GridW <= 0 && plan.Actions[2].GridW <= 0 &&
		plan.Actions[3].GridW <= 0 {
		t.Error("uncapped slots should import at least somewhere in a " +
			"4-hour flat-cheap window")
	}
}

// TestOptimizeRespectsExportCap mirrors the import test — PV surplus
// exported into a negative-price slot must respect the cap.
func TestOptimizeRespectsExportCap(t *testing.T) {
	// 3 hourly slots. Middle slot has negative price (export is
	// painful) AND a hard export cap of 500 W. The DP's export
	// decision in that slot must not exceed the cap.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 100, SpotOre: 80,
			PVW: -4000, LoadW: 500, Confidence: 1.0},
		{StartMs: 3600_000, LenMin: 60, PriceOre: 100, SpotOre: 80,
			PVW: -4000, LoadW: 500, Confidence: 1.0,
			Limits: PowerLimits{MaxExportW: 500}},
		{StartMs: 7200_000, LenMin: 60, PriceOre: 100, SpotOre: 80,
			PVW: -4000, LoadW: 500, Confidence: 1.0},
	}
	p := Params{
		Mode:                ModeArbitrage,
		SoCLevels:           41,
		CapacityWh:          15000,
		SoCMinPct:           10,
		SoCMaxPct:           95,
		InitialSoCPct:       70,
		ActionLevels:        21,
		MaxChargeW:          5000,
		MaxDischargeW:       5000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    80,
	}
	plan := Optimize(slots, p)
	g := plan.Actions[1].GridW
	if g < -500-1e-6 {
		t.Errorf("capped slot GridW = %.1f, below export cap -500 W", g)
	}
}
