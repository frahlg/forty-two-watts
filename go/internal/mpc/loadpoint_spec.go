package mpc

// LoadpointSpec tells the DP how to extend its state space with an EV
// loadpoint. Set `Params.Loadpoint` to a non-nil spec to have the
// optimizer treat charging the EV as a decision variable alongside
// battery action. Leave nil to preserve the legacy battery-only DP.
//
// Design choices:
//
//   - The action set is DISCRETE (AllowedStepsW) rather than continuous.
//     EVs have real disjunctive constraints — a 1-phase charger jumps
//     {0, 1.4, 2.3} kW but not 3.5 kW; a 3-phase jumps to 4.1+ only.
//     LP/MILP would need binary variables; DP just enumerates the
//     allowed levels and the infeasible gap between 1-phase and
//     3-phase minima is handled for free.
//
//   - Only charging is modeled — no V2G. Our current chargers
//     (Easee, Zap) don't discharge to the grid, and including V2G
//     would double the action dimension.
//
//   - Target SoC + deadline are honored via a linearly decaying
//     terminal penalty in the DP (see optimize.go). This is a
//     "lexicographic fallback" analogue: prefer meeting target, but
//     when infeasible, maximize delivered energy instead of
//     returning no plan.
type LoadpointSpec struct {
	ID string // matches loadpoint.Config.ID for dispatch routing

	// Vehicle battery capacity (Wh). Drives SoC% ↔ Wh conversion.
	CapacityWh float64

	// SoC grid. Coarser than battery (11 is typical — EV loads are
	// lumpy anyway).
	Levels  int
	MinPct  float64 // usually 0
	MaxPct  float64 // usually 100

	// Plan-start conditions.
	InitialSoCPct float64 // EV SoC at the first slot
	PluggedIn     bool    // when false, Optimize treats the loadpoint as absent

	// User intent. Zero target (< 1%) = no deadline — charge
	// opportunistically based on price/PV surplus only.
	TargetSoCPct    float64
	TargetSlotIdx   int // slot index by which target must be met; 0 or negative = no deadline

	// Electrical constraints. AllowedStepsW MUST include 0 (off) and
	// should enumerate the discrete charger power levels. If empty,
	// defaults to {0, MaxChargeW}.
	MaxChargeW      float64
	AllowedStepsW   []float64

	// Charge-side efficiency (AC → battery). Typical 0.90 for a
	// modern 3-phase EV charger. 0 defaults to 0.90.
	ChargeEfficiency float64

	// Policy applies energy-source constraints to the DP action loop.
	// nil = legacy unrestricted behaviour (EV may use any source);
	// non-nil activates the AllowGrid / AllowBatterySupport /
	// OnlySurplus gates described on the Policy type. Stored as a
	// pointer so the zero-value LoadpointSpec — used extensively in
	// the existing tests — keeps its unrestricted semantics.
	Policy *Policy
}

// Policy is the per-schedule energy-source gate set consulted by the
// DP when evaluating EV charging actions. Each field is an
// independent boolean constraint; they combine by AND.
//
//   - OnlySurplus: during daylight slots (slot.PVW < -SurplusEpsilonW)
//     the DP clamps evW to the forecast surplus -(LoadW + PVW). Night
//     slots are not clamped by this flag — pair with AllowGrid=false
//     for a strict "no grid import ever" schedule.
//
//   - AllowGrid: when false, the DP rejects any action where a non-
//     zero evW forces net grid import (gridW > SurplusEpsilonW). Hard
//     counterpart to OnlySurplus — applies day AND night.
//
//   - AllowBatterySupport: when false, the DP rejects actions where
//     the home battery discharges (battW < 0) while the EV draws
//     (evW > 0). Battery may not prop up EV charging; EV must be
//     covered by PV or grid (subject to AllowGrid).
//
// SurplusEpsilonW is the shared slack value (forecast jitter / kWh
// round-off). Defaults to 100 W when 0.
//
// Construct Policy via loadpoint.Policy → mpc.Policy at the main.go
// wire boundary. This package stays independent of loadpoint's
// in-memory types.
type Policy struct {
	AllowGrid           bool
	AllowBatterySupport bool
	OnlySurplus         bool
	SurplusEpsilonW     float64
}

// normalizedSteps returns a non-nil, 0-included, dedup'd + sorted
// action set. Used internally by the DP.
func (l *LoadpointSpec) normalizedSteps() []float64 {
	if l == nil {
		return nil
	}
	if len(l.AllowedStepsW) == 0 {
		if l.MaxChargeW <= 0 {
			return []float64{0}
		}
		return []float64{0, l.MaxChargeW}
	}
	seen := map[float64]struct{}{0: {}}
	out := []float64{0}
	for _, s := range l.AllowedStepsW {
		if s < 0 {
			continue
		}
		if l.MaxChargeW > 0 && s > l.MaxChargeW {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	// Bubble-insertion sort (few elements, clarity > performance).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// active reports whether the DP should include EV dimensions for
// this spec. Nil or un-plugged = inactive; treat as pure battery.
func (l *LoadpointSpec) active() bool {
	return l != nil && l.PluggedIn && l.CapacityWh > 0 && l.Levels >= 2
}
