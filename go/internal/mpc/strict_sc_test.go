package mpc

import "testing"

// TestStrictSelfConsumptionDischargesWhenSoCHealthy mirrors Fredrik's
// 2026-04-19 08:19 incident: self_consumption mode, ~50% SoC of a
// small battery, grid would import 2 kW at 166 öre. Before the
// strict-SC bias the DP happily idled and imported, treating
// "preserve SoC for later" as a better bet than "use battery now".
// After the bias, any action that reduces import should beat idle.
func TestStrictSelfConsumptionDischargesWhenSoCHealthy(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 166, SpotOre: 63,
			LoadW: 3480, PVW: -1390, Confidence: 1.0},
		{StartMs: 3600 * 1000, LenMin: 60, PriceOre: 165, SpotOre: 63,
			LoadW: 3480, PVW: -1985, Confidence: 1.0},
	}
	p := baseParams(ModeSelfConsumption)
	p.InitialSoCPct = 50
	p.TerminalSoCPrice = 107 // roughly selfConsumptionTerminalPrice output

	plan := Optimize(slots, p)
	if len(plan.Actions) == 0 {
		t.Fatal("no actions — plan empty")
	}
	first := plan.Actions[0]
	if first.BatteryW >= -100 {
		t.Errorf("strict SC expected battery discharge > 100 W, got BatteryW=%.1f "+
			"(GridW=%.1f, SoC=%.1f%%, reason=%q)",
			first.BatteryW, first.GridW, first.SoCPct, first.Reason)
	}
	// GridW should be lower than baseline 2090 W import — discharging
	// pulls it down.
	if first.GridW > 2000 {
		t.Errorf("GridW still near baseline (%.1f); battery not helping", first.GridW)
	}
}

// TestStrictSelfConsumptionDoesNotStarveEVDeadline is Codex P1 on
// PR #122: the original strict-SC bias multiplied the cost of every
// importing action by 3, including EV-charging import. At slot
// prices above ~4/3 × meanPrice the DP would prefer missing the
// EV's deadline (shortfall penalty = 4×meanPrice per kWh) over
// paying the tripled import cost. The fix scopes the bias to the
// HOUSE portion of the grid import only — EV import keeps its
// un-biased cost so the deadline-penalty comparison stays honest.
//
// Scenario: 3-slot horizon with a peak-price deadline slot.
// meanPrice = (400+100+100)/3 ≈ 200. Slot 0 price 400 = 2× mean,
// comfortably above the 4/3 breakpoint where the pre-fix bias
// would have chosen to miss the deadline. MaxDischargeW is
// limited to 1000 W so the battery physically CANNOT cover both
// house load and the EV's 2.5 kW draw — the DP has to import
// something, and the question is whether that import is priced
// under the SC bias (pre-fix → starves EV) or left at its plain
// cost (post-fix → DP charges to meet deadline).
func TestStrictSelfConsumptionDoesNotStarveEVDeadline(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 400, SpotOre: 200,
			LoadW: 500, PVW: -200, Confidence: 1.0},
		{StartMs: 3600 * 1000, LenMin: 60, PriceOre: 100, SpotOre: 40,
			LoadW: 500, PVW: -200, Confidence: 1.0},
		{StartMs: 2 * 3600 * 1000, LenMin: 60, PriceOre: 100, SpotOre: 40,
			LoadW: 500, PVW: -200, Confidence: 1.0},
	}
	p := baseParams(ModeSelfConsumption)
	p.InitialSoCPct = 80 // well above floor+20
	p.TerminalSoCPrice = 100
	p.MaxDischargeW = 1000 // battery can't cover load + EV alone
	p.MaxChargeW = 1000
	p.Loadpoint = &LoadpointSpec{
		ID:               "garage",
		CapacityWh:       10000,
		Levels:           11,
		MaxPct:           100,
		InitialSoCPct:    20,
		PluggedIn:        true,
		TargetSoCPct:     40, // need 2 kWh
		TargetSlotIdx:    0,  // deadline = slot 0
		MaxChargeW:       2500,
		AllowedStepsW:    []float64{0, 2500},
		ChargeEfficiency: 0.95,
	}
	plan := Optimize(slots, p)
	if len(plan.Actions) == 0 {
		t.Fatal("empty plan")
	}
	evW := plan.Actions[0].LoadpointW
	if evW < 1000 {
		t.Errorf("EV deadline starved: slot 0 LoadpointW=%.0f W (expected ≈ 2500). "+
			"Before the Codex fix the strict-SC import bias applied to EV energy, "+
			"making missed-deadline cost cheaper than charging at peak price.", evW)
	}
}

// TestUpdateCapacityPropagatesToDefaults covers Codex P1 on PR #121
// — the hot-reload path now pushes new totals into the running
// service so the next replan uses them instead of the startup
// snapshot. The field is mutex-protected because a reactive replan
// could race the reload.
func TestUpdateCapacityPropagatesToDefaults(t *testing.T) {
	s := &Service{Defaults: Params{CapacityWh: 99800, MaxChargeW: 11040, MaxDischargeW: 11040}}
	s.UpdateCapacity(24800, 8000, 8000)
	if s.Defaults.CapacityWh != 24800 {
		t.Errorf("CapacityWh = %f, want 24800", s.Defaults.CapacityWh)
	}
	if s.Defaults.MaxChargeW != 8000 {
		t.Errorf("MaxChargeW = %f, want 8000", s.Defaults.MaxChargeW)
	}
	if s.Defaults.MaxDischargeW != 8000 {
		t.Errorf("MaxDischargeW = %f, want 8000", s.Defaults.MaxDischargeW)
	}
	// Nil receiver must no-op, not panic.
	var nilSvc *Service
	nilSvc.UpdateCapacity(1, 2, 3)
}

// TestStrictSelfConsumptionBacksOffNearSoCFloor — below floor+20% the
// strict bias shouldn't kick in, so a nearly-empty battery doesn't
// suicide trying to cover a big load.
func TestStrictSelfConsumptionBacksOffNearSoCFloor(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 166, LoadW: 3480, PVW: -1390, Confidence: 1.0},
	}
	p := baseParams(ModeSelfConsumption)
	p.InitialSoCPct = 15  // just above min (10) but well below min+20
	p.TerminalSoCPrice = 200
	plan := Optimize(slots, p)
	// Either the DP idles or discharges only modestly — the key
	// property is that it's not forced to discharge aggressively.
	// We check that it doesn't push SoC down by more than a few
	// percent (action × 1h × 1/eff).
	if plan.Actions[0].SoCPct < 12 {
		t.Errorf("strict SC should respect min+20 floor; SoC dropped to %.1f",
			plan.Actions[0].SoCPct)
	}
}
