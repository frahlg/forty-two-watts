package loadpoint

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// Controller orchestrates one dispatch cycle for every configured
// loadpoint: observe driver telemetry, read the planner's per-slot
// energy budget, translate to an instantaneous W command, and send
// to the driver.
//
// Extracted verbatim from the monolithic block that used to live in
// main.go's control tick. Phase 1 is behaviour-preserving only — the
// main loop calls (*Controller).Tick(ctx, now) where it used to
// inline the logic. Phase 2 will give each loadpoint its own
// goroutine + cadence declared by the driver.
//
// Dependencies are injected as function types (not interfaces) to
// avoid pulling mpc and telemetry into loadpoint's import graph —
// mpc already imports loadpoint for its DP loadpoint_spec, so the
// cycle must go the other way. main.go wires short adapter closures
// from mpc.Service / telemetry.Store / drivers.Registry.
type Controller struct {
	manager *Manager
	plan    PlanFunc
	tel     TelemetryFunc
	send    SenderFunc

	// site is the grid-boundary fuse the per-phase clamp applies.
	// Zero means "no clamp" (tests that don't care about the fuse).
	// Set at startup via SetSiteFuse from main.go.
	site SiteFuse

	// phases tracks the most recently-commanded phase count per
	// loadpoint ID so MinPhaseHoldS can prevent flap. A zero
	// lastPhase means "no commitment yet" — first non-zero command
	// seeds it without triggering the hold.
	phases map[string]*lpPhaseState
}

// lpPhaseState is the per-loadpoint hold record used to rate-limit
// 1Φ↔3Φ flips.
type lpPhaseState struct {
	lastPhase    int
	lastChangeTs time.Time
}

// Directive is the loadpoint-relevant slice of mpc.SlotDirective.
// The mpc package defines the full type with BatteryEnergyWh etc;
// the controller only needs the slot window and per-loadpoint Wh
// budget, so we don't pull in the whole struct.
type Directive struct {
	SlotStart         time.Time
	SlotEnd           time.Time
	LoadpointEnergyWh map[string]float64
}

// EVSample is the loadpoint-relevant slice of telemetry.DerReading
// for a DerEV entry — power, cumulative session energy, plug state,
// and active-charging flag (current actually flowing). Chargers like
// Easee don't expose the vehicle's BMS SoC, so the controller only
// sees these four fields.
type EVSample struct {
	PowerW    float64
	SessionWh float64
	Connected bool
	Charging  bool
}

// PlanFunc returns the current-slot directive for now, or (_, false)
// when no plan is available (stale, missing, out of horizon).
type PlanFunc func(now time.Time) (Directive, bool)

// TelemetryFunc returns the latest EV reading for a driver. The
// second return is false when the driver hasn't produced a reading
// yet.
type TelemetryFunc func(driver string) (EVSample, bool)

// SenderFunc forwards a JSON command payload to a driver. Matches
// drivers.Registry.Send.
type SenderFunc func(ctx context.Context, driver string, payload []byte) error

// NewController wires the dependencies. Passing nil for plan, tel,
// or send disables the corresponding step — useful in tests.
func NewController(mgr *Manager, plan PlanFunc, tel TelemetryFunc, send SenderFunc) *Controller {
	return &Controller{
		manager: mgr, plan: plan, tel: tel, send: send,
		phases: map[string]*lpPhaseState{},
	}
}

// SetSiteFuse installs the grid-boundary fuse limit used by the
// per-phase EV clamp. Must be called once at startup from main.go
// after config load; a zero-value fuse leaves the clamp disabled
// (useful for unit tests and sites without a configured fuse).
func (c *Controller) SetSiteFuse(f SiteFuse) {
	if c == nil {
		return
	}
	c.site = f
}

// Tick runs one dispatch cycle for every configured loadpoint.
// Safe to call even when no loadpoints are configured. Idempotent —
// calling it twice in the same moment produces the same commands.
//
// Behaviour is equivalent to the inline block previously in main.go:
//
//  1. Read latest charger telemetry for this driver.
//  2. Feed the observation to the Manager (plug state, session Wh,
//     inferred SoC).
//  3. For unplugged loadpoints: skip command entirely.
//  4. For plugged loadpoints: ask the plan for this slot's Wh
//     allocation and translate to a W command via the energy-
//     allocation contract (remaining_wh × 3600 / remaining_s).
//  5. Snap to the charger's discrete steps.
//  6. Send `ev_set_current` with the resulting W. When no plan
//     allocation exists, 0 W is commanded explicitly — without it
//     the charger rides the previous slot's setpoint.
func (c *Controller) Tick(ctx context.Context, now time.Time) {
	if c == nil || c.manager == nil {
		return
	}
	// Preserve the old `len(cfg.Loadpoints) > 0 && mpcSvc != nil` guard:
	// when the planner isn't wired we stay fully out of the loadpoint
	// driver's state, the same as before the refactor. Phase 3 will
	// relax this once the controller owns its own fallback behaviour.
	if c.plan == nil {
		return
	}
	for _, lpCfg := range c.manager.Configs() {
		c.tickOne(ctx, now, lpCfg)
	}
}

func (c *Controller) tickOne(ctx context.Context, now time.Time, lpCfg Config) {
	var sample EVSample
	if c.tel != nil {
		sample, _ = c.tel(lpCfg.DriverName)
	}
	c.manager.Observe(lpCfg.ID, sample.Connected, sample.PowerW, sample.SessionWh, sample.Charging)
	if !sample.Connected {
		return
	}
	cmdW, phases := c.computeCommand(now, lpCfg, sample.PowerW)
	payload, err := json.Marshal(map[string]any{
		"action":  "ev_set_current",
		"power_w": cmdW,
		"phases":  phases,
	})
	if err != nil {
		return
	}
	if c.send == nil {
		return
	}
	if err := c.send(ctx, lpCfg.DriverName, payload); err != nil {
		slog.Warn("loadpoint dispatch", "lp", lpCfg.ID,
			"driver", lpCfg.DriverName, "err", err)
	}
}

// computeCommand resolves the (W, phases) setpoint for a plugged
// loadpoint. Returns (0, 0) when no plan allocation exists — an
// explicit standdown, not a lazy last-command. Otherwise:
//
//  1. MPC slot budget → wantW (continuous)
//  2. phaseFor(...) picks 1Φ vs 3Φ based on PhaseMode + wantW,
//     subject to MinPhaseHoldS hysteresis so we don't flap.
//  3. Fuse clamp: wantW ≤ fuse.MaxAmps × V × phases — the
//     non-negotiable site invariant.
//  4. AllowedStepsW filtered to the chosen phase's subset.
//  5. SnapChargeW picks the nearest feasible step.
func (c *Controller) computeCommand(now time.Time, lpCfg Config, currentPowerW float64) (float64, int) {
	if c.plan == nil {
		return 0, 0
	}
	d, ok := c.plan(now)
	if !ok {
		return 0, 0
	}
	budgetWh, hasBudget := d.LoadpointEnergyWh[lpCfg.ID]
	if !hasBudget {
		return 0, 0
	}
	remainingS := d.SlotEnd.Sub(now).Seconds()
	elapsed := d.SlotEnd.Sub(d.SlotStart).Seconds() - remainingS
	if elapsed < 0 {
		elapsed = 0
	}
	alreadyWh := currentPowerW * elapsed / 3600.0
	remainingWh := budgetWh - alreadyWh
	wantW := EnergyBudgetToPowerW(remainingWh, remainingS)

	phases := c.decidePhase(now, lpCfg, wantW)

	// Per-phase fuse clamp. This is the safety invariant — it must
	// apply BEFORE step filtering / snapping so the step search can
	// never return a value the fuse wouldn't tolerate.
	if c.site.MaxAmps > 0 && phases > 0 {
		ceiling := c.site.PerPhaseMaxW() * float64(phases)
		if wantW > ceiling {
			wantW = ceiling
		}
	}

	steps := filterStepsByPhase(lpCfg.AllowedStepsW, phases, lpCfg.PhaseSplitW)
	snapped := SnapChargeW(wantW, lpCfg.MinChargeW, lpCfg.MaxChargeW, steps)

	// Second-line safety: even if a step somehow exceeded the fuse
	// ceiling (misconfigured step list), never send it.
	if c.site.MaxAmps > 0 && phases > 0 {
		ceiling := c.site.PerPhaseMaxW() * float64(phases)
		if snapped > ceiling {
			snapped = ceiling
		}
	}

	return snapped, phases
}

// decidePhase picks 1 or 3 based on PhaseMode + wantW, applying
// MinPhaseHoldS hysteresis so an MPC budget oscillating around the
// split threshold doesn't trigger a contactor flip every tick. The
// hold is a minimum dwell, not a moving average — once we flip, the
// next flip can't happen until `hold` seconds elapsed since the last
// change.
func (c *Controller) decidePhase(now time.Time, lpCfg Config, wantW float64) int {
	desired := phaseFor(lpCfg.PhaseMode, wantW, lpCfg.PhaseSplitW)

	// Locked modes: no hysteresis to apply, just return.
	if lpCfg.PhaseMode != "auto" {
		return desired
	}

	hold := time.Duration(lpCfg.MinPhaseHoldS) * time.Second
	if lpCfg.MinPhaseHoldS <= 0 {
		hold = 60 * time.Second
	}

	if c.phases == nil {
		c.phases = map[string]*lpPhaseState{}
	}
	ps, ok := c.phases[lpCfg.ID]
	if !ok {
		ps = &lpPhaseState{lastPhase: desired, lastChangeTs: now}
		c.phases[lpCfg.ID] = ps
		return desired
	}
	if ps.lastPhase == desired {
		return desired
	}
	if now.Sub(ps.lastChangeTs) < hold {
		// Flip suppressed — keep the previous phase until the
		// hold window elapses.
		return ps.lastPhase
	}
	ps.lastPhase = desired
	ps.lastChangeTs = now
	return desired
}
