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

// Planner commanded the battery to CHARGE (off-peak hours, planner
// arbitrage). Grid is already at the fuse from external loads. The
// fuse-saver flips the charge command to discharge regardless of plan.
func TestFuseSaverFlipsChargeToDischargeWhenOverFuse(t *testing.T) {
	// Live grid at fuse limit; planner asked to charge 3 kW; current
	// battery doing nothing (target hasn't propagated yet).
	store, state, caps := setupFuseSaver(11000, 0, 0.6, 10000)
	targets := []DispatchTarget{{Driver: "bat", TargetW: 3000}}
	// Predicted = 11000 - 0 + 3000 = 14000. Over fuse by 2960.
	out := forceFuseDischarge(targets, store, state, caps, 11040)
	if out[0].TargetW >= 0 {
		t.Errorf("expected discharge override, got TargetW=%.0f", out[0].TargetW)
	}
	// 14000 - 11040 = 2960 overage → full discharge of 2960 W
	// (charge zeroed first, then negative magnitude added).
	expected := -2960.0
	if math.Abs(out[0].TargetW-expected) > 1 {
		t.Errorf("target = %.0f, want ≈ %.0f", out[0].TargetW, expected)
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
