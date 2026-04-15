package control

import (
	"math"
	"time"

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
// (grid_target_w, ok). When ok=false, the plan is stale/missing and the
// control loop falls back to self_consumption with grid_target_w = 0.
type PlanTargetFunc func(now time.Time) (float64, bool)

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
	PlanTarget PlanTargetFunc

	// PlanStale tracks whether the last cycle fell back to self_consumption
	// because the plan was missing. Surfaced via the API for the UI.
	PlanStale bool
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
	driver     string
	capacityWh float64
	currentW   float64
	soc        float64
	online     bool
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
	// ---- Planner modes: override grid_target from the plan ----
	// We don't mutate state.Mode — the operator's selected mode is
	// preserved for display. Once the grid_target is set from the plan,
	// the rest of this function behaves like self_consumption: PI chases
	// the target we just set. The downstream switches special-case this
	// by treating planner modes as self_consumption.
	if state.Mode.IsPlannerMode() {
		var target float64
		ok := false
		if state.PlanTarget != nil {
			target, ok = state.PlanTarget(time.Now())
		}
		if ok {
			state.SetGridTarget(target)
			state.PlanStale = false
		} else {
			// Plan missing or stale. Fail-safe: self_consumption with
			// target=0. Operator sees PlanStale=true.
			state.SetGridTarget(0)
			state.PlanStale = true
		}
	}

	// effectiveMode collapses planner modes into self_consumption for
	// the rest of this function. Real state.Mode is untouched.
	effectiveMode := state.Mode
	if effectiveMode.IsPlannerMode() {
		effectiveMode = ModeSelfConsumption
	}

	// ---- Idle + Charge short-circuits ----
	switch effectiveMode {
	case ModeIdle:
		state.LastTargets = nil
		return nil
	case ModeCharge:
		targets := chargeAll(store, driverCapacities)
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
	var evSum float64
	for _, r := range store.ReadingsByType(telemetry.DerEV) {
		evSum += r.SmoothedW
	}
	if evSum > 0 {
		state.EVChargingW = evSum
	}
	// EV signal: subtract EV load from grid so batteries don't try to cover it.
	// EV is always a positive import at the meter; subtracting it makes the
	// "effective grid" the controller works on the house-side portion only.
	gridW := rawGridW - state.EVChargingW

	// ---- Gather online batteries ----
	batteries := make([]batteryInfo, 0, len(driverCapacities))
	for name, cap := range driverCapacities {
		r := store.Get(name, telemetry.DerBattery)
		h := store.DriverHealth(name)
		if r == nil || h == nil {
			continue
		}
		soc := 0.5
		if r.SoC != nil {
			soc = *r.SoC
		}
		batteries = append(batteries, batteryInfo{
			driver:     name,
			capacityWh: cap,
			currentW:   r.SmoothedW,
			soc:        soc,
			online:     h.IsOnline(),
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

	// ---- Compute error based on mode ----
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

	// ---- Deadband ----
	if math.Abs(errW) < state.GridToleranceW {
		return nil
	}

	// ---- Outer PI — drives total correction we want across all batteries ----
	// Site convention: gridW positive = too much import → we want to discharge
	// batteries (site-signed correction should be negative).
	// PI setpoint = GridTargetW, measurement = gridW. error = setpoint - measurement.
	// When gridW > target, error < 0, PI output < 0 → total_correction < 0 → bat targets shift
	// toward more discharge (more negative). That's exactly what we want.
	//
	// For PeakShaving we feed a slightly different measurement so the same PI works.
	var piMeasurement float64
	if effectiveMode == ModePeakShaving {
		piMeasurement = state.GridTargetW + errW // puts the bias into the setpoint-error
	} else {
		piMeasurement = gridW
	}
	out := state.PI.Update(piMeasurement)
	totalCorrection := out.Output

	// ---- Distribute across batteries ----
	var raw []DispatchTarget
	switch effectiveMode {
	case ModeSelfConsumption, ModePeakShaving:
		raw = distributeProportional(onlineBats, totalCorrection)
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

	// ---- Fuse guard ----
	raw = applyFuseGuard(raw, store, fuseMaxW)

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
func distributeProportional(bats []batteryInfo, totalCorrection float64) []DispatchTarget {
	var totalCap float64
	for _, b := range bats { totalCap += b.capacityWh }
	if totalCap <= 0 { return nil }
	var currentTotal float64
	for _, b := range bats { currentTotal += b.currentW }
	desiredTotal := currentTotal + totalCorrection

	out := make([]DispatchTarget, 0, len(bats))
	for _, b := range bats {
		target := desiredTotal * (b.capacityWh / totalCap)
		clamped, was := clampWithSoC(target, b.soc)
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
			clamped, was := clampWithSoC(t, b.soc)
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
		clamped, was := clampWithSoC(t, b.soc)
		out = append(out, DispatchTarget{Driver: b.driver, TargetW: clamped, Clamped: was})
	}
	return out
}

// chargeAll forces all online batteries to max charge (+5 kW each, site convention).
func chargeAll(store *telemetry.Store, capacities map[string]float64) []DispatchTarget {
	out := make([]DispatchTarget, 0)
	for name := range capacities {
		h := store.DriverHealth(name)
		if h == nil || !h.IsOnline() {
			continue
		}
		// Site convention: + = charge
		out = append(out, DispatchTarget{Driver: name, TargetW: 5000})
	}
	return out
}

// clampWithSoC applies the hard safety clamps:
//   - don't discharge below SoC 5% (site: don't make target < 0 when SoC < 0.05)
//   - cap absolute power at 5000W per command
//
// BMS handles fine-grained SoC management; we just prevent obviously-dumb values.
func clampWithSoC(target, soc float64) (float64, bool) {
	clamped := target
	wasClamped := false
	// Block discharge (negative target) when battery is empty
	if soc < 0.05 && target < 0 {
		clamped = 0
		wasClamped = true
	}
	// Per-command cap
	const maxPower = 5000
	if math.Abs(clamped) > maxPower {
		sign := 1.0
		if clamped < 0 {
			sign = -1.0
		}
		clamped = sign * maxPower
		wasClamped = true
	}
	return clamped, wasClamped
}

// applyFuseGuard caps total site current. In site convention:
//   - Battery discharge is − W (source, contributes current to the house bus)
//   - PV is − W (source)
// If |total discharge| + |PV| > fuse limit, scale all discharge targets
// proportionally to bring the total under budget.
func applyFuseGuard(targets []DispatchTarget, store *telemetry.Store, fuseMaxW float64) []DispatchTarget {
	var totalPVW float64
	for _, r := range store.ReadingsByType(telemetry.DerPV) {
		totalPVW += math.Abs(r.SmoothedW)
	}
	var totalDischargeW float64
	for _, t := range targets {
		if t.TargetW < 0 {
			totalDischargeW += -t.TargetW
		}
	}
	totalGeneration := totalPVW + totalDischargeW
	if totalGeneration <= fuseMaxW {
		return targets
	}
	scale := fuseMaxW / totalGeneration
	out := make([]DispatchTarget, len(targets))
	copy(out, targets)
	for i := range out {
		if out[i].TargetW < 0 {
			out[i].TargetW *= scale
			out[i].Clamped = true
		}
	}
	return out
}
