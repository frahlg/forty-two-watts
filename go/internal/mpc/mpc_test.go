package mpc

import (
	"math"
	"testing"
)

// baseParams = small-but-realistic problem for tests.
func baseParams(mode Mode) Params {
	return Params{
		Mode:                mode,
		SoCLevels:           21,
		CapacityWh:          10000, // 10 kWh
		SoCMinPct:           10,
		SoCMaxPct:           95,
		InitialSoCPct:       50,
		ActionLevels:        21,
		MaxChargeW:          5000,
		MaxDischargeW:       5000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    0, // neutral — force cost minimization
		ExportOrePerKWh:     0,
	}
}

// Helper: 4 slots × 60 min, no PV, flat 1000W load.
func flatLoadSlots(prices []float64) []Slot {
	out := make([]Slot, len(prices))
	for i, p := range prices {
		out[i] = Slot{
			StartMs:  int64(i) * 60 * 60 * 1000,
			LenMin:   60,
			PriceOre: p,
			PVW:      0,
			LoadW:    1000,
		}
	}
	return out
}

// ---- Mode: self_consumption ----

func TestSelfConsumptionNoGridCharge(t *testing.T) {
	// Flat load 1000W, no PV. In self_consumption we can only discharge
	// to cover load — we should NEVER import to charge.
	prices := []float64{100, 200, 50, 300} // cheap slot at index 2
	slots := flatLoadSlots(prices)
	p := baseParams(ModeSelfConsumption)
	p.InitialSoCPct = 80 // full-ish
	plan := Optimize(slots, p)
	for i, a := range plan.Actions {
		// In self-consumption with only load and no PV: baseline_grid = load = +1000.
		// grid_w must be in [0, 1000]. Battery must be ≤ 0 (discharge) or 0.
		if a.BatteryW > 1e-6 {
			t.Errorf("slot %d: charging %fW from grid in self_consumption (price %f)",
				i, a.BatteryW, a.PriceOre)
		}
		if a.GridW < -1e-6 || a.GridW > 1000+1e-6 {
			t.Errorf("slot %d: grid %fW outside [0,1000] in self_consumption", i, a.GridW)
		}
	}
}

func TestSelfConsumptionAbsorbsPVSurplus(t *testing.T) {
	// 2000W load, 3500W PV (1500W surplus). Battery should charge from surplus.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 100, LoadW: 2000, PVW: -3500},
	}
	p := baseParams(ModeSelfConsumption)
	p.InitialSoCPct = 50
	plan := Optimize(slots, p)
	a := plan.Actions[0]
	if a.BatteryW < 0 {
		t.Errorf("should charge from PV surplus, got %fW", a.BatteryW)
	}
	if a.GridW < -1e-6 {
		// We can tolerate a small exported fraction if action grid is coarse,
		// but gridW should not be more negative than -baseline (i.e. full surplus).
		if a.GridW < -1500-1e-6 {
			t.Errorf("grid export %fW exceeds surplus", a.GridW)
		}
	}
}

// ---- Mode: cheap_charge ----

func TestCheapChargeUsesCheapGrid(t *testing.T) {
	// Flat 1000W load, no PV. Prices 100,100,50,100,100,100. Cheap hour
	// is slot 2. The planner SHOULD charge in slot 2 to reduce import
	// later — but since there's no expensive hour later, it only helps
	// if we credit SoC at the terminal. Set a modest terminal credit.
	prices := []float64{100, 100, 50, 100, 100, 100}
	slots := flatLoadSlots(prices)
	p := baseParams(ModeCheapCharge)
	p.InitialSoCPct = 30
	p.TerminalSoCPrice = 100 // credit stored energy at 100 öre/kWh
	plan := Optimize(slots, p)

	cheapSlotBattery := plan.Actions[2].BatteryW
	expensiveSlotBattery := plan.Actions[0].BatteryW
	if cheapSlotBattery <= expensiveSlotBattery {
		t.Errorf("cheap_charge should charge more in cheap slot: cheap=%f expensive=%f",
			cheapSlotBattery, expensiveSlotBattery)
	}
}

func TestCheapChargeNeverExports(t *testing.T) {
	// With a very expensive slot, arbitrage would discharge to grid.
	// cheap_charge must not.
	prices := []float64{50, 50, 500, 50}
	slots := flatLoadSlots(prices)
	p := baseParams(ModeCheapCharge)
	p.InitialSoCPct = 90
	p.ExportOrePerKWh = 400 // tempting
	plan := Optimize(slots, p)
	for i, a := range plan.Actions {
		if a.GridW < -1e-6 {
			t.Errorf("slot %d: grid export %fW in cheap_charge", i, a.GridW)
		}
	}
}

// ---- Mode: arbitrage ----

func TestArbitrageDischargesToExpensive(t *testing.T) {
	// Charge cheap, export to grid during expensive hour.
	prices := []float64{50, 50, 500, 50}
	slots := flatLoadSlots(prices)
	// Force SoC to plenty, give meaningful export credit.
	p := baseParams(ModeArbitrage)
	p.InitialSoCPct = 80
	p.ExportOrePerKWh = 400
	plan := Optimize(slots, p)
	// Slot 2 (price 500) should see discharge (battery < 0).
	if plan.Actions[2].BatteryW >= -1e-6 {
		t.Errorf("arbitrage should discharge when price spikes: got %fW at price %f",
			plan.Actions[2].BatteryW, plan.Actions[2].PriceOre)
	}
}

// ---- Efficiency ----

func TestEfficiencyCostsSoC(t *testing.T) {
	// Charging 1000W × 1h with 95% eff should add 950Wh to SoC (9.5% of 10kWh).
	// Use fine-grained SoC buckets to avoid snap rounding.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 100, LoadW: 0, PVW: -1000},
	}
	p := baseParams(ModeArbitrage)
	p.SoCLevels = 171 // 0.5%-grid: (95-10)/170 = 0.5
	p.InitialSoCPct = 50
	p.ActionLevels = 11
	p.MaxChargeW = 1000
	p.MaxDischargeW = 0
	p.TerminalSoCPrice = 100 // give DP reason to charge (vs let PV waste)
	plan := Optimize(slots, p)
	a := plan.Actions[0]
	expected := 50.0 + (1000*1.0*0.95)/10000.0*100.0
	if math.Abs(a.SoCPct-expected) > 1.0 {
		t.Errorf("eff-aware SoC: got %f, want ~%f", a.SoCPct, expected)
	}
}

func TestRoundTripLossMakesArbitrageHarder(t *testing.T) {
	// Buy at 100, sell at 150, 50% round-trip → guaranteed loss (need ≥200
	// to break even). Start empty so the only way to "arbitrage" is charge
	// in slot 0 then sell in slot 1. Planner should hold.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 100, LoadW: 0, PVW: 0},
		{StartMs: 60 * 60 * 1000, LenMin: 60, PriceOre: 150, LoadW: 0, PVW: 0},
	}
	p := baseParams(ModeArbitrage)
	p.InitialSoCPct = 10 // empty
	p.ChargeEfficiency = 0.707
	p.DischargeEfficiency = 0.707
	p.ExportOrePerKWh = 150
	p.TerminalSoCPrice = 0
	plan := Optimize(slots, p)
	if math.Abs(plan.Actions[0].BatteryW) > 100 {
		t.Errorf("lossy arbitrage shouldn't charge from empty: slot0 batt=%f", plan.Actions[0].BatteryW)
	}
}

// ---- Output integrity ----

func TestGridEqualsLoadPlusPVPlusBattery(t *testing.T) {
	prices := []float64{100, 200, 50, 300}
	slots := flatLoadSlots(prices)
	plan := Optimize(slots, baseParams(ModeArbitrage))
	for i, a := range plan.Actions {
		want := a.LoadW + a.PVW + a.BatteryW
		if math.Abs(a.GridW-want) > 1e-6 {
			t.Errorf("slot %d: grid %f != load+pv+batt %f", i, a.GridW, want)
		}
	}
}

func TestSoCStaysInBounds(t *testing.T) {
	prices := []float64{50, 500, 50, 500, 50, 500, 50, 500}
	slots := flatLoadSlots(prices)
	p := baseParams(ModeArbitrage)
	p.ExportOrePerKWh = 400
	plan := Optimize(slots, p)
	for i, a := range plan.Actions {
		if a.SoCPct < p.SoCMinPct-1e-6 || a.SoCPct > p.SoCMaxPct+1e-6 {
			t.Errorf("slot %d: SoC %f outside [%f, %f]", i, a.SoCPct, p.SoCMinPct, p.SoCMaxPct)
		}
	}
}

func TestEmptySlotsReturnsEmptyPlan(t *testing.T) {
	plan := Optimize(nil, baseParams(ModeSelfConsumption))
	if len(plan.Actions) != 0 {
		t.Errorf("empty input should return empty plan, got %d actions", len(plan.Actions))
	}
}

// ---- Mode enforcement at boundary ----

// ---- Tariffs + export bonus ----

func TestImportTariffRaisesMPCImportCost(t *testing.T) {
	// Tariff-free vs heavy-tariff day: same spot, very different consumer
	// prices. cheap_charge should charge LESS aggressively when import
	// tariff is high (because grid import is more expensive).
	makeSlots := func(total float64) []Slot {
		s := make([]Slot, 4)
		for i := range s {
			s[i] = Slot{
				StartMs:  int64(i) * 3600 * 1000,
				LenMin:   60,
				PriceOre: total,
				LoadW:    500,
				PVW:      0,
			}
		}
		return s
	}
	p := baseParams(ModeCheapCharge)
	p.InitialSoCPct = 30
	p.TerminalSoCPrice = 100

	cheap := Optimize(makeSlots(50), p)  // low consumer price — grid-charge
	tariff := Optimize(makeSlots(300), p) // high consumer price — hold off

	var chgCheap, chgTariff float64
	for _, a := range cheap.Actions {
		chgCheap += math.Max(a.BatteryW, 0)
	}
	for _, a := range tariff.Actions {
		chgTariff += math.Max(a.BatteryW, 0)
	}
	if chgTariff >= chgCheap {
		t.Errorf("high-tariff charge (%.0fW) should be less than low-tariff charge (%.0fW)", chgTariff, chgCheap)
	}
}

func TestExportBonusMakesArbitrageMoreProfitable(t *testing.T) {
	// With a big export bonus, arbitrage should discharge MORE at
	// expensive hours because revenue per kWh is higher.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 50, LoadW: 500, PVW: 0},
		{StartMs: 3600 * 1000, LenMin: 60, PriceOre: 500, LoadW: 500, PVW: 0},
	}
	p := baseParams(ModeArbitrage)
	p.InitialSoCPct = 80
	p.TerminalSoCPrice = 0

	p.ExportOrePerKWh = 40
	lowBonus := Optimize(slots, p)

	p.ExportOrePerKWh = 200
	highBonus := Optimize(slots, p)

	if highBonus.TotalCostOre >= lowBonus.TotalCostOre {
		t.Errorf("high export bonus should yield more revenue (lower cost): low=%.1f high=%.1f",
			lowBonus.TotalCostOre, highBonus.TotalCostOre)
	}
}

// ---- Solar curtailment ----

func TestCurtailmentFlagsNegativeExportSlots(t *testing.T) {
	// Big PV surplus, no load absorption left (battery already full),
	// zero export revenue. Expect curtailment suggestion on those slots.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 10, LoadW: 500, PVW: -8000},
	}
	p := baseParams(ModeArbitrage)
	p.InitialSoCPct = 95 // already at max — battery can't absorb more
	p.ExportOrePerKWh = 0
	plan := Optimize(slots, p)
	a := plan.Actions[0]
	if a.PVLimitW == 0 {
		t.Errorf("expected curtailment on negative-export slot, got pv_limit_w=0 (grid_w=%f)", a.GridW)
	}
	// Recommended limit should roughly equal what the site can consume.
	expected := a.LoadW + math.Max(0, a.BatteryW)
	if math.Abs(a.PVLimitW-expected) > 500 {
		t.Errorf("pv_limit_w = %f, expected ~%f (load + charge)", a.PVLimitW, expected)
	}
}

func TestCurtailmentSkipsWhenExportProfitable(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 100, LoadW: 500, PVW: -8000},
	}
	p := baseParams(ModeArbitrage)
	p.InitialSoCPct = 95
	p.ExportOrePerKWh = 80 // profitable export
	plan := Optimize(slots, p)
	if plan.Actions[0].PVLimitW != 0 {
		t.Errorf("profitable export should not trigger curtailment, got pv_limit_w=%f",
			plan.Actions[0].PVLimitW)
	}
}

func TestSelfConsumptionWithZeroBaseline(t *testing.T) {
	// load==PV → baseline=0. Battery must stay at 0.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 100, LoadW: 2000, PVW: -2000},
	}
	p := baseParams(ModeSelfConsumption)
	plan := Optimize(slots, p)
	if math.Abs(plan.Actions[0].BatteryW) > 100 { // tolerance for action grid granularity
		t.Errorf("zero baseline should keep battery idle, got %f", plan.Actions[0].BatteryW)
	}
}
