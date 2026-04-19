package control

import (
	"math"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// ---- PI controller ----

func TestPIProducesNegativeOutputWhenGridAboveTarget(t *testing.T) {
	p := NewPI(0.5, 0.1, 3000, 10000)
	p.Setpoint = 0
	// grid = +2000 (too much import)
	out := p.Update(2000)
	if out.Output >= 0 {
		t.Errorf("expected negative correction (→ more discharge), got %f", out.Output)
	}
	if out.Error != -2000 {
		t.Errorf("error should be setpoint-measurement = -2000, got %f", out.Error)
	}
}

func TestPIIntegralClampsAtLimit(t *testing.T) {
	p := NewPI(0, 100, 500, 10000) // only integral term, small limit
	p.Setpoint = 0
	// Feed a persistent error far beyond limit
	for i := 0; i < 100; i++ {
		p.Update(1000)
	}
	out := p.Update(1000)
	if math.Abs(out.I) > 500.0001 {
		t.Errorf("integral should be clamped to ±500, got %f", out.I)
	}
}

func TestPIReset(t *testing.T) {
	p := NewPI(0.5, 0.1, 3000, 10000)
	p.Setpoint = 0
	for i := 0; i < 10; i++ {
		p.Update(500)
	}
	p.Reset()
	out := p.Update(0)
	if out.I != 0 {
		t.Errorf("integral should be 0 after reset, got %f", out.I)
	}
}

// ---- Dispatch tests ----

// helper: build a store with one site meter + N batteries at given SoC
func seedStore(gridW float64, batteries []struct {
	name    string
	currentW, soc float64
}) *telemetry.Store {
	s := telemetry.NewStore()
	s.Update("ferroamp", telemetry.DerMeter, gridW, nil, nil)
	s.DriverHealthMut("ferroamp").RecordSuccess()
	for _, b := range batteries {
		soc := b.soc
		s.Update(b.name, telemetry.DerBattery, b.currentW, &soc, nil)
		s.DriverHealthMut(b.name).RecordSuccess()
	}
	return s
}

func caps(items map[string]float64) map[string]float64 { return items }

func TestIdleModeReturnsNothing(t *testing.T) {
	store := seedStore(2000, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeIdle
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 0 {
		t.Errorf("idle should dispatch nothing, got %d", len(targets))
	}
}

func TestChargeModeForcesAllBatteriesPositive5kW(t *testing.T) {
	store := seedStore(0, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
		{"sungrow", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeCharge
	targets := ComputeDispatch(store, st,
		caps(map[string]float64{"ferroamp": 15200, "sungrow": 9600}), 11040)
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(targets))
	}
	for _, tg := range targets {
		if tg.TargetW != 5000 {
			t.Errorf("charge mode should set +5000, got %f", tg.TargetW)
		}
	}
}

func TestDeadbandSkipsWithinTolerance(t *testing.T) {
	store := seedStore(30, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp") // tolerance 50W, error 30W → skip
	st.Mode = ModeSelfConsumption
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 0 {
		t.Errorf("within deadband should return nothing, got %d", len(targets))
	}
}

func TestSelfConsumptionDischargesOnImport(t *testing.T) {
	// grid = +1000 (importing too much) → want battery to discharge (negative target)
	store := seedStore(1000, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000 // big so slew doesn't interfere
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target")
	}
	if targets[0].TargetW >= 0 {
		t.Errorf("site convention: importing should lead to NEGATIVE (discharge) target, got %f",
			targets[0].TargetW)
	}
}

func TestSelfConsumptionChargesOnExport(t *testing.T) {
	// grid = -2000 (exporting) → want battery to charge (positive target)
	store := seedStore(-2000, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 { t.Fatal("expected 1 target") }
	if targets[0].TargetW <= 0 {
		t.Errorf("exporting should lead to POSITIVE (charge) target, got %f", targets[0].TargetW)
	}
}

func TestProportionalSplitByCapacity(t *testing.T) {
	bats := []batteryInfo{
		{driver: "big", capacityWh: 15000, currentW: 0, soc: 0.5, online: true},
		{driver: "small", capacityWh: 5000, currentW: 0, soc: 0.5, online: true},
	}
	targets := distributeProportional(bats, -1000) // want -1000W total discharge
	var big, small float64
	for _, tg := range targets {
		if tg.Driver == "big" { big = tg.TargetW }
		if tg.Driver == "small" { small = tg.TargetW }
	}
	// Big is 75%, small 25% → big = -750, small = -250
	if math.Abs(big+750) > 1 { t.Errorf("big got %f, want -750", big) }
	if math.Abs(small+250) > 1 { t.Errorf("small got %f, want -250", small) }
}

func TestProportionalUsesTotalDesired(t *testing.T) {
	// Both batteries currently at +500 (charging). Correction -200 means "reduce charging".
	// Expected: both end up at +400 each (half of +800 total desired).
	bats := []batteryInfo{
		{driver: "a", capacityWh: 10000, currentW: 500, soc: 0.5, online: true},
		{driver: "b", capacityWh: 10000, currentW: 500, soc: 0.5, online: true},
	}
	targets := distributeProportional(bats, -200)
	for _, tg := range targets {
		if math.Abs(tg.TargetW-400) > 1 {
			t.Errorf("%s: got %f, want 400", tg.Driver, tg.TargetW)
		}
	}
}

func TestPriorityDrainsPrimaryFirst(t *testing.T) {
	bats := []batteryInfo{
		{driver: "primary", capacityWh: 15000, currentW: 0, soc: 0.5, online: true},
		{driver: "secondary", capacityWh: 10000, currentW: 0, soc: 0.5, online: true},
	}
	// Small correction - primary should take it all
	targets := distributePriority(bats, -1000, []string{"primary", "secondary"})
	var p, s float64
	for _, tg := range targets {
		if tg.Driver == "primary" { p = tg.TargetW }
		if tg.Driver == "secondary" { s = tg.TargetW }
	}
	if math.Abs(p+1000) > 1 { t.Errorf("primary: got %f, want -1000", p) }
	if s != 0 { t.Errorf("secondary: got %f, want 0", s) }
}

func TestPriorityOverflowsToSecondary(t *testing.T) {
	bats := []batteryInfo{
		{driver: "primary", capacityWh: 15000, currentW: 0, soc: 0.5, online: true},
		{driver: "secondary", capacityWh: 10000, currentW: 0, soc: 0.5, online: true},
	}
	// Big correction - primary saturates at -5000 (per-command cap), rest spills
	targets := distributePriority(bats, -7000, []string{"primary", "secondary"})
	var p, s float64
	for _, tg := range targets {
		if tg.Driver == "primary" { p = tg.TargetW }
		if tg.Driver == "secondary" { s = tg.TargetW }
	}
	if p != -5000 { t.Errorf("primary: got %f, want -5000", p) }
	if math.Abs(s+2000) > 1 { t.Errorf("secondary: got %f, want -2000", s) }
}

func TestWeightedDistribution(t *testing.T) {
	bats := []batteryInfo{
		{driver: "a", capacityWh: 10000, currentW: 0, soc: 0.5, online: true},
		{driver: "b", capacityWh: 10000, currentW: 0, soc: 0.5, online: true},
	}
	weights := map[string]float64{"a": 0.8, "b": 0.2}
	targets := distributeWeighted(bats, 1000, weights)
	var a, b float64
	for _, tg := range targets {
		if tg.Driver == "a" { a = tg.TargetW }
		if tg.Driver == "b" { b = tg.TargetW }
	}
	if math.Abs(a-800) > 1 { t.Errorf("a: got %f, want 800", a) }
	if math.Abs(b-200) > 1 { t.Errorf("b: got %f, want 200", b) }
}

// ---- Clamps ----

func TestClampWithSoCBlocksDischargeWhenEmpty(t *testing.T) {
	v, was := clampWithSoC(-1000, 0.04)
	if v != 0 || !was {
		t.Errorf("SoC<5%%: discharge should be blocked, got %f clamped=%v", v, was)
	}
	v, was = clampWithSoC(+1000, 0.04)
	if v != 1000 || was {
		t.Error("charge at low SoC should pass through unchanged")
	}
}

func TestClampWithSoCCapsAt5kW(t *testing.T) {
	v, was := clampWithSoC(+7000, 0.5)
	if v != 5000 || !was { t.Errorf("expected cap at +5000, got %f", v) }
	v, was = clampWithSoC(-7000, 0.5)
	if v != -5000 || !was { t.Errorf("expected cap at -5000, got %f", v) }
}

// ---- Fuse guard ----

func TestFuseGuardScalesDischargeWhenPVHigh(t *testing.T) {
	s := telemetry.NewStore()
	s.Update("a", telemetry.DerPV, -8000, nil, nil) // site: PV = -8000 (generating)
	s.DriverHealthMut("a").RecordSuccess()
	targets := []DispatchTarget{{Driver: "a", TargetW: -6000}} // 6 kW discharge
	// PV 8000 + discharge 6000 = 14 kW > 11040 fuse
	scaled := applyFuseGuard(targets, s, 11040)
	if !scaled[0].Clamped {
		t.Error("expected clamped=true")
	}
	expected := -6000.0 * (11040.0 / 14000.0)
	if math.Abs(scaled[0].TargetW-expected) > 1 {
		t.Errorf("expected ~%.0f, got %f", expected, scaled[0].TargetW)
	}
}

func TestFuseGuardDoesNotScaleCharging(t *testing.T) {
	s := telemetry.NewStore()
	s.Update("a", telemetry.DerPV, -12000, nil, nil)
	targets := []DispatchTarget{{Driver: "a", TargetW: 3000}} // charging
	scaled := applyFuseGuard(targets, s, 11040)
	if scaled[0].TargetW != 3000 {
		t.Error("charging shouldn't be scaled by fuse guard (doesn't add to generation)")
	}
}

// ---- Full cycle ----

func TestFullCycleRespondsToTransient(t *testing.T) {
	store := seedStore(0, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0 // disable holdoff

	// Cycle 1: grid balanced, no action
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 0 {
		t.Error("balanced grid should not dispatch")
	}

	// Cycle 2: simulate a load step - grid rises to +1500
	store.Update("ferroamp", telemetry.DerMeter, 1500, nil, nil)
	time.Sleep(10 * time.Millisecond) // move past MinDispatchInterval=0
	targets = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatal("should dispatch on load step")
	}
	if targets[0].TargetW >= 0 {
		t.Errorf("import → discharge target (negative), got %f", targets[0].TargetW)
	}
}

func TestHoldoffBlocksRapidDispatch(t *testing.T) {
	store := seedStore(2000, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	now := time.Now()
	st.LastDispatch = &now
	st.MinDispatchIntervalS = 5
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 0 {
		t.Error("holdoff should block dispatch")
	}
}

func TestSetGridTargetUpdatesPI(t *testing.T) {
	st := NewState(0, 50, "ferroamp")
	st.SetGridTarget(-500)
	if st.GridTargetW != -500 { t.Errorf("state: %f", st.GridTargetW) }
	if st.PI.Setpoint != -500 { t.Errorf("pi setpoint: %f", st.PI.Setpoint) }
}

func TestEmptyBatteriesReturnsNoTargets(t *testing.T) {
	store := seedStore(1000, nil)
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	targets := ComputeDispatch(store, st, caps(map[string]float64{}), 11040)
	if len(targets) != 0 { t.Error("no batteries → no dispatch") }
}

func TestPeakShavingNoActionInBand(t *testing.T) {
	store := seedStore(3000, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePeakShaving
	st.PeakLimitW = 5000
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 0 {
		t.Errorf("within peak band should be no-op, got %d targets", len(targets))
	}
}

func TestPeakShavingActsWhenOverLimit(t *testing.T) {
	store := seedStore(7000, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePeakShaving
	st.PeakLimitW = 5000
	st.SlewRateW = 100000
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) == 0 {
		t.Error("over peak limit should dispatch")
	}
}

func TestEVChargingSignalExcludedFromGrid(t *testing.T) {
	// Grid = +3000 includes 2500W EV charging. Effective = +500W → within tolerance.
	store := seedStore(3000, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.EVChargingW = 2500
	st.SlewRateW = 100000
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	// Effective grid 500 → beyond 50 band, but small correction expected
	if len(targets) > 0 {
		// Allow small dispatch, but verify not trying to cover all 3000W
		if math.Abs(targets[0].TargetW) > 2000 {
			t.Errorf("EV-corrected dispatch should be modest, got %f", targets[0].TargetW)
		}
	}
}

func TestEVChargingSignalOverriddenByDerEVReading(t *testing.T) {
	// A DerEV driver reports 4000W. EVChargingW was 0 (no manual slider).
	// After ComputeDispatch, EVChargingW must reflect the live reading
	// so the dispatch clamp works against real hardware.
	store := seedStore(5000, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	store.Update("easee", telemetry.DerEV, 4000, nil, nil)
	store.DriverHealthMut("easee").RecordSuccess()
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.EVChargingW = 0
	st.SlewRateW = 100000
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if st.EVChargingW != 4000 {
		t.Errorf("expected EVChargingW to be overridden by live EV reading = 4000, got %f", st.EVChargingW)
	}
}

func TestEVChargingManualPreservedWhenNoDriver(t *testing.T) {
	// No DerEV reading. The manual slider value (1500W) must survive —
	// we don't want an offline / stale driver to silently zero it out.
	store := seedStore(1500, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.EVChargingW = 1500
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if st.EVChargingW != 1500 {
		t.Errorf("expected EVChargingW manual value 1500 to survive, got %f", st.EVChargingW)
	}
}

// ---- Slew rate anchor ----

// xorath's reported bug: battery at 10% SoC was commanded -5000 W the
// previous cycle but physically responded with 0 W (empty). When the user
// removed EV load creating surplus, the PI wanted +2000 W, but slew
// anchored on the stale -5000 W command capped new command at
// -5000 + 500 = -4500 W. Reversing direction took 5000/500 = 10 cycles
// (~50 s at 5 s interval). Anchoring slew on actual smoothed power
// (which was 0 W, not -5000 W) lets dispatch pivot within one slew step.
func TestSlewAnchorsOnActualNotStaleCommand(t *testing.T) {
	// Battery: previous command -5000 W, actual output 0 W (empty).
	// Grid: -2000 W (surplus → PI wants to charge the battery).
	store := seedStore(-2000, []struct{ name string; currentW, soc float64 }{
		{"pixii", 0, 0.10}, // at SoC min, actual bat_w = 0 despite command
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 500
	// Seed the stale command — this is what the dispatch stored after
	// last cycle, when it commanded -5000 W and the battery couldn't
	// comply.
	st.PrevTargets["pixii"] = -5000
	// Note: no battery called "ferroamp" in this store, so dispatch
	// skips it as unavailable. Only pixii is in the game.
	targets := ComputeDispatch(store, st, caps(map[string]float64{"pixii": 10000}), 11040)
	if len(targets) != 1 {
		t.Fatalf("expected 1 dispatch target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// With actual-anchored slew (anchor = 0 W), one step toward +W
	// should land the command in (0, +500]. With the old stale-anchored
	// slew it would land at -4500 W.
	if got < 0 {
		t.Errorf("expected positive (charge) target, got %f W — slew still anchored to stale command", got)
	}
	if got > st.SlewRateW+1e-6 {
		t.Errorf("expected target within one slew step of 0 W actual, got %f W", got)
	}
}

// Ensure normal in-tracking operation (actual ≈ command) still respects
// the slew limit from the actual. This prevents the fix from letting the
// PI jump more than slew_rate per cycle when the battery is tracking well.
func TestSlewRespectsRateWhenTracking(t *testing.T) {
	// Battery actively discharging at -1000 W, both actual and prev command.
	store := seedStore(1500, []struct{ name string; currentW, soc float64 }{
		{"pixii", -1000, 0.6},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 500
	st.PrevTargets["pixii"] = -1000
	// PI will want a big discharge to cover the +1500 import.
	targets := ComputeDispatch(store, st, caps(map[string]float64{"pixii": 10000}), 11040)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// Anchor is actual -1000; one slew step toward negative lands at -1500.
	if math.Abs(got-(-1500)) > 1e-6 {
		t.Errorf("expected slewed target = -1500 W (anchor -1000 + step -500), got %f W", got)
	}
}

// ---- Energy-allocation dispatch path (UseEnergyDispatch) ----

// newStateWithEnergyDispatch sets up a fresh State in planner_arbitrage mode
// with the energy-allocation path enabled. Slew + holdoff are relaxed so the
// test exercises the formula, not the rate limiter.
func newStateWithEnergyDispatch(dir SlotDirective, siteMeter string) *State {
	st := NewState(0, 0, siteMeter) // tolerance=0 so no deadband noise
	st.Mode = ModePlannerArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 100000 // effectively unbounded for the formula test
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }
	return st
}

// Core conversion: 200 Wh allocated, whole 15-min slot remaining,
// no energy delivered yet → target = 200 × 3600 / 900 = 800 W.
func TestEnergyDispatchConvertsWhToW(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 200,
		Strategy:        "arbitrage",
	}
	store := seedStore(0, []struct {
		name    string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := newStateWithEnergyDispatch(dir, "ferroamp")

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	// 200 Wh × 3600 s/h / 900 s ≈ 800 W. Small tolerance for time drift.
	if got := targets[0].TargetW; math.Abs(got-800) > 5 {
		t.Errorf("TargetW = %f, want ≈800 (200 Wh / 15 min)", got)
	}
}

// The motivating scenario (operator report 2026-04-17): forecast PV 700 W,
// actual 4800 W, plan wants to charge 200 Wh this slot. Under the legacy
// path the PI drives battery to ~3.9 kW charge (absorb everything) to hit
// grid_target. Under energy dispatch the battery stays at ~800 W and the
// 4 kW surplus flows to the grid.
func TestEnergyDispatchDoesNotAbsorbPVSurprise(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 200, // plan: charge 200 Wh
		Strategy:        "arbitrage",
	}
	// Grid exporting 4 kW (because PV 4.8 kW, load 0.8 kW, battery 0 W).
	// Under legacy PI with grid_target=−51 the controller would pull the
	// battery into aggressive charging to pin grid to −51.
	store := seedStore(-4000, []struct {
		name    string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	// PV telemetry so applyFuseGuard has something to count.
	store.Update("pv-1", telemetry.DerPV, -4800, nil, nil)
	store.DriverHealthMut("pv-1").RecordSuccess()

	st := newStateWithEnergyDispatch(dir, "ferroamp")

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	// Expect ~800 W charge, NOT multi-kW absorption. Flag anything beyond
	// 1.5 kW — that's 7× the plan's intent and signals the old bug.
	got := targets[0].TargetW
	if got > 1500 {
		t.Errorf("TargetW = %f W — battery is absorbing the PV surprise (regression). plan wanted ~800 W charge.", got)
	}
	if got < 100 {
		t.Errorf("TargetW = %f W — battery should still be charging per plan intent.", got)
	}
}

// Slot rollover: when SlotDirective returns a new SlotStart, the delivered
// accumulator must reset so the next slot starts from zero.
func TestEnergyDispatchResetsOnSlotRollover(t *testing.T) {
	now := time.Now()
	// First slot: mid-slot, accumulator should build up.
	dir1 := SlotDirective{
		SlotStart:       now.Add(-10 * time.Minute),
		SlotEnd:         now.Add(5 * time.Minute),
		BatteryEnergyWh: 200,
	}
	store := seedStore(0, []struct {
		name    string
		currentW, soc float64
	}{
		{"ferroamp", 800, 0.5}, // battery already charging 800 W
	})
	st := newStateWithEnergyDispatch(dir1, "ferroamp")

	// First call establishes the slot.
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)

	// New slot — different SlotStart. Should reset slotDelivered.
	dir2 := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 300,
	}
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir2, true }

	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)

	if st.slotDelivered != 0 {
		t.Errorf("slotDelivered = %f after slot rollover, want 0", st.slotDelivered)
	}
	if !st.currentDirective.SlotStart.Equal(dir2.SlotStart) {
		t.Errorf("currentDirective.SlotStart = %v, want %v", st.currentDirective.SlotStart, dir2.SlotStart)
	}
}

// Energy-dispatch must keep GridTargetW and PI.Setpoint in lockstep so
// the legacy path doesn't inherit a stale PI setpoint when the operator
// later switches out of a planner mode. Regression test for a P1
// raised on PR #79 (Codex).
func TestEnergyDispatchSyncsPISetpointWithGridTarget(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 200,
	}
	store := seedStore(0, []struct {
		name    string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := newStateWithEnergyDispatch(dir, "ferroamp")
	// Pre-poison the setpoint as if a manual mode had set it earlier —
	// this is what SetGridTarget needs to overwrite atomically.
	st.PI.Setpoint = 3000
	st.GridTargetW = 3000

	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)

	if st.PI.Setpoint != 0 {
		t.Errorf("PI.Setpoint = %f, want 0 after energy-dispatch cycle (stale setpoint would produce wrong corrections after mode switch)", st.PI.Setpoint)
	}
	if st.GridTargetW != 0 {
		t.Errorf("GridTargetW = %f, want 0", st.GridTargetW)
	}
}

// When the energy path is enabled but the plan is stale, the legacy path
// runs (PI on grid_target=0, self_consumption distribution). Verifies the
// fallback doesn't leave the path flag mis-set.
func TestEnergyDispatchFallsBackToLegacyWhenDirectiveUnavailable(t *testing.T) {
	store := seedStore(1000, []struct {
		name    string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePlannerArbitrage
	st.UseEnergyDispatch = true
	// Directive returns ok=false — plan stale.
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return SlotDirective{}, false }
	// Legacy fallback hook: return ok=false too, should route to the
	// "plan stale" self-consumption-with-grid-target=0 branch.
	st.PlanTarget = func(time.Time) (string, float64, bool) { return "", 0, false }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if !st.PlanStale {
		t.Error("expected PlanStale=true when both energy + legacy paths lack a plan")
	}
	// With grid = +1000 and target = 0, PI should command some charge-absorption
	// (negative correction if batteries > 0, toward discharge). Just assert the
	// path dispatched something.
	if len(targets) == 0 {
		t.Error("expected some dispatch under legacy fallback, got nothing")
	}
}

// Fredrik's morning incident (2026-04-19, self_consumption): planner
// forecasted load = 3.24 kW, actual load was ~300 W. Under the legacy
// PI-on-grid-target path the battery command converged on whatever
// would drive grid to 0, which after the load surprise meant a small
// discharge that exported straight to grid at the day-ahead low price.
// Under energy dispatch the battery follows the plan's energy
// allocation for the slot; if reality diverges the reactive replan
// takes over on the next cycle with updated forecasts. This test
// confirms the battery stays on plan even when live grid swings
// into export — i.e. "grid is the residual".
func TestEnergyDispatchHoldsPlanWhenLoadForecastWrong(t *testing.T) {
	now := time.Now()
	// Plan: discharge 552 Wh this slot (covers the forecasted 3.2 kW
	// load for 15 min minus PV generation). In W terms that's −2208.
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -552,
		Strategy:        "self_consumption",
	}
	// Grid is exporting 1.3 kW (actual load tiny, PV + plan's discharge
	// > load). Under legacy PI chasing grid_target=0 the controller
	// would back the battery off toward idle, missing the plan's intent.
	store := seedStore(-1310, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -2200, 0.5},
	})
	store.Update("pv-1", telemetry.DerPV, -740, nil, nil)
	store.DriverHealthMut("pv-1").RecordSuccess()

	st := newStateWithEnergyDispatch(dir, "ferroamp")
	st.Mode = ModePlannerSelf

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	// Plan says −2208 W average over the slot. Near slot start with
	// nothing delivered yet, the per-tick target should be close to
	// that. Accept a ±200 W band for time drift.
	got := targets[0].TargetW
	if got > -1900 || got < -2500 {
		t.Errorf("TargetW = %f W — battery should follow plan (~−2208 W) even when grid is exporting (regression: legacy PI would back off toward 0)", got)
	}
}
