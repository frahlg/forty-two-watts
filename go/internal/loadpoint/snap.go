package loadpoint

import "math"

// SnapChargeW turns an ideal charging-power request into the nearest
// feasible level the charger can actually deliver. Used by the
// loadpoint Controller to convert a smooth MPC-derived power target
// into one of the discrete levels a charger supports (e.g. Easee's 0
// plus 6-32 A bands).
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

// phaseFor returns the phase count chosen for wantW given the mode
// and split threshold (W). "auto" below split → 1Φ, above → 3Φ.
// Unknown modes fall back to 3Φ for safety (the pre-switching
// default). splitW must be > 0; the caller is responsible for
// deriving it from site voltage × per-phase amperage rather than
// hard-coding 230 V here. See Controller.effectiveSplit.
func phaseFor(mode string, wantW, splitW float64) int {
	switch mode {
	case "1p":
		return 1
	case "auto":
		if splitW > 0 && wantW < splitW {
			return 1
		}
		return 3
	default: // "", "3p"
		return 3
	}
}

// filterStepsByPhase narrows AllowedStepsW to only the entries that
// match the chosen phase count. The classifier is purely magnitude-
// based: step ≤ splitW → 1Φ, step > splitW → 3Φ. 0 (off) is always
// included. Returns nil when the input is nil so downstream
// SnapChargeW falls through to its continuous-passthrough behaviour.
// splitW must be > 0; caller is responsible for deriving it from
// the actual site voltage (see Controller.effectiveSplit).
func filterStepsByPhase(steps []float64, phases int, splitW float64) []float64 {
	if len(steps) == 0 {
		return nil
	}
	if splitW <= 0 {
		// Defensive: fall back to the historical 16 A × 230 V default
		// rather than treating "every step" as both 1Φ and 3Φ. Caller
		// should have computed this from site voltage already.
		splitW = 3680
	}
	out := make([]float64, 0, len(steps))
	out = append(out, 0)
	for _, s := range steps {
		if s <= 0 {
			continue
		}
		if phases == 1 && s <= splitW {
			out = append(out, s)
		} else if phases == 3 && s > splitW {
			out = append(out, s)
		}
	}
	return out
}

// EnergyBudgetToPowerW translates a remaining-Wh budget over a
// remaining-seconds window into instantaneous W. Mirrors the battery
// energy-allocation dispatch path (see docs/plan-ems-contract.md)
// so EV and battery share one mental model.
//
// Negative remaining energy (already overshot the plan) → 0 so we
// stop drawing. Non-positive remaining time → 0; the next tick will
// see a fresh slot anyway.
func EnergyBudgetToPowerW(remainingWh, remainingS float64) float64 {
	if remainingWh <= 0 {
		return 0
	}
	if remainingS <= 0 {
		return 0
	}
	return remainingWh * 3600.0 / remainingS
}
