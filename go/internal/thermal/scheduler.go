// Package thermal schedules and dispatches "block" commands to
// thermal-battery drivers (heat pumps, smart hot-water cylinders)
// based on the price forecast.
//
// Why a separate package, not the MPC DP
// ======================================
// Thermal stores model badly as a discrete-action DP variable for
// three reasons:
//
//  1. Real thermal SoC is opaque — we don't know how full the
//     hot-water tank actually is, only that the operator says it
//     can be blocked for ~N kWh per day before comfort suffers.
//  2. Adding a per-slot block-w dimension to the DP would multiply
//     the state space by L (block levels) — measurable cost on a
//     Pi, and the optimisation is dominated by a coarse "block N
//     most-expensive slots within budget" heuristic anyway.
//  3. The DerThermalBattery type intentionally keeps thermal
//     drivers OUT of every site-equation aggregator (api batW,
//     mpc loadW reconstruction, dispatch currentTotal). Modelling
//     them inside the DP would re-introduce the double-counting
//     this whole change exists to avoid.
//
// Instead this package runs an O(N log N) sort over the price
// forecast after each MPC replan and picks the cheapest set of
// slots to block. The DP doesn't see the load reduction, which is
// a mild correctness sacrifice — the battery's planned charge
// timing is unaware that the heat pump will skip its draw at
// expensive hours. In practice that just means the battery
// charges from its own cheap-window plan and the heat pump's
// blocking is a free win on top.
package thermal

import (
	"sort"
	"time"
)

// PriceSlot is the minimal price signal the scheduler needs:
// when the slot starts, how long it is, and how expensive
// importing during it would be. Mirrors mpc.Slot but kept
// independent so the package doesn't need to import mpc (which
// would create a cycle once the MPC service consumes thermal
// schedules).
type PriceSlot struct {
	StartMs  int64
	LenMin   int
	PriceOre float64
}

// DriverConfig is the operator-tunable per-driver schedule policy.
// Reasonable defaults documented inline so a brand-new install
// produces a sane schedule from the first replan.
type DriverConfig struct {
	// Driver name as registered in the Lua registry — the
	// thermal.Controller routes block commands to this name via
	// drivers.Registry.Send.
	Name string

	// MaxBlockW is how much load the operator believes the
	// thermal store can shed when fully blocked. For a NIBE F1145
	// in heating-only mode this is the rated compressor draw.
	// Required (zero disables the driver in this scheduler).
	MaxBlockW float64

	// BudgetKwhPerDay caps cumulative blocking over a rolling
	// 24-hour window. Default 6 kWh — empirically the most a
	// well-insulated detached house in S. Sweden can tolerate
	// without losing >1 °C indoor comfort during winter
	// expensive-price hours. Operators on heat-pump-only homes
	// (no resistive backup) should tune lower.
	BudgetKwhPerDay float64

	// MinSlotPriceOre is the price floor below which blocking is
	// pointless: even if budget remains, the saving per kWh
	// blocked is below this number. Default 50 öre/kWh —
	// blocking during a 30 öre slot saves 30 öre × 2 kW × 1 h =
	// 60 öre, not worth the comfort hit.
	MinSlotPriceOre float64
}

// SlotBlock is one scheduled blocking decision: from when, for how
// long, at what command power.
type SlotBlock struct {
	StartMs  int64
	LenMin   int
	BlockW   float64
}

// ScheduleBlocks decides which of the upcoming slots to block,
// honouring the daily kWh budget and the price floor. Greedy:
// block the most expensive slots first, accumulating cost-per-kWh
// savings until the budget is exhausted.
//
// Returns an empty slice if MaxBlockW <= 0 or no slots qualify
// — which is the right thing to do when the operator hasn't
// tuned the driver. The Controller treats an empty schedule as
// "release any held block immediately".
func ScheduleBlocks(slots []PriceSlot, cfg DriverConfig) []SlotBlock {
	if cfg.MaxBlockW <= 0 || len(slots) == 0 {
		return nil
	}
	budget := cfg.BudgetKwhPerDay
	if budget <= 0 {
		budget = 6.0
	}
	floor := cfg.MinSlotPriceOre
	if floor <= 0 {
		floor = 50.0
	}

	// Filter to slots above the price floor and sort descending
	// by price. Stable secondary sort by StartMs so two equally-
	// priced slots are picked in temporal order — gives the
	// operator a slightly more predictable comfort experience
	// than picking the later slot arbitrarily.
	candidates := make([]PriceSlot, 0, len(slots))
	for _, s := range slots {
		if s.PriceOre >= floor {
			candidates = append(candidates, s)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].PriceOre != candidates[j].PriceOre {
			return candidates[i].PriceOre > candidates[j].PriceOre
		}
		return candidates[i].StartMs < candidates[j].StartMs
	})

	out := make([]SlotBlock, 0, len(candidates))
	usedKwh := 0.0
	for _, s := range candidates {
		slotKwh := cfg.MaxBlockW * float64(s.LenMin) / 60.0 / 1000.0
		if usedKwh+slotKwh > budget {
			// Try a partial — we may be able to block at less
			// than full power for the slot if there's budget
			// for some kWh. Skip if even a 30-min partial would
			// exceed budget.
			remaining := budget - usedKwh
			if remaining <= 0 {
				break
			}
			// Linear: the most useful partial is full power for
			// part of the slot, but our actuator (heat pump) is
			// either blocked or not. Translate the remaining
			// kWh back into a block-W command for this slot:
			//     block_w = remaining_kwh × 60 / len_min × 1000
			partialW := remaining * 60.0 / float64(s.LenMin) * 1000.0
			if partialW < cfg.MaxBlockW*0.25 {
				// Less than 25 % of full power isn't worth the
				// session-restart wear on the compressor.
				break
			}
			out = append(out, SlotBlock{
				StartMs: s.StartMs,
				LenMin:  s.LenMin,
				BlockW:  partialW,
			})
			usedKwh = budget
			break
		}
		out = append(out, SlotBlock{
			StartMs: s.StartMs,
			LenMin:  s.LenMin,
			BlockW:  cfg.MaxBlockW,
		})
		usedKwh += slotKwh
	}
	// Re-sort by StartMs so the Controller can do an O(log N)
	// binary search for the current slot rather than scanning
	// the whole schedule each tick.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].StartMs < out[j].StartMs
	})
	return out
}

// BlockAt returns the scheduled block-W for the slot containing
// `now`, or 0 if the current time isn't inside any scheduled
// block. Schedule must be sorted by StartMs (ScheduleBlocks
// guarantees this).
func BlockAt(schedule []SlotBlock, now time.Time) float64 {
	if len(schedule) == 0 {
		return 0
	}
	nowMs := now.UnixMilli()
	// Binary search for the latest slot starting at or before now.
	idx := sort.Search(len(schedule), func(i int) bool {
		return schedule[i].StartMs > nowMs
	}) - 1
	if idx < 0 {
		return 0
	}
	endMs := schedule[idx].StartMs + int64(schedule[idx].LenMin)*60*1000
	if nowMs >= endMs {
		return 0
	}
	return schedule[idx].BlockW
}
