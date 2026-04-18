package control

import "math"

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
