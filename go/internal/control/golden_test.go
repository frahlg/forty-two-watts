package control

// Golden replay of ComputeDispatch.
//
// testdata/golden/ holds 435 recorded dispatch ticks: seven families covering
// the reactive modes, all four planner modes, fuse pressure in both
// directions, degraded siblings, EV and battery-boost reserves, and the
// incident scenarios that forecast_scenarios_test.go already names. Each
// record stores the inputs to one ComputeDispatch call — telemetry, State,
// slot directive, driver capacities, fuse ceiling — next to the per-driver
// targets and clamp attributions that call returned. This test rebuilds each
// scenario, calls the real ComputeDispatch, and checks the answer still
// matches.
//
// What the corpus is NOT: a statement of what dispatch SHOULD do. It is what
// dispatch DID do at commit c7fe6c98 (recorded in each file's ftw_commit),
// clamps, quirks and all. Nothing here was reviewed as correct. Its value is
// that it makes any change to dispatch visible and specific, which the unit
// tests around it cannot: they pin the cases somebody thought to write down,
// and dispatch is a long function whose paths interact.
//
// So a diff is a question, not a verdict. When this test fails, it is telling
// you which scenarios your change moved and by how many watts. Read them. If
// they are the ones you meant to move, and they moved the way you meant, the
// corpus is stale and you should re-record it — from the go/ directory:
//
//	FTW_GOLDEN_DUMP=1 go test ./internal/control/ -run TestGoldenDump -v
//
// then commit testdata/golden/ with your change, and put the summary of what
// moved in the pull request. Reviewers get to see the behaviour change in
// watts instead of inferring it from a diff of control flow. If scenarios you
// did not expect also moved, you have found something before your users did.
//
// Two things never fix a failure here: deleting the record, and widening the
// tolerance. The tolerance remains 0.01 W absolute so the corpus still reports
// small numeric changes without making the test depend on exact float output;
// golden scenarios now use one injected clock instant for the slot and the
// dispatch calculation. Anything above 0.01 W is a real change in behaviour.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	// goldenCorpusDir is both where the replay reads and where the recorder
	// writes by default. Relative to this package; `go test` runs here.
	goldenCorpusDir = "testdata/golden"

	// goldenToleranceW is the absolute watt tolerance for target and
	// attribution magnitudes. See the file comment: never raise it to make a
	// failure go away.
	goldenToleranceW = 0.01

	// goldenExpectedRecords guards against a half-written re-recording
	// silently shrinking the net.
	goldenExpectedRecords = 435

	// goldenMaxReported caps how many changed records get a full diff, so a
	// change that moves everything still produces readable output.
	goldenMaxReported = 20
)

// goldenFamilyNames is the list the replay reads and the recorder writes. The
// recorder fails if the two disagree.
var goldenFamilyNames = []string{
	"incident_scenarios",
	"targeted_invariants",
	"seeded_reactive",
	"seeded_planner",
	"seeded_fuse",
	"seeded_siblings",
	"seeded_ev_boost",
}

// readGoldenCorpus loads every family from dir and returns the records in
// file order plus an index by scenario ID.
func readGoldenCorpus(t *testing.T, dir string) ([]goldenRecord, map[string]goldenRecord) {
	t.Helper()
	var all []goldenRecord
	byID := map[string]goldenRecord{}
	for _, name := range goldenFamilyNames {
		path := filepath.Join(dir, name+".json")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read golden family %s: %v", path, err)
		}
		var gf goldenFile
		if err := json.Unmarshal(b, &gf); err != nil {
			t.Fatalf("parse golden family %s: %v", path, err)
		}
		if gf.RecordCount != len(gf.Records) {
			t.Errorf("%s: record_count %d but %d records present",
				path, gf.RecordCount, len(gf.Records))
		}
		for _, r := range gf.Records {
			if prev, dup := byID[r.ScenarioID]; dup {
				t.Errorf("duplicate scenario_id %q (already seen in this corpus with %d targets)",
					r.ScenarioID, len(prev.PerDriverTargets))
			}
			byID[r.ScenarioID] = r
			all = append(all, r)
		}
	}
	return all, byID
}

// TestGoldenCorpusReplay is the regression gate: every recorded scenario, back
// through the real ComputeDispatch, compared to what it answered before.
func TestGoldenCorpusReplay(t *testing.T) {
	all, _ := readGoldenCorpus(t, goldenCorpusDir)
	if len(all) != goldenExpectedRecords {
		t.Errorf("golden corpus has %d records, expected %d — was it re-recorded with a changed scenario set?",
			len(all), goldenExpectedRecords)
	}

	var changed, reported int
	for _, want := range all {
		got := runGoldenScenario(goldenScenario{
			ID:     want.ScenarioID,
			Seed:   want.Seed,
			Inputs: want.Inputs,
		})
		lines := diffGoldenRecord(want, got)
		if len(lines) == 0 {
			continue
		}
		changed++
		if reported < goldenMaxReported {
			reported++
			t.Errorf("dispatch changed for %s:\n%s", want.ScenarioID, strings.Join(lines, "\n"))
		}
	}
	if changed > reported {
		t.Errorf("%d more records changed but were not printed; raise goldenMaxReported or narrow the run with -run",
			changed-reported)
	}
	if changed > 0 {
		t.Logf("%d of %d golden records changed. If every one is intended, re-record with "+
			"FTW_GOLDEN_DUMP=1 go test ./internal/control/ -run TestGoldenDump and commit testdata/golden/.",
			changed, len(all))
	}
}

// TestGoldenCorpusCoverage checks the committed corpus still exercises the
// mechanisms it was built to hold still, so a re-recording that quietly loses
// a clamp path is caught at the source rather than years later.
func TestGoldenCorpusCoverage(t *testing.T) {
	all, byID := readGoldenCorpus(t, goldenCorpusDir)
	assertGoldenCoverage(t, all, byID)
}

// assertGoldenCoverage is shared with the recorder, which runs it against what
// it just wrote.
func assertGoldenCoverage(t *testing.T, all []goldenRecord, byID map[string]goldenRecord) {
	t.Helper()

	// 1. Floored-sibling reallocation: the blocked battery parks, the capable
	//    one takes the load.
	if fs, ok := byID["targeted/floored_sibling_discharge"]; !ok {
		t.Error("missing targeted/floored_sibling_discharge")
	} else {
		var parkedOK, siblingOK bool
		for _, tg := range fs.PerDriverTargets {
			if tg.Driver == "ferroamp" && math.Abs(tg.TargetW) <= 1 && tg.Clamped {
				parkedOK = true
			}
			if tg.Driver == "sungrow" && tg.TargetW < -1 {
				siblingOK = true
			}
		}
		if !parkedOK || !siblingOK {
			t.Errorf("floored-sibling reallocation not exercised: parked=%v sibling=%v targets=%v",
				parkedOK, siblingOK, fs.PerDriverTargets)
		}
	}

	// 2. forceFuseDischarge from idle mode.
	if ff, ok := byID["targeted/force_fuse_discharge_idle"]; !ok {
		t.Error("missing targeted/force_fuse_discharge_idle")
	} else if sumGoldenTargets(ff) >= -100 {
		t.Errorf("forceFuseDischarge not exercised: idle-mode over-fuse record sum=%.0f targets=%v",
			sumGoldenTargets(ff), ff.PerDriverTargets)
	}

	// 3. Per-phase overage: aggregate under the fuse, one phase over it.
	if pp, ok := byID["targeted/per_phase_overage_import"]; !ok {
		t.Error("missing targeted/per_phase_overage_import")
	} else {
		if pp.ClampAttributions.PerPhaseOverageW <= 0 {
			t.Errorf("per-phase overage not attributed: %+v", pp.ClampAttributions)
		}
		if sumGoldenTargets(pp) >= 0 {
			t.Errorf("per-phase import overage should force discharge; sum=%.0f", sumGoldenTargets(pp))
		}
	}

	// 4. Plan/exec sign floor.
	if sf, ok := byID["targeted/plan_sign_floor_discharge_slot"]; !ok {
		t.Error("missing targeted/plan_sign_floor_discharge_slot")
	} else if !sf.ClampAttributions.PlanSignFloorZeroed || len(sf.PerDriverTargets) == 0 {
		t.Errorf("plan sign floor not exercised: attr=%+v targets=%v",
			sf.ClampAttributions, sf.PerDriverTargets)
	}

	// Generic scans across the whole corpus.
	var nFuseSaturated, nHoldLatched, nPerPhase, nStale, nBlocked, nEmpty int
	for _, r := range all {
		if r.ClampAttributions.FuseSaturated {
			nFuseSaturated++
		}
		if r.ClampAttributions.FuseHoldLatched {
			nHoldLatched++
		}
		if r.ClampAttributions.PerPhaseOverageW > 0 {
			nPerPhase++
		}
		if r.ClampAttributions.PlanStale {
			nStale++
		}
		if len(r.PerDriverTargets) == 0 {
			nEmpty++
		}
		for _, b := range r.Inputs.Batteries {
			if b.DischargeBlocked || b.ChargeBlocked {
				nBlocked++
				break
			}
		}
	}
	t.Logf("coverage: fuse_saturated=%d hold_latched=%d per_phase=%d stale_plan=%d blocked_batteries=%d no_dispatch=%d",
		nFuseSaturated, nHoldLatched, nPerPhase, nStale, nBlocked, nEmpty)
	if nFuseSaturated == 0 || nHoldLatched == 0 || nPerPhase == 0 || nStale == 0 {
		t.Error("coverage gap: one of the generic families never fired its clamp")
	}
}

func sumGoldenTargets(r goldenRecord) float64 {
	var s float64
	for _, tg := range r.PerDriverTargets {
		s += tg.TargetW
	}
	return s
}

// ---- diff -----------------------------------------------------------------

func goldenWattsEqual(a, b float64) bool {
	if math.IsNaN(a) || math.IsNaN(b) {
		return math.IsNaN(a) && math.IsNaN(b)
	}
	return math.Abs(a-b) <= goldenToleranceW
}

// diffGoldenRecord returns one human line per difference, empty when the
// replay reproduced the record. It names the drivers that moved and by how
// much, because that is the question a reader has when this test fails.
func diffGoldenRecord(want, got goldenRecord) []string {
	var lines []string

	// Per-driver targets. Grouped by driver rather than compared positionally
	// so that a driver appearing or disappearing reads as such.
	wantByDriver := map[string][]goldenTarget{}
	gotByDriver := map[string][]goldenTarget{}
	drivers := map[string]bool{}
	for _, tg := range want.PerDriverTargets {
		wantByDriver[tg.Driver] = append(wantByDriver[tg.Driver], tg)
		drivers[tg.Driver] = true
	}
	for _, tg := range got.PerDriverTargets {
		gotByDriver[tg.Driver] = append(gotByDriver[tg.Driver], tg)
		drivers[tg.Driver] = true
	}
	names := make([]string, 0, len(drivers))
	for d := range drivers {
		names = append(names, d)
	}
	sort.Strings(names)

	var targetLines []string
	for _, d := range names {
		w, g := wantByDriver[d], gotByDriver[d]
		switch {
		case len(w) == 0:
			targetLines = append(targetLines, fmt.Sprintf(
				"    %s: not dispatched before, now %s", d, formatGoldenTargets(g)))
		case len(g) == 0:
			targetLines = append(targetLines, fmt.Sprintf(
				"    %s: was %s, now not dispatched", d, formatGoldenTargets(w)))
		case len(w) != len(g):
			targetLines = append(targetLines, fmt.Sprintf(
				"    %s: was %s, now %s", d, formatGoldenTargets(w), formatGoldenTargets(g)))
		default:
			for i := range w {
				if !goldenWattsEqual(w[i].TargetW, g[i].TargetW) {
					targetLines = append(targetLines, fmt.Sprintf(
						"    %s: was %+.2f W, now %+.2f W (moved %+.2f W)",
						d, w[i].TargetW, g[i].TargetW, g[i].TargetW-w[i].TargetW))
				}
				if w[i].Clamped != g[i].Clamped {
					targetLines = append(targetLines, fmt.Sprintf(
						"    %s: clamped was %v, now %v", d, w[i].Clamped, g[i].Clamped))
				}
			}
		}
	}
	if len(targetLines) > 0 {
		lines = append(lines, "  per-driver targets:")
		lines = append(lines, targetLines...)
	}

	// Clamp attributions: why dispatch landed where it did.
	wa, ga := want.ClampAttributions, got.ClampAttributions
	var attrLines []string
	addBool := func(name string, w, g bool) {
		if w != g {
			attrLines = append(attrLines, fmt.Sprintf("    %s: was %v, now %v", name, w, g))
		}
	}
	addWatts := func(name string, w, g float64) {
		if !goldenWattsEqual(w, g) {
			attrLines = append(attrLines, fmt.Sprintf("    %s: was %.2f W, now %.2f W (moved %+.2f W)",
				name, w, g, g-w))
		}
	}
	if strings.Join(wa.ClampedDrivers, ",") != strings.Join(ga.ClampedDrivers, ",") {
		attrLines = append(attrLines, fmt.Sprintf("    clamped_drivers: was [%s], now [%s]",
			strings.Join(wa.ClampedDrivers, " "), strings.Join(ga.ClampedDrivers, " ")))
	}
	addBool("all_targets_zero_clamped", wa.AllTargetsZeroClamped, ga.AllTargetsZeroClamped)
	if wa.PlanSignIntent != ga.PlanSignIntent {
		attrLines = append(attrLines, fmt.Sprintf("    plan_sign_intent: was %d, now %d",
			wa.PlanSignIntent, ga.PlanSignIntent))
	}
	addBool("plan_sign_floor_zeroed", wa.PlanSignFloorZeroed, ga.PlanSignFloorZeroed)
	addBool("plan_stale", wa.PlanStale, ga.PlanStale)
	addBool("fuse_saturated", wa.FuseSaturated, ga.FuseSaturated)
	addWatts("fuse_ev_max_w", wa.FuseEVMaxW, ga.FuseEVMaxW)
	addBool("fuse_hold_latched", wa.FuseHoldLatched, ga.FuseHoldLatched)
	addWatts("fuse_hold_max_charge_w", wa.FuseHoldMaxChargeW, ga.FuseHoldMaxChargeW)
	addWatts("fuse_hold_max_discharge_w", wa.FuseHoldMaxDischargeW, ga.FuseHoldMaxDischargeW)
	addWatts("per_phase_overage_w", wa.PerPhaseOverageW, ga.PerPhaseOverageW)
	addWatts("effective_import_ceiling_w", wa.EffectiveImportCeilingW, ga.EffectiveImportCeilingW)
	addWatts("effective_export_ceiling_w", wa.EffectiveExportCeilingW, ga.EffectiveExportCeilingW)
	addBool("import_ceiling_binding", wa.ImportCeilingBinding, ga.ImportCeilingBinding)
	addBool("export_ceiling_binding", wa.ExportCeilingBinding, ga.ExportCeilingBinding)
	if len(attrLines) > 0 {
		lines = append(lines, "  clamp attributions:")
		lines = append(lines, attrLines...)
	}

	// Site totals: the one-number summary of what the fleet was asked to do.
	ws, gs := want.SiteTotals, got.SiteTotals
	var totalLines []string
	addTotal := func(name string, w, g float64) {
		if !goldenWattsEqual(w, g) {
			totalLines = append(totalLines, fmt.Sprintf("    %s: was %.2f W, now %.2f W (moved %+.2f W)",
				name, w, g, g-w))
		}
	}
	addTotal("battery_current_w", ws.BatteryCurrentW, gs.BatteryCurrentW)
	addTotal("battery_target_sum_w", ws.BatteryTargetSumW, gs.BatteryTargetSumW)
	addTotal("raw_grid_w", ws.RawGridW, gs.RawGridW)
	addTotal("projected_grid_w", ws.ProjectedGridW, gs.ProjectedGridW)
	if len(totalLines) > 0 {
		lines = append(lines, "  site totals:")
		lines = append(lines, totalLines...)
	}

	if len(lines) > 0 {
		lines = append(lines, fmt.Sprintf("  scenario: mode=%s grid=%.0f W fuse=%.0f W batteries=%d slot=%s",
			want.Inputs.State.Mode, want.Inputs.GridW, want.Inputs.FuseMaxW,
			len(want.Inputs.Batteries), describeGoldenSlot(want.Inputs.Slot)))
	}
	return lines
}

func formatGoldenTargets(tgs []goldenTarget) string {
	parts := make([]string, 0, len(tgs))
	for _, tg := range tgs {
		s := fmt.Sprintf("%+.2f W", tg.TargetW)
		if tg.Clamped {
			s += " (clamped)"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

func describeGoldenSlot(s *goldenSlot) string {
	switch {
	case s == nil:
		return "none"
	case !s.Present:
		return "stale"
	default:
		return fmt.Sprintf("%s %.0f Wh, %.0f s left", s.Strategy, s.BatteryEnergyWh, s.RemainingS)
	}
}
