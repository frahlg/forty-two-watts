package mpc

import (
	"errors"
	"fmt"
	"math"
)

// Costing a foreign plan is not the same thing as reading the number that
// plan came with. A solver reports the value of ITS objective — scenario
// weights, a CVaR tail term, its own penalties and bonuses — and subtracting
// that from Core's cost measures the two objectives rather than the two
// plans. evaluatePlan closes that gap: it walks somebody else's action
// sequence through Core's own forward pass and reports what Core says it
// costs, so both sides of a comparison are priced by one piece of code.
//
// Refusal is a first-class result. A plan that cannot be costed honestly —
// wrong length, non-finite output, state outside the operating band Core
// enforces — yields no number at all, because a logged refusal is worth more
// than a plausible fabrication nobody can tell apart from a measurement.

// planEvaluation is Core's valuation of a plan's actions.
type planEvaluation struct {
	// CostOre is the raw grid cost over the horizon — the same quantity
	// Plan.TotalCostOre carries for a Core plan. Terminal-correct it before
	// comparing two plans that park the horizon at different SoC.
	CostOre float64
	// EndSoC is the SoC the walk reached, not the SoC the plan claimed.
	// Trusting a foreign plan's own SoC column would put the terminal
	// credit back on the challenger's honour system.
	EndSoC float64
	// CurtailedSlots counts slots whose action asks to cap PV. Core's cost
	// model prices every slot at full forecast generation, for its own plans
	// and foreign ones alike, so this is not a mispricing — it is the count
	// of slots where the two plans could still differ in a way the model
	// does not resolve.
	CurtailedSlots int
}

var errPlanEvaluationCapacity = errors.New("capacity_wh is not positive")

// evaluatePlanOre reports what a plan's actions cost under Core's cost
// model, so a foreign plan can be judged on the same terms as Core's own.
// ok is false when the plan cannot be costed; evaluatePlan carries the
// reason for callers that log it.
func evaluatePlanOre(plan Plan, slots []Slot, p Params) (ore float64, ok bool) {
	eval, err := evaluatePlan(plan, slots, p)
	if err != nil {
		return 0, false
	}
	return eval.CostOre, true
}

// evaluatePlan walks plan.Actions across slots under Core's cost model and
// returns Core's valuation, or the reason it refuses to produce one.
//
// Storage is walked in aggregate — one SoC against p.CapacityWh and the
// aggregate efficiencies — because that is the state the champion plans in
// and the state its own reported cost describes. Cost never reads SoC, so a
// fleet of batteries with unequal efficiencies can only move the terminal
// correction and the band check, never the öre per slot. ValidatePlan
// already replays a foreign plan per storage on the way in.
func evaluatePlan(plan Plan, slots []Slot, p Params) (planEvaluation, error) {
	// Optimize sanitizes and defaults before it costs anything. Judging a
	// plan under other inputs than the ones Core would have used is not
	// judging it under Core's model.
	slots = sanitizeOptimizeSlots(slots)
	if len(slots) == 0 {
		return planEvaluation{}, errors.New("no slots to evaluate against")
	}
	if p.CapacityWh <= 0 {
		return planEvaluation{}, errPlanEvaluationCapacity
	}
	if len(plan.Actions) != len(slots) {
		return planEvaluation{}, fmt.Errorf("action count %d, want %d",
			len(plan.Actions), len(slots))
	}
	if p.ChargeEfficiency <= 0 {
		p.ChargeEfficiency = 0.95
	}
	if p.DischargeEfficiency <= 0 {
		p.DischargeEfficiency = 0.95
	}

	bandActive := p.SoCMax > p.SoCMin
	bandTol := socBandTolerance(p)
	soc := p.InitialSoC
	if bandActive {
		// Same clamp the DP's forward pass opens with: the site may report
		// a SoC outside the band, and Core plans from the band edge.
		soc = math.Max(p.SoCMin, math.Min(p.SoCMax, soc))
	}

	loadpoints := p.activeLoadpoints()
	lpSoC := make(map[string]float64, len(loadpoints))
	for _, lp := range loadpoints {
		lpSoC[lp.ID] = lp.InitialSoC
	}

	eval := planEvaluation{EndSoC: soc}
	for i, slot := range slots {
		a := plan.Actions[i]
		if a.SlotStartMs != slot.StartMs || a.SlotLenMin != slot.LenMin {
			return planEvaluation{}, fmt.Errorf("slot %d is not the slot the action was planned for", i)
		}
		loadpointW := 0.0
		for idx := range loadpoints {
			powerW := evaluationLoadpointStepW(a, loadpoints, idx)
			if !finite(powerW) {
				return planEvaluation{}, fmt.Errorf("slot %d loadpoint %s has non-finite power",
					i, loadpoints[idx].ID)
			}
			loadpointW += powerW
		}
		if !finite(a.BatteryW) || !finite(a.PVLimitW) {
			return planEvaluation{}, fmt.Errorf("slot %d contains non-finite output", i)
		}
		if a.BatteryW > p.MaxChargeW+planEvaluationPowerTolW ||
			a.BatteryW < -p.MaxDischargeW-planEvaluationPowerTolW {
			return planEvaluation{}, fmt.Errorf(
				"slot %d battery_w %.1f outside charge/discharge limits %.1f…%.1f",
				i, a.BatteryW, -p.MaxDischargeW, p.MaxChargeW)
		}

		step := stepPlanSlot(slot, p, soc, a.BatteryW, loadpointW)
		if !finite(step.SoC) || !finite(step.CostOre) {
			return planEvaluation{}, fmt.Errorf("slot %d costs to a non-finite value", i)
		}
		if bandActive {
			if step.SoC < p.SoCMin-bandTol || step.SoC > p.SoCMax+bandTol {
				return planEvaluation{}, fmt.Errorf(
					"slot %d drives soc to %.4f, outside %.4f…%.4f", i, step.SoC, p.SoCMin, p.SoCMax)
			}
			// The DP pins its own walk to the band; matching it keeps a Core
			// plan's evaluation identical to the cost the DP reported.
			soc = math.Max(p.SoCMin, math.Min(p.SoCMax, step.SoC))
		} else {
			soc = step.SoC
		}
		for idx, lp := range loadpoints {
			ceiling := lp.SoCMax
			if ceiling <= lp.SoCMin {
				ceiling = 1.0 // same normalization the DP applies
			}
			eff := lp.ChargeEfficiency
			if eff <= 0 {
				eff = 0.9
			}
			next := stepLoadpointSoC(lpSoC[lp.ID], evaluationLoadpointStepW(a, loadpoints, idx),
				float64(slot.LenMin)/60.0, lp.CapacityWh, eff)
			if !finite(next) {
				return planEvaluation{}, fmt.Errorf("slot %d loadpoint %s reaches a non-finite soc", i, lp.ID)
			}
			if next > ceiling+planEvaluationLoadpointSoCTol {
				return planEvaluation{}, fmt.Errorf(
					"slot %d drives loadpoint %s to soc %.4f, above %.4f", i, lp.ID, next, ceiling)
			}
			// The DP pins the EV walk to its ceiling too.
			lpSoC[lp.ID] = math.Min(next, ceiling)
		}

		eval.CostOre += step.CostOre
		if a.PVLimitW > 0 {
			eval.CurtailedSlots++
		}
	}
	eval.EndSoC = soc
	if !finite(eval.CostOre) {
		return planEvaluation{}, errors.New("horizon cost is not finite")
	}
	return eval, nil
}

const (
	// planEvaluationPowerTolW matches the slack ValidatePlan already grants a
	// continuous solver riding a bound: float residue lands a plan a
	// fraction of a watt over its own limit, and rejecting the measurement
	// for that would only hide it.
	planEvaluationPowerTolW = 2.0
	// planEvaluationLoadpointSoCTol mirrors ValidatePlan's EV replay
	// tolerance.
	planEvaluationLoadpointSoCTol = 0.0005
)

// socBandTolerance is how far past soc_min…soc_max one slot may land before
// the walk refuses. The DP looks its policy up on a discretized SoC grid
// while propagating a continuous SoC, so a legitimate Core plan can overshoot
// by up to a grid step; a plan that leaves the band by more than the DP's own
// resolution allows is asking for state the band does not permit.
func socBandTolerance(p Params) float64 {
	levels := p.SoCLevels
	if levels < 3 {
		levels = 3
	}
	step := (p.SoCMax - p.SoCMin) / float64(levels-1)
	return math.Max(0.001, step)
}

// evaluationLoadpointStepW reads one action's power for one active flex
// load, using the same per-ID map with a first-loadpoint scalar fallback
// ValidatePlan reads — the DP writes only the scalar, the external optimizer
// writes both.
func evaluationLoadpointStepW(a Action, loadpoints []*LoadpointSpec, idx int) float64 {
	if len(a.LoadpointPowerW) == 0 {
		if idx == 0 {
			return a.LoadpointW
		}
		return 0
	}
	return a.LoadpointPowerW[loadpoints[idx].ID]
}
