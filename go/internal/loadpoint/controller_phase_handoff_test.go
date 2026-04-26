package loadpoint

import (
	"context"
	"testing"
	"time"
)

// Phase decisions live in the driver. The controller's job is to pass
// the operator preferences (PhaseMode / PhaseSplitW / MinPhaseHoldS)
// and the site fuse parameters (Voltage / MaxAmpsPerPhase / SitePhases)
// through verbatim. These tests verify the hand-off — the driver-side
// behaviour (1Φ vs 3Φ choice, hysteresis, per-phase clamp, W→A
// conversion) is exercised by drivers/easee_cloud.lua against the
// real Easee API in integration testing.

var ftwStepSet = []float64{
	0,
	1380, 1610, 1840, 2070, 2300, 2530, 2760, // 1Φ 6-12 A @ 230 V
	4140, 4830, 5520, 6210, 6900, 7400, 7590, 8280, 11000, // 3Φ 6-12 A + legacy
}

func phaseLoadpoint(mode string, splitW float64, holdS int) Config {
	return Config{
		ID:            "garage",
		DriverName:    "easee",
		MinChargeW:    1380,
		MaxChargeW:    11000,
		AllowedStepsW: ftwStepSet,
		PhaseMode:     mode,
		PhaseSplitW:   splitW,
		MinPhaseHoldS: holdS,
	}
}

func runPhaseTick(t *testing.T, cfg Config, site SiteFuse, wantWh float64) sentCommand {
	t.Helper()
	now := time.Now()
	slotStart := now.Add(-1 * time.Second)
	slotEnd := slotStart.Add(60 * time.Minute)
	dir := &Directive{
		SlotStart:         slotStart,
		SlotEnd:           slotEnd,
		LoadpointEnergyWh: map[string]float64{cfg.ID: wantWh},
	}
	sender := &fakeSender{}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, PowerW: 0}}
	c := newTestController(t, []Config{cfg}, dir, samples, sender)
	c.SetSiteFuse(site)
	c.Tick(context.Background(), now)
	if len(sender.calls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sender.calls))
	}
	return sender.calls[0]
}

func TestCommandCarriesPhaseModePreference(t *testing.T) {
	cfg := phaseLoadpoint("auto", 0, 0)
	cmd := runPhaseTick(t, cfg, SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3}, 4000)
	if cmd.phaseMode != "auto" {
		t.Errorf("phase_mode in cmd = %q, want \"auto\"", cmd.phaseMode)
	}
}

func TestEmptyPhaseModeOmittedFromCommand(t *testing.T) {
	// Empty PhaseMode (legacy default) MUST NOT land in the cmd —
	// drivers that don't read this field should see exactly the
	// pre-phase-switching shape and behave as locked-3Φ.
	cfg := phaseLoadpoint("", 0, 0)
	cmd := runPhaseTick(t, cfg, SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3}, 4000)
	if cmd.phaseMode != "" {
		t.Errorf("phase_mode = %q, want empty (omitted from cmd)", cmd.phaseMode)
	}
}

func TestCommandCarriesSplitOverride(t *testing.T) {
	cfg := phaseLoadpoint("auto", 4500, 0)
	cmd := runPhaseTick(t, cfg, SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3}, 4000)
	if cmd.phaseSplitW != 4500 {
		t.Errorf("phase_split_w = %.0f, want 4500", cmd.phaseSplitW)
	}
}

func TestCommandCarriesHoldOverride(t *testing.T) {
	cfg := phaseLoadpoint("auto", 0, 90)
	cmd := runPhaseTick(t, cfg, SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3}, 4000)
	if cmd.minPhaseHoldS != 90 {
		t.Errorf("min_phase_hold_s = %d, want 90", cmd.minPhaseHoldS)
	}
}

func TestCommandCarriesSiteFuseParams(t *testing.T) {
	cfg := phaseLoadpoint("auto", 0, 0)
	// Non-230 V site so we can prove the literal 230 isn't hard-coded
	// anywhere in the path.
	cmd := runPhaseTick(t, cfg, SiteFuse{MaxAmps: 20, Voltage: 240, PhaseCnt: 3}, 4000)
	if cmd.voltage != 240 {
		t.Errorf("voltage in cmd = %.0f, want 240", cmd.voltage)
	}
	if cmd.maxAmpsPerPhase != 20 {
		t.Errorf("max_amps_per_phase = %.0f, want 20", cmd.maxAmpsPerPhase)
	}
	if cmd.sitePhases != 3 {
		t.Errorf("site_phases = %d, want 3", cmd.sitePhases)
	}
}

func TestCommandOmitsFuseParamsWhenUnset(t *testing.T) {
	// Controllers without a configured fuse send no fuse fields —
	// drivers fall back to their own defaults (legacy behaviour).
	cfg := phaseLoadpoint("auto", 0, 0)
	cmd := runPhaseTick(t, cfg, SiteFuse{}, 4000)
	if cmd.voltage != 0 || cmd.maxAmpsPerPhase != 0 || cmd.sitePhases != 0 {
		t.Errorf("expected fuse fields absent, got voltage=%.0f maxA=%.0f phases=%d",
			cmd.voltage, cmd.maxAmpsPerPhase, cmd.sitePhases)
	}
}

func TestPowerWStillReachesDriver(t *testing.T) {
	// Sanity: the controller still computes the energy budget and
	// sends a non-zero power_w when the plan has an allocation.
	cfg := phaseLoadpoint("auto", 0, 0)
	cmd := runPhaseTick(t, cfg, SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3}, 6000)
	if cmd.power <= 0 {
		t.Errorf("power_w = %.0f, want > 0", cmd.power)
	}
}
