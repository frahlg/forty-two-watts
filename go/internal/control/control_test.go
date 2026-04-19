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

func ptrF64(v float64) *float64 { return &v }

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

// Under planner_arbitrage — where the DP is explicitly allowed to export via
// battery — energy dispatch holds the plan even when live grid diverges.
// That's the point of "grid is the residual": arbitrage decides slot-by-slot
// that this_slot_W × slot_duration of battery energy is the cost-optimal
// cycle, and the EMS just executes it. Live export is a legal outcome.
//
// Contrast: under planner_self (see TestPlannerSelf* below) the same plan
// would be ignored in favour of reactive self-consumption — because that
// mode's contract is "never export via battery" regardless of what the
// forecast-based plan prescribes.
func TestEnergyDispatchHoldsPlanUnderArbitrage(t *testing.T) {
	now := time.Now()
	// Plan: discharge 552 Wh this slot. In W terms that's −2208.
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -552,
		Strategy:        "arbitrage",
	}
	store := seedStore(-1310, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -2200, 0.5},
	})
	store.Update("pv-1", telemetry.DerPV, -740, nil, nil)
	store.DriverHealthMut("pv-1").RecordSuccess()

	st := newStateWithEnergyDispatch(dir, "ferroamp")
	// Mode stays ModePlannerArbitrage from the helper — the point.

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	if got > -1900 || got < -2500 {
		t.Errorf("TargetW = %f W — arbitrage battery should follow plan (~−2208 W) regardless of live grid flow", got)
	}
}

// ---- planner_self reactive execution (issue #130) ----
//
// planner_self promises "never imports to charge, never exports via the
// battery" (UI tooltip + docs/mpc-planner.md). The DP enforces this on
// forecast, but the energy-allocation dispatch path honours plan Wh
// regardless of live grid flow — so when PV or load diverges from the
// forecast the battery can cross the zero-grid invariant both ways.
//
// The fix: planner_self bypasses energy-allocation and uses reactive
// self-consumption (PI → gridW=0), with the plan providing only a
// per-slot *idle gate*. When the DP decided not to participate this
// slot (|planned BatteryEnergyWh| < IdleGateThresholdW when averaged
// over the slot) the EMS holds the battery at 0 and lets PV flow to
// grid — deferring opportunity to a richer later slot.

// Motivating scenario (operator report 2026-04-19): plan wanted to charge
// aggressively (forecast said big PV surplus), but actual PV came in low.
// Under energy dispatch the battery imports from grid to hit the Wh budget.
// Under the fix, the battery only absorbs what's actually available.
func TestPlannerSelfReactsToForecastOverestimate(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 2000, // plan: charge 2000 Wh ≈ 8 kW avg
		Strategy:        "self_consumption",
	}
	// Live: grid exporting 300 W (actual PV minus actual load).
	// Under energy dispatch the battery would charge at ~8 kW and
	// drag grid to +7.7 kW import. Under reactive planner_self the
	// battery's charge is bounded by the live surplus (~300 W).
	store := seedStore(-300, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp") // tolerance=0 so PI fires
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 10000 // unbounded for single-tick formula test
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// Reactive PI on grid=-300 with Kp=0.5 yields ~+150 W charge. The
	// key assertion is "nowhere near the 8 kW the energy path would
	// command" — that's the bug.
	if got > 1000 {
		t.Errorf("TargetW = %f W — planner_self must NOT charge beyond live surplus (issue #130 regression). Plan said +8 kW but reality had ~300 W surplus.", got)
	}
	if got < 0 {
		t.Errorf("TargetW = %f W — expected modest charge absorbing live surplus, got discharge", got)
	}
}

// Idle-gate scenario: DP decided to sit this slot out (save SoC for a
// later, more profitable slot). Plan's Wh is below threshold → EMS holds
// battery at 0 even when live PV surplus exists.
func TestPlannerSelfIdleGateHoldsBatteryAtZero(t *testing.T) {
	now := time.Now()
	// Plan: avg ~0 W (well below IdleGateThresholdW=100).
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0,
		Strategy:        "self_consumption",
	}
	// Live: grid exporting 4 kW (real surplus exists). Battery currently
	// discharging 1 kW — should ramp toward 0, not toward absorbing
	// the surplus.
	store := seedStore(-4000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -1000, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 500 // realistic — expect one slew step from −1000 toward 0
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// Anchor on actual SmoothedW=-1000. Moving toward 0 by one slew
	// step of 500 W → -500 W. Not pushing toward -4000 to absorb surplus.
	if got < -500.001 || got > 0.001 {
		t.Errorf("TargetW = %f W — idle-gated battery should ramp toward 0 (expected [−500, 0]), not react to live surplus", got)
	}
}

// Plan says discharge this slot (above idle threshold) and live grid is
// importing. Reactive PI drives battery to cover live import exactly —
// not the planned Wh magnitude (which was larger because forecast load
// was higher than reality). Never crosses into export.
func TestPlannerSelfParticipatesReactivelyCoveringImport(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -800, // plan: discharge 800 Wh ≈ −3.2 kW avg
		Strategy:        "self_consumption",
	}
	// Live: importing 2 kW. Reactive PI wants to kill the import —
	// battery should discharge ~2 kW (NOT the planned −3.2 kW).
	store := seedStore(2000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 10000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	if got >= 0 {
		t.Errorf("TargetW = %f W — expected discharge (negative) to cover live import", got)
	}
	// Bounded by live import magnitude, not the plan's larger ask.
	// PI with Kp=0.5 on 2 kW error gives ~−1000 W first cycle.
	// Anything past −2500 W would be over-discharging toward export.
	if got < -2500 {
		t.Errorf("TargetW = %f W — reactive discharge should be bounded by live import (~2 kW), not the planned −3.2 kW", got)
	}
}

// Multi-cycle steady-state: idle-gated battery starts far from 0 and must
// reach 0 monotonically (no PI integral-windup overshoot, slew respected).
// Guards against the "gate goes on but PI wound up from earlier cycles
// keeps pushing" class of bug.
func TestPlannerSelfIdleGateRampsBatteryToZeroOverCycles(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0, // idle-gated
		Strategy:        "self_consumption",
	}
	// Battery at -2000 W (discharging), live grid exporting 4 kW.
	store := seedStore(-4000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -2000, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 500
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	// Simulate N cycles. Each cycle advances the battery's SmoothedW
	// toward the prev target so the slew anchor tracks reality.
	var last float64
	for i := 0; i < 10; i++ {
		targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
		if len(targets) != 1 {
			t.Fatalf("cycle %d: want 1 target, got %d", i, len(targets))
		}
		last = targets[0].TargetW
		// Fake the battery responding instantly to the new command.
		store.Update("ferroamp", telemetry.DerBattery, last, ptrF64(0.5), nil)
		store.DriverHealthMut("ferroamp").RecordSuccess()
	}
	if math.Abs(last) > 1 {
		t.Errorf("after 10 cycles with slew=500 from -2000 toward 0, expected final target ≈ 0, got %f", last)
	}
}

// EV load on the grid meter is subtracted from gridW before the PI kicks
// in (dispatch.go: `gridW := rawGridW - state.EVChargingW`). Verify that
// planner_self inherits this — an active EV should NOT drive the battery
// to cover EV charging.
func TestPlannerSelfRespectsEVChargingSignal(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -200, // plan above idle threshold
		Strategy:        "self_consumption",
	}
	// Grid reads +3000 (total import), but 3000 W of it is the EV.
	// Effective house gridW = 0 → battery should sit still.
	store := seedStore(3000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.EVChargingW = 3000
	st.SlewRateW = 10000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	// Effective house grid is 0 — within the 50 W deadband — dispatch skips.
	if len(targets) != 0 {
		t.Errorf("expected no dispatch when EV absorbs all import (effective gridW=0), got %d targets: %+v", len(targets), targets)
	}
}

// When the plan is absent (SlotDirective returns false) planner_self
// degrades to plain manual self_consumption — same behaviour as the
// operator gets today when they pick "Self-consumption" without planner.
func TestPlannerSelfWithoutPlanActsLikeManual(t *testing.T) {
	// Live: importing 1 kW — same setup as TestSelfConsumptionDischargesOnImport.
	store := seedStore(1000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 10000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return SlotDirective{}, false }
	st.PlanTarget = func(time.Time) (string, float64, bool) { return "", 0, false }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	if targets[0].TargetW >= 0 {
		t.Errorf("TargetW = %f W — planner_self with stale plan must still cover import (fall through to reactive self_consumption)", targets[0].TargetW)
	}
	if !st.PlanStale {
		t.Error("expected PlanStale=true when planner_self sees no directive")
	}
}
