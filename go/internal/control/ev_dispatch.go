package control

import (
	"math"
	"time"
)

// SnapChargeW turns an ideal charging-power request into the nearest
// feasible level the charger can actually deliver. Used by EV
// dispatch to convert a smooth MPC-derived power target into one of
// the discrete levels a charger supports (e.g. Easee's 0 plus 6-32 A
// bands).
//
// Rules:
//
//   - Clamp to [min, max] first. A zero `want` short-circuits to 0
//     (off) without falling through to a 6 A minimum.
//   - If `steps` is empty, return the clamped value. Callers without
//     discrete levels get a continuous power number.
//   - Otherwise pick the step with the smallest absolute difference
//     from the clamped target.
//
// We deliberately snap to NEAREST rather than floor — a 4.1 kW want
// on a {0, 4.1, 7.4, 11} step set should hit 4.1 exactly even when
// floating-point math puts it at 4099 W.
func SnapChargeW(want, min, max float64, steps []float64) float64 {
	if want <= 0 {
		return 0
	}
	if want < min {
		want = min
	}
	if max > 0 && want > max {
		want = max
	}
	if len(steps) == 0 {
		return want
	}
	best := steps[0]
	bestDiff := math.Abs(want - best)
	for _, s := range steps[1:] {
		if d := math.Abs(want - s); d < bestDiff {
			best = s
			bestDiff = d
		}
	}
	return best
}

// EnergyBudgetToPowerW translates a remaining-Wh budget over a
// remaining-seconds window into instantaneous W. Mirrors the battery
// energy-allocation dispatch path (see docs/plan-ems-contract.md)
// so EV and battery share one mental model.
//
// Negative remaining energy (already overshot the plan) → 0 so we
// stop drawing. Non-positive remaining time → return `want` as if
// the slot were just beginning; the next dispatch tick will see a
// fresh slot anyway.
func EnergyBudgetToPowerW(remainingWh, remainingS float64) float64 {
	if remainingWh <= 0 {
		return 0
	}
	if remainingS <= 0 {
		return 0
	}
	return remainingWh * 3600.0 / remainingS
}

// EVDispatchState carries per-loadpoint dispatch bookkeeping across ticks.
// One instance per loadpoint, owned by the dispatch loop in main.go.
//
// Replaces the inline alreadyWh = powerW × elapsed approximation that
// over-counted recently-changed power and produced ~30 s on/off
// oscillations matching the Easee cloud reporting lag. State is in-memory
// only — a process restart resets the accumulator, which is consistent
// with the battery dispatch's slotDelivered (see dispatch.go:140-142).
type EVDispatchState struct {
	slotStart   time.Time
	deliveredWh float64
	lastTickAt  time.Time

	lastSentAt time.Time
	lastCmdW   float64
}

// Tick advances one dispatch cycle for one loadpoint and returns the
// command (cmdW) plus whether to actually send it (send) this tick.
//
//   - Slot rollover (slotStart changed) resets the accumulator.
//   - On-tick: integrates `nowPowerW × dt` into deliveredWh so the
//     formula sees a real running total, not the inline approximation.
//   - wantW is clamped to maxW before SnapChargeW so the formula's
//     end-of-slot blow-up is bounded by the charger's electrical max.
//   - Holdoff suppresses CHANGE-spam: when the formula wants a different
//     value but the previous send was less than `holdoff` ago, the prior
//     command is repeated instead. This is what kills the 6→11→0→6 flap
//     caused by the Easee cloud's ~25 s reporting lag — the dispatch
//     loop can run every 5 s without commanding faster than the
//     telemetry can confirm.
//   - Heartbeat keeps the charger awake: if cmdW hasn't changed in
//     `heartbeat` seconds, re-send anyway so a watchdog reset doesn't
//     silently zero the setpoint.
//
// First-tick behavior: send=true so the charger receives an initial
// command immediately (no implicit "wait the holdoff" delay at startup).
func (s *EVDispatchState) Tick(
	now, slotStart, slotEnd time.Time,
	budgetWh, nowPowerW float64,
	minW, maxW float64,
	stepsW []float64,
	holdoff, heartbeat time.Duration,
) (cmdW float64, send bool) {
	// Slot rollover bookkeeping.
	if !s.slotStart.Equal(slotStart) {
		s.slotStart = slotStart
		s.deliveredWh = 0
		s.lastTickAt = now
	} else {
		if !s.lastTickAt.IsZero() {
			dt := now.Sub(s.lastTickAt).Seconds()
			// Cap dt at 5 min so a long pause (process suspend, clock
			// jump) doesn't poison the accumulator.
			if dt > 0 && dt < 300 {
				s.deliveredWh += nowPowerW * dt / 3600.0
			}
		}
		s.lastTickAt = now
	}

	// Compute the desired command.
	remainingS := slotEnd.Sub(now).Seconds()
	remainingWh := budgetWh - s.deliveredWh
	wantW := EnergyBudgetToPowerW(remainingWh, remainingS)
	if maxW > 0 && wantW > maxW {
		wantW = maxW
	}
	desired := SnapChargeW(wantW, minW, maxW, stepsW)

	// Holdoff (suppress changes) + heartbeat (re-send unchanged).
	initial := s.lastSentAt.IsZero()
	sinceSent := now.Sub(s.lastSentAt)
	if !initial && desired != s.lastCmdW && sinceSent < holdoff {
		// Want to change but holdoff hasn't elapsed — keep prior value.
		return s.lastCmdW, false
	}
	if !initial && desired == s.lastCmdW && sinceSent < heartbeat {
		// No change and not yet time to heartbeat — skip this tick.
		return s.lastCmdW, false
	}

	s.lastSentAt = now
	s.lastCmdW = desired
	return desired, true
}
