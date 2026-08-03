package mpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/thermal"
)

type thermalOptimizerCall struct {
	slots []Slot
	loads []thermal.OptimizerLoad
}

type recordingThermalOptimizer struct {
	calls []thermalOptimizerCall
}

func (optimizer *recordingThermalOptimizer) Optimize(_ context.Context, slots []Slot, params Params) (Plan, error) {
	optimizer.calls = append(optimizer.calls, thermalOptimizerCall{
		slots: append([]Slot(nil), slots...),
		loads: append([]thermal.OptimizerLoad(nil), params.ThermalLoads...),
	})
	plan := exactIdleOptimizerPlan(slots, params)
	plan.Solver = &SolverInfo{Engine: "test", Backend: "replay", Status: "optimal"}
	plan.OptimizerInput = json.RawMessage(fmt.Sprintf(`{"thermal_loads":%d}`, len(params.ThermalLoads)))
	if len(params.ThermalLoads) == 0 {
		return plan, nil
	}

	states := make(map[string]thermal.ModelState, len(params.ThermalLoads))
	for _, load := range params.ThermalLoads {
		massC := load.InitialTempC
		if load.InitialMassTempC != nil {
			massC = *load.InitialMassTempC
		}
		states[load.ID] = thermal.ModelState{AirC: load.InitialTempC, MassC: massC}
	}
	for index := range plan.Actions {
		plan.Actions[index].ThermalPowerW = make(map[string]float64, len(params.ThermalLoads))
		plan.Actions[index].ThermalStateC = make(map[string]float64, len(params.ThermalLoads))
		for _, load := range params.ThermalLoads {
			next, err := load.NextState(index, states[load.ID], 0, float64(slots[index].LenMin)/60)
			if err != nil {
				return Plan{}, err
			}
			plan.Actions[index].ThermalPowerW[load.ID] = 0
			plan.Actions[index].ThermalStateC[load.ID] = next.AirC
			if load.ModelType == thermal.ModelType2R2C {
				if plan.Actions[index].ThermalMassStateC == nil {
					plan.Actions[index].ThermalMassStateC = make(map[string]float64)
				}
				plan.Actions[index].ThermalMassStateC[load.ID] = next.MassC
			}
			states[load.ID] = next
		}
	}
	if err := ValidatePlan(slots, params, &plan); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func (*recordingThermalOptimizer) Close() error { return nil }

func TestBuildSlotsCarriesOutdoorTemperature(t *testing.T) {
	now := int64(1)
	temperature := -4.5
	prices := []state.PricePoint{{SlotTsMs: now, SlotLenMin: 60, TotalOreKwh: 100}}
	forecasts := []state.ForecastPoint{{SlotTsMs: now, SlotLenMin: 60, TempC: &temperature}}
	slots := buildSlots(prices, forecasts, 500, now, nil, nil, nil)
	if len(slots) != 1 || slots[0].OutdoorTempC == nil || *slots[0].OutdoorTempC != temperature {
		t.Fatalf("slots = %+v", slots)
	}
}

func TestLookupTemperatureRejectsForecastGapAndExtrapolation(t *testing.T) {
	first := 5.0
	second := 6.0
	forecasts := []state.ForecastPoint{
		{SlotTsMs: 0, SlotLenMin: 60, TempC: &first},
		{SlotTsMs: 2 * 60 * 60 * 1_000, SlotLenMin: 60, TempC: &second},
	}
	if _, ok := lookupTemperature(forecasts, 90*60*1_000); ok {
		t.Fatal("forecast gap was filled with a stale temperature")
	}
	if _, ok := lookupTemperature(forecasts, 4*60*60*1_000); ok {
		t.Fatal("temperature was extrapolated beyond the forecast")
	}
}

func TestPrepareThermalPlanningReplacesLoadOnlyAfterAllInputsPass(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	outdoor := -5.0
	slots := []Slot{{StartMs: now.UnixMilli(), LenMin: 15, LoadW: 2_000, OutdoorTempC: &outdoor}}
	probe := func(time.Time, []thermal.ForecastSlot) ([]thermal.OptimizerLoad, error) {
		return []thermal.OptimizerLoad{{ID: "main"}}, nil
	}
	proposal, loads, err := prepareThermalPlanning(now, slots, func(time.Time) float64 { return 600 }, probe)
	if err != nil || len(loads) != 1 || len(proposal) != 1 || proposal[0].LoadW != 600 {
		t.Fatalf("prepareThermalPlanning = proposal %+v, loads %+v, err %v", proposal, loads, err)
	}
	if slots[0].LoadW != 2_000 {
		t.Fatalf("helper mutated whole-house fallback: %.0f", slots[0].LoadW)
	}
}

func TestPrepareThermalPlanningStartsAtNextCompleteSlot(t *testing.T) {
	hour := time.Unix(1_800_000_000, 0).Truncate(time.Hour)
	now := hour.Add(10 * time.Minute)
	outdoor := 5.0
	slots := []Slot{
		{StartMs: hour.UnixMilli(), LenMin: 60, LoadW: 2_000, OutdoorTempC: &outdoor},
		{StartMs: hour.Add(time.Hour).UnixMilli(), LenMin: 60, LoadW: 2_100, OutdoorTempC: &outdoor},
	}
	var forecast []thermal.ForecastSlot
	probe := func(_ time.Time, input []thermal.ForecastSlot) ([]thermal.OptimizerLoad, error) {
		forecast = append([]thermal.ForecastSlot(nil), input...)
		return []thermal.OptimizerLoad{{ID: "main"}}, nil
	}
	proposal, _, err := prepareThermalPlanning(now, slots, func(time.Time) float64 { return 600 }, probe)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal) != 1 || proposal[0].StartMs != slots[1].StartMs || len(forecast) != 1 || !forecast[0].Start.Equal(hour.Add(time.Hour)) {
		t.Fatalf("thermal proposal included an in-flight slot: proposal=%+v forecast=%+v", proposal, forecast)
	}
}

func TestPrepareThermalPlanningKeepsWholeLoadOnFailure(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	slots := []Slot{{StartMs: now.UnixMilli(), LenMin: 15, LoadW: 2_000}}
	probeErr := errors.New("indoor temperature is stale")
	probe := func(time.Time, []thermal.ForecastSlot) ([]thermal.OptimizerLoad, error) {
		return nil, probeErr
	}
	if _, _, err := prepareThermalPlanning(now, slots, func(time.Time) float64 { return 600 }, probe); !errors.Is(err, probeErr) {
		t.Fatalf("error = %v, want %v", err, probeErr)
	}
	if slots[0].LoadW != 2_000 {
		t.Fatalf("whole-house load changed after failure: %.0f", slots[0].LoadW)
	}

	validProbe := func(time.Time, []thermal.ForecastSlot) ([]thermal.OptimizerLoad, error) {
		return []thermal.OptimizerLoad{{ID: "main"}}, nil
	}
	if _, _, err := prepareThermalPlanning(now, slots, func(time.Time) float64 { return math.NaN() }, validProbe); err == nil {
		t.Fatal("invalid native load was accepted")
	}
}

func TestReplanKeepsThermalScheduleOutOfLivePlan(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Hour)
	temperatureC := 20.0
	for index := 0; index < 4; index++ {
		slotStart := now.Add(time.Duration(index) * time.Hour)
		if err := store.SavePrices([]state.PricePoint{{
			Zone: "SE3", SlotTsMs: slotStart.UnixMilli(), SlotLenMin: 60,
			SpotOreKwh: 50, TotalOreKwh: 100, Source: "test", FetchedAtMs: now.UnixMilli(),
		}}); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveForecasts([]state.ForecastPoint{{
			SlotTsMs: slotStart.UnixMilli(), SlotLenMin: 60, TempC: &temperatureC,
			Source: "test", FetchedAtMs: now.UnixMilli(),
		}}); err != nil {
			t.Fatal(err)
		}
	}

	optimizer := &recordingThermalOptimizer{}
	thermalOptimizer := &recordingThermalOptimizer{}
	service := New(store, nil, "SE3", Params{
		Mode: ModeSelfConsumption, SoCLevels: 11, CapacityWh: 10_000,
		SoCMinPct: 10, SoCMaxPct: 95, InitialSoCPct: 50,
		ActionLevels: 5, MaxChargeW: 2_000, MaxDischargeW: 2_000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	service.Load = func(time.Time) float64 { return 500 }
	service.LoadWithoutThermal = func(time.Time) float64 { return 200 }
	service.Optimizer = optimizer
	service.ThermalOptimizer = thermalOptimizer
	service.Thermal = func(_ time.Time, forecast []thermal.ForecastSlot) ([]thermal.OptimizerLoad, error) {
		capacity := 10_000.0
		outside := make([]float64, len(forecast))
		cop := make([]float64, len(forecast))
		disturbance := make([]float64, len(forecast))
		for index, slot := range forecast {
			if slot.OutdoorTempC == nil {
				return nil, fmt.Errorf("slot %d has no outdoor temperature", index)
			}
			outside[index] = *slot.OutdoorTempC
			cop[index] = 3
		}
		return []thermal.OptimizerLoad{{
			ID: "main", ModelType: thermal.ModelType1R1C,
			SourceRevision: strings.Repeat("a", 64),
			InitialTempC:   20, MinTempC: 18, MaxTempC: 23,
			OutsideTempC: outside, MaxPowerW: 3_000,
			HeatLossWPerK: 100, ThermalCapacityWhPerK: &capacity,
			COP: cop, DisturbanceHeatW: disturbance,
		}}, nil
	}

	plan := service.Replan(context.Background())
	if plan == nil {
		t.Fatal("Replan returned nil")
	}
	if len(optimizer.calls) != 1 || len(thermalOptimizer.calls) != 1 {
		t.Fatalf("optimizer calls = active %d, thermal %d, want one on each worker", len(optimizer.calls), len(thermalOptimizer.calls))
	}
	for _, slot := range optimizer.calls[0].slots {
		if slot.LoadW != 500 {
			t.Fatalf("active optimizer load = %.0f W, want whole-house 500 W", slot.LoadW)
		}
	}
	if len(optimizer.calls[0].loads) != 0 {
		t.Fatal("active optimizer received thermal loads")
	}
	for _, slot := range thermalOptimizer.calls[0].slots {
		if slot.LoadW != 200 {
			t.Fatalf("thermal shadow native load = %.0f W, want 200 W", slot.LoadW)
		}
	}
	if len(thermalOptimizer.calls[0].loads) != 1 {
		t.Fatalf("thermal shadow loads = %d, want 1", len(thermalOptimizer.calls[0].loads))
	}
	if hasThermalActions(plan.Actions) {
		t.Fatal("thermal actions leaked into the active plan")
	}
	if plan.ThermalProposal == nil || !hasThermalActions(plan.ThermalProposal.Actions) {
		t.Fatalf("validated thermal proposal missing: %+v", plan.ThermalProposal)
	}
	directiveAt := time.UnixMilli(plan.Actions[0].SlotStartMs).Add(time.Minute)
	service.mu.Lock()
	updated := *service.last
	updated.GeneratedAtMs = directiveAt.UnixMilli()
	service.last = &updated
	service.mu.Unlock()
	directive, ok := service.SlotDirectiveAt(directiveAt)
	if !ok {
		t.Fatal("active slot directive is missing")
	}
	if len(directive.ThermalEnergyWh) != 0 || len(directive.ThermalStateC) != 0 || len(directive.ThermalMassStateC) != 0 {
		t.Fatalf("thermal shadow leaked into live directive: %+v", directive)
	}
	diagnostic := service.Diagnose()
	if diagnostic == nil || diagnostic.ThermalProposal == nil ||
		string(diagnostic.ThermalOptimizerInput) != `{"thermal_loads":1}` {
		t.Fatalf("thermal shadow missing from diagnostic: %+v", diagnostic)
	}
}
