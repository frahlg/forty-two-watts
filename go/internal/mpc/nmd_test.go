package mpc

import "testing"

func TestApplyNMDImportCeiling(t *testing.T) {
	slots := []Slot{
		{LoadW: 20000, Limits: PowerLimits{MaxImportW: 69000}},
		{LoadW: 55000, Limits: PowerLimits{MaxImportW: 69000}}, // load above NMD
		{LoadW: 10000, Limits: PowerLimits{MaxImportW: 30000}}, // tighter existing limit
	}
	applyNMDImportCeiling(slots, 40000)
	if slots[0].Limits.MaxImportW != 40000 {
		t.Errorf("slot 0: %v, want 40000", slots[0].Limits.MaxImportW)
	}
	if slots[1].Limits.MaxImportW != 55000 {
		t.Errorf("slot 1 (load>NMD) must stay feasible at its load: %v", slots[1].Limits.MaxImportW)
	}
	if slots[2].Limits.MaxImportW != 30000 {
		t.Errorf("slot 2: existing tighter limit must not loosen: %v", slots[2].Limits.MaxImportW)
	}
	// 0 = no-op.
	applyNMDImportCeiling(slots, 0)
	if slots[0].Limits.MaxImportW != 40000 {
		t.Errorf("no-op changed a limit")
	}
}

func TestDPFallbackHonorsBackupFloor(t *testing.T) {
	// Expensive slot then cheap: an unconstrained arbitrage DP would
	// discharge deep in slot 0. The backup floor forbids planning below
	// 40% SoC.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 500, SpotOre: 400, Confidence: 1, LoadW: 3000},
		{StartMs: 3_600_000, LenMin: 60, PriceOre: 50, SpotOre: 20, Confidence: 1, LoadW: 3000},
	}
	p := Params{
		Mode: ModeArbitrage, SoCLevels: 41, ActionLevels: 41,
		CapacityWh: 10000, InitialSoCPct: 60, SoCMinPct: 10, SoCMaxPct: 95,
		MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 1, DischargeEfficiency: 1,
		Commercial: &CommercialSpec{BackupMinUsableEnergyWh: 4000},
	}
	plan := Optimize(slots, p)
	for i, a := range plan.Actions {
		if a.SoCPct < 40-0.5 {
			t.Errorf("slot %d plans SoC %.1f%% below the 40%% backup floor", i, a.SoCPct)
		}
	}
}
