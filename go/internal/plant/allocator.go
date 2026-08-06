// Package plant turns N PCS/rack units into one logical battery. Core
// (via the ftw_plant driver) sees a single aggregate: one setpoint in,
// aggregate SoC/headroom out. Internally the allocator splits the
// setpoint across healthy units, biased toward SoC balance, and derates
// as units fault or drop comms. The module holds no authority beyond
// what core grants: setpoints arrive with a lease, and lease expiry
// ramps every unit to zero (see watchdog.go).
//
// Power follows the site convention throughout: positive W charges.
package plant

import "math"

// UnitState is the allocator's view of one rack.
type UnitState struct {
	ID            string
	Online        bool // answering Modbus and not faulted
	SoC           float64
	CapacityWh    float64
	MaxChargeW    float64
	MaxDischargeW float64
	PowerW        float64 // last reported actual
}

// socBalanceGain scales how strongly allocation prefers low-SoC units
// when charging (and high-SoC units when discharging). 1.0 means a unit
// 10 pp below the fleet mean gets ~10% extra weight — enough to converge
// the fleet over minutes without starving any unit's share.
const socBalanceGain = 1.0

// Allocate splits an aggregate setpoint across units. Guarantees:
//   - only online units receive non-zero targets;
//   - no unit exceeds its own direction limit;
//   - Σ targets == clamp(aggregate, fleet headroom) within 1 W;
//   - charge biases toward low-SoC units, discharge toward high-SoC,
//     with full/empty units excluded from the respective direction.
func Allocate(units []UnitState, aggregateW float64) map[string]float64 {
	out := make(map[string]float64, len(units))
	for _, u := range units {
		out[u.ID] = 0
	}
	if aggregateW == 0 {
		return out
	}
	charging := aggregateW > 0

	// Eligible units and their direction caps.
	type cand struct {
		id     string
		capW   float64 // direction limit, positive magnitude
		weight float64
	}
	var cands []cand
	var meanSoC, capSum float64
	for _, u := range units {
		if !u.Online || u.CapacityWh <= 0 {
			continue
		}
		meanSoC += u.SoC * u.CapacityWh
		capSum += u.CapacityWh
	}
	if capSum <= 0 {
		return out
	}
	meanSoC /= capSum

	for _, u := range units {
		if !u.Online || u.CapacityWh <= 0 {
			continue
		}
		var capW float64
		if charging {
			if u.SoC >= 0.999 {
				continue
			}
			capW = u.MaxChargeW
		} else {
			if u.SoC <= 0.001 {
				continue
			}
			capW = u.MaxDischargeW
		}
		if capW <= 0 {
			continue
		}
		// Balance bias: charging favors below-mean SoC, discharging
		// favors above-mean. Weight floor keeps every eligible unit
		// participating so no single rack takes all cycling wear.
		bias := (meanSoC - u.SoC)
		if !charging {
			bias = -bias
		}
		w := capW * math.Max(0.1, 1+socBalanceGain*bias)
		cands = append(cands, cand{id: u.ID, capW: capW, weight: w})
	}
	if len(cands) == 0 {
		return out
	}

	// Clamp the aggregate to fleet headroom.
	var headroom float64
	for _, c := range cands {
		headroom += c.capW
	}
	magnitude := math.Min(math.Abs(aggregateW), headroom)

	// Weighted split with residual redistribution: a unit pinned at its
	// cap frees its excess for the others. Two passes always converge
	// because caps only ever remove candidates.
	remaining := magnitude
	active := cands
	for pass := 0; pass < len(cands) && remaining > 0.5 && len(active) > 0; pass++ {
		var weightSum float64
		for _, c := range active {
			weightSum += c.weight
		}
		if weightSum <= 0 {
			break
		}
		var next []cand
		allocated := 0.0
		for _, c := range active {
			share := remaining * c.weight / weightSum
			already := out[c.id]
			if already+share >= c.capW {
				share = c.capW - already // pin at cap, drop from next pass
			} else {
				next = append(next, c)
			}
			out[c.id] = already + share
			allocated += share
		}
		remaining -= allocated
		active = next
	}

	if !charging {
		for id := range out {
			out[id] = -out[id]
		}
	}
	return out
}

// Aggregate is the fleet summary reported to core.
type Aggregate struct {
	SoC                 float64 `json:"soc"`
	UsableEnergyWh      float64 `json:"usable_energy_wh"`
	CapacityWh          float64 `json:"capacity_wh"`
	PowerW              float64 `json:"power_w"`
	AvailableChargeW    float64 `json:"available_charge_w"`
	AvailableDischargeW float64 `json:"available_discharge_w"`
	UnitsOnline         int     `json:"units_online"`
	UnitsTotal          int     `json:"units_total"`
}

// Summarize folds unit states into the aggregate core consumes. Offline
// units contribute nothing: their energy is unreachable and counting it
// would overstate both SoC and headroom.
func Summarize(units []UnitState) Aggregate {
	var agg Aggregate
	agg.UnitsTotal = len(units)
	var socWeighted float64
	for _, u := range units {
		if !u.Online {
			continue
		}
		agg.UnitsOnline++
		agg.CapacityWh += u.CapacityWh
		socWeighted += u.SoC * u.CapacityWh
		agg.UsableEnergyWh += u.SoC * u.CapacityWh
		agg.PowerW += u.PowerW
		if u.SoC < 0.999 {
			agg.AvailableChargeW += u.MaxChargeW
		}
		if u.SoC > 0.001 {
			agg.AvailableDischargeW += u.MaxDischargeW
		}
	}
	if agg.CapacityWh > 0 {
		agg.SoC = socWeighted / agg.CapacityWh
	}
	return agg
}
