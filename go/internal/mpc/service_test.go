package mpc

import (
	"math"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
)

func TestBuildSlotsFallsBackToForecastWhenTwinCollapses(t *testing.T) {
	ts := time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC).UnixMilli()
	cloud := 48.1
	forecastPV := 1488.5770353837524
	slots := buildSlots(
		[]state.PricePoint{{
			SlotTsMs:    ts,
			SlotLenMin:  15,
			SpotOreKwh:  120,
			TotalOreKwh: 180,
		}},
		[]state.ForecastPoint{{
			SlotTsMs:      ts,
			SlotLenMin:    60,
			CloudCoverPct: &cloud,
			PVWEstimated:  &forecastPV,
		}},
		2500,
		ts,
		func(time.Time, float64) float64 { return 0 },
		nil,
	)
	if len(slots) != 1 {
		t.Fatalf("got %d slots, want 1", len(slots))
	}
	if got := slots[0].PVW; math.Abs(got+forecastPV) > 1e-6 {
		t.Fatalf("slot PVW = %f, want %f", got, -forecastPV)
	}
}

func TestBuildSlotsKeepsTwinWhenPredictionIsSane(t *testing.T) {
	ts := time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC).UnixMilli()
	cloud := 48.1
	forecastPV := 1488.5770353837524
	twinPV := 1180.0
	slots := buildSlots(
		[]state.PricePoint{{
			SlotTsMs:    ts,
			SlotLenMin:  15,
			SpotOreKwh:  120,
			TotalOreKwh: 180,
		}},
		[]state.ForecastPoint{{
			SlotTsMs:      ts,
			SlotLenMin:    60,
			CloudCoverPct: &cloud,
			PVWEstimated:  &forecastPV,
		}},
		2500,
		ts,
		func(time.Time, float64) float64 { return twinPV },
		nil,
	)
	if len(slots) != 1 {
		t.Fatalf("got %d slots, want 1", len(slots))
	}
	if got := slots[0].PVW; math.Abs(got+twinPV) > 1e-6 {
		t.Fatalf("slot PVW = %f, want %f", got, -twinPV)
	}
}

// ---- Terminal SoC valuation ----

func TestSelfConsumptionTerminalPriceIsImportMinusExport(t *testing.T) {
	// Retail 300 öre/kWh, spot 80 öre/kWh, bonus 60, fee 6.
	// Per slot: export rate = 80 + 60 − 6 = 134. Spread = 300 − 134 = 166.
	prices := []state.PricePoint{
		{SpotOreKwh: 80, TotalOreKwh: 300},
		{SpotOreKwh: 80, TotalOreKwh: 300},
	}
	got := selfConsumptionTerminalPrice(prices, 60, 6)
	if math.Abs(got-166) > 1e-9 {
		t.Fatalf("terminal price = %f, want 166", got)
	}
}

func TestSelfConsumptionTerminalPriceClampsToZero(t *testing.T) {
	// Export rate (spot+bonus−fee) > retail → spread would be negative.
	// Must floor at 0 so we never actively credit draining the battery.
	prices := []state.PricePoint{{SpotOreKwh: 500, TotalOreKwh: 100}}
	got := selfConsumptionTerminalPrice(prices, 0, 0)
	if got != 0 {
		t.Fatalf("terminal price = %f, want 0", got)
	}
}

func TestSelfConsumptionTerminalPriceEmpty(t *testing.T) {
	got := selfConsumptionTerminalPrice(nil, 0, 0)
	if got != 0 {
		t.Fatalf("terminal price = %f, want 0", got)
	}
}

// End-to-end proof: with the new self-consumption terminal valuation, a
// battery that's ≥50% full WILL discharge to cover load instead of
// choosing "idle — import to cover load". Regression test for the exact
// bug we saw on homelab-rpi (bat_w=0 on every slot with SoC=84%).
func TestOptimizeSelfConsumptionDischargesWithSpreadTerminalPrice(t *testing.T) {
	// 4-slot horizon, PV < load in every slot so battery has work to do.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 300, SpotOre: 80, LoadW: 3000, PVW: -500, Confidence: 1},
		{StartMs: 3600 * 1000, LenMin: 60, PriceOre: 300, SpotOre: 80, LoadW: 3000, PVW: -500, Confidence: 1},
		{StartMs: 7200 * 1000, LenMin: 60, PriceOre: 300, SpotOre: 80, LoadW: 3000, PVW: -500, Confidence: 1},
		{StartMs: 10800 * 1000, LenMin: 60, PriceOre: 300, SpotOre: 80, LoadW: 3000, PVW: -500, Confidence: 1},
	}

	// Build PricePoints identical to the slots and compute the
	// mode-appropriate terminal price. Mirrors what service.replan does.
	prices := []state.PricePoint{
		{SpotOreKwh: 80, TotalOreKwh: 300}, {SpotOreKwh: 80, TotalOreKwh: 300},
		{SpotOreKwh: 80, TotalOreKwh: 300}, {SpotOreKwh: 80, TotalOreKwh: 300},
	}
	p := baseParams(ModeSelfConsumption)
	p.InitialSoCPct = 80
	p.ExportBonusOreKwh = 60
	p.ExportFeeOreKwh = 6
	p.TerminalSoCPrice = selfConsumptionTerminalPrice(prices, 60, 6)

	plan := Optimize(slots, p)
	var discharging int
	for _, a := range plan.Actions {
		if a.BatteryW < -1e-6 {
			discharging++
		}
		if a.BatteryW > 1e-6 {
			t.Errorf("slot at %d charging %.0fW with no PV surplus", a.SlotStartMs, a.BatteryW)
		}
	}
	if discharging == 0 {
		t.Fatalf("expected at least one discharging slot with SoC=80%% and load>PV, got %+v", plan.Actions)
	}
}

// ---- Edge cases / hardening ----

func TestBuildSlotsEmptyForecast(t *testing.T) {
	ts := time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC).UnixMilli()
	slots := buildSlots(
		[]state.PricePoint{{
			SlotTsMs:    ts,
			SlotLenMin:  60,
			SpotOreKwh:  100,
			TotalOreKwh: 200,
		}},
		nil, // empty forecasts
		1500,
		ts,
		nil,
		nil,
	)
	if len(slots) != 1 {
		t.Fatalf("expected 1 slot, got %d", len(slots))
	}
	// With no forecast, PVW should be 0 (no panic).
	if slots[0].PVW != 0 {
		t.Errorf("expected PVW=0 with empty forecast, got %f", slots[0].PVW)
	}
	if slots[0].LoadW != 1500 {
		t.Errorf("expected LoadW=1500, got %f", slots[0].LoadW)
	}
}

func TestSelectPlannerPVWBothNaN(t *testing.T) {
	got := selectPlannerPVW(math.NaN(), math.NaN())
	if got != 0 {
		t.Errorf("both NaN should return 0, got %f", got)
	}
}

// Guardrail: if we had used the OLD default (mean retail import price as
// terminal value), the same scenario wouldn't discharge. This test exists
// to document *why* the fix matters.
func TestOptimizeSelfConsumptionDoesNotDischargeWithOldTerminalPrice(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 300, SpotOre: 80, LoadW: 3000, PVW: -500, Confidence: 1},
		{StartMs: 3600 * 1000, LenMin: 60, PriceOre: 300, SpotOre: 80, LoadW: 3000, PVW: -500, Confidence: 1},
	}
	p := baseParams(ModeSelfConsumption)
	p.InitialSoCPct = 80
	p.TerminalSoCPrice = 300 // old default = mean import price

	plan := Optimize(slots, p)
	for _, a := range plan.Actions {
		if a.BatteryW < -1e-6 {
			t.Fatalf("OLD behavior unexpectedly discharged — test is stale. got %+v", plan.Actions)
		}
	}
}
