package control

// Recorder for the golden dispatch corpus replayed by golden_test.go.
//
// It builds the scenarios, drives the real ComputeDispatch over each one and
// writes testdata/golden/<family>.json. The recording is gated on an
// environment variable so a normal test run never rewrites the corpus:
// overwriting it is how a regression gets erased, so it has to be asked for.
//
// Regenerate deliberately, from the go/ module directory:
//
//	FTW_GOLDEN_DUMP=1 go test ./internal/control/ -run TestGoldenDump -v
//
// Then read `git diff` on testdata/golden/ before committing it. That diff is
// the behaviour change your work makes, stated in watts. FTW_GOLDEN_OUT
// overrides the output directory when you want to compare two builds
// side by side instead of overwriting.
//
// The scenario builders below are the corpus's coverage plan: which mode,
// fuse, sibling, EV and boost situations exist at all. Adding a scenario here
// and re-recording widens the net; it does not change what the existing
// records assert.
//
// Every record is one call to ComputeDispatch from a freshly-constructed
// telemetry.Store and control.State (single-tick semantics: PI integral
// starts at zero, PrevTargets empty, slot accumulator on its rollover
// branch). Slot directives are anchored to time.Now() with fixed nominal
// elapsed/remaining offsets, so energy-path magnitudes are deterministic
// up to sub-millisecond clock jitter (< 0.01 % relative) — which is why the
// replay compares with a tolerance rather than for equality.

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// ---- record schema ------------------------------------------------------

type goldenBattery struct {
	Driver           string  `json:"driver"`
	CapacityWh       float64 `json:"capacity_wh"`
	CurrentW         float64 `json:"current_w"`
	SoC              float64 `json:"soc"`
	Online           bool    `json:"online"`
	DeviceFault      bool    `json:"device_fault"`
	DischargeBlocked bool    `json:"discharge_blocked"`
	ChargeBlocked    bool    `json:"charge_blocked"`
	MaxChargeW       float64 `json:"max_charge_w"`
	MaxDischargeW    float64 `json:"max_discharge_w"`
	Group            string  `json:"group,omitempty"`
}

type goldenPV struct {
	Driver string  `json:"driver"`
	PowerW float64 `json:"power_w"`
}

type goldenEV struct {
	Driver string  `json:"driver"`
	PowerW float64 `json:"power_w"`
}

type goldenPhases struct {
	L1A float64 `json:"l1_a"`
	L2A float64 `json:"l2_a"`
	L3A float64 `json:"l3_a"`
}

type goldenSlot struct {
	// Present=false models a stale/missing plan (SlotDirectiveFunc ok=false).
	Present                bool    `json:"present"`
	BatteryEnergyWh        float64 `json:"battery_energy_wh"`
	Strategy               string  `json:"strategy"`
	PlannedGridW           float64 `json:"planned_grid_w"`
	HasPlannedGridW        bool    `json:"has_planned_grid_w"`
	LivePVSurplusSoCCapPct float64 `json:"live_pv_surplus_soc_cap_pct"`
	// Nominal slot geometry relative to the dispatch call:
	// SlotStart = now - elapsed_s, SlotEnd = now + remaining_s.
	ElapsedS   float64 `json:"elapsed_s"`
	RemainingS float64 `json:"remaining_s"`
}

type goldenLegacyPlan struct {
	Mode  string  `json:"mode"`
	GridW float64 `json:"grid_w"`
}

type goldenStateInputs struct {
	Mode                      string             `json:"mode"`
	GridTargetW               float64            `json:"grid_target_w"`
	GridToleranceW            float64            `json:"grid_tolerance_w"`
	SlewRateW                 float64            `json:"slew_rate_w"`
	SlewEnabled               bool               `json:"slew_enabled"`
	MinDispatchIntervalS      int                `json:"min_dispatch_interval_s"`
	HoldoffActive             bool               `json:"holdoff_active"`
	UseEnergyDispatch         bool               `json:"use_energy_dispatch"`
	EVChargingW               float64            `json:"ev_charging_w"`
	BatteryCoversEV           bool               `json:"battery_covers_ev"`
	BatteryBoostEVChargingW   float64            `json:"battery_boost_ev_charging_w"`
	BatteryBoostReserveSoC    float64            `json:"battery_boost_reserve_soc"`
	EVSurplusOnlyReserveW     float64            `json:"ev_surplus_only_reserve_w"`
	PeakLimitW                float64            `json:"peak_limit_w"`
	PeakImportCeilingW        float64            `json:"peak_import_ceiling_w"`
	MaxExportW                float64            `json:"max_export_w"`
	SiteFuseAmps              float64            `json:"site_fuse_amps"`
	SiteFuseVoltage           float64            `json:"site_fuse_voltage"`
	SiteFusePhases            int                `json:"site_fuse_phases"`
	SiteFuseSafetyA           float64            `json:"site_fuse_safety_a"`
	PVSurplusAbsorbSoCCapPct  float64            `json:"pv_surplus_absorb_soc_cap_pct"`
	PVSurplusAbsorbThresholdW float64            `json:"pv_surplus_absorb_threshold_w"`
	InverterGroups            map[string]string  `json:"inverter_groups,omitempty"`
	PriorityOrder             []string           `json:"priority_order,omitempty"`
	Weights                   map[string]float64 `json:"weights,omitempty"`
}

type goldenInputs struct {
	FuseMaxW    float64           `json:"fuse_max_w"`
	GridW       float64           `json:"grid_w"`
	MeterPhases *goldenPhases     `json:"meter_phase_amps,omitempty"`
	PV          []goldenPV        `json:"pv,omitempty"`
	EV          []goldenEV        `json:"ev,omitempty"`
	Batteries   []goldenBattery   `json:"batteries"`
	State       goldenStateInputs `json:"state"`
	Slot        *goldenSlot       `json:"slot,omitempty"`
	LegacyPlan  *goldenLegacyPlan `json:"legacy_plan,omitempty"`
}

type goldenTarget struct {
	Driver  string  `json:"driver"`
	TargetW float64 `json:"target_w"`
	Clamped bool    `json:"clamped"`
}

type goldenClampAttribution struct {
	ClampedDrivers          []string `json:"clamped_drivers"`
	AllTargetsZeroClamped   bool     `json:"all_targets_zero_clamped"`
	PlanSignIntent          int      `json:"plan_sign_intent"`
	PlanSignFloorZeroed     bool     `json:"plan_sign_floor_zeroed"`
	PlanStale               bool     `json:"plan_stale"`
	FuseSaturated           bool     `json:"fuse_saturated"`
	FuseEVMaxW              float64  `json:"fuse_ev_max_w"`
	FuseHoldLatched         bool     `json:"fuse_hold_latched"`
	FuseHoldMaxChargeW      float64  `json:"fuse_hold_max_charge_w"`
	FuseHoldMaxDischargeW   float64  `json:"fuse_hold_max_discharge_w"`
	PerPhaseOverageW        float64  `json:"per_phase_overage_w"`
	EffectiveImportCeilingW float64  `json:"effective_import_ceiling_w"`
	EffectiveExportCeilingW float64  `json:"effective_export_ceiling_w"`
	ImportCeilingBinding    bool     `json:"import_ceiling_binding"`
	ExportCeilingBinding    bool     `json:"export_ceiling_binding"`
}

type goldenSiteTotals struct {
	BatteryCurrentW   float64 `json:"battery_current_w"`
	BatteryTargetSumW float64 `json:"battery_target_sum_w"`
	RawGridW          float64 `json:"raw_grid_w"`
	ProjectedGridW    float64 `json:"projected_grid_w"`
}

type goldenRecord struct {
	ScenarioID        string                 `json:"scenario_id"`
	Seed              int64                  `json:"seed"`
	Inputs            goldenInputs           `json:"inputs"`
	PerDriverTargets  []goldenTarget         `json:"per_driver_targets"`
	ClampAttributions goldenClampAttribution `json:"clamp_attributions"`
	SiteTotals        goldenSiteTotals       `json:"site_totals"`
}

type goldenFile struct {
	Family      string         `json:"family"`
	GeneratedBy string         `json:"generated_by"`
	FTWCommit   string         `json:"ftw_commit"`
	RecordCount int            `json:"record_count"`
	Records     []goldenRecord `json:"records"`
}

type goldenScenario struct {
	ID     string
	Seed   int64
	Inputs goldenInputs
}

// ---- scenario execution -------------------------------------------------

func defaultGoldenState(mode Mode) goldenStateInputs {
	return goldenStateInputs{
		Mode:                 string(mode),
		GridToleranceW:       50,
		SlewRateW:            100_000,
		SlewEnabled:          true,
		MinDispatchIntervalS: 0,
		PeakLimitW:           5000,
	}
}

func runGoldenScenario(sc goldenScenario) goldenRecord {
	store := telemetry.NewStore()

	var meterData json.RawMessage
	if sc.Inputs.MeterPhases != nil {
		b, err := json.Marshal(sc.Inputs.MeterPhases)
		if err == nil {
			meterData = b
		}
	}
	store.Update("meter", telemetry.DerMeter, sc.Inputs.GridW, nil, meterData)
	store.DriverHealthMut("meter").RecordSuccess()

	for _, pv := range sc.Inputs.PV {
		store.Update(pv.Driver, telemetry.DerPV, pv.PowerW, nil, nil)
		store.DriverHealthMut(pv.Driver).RecordSuccess()
	}
	for _, ev := range sc.Inputs.EV {
		store.Update(ev.Driver, telemetry.DerEV, ev.PowerW, nil, nil)
		store.DriverHealthMut(ev.Driver).RecordSuccess()
	}

	capsMap := map[string]float64{}
	limits := map[string]PowerLimits{}
	groups := map[string]string{}
	for k, v := range sc.Inputs.State.InverterGroups {
		groups[k] = v
	}
	for _, b := range sc.Inputs.Batteries {
		soc := b.SoC
		var data json.RawMessage
		if b.DischargeBlocked || b.ChargeBlocked {
			blocks := struct {
				DischargeCapable bool `json:"discharge_capable"`
				ChargeCapable    bool `json:"charge_capable"`
			}{DischargeCapable: !b.DischargeBlocked, ChargeCapable: !b.ChargeBlocked}
			bd, err := json.Marshal(blocks)
			if err == nil {
				data = bd
			}
		}
		store.Update(b.Driver, telemetry.DerBattery, b.CurrentW, &soc, data)
		h := store.DriverHealthMut(b.Driver)
		switch {
		case b.DeviceFault:
			h.RecordSuccess()
			h.SetDeviceFault(true, "golden: faulted sibling")
		case b.Online:
			h.RecordSuccess()
		default:
			h.SetOffline()
		}
		capsMap[b.Driver] = b.CapacityWh
		if b.MaxChargeW > 0 || b.MaxDischargeW > 0 {
			limits[b.Driver] = PowerLimits{MaxChargeW: b.MaxChargeW, MaxDischargeW: b.MaxDischargeW}
		}
		if b.Group != "" {
			groups[b.Driver] = b.Group
		}
	}

	si := sc.Inputs.State
	st := NewState(si.GridTargetW, si.GridToleranceW, "meter")
	st.Mode = Mode(si.Mode)
	st.SlewRateW = si.SlewRateW
	st.SlewEnabled = si.SlewEnabled
	st.MinDispatchIntervalS = si.MinDispatchIntervalS
	st.UseEnergyDispatch = si.UseEnergyDispatch
	st.EVChargingW = si.EVChargingW
	st.BatteryCoversEV = si.BatteryCoversEV
	st.BatteryBoostEVChargingW = si.BatteryBoostEVChargingW
	st.BatteryBoostReserveSoC = si.BatteryBoostReserveSoC
	st.EVSurplusOnlyReserveW = si.EVSurplusOnlyReserveW
	st.PeakLimitW = si.PeakLimitW
	st.PeakImportCeilingW = si.PeakImportCeilingW
	st.MaxExportW = si.MaxExportW
	st.SiteFuseAmps = si.SiteFuseAmps
	st.SiteFuseVoltage = si.SiteFuseVoltage
	st.SiteFusePhases = si.SiteFusePhases
	st.SiteFuseSafetyA = si.SiteFuseSafetyA
	st.PVSurplusAbsorbSoCCapPct = si.PVSurplusAbsorbSoCCapPct
	st.PVSurplusAbsorbThresholdW = si.PVSurplusAbsorbThresholdW
	if len(limits) > 0 {
		st.DriverLimits = limits
	}
	if len(groups) > 0 {
		st.InverterGroups = groups
	}
	if len(si.PriorityOrder) > 0 {
		st.PriorityOrder = si.PriorityOrder
	}
	if len(si.Weights) > 0 {
		st.Weights = si.Weights
	}
	if si.HoldoffActive {
		now := time.Now()
		st.LastDispatch = &now
	}
	if sc.Inputs.Slot != nil {
		slot := *sc.Inputs.Slot
		st.SlotDirective = func(_ time.Time) (SlotDirective, bool) {
			if !slot.Present {
				return SlotDirective{}, false
			}
			now := time.Now()
			return SlotDirective{
				SlotStart:              now.Add(-time.Duration(slot.ElapsedS * float64(time.Second))),
				SlotEnd:                now.Add(time.Duration(slot.RemainingS * float64(time.Second))),
				BatteryEnergyWh:        slot.BatteryEnergyWh,
				Strategy:               slot.Strategy,
				PlannedGridW:           slot.PlannedGridW,
				HasPlannedGridW:        slot.HasPlannedGridW,
				LivePVSurplusSoCCapPct: slot.LivePVSurplusSoCCapPct,
			}, true
		}
	}
	if sc.Inputs.LegacyPlan != nil {
		lp := *sc.Inputs.LegacyPlan
		st.PlanTarget = func(_ time.Time) (string, float64, bool) {
			return lp.Mode, lp.GridW, true
		}
	}

	// Pre-dispatch attribution reads (pure).
	intent := planSignIntent(st)
	perPhase := perPhaseOverageW(store, st)
	effImp := st.effectiveImportCeilingW(sc.Inputs.FuseMaxW)
	effExp := st.effectiveExportCeilingW(sc.Inputs.FuseMaxW)

	targets := ComputeDispatch(store, st, capsMap, sc.Inputs.FuseMaxW)

	out := make([]goldenTarget, 0, len(targets))
	var clampedDrivers []string
	allZeroClamped := len(targets) > 0
	var sumTarget, currentBatOfTargeted float64
	seen := map[string]bool{}
	for _, tg := range targets {
		out = append(out, goldenTarget{Driver: tg.Driver, TargetW: tg.TargetW, Clamped: tg.Clamped})
		sumTarget += tg.TargetW
		if tg.Clamped {
			clampedDrivers = append(clampedDrivers, tg.Driver)
		}
		if math.Abs(tg.TargetW) > 1e-9 || !tg.Clamped {
			allZeroClamped = false
		}
		if !seen[tg.Driver] {
			seen[tg.Driver] = true
			if r := store.Get(tg.Driver, telemetry.DerBattery); r != nil {
				currentBatOfTargeted += r.SmoothedW
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Driver < out[j].Driver })
	sort.Strings(clampedDrivers)

	projected := sc.Inputs.GridW - currentBatOfTargeted + sumTarget

	mode := Mode(si.Mode)
	planSignFloorZeroed := allZeroClamped && intent != 0 &&
		mode.IsPlannerMode() && mode != ModePlannerSelf

	attr := goldenClampAttribution{
		ClampedDrivers:          clampedDrivers,
		AllTargetsZeroClamped:   allZeroClamped,
		PlanSignIntent:          intent,
		PlanSignFloorZeroed:     planSignFloorZeroed,
		PlanStale:               st.PlanStale,
		FuseSaturated:           st.FuseSaturated,
		FuseEVMaxW:              st.FuseEVMaxW,
		FuseHoldLatched:         st.FuseHoldUntil.After(time.Now()),
		FuseHoldMaxChargeW:      st.FuseHoldMaxChargeW,
		FuseHoldMaxDischargeW:   st.FuseHoldMaxDischargeW,
		PerPhaseOverageW:        perPhase,
		EffectiveImportCeilingW: effImp,
		EffectiveExportCeilingW: effExp,
		ImportCeilingBinding:    len(targets) > 0 && math.Abs(projected-effImp) <= 25,
		ExportCeilingBinding:    len(targets) > 0 && math.Abs(projected+effExp) <= 25,
	}
	if clampedDrivers == nil {
		attr.ClampedDrivers = []string{}
	}

	var currentBatOnline float64
	for _, b := range sc.Inputs.Batteries {
		if b.Online && !b.DeviceFault {
			currentBatOnline += b.CurrentW
		}
	}

	return goldenRecord{
		ScenarioID:        sc.ID,
		Seed:              sc.Seed,
		Inputs:            sc.Inputs,
		PerDriverTargets:  out,
		ClampAttributions: attr,
		SiteTotals: goldenSiteTotals{
			BatteryCurrentW:   currentBatOnline,
			BatteryTargetSumW: sumTarget,
			RawGridW:          sc.Inputs.GridW,
			ProjectedGridW:    projected,
		},
	}
}

// ---- scenario builder helpers -------------------------------------------

func gb(name string, capWh, curW, soc float64) goldenBattery {
	return goldenBattery{Driver: name, CapacityWh: capWh, CurrentW: curW, SoC: soc, Online: true}
}

func slotFor(energyWh float64, strategy string) *goldenSlot {
	return &goldenSlot{
		Present: true, BatteryEnergyWh: energyWh, Strategy: strategy,
		ElapsedS: 450, RemainingS: 450,
	}
}

func staleSlot() *goldenSlot { return &goldenSlot{Present: false} }

// ---- family: incident-derived (forecast_scenarios_test ports) -----------

func incidentScenarios() []goldenScenario {
	mk := func(id string, in goldenInputs) goldenScenario {
		return goldenScenario{ID: "incident/" + id, Inputs: in}
	}
	planner := func(mode Mode) goldenStateInputs {
		s := defaultGoldenState(mode)
		s.GridToleranceW = 60
		s.UseEnergyDispatch = true
		return s
	}
	var out []goldenScenario

	out = append(out, mk("A1_passive_arb_idle_slot_pv_miss", goldenInputs{
		FuseMaxW: 11040, GridW: 600,
		PV:        []goldenPV{{Driver: "pv-0", PowerW: -140}},
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.70)},
		State:     planner(ModePlannerPassiveArbitrage),
		Slot:      slotFor(0, "passive_arbitrage"),
	}))
	out = append(out, mk("A2_passive_arb_idle_slot_load_miss", goldenInputs{
		FuseMaxW: 11040, GridW: 800,
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.60)},
		State:     planner(ModePlannerPassiveArbitrage),
		Slot:      slotFor(0, "passive_arbitrage"),
	}))
	out = append(out, mk("A3_passive_arb_charge_slot_live_import", goldenInputs{
		FuseMaxW: 11040, GridW: 600,
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.30)},
		State:     planner(ModePlannerPassiveArbitrage),
		Slot:      slotFor(4000, "passive_arbitrage"),
	}))
	out = append(out, mk("A4_planner_arb_discharge_slot_live_pv_surplus", goldenInputs{
		FuseMaxW: 11040, GridW: -2000,
		PV:        []goldenPV{{Driver: "pv-0", PowerW: -4000}},
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.80)},
		State:     planner(ModePlannerArbitrage),
		Slot:      slotFor(-2000, "arbitrage"),
	}))
	out = append(out, mk("A5_planner_self_idle_slot_pv_miss", goldenInputs{
		FuseMaxW: 11040, GridW: 600,
		PV:        []goldenPV{{Driver: "pv-0", PowerW: -140}},
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.70)},
		State:     planner(ModePlannerSelf),
		Slot:      slotFor(0, "self_consumption"),
	}))
	a6slot := slotFor(0, "self_consumption")
	a6slot.PlannedGridW = -2000
	a6slot.HasPlannedGridW = true
	out = append(out, mk("A6_planner_self_planned_export_no_self_charge", goldenInputs{
		FuseMaxW: 11040, GridW: 100,
		PV:        []goldenPV{{Driver: "pv-0", PowerW: -2500}},
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.75)},
		State:     planner(ModePlannerSelf),
		Slot:      a6slot,
	}))

	out = append(out, mk("B7_self_consumption_import_discharges", goldenInputs{
		FuseMaxW: 11040, GridW: 800,
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.60)},
		State:     defaultGoldenState(ModeSelfConsumption),
	}))
	out = append(out, mk("B8_self_consumption_export_charges", goldenInputs{
		FuseMaxW: 11040, GridW: -3000,
		PV:        []goldenPV{{Driver: "pv-0", PowerW: -5000}},
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.40)},
		State:     defaultGoldenState(ModeSelfConsumption),
	}))
	out = append(out, mk("B9_idle_mode_never_moves", goldenInputs{
		FuseMaxW: 11040, GridW: 5000,
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.60)},
		State:     defaultGoldenState(ModeIdle),
	}))
	out = append(out, mk("B10_charge_mode_near_full", goldenInputs{
		FuseMaxW: 11040, GridW: 0,
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.96)},
		State:     defaultGoldenState(ModeCharge),
	}))
	out = append(out, mk("B11_planner_self_empty_battery_no_discharge", goldenInputs{
		FuseMaxW: 11040, GridW: 500,
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.04)},
		State:     planner(ModePlannerSelf),
		Slot:      slotFor(0, "self_consumption"),
	}))
	out = append(out, mk("B12_stale_plan_falls_back_to_self_consumption", goldenInputs{
		FuseMaxW: 11040, GridW: 700,
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.60)},
		State:     planner(ModePlannerArbitrage),
		Slot:      staleSlot(),
	}))

	// E22 fuse invariants.
	e22 := []struct {
		name  string
		gridW float64
		mode  Mode
		slot  *goldenSlot
		soc   float64
	}{
		{"arb_charge_slot_high_load", 8000, ModePlannerArbitrage, slotFor(1000, "arbitrage"), 0.30},
		{"self_consumption_heavy_import", 10000, ModeSelfConsumption, nil, 0.50},
		{"passive_arb_charge_slot_surge", 7500, ModePlannerPassiveArbitrage, slotFor(1250, "passive_arbitrage"), 0.20},
		{"peak_shaving_over_limit", 9000, ModePeakShaving, nil, 0.60},
		{"manual_charge_high_import", 10000, ModeCharge, nil, 0.60},
	}
	for _, c := range e22 {
		st := planner(c.mode)
		out = append(out, mk("E22_fuse_"+c.name, goldenInputs{
			FuseMaxW: 11040, GridW: c.gridW,
			Batteries: []goldenBattery{gb("ferroamp", 15200, 0, c.soc)},
			State:     st,
			Slot:      c.slot,
		}))
	}
	out = append(out, mk("E23_soc_floor_no_discharge_when_empty", goldenInputs{
		FuseMaxW: 11040, GridW: 2000,
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.04)},
		State:     planner(ModePlannerArbitrage),
		Slot:      slotFor(-500, "arbitrage"),
	}))
	out = append(out, mk("E24_soc_ceiling_no_discharge_on_charge_cmd", goldenInputs{
		FuseMaxW: 11040, GridW: -2000,
		PV:        []goldenPV{{Driver: "pv-0", PowerW: -3000}},
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.98)},
		State:     planner(ModePlannerArbitrage),
		Slot:      slotFor(500, "arbitrage"),
	}))
	e25 := defaultGoldenState(ModeSelfConsumption)
	e25.MinDispatchIntervalS = 60
	e25.HoldoffActive = true
	out = append(out, mk("E25_holdoff_suppresses_dispatch", goldenInputs{
		FuseMaxW: 11040, GridW: 2000,
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.60)},
		State:     e25,
	}))

	out = append(out, mk("F26_two_batteries_proportional_split", goldenInputs{
		FuseMaxW: 11040, GridW: 1000,
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.50), gb("sungrow", 9600, 0, 0.50)},
		State:     defaultGoldenState(ModeSelfConsumption),
	}))
	f27b := []goldenBattery{gb("ferroamp", 15200, 0, 0.50), gb("sungrow", 9600, 0, 0.50)}
	f27b[1].Online = false
	out = append(out, mk("F27_two_batteries_one_offline", goldenInputs{
		FuseMaxW: 11040, GridW: 1000,
		Batteries: f27b,
		State:     defaultGoldenState(ModeSelfConsumption),
	}))
	for _, mode := range []Mode{ModePlannerArbitrage, ModePlannerCheap, ModePlannerPassiveArbitrage} {
		out = append(out, mk("F28_stale_plan_fallback_"+string(mode), goldenInputs{
			FuseMaxW: 11040, GridW: 700,
			Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.60)},
			State:     planner(mode),
			Slot:      staleSlot(),
		}))
	}
	out = append(out, mk("F29_negative_price_idle_plan_reactive_discharge", goldenInputs{
		FuseMaxW: 11040, GridW: 650,
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.70)},
		State:     planner(ModePlannerPassiveArbitrage),
		Slot:      slotFor(0, "passive_arbitrage"),
	}))
	f30 := defaultGoldenState(ModeSelfConsumption)
	f30.EVChargingW = 4000
	f30.BatteryCoversEV = true
	out = append(out, mk("F30_ev_charging_battery_cover_enabled", goldenInputs{
		FuseMaxW: 11040, GridW: 4500,
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.65)},
		State:     f30,
	}))
	return out
}

// ---- family: targeted invariant scenarios --------------------------------

func targetedScenarios() []goldenScenario {
	mk := func(id string, in goldenInputs) goldenScenario {
		return goldenScenario{ID: "targeted/" + id, Inputs: in}
	}
	var out []goldenScenario

	// Floored-sibling reallocation (discharge direction).
	fb := gb("ferroamp", 15200, 0, 0.50)
	fb.DischargeBlocked = true
	out = append(out, mk("floored_sibling_discharge", goldenInputs{
		FuseMaxW: 11040, GridW: 2000,
		Batteries: []goldenBattery{fb, gb("sungrow", 9600, 0, 0.50)},
		State:     defaultGoldenState(ModeSelfConsumption),
	}))

	// Floored-sibling reallocation (charge direction).
	cb := gb("ferroamp", 15200, 0, 0.50)
	cb.ChargeBlocked = true
	out = append(out, mk("floored_sibling_charge", goldenInputs{
		FuseMaxW: 11040, GridW: -2000,
		Batteries: []goldenBattery{cb, gb("sungrow", 9600, 0, 0.50)},
		State:     defaultGoldenState(ModeSelfConsumption),
	}))

	// Capable sibling capped at its own limit.
	rb := gb("ferroamp", 15200, 0, 0.50)
	rb.DischargeBlocked = true
	rc := gb("sungrow", 9600, 0, 0.50)
	rc.MaxDischargeW = 300
	out = append(out, mk("reallocation_respects_sibling_cap", goldenInputs{
		FuseMaxW: 11040, GridW: 3000,
		Batteries: []goldenBattery{rb, rc},
		State:     defaultGoldenState(ModeSelfConsumption),
	}))

	// Every battery blocked: all parked, residual to grid.
	ab1 := gb("ferroamp", 15200, 0, 0.50)
	ab1.DischargeBlocked = true
	ab2 := gb("sungrow", 9600, 0, 0.50)
	ab2.DischargeBlocked = true
	out = append(out, mk("all_blocked_parked", goldenInputs{
		FuseMaxW: 11040, GridW: 2000,
		Batteries: []goldenBattery{ab1, ab2},
		State:     defaultGoldenState(ModeSelfConsumption),
	}))

	// Faulted sibling: device fault excludes it; capable sibling covers.
	db := gb("ferroamp", 15200, -500, 0.60)
	db.DeviceFault = true
	out = append(out, mk("faulted_sibling_excluded", goldenInputs{
		FuseMaxW: 11040, GridW: 1500,
		Batteries: []goldenBattery{db, gb("sungrow", 9600, 0, 0.55)},
		State:     defaultGoldenState(ModeSelfConsumption),
	}))

	// forceFuseDischarge from idle mode (fuseSaverFromZero path).
	out = append(out, mk("force_fuse_discharge_idle", goldenInputs{
		FuseMaxW: 11040, GridW: 12500,
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.60)},
		State:     defaultGoldenState(ModeIdle),
	}))

	// forceFuseDischarge overrides the holdoff window.
	hf := defaultGoldenState(ModeSelfConsumption)
	hf.MinDispatchIntervalS = 60
	hf.HoldoffActive = true
	out = append(out, mk("force_fuse_discharge_overrides_holdoff", goldenInputs{
		FuseMaxW: 11040, GridW: 13000,
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.70)},
		State:     hf,
	}))

	// Per-phase overage, import side (aggregate under fuse, one phase over).
	pp := defaultGoldenState(ModeSelfConsumption)
	pp.SiteFuseAmps = 16
	pp.SiteFuseVoltage = 230
	pp.SiteFusePhases = 3
	out = append(out, mk("per_phase_overage_import", goldenInputs{
		FuseMaxW: 11040, GridW: 1000,
		MeterPhases: &goldenPhases{L1A: 20, L2A: 4, L3A: 5},
		Batteries:   []goldenBattery{gb("ferroamp", 15200, 0, 0.60)},
		State:       pp,
	}))

	// Per-phase overage, export side (signed negative amps, discharge slot).
	pe := defaultGoldenState(ModePlannerArbitrage)
	pe.UseEnergyDispatch = true
	pe.SiteFuseAmps = 16
	pe.SiteFuseVoltage = 230
	pe.SiteFusePhases = 3
	out = append(out, mk("per_phase_overage_export", goldenInputs{
		FuseMaxW: 11040, GridW: -6000,
		MeterPhases: &goldenPhases{L1A: -20, L2A: -8, L3A: -8},
		PV:          []goldenPV{{Driver: "pv-0", PowerW: -5000}},
		Batteries:   []goldenBattery{gb("ferroamp", 15200, -2000, 0.80)},
		State:       pe,
		Slot:        slotFor(-1000, "arbitrage"),
	}))

	// Plan/exec sign floor: discharge slot, slew-anchored exec still charging.
	sf := defaultGoldenState(ModePlannerArbitrage)
	sf.UseEnergyDispatch = true
	sf.SlewRateW = 500
	out = append(out, mk("plan_sign_floor_discharge_slot", goldenInputs{
		FuseMaxW: 11040, GridW: 500,
		Batteries: []goldenBattery{gb("ferroamp", 15200, 4800, 0.60)},
		State:     sf,
		Slot:      slotFor(-2000, "arbitrage"),
	}))

	// Plan/exec sign floor: charge slot, slew-anchored exec still discharging.
	sfc := defaultGoldenState(ModePlannerArbitrage)
	sfc.UseEnergyDispatch = true
	sfc.SlewRateW = 500
	out = append(out, mk("plan_sign_floor_charge_slot", goldenInputs{
		FuseMaxW: 11040, GridW: 500,
		Batteries: []goldenBattery{gb("ferroamp", 15200, -4800, 0.50)},
		State:     sfc,
		Slot:      slotFor(2000, "arbitrage"),
	}))

	// Battery-boost reserve: SoC at/below reserve pins discharge to zero.
	bf := defaultGoldenState(ModeSelfConsumption)
	bf.BatteryBoostReserveSoC = 0.50
	bf.BatteryBoostEVChargingW = 2000
	out = append(out, mk("boost_reserve_floor_soc_below", goldenInputs{
		FuseMaxW: 11040, GridW: 3000,
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.45)},
		State:     bf,
	}))

	// Battery-boost reserve: thin headroom above reserve caps magnitude.
	bh := defaultGoldenState(ModeSelfConsumption)
	bh.BatteryBoostReserveSoC = 0.50
	bh.BatteryBoostEVChargingW = 2000
	bh.MinDispatchIntervalS = 30
	out = append(out, mk("boost_reserve_headroom_clamp", goldenInputs{
		FuseMaxW: 11040, GridW: 3000,
		Batteries: []goldenBattery{gb("ferroamp", 5000, 0, 0.501)},
		State:     bh,
	}))

	// DC-locality: charge routed to the battery sharing a group with live PV.
	dc := defaultGoldenState(ModeSelfConsumption)
	fa := gb("ferroamp", 15200, 0, 0.40)
	fa.Group = "fa"
	sg := gb("sungrow", 9600, 0, 0.40)
	sg.Group = "sg"
	dc.InverterGroups = map[string]string{"pv-fa": "fa"}
	out = append(out, mk("dc_locality_group_charge", goldenInputs{
		FuseMaxW: 11040, GridW: -4500,
		PV:        []goldenPV{{Driver: "pv-fa", PowerW: -4000}},
		Batteries: []goldenBattery{fa, sg},
		State:     dc,
	}))

	// Joint fuse allocator: EV + battery charge share the import budget.
	jf := defaultGoldenState(ModePlannerCheap)
	jf.UseEnergyDispatch = true
	jf.EVChargingW = 6000
	out = append(out, mk("fuse_joint_ev_allocator_saturates", goldenInputs{
		FuseMaxW: 11040, GridW: 7000,
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.40), gb("sungrow", 9600, 0, 0.35)},
		State:     jf,
		Slot:      slotFor(1500, "cheap"),
	}))

	// MaxExportW ceiling tighter than the fuse on a discharge slot.
	me := defaultGoldenState(ModePlannerArbitrage)
	me.UseEnergyDispatch = true
	me.MaxExportW = 8000
	out = append(out, mk("max_export_ceiling_scales_discharge", goldenInputs{
		FuseMaxW: 11040, GridW: -3000,
		PV:        []goldenPV{{Driver: "pv-0", PowerW: -1500}},
		Batteries: []goldenBattery{gb("ferroamp", 15200, -1000, 0.85), gb("sungrow", 9600, -1000, 0.85)},
		State:     me,
		Slot:      slotFor(-1500, "arbitrage"),
	}))

	// PeakImportCeilingW tighter than the fuse in manual charge mode.
	pi := defaultGoldenState(ModeCharge)
	pi.PeakImportCeilingW = 6000
	out = append(out, mk("peak_import_ceiling_scales_charge", goldenInputs{
		FuseMaxW: 11040, GridW: 4000,
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.30)},
		State:     pi,
	}))

	// Stale plan on planner_cheap: fallback grid target 0, PlanStale latched.
	sp := defaultGoldenState(ModePlannerCheap)
	sp.UseEnergyDispatch = true
	out = append(out, mk("stale_plan_fallback_planner_cheap", goldenInputs{
		FuseMaxW: 11040, GridW: 900,
		Batteries: []goldenBattery{gb("ferroamp", 15200, 0, 0.55)},
		State:     sp,
		Slot:      staleSlot(),
	}))

	return out
}

// ---- seeded families ------------------------------------------------------

var goldenNames = []string{"ferroamp", "sungrow", "pixii"}

func roundW(v float64) float64 { return math.Round(v) }
func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round1(v float64) float64 { return math.Round(v*10) / 10 }

func seededBatteries(rng *rand.Rand, n int, socMin, socMax float64) []goldenBattery {
	caps := []float64{5000, 9600, 10000, 15200}
	out := make([]goldenBattery, 0, n)
	for i := 0; i < n; i++ {
		b := gb(goldenNames[i], caps[rng.Intn(len(caps))],
			roundW(rng.Float64()*8000-4000),
			round2(socMin+(socMax-socMin)*rng.Float64()))
		out = append(out, b)
	}
	return out
}

func seededReactive() []goldenScenario {
	modes := []Mode{ModeSelfConsumption, ModePeakShaving, ModeCharge, ModeIdle, ModePriority, ModeWeighted}
	out := make([]goldenScenario, 0, 100)
	for i := 0; i < 100; i++ {
		seed := int64(1000 + i)
		rng := rand.New(rand.NewSource(seed))
		mode := modes[i%len(modes)]
		n := 1 + rng.Intn(3)
		bats := seededBatteries(rng, n, 0.05, 0.95)
		if n > 1 && rng.Float64() < 0.20 {
			bats[n-1].Online = false
		}
		for j := range bats {
			if rng.Float64() < 0.30 {
				limitVals := []float64{2000, 3000, 5000, 8000}
				bats[j].MaxChargeW = limitVals[rng.Intn(len(limitVals))]
				bats[j].MaxDischargeW = limitVals[rng.Intn(len(limitVals))]
			}
		}
		st := defaultGoldenState(mode)
		if rng.Float64() < 0.30 {
			st.SlewRateW = float64(500 + rng.Intn(1500))
		}
		if rng.Float64() < 0.10 {
			st.SlewEnabled = false
		}
		if rng.Float64() < 0.15 {
			st.GridTargetW = roundW(rng.Float64()*1000 - 500)
		}
		if mode == ModePeakShaving {
			st.PeakLimitW = roundW(3000 + rng.Float64()*4000)
		}
		if mode == ModePriority {
			order := make([]string, len(bats))
			for j, b := range bats {
				order[j] = b.Driver
			}
			rng.Shuffle(len(order), func(a, b int) { order[a], order[b] = order[b], order[a] })
			st.PriorityOrder = order
		}
		if mode == ModeWeighted {
			w := map[string]float64{}
			for _, b := range bats {
				w[b.Driver] = round1(0.5 + rng.Float64()*2.5)
			}
			st.Weights = w
		}
		in := goldenInputs{
			FuseMaxW:  11040,
			GridW:     roundW(rng.Float64()*16000 - 8000),
			Batteries: bats,
			State:     st,
		}
		if rng.Float64() < 0.50 {
			in.PV = []goldenPV{{Driver: "pv-0", PowerW: -roundW(rng.Float64() * 6000)}}
		}
		out = append(out, goldenScenario{
			ID: fmt.Sprintf("seeded_reactive/%03d_%s", i, mode), Seed: seed, Inputs: in,
		})
	}
	return out
}

func seededPlanner() []goldenScenario {
	modes := []Mode{ModePlannerSelf, ModePlannerCheap, ModePlannerPassiveArbitrage, ModePlannerArbitrage}
	strategies := map[Mode]string{
		ModePlannerSelf:             "self_consumption",
		ModePlannerCheap:            "cheap",
		ModePlannerPassiveArbitrage: "passive_arbitrage",
		ModePlannerArbitrage:        "arbitrage",
	}
	out := make([]goldenScenario, 0, 100)
	for i := 0; i < 100; i++ {
		seed := int64(2000 + i)
		rng := rand.New(rand.NewSource(seed))
		mode := modes[i%len(modes)]
		n := 1 + rng.Intn(2)
		bats := seededBatteries(rng, n, 0.05, 0.95)
		st := defaultGoldenState(mode)
		st.GridToleranceW = 60
		st.UseEnergyDispatch = true

		in := goldenInputs{
			FuseMaxW:  11040,
			GridW:     roundW(rng.Float64()*16000 - 8000),
			Batteries: bats,
			State:     st,
		}
		if rng.Float64() < 0.60 {
			in.PV = []goldenPV{{Driver: "pv-0", PowerW: -roundW(rng.Float64() * 7000)}}
		}

		useLegacy := (mode == ModePlannerCheap || mode == ModePlannerArbitrage) && rng.Float64() < 0.15
		switch {
		case useLegacy:
			in.State.UseEnergyDispatch = false
			lpModes := []string{"self_consumption", "charge"}
			lp := &goldenLegacyPlan{Mode: lpModes[rng.Intn(2)]}
			if lp.Mode == "self_consumption" {
				lp.GridW = roundW(rng.Float64()*4000 - 2000)
			}
			in.LegacyPlan = lp
		case rng.Float64() < 0.12:
			in.Slot = staleSlot()
		default:
			slot := &goldenSlot{
				Present:    true,
				Strategy:   strategies[mode],
				ElapsedS:   roundW(60 + rng.Float64()*640),
				RemainingS: roundW(200 + rng.Float64()*600),
			}
			r := rng.Float64()
			switch {
			case r < 0.40:
				slot.BatteryEnergyWh = roundW(300 + rng.Float64()*2700)
			case r < 0.65:
				slot.BatteryEnergyWh = roundW(rng.Float64()*80 - 40)
			default:
				slot.BatteryEnergyWh = -roundW(300 + rng.Float64()*2700)
			}
			if rng.Float64() < 0.50 {
				slot.HasPlannedGridW = true
				slot.PlannedGridW = roundW(rng.Float64()*6000 - 3000)
			}
			if rng.Float64() < 0.15 {
				slot.LivePVSurplusSoCCapPct = roundW(60 + rng.Float64()*35)
			}
			in.Slot = slot
		}
		if rng.Float64() < 0.10 {
			in.State.PVSurplusAbsorbSoCCapPct = roundW(70 + rng.Float64()*20)
		}
		out = append(out, goldenScenario{
			ID: fmt.Sprintf("seeded_planner/%03d_%s", i, mode), Seed: seed, Inputs: in,
		})
	}
	return out
}

func seededFuse() []goldenScenario {
	out := make([]goldenScenario, 0, 80)
	for i := 0; i < 80; i++ {
		seed := int64(3000 + i)
		rng := rand.New(rand.NewSource(seed))
		importSide := i%2 == 0
		fuseA := 16.0
		fuseMaxW := 11040.0
		if i%4 >= 2 {
			fuseA = 25.0
			fuseMaxW = 17250.0
		}
		var gridW float64
		if importSide {
			gridW = roundW(fuseMaxW*0.7 + rng.Float64()*fuseMaxW*0.7)
		} else {
			gridW = -roundW(fuseMaxW*0.7 + rng.Float64()*fuseMaxW*0.7)
		}
		n := 1 + rng.Intn(3)
		bats := seededBatteries(rng, n, 0.20, 0.90)

		var st goldenStateInputs
		var slot *goldenSlot
		switch i % 4 {
		case 0:
			st = defaultGoldenState(ModeSelfConsumption)
		case 1:
			st = defaultGoldenState(ModeCharge)
		case 2:
			st = defaultGoldenState(ModeIdle)
		case 3:
			st = defaultGoldenState(ModePlannerArbitrage)
			st.GridToleranceW = 60
			st.UseEnergyDispatch = true
			energy := 2500.0
			if !importSide {
				energy = -2500.0
			}
			slot = slotFor(energy, "arbitrage")
			slot.RemainingS = 300
		}
		if rng.Float64() < 0.50 {
			st.SiteFuseAmps = fuseA
			st.SiteFuseVoltage = 230
			st.SiteFusePhases = 3
			safeties := []float64{0, 0.5, 1}
			st.SiteFuseSafetyA = safeties[rng.Intn(len(safeties))]
			worst := round1(fuseA - 2 + rng.Float64()*8)
			pA := round1(rng.Float64() * (fuseA - 4))
			pB := round1(rng.Float64() * (fuseA - 4))
			ph := &goldenPhases{L1A: worst, L2A: pA, L3A: pB}
			if !importSide {
				ph.L1A, ph.L2A, ph.L3A = -ph.L1A, -ph.L2A, -ph.L3A
			}
			in := rng.Intn(3)
			if in == 1 {
				ph.L1A, ph.L2A = ph.L2A, ph.L1A
			} else if in == 2 {
				ph.L1A, ph.L3A = ph.L3A, ph.L1A
			}
			// Per-phase variant: the worst phase is rotated so the clamp
			// is not always exercised on L1.
			out = append(out, goldenScenario{
				ID:     fmt.Sprintf("seeded_fuse/%03d", i),
				Seed:   seed,
				Inputs: seededFuseInputs(rng, fuseMaxW, gridW, ph, bats, st, slot, importSide),
			})
			continue
		}
		out = append(out, goldenScenario{
			ID:     fmt.Sprintf("seeded_fuse/%03d", i),
			Seed:   seed,
			Inputs: seededFuseInputs(rng, fuseMaxW, gridW, nil, bats, st, slot, importSide),
		})
	}
	return out
}

func seededFuseInputs(rng *rand.Rand, fuseMaxW, gridW float64, ph *goldenPhases,
	bats []goldenBattery, st goldenStateInputs, slot *goldenSlot, importSide bool) goldenInputs {
	if rng.Float64() < 0.20 {
		st.PeakImportCeilingW = roundW(4000 + rng.Float64()*4000)
	}
	if rng.Float64() < 0.20 {
		st.MaxExportW = roundW(5000 + rng.Float64()*4000)
	}
	if importSide && rng.Float64() < 0.25 {
		st.EVChargingW = roundW(3000 + rng.Float64()*6000)
	}
	in := goldenInputs{
		FuseMaxW:    fuseMaxW,
		GridW:       gridW,
		MeterPhases: ph,
		Batteries:   bats,
		State:       st,
		Slot:        slot,
	}
	if !importSide {
		in.PV = []goldenPV{{Driver: "pv-0", PowerW: -roundW(2000 + rng.Float64()*8000)}}
	}
	return in
}

func seededSiblings() []goldenScenario {
	out := make([]goldenScenario, 0, 60)
	conditions := []string{"discharge_blocked", "charge_blocked", "offline", "device_fault", "soc_floor", "tiny_cap"}
	for i := 0; i < 60; i++ {
		seed := int64(4000 + i)
		rng := rand.New(rand.NewSource(seed))
		n := 2 + i%2
		bats := seededBatteries(rng, n, 0.30, 0.80)
		cond := conditions[i%len(conditions)]
		switch cond {
		case "discharge_blocked":
			bats[0].DischargeBlocked = true
		case "charge_blocked":
			bats[0].ChargeBlocked = true
		case "offline":
			bats[0].Online = false
		case "device_fault":
			bats[0].DeviceFault = true
		case "soc_floor":
			bats[0].SoC = 0.03
		case "tiny_cap":
			bats[0].MaxDischargeW = 300
			bats[0].MaxChargeW = 300
		}
		importDir := i%2 == 0
		var gridW float64
		if importDir {
			gridW = roundW(1500 + rng.Float64()*4500)
		} else {
			gridW = -roundW(1500 + rng.Float64()*4500)
		}
		st := defaultGoldenState(ModeSelfConsumption)
		var slot *goldenSlot
		if i%5 == 4 {
			st = defaultGoldenState(ModePlannerArbitrage)
			st.GridToleranceW = 60
			st.UseEnergyDispatch = true
			energy := -1200.0
			if !importDir {
				energy = 1200.0
			}
			slot = slotFor(energy, "arbitrage")
		}
		in := goldenInputs{
			FuseMaxW:  11040,
			GridW:     gridW,
			Batteries: bats,
			State:     st,
			Slot:      slot,
		}
		if i%3 == 0 {
			// DC-locality variant: tag groups + a group-local PV feed.
			for j := range in.Batteries {
				in.Batteries[j].Group = []string{"fa", "sg", "px"}[j]
			}
			in.State.InverterGroups = map[string]string{"pv-fa": "fa"}
			in.PV = []goldenPV{{Driver: "pv-fa", PowerW: -roundW(2000 + rng.Float64()*3000)}}
		} else if !importDir {
			in.PV = []goldenPV{{Driver: "pv-0", PowerW: -roundW(2000 + rng.Float64()*4000)}}
		}
		out = append(out, goldenScenario{
			ID: fmt.Sprintf("seeded_siblings/%03d_%s", i, cond), Seed: seed, Inputs: in,
		})
	}
	return out
}

func seededEVBoost() []goldenScenario {
	out := make([]goldenScenario, 0, 50)
	for i := 0; i < 50; i++ {
		seed := int64(5000 + i)
		rng := rand.New(rand.NewSource(seed))
		n := 1 + rng.Intn(2)
		bats := seededBatteries(rng, n, 0.20, 0.90)
		evW := roundW(2000 + rng.Float64()*6000)

		var st goldenStateInputs
		var slot *goldenSlot
		switch i % 3 {
		case 0:
			st = defaultGoldenState(ModeSelfConsumption)
		case 1:
			st = defaultGoldenState(ModePlannerCheap)
			st.GridToleranceW = 60
			st.UseEnergyDispatch = true
			slot = slotFor(roundW(500+rng.Float64()*2000), "cheap")
		case 2:
			st = defaultGoldenState(ModePlannerArbitrage)
			st.GridToleranceW = 60
			st.UseEnergyDispatch = true
			slot = slotFor(-roundW(500+rng.Float64()*2000), "arbitrage")
		}
		st.BatteryCoversEV = i%2 == 0

		in := goldenInputs{
			FuseMaxW:  11040,
			GridW:     roundW(rng.Float64()*12000 - 4000),
			Batteries: bats,
			State:     st,
			Slot:      slot,
		}
		if i%4 < 2 {
			// Manual/aggregate EV signal on state.
			in.State.EVChargingW = evW
		} else {
			// Live EV telemetry driver.
			in.EV = []goldenEV{{Driver: "ev-0", PowerW: evW}}
		}
		if rng.Float64() < 0.30 {
			in.State.EVSurplusOnlyReserveW = roundW(1500 + rng.Float64()*2500)
			in.GridW = -roundW(1000 + rng.Float64()*5000)
			in.PV = []goldenPV{{Driver: "pv-0", PowerW: -roundW(3000 + rng.Float64()*5000)}}
		}
		if rng.Float64() < 0.30 {
			reserve := round2(0.30 + rng.Float64()*0.40)
			in.State.BatteryBoostReserveSoC = reserve
			in.State.BatteryBoostEVChargingW = evW
			for j := range in.Batteries {
				in.Batteries[j].SoC = round2(clampF(reserve+rng.Float64()*0.2-0.1, 0.02, 0.98))
			}
		}
		out = append(out, goldenScenario{
			ID: fmt.Sprintf("seeded_ev_boost/%03d", i), Seed: seed, Inputs: in,
		})
	}
	return out
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ---- dump + validation ----------------------------------------------------

func ftwCommit() string {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	s := string(out)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func writeGoldenFamily(t *testing.T, dir, family string, records []goldenRecord) {
	t.Helper()
	f := goldenFile{
		Family:      family,
		GeneratedBy: "FTW_GOLDEN_DUMP=1 go test ./internal/control/ -run TestGoldenDump",
		FTWCommit:   ftwCommit(),
		RecordCount: len(records),
		Records:     records,
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", family, err)
	}
	b = append(b, '\n')
	path := filepath.Join(dir, family+".json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Logf("wrote %s (%d records, %d bytes)", path, len(records), len(b))
}

func TestGoldenDump(t *testing.T) {
	if os.Getenv("FTW_GOLDEN_DUMP") != "1" {
		t.Skip("set FTW_GOLDEN_DUMP=1 to record the golden corpus")
	}
	outDir := os.Getenv("FTW_GOLDEN_OUT")
	if outDir == "" {
		outDir = goldenCorpusDir
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", outDir, err)
	}

	// Keyed by the same family names the replay test reads back, so a new
	// family cannot be recorded without also being replayed.
	builders := map[string]func() []goldenScenario{
		"incident_scenarios":  incidentScenarios,
		"targeted_invariants": targetedScenarios,
		"seeded_reactive":     seededReactive,
		"seeded_planner":      seededPlanner,
		"seeded_fuse":         seededFuse,
		"seeded_siblings":     seededSiblings,
		"seeded_ev_boost":     seededEVBoost,
	}
	families := goldenFamilyNames
	for _, name := range families {
		if builders[name] == nil {
			t.Fatalf("family %q is replayed but has no recorder", name)
		}
	}
	if len(builders) != len(families) {
		t.Fatalf("recorder has %d families, replay expects %d", len(builders), len(families))
	}

	total := 0
	for _, name := range families {
		scenarios := builders[name]()
		records := make([]goldenRecord, 0, len(scenarios))
		for _, sc := range scenarios {
			records = append(records, runGoldenScenario(sc))
		}
		writeGoldenFamily(t, outDir, name, records)
		total += len(records)
	}
	if total < 300 {
		t.Fatalf("golden corpus too small: %d records (want >= 300)", total)
	}

	// Validate what was just written: re-read it from disk and run the same
	// coverage assertions the replay test runs on the committed corpus.
	all, byID := readGoldenCorpus(t, outDir)
	t.Logf("golden corpus: %d records across %d families", len(all), len(families))
	assertGoldenCoverage(t, all, byID)
}
