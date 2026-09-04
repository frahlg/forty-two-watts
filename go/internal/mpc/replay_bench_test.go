package mpc

// Replay bench (#1020): re-solve recorded /api/mpc/diagnose/at blobs so
// solver claims are measured, not argued. Point FTW_MPC_SNAPSHOT_DIR at
// a directory of downloaded blobs and the bench re-solves each with the
// Go DP — and, when FTW_TEST_OPTIMIZER_PYTHON is also set, with the
// Python champion — on IDENTICAL inputs, reporting terminal-corrected
// cost: raw grid cost minus the terminal-SoC credit both solvers
// optimize with but neither reports. Without that correction a solver
// that parks the horizon with a fuller battery looks expensive when it
// is merely storing value (up to ~21 SEK swing on a 9.6 kWh pack) —
// the stateful shadow evaluator already nets this out; the DP shadow
// comparisons do not, which is how a measurement artifact once read as
// a 39 SEK/day solver gap.
//
// The snapshot directory stays outside the repository on purpose: real
// blobs carry a household's load traces. CI runs only the synthetic
// fixture tests below.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"testing"
	"time"
)

// loadDiagnosticBlob accepts every shape the API hands out: a raw
// Diagnostic, {"diagnostic": {...}}, or the /diagnose/at envelope
// {"snapshot": {"diagnostic": {...}}}.
func loadDiagnosticBlob(data []byte) (*Diagnostic, error) {
	var envelope struct {
		Snapshot *struct {
			Diagnostic *Diagnostic `json:"diagnostic"`
		} `json:"snapshot"`
		Diagnostic *Diagnostic `json:"diagnostic"`
	}
	if err := json.Unmarshal(data, &envelope); err == nil {
		if envelope.Snapshot != nil && envelope.Snapshot.Diagnostic != nil {
			return envelope.Snapshot.Diagnostic, nil
		}
		if envelope.Diagnostic != nil {
			return envelope.Diagnostic, nil
		}
	}
	var d Diagnostic
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	if len(d.Slots) == 0 {
		return nil, fmt.Errorf("no slots in blob")
	}
	return &d, nil
}

// terminalCorrectedOre and planEndSoC now live in mpc.go: the Python
// field shadow reports the same correction every replan, so the bench
// and the running planner must not drift apart on the formula.

// benchEvaluate costs one plan under Core's model. A plan Core cannot cost
// fails the bench outright rather than being silently replaced by the number
// it came with — the same rule the field shadow follows. The caller
// terminal-corrects; the raw cost and the reached end SoC are reported
// alongside so a reader can see whether an advantage is cheaper decisions or
// merely more energy left in the pack.
func benchEvaluate(t *testing.T, leg, snapshot string, plan *Plan, slots []Slot, p Params) planEvaluation {
	t.Helper()
	eval, err := evaluatePlan(*plan, slots, p)
	if err != nil {
		t.Fatalf("%s: core cannot cost the %s plan: %v", snapshot, leg, err)
	}
	return eval
}

// TestReplayBenchSnapshots is the A/B instrument. Skipped without
// FTW_MPC_SNAPSHOT_DIR. Optional knobs:
//
//	FTW_TEST_OPTIMIZER_PYTHON  — adds the Python challenger leg
//	FTW_MPC_BENCH_SPREAD_ORE   — MinArbitrageSpreadOreKwh fallback for
//	                             blobs written before the diagnostic
//	                             persisted it. A spread carried by the
//	                             snapshot always wins: it is what the
//	                             replan actually solved under.
//	FTW_MPC_BENCH_CVAR_WEIGHT  — the site's optimizer.cvar_weight
//	FTW_MPC_BENCH_CVAR_ALPHA   — the site's optimizer.cvar_alpha
//	FTW_MPC_BENCH_MIP_GAP      — the site's optimizer.mip_rel_gap
//	FTW_MPC_BENCH_TERMINAL_SCALE — multiplier on the snapshot's
//	                             TerminalSoCPrice: 1.0 as recorded, 0 for no
//	                             terminal credit at all.
//
// The three optimizer knobs exist because the diagnostic records the
// PLANNING inputs, not the challenger's solver configuration: replaying a
// box's snapshot with the bench's defaults asks Python a different question
// than the box asked it, and the answer differs by more than the gap being
// measured. Set them to the site's values before reading a py column as "what
// that box's shadow would have said".
//
// The terminal knob answers a different question: whether a measured gap is a
// gap between plans or an artifact of how stored energy is priced. The
// terminal price is both an input the solvers optimize against and the rate
// the comparison credits leftover charge at, so the scale moves both together
// — every leg still solves and is scored under one consistent price of stored
// energy. If a challenger's lead melts as the scale goes to 0, the lead was
// bought with end-of-horizon SoC, not with cheaper decisions.
//
// py_self is what Python reported for its own plan; py_corr is what that same
// plan costs under Core's model, terminal-corrected. Only py_corr may be
// subtracted from dp_corr. The *_raw columns are the same plans before the
// correction and *_soc the SoC each parks the horizon at, which is what makes
// the difference between the two readable.
//
// Run with -v; the verdict is the table, not a pass/fail.
func TestReplayBenchSnapshots(t *testing.T) {
	dir := os.Getenv("FTW_MPC_SNAPSHOT_DIR")
	if dir == "" {
		t.Skip("FTW_MPC_SNAPSHOT_DIR not set")
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no snapshots in %q (err=%v)", dir, err)
	}
	sort.Strings(paths)

	fallbackSpreadOre := 0.0
	if v := os.Getenv("FTW_MPC_BENCH_SPREAD_ORE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			fallbackSpreadOre = f
		}
	}

	benchFloat := func(key string, fallback float64) float64 {
		if v := os.Getenv(key); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f
			}
		}
		return fallback
	}

	var ext *ExternalOptimizer
	if python := os.Getenv("FTW_TEST_OPTIMIZER_PYTHON"); python != "" {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			t.Fatal("runtime.Caller failed")
		}
		moduleDir := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "optimizer"))
		cvarWeight := benchFloat("FTW_MPC_BENCH_CVAR_WEIGHT", 0)
		cvarAlpha := benchFloat("FTW_MPC_BENCH_CVAR_ALPHA", 0.9)
		mipGap := benchFloat("FTW_MPC_BENCH_MIP_GAP", 0.001)
		t.Logf("python leg: cvar_weight=%g cvar_alpha=%g mip_rel_gap=%g", cvarWeight, cvarAlpha, mipGap)
		ext, err = NewExternalOptimizer(ExternalOptimizerConfig{
			Command:     []string{python, "-m", "ftw_optimizer.worker"},
			ModuleDir:   moduleDir,
			Timeout:     60 * time.Second,
			Solver:      "HIGHS",
			Formulation: "auto",
			MIPRelGap:   mipGap,
			CVaRWeight:  cvarWeight,
			CVaRAlpha:   cvarAlpha,
			IdleTimeout: 5 * time.Second,
		})
		if err != nil {
			t.Fatalf("external optimizer: %v", err)
		}
		defer ext.Close()
	}

	terminalScale := benchFloat("FTW_MPC_BENCH_TERMINAL_SCALE", 1)
	if terminalScale != 1 {
		t.Logf("terminal price scaled by %g (solve and scoring alike)", terminalScale)
	}

	t.Logf("%-15s %-18s %9s %9s %6s %9s %9s %9s %9s %6s %9s %9s %9s %8s",
		"snapshot", "mode", "rec_corr",
		"dp_raw", "dp_soc", "dp_corr", "dp-rec",
		"py_self", "py_raw", "py_soc", "py_corr", "py-dp", "py-dp_raw", "dp_ms")
	var sumDPvsRec, sumPYvsDP, sumPYvsDPRaw, sumSoCDiff float64
	var nDP, nPY int
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		d, err := loadDiagnosticBlob(data)
		if err != nil {
			t.Logf("%-15s SKIP: %v", filepath.Base(path), err)
			continue
		}
		recorded, slots, params, _, ok := planFromDiagnostic(d)
		if !ok {
			t.Logf("%-15s SKIP: not rehydratable", filepath.Base(path))
			continue
		}
		if params.MinArbitrageSpreadOreKwh == 0 {
			params.MinArbitrageSpreadOreKwh = fallbackSpreadOre
		}
		// Applied before anything solves, so the DP, Python and the
		// correction all price stored energy the same way.
		params.TerminalSoCPrice *= terminalScale
		// A blob carries the RECORDED replan's grid resolution, so a
		// default bump would be invisible here without an override —
		// judging a resolution change is exactly what the knobs exist
		// for (FTW_MPC_BENCH_SOC_LEVELS / FTW_MPC_BENCH_ACTION_LEVELS).
		if v := os.Getenv("FTW_MPC_BENCH_SOC_LEVELS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 3 {
				params.SoCLevels = n
			}
		}
		if v := os.Getenv("FTW_MPC_BENCH_ACTION_LEVELS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 3 {
				params.ActionLevels = n
			}
		}

		// Every leg is costed by Core, on Core's terms. A solver's own
		// total is its objective's opinion of its own plan; subtracting
		// one solver's objective from another's measures the objectives,
		// not the plans (#1020 follow-up).
		recEval := benchEvaluate(t, "recorded", filepath.Base(path), recorded, slots, params)
		recCorr := terminalCorrectedOre(recEval.CostOre, recEval.EndSoC, params)

		dpStart := time.Now()
		dpPlan := Optimize(slots, params)
		dpMs := time.Since(dpStart).Milliseconds()
		dpEval := benchEvaluate(t, "dp", filepath.Base(path), &dpPlan, slots, params)
		dpCorr := terminalCorrectedOre(dpEval.CostOre, dpEval.EndSoC, params)
		sumDPvsRec += dpCorr - recCorr
		nDP++

		pySelf, pyRaw, pySoC, pyCol, pyDelta, pyDeltaRaw := "-", "-", "-", "-", "-", "-"
		if ext != nil {
			pyPlan, err := ext.Optimize(t.Context(), slots, params)
			switch {
			case err != nil:
				pyCol = "ERR"
				t.Logf("%-15s python: %v", filepath.Base(path), err)
			default:
				pySelf = fmt.Sprintf("%9.1f",
					terminalCorrectedOre(pyPlan.TotalCostOre, planEndSoC(&pyPlan), params))
				pyEval, evalErr := evaluatePlan(pyPlan, slots, params)
				if evalErr != nil {
					pyCol = "REFUSED"
					t.Logf("%-15s python plan not costable by core: %v", filepath.Base(path), evalErr)
					break
				}
				pyCorr := terminalCorrectedOre(pyEval.CostOre, pyEval.EndSoC, params)
				pyRaw = fmt.Sprintf("%9.1f", pyEval.CostOre)
				pySoC = fmt.Sprintf("%6.3f", pyEval.EndSoC)
				pyCol = fmt.Sprintf("%9.1f", pyCorr)
				pyDelta = fmt.Sprintf("%9.1f", pyCorr-dpCorr)
				pyDeltaRaw = fmt.Sprintf("%9.1f", pyEval.CostOre-dpEval.CostOre)
				sumPYvsDP += pyCorr - dpCorr
				sumPYvsDPRaw += pyEval.CostOre - dpEval.CostOre
				sumSoCDiff += pyEval.EndSoC - dpEval.EndSoC
				nPY++
			}
		}
		t.Logf("%-15s %-18s %9.1f %9.1f %6.3f %9.1f %9.1f %9s %9s %6s %9s %9s %9s %6dms",
			filepath.Base(path), string(params.Mode), recCorr,
			dpEval.CostOre, dpEval.EndSoC, dpCorr, dpCorr-recCorr,
			pySelf, pyRaw, pySoC, pyCol, pyDelta, pyDeltaRaw, dpMs)
	}
	if nDP > 0 {
		t.Logf("SUMMARY dp_vs_recorded: n=%d mean=%.1f öre/plan", nDP, sumDPvsRec/float64(nDP))
	}
	if nPY > 0 {
		t.Logf("SUMMARY python_vs_dp (both costed by core, terminal-corrected, positive = python costs more): n=%d mean=%.1f öre/plan", nPY, sumPYvsDP/float64(nPY))
		t.Logf("SUMMARY python_vs_dp_raw (no terminal correction): n=%d mean=%.1f öre/plan; mean end_soc diff (py-dp) = %+.4f (%+.1f Wh)",
			nPY, sumPYvsDPRaw/float64(nPY), sumSoCDiff/float64(nPY), sumSoCDiff/float64(nPY)*9600)
	}
}

// ---- CI-runnable fixture tests (no external data, no Python) ----

func benchFixtureDiagnostic() *Diagnostic {
	slots := make([]DiagnosticSlot, 8)
	for i := range slots {
		price := 100.0
		if i >= 4 {
			price = 300.0 // expensive evening: charge early, discharge late
		}
		slots[i] = DiagnosticSlot{
			Idx:         i,
			SlotStartMs: 1_700_000_000_000 + int64(i)*900_000,
			SlotEndMs:   1_700_000_000_000 + int64(i+1)*900_000,
			LenMin:      15,
			PriceOre:    price,
			SpotOre:     price / 2,
			Confidence:  1,
			LoadW:       1000,
			BatteryW:    0,
			GridW:       1000,
			SoC:         0.5,
			CostOre:     100 * (1000 * 0.25) / 1000,
		}
	}
	return &Diagnostic{
		ComputedAtMs:   1_700_000_000_000,
		LastReplanAtMs: 1_700_000_000_000,
		Horizon:        len(slots),
		Slots:          slots,
		TotalCostOre:   8 * 25, // filled loosely; loader does not re-derive
		Params: DiagnosticParams{
			Mode:                ModeArbitrage,
			InitialSoC:          0.5,
			SoCMin:              0.1,
			SoCMax:              1.0,
			SoCLevels:           21,
			ActionLevels:        41,
			MaxChargeW:          3000,
			MaxDischargeW:       3000,
			ChargeEfficiency:    0.95,
			DischargeEfficiency: 0.95,
			CapacityWh:          9600,
			TerminalSoCPrice:    200,
		},
	}
}

// TestReplayBenchLoaderAcceptsEveryEnvelope pins that the loader reads
// the raw Diagnostic, the {"diagnostic": ...} wrapper, and the
// /diagnose/at {"snapshot": {"diagnostic": ...}} envelope identically.
func TestReplayBenchLoaderAcceptsEveryEnvelope(t *testing.T) {
	d := benchFixtureDiagnostic()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := []byte(`{"diagnostic":` + string(raw) + `}`)
	envelope := []byte(`{"enabled":true,"snapshot":{"ts_ms":1,"reason":"x","diagnostic":` + string(raw) + `}}`)
	for name, blob := range map[string][]byte{"raw": raw, "wrapped": wrapped, "envelope": envelope} {
		got, err := loadDiagnosticBlob(blob)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got.Slots) != len(d.Slots) || got.Params.CapacityWh != d.Params.CapacityWh {
			t.Fatalf("%s: lossy load: %d slots, capacity %v", name, len(got.Slots), got.Params.CapacityWh)
		}
	}
}

// TestReplayBenchTerminalCorrection pins the correction formula and the
// property it exists for: two plans with identical raw cost but
// different end SoC must NOT compare equal — the fuller one is cheaper
// once its stored value is counted.
func TestReplayBenchTerminalCorrection(t *testing.T) {
	p := Params{TerminalSoCPrice: 200, CapacityWh: 10000}
	// 200 öre/kWh × (0.8−0.3)×10 kWh = 1000 öre difference.
	fuller := terminalCorrectedOre(5000, 0.8, p)
	emptier := terminalCorrectedOre(5000, 0.3, p)
	if diff := emptier - fuller; diff != 1000 {
		t.Fatalf("terminal correction: want 1000 öre difference, got %v", diff)
	}
	if fuller != 5000-200*8 {
		t.Fatalf("corrected = %v, want %v", fuller, 5000-200*8.0)
	}
}

// TestReplayBenchRehydratedSolveIsDeterministic pins that a rehydrated
// snapshot re-solves to the identical plan twice — the property that
// makes the bench a regression instrument rather than a dice roll.
func TestReplayBenchRehydratedSolveIsDeterministic(t *testing.T) {
	d := benchFixtureDiagnostic()
	_, slots, params, _, ok := planFromDiagnostic(d)
	if !ok {
		t.Fatal("fixture not rehydratable")
	}
	a := Optimize(slots, params)
	b := Optimize(slots, params)
	if a.TotalCostOre != b.TotalCostOre || len(a.Actions) != len(b.Actions) {
		t.Fatalf("non-deterministic: %v vs %v", a.TotalCostOre, b.TotalCostOre)
	}
	for i := range a.Actions {
		if a.Actions[i].BatteryW != b.Actions[i].BatteryW {
			t.Fatalf("slot %d: %v vs %v", i, a.Actions[i].BatteryW, b.Actions[i].BatteryW)
		}
	}
	// And the cheap-then-expensive fixture must actually arbitrage:
	// charge in the first half, discharge in the second.
	var early, late float64
	for i, act := range a.Actions {
		if i < 4 {
			early += act.BatteryW
		} else {
			late += act.BatteryW
		}
	}
	if early <= 0 || late >= 0 {
		t.Fatalf("fixture should charge early (%v) and discharge late (%v)", early, late)
	}
}
