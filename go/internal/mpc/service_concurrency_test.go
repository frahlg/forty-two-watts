package mpc

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/thermal"
)

type supersededOptimizer struct {
	calls        atomic.Int32
	firstStarted chan struct{}
	releaseFirst chan struct{}
}

func (o *supersededOptimizer) Optimize(_ context.Context, slots []Slot, p Params) (Plan, error) {
	call := o.calls.Add(1)
	if call == 1 {
		close(o.firstStarted)
		<-o.releaseFirst // Deliberately ignore cancellation to test Core's guard.
	}
	plan := exactIdleOptimizerPlan(slots, p)
	plan.Solver = &SolverInfo{Engine: "test", Backend: "controlled", Status: "call-" + string(rune('0'+call))}
	return plan, nil
}

func (*supersededOptimizer) Close() error { return nil }

type countingPrimaryOptimizer struct{ calls atomic.Int32 }

func (o *countingPrimaryOptimizer) Optimize(_ context.Context, slots []Slot, p Params) (Plan, error) {
	o.calls.Add(1)
	plan := exactIdleOptimizerPlan(slots, p)
	plan.Solver = &SolverInfo{Engine: "test", Backend: "primary", Status: "optimal"}
	return plan, nil
}

func (*countingPrimaryOptimizer) Close() error { return nil }

type blockingThermalOptimizer struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (o *blockingThermalOptimizer) Optimize(ctx context.Context, slots []Slot, p Params) (Plan, error) {
	if o.calls.Add(1) == 1 {
		close(o.started)
	}
	<-o.release // Deliberately ignore cancellation; active planning must proceed.
	recorder := &recordingThermalOptimizer{}
	return recorder.Optimize(ctx, slots, p)
}

func (*blockingThermalOptimizer) Close() error { return nil }

func concurrencyTestService(t *testing.T) *Service {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC().Truncate(time.Hour)
	temperature := 20.0
	for index := 0; index < 6; index++ {
		start := now.Add(time.Duration(index) * time.Hour)
		if err := store.SavePrices([]state.PricePoint{{
			Zone: "SE3", SlotTsMs: start.UnixMilli(), SlotLenMin: 60,
			SpotOreKwh: 50, TotalOreKwh: 100, Source: "test", FetchedAtMs: now.UnixMilli(),
		}}); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveForecasts([]state.ForecastPoint{{
			SlotTsMs: start.UnixMilli(), SlotLenMin: 60, TempC: &temperature,
			Source: "test", FetchedAtMs: now.UnixMilli(),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	service := New(store, nil, "SE3", Params{
		Mode: ModeSelfConsumption, SoCLevels: 11, CapacityWh: 10_000,
		SoCMinPct: 10, SoCMaxPct: 95, InitialSoCPct: 50,
		ActionLevels: 5, MaxChargeW: 2_000, MaxDischargeW: 2_000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	service.BaseLoad = 1_000
	service.Horizon = 4 * time.Hour
	return service
}

func waitForCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func TestNewerReplanPreventsOlderResultFromPublishing(t *testing.T) {
	service := concurrencyTestService(t)
	optimizer := &supersededOptimizer{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	service.Optimizer = optimizer
	firstDone := make(chan *Plan, 1)
	go func() { firstDone <- service.ReplanWithReason(context.Background(), "older") }()
	<-optimizer.firstStarted

	secondDone := make(chan *Plan, 1)
	go func() { secondDone <- service.ReplanWithReason(context.Background(), "newer") }()
	waitForCondition(t, "new replan registration", func() bool {
		service.mu.RLock()
		defer service.mu.RUnlock()
		return service.replanGeneration == 2
	})
	close(optimizer.releaseFirst)

	if plan := <-firstDone; plan != nil {
		t.Fatalf("superseded replan returned a publishable plan: %+v", plan.Solver)
	}
	if plan := <-secondDone; plan == nil || plan.Solver == nil || plan.Solver.Status != "call-2" {
		t.Fatalf("new replan did not win: %+v", plan)
	}
	latest := service.Latest()
	if latest == nil || latest.Solver == nil || latest.Solver.Status != "call-2" {
		t.Fatalf("latest plan was overwritten by old work: %+v", latest)
	}
	_, reason := service.LastReplanInfo()
	if reason != "newer" {
		t.Fatalf("last reason = %q, want newer", reason)
	}
}

func TestBlockedThermalShadowDoesNotBlockNextActivePlan(t *testing.T) {
	service := concurrencyTestService(t)
	primary := &countingPrimaryOptimizer{}
	shadow := &blockingThermalOptimizer{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	service.Optimizer = primary
	service.ThermalOptimizer = shadow
	service.LoadWithoutThermal = func(time.Time) float64 { return 600 }
	service.Thermal = func(_ time.Time, slots []thermal.ForecastSlot) ([]thermal.OptimizerLoad, error) {
		outside := make([]float64, len(slots))
		cop := make([]float64, len(slots))
		disturbance := make([]float64, len(slots))
		for index := range slots {
			outside[index] = 20
			cop[index] = 3
		}
		capacity := 10_000.0
		return []thermal.OptimizerLoad{{
			ID: "main", ModelType: thermal.ModelType1R1C,
			SourceRevision: strings.Repeat("a", 64), InitialTempC: 20,
			MinTempC: 19, MaxTempC: 23, OutsideTempC: outside,
			MaxPowerW: 4_000, HeatLossWPerK: 100,
			ThermalCapacityWhPerK: &capacity, COP: cop,
			DisturbanceHeatW: disturbance,
		}}, nil
	}

	firstDone := make(chan *Plan, 1)
	go func() { firstDone <- service.ReplanWithReason(context.Background(), "first") }()
	<-shadow.started
	if service.Latest() == nil {
		t.Fatal("active plan was not published before thermal shadow started")
	}

	secondDone := make(chan *Plan, 1)
	go func() { secondDone <- service.ReplanWithReason(context.Background(), "second") }()
	waitForCondition(t, "second primary solve", func() bool {
		return primary.calls.Load() >= 2
	})
	if latest := service.Latest(); latest == nil {
		t.Fatal("second active solve did not publish while shadow was blocked")
	}
	close(shadow.release)
	<-firstDone
	if plan := <-secondDone; plan == nil {
		t.Fatal("second replan returned nil")
	}
}
