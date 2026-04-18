package mpc

// DiagnosticSlot joins the per-slot inputs the DP saw with the action
// it chose. One row per horizon slot, indexed from 0 at the slot
// containing "now". The UI renders this as the per-slot explainability
// table so operators can answer "why did the planner charge at 21:00?".
type DiagnosticSlot struct {
	Idx         int     `json:"idx"`
	SlotStartMs int64   `json:"slot_start_ms"`
	SlotEndMs   int64   `json:"slot_end_ms"`
	LenMin      int     `json:"len_min"`

	// Inputs
	PriceOre   float64 `json:"price_ore"`   // consumer total (spot + tariff + VAT)
	SpotOre    float64 `json:"spot_ore"`    // raw spot — used for export revenue
	Confidence float64 `json:"confidence"`  // 1.0 = day-ahead, 0.6 = forecast
	PVW        float64 `json:"pv_w"`        // site-signed (≤ 0 when producing)
	LoadW      float64 `json:"load_w"`

	// Outputs
	BatteryW float64 `json:"battery_w"`
	GridW    float64 `json:"grid_w"`
	SoCPct   float64 `json:"soc_pct"`       // SoC at END of slot
	CostOre  float64 `json:"cost_ore"`      // raw (un-blended) slot cost
	Reason   string  `json:"reason"`
	EMSMode  string  `json:"ems_mode"`
	PVLimitW float64 `json:"pv_limit_w,omitempty"`
}

// DiagnosticParams is a JSON-friendly subset of the Params struct —
// enough for operators to verify the DP was parameterized correctly
// without pulling the whole internal struct.
type DiagnosticParams struct {
	Mode                Mode    `json:"mode"`
	InitialSoCPct       float64 `json:"initial_soc_pct"`
	SoCMinPct           float64 `json:"soc_min_pct"`
	SoCMaxPct           float64 `json:"soc_max_pct"`
	SoCLevels           int     `json:"soc_levels"`
	ActionLevels        int     `json:"action_levels"`
	MaxChargeW          float64 `json:"max_charge_w"`
	MaxDischargeW       float64 `json:"max_discharge_w"`
	ChargeEfficiency    float64 `json:"charge_efficiency"`
	DischargeEfficiency float64 `json:"discharge_efficiency"`
	CapacityWh          float64 `json:"capacity_wh"`
	TerminalSoCPrice    float64 `json:"terminal_soc_price_ore_kwh"`
	ExportBonusOreKwh   float64 `json:"export_bonus_ore_kwh"`
	ExportFeeOreKwh     float64 `json:"export_fee_ore_kwh"`
}

// Diagnostic is the full post-mortem of the most recent Optimize call.
// Returned by Service.Diagnose for the /api/mpc/diagnose endpoint.
type Diagnostic struct {
	ComputedAtMs   int64            `json:"computed_at_ms"`
	Zone           string           `json:"zone"`
	Horizon        int              `json:"horizon_slots"`
	TotalCostOre   float64          `json:"total_cost_ore"`
	Params         DiagnosticParams `json:"params"`
	Slots          []DiagnosticSlot `json:"slots"`
	LastReplanAtMs int64            `json:"last_replan_at_ms"`
	LastReason     string           `json:"last_reason"`
}

// Diagnose returns the inputs + outputs of the most recent Optimize
// call, joined per slot. Returns nil until the first successful replan.
//
// The shape matches what the UI renders in the planner inspector so
// operators can audit each slot: "what did the DP see, what did it
// decide, and why". The per-slot `Reason` string already explains the
// decision class; the adjacent inputs show whether the decision was
// grounded in a real day-ahead price (`confidence == 1.0`) or a
// forecasted one (`confidence == 0.6`).
func (s *Service) Diagnose() *Diagnostic {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.last == nil || len(s.lastSlots) == 0 {
		return nil
	}
	// Slots and Actions are generated in the same order by Optimize:
	// action[i] corresponds to slot[i]. Guard against a mismatch
	// anyway — better one-off wrong row than a panic in production.
	n := len(s.lastSlots)
	if len(s.last.Actions) < n {
		n = len(s.last.Actions)
	}
	out := make([]DiagnosticSlot, n)
	for i := 0; i < n; i++ {
		slot := s.lastSlots[i]
		action := s.last.Actions[i]
		out[i] = DiagnosticSlot{
			Idx:         i,
			SlotStartMs: slot.StartMs,
			SlotEndMs:   slot.StartMs + int64(slot.LenMin)*60*1000,
			LenMin:      slot.LenMin,
			PriceOre:    slot.PriceOre,
			SpotOre:     slot.SpotOre,
			Confidence:  slot.Confidence,
			PVW:         slot.PVW,
			LoadW:       slot.LoadW,
			BatteryW:    action.BatteryW,
			GridW:       action.GridW,
			SoCPct:      action.SoCPct,
			CostOre:     action.CostOre,
			Reason:      action.Reason,
			EMSMode:     action.EMSMode,
			PVLimitW:    action.PVLimitW,
		}
	}
	p := s.lastParams
	return &Diagnostic{
		ComputedAtMs: s.last.GeneratedAtMs,
		Zone:         s.Zone,
		Horizon:      s.last.HorizonSlots,
		TotalCostOre: s.last.TotalCostOre,
		Params: DiagnosticParams{
			Mode:                p.Mode,
			InitialSoCPct:       p.InitialSoCPct,
			SoCMinPct:           p.SoCMinPct,
			SoCMaxPct:           p.SoCMaxPct,
			SoCLevels:           p.SoCLevels,
			ActionLevels:        p.ActionLevels,
			MaxChargeW:          p.MaxChargeW,
			MaxDischargeW:       p.MaxDischargeW,
			ChargeEfficiency:    p.ChargeEfficiency,
			DischargeEfficiency: p.DischargeEfficiency,
			CapacityWh:          p.CapacityWh,
			TerminalSoCPrice:    p.TerminalSoCPrice,
			ExportBonusOreKwh:   p.ExportBonusOreKwh,
			ExportFeeOreKwh:     p.ExportFeeOreKwh,
		},
		Slots:          out,
		LastReplanAtMs: s.lastReplanAt.UnixMilli(),
		LastReason:     s.lastReason,
	}
}
