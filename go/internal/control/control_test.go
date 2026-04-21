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
	targets := distributeProportional(bats, -1000, nil) // want -1000W total discharge; nil groupPV → capacity-only split
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
	targets := distributeProportional(bats, -200, nil)
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
	b := batteryInfo{soc: 0.04}
	v, was := clampWithSoC(-1000, b)
	if v != 0 || !was {
		t.Errorf("SoC<5%%: discharge should be blocked, got %f clamped=%v", v, was)
	}
	v, was = clampWithSoC(+1000, b)
	if v != 1000 || was {
		t.Error("charge at low SoC should pass through unchanged")
	}
}

func TestClampWithSoCCapsAtDefaultWhenLimitsUnset(t *testing.T) {
	// No per-driver limits → falls back to global MaxCommandW default.
	b := batteryInfo{soc: 0.5}
	v, was := clampWithSoC(+7000, b)
	if v != MaxCommandW || !was {
		t.Errorf("expected cap at +%d default, got %f", MaxCommandW, v)
	}
	v, was = clampWithSoC(-7000, b)
	if v != -MaxCommandW || !was {
		t.Errorf("expected cap at -%d default, got %f", MaxCommandW, v)
	}
}

// Per-driver limits override the global default. Charge + discharge can be
// asymmetric (hybrid inverters often are). Issue #145.
func TestClampWithSoCUsesPerBatteryLimits(t *testing.T) {
	b := batteryInfo{soc: 0.5, maxChargeW: 10000, maxDischargeW: 8000}
	// Charge up to 10 kW — passes through.
	if v, was := clampWithSoC(+9500, b); v != 9500 || was {
		t.Errorf("+9500 under 10 kW cap: got %f clamped=%v, want 9500 false", v, was)
	}
	// +12 kW above cap → clamped at 10 kW.
	if v, was := clampWithSoC(+12000, b); v != 10000 || !was {
		t.Errorf("+12000 vs 10 kW cap: got %f clamped=%v, want 10000 true", v, was)
	}
	// Discharge cap is separate (8 kW). -9 kW → clamped at -8 kW.
	if v, was := clampWithSoC(-9000, b); v != -8000 || !was {
		t.Errorf("-9000 vs 8 kW discharge cap: got %f clamped=%v, want -8000 true", v, was)
	}
}

// ---- Fuse guard ----

// Old-world test updated for the bidirectional predicted-grid guard
// (#145). Previous semantics ("PV + discharge > fuse → scale") assumed
// zero load and treated discharge as always pushing past the fuse. The
// new guard predicts site-boundary flow from live telemetry, which is
// physically accurate AND covers the charge side symmetrically.
func TestFuseGuardScalesDischargeWhenExportExceedsFuse(t *testing.T) {
	s := telemetry.NewStore()
	s.Update("meter", telemetry.DerMeter, -8000, nil, nil) // grid exporting 8 kW (PV dominant)
	s.DriverHealthMut("meter").RecordSuccess()
	s.Update("a", telemetry.DerBattery, 0, nil, nil)
	s.DriverHealthMut("a").RecordSuccess()
	targets := []DispatchTarget{{Driver: "a", TargetW: -6000}} // add 6 kW discharge on top
	// Predicted grid = -8000 - 0 + (-6000) = -14000 (exporting). Fuse 11040.
	scaled := applyFuseGuard(targets, s, "meter", 11040)
	if !scaled[0].Clamped {
		t.Error("expected clamped=true")
	}
	// Over by 14000 − 11040 = 2960. Discharge scales from 6000 → 6000 − 2960 = 3040.
	if math.Abs(scaled[0].TargetW-(-3040)) > 1 {
		t.Errorf("expected target ≈ -3040 after scaling, got %f", scaled[0].TargetW)
	}
}

// Small charge under the fuse → unchanged. No scaling.
func TestFuseGuardPassesThroughWhenWithinFuse(t *testing.T) {
	s := telemetry.NewStore()
	s.Update("meter", telemetry.DerMeter, 0, nil, nil)
	s.DriverHealthMut("meter").RecordSuccess()
	s.Update("a", telemetry.DerBattery, 0, nil, nil)
	s.DriverHealthMut("a").RecordSuccess()
	targets := []DispatchTarget{{Driver: "a", TargetW: 3000}}
	scaled := applyFuseGuard(targets, s, "meter", 11040)
	if scaled[0].TargetW != 3000 || scaled[0].Clamped {
		t.Errorf("within fuse: got %f clamped=%v, want 3000 false", scaled[0].TargetW, scaled[0].Clamped)
	}
}

// Charge side now protected (#145). With high load and aggressive
// charge targets, predicted grid import can exceed the fuse — the
// guard must scale charge down.
func TestFuseGuardScalesChargingWhenImportExceedsFuse(t *testing.T) {
	s := telemetry.NewStore()
	// Grid currently importing 8 kW (load-dominated, night/no PV).
	s.Update("meter", telemetry.DerMeter, 8000, nil, nil)
	s.DriverHealthMut("meter").RecordSuccess()
	s.Update("a", telemetry.DerBattery, 0, nil, nil)
	s.DriverHealthMut("a").RecordSuccess()
	s.Update("b", telemetry.DerBattery, 0, nil, nil)
	s.DriverHealthMut("b").RecordSuccess()
	targets := []DispatchTarget{
		{Driver: "a", TargetW: 5000},
		{Driver: "b", TargetW: 5000},
	}
	// Predicted = 8000 - 0 + 10000 = 18000 W. Fuse 11040.
	// Overage = 6960. Total charge = 10000. New total = 3040. Scale = 0.304.
	scaled := applyFuseGuard(targets, s, "meter", 11040)
	var totalCharge float64
	for _, tgt := range scaled {
		if tgt.TargetW > 0 {
			totalCharge += tgt.TargetW
		}
	}
	if math.Abs(totalCharge-3040) > 2 {
		t.Errorf("total charging after scaling = %f, want ≈ 3040", totalCharge)
	}
	for _, tgt := range scaled {
		if !tgt.Clamped {
			t.Errorf("%s: expected Clamped=true after charge scaling", tgt.Driver)
		}
	}
}

// Mirror of the charging test with no grid reading: guard can't predict
// reliably, stays conservative by NOT scaling (leave the decision to
// the upstream per-battery cap). Guards against a bare-metal test
// setup accidentally triggering the guard.
func TestFuseGuardNoOpWithoutMeterReading(t *testing.T) {
	s := telemetry.NewStore()
	targets := []DispatchTarget{{Driver: "a", TargetW: 5000}}
	// No meter reading → currentGrid defaults to 0 → predicted = 5000 which is fine.
	scaled := applyFuseGuard(targets, s, "does-not-exist", 11040)
	if scaled[0].TargetW != 5000 || scaled[0].Clamped {
		t.Errorf("absent meter → no prediction-based clamp, got %f clamped=%v",
			scaled[0].TargetW, scaled[0].Clamped)
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

// TestBatteryCoversEV_OffExcludesEVFromGrid mirrors the existing
// exclusion behaviour. Grid meter reads +3000 W, 2500 W is EV, so
// the effective grid the controller sees should be 500 W — well
// within the dead-band — and the battery should not try to cover
// the whole 3000 W. Regression guard that the new flag's default
// preserves current behaviour.
func TestBatteryCoversEV_OffExcludesEVFromGrid(t *testing.T) {
	store := seedStore(3000, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.EVChargingW = 2500
	st.BatteryCoversEV = false // explicit — this is the default
	st.SlewRateW = 100000
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) > 0 && math.Abs(targets[0].TargetW) > 2000 {
		t.Errorf("with flag off, battery must not try to cover 2500W EV draw; got target=%f", targets[0].TargetW)
	}
}

// TestBatteryCoversEV_OnIncludesEVInGrid covers the opt-in scenario:
// high grid prices now, cheap solar later → operator flips the
// override and the battery discharges to cover full grid load
// including EV. The dispatch should produce a target that actively
// pulls the battery into discharge territory for the full 3000 W
// import, not just the 500 W house portion.
func TestBatteryCoversEV_OnIncludesEVInGrid(t *testing.T) {
	store := seedStore(3000, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.EVChargingW = 2500
	st.BatteryCoversEV = true // opt in — battery covers everything
	st.SlewRateW = 100000
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) == 0 {
		t.Fatal("expected a dispatch target when grid is 3 kW over target, got none")
	}
	// With flag on, the full 3000 W import should drive battery toward
	// discharge (negative target in site convention). Require at least
	// half of the raw import as discharge command — conservative on the
	// PI gain but clearly separates "covers EV too" from "house only".
	if targets[0].TargetW > -1500 {
		t.Errorf("with flag on, battery must drive toward discharge for full import; got target=%f (want <= -1500)", targets[0].TargetW)
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
	// Battery at -2000 W (discharging), live grid exporting 500 W — under
	// the #153 idle-gate-override threshold, so the gate truly holds and
	// this test exercises the ramp-to-zero behaviour it was written to
	// guard. Heavier export would correctly trigger the override and
	// invalidate the assumption.
	store := seedStore(-500, []struct {
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

// ---- Per-driver power limits (#145) ----

// End-to-end via ModeCharge (the "fill every battery fully" knob):
// Ferroamp with max_charge_w=10000, Sungrow using the default. With
// per-driver caps, Ferroamp should be commanded at its 10 kW limit
// and Sungrow at the 5 kW default — previously both would have been
// pinned to 5 kW regardless of hardware capability.
func TestPerDriverLimits_ChargeModeRespectsAsymmetricCaps(t *testing.T) {
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
		{"sungrow", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModeCharge
	st.DriverLimits = map[string]PowerLimits{
		"ferroamp": {MaxChargeW: 10000, MaxDischargeW: 10000},
		// sungrow intentionally omitted → falls through to MaxCommandW default.
	}

	// Fuse set generously so the per-driver caps are what actually binds.
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200, "sungrow": 9600}), 50000)
	got := map[string]float64{}
	for _, t := range targets {
		got[t.Driver] = t.TargetW
	}
	if got["ferroamp"] != 10000 {
		t.Errorf("ferroamp = %f — ModeCharge should drive to per-driver MaxChargeW (10000)", got["ferroamp"])
	}
	if got["sungrow"] != 5000 {
		t.Errorf("sungrow = %f — expected the MaxCommandW default (5000) when no override", got["sungrow"])
	}
}

// A per-driver discharge cap is honoured separately from the charge
// cap. Real hybrid inverters commonly differ between the two directions.
func TestPerDriverLimits_AsymmetricChargeVsDischarge(t *testing.T) {
	// Grid importing 12 kW (high load → big discharge correction).
	store := seedStore(12000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	st.DriverLimits = map[string]PowerLimits{
		"ferroamp": {MaxChargeW: 15000, MaxDischargeW: 7000}, // asymmetric caps
	}

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 22080)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	// Must be discharging, and must NOT exceed -7000 (discharge cap).
	if targets[0].TargetW > 0 {
		t.Errorf("expected negative (discharge) target, got %f", targets[0].TargetW)
	}
	if targets[0].TargetW < -7000 {
		t.Errorf("target = %f — asymmetric discharge cap (7 kW) was not honoured", targets[0].TargetW)
	}
}

// ---- Inverter-affinity routing (#143) ----

// All charging flows to the group that has live PV — cross-inverter
// DC→AC→AC→DC is avoided.
func TestInverterAffinity_PrefersLocalBatteryForLocalSurplus(t *testing.T) {
	bats := []batteryInfo{
		{driver: "ferroamp", capacityWh: 15200, currentW: 0, soc: 0.5, online: true, group: "ferroamp"},
		{driver: "sungrow", capacityWh: 9600, currentW: 0, soc: 0.5, online: true, group: "sungrow"},
	}
	// All surplus on Ferroamp's inverter; none on Sungrow's.
	groupPV := map[string]float64{"ferroamp": 4000, "sungrow": 0}
	// +3 kW correction — fits entirely inside Ferroamp's local surplus.
	targets := distributeProportional(bats, 3000, groupPV)
	got := map[string]float64{}
	for _, t := range targets {
		got[t.Driver] = t.TargetW
	}
	if math.Abs(got["ferroamp"]-3000) > 1 {
		t.Errorf("ferroamp target = %f, want 3000 (all local routing)", got["ferroamp"])
	}
	if math.Abs(got["sungrow"]) > 1 {
		t.Errorf("sungrow target = %f, want 0 (no local PV, correction absorbable locally)", got["sungrow"])
	}
}

// When the operator demands more charge than the sum of local PV, the
// overflow is routed proportionally across the whole fleet. The
// locality bonus is exhausted first; nothing to gain after that.
func TestInverterAffinity_FallsBackToProportionalOnOverflow(t *testing.T) {
	bats := []batteryInfo{
		{driver: "ferroamp", capacityWh: 15200, currentW: 0, soc: 0.5, online: true, group: "ferroamp"},
		{driver: "sungrow", capacityWh: 9600, currentW: 0, soc: 0.5, online: true, group: "sungrow"},
	}
	// 4 kW of PV on ferroamp only; operator wants to charge 6 kW total.
	groupPV := map[string]float64{"ferroamp": 4000, "sungrow": 0}
	targets := distributeProportional(bats, 6000, groupPV)
	got := map[string]float64{}
	for _, t := range targets {
		got[t.Driver] = t.TargetW
	}
	// Ferroamp gets full 4 kW local + its capacity share of the 2 kW
	// overflow = 4000 + 2000 * 15200/24800 = 4000 + 1226 ≈ 5226.
	// clampWithSoC caps at 5000 per command.
	if got["ferroamp"] != 5000 {
		t.Errorf("ferroamp target = %f, want 5000 (local 4000 + overflow share, clamped at MaxCommandW)", got["ferroamp"])
	}
	// Sungrow gets its capacity share of the 2 kW overflow = 2000 * 9600/24800 ≈ 774.
	if math.Abs(got["sungrow"]-774) > 1 {
		t.Errorf("sungrow target = %f, want ≈774 (overflow × capacity share)", got["sungrow"])
	}
}

// With no inverter groups configured, the algorithm must produce
// identical results to today's capacity-proportional split — the
// backward-compat invariant.
func TestInverterAffinity_UngroupedBehavesAsBefore(t *testing.T) {
	bats := []batteryInfo{
		{driver: "a", capacityWh: 15200, currentW: 0, soc: 0.5, online: true}, // no group
		{driver: "b", capacityWh: 9600, currentW: 0, soc: 0.5, online: true},
	}
	// groupPV nil → "no locality info available" → fall back to
	// capacity-proportional.
	targets := distributeProportional(bats, 3000, nil)
	got := map[string]float64{}
	for _, t := range targets {
		got[t.Driver] = t.TargetW
	}
	wantA := 3000 * 15200.0 / 24800.0 // ~1838
	wantB := 3000 * 9600.0 / 24800.0  // ~1161
	if math.Abs(got["a"]-wantA) > 1 {
		t.Errorf("a = %f, want %f (proportional, no groups)", got["a"], wantA)
	}
	if math.Abs(got["b"]-wantB) > 1 {
		t.Errorf("b = %f, want %f", got["b"], wantB)
	}
}

// Discharge skips the locality math entirely — routing discharge to
// a group with PV buys nothing (discharge energy goes on the AC bus
// regardless of origin), and the simpler formula keeps behaviour
// predictable for multi-battery sites running in self_consumption
// during import peaks.
func TestInverterAffinity_DischargeStillProportional(t *testing.T) {
	bats := []batteryInfo{
		{driver: "ferroamp", capacityWh: 15200, currentW: 0, soc: 0.5, online: true, group: "ferroamp"},
		{driver: "sungrow", capacityWh: 9600, currentW: 0, soc: 0.5, online: true, group: "sungrow"},
	}
	// PV on ferroamp only — but it's night-time demand so we're discharging.
	groupPV := map[string]float64{"ferroamp": 4000, "sungrow": 0}
	targets := distributeProportional(bats, -2000, groupPV)
	got := map[string]float64{}
	for _, t := range targets {
		got[t.Driver] = t.TargetW
	}
	wantFerro := -2000 * 15200.0 / 24800.0 // ~-1226
	wantSun := -2000 * 9600.0 / 24800.0    // ~-774
	if math.Abs(got["ferroamp"]-wantFerro) > 1 {
		t.Errorf("ferroamp discharge = %f, want %f (proportional split on discharge)", got["ferroamp"], wantFerro)
	}
	if math.Abs(got["sungrow"]-wantSun) > 1 {
		t.Errorf("sungrow discharge = %f, want %f", got["sungrow"], wantSun)
	}
}

// End-to-end via ComputeDispatch: with InverterGroups wired on State
// and PV telemetry per driver, the dispatcher computes groupPV from
// live telemetry and routes charge preferentially. Guards against
// wiring bugs between the per-driver PV reading, the State map, and
// distributeProportional's input.
func TestInverterAffinity_EndToEndViaComputeDispatch(t *testing.T) {
	// Site exporting 3 kW surplus — PI will want to charge the fleet.
	store := seedStore(-3000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
		{"sungrow", 0, 0.5},
	})
	// All PV on Ferroamp's inverter; Sungrow's inverter has no PV right now.
	store.Update("ferroamp", telemetry.DerPV, -3500, nil, nil)
	store.DriverHealthMut("ferroamp").RecordSuccess()

	st := NewState(0, 0, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000 // unbounded so the formula is what we see
	st.MinDispatchIntervalS = 0
	st.InverterGroups = map[string]string{
		"ferroamp": "ferroamp",
		"sungrow":  "sungrow",
	}

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200, "sungrow": 9600}), 11040)
	got := map[string]float64{}
	for _, t := range targets {
		got[t.Driver] = t.TargetW
	}
	// Under plain proportional (no affinity) sungrow would receive
	// ~39% of the charge command (≈600 W on a 1.5 kW correction).
	// Under affinity sungrow gets ≈0 because ferroamp's local PV can
	// absorb the whole correction DC-direct.
	if got["sungrow"] > 200 {
		t.Errorf("sungrow target = %f — affinity should keep cross-inverter charge near zero when ferroamp's PV covers the surplus", got["sungrow"])
	}
	if got["ferroamp"] <= 0 {
		t.Errorf("ferroamp target = %f — should be charging since export + local PV = local routing opportunity", got["ferroamp"])
	}
}

// Issue #153: when planner_self's plan wants idle but live conditions
// are exporting well beyond any plausible forecast error, the gate
// flips off and reactive PI kicks in to absorb the unused surplus.
// Operator scenario that prompted this: forecast said 3.4 kW surplus,
// reality 10 kW; plan idled, batteries sat while 8 kW flowed out to
// grid at curtail pricing.
func TestPlannerSelfIdleGateOverridesOnLargeLiveSurplus(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0, // plan wants idle
		Strategy:        "self_consumption",
	}
	// Live: grid exporting 3 kW — well over the 1 kW override threshold.
	store := seedStore(-3000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	// With the override, reactive PI sees grid=-3000 and commands positive
	// charge to drive toward zero. Specifically NOT at 0 (which would be
	// the idle-gate behaviour).
	if targets[0].TargetW <= 200 {
		t.Errorf("TargetW = %f — expected positive charge from reactive PI after override, not idle-held at ~0", targets[0].TargetW)
	}
}

// The override must NOT fire when the forecast and reality roughly
// agree — plans can legitimately choose idle even with small live
// export (e.g., 500 W slack between PV and load), and the override
// flipping there would steamroll the DP's optimisation.
func TestPlannerSelfIdleGateHoldsWhenLiveSurplusUnderThreshold(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0,
		Strategy:        "self_consumption",
	}
	// Live: grid exporting 500 W — below the 1 kW override threshold.
	store := seedStore(-500, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	// Idle-gate held → totalCorrection = -currentTotal. Battery at 0 →
	// desired 0, no charge command.
	if math.Abs(targets[0].TargetW) > 1 {
		t.Errorf("TargetW = %f — idle-gate should hold with export below override threshold, want ~0", targets[0].TargetW)
	}
}

// Idle-gate override is one-directional — triggers only on export
// (negative grid). If live grid is importing, there's no unused surplus
// and the gate's "hold SoC for later" reasoning still applies.
func TestPlannerSelfIdleGateHoldsDuringImport(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0,
		Strategy:        "self_consumption",
	}
	// Live: grid importing 2 kW (load-dominated evening).
	store := seedStore(2000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	// Import side: override does nothing, idle-gate drives battery to 0.
	if math.Abs(targets[0].TargetW) > 1 {
		t.Errorf("TargetW = %f — override must not fire on import; idle-gate should hold at 0", targets[0].TargetW)
	}
}

// Codex P2 on PR #131: planner_cheap → planner_self → planner_cheap within
// the same 15-minute slot must not let the energy path read stale
// `slotDelivered` accumulated before the planner_self hop. If that leak
// happens, the second cheap cycle computes `remainingWh` off the pre-hop
// delivery number and over-commands battery for the rest of the slot.
func TestPlannerSelfResetsEnergyBookkeepingOnEntry(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 200,
		Strategy:        "arbitrage",
	}
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 800, 0.5}, // mid-charge so cheap path accumulates delivery
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	// 1. Run arbitrage — primes state.currentDirective / slotDelivered / lastTickTs.
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if st.currentDirective.SlotStart.IsZero() {
		t.Fatal("precondition: arbitrage cycle should have set currentDirective")
	}

	// 2. Operator flips to planner_self inside the same slot.
	st.Mode = ModePlannerSelf
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)

	// After planner_self runs the energy-path bookkeeping must be
	// cleared so a future cheap/arbitrage cycle can't read stale state.
	if !st.currentDirective.SlotStart.IsZero() {
		t.Errorf("currentDirective.SlotStart = %v after planner_self, want zero", st.currentDirective.SlotStart)
	}
	if st.slotDelivered != 0 {
		t.Errorf("slotDelivered = %f after planner_self, want 0", st.slotDelivered)
	}
	if !st.lastTickTs.IsZero() {
		t.Errorf("lastTickTs = %v after planner_self, want zero", st.lastTickTs)
	}

	// 3. Flip back to arbitrage — the SlotStart-equality branch should
	// no longer match, so the code takes the rollover-reset path
	// cleanly rather than accumulating off a frozen baseline.
	st.Mode = ModePlannerArbitrage
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	// The re-primed directive should equal the fresh one exactly (reset
	// branch fires), which implies slotDelivered starts from 0 again.
	if !st.currentDirective.SlotStart.Equal(dir.SlotStart) {
		t.Errorf("arbitrage cycle after planner_self didn't re-prime directive; SlotStart=%v want %v",
			st.currentDirective.SlotStart, dir.SlotStart)
	}
}
