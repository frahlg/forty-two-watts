package mpc

import (
	"math"
	"strings"
	"testing"
)

// evaluationSlots builds a cheap-then-expensive horizon: enough spread that
// every mode below actually moves the battery.
func evaluationSlots(n int) []Slot {
	slots := make([]Slot, n)
	for i := range slots {
		price := 80.0
		pv := 0.0
		if i >= n/2 {
			price = 320.0
		}
		if i >= n/4 && i < n/2 {
			pv = -2500 // midday surplus
		}
		slots[i] = Slot{
			StartMs:    1_700_000_000_000 + int64(i)*900_000,
			LenMin:     15,
			PriceOre:   price,
			SpotOre:    price / 2,
			Confidence: 1,
			PVW:        pv,
			LoadW:      900,
		}
	}
	return slots
}

func evaluationParams(mode Mode) Params {
	return Params{
		Mode: mode, CapacityWh: 9600, InitialSoC: 0.5,
		SoCMin: 0.1, SoCMax: 1.0, SoCLevels: 61, ActionLevels: 41,
		MaxChargeW: 4000, MaxDischargeW: 4000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
		TerminalSoCPrice: 150,
	}
}

// TestEvaluatePlanCostsTheDPsOwnPlanIdentically is the sharing proof. The DP
// reports a total; the evaluator re-derives it from the same actions. They
// agree only while both drive the one piece of cost arithmetic — fork it and
// this test is what fails, before a measurement quietly stops meaning
// anything.
func TestEvaluatePlanCostsTheDPsOwnPlanIdentically(t *testing.T) {
	cases := map[string]func() (Params, []Slot){
		"arbitrage": func() (Params, []Slot) {
			return evaluationParams(ModeArbitrage), evaluationSlots(24)
		},
		"passive arbitrage": func() (Params, []Slot) {
			return evaluationParams(ModePassiveArbitrage), evaluationSlots(24)
		},
		"self consumption": func() (Params, []Slot) {
			return evaluationParams(ModeSelfConsumption), evaluationSlots(24)
		},
		"arbitrage with a minimum spread": func() (Params, []Slot) {
			p := evaluationParams(ModeArbitrage)
			p.MinArbitrageSpreadOreKwh = 30
			return p, evaluationSlots(24)
		},
		"arbitrage starting at the floor": func() (Params, []Slot) {
			p := evaluationParams(ModeArbitrage)
			p.InitialSoC = p.SoCMin
			return p, evaluationSlots(24)
		},
		"arbitrage starting below the floor": func() (Params, []Slot) {
			// The DP plans from the clamped band edge; so must the
			// evaluator, or every recovery replan would be unscoreable.
			p := evaluationParams(ModeArbitrage)
			p.InitialSoC = 0.05
			return p, evaluationSlots(24)
		},
		"with an active loadpoint": func() (Params, []Slot) {
			p := evaluationParams(ModeArbitrage)
			p.Loadpoint = &LoadpointSpec{
				ID: "ev", CapacityWh: 60000, InitialSoC: 0.3, SoCMin: 0,
				SoCMax: 0.9, TargetSoC: 0.8, TargetSlotIdx: 20, Levels: 11,
				MaxChargeW: 11000, ChargeEfficiency: 0.9,
			}
			return p, evaluationSlots(24)
		},
		"loadpoint on surplus only": func() (Params, []Slot) {
			p := evaluationParams(ModePassiveArbitrage)
			p.Loadpoint = &LoadpointSpec{
				ID: "ev", CapacityWh: 60000, InitialSoC: 0.3, SoCMin: 0,
				SoCMax: 0.9, TargetSoC: 0.8, TargetSlotIdx: 20, Levels: 11,
				MaxChargeW: 11000, ChargeEfficiency: 0.9, SurplusOnly: true,
			}
			return p, evaluationSlots(24)
		},
		"export priced flat": func() (Params, []Slot) {
			p := evaluationParams(ModeArbitrage)
			p.ExportOrePerKWh = 60
			return p, evaluationSlots(24)
		},
		"fifteen and sixty minute slots mixed": func() (Params, []Slot) {
			slots := evaluationSlots(24)
			for i := range slots {
				if i%3 == 0 {
					slots[i].LenMin = 60
				}
			}
			return evaluationParams(ModeArbitrage), slots
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			p, slots := build()
			plan := Optimize(slots, p)
			if len(plan.Actions) == 0 {
				t.Fatal("the DP produced no plan to evaluate")
			}
			ore, ok := evaluatePlanOre(plan, slots, p)
			if !ok {
				_, err := evaluatePlan(plan, slots, p)
				t.Fatalf("core refused to cost its own plan: %v", err)
			}
			if diff := math.Abs(ore - plan.TotalCostOre); diff > 1e-9 {
				t.Fatalf("evaluated %.9f öre, the DP reported %.9f (%.9f apart)",
					ore, plan.TotalCostOre, diff)
			}
			// The end SoC drives the terminal correction, so it has to come
			// from the same walk rather than the plan's own column.
			eval, err := evaluatePlan(plan, slots, p)
			if err != nil {
				t.Fatal(err)
			}
			if diff := math.Abs(eval.EndSoC - planEndSoC(&plan)); diff > 1e-9 {
				t.Fatalf("walked end soc %.9f, plan reported %.9f", eval.EndSoC, planEndSoC(&plan))
			}
		})
	}
}

// TestEvaluatePlanRefusesRatherThanGuessing — every refusal here is a plan
// Core cannot honestly price. A number would be indistinguishable from a
// measurement; the absence of one is not.
func TestEvaluatePlanRefusesRatherThanGuessing(t *testing.T) {
	base := evaluationParams(ModeArbitrage)
	slots := evaluationSlots(12)
	good := Optimize(slots, base)

	cases := map[string]struct {
		mutate func(Plan) Plan
		params func(Params) Params
		reason string
	}{
		"one action short": {
			mutate: func(p Plan) Plan { p.Actions = p.Actions[:len(p.Actions)-1]; return p },
			reason: "action count",
		},
		"one action too many": {
			mutate: func(p Plan) Plan { p.Actions = append(p.Actions, p.Actions[0]); return p },
			reason: "action count",
		},
		"a NaN battery action": {
			mutate: func(p Plan) Plan { p.Actions[3].BatteryW = math.NaN(); return p },
			reason: "non-finite",
		},
		"an infinite battery action": {
			mutate: func(p Plan) Plan { p.Actions[3].BatteryW = math.Inf(1); return p },
			reason: "non-finite",
		},
		"discharging straight through the floor": {
			mutate: func(p Plan) Plan {
				for i := range p.Actions {
					p.Actions[i].BatteryW = -4000
				}
				return p
			},
			reason: "outside",
		},
		"charging straight through the ceiling": {
			mutate: func(p Plan) Plan {
				for i := range p.Actions {
					p.Actions[i].BatteryW = 4000
				}
				return p
			},
			reason: "outside",
		},
		"a battery action beyond the discharge limit": {
			mutate: func(p Plan) Plan { p.Actions[2].BatteryW = -9000; return p },
			reason: "charge/discharge limits",
		},
		"a battery action beyond the charge limit": {
			mutate: func(p Plan) Plan { p.Actions[2].BatteryW = 9000; return p },
			reason: "charge/discharge limits",
		},
		"actions planned for other slots": {
			mutate: func(p Plan) Plan { p.Actions[5].SlotStartMs += 1000; return p },
			reason: "not the slot",
		},
		"no battery to evaluate": {
			params: func(p Params) Params { p.CapacityWh = 0; return p },
			reason: "capacity_wh",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			plan := clonePlan(&good)
			if tc.mutate != nil {
				*plan = tc.mutate(*plan)
			}
			p := base
			if tc.params != nil {
				p = tc.params(p)
			}
			if _, ok := evaluatePlanOre(*plan, slots, p); ok {
				t.Fatal("core costed a plan it should have refused")
			}
			_, err := evaluatePlan(*plan, slots, p)
			if err == nil {
				t.Fatal("refused without a reason")
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("refusal reason %q does not mention %q", err, tc.reason)
			}
		})
	}

	// The unmutated plan still evaluates — otherwise the table above proves
	// nothing about the mutations.
	if _, ok := evaluatePlanOre(good, slots, base); !ok {
		t.Fatal("the control plan was refused too")
	}
}

// TestEvaluatePlanRanksPlans — the evaluator has to order plans, not merely
// reproduce one number. Two hand-built action sequences over the same two
// slots: buy cheap and sell dear, or the reverse.
func TestEvaluatePlanRanksPlans(t *testing.T) {
	slots := []Slot{
		{StartMs: 1_000_000, LenMin: 60, PriceOre: 100, SpotOre: 100, Confidence: 1, LoadW: 1000},
		{StartMs: 1_000_000 + 3_600_000, LenMin: 60, PriceOre: 400, SpotOre: 400, Confidence: 1, LoadW: 1000},
	}
	p := Params{
		Mode: ModeArbitrage, CapacityWh: 10000, InitialSoC: 0.5,
		SoCMin: 0.1, SoCMax: 1.0, SoCLevels: 101, ActionLevels: 41,
		MaxChargeW: 2000, MaxDischargeW: 2000,
		ChargeEfficiency: 1, DischargeEfficiency: 1,
	}
	build := func(first, second float64) Plan {
		return Plan{Actions: []Action{
			{SlotStartMs: slots[0].StartMs, SlotLenMin: 60, BatteryW: first},
			{SlotStartMs: slots[1].StartMs, SlotLenMin: 60, BatteryW: second},
		}}
	}

	// Charge 2 kW at 100 öre, discharge 2 kW at 400 öre.
	//   slot 0: grid = 1000 + 2000 = 3 kWh × 100 = 300 öre
	//   slot 1: grid = 1000 − 2000 = −1 kWh × 400 = −400 öre
	smart, ok := evaluatePlanOre(build(2000, -2000), slots, p)
	if !ok {
		t.Fatal("the sensible plan was refused")
	}
	if math.Abs(smart-(-100)) > 1e-9 {
		t.Fatalf("buy-low-sell-high costs %.6f öre, want −100", smart)
	}

	// The same energy moved the wrong way round.
	//   slot 0: grid = 1000 − 2000 = −1 kWh × 100 = −100 öre
	//   slot 1: grid = 1000 + 2000 = 3 kWh × 400 = 1200 öre
	silly, ok := evaluatePlanOre(build(-2000, 2000), slots, p)
	if !ok {
		t.Fatal("the wasteful plan was refused")
	}
	if math.Abs(silly-1100) > 1e-9 {
		t.Fatalf("sell-low-buy-high costs %.6f öre, want 1100", silly)
	}
	if silly <= smart {
		t.Fatalf("the wasteful plan (%v) did not cost more than the sensible one (%v)", silly, smart)
	}

	// Doing nothing sits between them: 2 kWh of house load, 100 + 400.
	idle, ok := evaluatePlanOre(build(0, 0), slots, p)
	if !ok {
		t.Fatal("the idle plan was refused")
	}
	if math.Abs(idle-500) > 1e-9 {
		t.Fatalf("idling costs %.6f öre, want 500", idle)
	}
	if !(smart < idle && idle < silly) {
		t.Fatalf("ranking broken: smart=%v idle=%v silly=%v", smart, idle, silly)
	}
}

// TestEvaluatePlanIgnoresTheCostAPlanClaims — the whole point. A challenger
// that reports a flattering total gets no credit for it.
func TestEvaluatePlanIgnoresTheCostAPlanClaims(t *testing.T) {
	p := evaluationParams(ModeArbitrage)
	slots := evaluationSlots(16)
	plan := Optimize(slots, p)
	honest, ok := evaluatePlanOre(plan, slots, p)
	if !ok {
		t.Fatal("refused")
	}
	plan.TotalCostOre = -999999
	for i := range plan.Actions {
		plan.Actions[i].CostOre = -1
	}
	plan.Actions[len(plan.Actions)-1].SoC = 1.0
	flattered, ok := evaluatePlanOre(plan, slots, p)
	if !ok {
		t.Fatal("refused after the totals were rewritten")
	}
	if flattered != honest {
		t.Fatalf("rewriting the reported cost moved the evaluation: %v then %v", honest, flattered)
	}
}
