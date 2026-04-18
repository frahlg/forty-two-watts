package mpc

import (
	"testing"
	"time"
)

// TestSlotDirectiveCarriesLoadpointEnergyWh asserts that when the DP
// decided an EV should charge in a slot, SlotDirectiveAt surfaces the
// planned Wh under the correct loadpoint ID. This is the contract the
// dispatch layer consumes to drive the charger.
func TestSlotDirectiveCarriesLoadpointEnergyWh(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Truncate(15 * time.Minute)
	// 4 hourly slots with cheap nighttime + expensive daytime. A
	// target of 40 % on a 20-% start forces the DP to schedule EV
	// charging across multiple slots — we don't assert WHICH ones;
	// only that at least one gets a loadpoint entry.
	slots := make([]Slot, 4)
	for i := range slots {
		slots[i] = Slot{
			StartMs:    start.Add(time.Duration(i) * time.Hour).UnixMilli(),
			LenMin:     60,
			PriceOre:   40,
			SpotOre:    20,
			LoadW:      400,
			Confidence: 1.0,
		}
	}
	p := Params{
		Mode:                ModeCheapCharge,
		SoCLevels:           11,
		CapacityWh:          5000,
		SoCMinPct:           10,
		SoCMaxPct:           95,
		InitialSoCPct:       50,
		ActionLevels:        5,
		MaxChargeW:          2000,
		MaxDischargeW:       2000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    40,
		Loadpoint: &LoadpointSpec{
			ID:               "garage",
			CapacityWh:       60000,
			Levels:           11,
			InitialSoCPct:    20,
			PluggedIn:        true,
			TargetSoCPct:     40,
			TargetSlotIdx:    3,
			MaxChargeW:       11000,
			AllowedStepsW:    []float64{0, 11000},
			ChargeEfficiency: 0.9,
		},
	}
	plan := Optimize(slots, p)

	// Find a slot where DP scheduled charging — assert the Service
	// routes its Wh under the loadpoint ID.
	var chargedSlotIdx int = -1
	for i, a := range plan.Actions {
		if a.LoadpointW > 0 {
			chargedSlotIdx = i
			break
		}
	}
	if chargedSlotIdx < 0 {
		t.Fatalf("DP never scheduled EV charging; actions: %+v", plan.Actions)
	}

	svc := &Service{
		Zone:            "SE3",
		Defaults:        Params{Mode: ModeCheapCharge},
		last:            &plan,
		lastLoadpointID: "garage",
	}
	// Query inside the charged slot.
	queryAt := time.UnixMilli(plan.Actions[chargedSlotIdx].SlotStartMs).Add(1 * time.Minute)
	d, ok := svc.SlotDirectiveAt(queryAt)
	if !ok {
		t.Fatal("SlotDirectiveAt returned ok=false")
	}
	if d.LoadpointEnergyWh == nil {
		t.Fatalf("LoadpointEnergyWh nil on slot %d where DP set LoadpointW=%f",
			chargedSlotIdx, plan.Actions[chargedSlotIdx].LoadpointW)
	}
	wh, exists := d.LoadpointEnergyWh["garage"]
	if !exists {
		t.Fatalf("garage missing: %+v", d.LoadpointEnergyWh)
	}
	if wh <= 0 {
		t.Errorf("LoadpointEnergyWh[garage] = %.1f, want > 0", wh)
	}
	if _, ok := d.LoadpointSoCTargetPct["garage"]; !ok {
		t.Errorf("LoadpointSoCTargetPct missing garage entry")
	}
}

// TestSlotDirectiveEmptyWhenNoLoadpoint asserts the legacy path:
// when no loadpoint was active, SlotDirective's LP fields stay nil
// so older dispatch code paths see no change.
func TestSlotDirectiveEmptyWhenNoLoadpoint(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Truncate(15 * time.Minute)
	slots := []Slot{
		{StartMs: start.UnixMilli(), LenMin: 15, PriceOre: 50,
			LoadW: 500, Confidence: 1.0},
	}
	plan := Optimize(slots, Params{
		Mode: ModeSelfConsumption, SoCLevels: 11, CapacityWh: 10000,
		SoCMinPct: 10, SoCMaxPct: 95, InitialSoCPct: 50,
		ActionLevels: 5, MaxChargeW: 2000, MaxDischargeW: 2000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	svc := &Service{last: &plan, lastLoadpointID: ""}
	d, ok := svc.SlotDirectiveAt(start.Add(1 * time.Minute))
	if !ok {
		t.Fatal("SlotDirectiveAt ok=false")
	}
	if d.LoadpointEnergyWh != nil {
		t.Errorf("expected nil LoadpointEnergyWh, got %+v", d.LoadpointEnergyWh)
	}
}
