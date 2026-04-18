// Package mpc is a receding-horizon energy scheduler. It turns forecast
// prices + forecast PV + current SoC into an optimal battery power
// schedule by running dynamic programming over a discretized SoC grid.
//
// We deliberately avoid an LP/QP solver here:
//   - DP is exact over the quantization grid
//   - No dependencies, one file of pure Go
//   - Easy to explain + audit
//   - Fast enough for horizons up to ~100 slots × 50 SoC × 50 actions
//     (~250k state evaluations — under 50ms on any modern CPU)
//
// Site sign convention (same as the rest of the codebase):
//
//	grid_w  > 0 → importing (paying)
//	grid_w  < 0 → exporting
//	pv_w    < 0 → PV generating into site
//	battery > 0 → charging (load on site)
//	battery < 0 → discharging (source on site)
//
// Power balance per slot (from the grid meter's point of view):
//
//	grid_w = load_w + pv_w + battery_w
//
// Battery efficiency: the `battery_w` we command is measured at the AC
// terminals (site-facing). Due to conversion losses, only a fraction
// actually lands in (or comes out of) the cells:
//
//	charging   (battery_w > 0):  ΔSoC_kWh = +battery_w × dt × charge_eff
//	discharging(battery_w < 0):  ΔSoC_kWh = +battery_w × dt / discharge_eff
//
// So a 1000W charge command with 95% efficiency adds 950Wh/h to SoC. A
// 1000W discharge command with 95% efficiency drains ~1053Wh/h from SoC.
// Round-trip = charge_eff × discharge_eff (typically ~0.90).
package mpc

import (
	"math"
	"strings"
	"time"
)

// Mode selects how aggressively the planner uses the battery.
type Mode string

const (
	// ModeSelfConsumption: only use the battery to cover local load or
	// absorb PV surplus. Never import to charge; never export to discharge.
	// Matches the behavior of the base control loop (no planning needed).
	ModeSelfConsumption Mode = "self_consumption"

	// ModeCheapCharge: allow importing to charge when prices are low
	// (the DP decides based on forecast). Still never export battery to
	// grid — discharge stays ≤ local load.
	ModeCheapCharge Mode = "cheap_charge"

	// ModeArbitrage: unrestricted. Charge from grid, discharge to grid —
	// whatever minimizes total cost over the horizon, subject to SoC and
	// power limits.
	ModeArbitrage Mode = "arbitrage"
)

// Slot is one input time slot for the optimizer.
type Slot struct {
	StartMs  int64
	LenMin   int
	PriceOre float64 // total consumer öre/kWh (incl. grid + VAT) — used for IMPORT cost
	SpotOre  float64 // raw spot öre/kWh — used for EXPORT revenue (before bonus/fee)
	PVW      float64 // negative (site sign). 0 if no forecast.
	LoadW    float64 // positive (site sign). Defaults to a flat baseline.

	// Confidence in [0, 1]. 1.0 = real day-ahead price; < 1.0 = ML-
	// forecasted price where we're less sure of both level and shape.
	// The DP blends low-confidence prices toward the horizon mean so
	// the planner doesn't over-commit to uncertain spikes. Defaults to
	// 1.0 when callers leave it zero.
	Confidence float64

	// Limits caps grid flow for this slot. Zero value = unlimited.
	// See power_limits.go for use cases (peak-tariff capacity, DSO
	// curtailment, service-entrance current limit).
	Limits PowerLimits
}

// Params bounds the optimization. All fields are required.
type Params struct {
	Mode Mode

	// SoC grid
	SoCLevels     int     // e.g. 41 (2.5% steps)
	CapacityWh    float64 // aggregate battery capacity
	SoCMinPct     float64 // e.g. 10
	SoCMaxPct     float64 // e.g. 95
	InitialSoCPct float64

	// Action grid (+charge, −discharge; site sign)
	ActionLevels  int     // odd number preferred so 0 is represented (e.g. 21)
	MaxChargeW    float64 // ≥ 0
	MaxDischargeW float64 // ≥ 0 (magnitude)

	// Efficiency (0..1). Default 0.95 each → ~90% round-trip.
	ChargeEfficiency    float64
	DischargeEfficiency float64

	// Terminal valuation. If > 0, we credit the plan with
	// TerminalSoCPrice × remaining_kwh at the final slot. Prevents the
	// planner from always ending at SoCMin to minimize cost. A good
	// default is the mean price over the horizon.
	TerminalSoCPrice float64

	// Export revenue. Two modes:
	//   - If ExportOrePerKWh > 0, every slot earns this flat rate on
	//     export. Useful for operators with a fixed feed-in tariff.
	//   - If ExportOrePerKWh == 0, each slot earns
	//         slot.SpotOre + ExportBonusOreKwh − ExportFeeOreKwh
	//     (clamped to zero) — per-slot pricing, which the DP needs to
	//     see morning-vs-midday arbitrage opportunities.
	ExportOrePerKWh    float64
	ExportBonusOreKwh  float64
	ExportFeeOreKwh    float64
}

// Action is one scheduled battery target.
type Action struct {
	SlotStartMs int64   `json:"slot_start_ms"`
	SlotLenMin  int     `json:"slot_len_min"`
	PriceOre    float64 `json:"price_ore"`
	PVW         float64 `json:"pv_w"`
	LoadW       float64 `json:"load_w"`
	BatteryW    float64 `json:"battery_w"`  // decision (site sign, AC terminals)
	GridW       float64 `json:"grid_w"`     // resulting grid power
	SoCPct      float64 `json:"soc_pct"`    // SoC at END of slot
	CostOre     float64 `json:"cost_ore"`   // this slot's cost (öre). Negative = revenue.
	Confidence  float64 `json:"confidence"` // 1.0 real, <1.0 forecasted (UI uses this to style)
	Reason      string  `json:"reason"`     // short human-readable explanation
	EMSMode     string  `json:"ems_mode"`   // effective EMS mode for this slot (set by SlotAt post-processing)

	// PVLimitW is the recommended cap on PV inverter output (W, positive).
	// 0 = no curtailment. Set by post-processing when exporting would
	// cost money (negative export revenue after fees). Consumed by the
	// control loop only when the driver advertises `supports_pv_curtail`.
	PVLimitW float64 `json:"pv_limit_w,omitempty"`
}

// Plan is the output.
type Plan struct {
	GeneratedAtMs int64    `json:"generated_at_ms"`
	Mode          Mode     `json:"mode"`
	HorizonSlots  int      `json:"horizon_slots"`
	CapacityWh    float64  `json:"capacity_wh"`
	InitialSoCPct float64  `json:"initial_soc_pct"`
	TotalCostOre  float64  `json:"total_cost_ore"`
	Actions       []Action `json:"actions"`
}

// Optimize runs DP and returns the cost-minimizing plan.
//
// Complexity: O(N × S × A) where N = len(slots), S = SoCLevels, A = ActionLevels.
// For a 96-slot (24h × 15m) horizon with 41 SoC × 21 action levels, that's
// ~82k evaluations — well under 10ms.
func Optimize(slots []Slot, p Params) Plan {
	now := time.Now().UnixMilli()
	if len(slots) == 0 || p.CapacityWh <= 0 {
		return Plan{GeneratedAtMs: now, Mode: p.Mode}
	}
	if p.Mode == "" {
		p.Mode = ModeSelfConsumption
	}
	if p.ChargeEfficiency <= 0 {
		p.ChargeEfficiency = 0.95
	}
	if p.DischargeEfficiency <= 0 {
		p.DischargeEfficiency = 0.95
	}
	N := len(slots)
	S := p.SoCLevels
	if S < 3 {
		S = 3
	}
	A := p.ActionLevels
	if A < 3 {
		A = 3
	}

	socStep := (p.SoCMaxPct - p.SoCMinPct) / float64(S-1)
	socAt := func(i int) float64 { return p.SoCMinPct + float64(i)*socStep }

	// Confidence handling: compute the horizon mean (real + forecast)
	// so we can blend low-confidence prices toward it. Default any
	// missing confidence to 1.0 (treat caller-unaware slots as "real").
	var sumPrice float64
	for i := range slots {
		if slots[i].Confidence <= 0 {
			slots[i].Confidence = 1.0
		}
		sumPrice += slots[i].PriceOre
	}
	meanPrice := sumPrice / float64(N)
	// effPrice(slot) = c × raw + (1 − c) × mean. c=1 → raw; c<1 pulls
	// toward horizon mean, dampening arbitrage the DP sees on shaky
	// forecasted slots without hiding them entirely.
	effPrice := func(s Slot) float64 {
		return s.Confidence*s.PriceOre + (1-s.Confidence)*meanPrice
	}
	// slotExportOre: per-slot export revenue. When Params.ExportOrePerKWh
	// is set, it wins (fixed feed-in tariff). Otherwise each slot earns
	// spot + bonus − fee, clamped at zero. Without this, the DP saw
	// flat export revenue across the day and couldn't prefer "export
	// at high-price morning, charge at cheap midday".
	slotExportOre := func(s Slot) float64 {
		if p.ExportOrePerKWh > 0 {
			return p.ExportOrePerKWh
		}
		v := s.SpotOre + p.ExportBonusOreKwh - p.ExportFeeOreKwh
		// Confidence blend on the same principle as import price.
		mean := meanPrice * 0.7 // rough: spot ≈ 70% of consumer total
		v = s.Confidence*v + (1-s.Confidence)*mean
		if v < 0 {
			v = 0
		}
		return v
	}

	// Action grid spans −MaxDischargeW … +MaxChargeW. Forcing an odd
	// ActionLevels puts 0 exactly at the midpoint.
	actionAt := func(j int) float64 {
		if A == 1 {
			return 0
		}
		frac := float64(j) / float64(A-1) // 0..1
		return -p.MaxDischargeW + frac*(p.MaxChargeW+p.MaxDischargeW)
	}

	// V[t][s] = minimum expected cost from slot t onward, starting from
	// SoC index s. V is filled backwards.
	V := make([][]float64, N+1)
	Policy := make([][]int, N)
	for t := 0; t <= N; t++ {
		V[t] = make([]float64, S)
		if t < N {
			Policy[t] = make([]int, S)
		}
	}

	// Terminal value: credit stored energy at TerminalSoCPrice (öre/kWh).
	// Cost is negative (=credit), so we subtract.
	for s := 0; s < S; s++ {
		kwh := p.CapacityWh * socAt(s) / 100.0 / 1000.0
		V[N][s] = -p.TerminalSoCPrice * kwh
	}

	// Backwards induction.
	for t := N - 1; t >= 0; t-- {
		slot := slots[t]
		dtH := float64(slot.LenMin) / 60.0
		baselineGridW := slot.LoadW + slot.PVW // grid if battery did nothing
		for s := 0; s < S; s++ {
			soc := socAt(s)
			bestV := math.Inf(1)
			bestA := 0
			for j := 0; j < A; j++ {
				actW := actionAt(j)
				gridW := baselineGridW + actW

				// Mode-based feasibility.
				if !modeAllows(p.Mode, baselineGridW, gridW, actW) {
					continue
				}

				// Per-slot power limits (tariff capacity / DSO
				// curtailment / service-entrance fuse). Zero-value
				// PowerLimits is unlimited — the zero-check lives
				// inside allowsImport/allowsExport.
				if !slot.Limits.allowsImport(gridW) || !slot.Limits.allowsExport(gridW) {
					continue
				}

				// SoC transition with efficiency (guarded at function entry).
				var dSoCWh float64
				if actW >= 0 {
					dSoCWh = +actW * dtH * p.ChargeEfficiency
				} else {
					dSoCWh = +actW * dtH / p.DischargeEfficiency
				}
				dSoCPct := dSoCWh / p.CapacityWh * 100.0
				soc2 := soc + dSoCPct
				if soc2 < p.SoCMinPct-1e-9 || soc2 > p.SoCMaxPct+1e-9 {
					continue
				}

				// Per-slot cost in öre. Import cost at consumer price;
				// export revenue at the slot's spot + bonus − fee.
				// Both sides are confidence-blended so ML-forecasted
				// slots nudge the DP less aggressively.
				gridKWh := gridW * dtH / 1000.0
				var cost float64
				if gridKWh > 0 {
					cost = effPrice(slot) * gridKWh
				} else {
					cost = -slotExportOre(slot) * (-gridKWh)
				}

				// Next SoC index: linear interpolation between floor/ceil.
				fIdx := (soc2 - p.SoCMinPct) / socStep
				lo := int(math.Floor(fIdx))
				hi := lo + 1
				if lo < 0 {
					lo, hi = 0, 0
				}
				if hi >= S {
					lo, hi = S-1, S-1
				}
				frac := fIdx - float64(lo)
				vNext := (1-frac)*V[t+1][lo] + frac*V[t+1][hi]
				total := cost + vNext
				if total < bestV {
					bestV = total
					bestA = j
				}
			}
			V[t][s] = bestV
			Policy[t][s] = bestA
		}
	}

	// Forward simulate using the policy.
	plan := Plan{
		GeneratedAtMs: now,
		Mode:          p.Mode,
		HorizonSlots:  N,
		CapacityWh:    p.CapacityWh,
		InitialSoCPct: p.InitialSoCPct,
		Actions:       make([]Action, 0, N),
	}
	fIdx := (p.InitialSoCPct - p.SoCMinPct) / socStep
	s := int(math.Round(fIdx))
	if s < 0 {
		s = 0
	}
	if s >= S {
		s = S - 1
	}
	soc := socAt(s)
	var totalCost float64
	for t := 0; t < N; t++ {
		slot := slots[t]
		dtH := float64(slot.LenMin) / 60.0
		j := Policy[t][s]
		actW := actionAt(j)
		// SoC transition with efficiency (guarded at function entry).
		var dSoCWh float64
		if actW >= 0 {
			dSoCWh = +actW * dtH * p.ChargeEfficiency
		} else {
			dSoCWh = +actW * dtH / p.DischargeEfficiency
		}
		soc2 := soc + dSoCWh/p.CapacityWh*100.0
		if soc2 < p.SoCMinPct {
			soc2 = p.SoCMinPct
		}
		if soc2 > p.SoCMaxPct {
			soc2 = p.SoCMaxPct
		}
		gridW := slot.LoadW + slot.PVW + actW
		gridKWh := gridW * dtH / 1000.0
		// Report the ACTUAL expected cost using the raw (un-blended)
		// prices so the UI summary reflects "what we'd actually pay
		// if prices hold". Blending is a decision lens only.
		var cost float64
		if gridKWh > 0 {
			cost = slot.PriceOre * gridKWh
		} else {
			rawExport := p.ExportOrePerKWh
			if rawExport <= 0 {
				rawExport = slot.SpotOre + p.ExportBonusOreKwh - p.ExportFeeOreKwh
				if rawExport < 0 {
					rawExport = 0
				}
			}
			cost = -rawExport * (-gridKWh)
		}
		totalCost += cost
		plan.Actions = append(plan.Actions, Action{
			SlotStartMs: slot.StartMs,
			SlotLenMin:  slot.LenMin,
			PriceOre:    slot.PriceOre,
			Confidence:  slot.Confidence,
			PVW:         slot.PVW,
			LoadW:       slot.LoadW,
			BatteryW:    actW,
			GridW:       gridW,
			SoCPct:      soc2,
			CostOre:     cost,
			Reason:      reasonFor(slot, actW, meanPrice),
		})
		soc = soc2
		fIdx = (soc - p.SoCMinPct) / socStep
		s = int(math.Round(fIdx))
		if s < 0 {
			s = 0
		}
		if s >= S {
			s = S - 1
		}
	}
	plan.TotalCostOre = totalCost
	annotateCurtailment(&plan, p.ExportOrePerKWh)
	return plan
}

// annotateCurtailment walks the plan and flags slots where curtailing
// PV would avoid a net-negative export event. Triggered when:
//
//   - the slot is exporting (grid_w < 0)
//   - AND export revenue is non-positive (fee ≥ revenue, or negative spot)
//   - AND the battery can't absorb more (already charging at max)
//
// In that case exporting PV costs money with no offsetting benefit.
// Recommended PV limit = load + battery_charge (just cover what the
// site + battery can consume). Driver dispatches this only if it
// advertises PV-curtailment support. The CostOre doesn't change — the
// DP already priced this slot as-is; curtailment is a mitigation
// applied at dispatch time.
func annotateCurtailment(plan *Plan, exportOrePerKWh float64) {
	if exportOrePerKWh > 0 {
		// Positive export price → exporting is always better than
		// curtailing. Nothing to do.
		return
	}
	for i := range plan.Actions {
		a := &plan.Actions[i]
		if a.GridW >= 0 {
			continue // importing, not exporting
		}
		// Slot is exporting. If we can't earn on export, cap PV to
		// what's being consumed locally + stored.
		consumedW := a.LoadW
		if a.BatteryW > 0 {
			consumedW += a.BatteryW // site-sign: + = charging (absorbs PV)
		}
		if consumedW < 0 {
			consumedW = 0
		}
		// Only suggest curtailment if PV actually exceeds local consumption.
		pvAbs := -a.PVW // PV stored site-signed as negative
		if pvAbs > consumedW {
			a.PVLimitW = consumedW
			if a.Reason != "" && !strings.HasSuffix(a.Reason, ")") {
				a.Reason += " · curtail PV"
			} else {
				a.Reason = "curtail PV (negative export) · " + a.Reason
			}
		}
	}
}

// modeAllows enforces the mode's grid-use policy.
//
//	baselineGridW = load + pv  (what grid would see with no battery action)
//	gridW         = baselineGridW + actW  (what grid actually sees)
//	actW          = battery command (+ charge, − discharge)
func modeAllows(m Mode, baselineGridW, gridW, actW float64) bool {
	const eps = 1e-6
	switch m {
	case ModeSelfConsumption:
		// Battery must only move the grid toward zero, never past it:
		//   if baseline > 0 (import): grid must be in [0, baseline]
		//   if baseline < 0 (export): grid must be in [baseline, 0]
		//   if baseline == 0: battery must be 0
		if baselineGridW > eps {
			return gridW >= -eps && gridW <= baselineGridW+eps
		}
		if baselineGridW < -eps {
			return gridW <= eps && gridW >= baselineGridW-eps
		}
		return math.Abs(actW) < eps
	case ModeCheapCharge:
		// Allow charging from grid (any actW ≥ 0), but never discharge past
		// the local load: i.e. gridW must stay ≥ 0 when we'd otherwise be
		// importing, OR ≥ baseline when we'd otherwise be exporting.
		// Simpler rule: no battery-driven export, i.e. gridW ≥ min(0, baseline).
		minGrid := 0.0
		if baselineGridW < 0 {
			minGrid = baselineGridW
		}
		return gridW >= minGrid-eps
	case ModeArbitrage:
		return true
	default:
		return true
	}
}

// reasonFor returns a short human-readable explanation of the planner's
// decision for a single slot. The UI surfaces this on hover so operators
// can see *why* the battery is (dis)charging — explainable AI at the
// level it actually helps: per-decision.
func reasonFor(s Slot, batteryW, meanPrice float64) string {
	baseline := s.LoadW + s.PVW // what grid would see with no battery
	const chargeThresh = 100.0
	priceTag := ""
	if s.Confidence < 1.0 {
		priceTag = " (predicted)"
	}
	switch {
	case batteryW > chargeThresh && baseline < -chargeThresh:
		// Exporting baseline, battery charging → absorbing PV surplus.
		return "absorb PV surplus" + priceTag
	case batteryW > chargeThresh && s.PriceOre < meanPrice*0.9:
		return "charge — price below horizon mean" + priceTag
	case batteryW > chargeThresh:
		return "charge" + priceTag
	case batteryW < -chargeThresh && baseline > chargeThresh:
		return "discharge — cover local load" + priceTag
	case batteryW < -chargeThresh && s.PriceOre > meanPrice*1.1:
		return "discharge — price above horizon mean" + priceTag
	case batteryW < -chargeThresh:
		return "discharge" + priceTag
	default:
		if baseline > chargeThresh {
			return "idle — import to cover load" + priceTag
		}
		if baseline < -chargeThresh {
			return "idle — export PV surplus" + priceTag
		}
		return "idle" + priceTag
	}
}
