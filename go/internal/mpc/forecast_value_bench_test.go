package mpc

// Forecast-value bench: does guessing tomorrow's price change what the box
// does RIGHT NOW?
//
// Spot prices publish around 13:00 local. Before that the box holds perhaps
// 12 hours of real day-ahead prices; after, up to 36. The rest of the 48 h
// horizon is an ML price twin, marked Confidence < 1.0, and the DP blends it
// toward the horizon mean:
//
//	effPrice(slot) = confidence × rawPrice + (1 − confidence) × horizonMean
//
// MPC dispatches only the FIRST slot, so the decisive question is not whether
// the guess moves a plan 30 hours out. It is whether it moves the watt that
// reaches hardware in the next 15 minutes. This bench re-solves each recorded
// snapshot three ways on one grid and reports that watt:
//
//	A  as recorded — the confidences the box actually used
//	B  flat guess  — every forecast slot pinned to the horizon mean
//	C  truncated   — the guessed slots deleted, solve the known window only
//
// Two honest limits on what B and C mean, both worth stating before reading
// any number below:
//
//   - B does not erase the guess, it flattens it. horizonMeans() averages
//     over every slot, forecast rows included, so B still inherits the LEVEL
//     the twin predicted — it discards only the SHAPE. That is exactly what
//     Confidence → 0 does inside the DP today, which is the thing being
//     measured.
//   - C deletes the guessed slots from the solve but still runs at the
//     terminal price the box derived from the FULL horizon. A box that truly
//     knew nothing past day-ahead would price stored energy off the known
//     window alone, so the sweep below also reports C at a terminal price
//     rederived from the known slots, using Core's own mode-dependent
//     formula.
//
// And the limit on the whole exercise: these snapshots carry no ground truth
// about what prices turned out to be. This bench can say whether the guess
// changes the action. It cannot say which action earned more money. That
// needs a closed-loop backtest against realised prices.
//
// Skipped without FTW_MPC_SNAPSHOT_DIR; the snapshot directory stays outside
// the repository because real blobs carry a household's load traces.

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

// benchNoTrustConfidence is the "trust nothing" confidence for variant B.
//
// It is not 0. Confidence ≤ 0 means "caller did not fill this in" to Core and
// is coerced to 1.0 — twice, once in sanitizeOptimizeSlots and again in
// Optimize's own defaulting loop — so asking for zero trust with a zero would
// silently ask for TOTAL trust, the exact opposite of the variant. The
// smallest value that survives both coercions is any positive float; 1e-9
// puts effPrice within ~1e-7 öre of the horizon mean, which is far below the
// öre the DP can act on.
const benchNoTrustConfidence = 1e-9

// benchActionDiffW is how far two plans' battery power must part before the
// slot counts as a disagreement. 50 W is well under the resolution any
// inverter tracks and well over DP grid residue.
const benchActionDiffW = 50.0

// benchLastKnownSlot reports the index of the last confidence-1.0 slot and
// whether the known slots form a contiguous prefix. A non-contiguous known
// window would make "truncate to what we know" ill-defined, so the caller
// says so rather than quietly truncating at a hole.
func benchLastKnownSlot(slots []Slot) (last int, contiguous bool, count int) {
	last = -1
	for i, s := range slots {
		if s.Confidence >= 1.0 {
			last = i
			count++
		}
	}
	if last < 0 {
		return -1, false, 0
	}
	return last, count == last+1, count
}

// benchFlattenForecast returns a copy of slots with every slot the box was
// unsure about pinned to no-trust confidence. Known slots are untouched.
func benchFlattenForecast(slots []Slot) []Slot {
	out := make([]Slot, len(slots))
	copy(out, slots)
	for i := range out {
		if out[i].Confidence < 1.0 {
			out[i].Confidence = benchNoTrustConfidence
		}
	}
	return out
}

// benchHoursTo is the wall-clock offset of slot index i, so a divergence
// index can be read as "the guess stops mattering after N hours".
func benchHoursTo(slots []Slot, i int) float64 {
	var min float64
	for k := 0; k < i && k < len(slots); k++ {
		min += float64(slots[k].LenMin)
	}
	return min / 60.0
}

// benchFirstDivergence finds the first slot where two plans' battery power
// parts by more than benchActionDiffW, comparing only as far as the shorter
// plan reaches. ok is false when they agree the whole way.
func benchFirstDivergence(a, b *Plan) (idx int, ok bool) {
	n := len(a.Actions)
	if len(b.Actions) < n {
		n = len(b.Actions)
	}
	for i := 0; i < n; i++ {
		if math.Abs(a.Actions[i].BatteryW-b.Actions[i].BatteryW) > benchActionDiffW {
			return i, true
		}
	}
	return n, false
}

// benchFirstSlotW is the only number in this file that reaches hardware.
func benchFirstSlotW(p *Plan) float64 {
	if p == nil || len(p.Actions) == 0 {
		return math.NaN()
	}
	return p.Actions[0].BatteryW
}

// benchKnownWindowCost costs a plan over the KNOWN slots only, under Core's
// model, terminal-corrected at the scoring price.
//
// All three variants must be scored on the same real-price slots. Costing A
// over 48 h of guessed prices and C over 13 h of real ones compares two
// different questions and would make the shorter horizon look cheap for no
// reason but being shorter.
func benchKnownWindowCost(plan *Plan, slots []Slot, n int, solveP, scoreP Params) (corrected, raw, endSoC float64, err error) {
	if plan == nil || len(plan.Actions) < n {
		return 0, 0, 0, fmt.Errorf("plan has %d actions, need %d", len(plan.Actions), n)
	}
	trunc := Plan{Actions: plan.Actions[:n]}
	eval, err := evaluatePlan(trunc, slots[:n], solveP)
	if err != nil {
		return 0, 0, 0, err
	}
	return terminalCorrectedOre(eval.CostOre, eval.EndSoC, scoreP), eval.CostOre, eval.EndSoC, nil
}

// benchTerminalPrice rederives the terminal credit from a given set of slots
// using Core's own mode-dependent formula (service.go), so "C, knowing
// nothing beyond day-ahead" can price stored energy off the known window
// instead of inheriting a number computed from the guess.
func benchTerminalPrice(mode Mode, slots []Slot) float64 {
	prices := make([]state.PricePoint, 0, len(slots))
	for _, s := range slots {
		prices = append(prices, state.PricePoint{
			SlotTsMs:    s.StartMs,
			SlotLenMin:  s.LenMin,
			SpotOreKwh:  s.SpotOre,
			TotalOreKwh: s.PriceOre,
		})
	}
	switch mode {
	case ModeSelfConsumption, ModeCheapCharge, ModePassiveArbitrage:
		return selfConsumptionTerminalPrice(prices, 0, 0)
	default:
		return upperHalfMeanPrice(prices)
	}
}

type forecastVariant struct {
	name      string
	plan      *Plan
	firstW    float64
	corrected float64
	raw       float64
	endSoC    float64
	costErr   error
	solveMs   int64
}

// TestForecastValueBench answers the owner's question with numbers: does the
// price twin change the watt dispatched now, and for how many hours does the
// guess keep mattering? Run with -v; the verdict is the table.
//
// Knobs:
//
//	FTW_MPC_SNAPSHOT_DIR      — directory of /api/mpc/diagnose blobs (required)
//	FTW_MPC_FORECAST_SOC      — SoC grid (default 201)
//	FTW_MPC_FORECAST_ACTIONS  — action grid (default 401)
//
// Both grids are forced so every variant answers on one resolution: a blob
// carries the recorded replan's grid, and the older blobs were recorded at a
// coarser one under a different planner.
func TestForecastValueBench(t *testing.T) {
	dir := os.Getenv("FTW_MPC_SNAPSHOT_DIR")
	if dir == "" {
		t.Skip("FTW_MPC_SNAPSHOT_DIR not set")
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no snapshots in %q (err=%v)", dir, err)
	}
	sort.Strings(paths)

	socLevels, actionLevels := 201, 401
	if v := os.Getenv("FTW_MPC_FORECAST_SOC"); v != "" {
		fmt.Sscanf(v, "%d", &socLevels)
	}
	if v := os.Getenv("FTW_MPC_FORECAST_ACTIONS"); v != "" {
		fmt.Sscanf(v, "%d", &actionLevels)
	}
	t.Logf("grid forced to SoCLevels=%d ActionLevels=%d for every variant", socLevels, actionLevels)
	t.Logf("A = as recorded; B = forecast slots flattened to the horizon mean (confidence %g);"+
		" C = forecast slots deleted, known window only", benchNoTrustConfidence)

	terminalScales := []float64{0.5, 1.0, 1.5}

	t.Logf("")
	t.Logf("=== decision table: the first slot is the only number that reaches hardware ===")
	t.Logf("%-22s %-18s %7s %9s %9s %9s %9s %9s %8s %8s",
		"snapshot", "mode", "known_h", "rec_w", "A_w", "B_w", "C_w", "B-A_w", "C-A_w", "A_soc0")

	type row struct {
		name                      string
		live                      bool
		mode                      Mode
		knownH                    float64
		knownN                    int
		recW, aW, bW, cW          float64
		divABIdx, divACIdx        int
		divABOK, divACOK          bool
		divABH, divACH            float64
		aCorr, bCorr, cCorr       float64
		aRaw, bRaw, cRaw          float64
		aSoC, bSoC, cSoC          float64
		terminalOre, knownTermOre float64
		knownMeanOre              float64
		sweepW                    map[float64]float64
		sweepCorr                 map[float64]float64
		sweepSoC                  map[float64]float64
		sweepDivIdx               int
		sweepDivOK                bool
		knownTermW, knownTermCorr float64
		knownTermSoC              float64
		diffSlotsAB, diffSlotsAC  int
		costable                  bool
	}
	var rows []row

	for _, path := range paths {
		name := filepath.Base(path)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		d, err := loadDiagnosticBlob(data)
		if err != nil {
			t.Logf("%-22s SKIP: %v", name, err)
			continue
		}
		recorded, slots, params, _, ok := planFromDiagnostic(d)
		if !ok {
			t.Logf("%-22s SKIP: not rehydratable", name)
			continue
		}
		params.SoCLevels = socLevels
		params.ActionLevels = actionLevels

		lastKnown, contiguous, knownCount := benchLastKnownSlot(slots)
		if lastKnown < 0 {
			t.Logf("%-22s SKIP: no confidence-1.0 slot, nothing is known", name)
			continue
		}
		if !contiguous {
			t.Logf("%-22s WARN: known slots are not a contiguous prefix (%d known, last at %d);"+
				" C truncates at the last one anyway", name, knownCount, lastKnown)
		}
		n := lastKnown + 1

		// Scoring price never moves: every variant, every terminal scale, is
		// costed with the terminal price the box actually derived.
		scoreParams := params

		r := row{
			name:        name,
			live:        name == "live-2026-08-31.json",
			mode:        params.Mode,
			knownN:      n,
			knownH:      benchHoursTo(slots, n),
			recW:        benchFirstSlotW(recorded),
			terminalOre: params.TerminalSoCPrice,
			sweepW:      map[float64]float64{},
			sweepCorr:   map[float64]float64{},
			sweepSoC:    map[float64]float64{},
			costable:    true,
		}
		{
			var sum, w float64
			for _, s := range slots[:n] {
				sum += s.PriceOre * float64(s.LenMin)
				w += float64(s.LenMin)
			}
			if w > 0 {
				r.knownMeanOre = sum / w
			}
		}

		solve := func(label string, in []Slot, p Params) forecastVariant {
			start := time.Now()
			plan := Optimize(in, p)
			ms := time.Since(start).Milliseconds()
			v := forecastVariant{name: label, plan: &plan, firstW: benchFirstSlotW(&plan), solveMs: ms}
			v.corrected, v.raw, v.endSoC, v.costErr = benchKnownWindowCost(&plan, slots, n, p, scoreParams)
			if v.costErr != nil {
				t.Logf("%-22s %s: known-window cost refused: %v", name, label, v.costErr)
			}
			return v
		}

		a := solve("A", slots, params)
		b := solve("B", benchFlattenForecast(slots), params)
		c := solve("C", slots[:n], params)

		r.aW, r.bW, r.cW = a.firstW, b.firstW, c.firstW
		r.aCorr, r.bCorr, r.cCorr = a.corrected, b.corrected, c.corrected
		r.aRaw, r.bRaw, r.cRaw = a.raw, b.raw, c.raw
		r.aSoC, r.bSoC, r.cSoC = a.endSoC, b.endSoC, c.endSoC
		if a.costErr != nil || b.costErr != nil || c.costErr != nil {
			r.costable = false
		}
		r.divABIdx, r.divABOK = benchFirstDivergence(a.plan, b.plan)
		r.divACIdx, r.divACOK = benchFirstDivergence(a.plan, c.plan)
		r.divABH = benchHoursTo(slots, r.divABIdx)
		r.divACH = benchHoursTo(slots, r.divACIdx)
		// How much of the KNOWN window the variants disagree about at all.
		// "The plans differ" and "the dispatched watt differs" are separate
		// facts, and only the second one reaches hardware.
		for i := 0; i < n; i++ {
			if math.Abs(a.plan.Actions[i].BatteryW-b.plan.Actions[i].BatteryW) > benchActionDiffW {
				r.diffSlotsAB++
			}
			if math.Abs(a.plan.Actions[i].BatteryW-c.plan.Actions[i].BatteryW) > benchActionDiffW {
				r.diffSlotsAC++
			}
		}

		// Terminal sweep on C. Short horizon plus a terminal price above the
		// window's own mean is the combination that would make C bank energy
		// it has no measured reason to bank, so the end-of-window SoC is
		// reported next to the first-slot watt: the terminal price can move
		// the plan without moving the decision.
		sweepPlans := map[float64]*Plan{}
		for _, scale := range terminalScales {
			ps := params
			ps.TerminalSoCPrice = params.TerminalSoCPrice * scale
			plan := Optimize(slots[:n], ps)
			sweepPlans[scale] = &plan
			r.sweepW[scale] = benchFirstSlotW(&plan)
			corr, _, endSoC, err := benchKnownWindowCost(&plan, slots, n, ps, scoreParams)
			if err == nil {
				r.sweepCorr[scale] = corr
				r.sweepSoC[scale] = endSoC
			} else {
				r.sweepCorr[scale] = math.NaN()
				r.sweepSoC[scale] = math.NaN()
			}
		}
		if lo, hi := sweepPlans[0.5], sweepPlans[1.5]; lo != nil && hi != nil {
			r.sweepDivIdx, r.sweepDivOK = benchFirstDivergence(lo, hi)
		}
		// And C as a box that knows nothing past day-ahead would really run
		// it: terminal price rederived from the known window alone.
		r.knownTermOre = benchTerminalPrice(params.Mode, slots[:n])
		pk := params
		pk.TerminalSoCPrice = r.knownTermOre
		planK := Optimize(slots[:n], pk)
		r.knownTermW = benchFirstSlotW(&planK)
		if corr, _, endSoC, err := benchKnownWindowCost(&planK, slots, n, pk, scoreParams); err == nil {
			r.knownTermCorr = corr
			r.knownTermSoC = endSoC
		} else {
			r.knownTermCorr = math.NaN()
			r.knownTermSoC = math.NaN()
		}

		rows = append(rows, r)
		t.Logf("%-22s %-18s %7.2f %9.1f %9.1f %9.1f %9.1f %9.1f %8.1f %8.4f",
			r.name, string(r.mode), r.knownH, r.recW, r.aW, r.bW, r.cW,
			r.bW-r.aW, r.cW-r.aW, params.InitialSoC)
	}
	if len(rows) == 0 {
		t.Fatal("no snapshot produced a comparison")
	}

	t.Logf("")
	t.Logf("=== how long the guess keeps mattering (first slot where |ΔbatteryW| > %.0f W) ===", benchActionDiffW)
	t.Logf("%-22s %10s %10s %10s %10s %12s %12s", "snapshot",
		"A_vs_B_idx", "A_vs_B_h", "A_vs_C_idx", "A_vs_C_h", "AB_diff/known", "AC_diff/known")
	for _, r := range rows {
		ab, ac := fmt.Sprintf("%d", r.divABIdx), fmt.Sprintf("%d", r.divACIdx)
		abh, ach := fmt.Sprintf("%.2f", r.divABH), fmt.Sprintf("%.2f", r.divACH)
		if !r.divABOK {
			ab, abh = "none", ">"+fmt.Sprintf("%.2f", r.divABH)
		}
		if !r.divACOK {
			ac, ach = "none", ">"+fmt.Sprintf("%.2f", r.divACH)
		}
		t.Logf("%-22s %10s %10s %10s %10s %12s %12s", r.name, ab, abh, ac, ach,
			fmt.Sprintf("%d/%d", r.diffSlotsAB, r.knownN),
			fmt.Sprintf("%d/%d", r.diffSlotsAC, r.knownN))
	}

	t.Logf("")
	t.Logf("=== cost over the KNOWN window only (%s), öre, terminal-corrected at the recorded price ===",
		"real day-ahead slots, identical for all three")
	t.Logf("%-22s %6s %10s %10s %10s %9s %9s %8s %8s %8s",
		"snapshot", "known_n", "A_corr", "B_corr", "C_corr", "B-A", "C-A", "A_soc", "B_soc", "C_soc")
	for _, r := range rows {
		if !r.costable {
			t.Logf("%-22s %6d  REFUSED", r.name, r.knownN)
			continue
		}
		t.Logf("%-22s %6d %10.1f %10.1f %10.1f %9.1f %9.1f %8.4f %8.4f %8.4f",
			r.name, r.knownN, r.aCorr, r.bCorr, r.cCorr,
			r.bCorr-r.aCorr, r.cCorr-r.aCorr, r.aSoC, r.bSoC, r.cSoC)
	}

	t.Logf("")
	t.Logf("=== C's dependence on the terminal price ===")
	t.Logf("term_ore is the price the box derived from the FULL horizon; known_mean is the known window's own mean.")
	t.Logf("knownT_* rederives the terminal price from the known slots alone, with Core's mode-dependent formula.")
	t.Logf("%-22s %9s %9s %9s %9s %9s %9s %9s %9s %9s %8s",
		"snapshot", "term_ore", "known_mean", "C@0.5_w", "C@1.0_w", "C@1.5_w",
		"knownT_ore", "knownT_w", "A_w", "sweep_div", "C_w_sprd")
	for _, r := range rows {
		div := fmt.Sprintf("%d", r.sweepDivIdx)
		if !r.sweepDivOK {
			div = "none"
		}
		t.Logf("%-22s %9.1f %9.1f %9.1f %9.1f %9.1f %9.1f %9.1f %9.1f %9s %8.1f",
			r.name, r.terminalOre, r.knownMeanOre,
			r.sweepW[0.5], r.sweepW[1.0], r.sweepW[1.5], r.knownTermOre, r.knownTermW, r.aW,
			div, math.Abs(r.sweepW[1.5]-r.sweepW[0.5]))
	}
	t.Logf("--- and what the terminal price DOES move: SoC parked at the end of the known window ---")
	t.Logf("%-22s %9s %9s %9s %9s %11s %11s %11s",
		"snapshot", "C@0.5_soc", "C@1.0_soc", "C@1.5_soc", "knownT_soc", "C@0.5_ore", "C@1.0_ore", "C@1.5_ore")
	for _, r := range rows {
		t.Logf("%-22s %9.4f %9.4f %9.4f %9.4f %11.1f %11.1f %11.1f",
			r.name, r.sweepSoC[0.5], r.sweepSoC[1.0], r.sweepSoC[1.5], r.knownTermSoC,
			r.sweepCorr[0.5], r.sweepCorr[1.0], r.sweepCorr[1.5])
	}

	summarize := func(label string, sel func(row) bool) {
		var n, sameAB, sameAC, exactAB, exactAC int
		var sumAbsAB, sumAbsAC, maxAbsAB, maxAbsAC float64
		var sumCostAB, sumCostAC float64
		var sumDivAB, sumDivAC float64
		var sumDiffAB, sumDiffAC, sumKnownN float64
		var sumSoCAC, sumSweepSoC float64
		var sweepSpread float64
		for _, r := range rows {
			if !sel(r) {
				continue
			}
			n++
			dAB, dAC := math.Abs(r.bW-r.aW), math.Abs(r.cW-r.aW)
			sumAbsAB += dAB
			sumAbsAC += dAC
			maxAbsAB = math.Max(maxAbsAB, dAB)
			maxAbsAC = math.Max(maxAbsAC, dAC)
			if dAB <= benchActionDiffW {
				sameAB++
			}
			if dAC <= benchActionDiffW {
				sameAC++
			}
			if dAB < 1 {
				exactAB++
			}
			if dAC < 1 {
				exactAC++
			}
			sumCostAB += r.bCorr - r.aCorr
			sumCostAC += r.cCorr - r.aCorr
			sumDivAB += r.divABH
			sumDivAC += r.divACH
			sumDiffAB += float64(r.diffSlotsAB)
			sumDiffAC += float64(r.diffSlotsAC)
			sumKnownN += float64(r.knownN)
			sumSoCAC += r.cSoC - r.aSoC
			sumSweepSoC += math.Abs(r.sweepSoC[1.5] - r.sweepSoC[0.5])
			lo, hi := r.sweepW[0.5], r.sweepW[1.5]
			sweepSpread = math.Max(sweepSpread, math.Abs(hi-lo))
		}
		if n == 0 {
			return
		}
		t.Logf("")
		t.Logf("SUMMARY %s (n=%d)", label, n)
		t.Logf("  first-slot W, B vs A: identical(<1W) %d/%d, within %.0f W %d/%d, mean |Δ| %.1f W, max |Δ| %.1f W",
			exactAB, n, benchActionDiffW, sameAB, n, sumAbsAB/float64(n), maxAbsAB)
		t.Logf("  first-slot W, C vs A: identical(<1W) %d/%d, within %.0f W %d/%d, mean |Δ| %.1f W, max |Δ| %.1f W",
			exactAC, n, benchActionDiffW, sameAC, n, sumAbsAC/float64(n), maxAbsAC)
		t.Logf("  mean hours before the plans part: A/B %.2f h, A/C %.2f h", sumDivAB/float64(n), sumDivAC/float64(n))
		t.Logf("  slots of the known window the plans disagree about: A/B %.1f of %.1f, A/C %.1f of %.1f",
			sumDiffAB/float64(n), sumKnownN/float64(n), sumDiffAC/float64(n), sumKnownN/float64(n))
		t.Logf("  known-window cost, mean B−A %+.1f öre, mean C−A %+.1f öre (positive = the variant costs more)",
			sumCostAB/float64(n), sumCostAC/float64(n))
		t.Logf("  SoC parked at the end of the known window, mean C−A %+.4f (%+.0f Wh on a 9.6 kWh pack)",
			sumSoCAC/float64(n), sumSoCAC/float64(n)*9600)
		t.Logf("  C first-slot W across terminal scales 0.5…1.5: worst spread %.1f W;"+
			" mean end-of-window SoC spread %.4f", sweepSpread, sumSweepSoC/float64(n))
	}
	summarize("all snapshots", func(r row) bool { return true })
	summarize("live snapshot only (today's Core)", func(r row) bool { return r.live })
	summarize("older Python-era snapshots only", func(r row) bool { return !r.live })
	t.Logf("")
	t.Logf("NOTE: no snapshot carries realised prices, so nothing above says which variant EARNS more." +
		" It says only whether the guess changes the action.")
}

// benchRelabelKnown returns a copy of slots where the first k count as real
// day-ahead and everything after is marked forecast at forecastConf. The
// PRICES do not move — only how far the box is told to trust them.
//
// That is the point: it isolates the confidence mechanism from forecast
// error. Whatever the twin would have got wrong is held at zero here, so any
// difference the sweep finds is caused by the blending rule alone, and a real
// twin with real error can only differ by more.
func benchRelabelKnown(slots []Slot, k int, forecastConf float64) []Slot {
	out := make([]Slot, len(slots))
	copy(out, slots)
	for i := range out {
		if i < k {
			out[i].Confidence = 1.0
		} else {
			out[i].Confidence = forecastConf
		}
	}
	return out
}

// benchForecastConfidence is the confidence the box actually stamped on its
// guessed slots — 0.6 on these sites. Falls back to 0.6 when a snapshot has
// no forecast row to read it from.
func benchForecastConfidence(slots []Slot) float64 {
	for _, s := range slots {
		if s.Confidence > 0 && s.Confidence < 1.0 {
			return s.Confidence
		}
	}
	return 0.6
}

// TestForecastValueKnownWindowSweep asks the same question in the regime
// where the guess has the best chance of mattering: a SHORT known window.
//
// Every snapshot here happens to carry 11.75–13.5 h of real day-ahead price,
// which is the comfortable end of the daily cycle. Just before publication a
// box can be down to a few hours. The sweep shortens the known window by
// hand — 2, 4, 6, 8, 12 h and as recorded — and re-asks whether A (trust the
// guess as the box does), B (flatten it) and C (delete it) dispatch the same
// watt.
//
// Prices are left at their recorded values throughout, so this measures the
// blending rule, not the twin's accuracy. Skipped without
// FTW_MPC_SNAPSHOT_DIR.
func TestForecastValueKnownWindowSweep(t *testing.T) {
	dir := os.Getenv("FTW_MPC_SNAPSHOT_DIR")
	if dir == "" {
		t.Skip("FTW_MPC_SNAPSHOT_DIR not set")
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no snapshots in %q (err=%v)", dir, err)
	}
	sort.Strings(paths)

	socLevels, actionLevels := 201, 401
	if v := os.Getenv("FTW_MPC_FORECAST_SOC"); v != "" {
		fmt.Sscanf(v, "%d", &socLevels)
	}
	if v := os.Getenv("FTW_MPC_FORECAST_ACTIONS"); v != "" {
		fmt.Sscanf(v, "%d", &actionLevels)
	}

	// 15-minute slots on every snapshot here, so k is hours × 4.
	windows := []int{8, 16, 24, 32, 48}
	t.Logf("grid forced to SoCLevels=%d ActionLevels=%d", socLevels, actionLevels)
	t.Logf("prices are unchanged at every k — only the trust label moves, so this isolates")
	t.Logf("the confidence rule from forecast error")
	t.Logf("%-22s %6s %7s %9s %9s %9s %9s %8s", "snapshot", "known_k", "known_h", "A_w", "B_w", "C_w", "B-A_w", "C-A_w")

	agree := map[int][2]int{}
	total := map[int]int{}
	for _, path := range paths {
		name := filepath.Base(path)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		d, err := loadDiagnosticBlob(data)
		if err != nil {
			continue
		}
		_, slots, params, _, ok := planFromDiagnostic(d)
		if !ok {
			continue
		}
		params.SoCLevels = socLevels
		params.ActionLevels = actionLevels
		fConf := benchForecastConfidence(slots)
		recordedK, _, _ := benchLastKnownSlot(slots)
		ks := append([]int{}, windows...)
		if !slices.Contains(ks, recordedK+1) {
			ks = append(ks, recordedK+1)
		}
		for _, k := range ks {
			if k < 2 || k > len(slots) {
				continue
			}
			aPlan := Optimize(benchRelabelKnown(slots, k, fConf), params)
			bPlan := Optimize(benchRelabelKnown(slots, k, benchNoTrustConfidence), params)
			cPlan := Optimize(slots[:k], params)
			aW, bW, cW := benchFirstSlotW(&aPlan), benchFirstSlotW(&bPlan), benchFirstSlotW(&cPlan)
			label := name
			if k == recordedK+1 {
				label = name + " (as recorded)"
			}
			t.Logf("%-22s %6d %7.2f %9.1f %9.1f %9.1f %9.1f %8.1f",
				label, k, benchHoursTo(slots, k), aW, bW, cW, bW-aW, cW-aW)
			counts := agree[k]
			if math.Abs(bW-aW) <= benchActionDiffW {
				counts[0]++
			}
			if math.Abs(cW-aW) <= benchActionDiffW {
				counts[1]++
			}
			agree[k] = counts
			total[k]++
		}
	}
	ks := make([]int, 0, len(total))
	for k := range total {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	t.Logf("")
	t.Logf("SUMMARY first-slot agreement within %.0f W, by assumed known window", benchActionDiffW)
	t.Logf("%6s %7s %14s %14s", "known_k", "known_h", "B_matches_A", "C_matches_A")
	for _, k := range ks {
		t.Logf("%6d %7.2f %14s %14s", k, float64(k)*15/60,
			fmt.Sprintf("%d/%d", agree[k][0], total[k]),
			fmt.Sprintf("%d/%d", agree[k][1], total[k]))
	}
}

// ---- CI-runnable fixture tests (no external data) ----

// TestForecastBenchZeroConfidenceMeansTotalTrust pins the trap the bench's
// epsilon exists for: asking for zero confidence gets full confidence, so a
// "trust nothing" variant written with Confidence = 0 would silently measure
// the opposite of what it claims.
func TestForecastBenchZeroConfidenceMeansTotalTrust(t *testing.T) {
	got := sanitizeOptimizeSlots([]Slot{{LenMin: 15, PriceOre: 100, Confidence: 0}})
	if len(got) != 1 || got[0].Confidence != 1.0 {
		t.Fatalf("Confidence 0 sanitized to %v, want 1.0 (the coercion the bench works around)", got)
	}
	eps := sanitizeOptimizeSlots([]Slot{{LenMin: 15, PriceOre: 100, Confidence: benchNoTrustConfidence}})
	if len(eps) != 1 || eps[0].Confidence != benchNoTrustConfidence {
		t.Fatalf("epsilon confidence did not survive sanitisation: %v", eps)
	}
}

// TestForecastBenchFlattenLeavesKnownSlotsAlone pins that variant B touches
// only the slots the box was unsure about.
func TestForecastBenchFlattenLeavesKnownSlotsAlone(t *testing.T) {
	in := []Slot{
		{LenMin: 15, PriceOre: 100, Confidence: 1.0},
		{LenMin: 15, PriceOre: 300, Confidence: 0.6},
		{LenMin: 15, PriceOre: 200, Confidence: 1.0},
	}
	out := benchFlattenForecast(in)
	if out[0].Confidence != 1.0 || out[2].Confidence != 1.0 {
		t.Fatalf("known slots were flattened: %v", out)
	}
	if out[1].Confidence != benchNoTrustConfidence {
		t.Fatalf("forecast slot not flattened: %v", out[1])
	}
	if in[1].Confidence != 0.6 {
		t.Fatal("benchFlattenForecast mutated the caller's slice")
	}
}

// TestForecastBenchRelabelKnownMovesTrustNotPrice pins that the known-window
// sweep shortens what the box is told it knows without touching a single
// price — the property that lets the sweep isolate the confidence rule from
// the twin's accuracy.
func TestForecastBenchRelabelKnownMovesTrustNotPrice(t *testing.T) {
	in := []Slot{
		{LenMin: 15, PriceOre: 100, Confidence: 1.0},
		{LenMin: 15, PriceOre: 200, Confidence: 1.0},
		{LenMin: 15, PriceOre: 300, Confidence: 1.0},
	}
	out := benchRelabelKnown(in, 1, 0.6)
	for i := range out {
		if out[i].PriceOre != in[i].PriceOre {
			t.Fatalf("slot %d price moved: %v -> %v", i, in[i].PriceOre, out[i].PriceOre)
		}
	}
	if out[0].Confidence != 1.0 || out[1].Confidence != 0.6 || out[2].Confidence != 0.6 {
		t.Fatalf("trust labels wrong: %v", []float64{out[0].Confidence, out[1].Confidence, out[2].Confidence})
	}
	if in[1].Confidence != 1.0 {
		t.Fatal("benchRelabelKnown mutated the caller's slice")
	}
	if got := benchForecastConfidence(out); got != 0.6 {
		t.Fatalf("forecast confidence read back as %v, want 0.6", got)
	}
}

// TestForecastBenchKnownWindow pins the prefix detection and the hours
// conversion that turn a slot index into "the guess stops mattering after N
// hours".
func TestForecastBenchKnownWindow(t *testing.T) {
	slots := []Slot{
		{LenMin: 15, Confidence: 1.0},
		{LenMin: 15, Confidence: 1.0},
		{LenMin: 15, Confidence: 0.6},
		{LenMin: 15, Confidence: 0.6},
	}
	last, contiguous, count := benchLastKnownSlot(slots)
	if last != 1 || !contiguous || count != 2 {
		t.Fatalf("last=%d contiguous=%v count=%d, want 1/true/2", last, contiguous, count)
	}
	if h := benchHoursTo(slots, 2); h != 0.5 {
		t.Fatalf("hours to slot 2 = %v, want 0.5", h)
	}
	slots[3].Confidence = 1.0
	if _, contiguous, _ := benchLastKnownSlot(slots); contiguous {
		t.Fatal("a hole in the known window must not report contiguous")
	}
}

// TestForecastBenchFlatForecastRemovesArbitrageBeyondKnown is the property
// the whole bench rests on: flattening a forecast slot's confidence removes
// the price SHAPE the DP could arbitrage against, so a plan that only charges
// because of a guessed evening peak stops charging.
func TestForecastBenchFlatForecastRemovesArbitrageBeyondKnown(t *testing.T) {
	// Known window: four flat, unremarkable slots. Guessed window: a deep
	// cheap trough then an expensive peak — pure invented arbitrage.
	slots := make([]Slot, 12)
	for i := range slots {
		price := 200.0
		conf := 1.0
		switch {
		case i >= 4 && i < 8:
			price, conf = 20, 0.6
		case i >= 8:
			price, conf = 600, 0.6
		}
		slots[i] = Slot{
			StartMs: 1_700_000_000_000 + int64(i)*3_600_000, LenMin: 60,
			PriceOre: price, SpotOre: price / 2, Confidence: conf, LoadW: 1000,
		}
	}
	p := Params{
		Mode: ModeArbitrage, InitialSoC: 0.5, SoCMin: 0.1, SoCMax: 1.0,
		SoCLevels: 41, ActionLevels: 41, MaxChargeW: 3000, MaxDischargeW: 3000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95, CapacityWh: 9600,
		TerminalSoCPrice: 200,
	}
	trusted := Optimize(slots, p)
	flat := Optimize(benchFlattenForecast(slots), p)
	// Under the guess, the cheap trough is worth charging into; flattened,
	// those four slots all price at the horizon mean and there is nothing to
	// arbitrage, so the trough charging must weaken.
	var trustedTrough, flatTrough float64
	for i := 4; i < 8; i++ {
		trustedTrough += trusted.Actions[i].BatteryW
		flatTrough += flat.Actions[i].BatteryW
	}
	if trustedTrough <= flatTrough {
		t.Fatalf("flattening did not remove the invented arbitrage: trusted charges %.0f W, flat %.0f W",
			trustedTrough, flatTrough)
	}
}
