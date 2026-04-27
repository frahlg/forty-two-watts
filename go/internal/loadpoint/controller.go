package loadpoint

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"
)

// Controller orchestrates one dispatch cycle for every configured
// loadpoint: observe driver telemetry, read the planner's per-slot
// energy budget, translate to an instantaneous W command, and send
// to the driver.
//
// Phase decisions (1Φ vs 3Φ) live IN THE DRIVER, not here. The
// controller's job is purely energy allocation: how many watts the
// MPC budget says we can pour into this loadpoint right now. The
// driver knows its own physical constraints (minimum amps, contactor
// switching latency, manufacturer's phaseMode wire format) and
// decides which phase configuration to use given the requested W,
// the operator's `phase_mode`/`phase_split_w`/`min_phase_hold_s`
// preferences, and the site's per-phase fuse ceiling — all of which
// are passed through in the `ev_set_current` command.
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

	// site is the grid-boundary fuse. Its values are passed through
	// to the driver in every ev_set_current cmd so the driver knows
	// the per-phase ceiling and the mains voltage. Zero MaxAmps
	// disables the per-phase fields in the cmd; the driver then
	// falls back to its own configured defaults.
	site SiteFuse

	// holds is the manual-override registry: per-loadpoint power +
	// phase parameters that win over the MPC-driven dispatch until
	// they expire. Used by the diagnostics endpoint
	// `POST /api/loadpoints/{id}/manual_hold` so an operator can pin
	// a specific amperage / phase configuration on the charger for
	// long enough to observe driver behaviour, without fighting the
	// 5-second control loop. Missing entries (or expired holds, which
	// `GetManualHold` lazily evicts) fall through to the normal
	// compute-from-plan path.
	holdMu sync.Mutex
	holds  map[string]ManualHold
}

// ManualHold pins a loadpoint to a specific dispatch payload until
// ExpiresAt. PowerW is sent verbatim; PhaseMode / PhaseSplitW /
// MinPhaseHoldS / Voltage / MaxAmpsPerPhase override the loadpoint's
// configured defaults — but ONLY when explicitly set on the hold.
// Zero values mean "no override" and the controller falls back to
// the loadpoint's PhaseMode/PhaseSplitW/MinPhaseHoldS and the wired
// SiteFuse for voltage / max_amps_per_phase / site_phases. This
// preserves the per-phase fuse clamp on minimal holds (e.g. just
// `{power_w, hold_s}`) — without the fall-through, the driver would
// silently fall back to its 230 V × 16 A defaults, which on a
// non-standard site could exceed the actual fuse.
type ManualHold struct {
	PowerW          float64
	PhaseMode       string
	PhaseSplitW     float64
	MinPhaseHoldS   int
	Voltage         float64
	MaxAmpsPerPhase float64
	SitePhases      int
	ExpiresAt       time.Time
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
// for a DerEV entry — power, cumulative session energy, plug state.
// Chargers like Easee don't expose the vehicle's BMS SoC, so the
// controller only sees these three fields.
type EVSample struct {
	PowerW    float64
	SessionWh float64
	Connected bool
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
	return &Controller{manager: mgr, plan: plan, tel: tel, send: send}
}

// SetSiteFuse installs the grid-boundary fuse so the controller can
// pass voltage + per-phase amperage to drivers in every command.
// Called once at startup from main.go after config load. A zero-value
// fuse causes the controller to omit those fields, which leaves the
// driver to use its own defaults.
func (c *Controller) SetSiteFuse(f SiteFuse) {
	if c == nil {
		return
	}
	c.site = f
}

// SetManualHold pins the given loadpoint to a fixed dispatch payload
// until h.ExpiresAt. tickOne checks the hold on every cycle and emits
// the held values verbatim — bypassing the MPC budget translation —
// until the hold expires (then the controller resumes normal
// dispatch on the next cycle). Useful for diagnostics: hold a
// specific amperage on the charger long enough to observe driver
// behaviour without fighting the 5-second control tick.
//
// A zero ExpiresAt clears any hold for this loadpoint (same as
// ClearManualHold). Setting a hold for an unknown loadpoint ID is
// silently allowed — the hold has no effect because tickOne only
// runs for configured loadpoints.
func (c *Controller) SetManualHold(id string, h ManualHold) {
	if c == nil {
		return
	}
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	if c.holds == nil {
		c.holds = map[string]ManualHold{}
	}
	if h.ExpiresAt.IsZero() {
		delete(c.holds, id)
		return
	}
	c.holds[id] = h
}

// ClearManualHold removes any active hold for the given loadpoint,
// regardless of expiry. Idempotent.
func (c *Controller) ClearManualHold(id string) {
	if c == nil {
		return
	}
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	delete(c.holds, id)
}

// GetManualHold returns the current hold for a loadpoint. The bool
// is false when no hold is active. Expired holds are not returned —
// they're lazily evicted on the next read.
func (c *Controller) GetManualHold(id string, now time.Time) (ManualHold, bool) {
	if c == nil {
		return ManualHold{}, false
	}
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	h, ok := c.holds[id]
	if !ok {
		return ManualHold{}, false
	}
	if !now.Before(h.ExpiresAt) {
		delete(c.holds, id)
		return ManualHold{}, false
	}
	return h, true
}

// Tick runs one dispatch cycle for every configured loadpoint.
// Safe to call even when no loadpoints are configured. Idempotent —
// calling it twice in the same moment produces the same commands.
//
// Behaviour:
//
//  1. Read latest charger telemetry for this driver.
//  2. Feed the observation to the Manager (plug state, session Wh,
//     inferred SoC).
//  3. For unplugged loadpoints: skip command entirely.
//  4. For plugged loadpoints: ask the plan for this slot's Wh
//     allocation and translate to a W command via the energy-
//     allocation contract (remaining_wh × 3600 / remaining_s).
//  5. Send `ev_set_current` with that W plus the operator's phase
//     preferences and the site's fuse parameters; the driver picks
//     phases and converts W→A given that it knows the voltage.
func (c *Controller) Tick(ctx context.Context, now time.Time) {
	if c == nil || c.manager == nil {
		return
	}
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
	c.manager.Observe(lpCfg.ID, sample.Connected, sample.PowerW, sample.SessionWh)
	if !sample.Connected {
		return
	}

	cmd := map[string]any{"action": "ev_set_current"}
	if hold, ok := c.GetManualHold(lpCfg.ID, now); ok {
		// Manual override active — skip MPC translation. The hold's
		// non-zero fields override the loadpoint config + site fuse;
		// zero/empty fields fall through to the normal defaults so a
		// minimal hold (just `power_w`) still carries the per-phase
		// fuse clamp inputs the driver needs to stay safe.
		cmd["power_w"] = hold.PowerW
		switch {
		case hold.PhaseMode != "":
			cmd["phase_mode"] = hold.PhaseMode
		case lpCfg.PhaseMode != "":
			cmd["phase_mode"] = lpCfg.PhaseMode
		}
		switch {
		case hold.PhaseSplitW > 0:
			cmd["phase_split_w"] = hold.PhaseSplitW
		case lpCfg.PhaseSplitW > 0:
			cmd["phase_split_w"] = lpCfg.PhaseSplitW
		}
		switch {
		case hold.MinPhaseHoldS > 0:
			cmd["min_phase_hold_s"] = hold.MinPhaseHoldS
		case lpCfg.MinPhaseHoldS > 0:
			cmd["min_phase_hold_s"] = lpCfg.MinPhaseHoldS
		}
		switch {
		case hold.Voltage > 0:
			cmd["voltage"] = hold.Voltage
		case c.site.Voltage > 0:
			cmd["voltage"] = c.site.Voltage
		}
		switch {
		case hold.MaxAmpsPerPhase > 0:
			cmd["max_amps_per_phase"] = hold.MaxAmpsPerPhase
		case c.site.MaxAmps > 0:
			cmd["max_amps_per_phase"] = c.site.MaxAmps
		}
		switch {
		case hold.SitePhases > 0:
			cmd["site_phases"] = hold.SitePhases
		case c.site.MaxAmps > 0:
			cmd["site_phases"] = c.site.Phases()
		}
	} else {
		cmdW, planReady := c.computeCommand(now, lpCfg, sample.PowerW)
		if !planReady {
			// No plan budget for this loadpoint right now — explicit
			// 0 W standdown so the charger pauses cleanly.
			cmdW = 0
		}
		cmd["power_w"] = cmdW
		// Pass operator's phase preferences through verbatim. The driver
		// reads these and decides 1Φ vs 3Φ based on its own knowledge of
		// charger min/max amps, phase-switch latency, and the requested W.
		if lpCfg.PhaseMode != "" {
			cmd["phase_mode"] = lpCfg.PhaseMode
		}
		if lpCfg.PhaseSplitW > 0 {
			cmd["phase_split_w"] = lpCfg.PhaseSplitW
		}
		if lpCfg.MinPhaseHoldS > 0 {
			cmd["min_phase_hold_s"] = lpCfg.MinPhaseHoldS
		}
		// Pass the site fuse so the driver can compute the per-phase
		// ceiling using the actual mains voltage instead of hard-coding
		// 230 V × 16 A. Drivers that don't support phase switching can
		// safely ignore these fields.
		if c.site.MaxAmps > 0 {
			cmd["max_amps_per_phase"] = c.site.MaxAmps
			cmd["site_phases"] = c.site.Phases()
		}
		if v := c.site.Voltage; v > 0 {
			cmd["voltage"] = v
		}
	}

	payload, err := json.Marshal(cmd)
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

// computeCommand resolves the W setpoint for a plugged loadpoint.
// Returns (0, false) when the planner has no allocation for this
// slot — caller commands an explicit 0 W standdown rather than
// leaving the charger riding the previous setpoint.
//
// The returned W is the CONTINUOUS energy-budget translation; the
// driver may further snap to its own discrete amperage steps and
// will clamp to the per-phase fuse ceiling derived from the
// `voltage` + `max_amps_per_phase` cmd fields.
func (c *Controller) computeCommand(now time.Time, lpCfg Config, currentPowerW float64) (float64, bool) {
	if c.plan == nil {
		return 0, false
	}
	d, ok := c.plan(now)
	if !ok {
		return 0, false
	}
	budgetWh, hasBudget := d.LoadpointEnergyWh[lpCfg.ID]
	if !hasBudget {
		return 0, false
	}
	remainingS := d.SlotEnd.Sub(now).Seconds()
	elapsed := d.SlotEnd.Sub(d.SlotStart).Seconds() - remainingS
	if elapsed < 0 {
		elapsed = 0
	}
	alreadyWh := currentPowerW * elapsed / 3600.0
	remainingWh := budgetWh - alreadyWh
	wantW := EnergyBudgetToPowerW(remainingWh, remainingS)
	// Clamp to the loadpoint's static MaxChargeW (configured cap; the
	// driver's per-phase fuse clamp is the ultimate safety stop).
	return SnapChargeW(wantW, lpCfg.MinChargeW, lpCfg.MaxChargeW, lpCfg.AllowedStepsW), true
}
