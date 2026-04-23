package loadpoint

import (
	"context"
	"testing"
	"time"
)

// TestLiveLoadClampCapsEVBelowHouseSpike — the safety-critical
// regression test: oven + kitchen + house spike pushes grid over
// fuse, batteries at max discharge can't help more, controller
// MUST back EV off to protect the breaker.
//
// Scenario: fuse = 16 A × 3 × 230 V = 11 040 W site budget. The
// non-EV draw (load + pv + battery) currently sits at 12 000 W
// — already past the fuse by itself. MPC's plan allocates EV
// 11 kW. Without the live clamp the controller would command
// 11 kW and trip the main breaker. With the clamp, the effective
// EV cap is fuseSafe − 12 000 = negative → clamped to 0 W.
func TestLiveLoadClampCapsEVBelowHouseSpike(t *testing.T) {
	sender := &fakeSender{}
	slotStart := time.Now().Add(-time.Second)
	slotEnd := slotStart.Add(15 * time.Minute)
	cfgs := []Config{{
		ID:            "garage",
		DriverName:    "easee",
		MinChargeW:    1380,
		MaxChargeW:    11000,
		AllowedStepsW: []float64{0, 1380, 2760, 4140, 6210, 11000},
	}}
	dir := &Directive{
		SlotStart:         slotStart,
		SlotEnd:           slotEnd,
		LoadpointEnergyWh: map[string]float64{"garage": 2750}, // 11 kW over 15 min
	}
	samples := map[string]EVSample{"easee": {Connected: true, PowerW: 0}}
	c := newTestController(t, cfgs, dir, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3}) // 11 040 W safe

	// Live grid is at 12 000 W WITHOUT the EV drawing anything
	// (hypothetical oven + kitchen + house). Fuse budget breached
	// already; EV must be held at 0.
	c.SetLiveSiteReader(func() (float64, float64, bool) {
		return 12000, 0, true // grid=12000, ev=0 → non-EV load = 12000
	})

	c.Tick(context.Background(), time.Now())

	if len(sender.calls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sender.calls))
	}
	if sender.calls[0].power != 0 {
		t.Errorf("live clamp must force EV=0 when non-EV load exceeds fuse; got %.0f W", sender.calls[0].power)
	}
}

// TestLiveLoadClampShrinksEVToFitHeadroom — a partial headroom
// case: non-EV load is 7 kW, fuse is 11 040 W, so EV must fit in
// the remaining ~4 kW window. MPC wants 11 kW; clamp snaps to
// the nearest allowed step ≤ 4 kW.
func TestLiveLoadClampShrinksEVToFitHeadroom(t *testing.T) {
	sender := &fakeSender{}
	slotStart := time.Now().Add(-time.Second)
	slotEnd := slotStart.Add(15 * time.Minute)
	cfgs := []Config{{
		ID:            "garage",
		DriverName:    "easee",
		MinChargeW:    1380,
		MaxChargeW:    11000,
		AllowedStepsW: []float64{0, 1380, 2760, 4140, 6210, 11000},
	}}
	dir := &Directive{
		SlotStart:         slotStart,
		SlotEnd:           slotEnd,
		LoadpointEnergyWh: map[string]float64{"garage": 2750},
	}
	samples := map[string]EVSample{"easee": {Connected: true, PowerW: 0}}
	c := newTestController(t, cfgs, dir, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})

	// Non-EV load = 7000 W → headroom = 11040 - 7000 = 4040 W.
	// Nearest allowed step ≤ 4040 → 2760 (snap-to-nearest rule with
	// clamp ceiling prefers the one under the cap).
	c.SetLiveSiteReader(func() (float64, float64, bool) {
		return 7000, 0, true
	})
	c.Tick(context.Background(), time.Now())

	if len(sender.calls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sender.calls))
	}
	cmd := sender.calls[0].power
	if cmd > 4040 {
		t.Errorf("live clamp breach: EV=%0.f W exceeds headroom 4040 W", cmd)
	}
}

// TestLiveLoadClampAccountsForOwnDraw — the non-EV load subtraction
// is critical: if grid=12 000 W but that already INCLUDES 11 000 W
// of EV, the non-EV load is only 1 000 W and we have 10 kW of
// headroom. Without this subtraction the controller would
// incorrectly clamp to 0 and oscillate.
func TestLiveLoadClampAccountsForOwnDraw(t *testing.T) {
	sender := &fakeSender{}
	slotStart := time.Now().Add(-time.Second)
	slotEnd := slotStart.Add(15 * time.Minute)
	cfgs := []Config{{
		ID:            "garage",
		DriverName:    "easee",
		MinChargeW:    1380,
		MaxChargeW:    11000,
		AllowedStepsW: []float64{0, 1380, 2760, 4140, 6210, 11000},
	}}
	dir := &Directive{
		SlotStart:         slotStart,
		SlotEnd:           slotEnd,
		LoadpointEnergyWh: map[string]float64{"garage": 2750},
	}
	samples := map[string]EVSample{"easee": {Connected: true, PowerW: 11000}}
	c := newTestController(t, cfgs, dir, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})

	// grid=12 000 W, but evW=11 000 W → non-EV load = 1 000 W,
	// headroom = 10 040 W. MPC wants 11 kW → snap to highest
	// feasible (≤ 10 040 W). Allowed steps top at 11 000 (above cap)
	// and 6210 (below). 6210 wins: nearest step under the cap.
	c.SetLiveSiteReader(func() (float64, float64, bool) {
		return 12000, 11000, true
	})
	c.Tick(context.Background(), time.Now())

	if len(sender.calls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sender.calls))
	}
	cmd := sender.calls[0].power
	if cmd > 10040 {
		t.Errorf("live clamp breach: EV=%0.f W exceeds headroom 10040 W", cmd)
	}
	if cmd == 0 {
		t.Errorf("live clamp over-triggered; expected ~6 kW, got 0")
	}
}

// TestLiveLoadClampDisabledWhenReaderAbsent — back-compat: without
// a reader wired, the per-phase clamp alone controls. No change vs
// pre-clamp behaviour.
func TestLiveLoadClampDisabledWhenReaderAbsent(t *testing.T) {
	sender := &fakeSender{}
	slotStart := time.Now().Add(-time.Second)
	slotEnd := slotStart.Add(15 * time.Minute)
	cfgs := []Config{{
		ID:            "garage",
		DriverName:    "easee",
		MinChargeW:    1380,
		MaxChargeW:    11000,
		AllowedStepsW: []float64{0, 1380, 2760, 4140, 6210, 11000},
	}}
	dir := &Directive{
		SlotStart:         slotStart,
		SlotEnd:           slotEnd,
		LoadpointEnergyWh: map[string]float64{"garage": 2750},
	}
	samples := map[string]EVSample{"easee": {Connected: true, PowerW: 0}}
	c := newTestController(t, cfgs, dir, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	// No SetLiveSiteReader.
	c.Tick(context.Background(), time.Now())

	if len(sender.calls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sender.calls))
	}
	// Per-phase clamp at 11 040 W, MPC wants 11 000 W → 11 000.
	if sender.calls[0].power != 11000 {
		t.Errorf("without live reader expected 11000 W, got %.0f", sender.calls[0].power)
	}
}
