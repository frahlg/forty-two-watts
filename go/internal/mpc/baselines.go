package mpc

// ComputeBaselines returns counter-factual dispatch costs over the
// given horizon + params so the UI can show "savings vs X" numbers.
//
// Three baselines are computed:
//
//   - NoBatteryOre: each slot's grid flow = load + pv (pretend the
//     battery doesn't exist). Costed with the same import/export model
//     the DP uses.
//
//   - SelfConsumptionOre: re-runs Optimize with Mode=SelfConsumption
//     over the same slots and params. Using the optimizer itself (vs a
//     hand-rolled simulation) means we inherit the real efficiency,
//     power, SoC-bound, and grid-policy constraints — and the cost is
//     computed by the DP's own per-slot loop, so it's directly
//     comparable to plan.TotalCostOre.
//
//   - FlatAvgOre: total net consumption × the horizon's mean import
//     price. Shows the value of *when* energy is moved — if the
//     optimizer saves more vs FlatAvg than vs NoBattery, most of the
//     win is timing (shifting load into cheap hours) rather than PV
//     self-consumption. A diagnostic, not an operational baseline.
//
// Cheap to call — the SC re-optimize is one extra Optimize pass
// (~10ms for the default 193 slots × 51 SoC × 21 actions).
func ComputeBaselines(slots []Slot, p Params) Baselines {
	b := Baselines{}
	if len(slots) == 0 {
		return b
	}

	// ---- No-battery baseline + flat-average inputs ----
	// Both can be computed in one pass over the slots.
	var netKWh float64
	var priceWtMin float64
	var lenMinSum float64
	for _, s := range slots {
		dt := float64(s.LenMin) / 60.0
		gridKWh := (s.LoadW + s.PVW) * dt / 1000.0
		b.NoBatteryOre += SlotGridCostOre(s, gridKWh, p)
		netKWh += gridKWh
		priceWtMin += s.PriceOre * float64(s.LenMin)
		lenMinSum += float64(s.LenMin)
	}
	if lenMinSum > 0 {
		b.AvgPriceOre = priceWtMin / lenMinSum
	}
	b.NetKWh = netKWh
	// Flat-avg: charge the net consumption at the mean price. If net is
	// negative (export-dominated horizon), the sign is kept — consistent
	// with the NoBattery cost model where export shows up as negative.
	b.FlatAvgOre = netKWh * b.AvgPriceOre

	// ---- Self-consumption baseline ----
	// Re-run Optimize with the SC policy. Drop the loadpoint from the
	// SC baseline — SC mode wouldn't normally schedule an EV charge,
	// and including its mandatory SoC target would distort the "what if
	// we just did SC" comparison.
	pSC := p
	pSC.Mode = ModeSelfConsumption
	pSC.Loadpoint = nil
	scPlan := Optimize(slots, pSC)
	b.SelfConsumptionOre = scPlan.TotalCostOre
	return b
}
