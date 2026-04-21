package control

import (
	"log/slog"
	"math"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/mpc"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// Mode is the operating mode of the control loop.
type Mode string

const (
	ModeIdle            Mode = "idle"
	ModeSelfConsumption Mode = "self_consumption"
	ModePeakShaving     Mode = "peak_shaving"
	ModeCharge          Mode = "charge"
	ModePriority        Mode = "priority"
	ModeWeighted        Mode = "weighted"

	// Planner modes: control loop pulls GridTargetW from the MPC plan
	// for the current 15-min slot. If the plan is stale (>30 min) or
	// missing, we fall back to self_consumption behavior and log.
	// The three flavors mirror mpc.Mode — the difference is only what
	// the planner is allowed to do when it builds the plan:
	//   - planner_self:      no grid-charging, no export discharge
	//   - planner_cheap:     grid-charge ok, no export discharge
	//   - planner_arbitrage: full freedom within SoC + power limits
	ModePlannerSelf      Mode = "planner_self"
	ModePlannerCheap     Mode = "planner_cheap"
	ModePlannerArbitrage Mode = "planner_arbitrage"
)

// IsPlannerMode reports whether the mode is one of the planner modes.
func (m Mode) IsPlannerMode() bool {
	return m == ModePlannerSelf || m == ModePlannerCheap || m == ModePlannerArbitrage
}

// PlanTargetFunc is injected by main.go: given the current time, returns
// the plan's directive for the current slot. When ok=false, the plan is
// stale/missing and the control loop falls back to self_consumption with
// grid_target=0.
//
// Returns (mode_string, grid_target_w, ok). mode_string maps to a Mode
// constant; the dispatch uses its existing mode logic for HOW batteries
// respond. The plan is a scheduler, not a regulator.
//
// Legacy — the new contract (energy-allocation per slot, EMS converts to
// power) uses SlotDirectiveFunc. See docs/plan-ems-contract.md.
type PlanTargetFunc func(now time.Time) (string, float64, bool)

// SlotDirective mirrors mpc.SlotDirective — we redefine here to keep the
// control package import-cycle free. Populated by main.go's injected
// SlotDirectiveFunc adapter.
type SlotDirective struct {
	SlotStart       time.Time
	SlotEnd         time.Time
	BatteryEnergyWh float64 // site-signed: + = charge, − = discharge
	SoCTargetPct    float64
	Strategy        string // echoed for logging / API; mirrors mpc.Mode
}

// SlotDirectiveFunc returns the plan's energy-allocation directive for
// the slot containing `now`. When ok=false the plan is stale or missing
// and the control loop falls back to auto_fallback (local self-consumption
// rule with no forward-planning) — same behavior as PlanTargetFunc's
// stale-plan branch.
type SlotDirectiveFunc func(now time.Time) (SlotDirective, bool)

// MaxCommandW is the default per-command power cap (±5 kW), applied when
// a driver has no per-battery override. A deliberate floor-to-conservative
// pick for v0.2x: safer than guessing a driver's headroom wrong. Override
// on a per-driver basis via `config.Driver.max_charge_w` /
// `max_discharge_w` (see PowerLimits + State.DriverLimits, issue #145).
const MaxCommandW = 5000

// plannerSelfIdleOverrideW is the live-grid export threshold at which
// a planner_self idle-gate gets overridden back to reactive PI. Rationale
// in issue #153: when the plan decided "idle this slot" based on a
// forecasted surplus, and reality is exporting well beyond that amount,
// the plan's opportunity-cost math doesn't match live conditions — we're
// just leaking PV out the grid at curtail pricing. 1 kW is loose enough
// to tolerate normal forecast noise but clearly catches the "plan thought
// surplus = 3 kW, reality is 10 kW" class of error.
const plannerSelfIdleOverrideW = 1000

// PowerLimits holds the per-driver charge/discharge ceiling. Zero on
// either field means "use the global MaxCommandW default" — the value
// an unset config key carries through the YAML → Driver struct →
// dispatch map pipeline. A non-zero value overrides the default at
// every clamp point (clampWithSoC and the post-slew re-clamp).
//
// A per-driver cap higher than the site fuse doesn't buy extra throughput:
// the fuse-guard still scales at the site boundary (#145 safety invariant).
type PowerLimits struct {
	MaxChargeW    float64
	MaxDischargeW float64
}

// DispatchTarget is one command to issue to a single battery driver.
// `TargetW` is in site sign convention:
//   + = charge the battery (battery becomes a load, site imports more)
//   − = discharge the battery (battery becomes a source, site imports less)
type DispatchTarget struct {
	Driver  string  `json:"driver"`
	TargetW float64 `json:"target_w"`
	Clamped bool    `json:"clamped"`
}

// State holds all persistent state for one instance of the control loop.
// One per site.
type State struct {
	Mode           Mode
	GridTargetW    float64
	GridToleranceW float64
	SiteMeterDriver string

	// For Priority mode
	PriorityOrder []string
	// For Weighted mode
	Weights map[string]float64

	// Peak limit — enforced only in PeakShaving mode
	PeakLimitW float64
	// EV charging signal — batteries won't try to cover this much of import
	EVChargingW float64
	// BatteryCoversEV overrides the default EV-exclusion behaviour. When
	// false (default), EVChargingW is subtracted from the meter reading
	// before the PI runs so batteries don't shuffle energy through the
	// inverter to feed the EV on a normal day. When true, the subtraction
	// is skipped and the battery is free to discharge into the EV up to
	// its own SoC / power / fuse clamps — useful in price-arbitrage
	// situations where the operator wants to drain the battery now and
	// refill it later from cheap solar. Persisted in state.db via
	// "battery_covers_ev"; toggled from HA and POST /api/battery_covers_ev.
	BatteryCoversEV bool

	// PI controller (outer, site-level)
	PI *PIController

	// Slew + holdoff
	SlewRateW            float64
	MinDispatchIntervalS int
	LastDispatch         *time.Time
	PrevTargets          map[string]float64

	LastTargets []DispatchTarget

	// Cascade toggle — set by main.go based on whether models exist
	UseCascade bool

	// PlanTarget is consulted at the top of each control cycle when
	// Mode is a planner mode. Nil outside planner modes. Injected from
	// main.go — the control package doesn't need to know about mpc.
	//
	// Legacy (grid-target driven). The new path uses SlotDirective.
	PlanTarget PlanTargetFunc

	// SlotDirective is the new plan→EMS callback (energy-allocation
	// contract). When set AND UseEnergyDispatch is true, ComputeDispatch
	// uses the energy-driven code path instead of the PI-on-grid-target
	// path. Injected from main.go like PlanTarget. See
	// docs/plan-ems-contract.md.
	SlotDirective SlotDirectiveFunc

	// UseEnergyDispatch toggles between the legacy PI-on-grid path and
	// the new energy-allocation path. False until validated in production
	// and flipped via config. Default off preserves today's behavior.
	UseEnergyDispatch bool

	// currentDirective + slotDelivered track the active slot's energy
	// accounting. Reset when the slot rolls over (by SlotStart equality).
	// Zero-valued until UseEnergyDispatch fires its first cycle.
	currentDirective SlotDirective
	slotDelivered    float64   // Wh delivered to batteries since slot start
	lastTickTs       time.Time // for ∫ battery_w dt

	// PlanStale tracks whether the last cycle fell back to self_consumption
	// because the plan was missing. Surfaced via the API for the UI.
	PlanStale bool

	// InverterGroups maps driver name → inverter-group tag (e.g.
	// "ferroamp", "sungrow"). Drivers sharing a tag are assumed to
	// share a single inverter unit: their PV readings feed DC-direct
	// into the same-group battery. During charging, `distributeProportional`
	// prefers routing the total first to batteries whose group also
	// has live PV output, so a kWh doesn't cross inverters through
	// the AC bus (DC→AC→AC→DC ≈ 3-4 pp loss vs DC-local). Nil or empty
	// preserves the capacity-proportional default. Issue #143.
	InverterGroups map[string]string

	// DriverLimits maps driver name → per-battery charge/discharge cap.
	// Missing entries (or zero fields) fall through to the global
	// MaxCommandW default. Consulted in every clamp step — per-battery
	// clampWithSoC, post-slew re-clamp, and fuse-guard's reference to
	// total headroom. Hot-swappable via the config-reload watcher.
	// Issue #145.
	DriverLimits map[string]PowerLimits
}

// NewState creates default control state (port of Rust ControlState::new).
func NewState(gridTargetW, gridToleranceW float64, siteMeter string) *State {
	pi := NewPI(0.5, 0.1, 3000, 10000)
	pi.Setpoint = gridTargetW
	return &State{
		Mode:                 ModeSelfConsumption,
		GridTargetW:          gridTargetW,
		GridToleranceW:       gridToleranceW,
		SiteMeterDriver:      siteMeter,
		PriorityOrder:        nil,
		Weights:              map[string]float64{},
		PeakLimitW:           5000,
		EVChargingW:          0,
		PI:                   pi,
		SlewRateW:            500,
		MinDispatchIntervalS: 5,
		PrevTargets:          map[string]float64{},
		UseCascade:           true,
	}
}

// SetGridTarget updates both the state and the PI setpoint.
func (s *State) SetGridTarget(w float64) {
	s.GridTargetW = w
	s.PI.Setpoint = w
}

// batteryInfo is internal state read from telemetry per dispatch cycle.
type batteryInfo struct {
	driver        string
	capacityWh    float64
	currentW      float64
	soc           float64
	online        bool
	group         string  // inverter-affinity tag; empty = untagged (#143)
	maxChargeW    float64 // per-driver cap; 0 = use MaxCommandW default (#145)
	maxDischargeW float64 // per-driver cap; 0 = use MaxCommandW default (#145)
}

// chargeCap returns the effective per-battery charge ceiling, falling
// back to MaxCommandW when the driver didn't set an explicit limit.
// Kept a method so every clamp point queries the same fallback rule.
func (b batteryInfo) chargeCap() float64 {
	if b.maxChargeW > 0 {
		return b.maxChargeW
	}
	return MaxCommandW
}

// dischargeCap is the symmetric version of chargeCap for discharge
// targets. Returned as a positive magnitude; callers apply the minus
// sign at the comparison site.
func (b batteryInfo) dischargeCap() float64 {
	if b.maxDischargeW > 0 {
		return b.maxDischargeW
	}
	return MaxCommandW
}

// ComputeDispatch runs one cycle of the control loop and returns the targets
// to issue. Caller is expected to pass them to drivers.
//
// driverCapacities: map of driver name → battery capacity in Wh. Only drivers
// present here are considered for battery dispatch.
//
// fuseMaxW: total site current budget (amps × volts × phases).
func ComputeDispatch(
	store *telemetry.Store,
	state *State,
	driverCapacities map[string]float64,
	fuseMaxW float64,
) []DispatchTarget {
	// ---- Planner modes: the plan is a scheduler, not a regulator ----
	// The plan decides WHEN each strategy applies (self-consumption now,
	// charge at 02:00, export at 17:00). The EMS decides HOW batteries
	// respond every 5 s based on the live meter.
	//
	// Three execution paths, selected by the operator-picked planner mode:
	//
	//   * planner_self — reactive self-consumption (PI → gridW=0) with a
	//     per-slot idle gate from the plan. Honours the mode's contract
	//     ("never imports to charge, never exports via the battery")
	//     against forecast error. See docs/plan-ems-contract.md §"Exception:
	//     planner_self" and issue #130.
	//
	//   * planner_cheap / planner_arbitrage with UseEnergyDispatch=true
	//     (default): energy-allocation. Plan returns battery energy for
	//     the slot; EMS converts to instantaneous power from
	//     (remaining_wh / remaining_s); grid flow is the residual.
	//
	//   * planner_cheap / planner_arbitrage with UseEnergyDispatch=false
	//     (opt-out): legacy PI-on-grid-target path. Plan returns
	//     grid_target_w; PI chases it.
	//
	// All three share gather-batteries → distribute → slew → fuse below.
	// They differ only in how `totalCorrection` is computed and whether
	// the deadband applies.
	effectiveMode := state.Mode
	useEnergyPath := false
	// plannerSelfIdleGate is true when operator picked planner_self AND the
	// plan allocated a below-threshold amount of battery action for the
	// current slot — the EMS holds the battery at 0 (ramping via slew)
	// regardless of live surplus, so the DP's decision to save SoC for a
	// later slot is honoured.
	plannerSelfIdleGate := false
	var currentDirective SlotDirective
	switch {
	case state.Mode == ModePlannerSelf:
		effectiveMode = ModeSelfConsumption
		state.SetGridTarget(0)
		// Reset the energy-allocation bookkeeping so a future switch to
		// planner_cheap / planner_arbitrage within the same 15-minute
		// slot can't read stale `slotDelivered` accumulated before the
		// operator hopped through planner_self. Without this reset, the
		// `SlotStart` comparison in the energy path would match (still
		// the same clock-aligned slot) and skip its own rollover reset
		// — reading the pre-hop delivered-Wh number and over-commanding
		// charge/discharge for the rest of the slot. Codex P2 on PR #131.
		state.currentDirective = SlotDirective{}
		state.slotDelivered = 0
		state.lastTickTs = time.Time{}
		planFresh := false
		if state.SlotDirective != nil {
			if dir, ok := state.SlotDirective(time.Now()); ok {
				planFresh = true
				state.PlanStale = false
				slotH := dir.SlotEnd.Sub(dir.SlotStart).Hours()
				if slotH > 0 && math.Abs(dir.BatteryEnergyWh)/slotH < mpc.IdleGateThresholdW {
					plannerSelfIdleGate = true
				}
			}
		}
		if !planFresh {
			if !state.PlanStale {
				slog.Warn("planner_self: plan stale — reactive self_consumption, no idle gates")
			}
			state.PlanStale = true
		}
	case state.Mode.IsPlannerMode():
		// planner_cheap / planner_arbitrage.
		if state.UseEnergyDispatch && state.SlotDirective != nil {
			if dir, ok := state.SlotDirective(time.Now()); ok {
				currentDirective = dir
				useEnergyPath = true
				// Distribution mode is decoupled from planner strategy in
				// the energy path — the operator-selected strategy drives
				// the plan's DP, distribution is always proportional across
				// online batteries. If the operator wants priority or
				// weighted, they use the manual modes, not a planner mode.
				effectiveMode = ModeSelfConsumption
				state.PlanStale = false
			}
		}
		if !useEnergyPath {
			var modeStr string
			var gridW float64
			ok := false
			if state.PlanTarget != nil {
				modeStr, gridW, ok = state.PlanTarget(time.Now())
			}
			if ok {
				effectiveMode = Mode(modeStr)
				state.SetGridTarget(gridW)
				state.PlanStale = false
			} else {
				if !state.PlanStale {
					slog.Warn("mpc plan stale — falling back to self_consumption")
				}
				effectiveMode = ModeSelfConsumption
				state.SetGridTarget(0)
				state.PlanStale = true
			}
		}
	}

	// ---- Idle + Charge short-circuits ----
	switch effectiveMode {
	case ModeIdle:
		state.LastTargets = nil
		return nil
	case ModeCharge:
		targets := chargeAll(store, driverCapacities, state.DriverLimits)
		state.LastTargets = targets
		return targets
	}

	// ---- Holdoff ----
	if state.LastDispatch != nil {
		elapsed := time.Since(*state.LastDispatch).Seconds()
		if elapsed < float64(state.MinDispatchIntervalS) {
			return nil
		}
	}

	// ---- Read site meter ----
	rawGridW := 0.0
	if r := store.Get(state.SiteMeterDriver, telemetry.DerMeter); r != nil {
		rawGridW = r.SmoothedW
	}
	// Live EV charger readings override the manual slider on each tick —
	// hardware truth beats guesses. Only override when something >0 is
	// actually being reported, so an offline / stale EV driver doesn't
	// silently zero out a user-set manual value.
	evSum := store.SumOnlineEVW()
	if evSum > 0 {
		state.EVChargingW = evSum
	}
	// EV signal: subtract EV load from grid so batteries don't try to cover
	// it. EV is always a positive import at the meter; subtracting it makes
	// the "effective grid" the controller works on the house-side portion
	// only — a sensible default that avoids shuffling energy through the
	// inverter twice on a normal day.
	//
	// BatteryCoversEV (default false) flips this: the operator opts in to
	// have the battery discharge into the EV. Useful when grid prices are
	// high right now but expected to drop later (e.g. solar coming up), so
	// it's cheaper to drain the battery now and refill it off-peak. All
	// clamps (SoC, per-driver MaxDischargeW, fuse guard) still apply —
	// exceeding battery capacity just means the residual comes from grid.
	gridW := rawGridW
	if !state.BatteryCoversEV {
		gridW -= state.EVChargingW
	}

	// ---- planner_self idle-gate override (#153) ----
	// The DP's decision to idle this slot assumed a forecasted surplus.
	// If live conditions are exporting significantly beyond that —
	// forecast error on the PV or load twin, typically — idling is just
	// pouring free energy out the grid. Override to reactive PI so
	// batteries absorb the unused surplus.
	//
	// Self-consumption invariants still hold (PI targets gridW=0; the
	// battery can only move grid toward zero, not past it), so the
	// override can't mutate planner_self into export.
	if plannerSelfIdleGate && gridW < -plannerSelfIdleOverrideW {
		slog.Info("planner_self: idle-gate overridden — live export exceeds threshold",
			"grid_w", gridW, "threshold_w", plannerSelfIdleOverrideW)
		plannerSelfIdleGate = false
	}

	// ---- Gather online batteries ----
	batteries := make([]batteryInfo, 0, len(driverCapacities))
	for name, cap := range driverCapacities {
		r := store.Get(name, telemetry.DerBattery)
		h := store.DriverHealth(name)
		if r == nil || h == nil {
			continue
		}
		// Default to near-empty SoC so dispatch errs on the side of
		// caution (no discharge) if a battery never reports SoC.
		// Using 0.5 would allow discharge of a potentially empty battery.
		soc := 0.1
		if r.SoC != nil {
			soc = *r.SoC
		}
		lim := state.DriverLimits[name]
		batteries = append(batteries, batteryInfo{
			driver:        name,
			capacityWh:    cap,
			currentW:      r.SmoothedW,
			soc:           soc,
			online:        h.IsOnline(),
			group:         state.InverterGroups[name],
			maxChargeW:    lim.MaxChargeW,
			maxDischargeW: lim.MaxDischargeW,
		})
	}
	onlineBats := make([]batteryInfo, 0, len(batteries))
	for _, b := range batteries {
		if b.online {
			onlineBats = append(onlineBats, b)
		}
	}
	if len(onlineBats) == 0 {
		state.LastTargets = nil
		return nil
	}

	// ---- Sum of battery current power (site-signed) ----
	// Used by both paths: legacy distributors take (currentTotal + correction);
	// energy path computes correction as (desired_total - currentTotal).
	var currentTotal float64
	for _, b := range onlineBats {
		currentTotal += b.currentW
	}

	// ---- Compute totalCorrection — three paths diverge here ----
	var totalCorrection float64
	switch {
	case plannerSelfIdleGate:
		// planner_self + plan says idle this slot: drive the battery
		// total toward 0 regardless of live grid flow. Slew ramps it
		// down over several cycles; the PI stays out of it so no
		// integral wind-up carries into the next slot.
		state.PI.Reset()
		totalCorrection = -currentTotal
		// deliberately skip the deadband — it's a gridW check and
		// doesn't see "battery wants to be at zero but isn't yet".
	case useEnergyPath:
		// Energy-allocation path: plan's slot directive says "this many Wh
		// over this slot". Derive the instantaneous power needed to hit the
		// remaining energy in the remaining time, then pass (target - currentTotal)
		// as the correction the existing distributors expect.
		now := time.Now()
		// Slot rollover: new slot → reset the delivered accumulator.
		if !currentDirective.SlotStart.Equal(state.currentDirective.SlotStart) {
			state.currentDirective = currentDirective
			state.slotDelivered = 0
			state.lastTickTs = now
		} else {
			// Accumulate energy delivered since the last tick, using live
			// battery telemetry (the truth about what the fleet is doing
			// right now). This lets the formula course-correct when the
			// commanded setpoint couldn't be met.
			dt := now.Sub(state.lastTickTs).Seconds()
			if dt > 0 && dt < 300 { // cap dt at 5min so a long pause doesn't poison accumulator
				state.slotDelivered += currentTotal * dt / 3600.0
			}
			state.lastTickTs = now
		}
		remainingWh := currentDirective.BatteryEnergyWh - state.slotDelivered
		remainingS := currentDirective.SlotEnd.Sub(now).Seconds()
		var targetTotalW float64
		if remainingS > 0.5 {
			targetTotalW = remainingWh * 3600.0 / remainingS
		}
		// Grid target is a pure observation on this path — useful for UI
		// + legacy API, not driving PI. Use SetGridTarget so both
		// GridTargetW *and* PI.Setpoint move to 0 in lockstep: if the
		// operator later switches out of a planner mode, the legacy
		// path's PI.Update would otherwise compute error against a
		// stale setpoint while deadband/error checks use the synced
		// GridTargetW, producing wrong corrections.
		state.SetGridTarget(0)
		state.PI.Reset()
		totalCorrection = targetTotalW - currentTotal
	default:
		// Legacy PI-on-grid-target path. Used by:
		//   - manual modes (self_consumption, peak_shaving, priority, weighted)
		//   - planner_self (the "participate reactively" branch — idle-gate
		//     already handled above)
		//   - planner_cheap / planner_arbitrage when UseEnergyDispatch=false
		var errW float64
		switch effectiveMode {
		case ModePeakShaving:
			// Only act when grid import exceeds peak_limit. Allow any amount of
			// export, allow import up to peak_limit.
			if gridW > state.PeakLimitW {
				errW = gridW - state.PeakLimitW
			} else if gridW < 0 {
				errW = gridW // exporting → charge with surplus
			} else {
				errW = 0
			}
		default:
			errW = gridW - state.GridTargetW
		}

		// Deadband only applies to the legacy path — the energy formula
		// produces small corrections naturally when close to target.
		if math.Abs(errW) < state.GridToleranceW {
			return nil
		}

		// Outer PI — drives total correction we want across all batteries.
		// Site convention: gridW positive = too much import → we want to discharge
		// batteries (site-signed correction should be negative).
		// PI setpoint = GridTargetW, measurement = gridW.
		// For PeakShaving we feed a slightly different measurement so the same PI works.
		var piMeasurement float64
		if effectiveMode == ModePeakShaving {
			piMeasurement = state.GridTargetW + errW
		} else {
			piMeasurement = gridW
		}
		out := state.PI.Update(piMeasurement)
		totalCorrection = out.Output
	}

	// ---- Per-group PV surplus for DC-local charge routing (#143) ----
	// Empty when no drivers carry an inverter-group tag → distributeProportional
	// falls through to its capacity-only split (today's behavior).
	groupPV := map[string]float64{}
	if len(state.InverterGroups) > 0 {
		for _, r := range store.ReadingsByType(telemetry.DerPV) {
			group := state.InverterGroups[r.Driver]
			if group == "" {
				continue // untagged PV: no locality signal, treat as AC-bus
			}
			// PV is site-signed (negative = generating). Magnitude = surplus
			// potentially routable DC-direct to the same-group battery.
			groupPV[group] += math.Abs(r.SmoothedW)
		}
	}

	// ---- Distribute across batteries ----
	var raw []DispatchTarget
	switch effectiveMode {
	case ModeSelfConsumption, ModePeakShaving:
		raw = distributeProportional(onlineBats, totalCorrection, groupPV)
	case ModePriority:
		raw = distributePriority(onlineBats, totalCorrection, state.PriorityOrder)
	case ModeWeighted:
		raw = distributeWeighted(onlineBats, totalCorrection, state.Weights)
	}

	// ---- Slew rate limit per driver ----
	//
	// Slew FROM the battery's actual measured output (SmoothedW), not
	// from the previous command. When the battery can't meet a command
	// (e.g. SoC at min and commanded to discharge, SoC at max and
	// commanded to charge, or driver offline), the command stays pinned
	// far from reality. Using the stored command as the slew anchor then
	// forces `|target - stale_command| / slew_rate` cycles of ramping
	// before the direction reverses — a 5 kW stale command with a 500
	// W/cycle slew at 5 s interval means 50 s of wasted export before
	// the surplus-absorb starts.
	//
	// Using actual-smoothed-W is the truth about where the battery is,
	// and lets the dispatch pivot immediately when the setpoint reverses.
	// Falls back to the previous command if no reading is available
	// (driver just started, or stale telemetry).
	for i := range raw {
		anchor, hasAnchor := state.PrevTargets[raw[i].Driver]
		if r := store.Get(raw[i].Driver, telemetry.DerBattery); r != nil {
			anchor = r.SmoothedW
			hasAnchor = true
		}
		if !hasAnchor {
			continue
		}
		delta := raw[i].TargetW - anchor
		if math.Abs(delta) > state.SlewRateW {
			sign := 1.0
			if delta < 0 {
				sign = -1.0
			}
			raw[i].TargetW = anchor + sign*state.SlewRateW
			raw[i].Clamped = true
		}
	}

	// ---- Re-clamp after slew ----
	// The slew anchor is the battery's actual output (SmoothedW). If the
	// battery was already beyond its per-command cap (e.g. after a
	// manual restart, external control, or driver returning an out-of-range
	// reading), the slewed target inherits the overshoot. Re-apply the
	// per-driver cap (DriverLimits, falling back to MaxCommandW) so we
	// never issue a command outside safe bounds.
	for i := range raw {
		maxC := float64(MaxCommandW)
		maxD := float64(MaxCommandW)
		if lim, ok := state.DriverLimits[raw[i].Driver]; ok {
			if lim.MaxChargeW > 0 {
				maxC = lim.MaxChargeW
			}
			if lim.MaxDischargeW > 0 {
				maxD = lim.MaxDischargeW
			}
		}
		if raw[i].TargetW > maxC {
			raw[i].TargetW = maxC
			raw[i].Clamped = true
		} else if raw[i].TargetW < -maxD {
			raw[i].TargetW = -maxD
			raw[i].Clamped = true
		}
	}

	// ---- Fuse guard (bidirectional, #145) ----
	raw = applyFuseGuard(raw, store, state.SiteMeterDriver, fuseMaxW)

	// Update state
	now := time.Now()
	state.LastDispatch = &now
	for _, t := range raw {
		state.PrevTargets[t.Driver] = t.TargetW
	}
	state.LastTargets = raw
	return raw
}

// distributeProportional splits the total desired battery power across the
// available batteries by capacity. Each battery gets its share of the TOTAL
// desired site battery power — not its share of the delta. This prevents the
// "drift" bug where each battery drifts independently under prolonged error.
//
// When `groupPV` is non-empty AND the fleet is charging (desiredTotal > 0),
// the algorithm first routes up to `min(desiredTotal, ΣPV_g)` preferentially
// to batteries whose inverter-group also reports live PV output — keeping
// the flow DC-coupled on the same inverter avoids the ~3-4 pp round-trip
// loss of cross-inverter AC routing. Any remaining correction is then
// spread proportionally across all batteries (capacity-weighted), identical
// to today. With no `groupPV` info or during discharge, the algorithm
// collapses to a single capacity-proportional split. Issue #143.
func distributeProportional(bats []batteryInfo, totalCorrection float64, groupPV map[string]float64) []DispatchTarget {
	var totalCap float64
	for _, b := range bats {
		totalCap += b.capacityWh
	}
	if totalCap <= 0 {
		return nil
	}
	var currentTotal float64
	for _, b := range bats {
		currentTotal += b.currentW
	}
	desiredTotal := currentTotal + totalCorrection

	// Discharge, idle, or no PV locality info → capacity-only split.
	// Discharge energy flows to the AC bus regardless of where it
	// originated, so DC-locality has no win for the negative branch.
	var totalPV float64
	for _, w := range groupPV {
		totalPV += w
	}
	if desiredTotal <= 0 || totalPV <= 0 {
		return distributeByCapacity(bats, desiredTotal, totalCap)
	}

	// Charging with PV locality info: prefer DC-local routing.
	//   localCap  = min(desiredTotal, totalPV)  — how much of the total
	//               fleet charge can be kept DC-coupled
	//   overflow  = desiredTotal - localCap     — excess that has to cross
	//               inverters via the AC bus; no locality benefit, so
	//               it's allocated by capacity like today.
	//
	// Within the local pool, each group gets a share of localCap
	// proportional to its PV output; within a group, that share is split
	// by capacity (same rule as the fleet-wide split).
	localCap := math.Min(desiredTotal, totalPV)
	overflow := desiredTotal - localCap

	capByGroup := map[string]float64{}
	for _, b := range bats {
		capByGroup[b.group] += b.capacityWh
	}

	out := make([]DispatchTarget, 0, len(bats))
	for _, b := range bats {
		var localShare float64
		if capG := capByGroup[b.group]; capG > 0 && groupPV[b.group] > 0 {
			localShare = (groupPV[b.group] / totalPV) * localCap * (b.capacityWh / capG)
		}
		overflowShare := overflow * (b.capacityWh / totalCap)
		target := localShare + overflowShare
		clamped, was := clampWithSoC(target, b)
		out = append(out, DispatchTarget{Driver: b.driver, TargetW: clamped, Clamped: was})
	}
	return out
}

// distributeByCapacity is the legacy capacity-proportional split, extracted
// so both the discharge path and the no-groupPV fallback share the same code.
func distributeByCapacity(bats []batteryInfo, desiredTotal, totalCap float64) []DispatchTarget {
	out := make([]DispatchTarget, 0, len(bats))
	for _, b := range bats {
		target := desiredTotal * (b.capacityWh / totalCap)
		clamped, was := clampWithSoC(target, b)
		out = append(out, DispatchTarget{Driver: b.driver, TargetW: clamped, Clamped: was})
	}
	return out
}

// distributePriority assigns correction to the primary battery first, falling
// back to secondaries only when saturated.
func distributePriority(bats []batteryInfo, totalCorrection float64, order []string) []DispatchTarget {
	remaining := totalCorrection
	out := make([]DispatchTarget, 0, len(bats))
	// Named order first
	for _, name := range order {
		for _, b := range bats {
			if b.driver != name {
				continue
			}
			t := b.currentW + remaining
			clamped, was := clampWithSoC(t, b)
			remaining -= clamped - b.currentW
			out = append(out, DispatchTarget{Driver: b.driver, TargetW: clamped, Clamped: was})
		}
	}
	// Unmentioned batteries stay at their current power
	for _, b := range bats {
		seen := false
		for _, o := range out {
			if o.Driver == b.driver {
				seen = true
				break
			}
		}
		if !seen {
			out = append(out, DispatchTarget{Driver: b.driver, TargetW: b.currentW})
		}
	}
	return out
}

// distributeWeighted splits by custom weights. Missing batteries default to weight=1.
func distributeWeighted(bats []batteryInfo, totalCorrection float64, weights map[string]float64) []DispatchTarget {
	var totalW float64
	for _, b := range bats {
		w, ok := weights[b.driver]
		if !ok { w = 1.0 }
		totalW += w
	}
	if totalW <= 0 { return nil }
	var currentTotal float64
	for _, b := range bats { currentTotal += b.currentW }
	desiredTotal := currentTotal + totalCorrection

	out := make([]DispatchTarget, 0, len(bats))
	for _, b := range bats {
		w, ok := weights[b.driver]
		if !ok { w = 1.0 }
		t := desiredTotal * (w / totalW)
		clamped, was := clampWithSoC(t, b)
		out = append(out, DispatchTarget{Driver: b.driver, TargetW: clamped, Clamped: was})
	}
	return out
}

// chargeAll forces every online battery to its per-driver MaxChargeW
// (or MaxCommandW default when the driver doesn't override). Used by
// the "Charge" manual mode as a sanity-check / pre-peak-fill knob.
// Issue #145 — previously hardcoded at +5 kW regardless of hardware.
func chargeAll(store *telemetry.Store, capacities map[string]float64, limits map[string]PowerLimits) []DispatchTarget {
	out := make([]DispatchTarget, 0)
	for name := range capacities {
		h := store.DriverHealth(name)
		if h == nil || !h.IsOnline() {
			continue
		}
		target := float64(MaxCommandW)
		if lim, ok := limits[name]; ok && lim.MaxChargeW > 0 {
			target = lim.MaxChargeW
		}
		// Site convention: + = charge.
		out = append(out, DispatchTarget{Driver: name, TargetW: target})
	}
	return out
}

// clampWithSoC applies the hard safety clamps for one battery command:
//   - don't discharge below SoC 5 % (site: don't make target < 0 when SoC < 0.05);
//     BMS handles fine-grained SoC but we never ask it to pull an empty pack.
//   - cap charge at the battery's MaxChargeW (falls back to MaxCommandW default).
//   - cap discharge at the battery's MaxDischargeW (same fallback).
//
// The caps are asymmetric on purpose — real hybrid inverters often have
// different charge and discharge capability (e.g. Ferroamp 15 kW charge /
// 10 kW discharge). Issue #145.
func clampWithSoC(target float64, b batteryInfo) (float64, bool) {
	clamped := target
	wasClamped := false
	// Block discharge when the battery is empty.
	if b.soc < 0.05 && target < 0 {
		clamped = 0
		wasClamped = true
	}
	if clamped > b.chargeCap() {
		clamped = b.chargeCap()
		wasClamped = true
	} else if clamped < -b.dischargeCap() {
		clamped = -b.dischargeCap()
		wasClamped = true
	}
	return clamped, wasClamped
}

// applyFuseGuard enforces the site fuse budget on both directions of
// grid flow — import AND export. Any dispatched target would shift the
// grid flow by (target − current_battery_power); the guard predicts
// the post-dispatch grid reading and, if it would exceed ±fuseMaxW,
// scales the same-direction targets toward zero until the boundary is
// respected.
//
// Prediction (site sign: grid = load + pv + battery):
//
//	predicted_grid = live_grid − Σ current_battery_w + Σ target
//
// Because load and pv are invariant in the 5 s dispatch window, only
// the battery row changes when we apply new targets.
//
// Directional scaling:
//   - predicted > +fuseMaxW (too much import): scale POSITIVE (charge)
//     targets down. 1 W less charge = 1 W less import, so the reduction
//     directly offsets the overage.
//   - predicted < −fuseMaxW (too much export): scale NEGATIVE (discharge)
//     targets toward zero — the symmetric case.
//
// Issue #145 changed the guard from "PV + discharge > fuse → scale
// discharge" (old, discharge-only, assumed zero load) to this
// bidirectional predicted-grid approach so heavy PV-free charge slots
// can't push aggregate imports past the fuse. The new path also uses
// live load inference so the discharge side no longer over-scales
// during high-load hours.
func applyFuseGuard(targets []DispatchTarget, store *telemetry.Store, siteMeter string, fuseMaxW float64) []DispatchTarget {
	if fuseMaxW <= 0 {
		return targets
	}
	// Aggregate live battery power so we can hold load+pv constant.
	var currentBat float64
	for _, r := range store.ReadingsByType(telemetry.DerBattery) {
		currentBat += r.SmoothedW
	}
	var currentGrid float64
	if siteMeter != "" {
		if r := store.Get(siteMeter, telemetry.DerMeter); r != nil {
			currentGrid = r.SmoothedW
		}
	}
	var sumTarget float64
	for _, t := range targets {
		sumTarget += t.TargetW
	}
	predicted := currentGrid - currentBat + sumTarget

	if math.Abs(predicted) <= fuseMaxW {
		return targets
	}

	out := make([]DispatchTarget, len(targets))
	copy(out, targets)

	switch {
	case predicted > fuseMaxW:
		// Too much import → shrink charging.
		overage := predicted - fuseMaxW
		var totalCharge float64
		for _, t := range out {
			if t.TargetW > 0 {
				totalCharge += t.TargetW
			}
		}
		if totalCharge <= 0 {
			// No charge commands to pull back — the overage is load-driven
			// and nothing this layer can do. Leave targets untouched;
			// operator/BMS/load shedding is the next lever.
			return out
		}
		newTotal := totalCharge - overage
		if newTotal < 0 {
			newTotal = 0
		}
		scale := newTotal / totalCharge
		for i := range out {
			if out[i].TargetW > 0 {
				out[i].TargetW *= scale
				out[i].Clamped = true
			}
		}
	default: // predicted < -fuseMaxW
		overage := -fuseMaxW - predicted // positive magnitude over export fuse
		var totalDischarge float64
		for _, t := range out {
			if t.TargetW < 0 {
				totalDischarge += -t.TargetW
			}
		}
		if totalDischarge <= 0 {
			// Nothing to scale — PV alone is pushing past the fuse.
			// Fuse-guard can't curtail PV; pv_limit_w from the plan
			// is the lever in that scenario (annotateCurtailment).
			return out
		}
		newTotal := totalDischarge - overage
		if newTotal < 0 {
			newTotal = 0
		}
		scale := newTotal / totalDischarge
		for i := range out {
			if out[i].TargetW < 0 {
				out[i].TargetW *= scale
				out[i].Clamped = true
			}
		}
	}
	return out
}
