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
