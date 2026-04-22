package loadpoint

import "time"

// DispatchState holds the transient per-loadpoint state the main.go
// dispatch loop uses for forward-only hysteresis, starvation
// detection, and op-mode transition inference. Pure logic — no I/O,
// no clock singletons, no mutex (owner is the single dispatch
// goroutine).
//
// Zero-value is valid: all timers off.
type DispatchState struct {
	// surplusBelowMinSince is the start of the current below-min run
	// (zero time when surplus is comfortable or when a deep dip
	// already committed us to 0).
	surplusBelowMinSince time.Time

	// starvedSince is the start of the current cmdW==0 run, or zero.
	starvedSince time.Time
	// starvedFired latches so StarvationTick returns true exactly
	// once per run.
	starvedFired bool

	// Transient observations from the previous tick, written by Record.
	lastOpMode  int
	lastPlugged bool
	lastCmdW    float64

	// Current phase count the charger is believed to be in (1 or 3).
	// Zero means "unknown — don't switch until observed".
	phaseCount int
	// When we last switched phase mode, in unix millis. Cooldown
	// prevents flap around the 3φ/1φ boundary.
	lastPhaseSwitchMs int64
}

// SurplusDecision implements the forward-only hysteresis from the
// design's dispatch step 3:
//
//   - surplus >= min:              pass through, reset timer.
//   - surplus < min, gap <= cap:   grace window — hold at min until
//     SurplusHysteresisS elapses, then
//     commit to 0.
//   - surplus < min, gap >  cap:   commit to 0 immediately (deep dip).
//
// Returns (wantW, committed). When committed is true, the dispatch
// loop should send cmdW=0; the caller should still floor-snap wantW
// through FloorSnapChargeW before commanding the charger when
// committed is false.
func (s *DispatchState) SurplusDecision(surplusW, minW float64, cfg Settings, now time.Time) (float64, bool) {
	if surplusW >= minW {
		s.surplusBelowMinSince = time.Time{}
		return surplusW, false
	}
	gap := minW - surplusW
	if gap > cfg.SurplusHysteresisW {
		// Deep dip — no grace.
		s.surplusBelowMinSince = time.Time{}
		return 0, true
	}
	// Shallow dip — start or continue grace.
	if s.surplusBelowMinSince.IsZero() {
		s.surplusBelowMinSince = now
	}
	if int(now.Sub(s.surplusBelowMinSince)/time.Second) >= cfg.SurplusHysteresisS {
		return 0, true
	}
	return minW, false
}

// StarvationTick advances the starvation timer. Returns true on the
// tick where cmdW has been 0 continuously for SurplusStarvationS —
// exactly once per run. The caller publishes the event on true.
func (s *DispatchState) StarvationTick(cmdW float64, now time.Time, cfg Settings) bool {
	if cmdW > 0 {
		s.starvedSince = time.Time{}
		s.starvedFired = false
		return false
	}
	if s.starvedSince.IsZero() {
		s.starvedSince = now
		s.starvedFired = false
		return false
	}
	if s.starvedFired {
		return false
	}
	if int(now.Sub(s.starvedSince)/time.Second) >= cfg.SurplusStarvationS {
		s.starvedFired = true
		return true
	}
	return false
}

// ObserveOpMode returns true when the transition is an auto-clear
// trigger: charging (3) → completed (4). 3→2 with our lastCmdW==0
// is OUR pause (never clears). 3→2 with lastCmdW>0 is a user pause
// (target stays — user may resume; spec says don't clear).
//
// lastCmdW is accepted for future use by surplus-pause detection but
// doesn't affect the 3→4 decision.
func (s *DispatchState) ObserveOpMode(prev, cur int, lastCmdW float64) bool {
	_ = lastCmdW // reserved for a future surplus-pause heuristic
	return prev == 3 && cur == 4
}

// ObserveUnplug returns true on the plugged-in true→false edge.
func (s *DispatchState) ObserveUnplug(prev, cur bool) bool {
	return prev && !cur
}

// Record stores the current tick's observations so the next tick can
// compare for edge detection. Call exactly once per tick, AFTER
// SurplusDecision + ObserveOpMode + StarvationTick.
func (s *DispatchState) Record(opMode int, plugged bool, cmdW float64) {
	s.lastOpMode = opMode
	s.lastPlugged = plugged
	s.lastCmdW = cmdW
}

// LastOpMode returns the op-mode recorded on the previous tick.
func (s *DispatchState) LastOpMode() int { return s.lastOpMode }

// LastCmdW returns the cmdW we sent on the previous tick.
func (s *DispatchState) LastCmdW() float64 { return s.lastCmdW }

// LastPlugged returns the plugged-in flag from the previous tick.
func (s *DispatchState) LastPlugged() bool { return s.lastPlugged }

// PhaseCount returns the last-observed phase count. Zero if unknown.
func (s *DispatchState) PhaseCount() int { return s.phaseCount }

// ObservePhases records the phase count from the driver's telemetry.
// Call this on every tick with the JSON-decoded phases field. Values
// other than 1 or 3 are ignored (2φ is not a dispatch mode we use).
func (s *DispatchState) ObservePhases(n int) {
	if n == 1 || n == 3 {
		s.phaseCount = n
	}
}

// ShouldSwitchPhase decides if we should request a phase change this
// tick. Returns (targetPhases, true) when a switch is recommended,
// (0, false) otherwise. Applies a cooldown between switches to avoid
// flapping around the 3φ/1φ boundary.
//
// Side effect: records the switch time when it returns true, so
// repeated calls within the cooldown window are no-ops.
func (s *DispatchState) ShouldSwitchPhase(desired int, nowMs int64, cooldownMs int64) (int, bool) {
	if desired != 1 && desired != 3 {
		return 0, false
	}
	if s.phaseCount == desired {
		return 0, false
	}
	if s.lastPhaseSwitchMs > 0 && nowMs-s.lastPhaseSwitchMs < cooldownMs {
		return 0, false
	}
	s.lastPhaseSwitchMs = nowMs
	return desired, true
}
