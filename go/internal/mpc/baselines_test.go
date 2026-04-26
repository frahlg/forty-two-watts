package mpc

import (
	"math"
	"testing"
)

// Arbitrage over a clear cheap/expensive split should beat both
// no-battery and self-consumption baselines — that's the value of
// having the planner.
func TestBaselinesArbitrageBeatsNoBattery(t *testing.T) {
	// 4 hourly slots with a strong price dip at index 1 and a peak at 3.
	// Flat 1000W load, no PV. Arbitrage should buy cheap, sell expensive.
	prices := []float64{100, 20, 100, 300}
	slots := flatLoadSlots(prices)
	for i := range slots {
		slots[i].SpotOre = prices[i] // let export revenue scale with spot
	}
	p := baseParams(ModeArbitrage)
	p.InitialSoCPct = 50
	p.MaxChargeW = 3000
	p.MaxDischargeW = 3000

	plan := Optimize(slots, p)
	b := ComputeBaselines(slots, p)

	if b.NetKWh <= 0 {
		t.Fatalf("expected positive net kWh (flat load, no PV), got %f", b.NetKWh)
	}
	// Arbitrage plan must cost less than doing nothing (no battery).
	if plan.TotalCostOre >= b.NoBatteryOre {
		t.Errorf("arbitrage (%.1f öre) should beat no-battery (%.1f öre)",
			plan.TotalCostOre, b.NoBatteryOre)
	}
	// ...and less than passive self-consumption, which can't import at
	// price=20 to discharge at price=300.
	if plan.TotalCostOre >= b.SelfConsumptionOre {
		t.Errorf("arbitrage (%.1f öre) should beat self-consumption baseline (%.1f öre)",
			plan.TotalCostOre, b.SelfConsumptionOre)
	}
	// Average price over the horizon is the time-weighted mean.
	wantAvg := (100.0 + 20 + 100 + 300) / 4.0
	if math.Abs(b.AvgPriceOre-wantAvg) > 1e-6 {
		t.Errorf("avg price mismatch: got %f, want %f", b.AvgPriceOre, wantAvg)
	}
}

// NoBatteryOre must match analytical sum over slots at consumer price
// when all slots are importing. Locks down the cost model being shared
// with the DP's TotalCostOre path.
func TestBaselinesNoBatteryAnalytical(t *testing.T) {
	prices := []float64{50, 150, 100, 80}
	slots := flatLoadSlots(prices)
	p := baseParams(ModeArbitrage)

	b := ComputeBaselines(slots, p)

	// 1000W × 1h × price/1000 = price öre per slot. Sum over slots.
	want := 50.0 + 150 + 100 + 80
	if math.Abs(b.NoBatteryOre-want) > 1e-6 {
		t.Errorf("no-battery cost: got %.3f, want %.3f", b.NoBatteryOre, want)
	}
	// Net kWh = 4 slots × 1 kWh each.
	if math.Abs(b.NetKWh-4.0) > 1e-9 {
		t.Errorf("net kWh: got %.3f, want 4.0", b.NetKWh)
	}
}

// Self-consumption baseline must equal the cost of Optimize() called
// directly with ModeSelfConsumption on the same inputs. This is what
// makes the savings-vs-SC number honest — we're comparing to a real
// SC dispatch, not an approximation.
func TestBaselinesSelfConsumptionMatchesOptimize(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 80, LoadW: 1000, PVW: -2500, SpotOre: 80},
		{StartMs: 3_600_000, LenMin: 60, PriceOre: 120, LoadW: 1500, PVW: 0, SpotOre: 120},
		{StartMs: 7_200_000, LenMin: 60, PriceOre: 60, LoadW: 800, PVW: -300, SpotOre: 60},
		{StartMs: 10_800_000, LenMin: 60, PriceOre: 200, LoadW: 2000, PVW: 0, SpotOre: 200},
	}
	p := baseParams(ModeArbitrage) // doesn't matter — baseline overrides mode
	p.InitialSoCPct = 40

	pSC := p
	pSC.Mode = ModeSelfConsumption
	want := Optimize(slots, pSC).TotalCostOre

	got := ComputeBaselines(slots, p).SelfConsumptionOre
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("SC baseline mismatch: got %.6f, want %.6f (diff %.6f)",
			got, want, got-want)
	}
}

// Empty slot horizon returns a zero-valued Baselines without panicking.
func TestBaselinesEmpty(t *testing.T) {
	b := ComputeBaselines(nil, baseParams(ModeArbitrage))
	if b != (Baselines{}) {
		t.Errorf("expected zero Baselines for empty slots, got %+v", b)
	}
}

// FlatAvg on an export-heavy horizon must price the exported kWh at
// the spot mean (with bonus/fee), NOT the consumer-total mean. This
// is the regression test for the asymmetric-price bug: previously
// `flat_avg = netKWh × AvgPriceOre` overstated export revenue by the
// grid tariff + VAT on every PV-export-heavy day.
func TestFlatAvgUsesSpotForExports(t *testing.T) {
	// 4 hourly slots, all exporting 2 kW. Consumer total 250 öre/kWh
	// (e.g. 100 spot + 100 grid tariff + 50 VAT), spot 100 öre/kWh.
	// PVW dominates LoadW so gridKWh is negative every slot.
	prices := []float64{250, 250, 250, 250}
	spots := []float64{100, 100, 100, 100}
	slots := flatLoadSlots(prices) // load=1000W, no PV
	for i := range slots {
		slots[i].PVW = -3000 // 3 kW PV → net -2 kW per slot
		slots[i].SpotOre = spots[i]
	}
	p := baseParams(ModeArbitrage)
	b := ComputeBaselines(slots, p)

	// 4 slots × 2 kW × 1 h = 8 kWh exported.
	if b.NetKWh > -7.99 || b.NetKWh < -8.01 {
		t.Fatalf("net kWh = %.3f, want -8.0", b.NetKWh)
	}
	// AvgPriceOre stays at the consumer total (used elsewhere in the
	// UI). AvgSpotOre and AvgExportPriceOre are the new fields.
	if b.AvgPriceOre != 250 {
		t.Errorf("avg consumer-total price = %.1f, want 250", b.AvgPriceOre)
	}
	if b.AvgSpotOre != 100 {
		t.Errorf("avg spot = %.1f, want 100", b.AvgSpotOre)
	}
	if b.AvgExportPriceOre != 100 {
		t.Errorf("avg export price = %.1f, want 100 (spot, no bonus/fee)",
			b.AvgExportPriceOre)
	}
	// FlatAvg = imports × consumerTotal − exports × spot
	//        = 0 × 250 − 8 × 100
	//        = −800 öre  (revenue of 8 SEK)
	// The pre-fix bug would have given −8 × 250 = −2000 öre, which is
	// 2.5× more revenue than the operator can actually earn.
	if b.FlatAvgOre > -799.99 || b.FlatAvgOre < -800.01 {
		t.Errorf("flat-avg = %.2f, want -800 (8 kWh × 100 öre/kWh spot)", b.FlatAvgOre)
	}
}

// On a mixed horizon (some import slots, some export slots), the
// flat-avg breaks net into both directions and prices each at its own
// horizon mean.
func TestFlatAvgMixedDirectionHorizon(t *testing.T) {
	// 4 hourly slots: two importing at 1 kW, two exporting at 1 kW.
	// Consumer total 200 öre, spot 80 öre.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 200, SpotOre: 80, LoadW: 1000, PVW: 0},
		{StartMs: 3600_000, LenMin: 60, PriceOre: 200, SpotOre: 80, LoadW: 1000, PVW: 0},
		{StartMs: 7200_000, LenMin: 60, PriceOre: 200, SpotOre: 80, LoadW: 0, PVW: -1000},
		{StartMs: 10800_000, LenMin: 60, PriceOre: 200, SpotOre: 80, LoadW: 0, PVW: -1000},
	}
	p := baseParams(ModeArbitrage)
	b := ComputeBaselines(slots, p)

	// 2 kWh imported, 2 kWh exported, net 0.
	if math.Abs(b.NetKWh) > 1e-9 {
		t.Fatalf("net kWh = %.6f, want 0", b.NetKWh)
	}
	// FlatAvg = 2 × 200 − 2 × 80 = 400 − 160 = 240 öre.
	want := 240.0
	if math.Abs(b.FlatAvgOre-want) > 1e-6 {
		t.Errorf("flat-avg = %.3f, want %.3f", b.FlatAvgOre, want)
	}
}

// Operator-configured flat ExportOrePerKWh wins over spot-derived
// pricing — same precedence as SlotGridCostOre.
func TestFlatAvgFlatExportRateOverridesSpot(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 250, SpotOre: 100, LoadW: 0, PVW: -2000},
		{StartMs: 3600_000, LenMin: 60, PriceOre: 250, SpotOre: 100, LoadW: 0, PVW: -2000},
	}
	p := baseParams(ModeArbitrage)
	p.ExportOrePerKWh = 60 // operator's contracted feed-in rate
	b := ComputeBaselines(slots, p)

	if b.AvgExportPriceOre != 60 {
		t.Errorf("avg export price = %.1f, want 60 (flat rate)", b.AvgExportPriceOre)
	}
	// 4 kWh exported × 60 öre = 240 öre revenue → -240 in flat_avg.
	if math.Abs(b.FlatAvgOre+240) > 1e-6 {
		t.Errorf("flat-avg = %.3f, want -240 (4 kWh × 60 öre flat)", b.FlatAvgOre)
	}
}

// Negative effective export revenue (fee > spot + bonus) clamps to 0
// — same defensive guard as SlotGridCostOre. Without it the model
// would "reward" not exporting, which can break baseline comparisons.
func TestFlatAvgClampsNegativeExportRevenue(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 250, SpotOre: 10, LoadW: 0, PVW: -1000},
		{StartMs: 3600_000, LenMin: 60, PriceOre: 250, SpotOre: 10, LoadW: 0, PVW: -1000},
	}
	p := baseParams(ModeArbitrage)
	p.ExportFeeOreKwh = 50 // fee far exceeds spot
	b := ComputeBaselines(slots, p)

	// avg spot 10 + bonus 0 - fee 50 = -40 → clamped to 0.
	if b.AvgExportPriceOre != 0 {
		t.Errorf("clamped export price = %.1f, want 0", b.AvgExportPriceOre)
	}
	// 2 kWh exported × 0 = 0 export revenue. No imports.
	if b.FlatAvgOre != 0 {
		t.Errorf("flat-avg = %.3f, want 0 (export revenue clamped)", b.FlatAvgOre)
	}
}
