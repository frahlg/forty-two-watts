package control

import (
	"math"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// Reactive fuse-saver tests. These guard the contract: under no
// software-controllable circumstance should the operator's hardware
// fuse trip because the EMS sat idle while the meter went over the
// limit. The PR description has the full background — surfaced by
// the manual_hold ramp test where the EV was pinned at high amps
// while the home battery was idle per the planner's slot.

func setupFuseSaver(gridW, batW float64, batSoC float64, maxDischargeW float64) (*telemetry.Store, *State, map[string]float64) {
	s := telemetry.NewStore()
	s.Update("meter", telemetry.DerMeter, gridW, nil, nil)
	s.DriverHealthMut("meter").RecordSuccess()
	soc := batSoC
	s.Update("bat", telemetry.DerBattery, batW, &soc, nil)
	s.DriverHealthMut("bat").RecordSuccess()
	st := NewState(0, 50, "meter")
	st.DriverLimits = map[string]PowerLimits{
		"bat": {MaxChargeW: 10000, MaxDischargeW: maxDischargeW},
	}
	return s, st, map[string]float64{"bat": 15200}
}

// Idle battery + grid surge over fuse → battery is forced to discharge
// to bring grid back under the fuse. This is the manual_hold-ramp
// scenario from the PR description.
func TestFuseSaverForcesDischargeFromIdle(t *testing.T) {
	// Grid importing 14 kW (e.g. EV pinned at 11 kW + house 3 kW),
	// fuse 11.04 kW. Battery currently idle.
	store, state, caps := setupFuseSaver(14000, 0, 0.6, 10000)
	targets := []DispatchTarget{{Driver: "bat", TargetW: 0}}
	out := forceFuseDischarge(targets, store, state, caps, 11040)
	if len(out) != 1 {
		t.Fatalf("expected 1 target, got %d", len(out))
	}
	// Predicted = 14000 - 0 + 0 = 14000. Overage = 14000 - 11040 = 2960.
	// Battery has 10 kW of discharge headroom → absorb full overage.
	expected := -2960.0
	if math.Abs(out[0].TargetW-expected) > 1 {
		t.Errorf("target = %.0f, want %.0f (full overage absorbed by idle battery)",
			out[0].TargetW, expected)
	}
	if !out[0].Clamped {
		t.Errorf("Clamped flag must mark fuse-saver activation")
	}
}

// In production the call chain is applyFuseGuard THEN
// forceFuseDischarge. When the planner asks for charge while the grid
// is already at the fuse, applyFuseGuard scales the charge down toward
// 0; forceFuseDischarge then sees a small (or zero) target and either
// no-ops or adds a tiny extra discharge to close the residual gap.
// The standalone-flip-to-discharge behaviour the previous version of
// this test asserted doesn't happen on the real path — applyFuseGuard
// would never let a +3 kW charge reach forceFuseDischarge with grid
// already at the fuse.
func TestFuseSaverAfterFuseGuardKeepsScaledCharge(t *testing.T) {
	// Live grid near the fuse limit; planner asked to charge 3 kW;
	// battery currently idle.
	store, state, caps := setupFuseSaver(11000, 0, 0.6, 10000)
	targets := []DispatchTarget{{Driver: "bat", TargetW: 3000}}

	// Step 1: applyFuseGuard scales charging down because predicted
	// import (11000 + 3000 = 14000) exceeds the fuse.
	guarded := applyFuseGuard(targets, store, "meter", 11040)
	if guarded[0].TargetW < 0 {
		t.Fatalf("applyFuseGuard should NOT flip charge to discharge — "+
			"got %.0f W", guarded[0].TargetW)
	}
	if guarded[0].TargetW > 100 {
		t.Fatalf("applyFuseGuard should have scaled charge down toward 0, "+
			"got %.0f W (charge surviving the fuse guard is a regression)",
			guarded[0].TargetW)
	}

	// Step 2: forceFuseDischarge runs on the post-guard targets. With
	// charge already at ~0 the predicted gridW after the guard is at
	// the fuse limit; forceFuseDischarge no-ops or adds a small
	// residual discharge. Either way it must NOT take the target
	// further negative than -2960 W (the original overage).
	out := forceFuseDischarge(guarded, store, state, caps, 11040)
	if out[0].TargetW < -2960.001 {
		t.Errorf("post-guard discharge over-correction: target = %.0f W, "+
			"original overage was 2960 W", out[0].TargetW)
	}
}

// Battery already commanded to discharge a bit; fuse-saver TOPS UP the
// discharge instead of starting from scratch.
func TestFuseSaverAddsToExistingDischarge(t *testing.T) {
	store, state, caps := setupFuseSaver(13000, 0, 0.6, 10000)
	targets := []DispatchTarget{{Driver: "bat", TargetW: -1000}}
	// Predicted = 13000 - 0 + (-1000) = 12000. Over fuse by 960.
	out := forceFuseDischarge(targets, store, state, caps, 11040)
	expected := -1960.0
	if math.Abs(out[0].TargetW-expected) > 1 {
		t.Errorf("target = %.0f, want %.0f (existing discharge plus overage)",
			out[0].TargetW, expected)
	}
}

// Empty pack (SoC < 5%) → can't be drained. Function returns the
// targets unchanged. Hardware fuse is the next layer.
func TestFuseSaverRespectsEmptyBattery(t *testing.T) {
	store, state, caps := setupFuseSaver(14000, 0, 0.02, 10000)
	targets := []DispatchTarget{{Driver: "bat", TargetW: 0}}
	out := forceFuseDischarge(targets, store, state, caps, 11040)
	if out[0].TargetW != 0 || out[0].Clamped {
		t.Errorf("empty battery should not be drained: got %.0f clamped=%v",
			out[0].TargetW, out[0].Clamped)
	}
}

// MaxDischargeW caps the fuse-saver — never command more discharge
// than the battery can physically deliver, even if the fuse would
// otherwise be violated by even more.
func TestFuseSaverRespectsMaxDischarge(t *testing.T) {
	// Massive overage (10 kW), but battery can only discharge 4 kW.
	store, state, caps := setupFuseSaver(20000, 0, 0.6, 4000)
	targets := []DispatchTarget{{Driver: "bat", TargetW: 0}}
	out := forceFuseDischarge(targets, store, state, caps, 11040)
	if out[0].TargetW < -4000.001 {
		t.Errorf("target = %.0f, exceeds MaxDischargeW=4000",
			out[0].TargetW)
	}
}

// Within fuse → no-op. The fuse-saver doesn't touch dispatch when
// predicted gridW is already safe.
func TestFuseSaverNoOpWhenWithinFuse(t *testing.T) {
	store, state, caps := setupFuseSaver(5000, 0, 0.6, 10000)
	targets := []DispatchTarget{{Driver: "bat", TargetW: 1000}}
	out := forceFuseDischarge(targets, store, state, caps, 11040)
	if out[0].TargetW != 1000 || out[0].Clamped {
		t.Errorf("within fuse: got %.0f clamped=%v, want 1000 false",
			out[0].TargetW, out[0].Clamped)
	}
}

// Multi-battery: distribute the forced discharge proportionally to
// each battery's remaining headroom.
func TestFuseSaverDistributesAcrossBatteries(t *testing.T) {
	s := telemetry.NewStore()
	s.Update("meter", telemetry.DerMeter, 14000, nil, nil)
	s.DriverHealthMut("meter").RecordSuccess()
	soc := 0.6
	s.Update("a", telemetry.DerBattery, 0, &soc, nil)
	s.DriverHealthMut("a").RecordSuccess()
	s.Update("b", telemetry.DerBattery, 0, &soc, nil)
	s.DriverHealthMut("b").RecordSuccess()
	state := NewState(0, 50, "meter")
	state.DriverLimits = map[string]PowerLimits{
		"a": {MaxChargeW: 10000, MaxDischargeW: 6000},
		"b": {MaxChargeW: 10000, MaxDischargeW: 4000},
	}
	caps := map[string]float64{"a": 10000, "b": 5000}
	targets := []DispatchTarget{
		{Driver: "a", TargetW: 0},
		{Driver: "b", TargetW: 0},
	}
	// Overage 14000 - 11040 = 2960. Distributed by headroom (6:4):
	// a gets 2960*0.6 = 1776, b gets 2960*0.4 = 1184.
	out := forceFuseDischarge(targets, s, state, caps, 11040)
	if math.Abs(out[0].TargetW-(-1776)) > 1 {
		t.Errorf("battery a: %.0f, want -1776", out[0].TargetW)
	}
	if math.Abs(out[1].TargetW-(-1184)) > 1 {
		t.Errorf("battery b: %.0f, want -1184", out[1].TargetW)
	}
	var sum float64
	for _, t := range out {
		sum += t.TargetW
	}
	if math.Abs(sum-(-2960)) > 1 {
		t.Errorf("total forced discharge = %.0f, want -2960", sum)
	}
}

// fuseMaxW=0 → disabled. No-op.
func TestFuseSaverDisabledWhenFuseMaxZero(t *testing.T) {
	store, state, caps := setupFuseSaver(20000, 0, 0.6, 10000)
	targets := []DispatchTarget{{Driver: "bat", TargetW: 1000}}
	out := forceFuseDischarge(targets, store, state, caps, 0)
	if out[0].TargetW != 1000 || out[0].Clamped {
		t.Errorf("fuse_max=0 should disable: got %.0f clamped=%v",
			out[0].TargetW, out[0].Clamped)
	}
}

// Slew bypass — the fuse is the non-negotiable ceiling, slew rate
// must NOT limit the fuse-saver. End-to-end test: battery at rest
// (anchor SmoothedW = 0), aggressive 500 W/cycle slew, sudden grid
// import 14 kW (over 11 kW fuse). Expected: target ≈ -3 kW
// regardless of the slew rate, because forceFuseDischarge runs
// AFTER the slew loop in ComputeDispatch.
func TestFuseSaverBypassesSlew(t *testing.T) {
	store, state, caps := setupFuseSaver(14000, 0, 0.6, 10000)
	state.Mode = ModeIdle
	state.SlewRateW = 500 // tight slew that would otherwise cap the response
	out := ComputeDispatch(store, state, caps, 11040)
	if len(out) == 0 {
		t.Fatalf("idle + over-fuse: expected fuse-saver discharge, got empty")
	}
	// Predicted overage = 14000 − 11040 = 2960 W. With 10 kW headroom
	// the fuse-saver should command the full overage as discharge —
	// well beyond the 500 W/cycle slew. If we see −500 W instead of
	// ≈ −2960 W, slew is incorrectly clamping the safety primary.
	expected := -2960.0
	if math.Abs(out[0].TargetW-expected) > 1 {
		t.Errorf("target = %.0f W, want %.0f W (slew must NOT clamp the fuse-saver)",
			out[0].TargetW, expected)
	}
}

// End-to-end via ComputeDispatch: idle mode + grid surge → returns
// non-empty discharge targets. Idle mode would normally return [].
func TestFuseSaverFiresInIdleMode(t *testing.T) {
	store, state, caps := setupFuseSaver(14000, 0, 0.6, 10000)
	state.Mode = ModeIdle
	out := ComputeDispatch(store, state, caps, 11040)
	if len(out) == 0 {
		t.Fatalf("idle mode + over-fuse import: expected fuse-saver discharge, got empty")
	}
	if out[0].TargetW >= 0 {
		t.Errorf("expected discharge target, got %.0f", out[0].TargetW)
	}
	if !out[0].Clamped {
		t.Errorf("Clamped flag must mark fuse-saver activation")
	}
}
