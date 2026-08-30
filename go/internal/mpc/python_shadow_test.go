package mpc

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

// costShadowOptimizer answers with the DP's own plan re-labelled as the
// external solver, then overrides the two numbers the comparison is made of.
// That keeps the plan structurally valid while the cost difference stays a
// hand-computable constant.
type costShadowOptimizer struct {
	totalCostOre float64
	endSoC       float64
	calls        atomic.Int32
}

func (o *costShadowOptimizer) Optimize(_ context.Context, slots []Slot, p Params) (Plan, error) {
	o.calls.Add(1)
	plan := Optimize(slots, p)
	plan.TotalCostOre = o.totalCostOre
	if n := len(plan.Actions); n > 0 {
		plan.Actions[n-1].SoC = o.endSoC
	}
	plan.Solver = &SolverInfo{Engine: "cvxpy", Backend: "highs", Status: "optimal", SolveMs: 42}
	return plan, nil
}

func (o *costShadowOptimizer) Close() error { return nil }

type failingShadowOptimizer struct{ calls atomic.Int32 }

func (o *failingShadowOptimizer) Optimize(context.Context, []Slot, Params) (Plan, error) {
	o.calls.Add(1)
	return Plan{}, errors.New("worker socket unavailable")
}

func (o *failingShadowOptimizer) Close() error { return nil }

// blockingShadowOptimizer never answers until released. A champion replan that
// still returns while one of these is in flight is the proof the shadow cannot
// delay dispatch.
type blockingShadowOptimizer struct {
	release chan struct{}
	entered chan struct{}
	calls   atomic.Int32
}

func (o *blockingShadowOptimizer) Optimize(ctx context.Context, _ []Slot, _ Params) (Plan, error) {
	if o.calls.Add(1) == 1 {
		close(o.entered)
	}
	select {
	case <-o.release:
	case <-ctx.Done():
	}
	return Plan{}, errors.New("released without solving")
}

func (o *blockingShadowOptimizer) Close() error { return nil }

func shadowTestService(t *testing.T) *Service {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().UTC().Truncate(time.Hour)
	for i := 0; i < 4; i++ {
		if err := st.SavePrices([]state.PricePoint{{
			Zone: "SE3", SlotTsMs: now.Add(time.Duration(i) * time.Hour).UnixMilli(),
			SlotLenMin: 60, SpotOreKwh: 50 + float64(i)*40, TotalOreKwh: 100 + float64(i)*80,
			Source: "test", FetchedAtMs: now.UnixMilli(),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	svc := New(st, nil, "SE3", Params{
		Mode: ModePassiveArbitrage, SoCLevels: 11, CapacityWh: 10000,
		SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.5,
		ActionLevels: 5, MaxChargeW: 2000, MaxDischargeW: 2000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
		// Pinned so the terminal credit is an exact number the test can
		// assert by hand instead of a price-derived default.
		TerminalSoCPrice: 200,
	})
	svc.BaseLoad = 500
	return svc
}

// waitFor polls until cond holds. The shadow lands asynchronously by design,
// so tests wait for it instead of assuming an ordering.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestCoreChampionCarriesSolverIdentity pins what the UI reads to tell "core
// is the planner" from "the external planner failed and the DP caught it".
func TestCoreChampionCarriesSolverIdentity(t *testing.T) {
	svc := shadowTestService(t)
	plan := svc.Replan(context.Background())
	if plan == nil || plan.Solver == nil {
		t.Fatalf("Replan returned %+v", plan)
	}
	s := plan.Solver
	if s.Engine != "core" || s.Backend != "dp" || s.Status != "optimal" {
		t.Fatalf("solver identity = %+v", s)
	}
	if s.Fallback || s.FallbackReason != "" {
		t.Fatalf("core champion reported as a fallback: %+v", s)
	}
	if s.SoCLevels != 11 || s.ActionLevels != 5 {
		t.Fatalf("solver grid = %dx%d, want the params grid 11x5", s.SoCLevels, s.ActionLevels)
	}
	if s.SolveMs <= 0 {
		t.Fatalf("solve_ms = %v, want a measured duration", s.SolveMs)
	}
	// The diagnostic is the soak's evidence carrier, and it is read as JSON
	// offline — so assert the serialized shape, not just the struct. A blob
	// whose "solver" came back empty would be worthless for analysis.
	d := svc.Diagnose()
	if d == nil || d.Solver == nil {
		t.Fatalf("diagnostic solver missing: %+v", d)
	}
	blob, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var round Diagnostic
	if err := json.Unmarshal(blob, &round); err != nil {
		t.Fatal(err)
	}
	if round.Solver == nil {
		t.Fatalf("solver did not survive JSON: %s", blob)
	}
	if round.Solver.Engine != "core" || round.Solver.Backend != "dp" ||
		round.Solver.Status != "optimal" ||
		round.Solver.SoCLevels != 11 || round.Solver.ActionLevels != 5 ||
		round.Solver.SolveMs <= 0 {
		t.Fatalf("diagnostic solver = %+v", round.Solver)
	}
}

// TestPrimaryFailureStillMarksTheDPPlanAsFallback keeps the two states
// distinguishable now that both carry engine "core".
func TestPrimaryFailureStillMarksTheDPPlanAsFallback(t *testing.T) {
	svc := shadowTestService(t)
	svc.Optimizer = failingPrimaryOptimizer{}
	plan := svc.Replan(context.Background())
	if plan == nil || plan.Solver == nil {
		t.Fatalf("Replan returned %+v", plan)
	}
	if plan.Solver.Engine != "core" || !plan.Solver.Fallback ||
		plan.Solver.Status != "fallback" || plan.Solver.FallbackReason == "" {
		t.Fatalf("fallback solver = %+v", plan.Solver)
	}
}

// TestPythonShadowRecordsTerminalCorrectedComparison is the soak instrument:
// same inputs, one number per replan, on the Diagnostic that
// /api/mpc/diagnose/at hands out.
func TestPythonShadowRecordsTerminalCorrectedComparison(t *testing.T) {
	svc := shadowTestService(t)
	shadow := &costShadowOptimizer{totalCostOre: 1234, endSoC: 0.75}
	svc.ShadowOptimizer = shadow
	var saved atomic.Int32
	var withShadow atomic.Int32
	svc.SaveDiag = func(d *Diagnostic, _ string) error {
		saved.Add(1)
		if d.PythonShadow != nil {
			withShadow.Add(1)
		}
		return nil
	}

	plan := svc.Replan(context.Background())
	if plan == nil || plan.Solver == nil || plan.Solver.Engine != "core" {
		t.Fatalf("core champion missing: %+v", plan)
	}
	waitFor(t, "the python shadow to land", func() bool {
		d := svc.Diagnose()
		return d != nil && d.PythonShadow != nil
	})

	d := svc.Diagnose()
	block := d.PythonShadow
	if block.Solver == nil || block.Solver.Engine != "cvxpy" {
		t.Fatalf("shadow solver = %+v", block.Solver)
	}
	if block.ForecastBasis != "same downside input, python challenger" {
		t.Fatalf("forecast basis = %q", block.ForecastBasis)
	}
	if block.ComparedSlots != len(plan.Actions) || block.FirstAction == nil {
		t.Fatalf("comparison incomplete: %+v", block)
	}
	if block.TotalCostOre != 1234 {
		t.Fatalf("shadow raw cost = %v, want 1234", block.TotalCostOre)
	}
	if got, want := block.ActiveMinusShadowOre, plan.TotalCostOre-1234; got != want {
		t.Fatalf("raw core − python = %v, want %v", got, want)
	}

	// Hand value: corrected = raw − price·(SoC·capacity)/1000
	//                       = 1234 − 200·(0.75·10000)/1000 = 1234 − 1500 = −266.
	if d.Params.TerminalSoCPrice != 200 || d.Params.CapacityWh != 10000 {
		t.Fatalf("terminal economics moved: price=%v capacity=%v",
			d.Params.TerminalSoCPrice, d.Params.CapacityWh)
	}
	wantShadow := -266.0
	wantChampion := terminalCorrectedOre(plan.TotalCostOre,
		plan.Actions[len(plan.Actions)-1].SoC, Params{TerminalSoCPrice: 200, CapacityWh: 10000})
	if block.TerminalCorrectedOre != wantShadow {
		t.Fatalf("shadow corrected = %v, want %v", block.TerminalCorrectedOre, wantShadow)
	}
	if block.ActiveTerminalCorrectedOre != wantChampion {
		t.Fatalf("champion corrected = %v, want %v", block.ActiveTerminalCorrectedOre, wantChampion)
	}
	if got := block.ActiveMinusShadowTerminalCorrectedOre; got != wantChampion-wantShadow {
		t.Fatalf("corrected difference = %v, want %v", got, wantChampion-wantShadow)
	}
	if saved.Load() != 2 || withShadow.Load() != 1 {
		t.Fatalf("diagnostic writes = %d (%d carrying the shadow), want the replan's write plus one rewrite",
			saved.Load(), withShadow.Load())
	}
}

// TestPythonShadowFailureLeavesTheChampionAlone — a broken challenger costs
// the site nothing but a log line.
func TestPythonShadowFailureLeavesTheChampionAlone(t *testing.T) {
	svc := shadowTestService(t)
	shadow := &failingShadowOptimizer{}
	svc.ShadowOptimizer = shadow

	plan := svc.Replan(context.Background())
	if plan == nil || plan.Solver == nil || plan.Solver.Engine != "core" || plan.Solver.Fallback {
		t.Fatalf("shadow failure reached the champion plan: %+v", plan)
	}
	waitFor(t, "the failing shadow to be attempted", func() bool { return shadow.calls.Load() == 1 })
	waitFor(t, "the shadow slot to clear", func() bool {
		svc.mu.RLock()
		defer svc.mu.RUnlock()
		return !svc.shadowBusy
	})
	if d := svc.Diagnose(); d == nil || d.PythonShadow != nil {
		t.Fatalf("a failed shadow was recorded anyway: %+v", d)
	}
}

// TestPythonShadowNeverBlocksOrPilesUp: the replan returns while a challenger
// is stuck, and the next replan skips rather than queuing a second one.
func TestPythonShadowNeverBlocksOrPilesUp(t *testing.T) {
	svc := shadowTestService(t)
	shadow := &blockingShadowOptimizer{
		release: make(chan struct{}),
		entered: make(chan struct{}),
	}
	svc.ShadowOptimizer = shadow
	t.Cleanup(func() {
		close(shadow.release)
		waitFor(t, "the stuck shadow to unwind", func() bool {
			svc.mu.RLock()
			defer svc.mu.RUnlock()
			return !svc.shadowBusy
		})
	})

	if plan := svc.Replan(context.Background()); plan == nil {
		t.Fatal("replan did not return while the shadow was still solving")
	}
	select {
	case <-shadow.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("shadow never started")
	}
	if plan := svc.Replan(context.Background()); plan == nil {
		t.Fatal("second replan did not return")
	}
	// Give a queued-by-mistake second solve time to show up.
	time.Sleep(20 * time.Millisecond)
	if got := shadow.calls.Load(); got != 1 {
		t.Fatalf("shadow solves in flight = %d, want 1 — the busy guard let one pile up", got)
	}
}

// TestPythonShadowErrorsAreQuietAfterTheFirst keeps a missing worker from
// writing a warning every replan for weeks.
func TestPythonShadowErrorsAreQuietAfterTheFirst(t *testing.T) {
	svc := &Service{}
	err := errors.New("worker socket unavailable")
	base := time.Now()

	svc.logPythonShadowError(err, base)
	window := svc.shadowErrWindows[err.Error()]
	if window.openedAt != base || window.suppressed != 0 {
		t.Fatalf("first report = %+v", window)
	}
	for i := 1; i <= 3; i++ {
		svc.logPythonShadowError(err, base.Add(time.Duration(i)*time.Minute))
	}
	if got := svc.shadowErrWindows[err.Error()].suppressed; got != 3 {
		t.Fatalf("suppressed = %d, want 3", got)
	}
	svc.logPythonShadowError(err, base.Add(2*pythonShadowErrQuiet))
	window = svc.shadowErrWindows[err.Error()]
	if window.suppressed != 0 || !window.openedAt.After(base) {
		t.Fatalf("window did not reopen after the quiet period: %+v", window)
	}
}

// TestPythonShadowIgnoredWhileTheExternalOptimizerIsChampion — the external
// engine cannot shadow itself, and the DP shadows already cover that pairing.
func TestPythonShadowIgnoredWhileTheExternalOptimizerIsChampion(t *testing.T) {
	svc := shadowTestService(t)
	svc.Optimizer = testPrimaryOptimizer{}
	shadow := &costShadowOptimizer{totalCostOre: 1, endSoC: 0.5}
	svc.ShadowOptimizer = shadow

	plan := svc.Replan(context.Background())
	if plan == nil || plan.Solver == nil || plan.Solver.Engine != "cvxpy" {
		t.Fatalf("external champion missing: %+v", plan)
	}
	time.Sleep(20 * time.Millisecond)
	if got := shadow.calls.Load(); got != 0 {
		t.Fatalf("shadow ran behind an external champion %d times", got)
	}
	if d := svc.Diagnose(); d == nil || d.PythonShadow != nil {
		t.Fatalf("python shadow block recorded under a python champion: %+v", d)
	}
}

// TestCoreChampionClampsOutOfBandSoC — the field case: a driver discharge
// floor at 0.10 with the pack settling at 0.09 overnight. Core must plan, from
// the band edge, and say what it really read.
func TestCoreChampionClampsOutOfBandSoC(t *testing.T) {
	svc := shadowTestService(t)
	svc.Defaults.InitialSoC = 0.09
	svc.Defaults.SoCMin = 0.10

	plan := svc.Replan(context.Background())
	if plan == nil || plan.Solver == nil || plan.Solver.Engine != "core" {
		t.Fatalf("out-of-band SoC produced no Core plan: %+v", plan)
	}
	d := svc.Diagnose()
	if d == nil {
		t.Fatal("no diagnostic for the clamped plan")
	}
	if d.Params.InitialSoC != 0.10 {
		t.Fatalf("solved initial_soc = %v, want the clamped band edge 0.10", d.Params.InitialSoC)
	}
	if d.Params.InitialSoCUnclamped != 0.09 {
		t.Fatalf("initial_soc_unclamped = %v, want the real reading 0.09", d.Params.InitialSoCUnclamped)
	}
	for i, action := range plan.Actions {
		if action.SoC < 0.10-1e-9 {
			t.Fatalf("slot %d plans SoC %v below soc_min 0.10", i, action.SoC)
		}
	}
	// Baselines are computed again: the state is inside the band now, so this
	// is an ordinary solve rather than a recovery.
	if plan.Baselines == nil {
		t.Fatal("a clamped plan should carry baselines like any other plan")
	}
}

// TestCoreChampionClampsOutOfBandFleetMember — one battery below its own floor
// must not stop the fleet from being planned either.
func TestCoreChampionClampsOutOfBandFleetMember(t *testing.T) {
	svc := shadowTestService(t)
	configurePhysicsGateFleet(svc, 0.05, 0.55)

	plan := svc.Replan(context.Background())
	if plan == nil || plan.Solver == nil || plan.Solver.Engine != "core" {
		t.Fatalf("out-of-band fleet member produced no Core plan: %+v", plan)
	}
	d := svc.Diagnose()
	if d == nil || d.Params.InitialSoCUnclamped == 0 {
		t.Fatalf("clamp not recorded for the fleet: %+v", d)
	}
	if d.Params.InitialSoC < d.Params.SoCMin {
		t.Fatalf("solved from %v, still below soc_min %v", d.Params.InitialSoC, d.Params.SoCMin)
	}
}

// TestCoreChampionRefusesImpossibleSoC — broken telemetry is not a recoverable
// state, so the previous plan stands.
func TestCoreChampionRefusesImpossibleSoC(t *testing.T) {
	for name, soc := range map[string]float64{
		"above full": 1.5,
		"nan":        math.NaN(),
		"negative":   -0.2,
	} {
		t.Run(name, func(t *testing.T) {
			svc := shadowTestService(t)
			accepted := svc.Replan(context.Background())
			if accepted == nil {
				t.Fatal("baseline plan missing")
			}
			svc.Defaults.InitialSoC = soc
			got := svc.Replan(context.Background())
			if got != accepted || svc.Latest() != accepted {
				t.Fatalf("impossible SoC %v replaced the previous plan: got=%p accepted=%p",
					soc, got, svc.Latest())
			}
		})
	}
}

// TestClampParamsIntoOperatingBandKeepsTheFleetAggregateHonest — the storage
// aggregate identity validateStorageSpecs enforces (Σ initial_energy_wh =
// capacity × initial_soc) has to survive the clamp, because the Python shadow
// receives both halves.
func TestClampParamsIntoOperatingBandKeepsTheFleetAggregateHonest(t *testing.T) {
	p := validPlanningStorageParams()
	p.InitialSoC = 0.05
	p.Storages[0].InitialEnergyWh = p.CapacityWh * 0.05

	clamped, ok := clampParamsIntoOperatingBand(&p)
	if !ok || !clamped {
		t.Fatalf("clamp reported clamped=%v ok=%v, want true/true", clamped, ok)
	}
	if p.InitialSoC != p.SoCMin || p.InitialSoCUnclamped != 0.05 {
		t.Fatalf("aggregate = %v (unclamped %v), want %v", p.InitialSoC, p.InitialSoCUnclamped, p.SoCMin)
	}
	if got, want := p.Storages[0].InitialEnergyWh, p.CapacityWh*p.SoCMin; got != want {
		t.Fatalf("storage energy = %v, want the floor %v", got, want)
	}
	if err := validatePlanningParams(p); err != nil {
		t.Fatalf("clamped params no longer validate: %v", err)
	}

	// An in-band state is left exactly as it was.
	untouched := validPlanningStorageParams()
	before := untouched
	clamped, ok = clampParamsIntoOperatingBand(&untouched)
	if !ok || clamped {
		t.Fatalf("in-band clamp reported clamped=%v ok=%v, want false/true", clamped, ok)
	}
	if untouched.InitialSoC != before.InitialSoC || untouched.InitialSoCUnclamped != 0 {
		t.Fatalf("in-band params moved: %+v", untouched)
	}
}
