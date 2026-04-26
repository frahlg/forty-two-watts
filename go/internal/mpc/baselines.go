package mpc

// ComputeBaselines returns counter-factual dispatch costs over the
// given horizon + params so the UI can show "savings vs X" numbers.
//
// Three baselines are computed:
//
//   - NoBatteryOre: each slot's grid flow = load + pv (pretend the
//     battery doesn't exist). Costed with the same import/export model
//     the DP uses (consumer-total price for imports, spot + bonus − fee
//     for exports).
//
//   - SelfConsumptionOre: re-runs Optimize with Mode=SelfConsumption
//     over the same slots and params. Using the optimizer itself (vs a
//     hand-rolled simulation) means we inherit the real efficiency,
//     power, SoC-bound, and grid-policy constraints — and the cost is
//     computed by the DP's own per-slot loop, so it's directly
//     comparable to plan.TotalCostOre.
//
//   - FlatAvgOre: same physical flows as NoBatteryOre, but priced at
//     horizon-average rates instead of each slot's real price. Isolates
//     the *timing* component of savings from the *quantity* component
//     — if `plan_cost − flat_avg` ≫ `plan_cost − no_battery`, most of
//     the win is timing (shifting load into cheap hours and/or exporting
//     at expensive ones), not the existence of price variation per se.
//
//     FlatAvg uses an ASYMMETRIC price model that mirrors
//     SlotGridCostOre: imported kWh × the horizon's mean consumer-total
//     price MINUS exported kWh × the horizon's mean export price (mean
//     spot + ExportBonus − ExportFee, clamped ≥ 0; or ExportOrePerKWh
//     if the flat rate is set). Treating both directions with the
//     consumer-total average — as the previous implementation did —
//     overstated export revenue by the grid tariff + VAT, materially
//     distorting the savings panel on PV-export-heavy days.
//
// Cheap to call — the SC re-optimize is one extra Optimize pass
// (~10 ms for the default 193 slots × 51 SoC × 21 actions).
func ComputeBaselines(slots []Slot, p Params) Baselines {
	b := Baselines{}
	if len(slots) == 0 {
		return b
	}

	// ---- No-battery cost + horizon-mean inputs ----
	// One pass populates everything except the SC baseline.
	var (
		netKWh     float64
		importKWh  float64 // total imported (positive)
		exportKWh  float64 // total exported (positive magnitude)
		priceWtMin float64 // time-weighted Σ for consumer-total mean
		spotWtMin  float64 // time-weighted Σ for spot mean
		lenMinSum  float64
	)
	for _, s := range slots {
		dt := float64(s.LenMin) / 60.0
		gridKWh := (s.LoadW + s.PVW) * dt / 1000.0
		b.NoBatteryOre += SlotGridCostOre(s, gridKWh, p)
		netKWh += gridKWh
		switch {
		case gridKWh > 0:
			importKWh += gridKWh
		case gridKWh < 0:
			exportKWh += -gridKWh
		}
		priceWtMin += s.PriceOre * float64(s.LenMin)
		spotWtMin += s.SpotOre * float64(s.LenMin)
		lenMinSum += float64(s.LenMin)
	}
	if lenMinSum > 0 {
		b.AvgPriceOre = priceWtMin / lenMinSum
		b.AvgSpotOre = spotWtMin / lenMinSum
	}
	b.NetKWh = netKWh

	// Effective per-direction prices for the flat-avg baseline. Imports
	// pay the consumer-total mean. Exports earn the spot mean adjusted
	// by the operator's flat bonus/fee (clamped to non-negative so the
	// model never rewards "not exporting"), or the operator's flat
	// export rate when configured.
	avgExportRevenue := p.ExportOrePerKWh
	if avgExportRevenue <= 0 {
		avgExportRevenue = b.AvgSpotOre + p.ExportBonusOreKwh - p.ExportFeeOreKwh
		if avgExportRevenue < 0 {
			avgExportRevenue = 0
		}
	}
	b.AvgExportPriceOre = avgExportRevenue
	b.FlatAvgOre = importKWh*b.AvgPriceOre - exportKWh*avgExportRevenue

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
