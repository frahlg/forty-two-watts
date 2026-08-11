package mpc

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

type testPrimaryOptimizer struct{}

type failingPrimaryOptimizer struct{}

func (failingPrimaryOptimizer) Optimize(context.Context, []Slot, Params) (Plan, error) {
	return Plan{}, errors.New("test optimizer failure")
}

func (failingPrimaryOptimizer) Close() error { return nil }

func (testPrimaryOptimizer) Optimize(_ context.Context, slots []Slot, p Params) (Plan, error) {
	plan := Optimize(slots, p)
	plan.Solver = &SolverInfo{Engine: "cvxpy", Backend: "highs", Status: "optimal", SolveMs: 12}
	plan.OptimizerInput = json.RawMessage(`{"schema_version":1}`)
	return plan, nil
}

func (testPrimaryOptimizer) Close() error { return nil }

func (testPrimaryOptimizer) OptimizeRecourse(_ context.Context, slots []Slot, p Params, prefix int) (Plan, error) {
	plan := Optimize(slots, p)
	plan.Solver = &SolverInfo{
		Engine: "cvxpy", Backend: "highs", Status: "optimal",
		Formulation: "stochastic-recourse", ScenarioPolicy: "recourse",
		PolicyVersion:        "storage-recourse-v1",
		NonAnticipativeSlots: prefix,
	}
	return plan, nil
}

func (testPrimaryOptimizer) OptimizeMultistage(_ context.Context, slots []Slot, p Params, prefix int) (Plan, error) {
	plan := Optimize(slots, p)
	plan.Solver = &SolverInfo{
		Engine: "cvxpy", Backend: "highs", Status: "optimal",
		Formulation: "multistage-milp", ScenarioPolicy: "multistage",
		PolicyVersion:        "storage-multistage-v1",
		NonAnticipativeSlots: prefix,
		ServiceCVaRWeight:    1,
		ServiceCVaRAlpha:     0.95,
	}
	return plan, nil
}

// TestReplanCallsSaveDiag — after a successful replan, the SaveDiag
// hook fires once with (non-nil Diagnostic, reason). Verifies the hook
// wiring end-to-end without having to spin up the full stack.
func TestReplanCallsSaveDiag(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("Open state: %v", err)
	}
	defer st.Close()

	// Seed price and weather rows with fixed provenance. The weather row starts
	// before the price query margin but still covers the first price slot.
	now := time.Now().UTC().Truncate(time.Minute)
	priceStart := now.Add(-5 * time.Minute)
	priceFetchedAt := now.Add(-10 * time.Minute).UnixMilli()
	for i := 0; i < 4; i++ {
		err := st.SavePrices([]state.PricePoint{{
			Zone: "SE3", SlotTsMs: priceStart.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(),
			SlotLenMin: 15, SpotOreKwh: 50, TotalOreKwh: 100,
			Source: "entsoe", FetchedAtMs: priceFetchedAt,
		}})
		if err != nil {
			t.Fatalf("SavePrices: %v", err)
		}
	}
	weatherFetchedAt := now.Add(-20 * time.Minute).UnixMilli()
	pvW := 1200.0
	if err := st.SaveForecasts([]state.ForecastPoint{{
		SlotTsMs:   priceStart.Add(-30 * time.Minute).UnixMilli(),
		SlotLenMin: 60, PVWEstimated: &pvW,
		Source: "met.no", FetchedAtMs: weatherFetchedAt,
	}}); err != nil {
		t.Fatalf("SaveForecasts: %v", err)
	}

	svc := New(st, nil, "SE3", Params{
		Mode:                ModeSelfConsumption,
		SoCLevels:           11,
		CapacityWh:          10000,
		SoCMinPct:           10,
		SoCMaxPct:           95,
		InitialSoCPct:       50,
		ActionLevels:        5,
		MaxChargeW:          3000,
		MaxDischargeW:       3000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    80,
	})
	svc.BaseLoad = 500

	var called atomic.Int32
	var gotReason atomic.Value
	var gotZone atomic.Value
	svc.SaveDiag = func(d *Diagnostic, reason string) error {
		called.Add(1)
		gotReason.Store(reason)
		gotZone.Store(d.Zone)
		if len(d.Slots) == 0 {
			t.Error("Diagnostic.Slots empty — DP ran but no slots reached the snapshot")
		}
		js, err := json.Marshal(d)
		if err != nil {
			return err
		}
		return st.SaveDiagnostic(d.ComputedAtMs, reason, d.Zone,
			d.TotalCostOre, d.Horizon, string(js))
	}

	if plan := svc.Replan(context.Background()); plan == nil {
		t.Fatal("Replan returned nil — buildSlots likely empty")
	}
	if n := called.Load(); n != 1 {
		t.Errorf("SaveDiag called %d times, want 1", n)
	}
	if r, _ := gotReason.Load().(string); r == "" {
		t.Error("SaveDiag reason was empty")
	}
	if z, _ := gotZone.Load().(string); z != "SE3" {
		t.Errorf("SaveDiag zone = %q, want SE3", z)
	}
	row, err := st.LoadDiagnosticAt(time.Now().Add(time.Minute).UnixMilli())
	if err != nil || row == nil {
		t.Fatalf("LoadDiagnosticAt: row=%+v err=%v", row, err)
	}
	var persisted Diagnostic
	if err := json.Unmarshal([]byte(row.JSON), &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Slots) == 0 {
		t.Fatal("persisted diagnostic has no slots")
	}
	if persisted.InputProvenanceSchema != inputProvenanceSchemaVersion {
		t.Fatalf("input provenance schema = %d, want %d", persisted.InputProvenanceSchema, inputProvenanceSchemaVersion)
	}
	if got := persisted.Slots[0]; got.PriceInputSource != "entsoe" ||
		got.PriceInputAvailableAtMs != priceFetchedAt || got.WeatherRowSource != "met.no" ||
		got.WeatherRowAvailableAtMs != weatherFetchedAt {
		t.Fatalf("persisted input provenance = %+v", got)
	}
}

// TestReplanWithoutSaveDiagDoesNotPanic — the persistence hook is
// optional. A service without the hook must replan cleanly.
func TestReplanWithoutSaveDiagDoesNotPanic(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC().Truncate(time.Hour)
	_ = st.SavePrices([]state.PricePoint{{
		Zone: "SE3", SlotTsMs: now.UnixMilli(), SlotLenMin: 60,
		SpotOreKwh: 50, TotalOreKwh: 100, Source: "test",
		FetchedAtMs: now.UnixMilli(),
	}})
	svc := New(st, nil, "SE3", Params{
		Mode: ModeSelfConsumption, SoCLevels: 11, CapacityWh: 10000,
		SoCMinPct: 10, SoCMaxPct: 95, InitialSoCPct: 50,
		ActionLevels: 5, MaxChargeW: 2000, MaxDischargeW: 2000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	_ = svc.Replan(context.Background())
}

func TestReplanRetainsInputProvenanceAcrossOptimizerPaths(t *testing.T) {
	tests := []struct {
		name         string
		optimizer    PlanOptimizer
		wantFallback bool
	}{
		{name: "primary", optimizer: testPrimaryOptimizer{}},
		{name: "go fallback", optimizer: failingPrimaryOptimizer{}, wantFallback: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()

			now := time.Now().UTC().Truncate(time.Minute)
			priceStart := now.Add(-5 * time.Minute)
			priceFetchedAt := now.Add(-10 * time.Minute).UnixMilli()
			if err := st.SavePrices([]state.PricePoint{{
				Zone: "SE3", SlotTsMs: priceStart.UnixMilli(), SlotLenMin: 15,
				SpotOreKwh: 50, TotalOreKwh: 100,
				Source: "entsoe", FetchedAtMs: priceFetchedAt,
			}}); err != nil {
				t.Fatal(err)
			}
			weatherFetchedAt := now.Add(-20 * time.Minute).UnixMilli()
			pvW := 1200.0
			if err := st.SaveForecasts([]state.ForecastPoint{{
				SlotTsMs:   priceStart.Add(-30 * time.Minute).UnixMilli(),
				SlotLenMin: 60, PVWEstimated: &pvW,
				Source: "met.no", FetchedAtMs: weatherFetchedAt,
			}}); err != nil {
				t.Fatal(err)
			}

			svc := New(st, nil, "SE3", Params{
				Mode: ModeSelfConsumption, SoCLevels: 11, CapacityWh: 10000,
				SoCMinPct: 10, SoCMaxPct: 95, InitialSoCPct: 50,
				ActionLevels: 5, MaxChargeW: 2000, MaxDischargeW: 2000,
				ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
			})
			svc.BaseLoad = 500
			svc.Optimizer = tc.optimizer

			plan := svc.Replan(context.Background())
			if plan == nil || plan.Solver == nil {
				t.Fatalf("Replan returned %+v", plan)
			}
			if plan.Solver.Fallback != tc.wantFallback {
				t.Fatalf("fallback = %v, want %v; solver=%+v", plan.Solver.Fallback, tc.wantFallback, plan.Solver)
			}
			d := svc.Diagnose()
			if d == nil || len(d.Slots) != 1 {
				t.Fatalf("Diagnose returned %+v", d)
			}
			if d.InputProvenanceSchema != inputProvenanceSchemaVersion {
				t.Fatalf("input provenance schema = %d, want %d", d.InputProvenanceSchema, inputProvenanceSchemaVersion)
			}
			if got := d.Slots[0]; got.PriceInputSource != "entsoe" ||
				got.PriceInputAvailableAtMs != priceFetchedAt || got.WeatherRowSource != "met.no" ||
				got.WeatherRowAvailableAtMs != weatherFetchedAt {
				t.Fatalf("input provenance = %+v", got)
			}
		})
	}
}

func TestReplanLoadsHourlyWeatherCoveringCurrentPriceSlot(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC().Truncate(time.Minute)
	priceStart := now.Add(-5 * time.Minute)
	if err := st.SavePrices([]state.PricePoint{{
		Zone: "SE3", SlotTsMs: priceStart.UnixMilli(), SlotLenMin: 15,
		SpotOreKwh: 50, TotalOreKwh: 100, Source: "test",
		FetchedAtMs: now.UnixMilli(),
	}}); err != nil {
		t.Fatal(err)
	}
	pvW := 1234.0
	if err := st.SaveForecasts([]state.ForecastPoint{{
		SlotTsMs:   priceStart.Add(-30 * time.Minute).UnixMilli(),
		SlotLenMin: 60, PVWEstimated: &pvW, Source: "weather-test",
		FetchedAtMs: now.Add(-35 * time.Minute).UnixMilli(),
	}}); err != nil {
		t.Fatal(err)
	}

	svc := New(st, nil, "SE3", Params{
		Mode: ModeSelfConsumption, SoCLevels: 11, CapacityWh: 10000,
		SoCMinPct: 10, SoCMaxPct: 95, InitialSoCPct: 50,
		ActionLevels: 5, MaxChargeW: 2000, MaxDischargeW: 2000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	svc.BaseLoad = 500

	plan := svc.Replan(context.Background())
	if plan == nil || len(plan.Actions) != 1 {
		t.Fatalf("Replan returned %#v, want one action", plan)
	}
	if got := plan.Actions[0].PVW; got != -pvW {
		t.Fatalf("current slot PVW = %.1f, want %.1f from covering hourly row", got, -pvW)
	}
}

func TestPrimaryOptimizerKeepsDPAsDiagnosticShadow(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC().Truncate(time.Hour)
	for i := 0; i < 4; i++ {
		_ = st.SavePrices([]state.PricePoint{{
			Zone: "SE3", SlotTsMs: now.Add(time.Duration(i) * time.Hour).UnixMilli(),
			SlotLenMin: 60, SpotOreKwh: 50, TotalOreKwh: 100,
			Source: "test", FetchedAtMs: now.UnixMilli(),
		}})
	}
	svc := New(st, nil, "SE3", Params{
		Mode: ModePassiveArbitrage, SoCLevels: 11, CapacityWh: 10000,
		SoCMinPct: 10, SoCMaxPct: 95, InitialSoCPct: 50,
		ActionLevels: 5, MaxChargeW: 2000, MaxDischargeW: 2000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	svc.BaseLoad = 500
	svc.Optimizer = testPrimaryOptimizer{}
	svc.EnableRecourseShadow = true
	plan := svc.Replan(context.Background())
	if plan == nil || plan.Solver == nil || plan.Solver.Engine != "cvxpy" {
		t.Fatalf("primary plan not active: %+v", plan)
	}
	if plan.DPShadow == nil || plan.DPShadow.Solver == nil || plan.DPShadow.Solver.Engine != "go-dp" {
		t.Fatalf("DP shadow missing: %+v", plan.DPShadow)
	}
	if plan.DPShadow.ComparedSlots != len(plan.Actions) || plan.DPShadow.FirstAction == nil {
		t.Fatalf("shadow comparison incomplete: %+v", plan.DPShadow)
	}
	if plan.DPEvaluationShadow == nil || plan.DPEvaluationShadow.ForecastBasis != "same base forecast input" {
		t.Fatalf("same-input DP evaluation shadow missing: %+v", plan.DPEvaluationShadow)
	}
	if plan.RecourseShadow == nil || plan.RecourseShadow.Solver == nil || plan.RecourseShadow.Solver.ScenarioPolicy != "recourse" {
		t.Fatalf("recourse shadow missing: %+v", plan.RecourseShadow)
	}
	if plan.ShadowEvaluation == nil || plan.ShadowEvaluation.Status != "running" {
		t.Fatalf("stateful shadow evaluation missing: %+v", plan.ShadowEvaluation)
	}
	if d := svc.Diagnose(); d == nil || d.DPShadow == nil || d.DPEvaluationShadow == nil || d.RecourseShadow == nil || d.ShadowEvaluation == nil {
		t.Fatal("persisted diagnostic omitted a DP shadow")
	}
}

func TestPrimaryOptimizerCanSelectMultistageShadow(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC().Truncate(time.Hour)
	for i := 0; i < 4; i++ {
		_ = st.SavePrices([]state.PricePoint{{
			Zone: "SE3", SlotTsMs: now.Add(time.Duration(i) * time.Hour).UnixMilli(),
			SlotLenMin: 60, SpotOreKwh: 50, TotalOreKwh: 100,
			Source: "test", FetchedAtMs: now.UnixMilli(),
		}})
	}
	svc := New(st, nil, "SE3", Params{
		Mode: ModePassiveArbitrage, SoCLevels: 11, CapacityWh: 10000,
		SoCMinPct: 10, SoCMaxPct: 95, InitialSoCPct: 50,
		ActionLevels: 5, MaxChargeW: 2000, MaxDischargeW: 2000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	svc.BaseLoad = 500
	svc.Optimizer = testPrimaryOptimizer{}
	svc.EnableRecourseShadow = true
	svc.ChallengerPolicy = "multistage"
	svc.RecourseNonAnticipativeSlots = 1
	plan := svc.Replan(context.Background())
	if plan == nil || plan.Solver == nil || plan.Solver.ScenarioPolicy == "multistage" {
		t.Fatalf("challenger replaced active champion: %+v", plan)
	}
	if plan.RecourseShadow == nil || plan.RecourseShadow.Solver == nil || plan.RecourseShadow.Solver.ScenarioPolicy != "multistage" {
		t.Fatalf("multistage shadow missing: %+v", plan.RecourseShadow)
	}
}
