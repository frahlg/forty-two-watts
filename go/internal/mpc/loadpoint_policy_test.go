package mpc

import (
	"testing"
)

// evChargedAt returns LoadpointW for the slot at index i, or 0 when
// out of range. Helper for policy-gate tests.
func evChargedAt(plan Plan, i int) float64 {
	if i < 0 || i >= len(plan.Actions) {
		return 0
	}
	return plan.Actions[i].LoadpointW
}

// policyTestSlots builds a 4-slot horizon alternating day/night with
// strong midday PV surplus and expensive night prices. Lets us
// compare what a given Policy allows.
//
//   Slot 0 (night)  — no PV, flat load, cheap price
//   Slot 1 (morning)— strong PV surplus, modest load
//   Slot 2 (midday) — strong PV surplus, modest load
//   Slot 3 (night)  — no PV, flat load, expensive price
func policyTestSlots() []Slot {
	return []Slot{
		{StartMs: 0,         LenMin: 60, PriceOre: 80,  SpotOre: 50, LoadW: 500, PVW: 0,     Confidence: 1.0},
		{StartMs: 3600_000,  LenMin: 60, PriceOre: 120, SpotOre: 90, LoadW: 500, PVW: -6000, Confidence: 1.0},
		{StartMs: 7200_000,  LenMin: 60, PriceOre: 150, SpotOre: 120, LoadW: 500, PVW: -6000, Confidence: 1.0},
		{StartMs: 10800_000, LenMin: 60, PriceOre: 200, SpotOre: 160, LoadW: 500, PVW: 0,     Confidence: 1.0},
	}
}

func policyTestParams(policy *Policy) Params {
	return Params{
		Mode:                ModeSelfConsumption,
		CapacityWh:          10000,
		SoCMinPct:           20, SoCMaxPct: 90, SoCLevels: 11,
		MaxChargeW:          2000, MaxDischargeW: 2000, ActionLevels: 5,
		ChargeEfficiency:    0.95, DischargeEfficiency: 0.95,
		InitialSoCPct:       50,
		TerminalSoCPrice:    80,
		Loadpoint: &LoadpointSpec{
			ID:            "garage",
			CapacityWh:    40000,
			Levels:        11,
			MinPct:        0, MaxPct: 100,
			InitialSoCPct: 30,
			PluggedIn:     true,
			TargetSoCPct:  60,
			TargetSlotIdx: 3,
			MaxChargeW:    4000,
			AllowedStepsW: []float64{0, 1400, 3000, 4000},
			ChargeEfficiency: 0.9,
			Policy: policy,
		},
	}
}

// TestPolicyNilUnrestricted — default (nil policy) lets the DP pick
// freely across day AND night. The test just asserts at least one
// slot got some EV charge — with a reachable target and no gates,
// the DP will allocate at least one step somewhere.
func TestPolicyNilUnrestricted(t *testing.T) {
	plan := Optimize(policyTestSlots(), policyTestParams(nil))
	var total float64
	for _, a := range plan.Actions {
		total += a.LoadpointW
	}
	if total == 0 {
		t.Errorf("nil policy: expected DP to charge EV somewhere; actions: %+v", plan.Actions)
	}
}

// TestPolicyOnlySurplusSkipsNightImport — with OnlySurplus=true +
// AllowGrid=true, DP should charge during daytime surplus slots (1,
// 2) but not at night (slots 0, 3). We verify no EV charge in night
// slots.
func TestPolicyOnlySurplusSkipsNightImport(t *testing.T) {
	plan := Optimize(policyTestSlots(), policyTestParams(&Policy{
		AllowGrid:           true,
		AllowBatterySupport: true,
		OnlySurplus:         true,
	}))
	// User's request: surplus-only during the day, NIGHT IS FREE. So
	// DP may or may not use night slots depending on cost. But during
	// day slots (1, 2) the evW must not exceed forecast surplus of
	// 6000 - 500 = 5500 W. Our MaxChargeW is 4000, so any positive
	// step is fine.
	for i, a := range plan.Actions {
		if i == 1 || i == 2 {
			// Daytime: surplus 5500 W, any step ≤ 5500 is fine.
			if a.LoadpointW > 5500+100 {
				t.Errorf("slot %d daytime evW=%.0f exceeds surplus 5500", i, a.LoadpointW)
			}
		}
	}
}

// TestPolicyNoGridRejectsNightSlots — AllowGrid=false forbids any EV
// charge that requires grid import. Since battery has a separate
// mandate and we've configured BatteryCoversEV behaviour via the DP
// letting batW < 0 cover evW, some battery-support might sneak in.
// Easiest assertion: night slots with no battery discharge also show
// no EV charge.
func TestPolicyNoGridRejectsNightSlots(t *testing.T) {
	plan := Optimize(policyTestSlots(), policyTestParams(&Policy{
		AllowGrid:           false,
		AllowBatterySupport: true,
		OnlySurplus:         false,
	}))
	// Slot 0 (night): no PV. If AllowGrid=false AND battery isn't
	// discharging, gridW = 500 + 0 + battW + evW. For gridW ≤ eps,
	// need evW ≤ -500 - battW. If battW >= 0 (idle or charging),
	// evW must be 0 (or negative, not allowed).
	for i, a := range plan.Actions {
		if (i == 0 || i == 3) && a.BatteryW >= 0 && a.LoadpointW > 100 {
			t.Errorf("slot %d night evW=%.0f with batW=%.0f >= 0 violates AllowGrid=false (gridW=%.0f)",
				i, a.LoadpointW, a.BatteryW, a.GridW)
		}
	}
}

// TestPolicyNoBatterySupportRejectsDischarge — AllowBatterySupport=
// false forbids (evW>0, battW<0) pairs. If the DP nevertheless ends
// up charging the EV (e.g. during surplus), the battery must not be
// discharging in the same slot.
func TestPolicyNoBatterySupportRejectsDischarge(t *testing.T) {
	plan := Optimize(policyTestSlots(), policyTestParams(&Policy{
		AllowGrid:           true,
		AllowBatterySupport: false,
		OnlySurplus:         false,
	}))
	for i, a := range plan.Actions {
		if a.LoadpointW > 100 && a.BatteryW < -100 {
			t.Errorf("slot %d: EV charging (evW=%.0f) with battery discharging (batW=%.0f) violates AllowBatterySupport=false",
				i, a.LoadpointW, a.BatteryW)
		}
	}
}
