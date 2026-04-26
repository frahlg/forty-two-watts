package loadpoint

import (
	"context"
	"testing"
	"time"
)

// Phase-switching + per-phase fuse-clamp tests. Safety-critical: never
// allow a commanded EV current that, at the chosen phase count, would
// exceed the site fuse. Backward compat: a loadpoint without PhaseMode
// must continue to act as a statically-3Φ install (the existing fleet's
// behaviour before phase-switching landed).

var ftwStepSet = []float64{
	0,
	1380, 1610, 1840, 2070, 2300, 2530, 2760, // 1Φ 6-12 A @ 230 V
	4140, 4830, 5520, 6210, 6900, 7400, 7590, 8280, 11000, // 3Φ 6-12 A + legacy
}

func ftwLoadpoint(phaseMode string) Config {
	return Config{
		ID:            "garage",
		DriverName:    "easee",
		MinChargeW:    1380,
		MaxChargeW:    11000,
		AllowedStepsW: ftwStepSet,
		PhaseMode:     phaseMode,
	}
}

func runTick(t *testing.T, cfg Config, site SiteFuse, wantWh float64, slotMin int, now time.Time) sentCommand {
	t.Helper()
	slotStart := now.Add(-1 * time.Second)
	slotEnd := slotStart.Add(time.Duration(slotMin) * time.Minute)
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

func TestAutoPicksOnePhaseBelowSplit(t *testing.T) {
	// 2.3 kWh over 1 h → wantW ≈ 2300 W, below 16 A × 230 V = 3680 W
	// per-phase ceiling → 1Φ. Nearest 1Φ step: 2300.
	cfg := ftwLoadpoint("auto")
	cmd := runTick(t, cfg, SiteFuse{MaxAmps: 16, Voltage: 230}, 2300, 60, time.Now())
	if cmd.phases != 1 {
		t.Errorf("phases = %d, want 1", cmd.phases)
	}
	if cmd.power != 2300 {
		t.Errorf("power = %.0f, want 2300", cmd.power)
	}
}

func TestAutoPicksThreePhaseAboveSplit(t *testing.T) {
	cfg := ftwLoadpoint("auto")
	// 6 kWh / 1 h = 6000 W > 3680 → 3Φ. 6000 is closer to 6210 (Δ=210)
	// than 5520 (Δ=480).
	cmd := runTick(t, cfg, SiteFuse{MaxAmps: 16, Voltage: 230}, 6000, 60, time.Now())
	if cmd.phases != 3 {
		t.Errorf("phases = %d, want 3", cmd.phases)
	}
	if cmd.power != 6210 {
		t.Errorf("power = %.0f, want 6210", cmd.power)
	}
}

func TestLockedThreePhaseFiltersSteps(t *testing.T) {
	// Locked 3Φ with a small wantW that, if auto, would pick 1Φ. We force
	// 3Φ and must never emit a 1Φ step.
	cfg := ftwLoadpoint("3p")
	cmd := runTick(t, cfg, SiteFuse{MaxAmps: 16, Voltage: 230}, 2000, 60, time.Now())
	if cmd.phases != 3 {
		t.Errorf("phases = %d, want 3", cmd.phases)
	}
	// 2000 W: nearest 3Φ step is 0 (distance 2000) vs 4140 (distance 2140) → 0.
	if cmd.power != 0 {
		t.Errorf("power = %.0f, want 0 (too low for any 3Φ step)", cmd.power)
	}
}

func TestLockedOnePhaseFiltersSteps(t *testing.T) {
	// Locked 1Φ with a large wantW that would be 3Φ in auto. Must clamp
	// to the 1Φ fuse ceiling (3680 W) and snap to a 1Φ step.
	cfg := ftwLoadpoint("1p")
	cmd := runTick(t, cfg, SiteFuse{MaxAmps: 16, Voltage: 230}, 8000, 60, time.Now())
	if cmd.phases != 1 {
		t.Errorf("phases = %d, want 1", cmd.phases)
	}
	// 1Φ subset is {0, 1380…2760}. Fuse-clamped wantW = 3680, nearest 1Φ = 2760.
	if cmd.power != 2760 {
		t.Errorf("power = %.0f, want 2760 (clamped)", cmd.power)
	}
}

func TestFuseClampThreePhaseHoldsBelowCeiling(t *testing.T) {
	// Huge wantW must clamp to 16 A × 230 V × 3 = 11040 W, then snap to
	// nearest 3Φ step ≤ ceiling → 11000 W.
	cfg := ftwLoadpoint("3p")
	cmd := runTick(t, cfg, SiteFuse{MaxAmps: 16, Voltage: 230}, 20000, 60, time.Now())
	if cmd.power > 11040 {
		t.Errorf("power = %.0f exceeds 16 A/3Φ fuse ceiling 11040 W", cmd.power)
	}
	if cmd.phases != 3 {
		t.Errorf("phases = %d, want 3", cmd.phases)
	}
}

func TestFuseClampSinglePhaseHoldsBelowCeiling(t *testing.T) {
	// 1Φ fuse ceiling = 16 A × 230 = 3680 W.
	cfg := ftwLoadpoint("1p")
	cmd := runTick(t, cfg, SiteFuse{MaxAmps: 16, Voltage: 230}, 20000, 60, time.Now())
	if cmd.power > 3680 {
		t.Errorf("power = %.0f exceeds 16 A/1Φ fuse ceiling 3680 W", cmd.power)
	}
	if cmd.phases != 1 {
		t.Errorf("phases = %d, want 1", cmd.phases)
	}
}

func TestFuseClampWithSmallerFuse(t *testing.T) {
	// 10 A fuse → 1Φ ceiling 2300 W, 3Φ ceiling 6900 W. With auto and
	// want=12000 W the split = 2300 W (per-phase ceiling); above split →
	// 3Φ; clamp to ≤ 6900; nearest 3Φ step at-or-below = 6900.
	cfg := ftwLoadpoint("auto")
	cmd := runTick(t, cfg, SiteFuse{MaxAmps: 10, Voltage: 230}, 12000, 60, time.Now())
	if cmd.phases != 3 {
		t.Errorf("phases = %d, want 3", cmd.phases)
	}
	if cmd.power > 6900 {
		t.Errorf("power = %.0f exceeds 10 A/3Φ fuse ceiling 6900 W", cmd.power)
	}
	if cmd.power != 6900 {
		t.Errorf("power = %.0f, want 6900", cmd.power)
	}
}

func TestNonStandardVoltageHonored(t *testing.T) {
	// 240 V mains: 16 A × 240 V = 3840 W per phase. wantW=3700 sits
	// just below the per-phase ceiling → auto picks 1Φ. The site
	// voltage must propagate into the cmd payload so the driver can
	// convert W→A correctly.
	cfg := ftwLoadpoint("auto")
	cmd := runTick(t, cfg, SiteFuse{MaxAmps: 16, Voltage: 240}, 3700, 60, time.Now())
	if cmd.phases != 1 {
		t.Errorf("phases = %d at 240 V split=3840, want 1", cmd.phases)
	}
	if cmd.voltage != 240 {
		t.Errorf("voltage in payload = %.0f, want 240", cmd.voltage)
	}
}

func TestDefaultPhaseModeIsThreePhaseBackwardCompat(t *testing.T) {
	// Empty PhaseMode ("") must behave like locked 3Φ — the existing
	// fleet has `phases: 3` statically on the driver; we must not
	// suddenly start commanding 1Φ on them after upgrade.
	cfg := ftwLoadpoint("")
	cmd := runTick(t, cfg, SiteFuse{MaxAmps: 16, Voltage: 230}, 2000, 60, time.Now())
	if cmd.phases != 3 {
		t.Errorf("default phases = %d, want 3 (backward compat)", cmd.phases)
	}
}

func TestPhaseHoldPreventsFlap(t *testing.T) {
	// First tick: want 3Φ. Second tick 30s later: want 1Φ. Hold = 60s.
	// Second tick must stay 3Φ. Third tick after hold must flip to 1Φ.
	cfg := ftwLoadpoint("auto")
	cfg.MinPhaseHoldS = 60
	m := NewManager()
	m.Load([]Config{cfg})
	sender := &fakeSender{}

	now := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	slotEnd := now.Add(1 * time.Hour)
	var wantWh float64 = 6000
	plan := PlanFunc(func(_ time.Time) (Directive, bool) {
		return Directive{
			SlotStart:         now.Add(-time.Second),
			SlotEnd:           slotEnd,
			LoadpointEnergyWh: map[string]float64{"garage": wantWh},
		}, true
	})
	tel := TelemetryFunc(func(string) (EVSample, bool) {
		return EVSample{Connected: true}, true
	})
	c := NewController(m, plan, tel, sender.Send)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230})

	c.Tick(context.Background(), now)
	if sender.calls[0].phases != 3 {
		t.Fatalf("tick 1 phases = %d, want 3", sender.calls[0].phases)
	}

	wantWh = 2000
	later := now.Add(30 * time.Second)
	slotEnd = later.Add(1 * time.Hour)
	c.Tick(context.Background(), later)
	if sender.calls[1].phases != 3 {
		t.Errorf("tick 2 phases = %d within 30s of flip, want 3 (held)", sender.calls[1].phases)
	}

	afterHold := now.Add(61 * time.Second)
	slotEnd = afterHold.Add(1 * time.Hour)
	c.Tick(context.Background(), afterHold)
	if sender.calls[2].phases != 1 {
		t.Errorf("tick 3 phases = %d after hold elapsed, want 1", sender.calls[2].phases)
	}
}

func TestPayloadCarriesPhasesField(t *testing.T) {
	cfg := ftwLoadpoint("auto")
	cmd := runTick(t, cfg, SiteFuse{MaxAmps: 16, Voltage: 230}, 2300, 60, time.Now())
	if cmd.phases == 0 {
		t.Errorf("phases absent from payload: %+v", cmd)
	}
}

func TestZeroFuseMeansNoClamp(t *testing.T) {
	// Controllers constructed without SetSiteFuse must not magically
	// clamp to 0. wantW well under the legacy split → command normally.
	cfg := ftwLoadpoint("auto")
	cmd := runTick(t, cfg, SiteFuse{}, 2300, 60, time.Now())
	if cmd.power == 0 {
		t.Errorf("zero-fuse controller clamped to 0: %+v", cmd)
	}
}
