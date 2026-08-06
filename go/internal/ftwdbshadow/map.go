package ftwdbshadow

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/srcfl/ftw/go/internal/state"
)

var ErrMappedBatchTooLarge = errors.New("mapped FTWDB shadow batch is too large")

type MappedBatchTooLargeError struct {
	SourceRows int
	Points     int
	Metadata   int
}

func (e *MappedBatchTooLargeError) Error() string {
	return fmt.Sprintf("%v: source_rows=%d points=%d metadata=%d", ErrMappedBatchTooLarge,
		e.SourceRows, e.Points, e.Metadata)
}

func (e *MappedBatchTooLargeError) Unwrap() error { return ErrMappedBatchTooLarge }

// MapOptions contains only stable identity. SiteName remains for callers that
// already have it, but v1 does not put that mutable label in replay bytes.
type MapOptions struct {
	SourceID ID128
	SiteKey  string
	SiteName string
}

// MapShadowBatch maps one settled SQLite source window to one deterministic
// FTWDB transaction. CommitID stays zero: PrepareExportCommit hashes the exact
// canonical zero-ID wire frame and freezes it for retry.
func MapShadowBatch(options MapOptions, source state.ShadowExportBatch) (CommitBatchRequest, error) {
	if options.SourceID.IsZero() {
		return CommitBatchRequest{}, errors.New("map shadow batch: source id is zero")
	}
	if strings.TrimSpace(options.SiteKey) == "" {
		return CommitBatchRequest{}, errors.New("map shadow batch: site key is empty")
	}
	if source.AfterMS < 0 || source.CutoffMS <= source.AfterMS {
		return CommitBatchRequest{}, fmt.Errorf("map shadow batch: invalid cursor window (%d,%d]",
			source.AfterMS, source.CutoffMS)
	}
	if source.RowCount() == 0 {
		return CommitBatchRequest{}, errors.New("map shadow batch: source window is empty")
	}

	driverDevices, err := shadowDriverDeviceMap(source.DriverDevices)
	if err != nil {
		return CommitBatchRequest{}, err
	}
	m := shadowMapper{
		options:       options,
		entities:      make(map[ID128]Entity),
		series:        make(map[uint64]SeriesDefinition),
		runs:          make(map[ID128]Run),
		plans:         make(map[ID128]Plan),
		driverDevices: driverDevices,
	}
	m.siteID = shadowID("entity", "site", options.SiteKey)
	m.addEntity(Entity{
		ID: m.siteID, Kind: "site", Name: boundedShadowText(options.SiteKey), ValidFrom: 0,
		Properties: map[string]PropertyValue{
			"ftw_site_key": TextProperty(boundedShadowText(options.SiteKey)),
			"power_sign":   TextProperty("positive_into_site"),
		},
	})

	if err := m.mapHistory(source.History); err != nil {
		return CommitBatchRequest{}, err
	}
	if err := m.mapSamples(source.Samples); err != nil {
		return CommitBatchRequest{}, err
	}
	if err := m.mapEnergy(source.Energy); err != nil {
		return CommitBatchRequest{}, err
	}
	if err := m.mapDiagnostics(source.Diagnostics); err != nil {
		return CommitBatchRequest{}, err
	}
	if err := m.mapCommandResults(source.CommandResults); err != nil {
		return CommitBatchRequest{}, err
	}
	if err := m.mapPrices(source.Prices); err != nil {
		return CommitBatchRequest{}, err
	}
	if err := m.mapForecasts(source.Forecasts); err != nil {
		return CommitBatchRequest{}, err
	}
	if m.err != nil {
		return CommitBatchRequest{}, m.err
	}

	request := CommitBatchRequest{
		SourceID: options.SourceID,
		Sequence: uint64(source.CutoffMS),
		Entities: shadowEntityValues(m.entities),
		Series:   shadowSeriesValues(m.series),
		Runs:     shadowRunValues(m.runs),
		Plans:    shadowPlanValues(m.plans),
		Points:   m.points,
	}
	shadowSortPoints(request.Points)
	metadata := len(request.Entities) + len(request.Relations) + len(request.Series) +
		len(request.Runs) + len(request.Plans)
	if len(request.Points) > MaxBatchPoints || metadata > MaxMetadataRecords {
		return CommitBatchRequest{}, &MappedBatchTooLargeError{
			SourceRows: source.RowCount(), Points: len(request.Points), Metadata: metadata,
		}
	}
	if m.err != nil {
		return CommitBatchRequest{}, m.err
	}
	if _, err := Encode(request); err != nil {
		var protocolErr *ProtocolError
		if errors.As(err, &protocolErr) && protocolErr.Kind == ProtocolFrameTooLarge {
			return CommitBatchRequest{}, &MappedBatchTooLargeError{
				SourceRows: source.RowCount(), Points: len(request.Points), Metadata: metadata,
			}
		}
		return CommitBatchRequest{}, fmt.Errorf("map shadow batch: validate wire frame: %w", err)
	}
	return request, nil
}

type shadowMapper struct {
	options       MapOptions
	siteID        ID128
	entities      map[ID128]Entity
	series        map[uint64]SeriesDefinition
	runs          map[ID128]Run
	plans         map[ID128]Plan
	points        []Point
	err           error
	driverDevices map[string]shadowDriverIdentity
}

type shadowDriverIdentity struct {
	deviceID   string
	resolution state.ShadowDriverIdentityResolution
}

func (m *shadowMapper) mapHistory(rows []state.ShadowHistoryRow) error {
	for _, row := range rows {
		timestamp, err := shadowMillis("history timestamp", row.TsMS)
		if err != nil {
			return err
		}
		actuals := []struct {
			key, name, quantity, unit string
			value                     *float64
			scale                     float64
		}{
			{"history:grid_power", "grid_power", "power", "W", row.GridW, 1},
			{"history:pv_power", "pv_power", "power", "W", row.PVW, 1},
			{"history:battery_power", "battery_power", "power", "W", row.BatteryW, 1},
			{"history:load_power", "load_power", "power", "W", row.LoadW, 1},
			// history_hot stores SoC as 0..1; FTWDB's percent series stores 0..100.
			{"history:battery_soc", "battery_state_of_charge", "state_of_charge", "%", row.BatterySoC, 100},
		}
		for _, actual := range actuals {
			if actual.value == nil {
				continue
			}
			seriesID := m.ensureSeries(m.siteID, actual.key, actual.name, actual.quantity,
				actual.unit, SeriesGauge, shadowInt64Pointer(15_000_000))
			if err := m.addPoint(Point{SeriesID: seriesID, ValidTime: timestamp,
				ValidTimeEnd: timestamp, KnowledgeTime: timestamp, ChangeTime: timestamp,
				Value: *actual.value * actual.scale}); err != nil {
				return fmt.Errorf("map history %s: %w", actual.name, err)
			}
		}
		if len(row.Targets) == 0 && !row.TargetsMalformed {
			continue
		}
		runID := shadowID("run", "control_tick", m.options.SiteKey, strconv.FormatInt(row.TsMS, 10))
		runStatus := RunSucceeded
		if row.TargetsMalformed {
			runStatus = RunFailed
		}
		m.addRun(Run{
			ID: runID, Kind: RunControl, Status: runStatus,
			CreatedAt: timestamp, KnowledgeTime: timestamp,
			Workflow: "ftw.control.dispatch", Model: "core", ModelVersion: "state-v1",
			Attributes: map[string]PropertyValue{
				"target_count":      IntegerProperty(int64(len(row.Targets))),
				"targets_malformed": BoolProperty(row.TargetsMalformed),
			},
		})
		drivers := make([]string, 0, len(row.Targets))
		for driver := range row.Targets {
			drivers = append(drivers, driver)
		}
		sort.Strings(drivers)
		for _, driver := range drivers {
			owner, stable := m.driverEntity(driver)
			seriesKey := "control:target:" + driver
			if stable {
				seriesKey = "control:target_power"
			}
			seriesID := m.ensureSeries(owner, seriesKey, "dispatch_target_power",
				"power", "W", SeriesGauge, shadowInt64Pointer(15_000_000))
			if err := m.addPoint(Point{SeriesID: seriesID, ValidTime: timestamp,
				ValidTimeEnd: timestamp, KnowledgeTime: timestamp, ChangeTime: timestamp,
				RunID: runID, Value: row.Targets[driver]}); err != nil {
				return fmt.Errorf("map dispatch target %q: %w", driver, err)
			}
		}
	}
	return nil
}

func (m *shadowMapper) mapSamples(rows []state.ShadowSampleRow) error {
	for _, row := range rows {
		timestamp, err := shadowMillis("sample timestamp", row.TsMS)
		if err != nil {
			return err
		}
		owner, stable := m.driverEntity(row.Driver)
		unit, quantity := shadowUnitAndQuantity(row.Unit)
		seriesKey := "sample:" + row.Driver + "\x00" + row.Metric + "\x00" + unit
		if stable {
			seriesKey = "sample:" + row.Metric + "\x00" + unit
		}
		seriesID := m.ensureSeries(owner, seriesKey,
			"telemetry."+boundedShadowText(row.Metric), quantity, unit, SeriesGauge, nil)
		if err := m.addPoint(Point{SeriesID: seriesID, ValidTime: timestamp,
			ValidTimeEnd: timestamp, KnowledgeTime: timestamp, ChangeTime: timestamp,
			Value: row.Value}); err != nil {
			return fmt.Errorf("map sample %q/%q: %w", row.Driver, row.Metric, err)
		}
	}
	return nil
}

func (m *shadowMapper) mapEnergy(rows []state.ShadowEnergyRow) error {
	for _, row := range rows {
		start, err := shadowMillis("energy bucket start", row.BucketStartMS)
		if err != nil {
			return err
		}
		length, err := shadowMillisDuration("energy bucket length", row.BucketLenMS)
		if err != nil {
			return err
		}
		if start > math.MaxInt64-length {
			return errors.New("map energy: bucket end overflows microseconds")
		}
		observed, err := shadowMillis("energy observed time", row.ObservedAtMS)
		if err != nil {
			return err
		}
		owner, err := m.energyAssetEntity(row)
		if err != nil {
			return err
		}
		seriesID := m.ensureSeries(owner, "energy:"+row.AssetID+"\x00"+row.Flow,
			"energy."+boundedShadowText(row.Flow), "energy", "Wh", SeriesIntervalTotal, nil)
		runID := shadowID("run", "energy_import", m.options.SiteKey, row.AssetID, row.Flow,
			row.Source, row.Quality, row.Provenance, strconv.FormatInt(row.BucketStartMS, 10),
			strconv.FormatInt(row.BucketLenMS, 10), strconv.FormatInt(row.ObservedAtMS, 10))
		m.addRun(Run{
			ID: runID, Kind: RunImport, Status: RunSucceeded,
			CreatedAt: observed, KnowledgeTime: observed,
			Workflow: "ftw.energy_ledger", Model: boundedShadowText(row.Source), ModelVersion: "1",
			Attributes: map[string]PropertyValue{
				"asset_id":             TextProperty(boundedShadowText(row.AssetID)),
				"asset_metadata_known": BoolProperty(row.AssetMetadataKnown),
				"flow":                 TextProperty(boundedShadowText(row.Flow)),
				"provenance":           TextProperty(boundedShadowText(row.Provenance)),
				"quality":              TextProperty(boundedShadowText(row.Quality)),
				"sample_count":         IntegerProperty(row.SampleCount),
				"schema_version":       IntegerProperty(int64(row.SchemaVersion)),
			},
		})
		if err := m.addPoint(Point{SeriesID: seriesID, ValidTime: start,
			ValidTimeEnd: start + length, KnowledgeTime: observed, ChangeTime: observed,
			RunID: runID, Value: row.EnergyWh, Quality: shadowEnergyQuality(row.Quality)}); err != nil {
			return fmt.Errorf("map energy %q/%q: %w", row.AssetID, row.Flow, err)
		}
	}
	return nil
}

type shadowDiagnostic struct {
	ComputedAtMS int64 `json:"computed_at_ms"`
	Params       struct {
		Mode string `json:"mode"`
	} `json:"params"`
	Slots []shadowDiagnosticSlot `json:"slots"`
}

type shadowDiagnosticSlot struct {
	SlotStartMS int64 `json:"slot_start_ms"`
	SlotEndMS   int64 `json:"slot_end_ms"`
	LenMin      int   `json:"len_min"`

	PriceOre   float64 `json:"price_ore"`
	SpotOre    float64 `json:"spot_ore"`
	Confidence float64 `json:"confidence"`
	PVW        float64 `json:"pv_w"`
	LoadW      float64 `json:"load_w"`

	BatteryW     float64 `json:"battery_w"`
	GridW        float64 `json:"grid_w"`
	SoCPct       float64 `json:"soc_pct"`
	CostOre      float64 `json:"cost_ore"`
	Reason       string  `json:"reason"`
	EMSMode      string  `json:"ems_mode"`
	PVLimitW     float64 `json:"pv_limit_w"`
	LoadpointW   float64 `json:"loadpoint_w"`
	LoadpointSoC float64 `json:"loadpoint_soc_pct"`

	LoadpointPowerW  map[string]float64 `json:"loadpoint_power_w"`
	LoadpointSoCByID map[string]float64 `json:"loadpoint_soc_pct_by_id"`
	StoragePowerW    map[string]float64 `json:"storage_power_w"`
	StorageEnergyWh  map[string]float64 `json:"storage_energy_wh"`
}

func (m *shadowMapper) mapDiagnostics(rows []state.ShadowDiagnosticRow) error {
	for _, row := range rows {
		knowledge, err := shadowMillis("diagnostic timestamp", row.TsMS)
		if err != nil {
			return err
		}
		runID := shadowID("run", "optimization", m.options.SiteKey, strconv.FormatInt(row.TsMS, 10))
		attributes := map[string]PropertyValue{
			"reason":            TextProperty(boundedShadowText(row.Reason)),
			"zone":              TextProperty(boundedShadowText(row.Zone)),
			"horizon_slots":     IntegerProperty(int64(row.HorizonSlots)),
			"diagnostic_sha256": TextProperty(shadowDigest(row.JSON)),
		}
		var diagnostic shadowDiagnostic
		if err := json.Unmarshal([]byte(row.JSON), &diagnostic); err != nil {
			attributes["diagnostic_json_valid"] = BoolProperty(false)
			m.addRun(Run{ID: runID, Kind: RunOptimization, Status: RunFailed,
				CreatedAt: knowledge, KnowledgeTime: knowledge, Workflow: "ftw.mpc",
				Model: "planner_diagnostic", ModelVersion: "state-v1", Attributes: attributes})
			continue
		}
		attributes["diagnostic_json_valid"] = BoolProperty(true)
		m.addRun(Run{ID: runID, Kind: RunOptimization, Status: RunSucceeded,
			CreatedAt: knowledge, KnowledgeTime: knowledge, Workflow: "ftw.mpc",
			Model: "planner_diagnostic", ModelVersion: "state-v1", Attributes: attributes})
		if len(diagnostic.Slots) == 0 {
			continue
		}

		horizonStart, horizonEnd, resolution, err := shadowPlanHorizon(diagnostic.Slots)
		if err != nil {
			return fmt.Errorf("map diagnostic at %d: %w", row.TsMS, err)
		}
		planID := shadowID("plan", "optimization", m.options.SiteKey, strconv.FormatInt(row.TsMS, 10))
		scenario := strings.TrimSpace(diagnostic.Params.Mode)
		if scenario == "" {
			scenario = strings.TrimSpace(row.Reason)
		}
		if scenario == "" {
			scenario = "default"
		}
		objective := row.TotalCostOre
		if !shadowFinite(objective) {
			return fmt.Errorf("map diagnostic at %d: non-finite objective", row.TsMS)
		}
		m.addPlan(Plan{
			// A planner diagnostic proves a candidate plan, not dispatch or
			// hardware application. Command results carry outcome evidence.
			ID: planID, RunID: runID, Status: PlanCandidate,
			HorizonStart: horizonStart, HorizonEnd: horizonEnd, ResolutionMicros: resolution,
			Scenario: boundedShadowText(scenario), ObjectiveTerms: map[string]float64{"total_cost_minor_unit": objective},
			ObjectiveValue: &objective,
			Attributes: map[string]PropertyValue{
				"reason": TextProperty(boundedShadowText(row.Reason)),
				"zone":   TextProperty(boundedShadowText(row.Zone)),
			},
		})
		for slotIndex, slot := range diagnostic.Slots {
			if err := m.mapDiagnosticSlot(runID, planID, knowledge, slotIndex, slot); err != nil {
				return fmt.Errorf("map diagnostic at %d slot %d: %w", row.TsMS, slotIndex, err)
			}
		}
	}
	return nil
}

func shadowPlanHorizon(slots []shadowDiagnosticSlot) (int64, int64, int64, error) {
	var start, end, resolution int64
	for index, slot := range slots {
		slotStart, slotEnd, duration, err := shadowSlotTimes(slot.SlotStartMS, slot.SlotEndMS, slot.LenMin)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("slot %d: %w", index, err)
		}
		if index == 0 || slotStart < start {
			start = slotStart
		}
		if slotEnd > end {
			end = slotEnd
		}
		if resolution == 0 {
			resolution = duration
		} else {
			resolution = shadowGCD(resolution, duration)
		}
	}
	if end <= start || resolution <= 0 {
		return 0, 0, 0, errors.New("plan has no positive horizon")
	}
	return start, end, resolution, nil
}

func (m *shadowMapper) mapDiagnosticSlot(optimizerRunID, planID ID128, knowledge int64, index int,
	slot shadowDiagnosticSlot) error {
	start, end, _, err := shadowSlotTimes(slot.SlotStartMS, slot.SlotEndMS, slot.LenMin)
	if err != nil {
		return err
	}
	decisionRunID := shadowID("run", "plan_slot_decision", m.options.SiteKey,
		planID.String(), strconv.Itoa(index))
	parentRun := optimizerRunID
	m.addRun(Run{
		ID: decisionRunID, Kind: RunOptimization, Status: RunSucceeded,
		CreatedAt: knowledge, KnowledgeTime: knowledge, ParentRun: &parentRun,
		Workflow: "ftw.mpc.slot_decision", Model: "planner_slot", ModelVersion: "state-v1",
		Attributes: map[string]PropertyValue{
			"parent_run_id": TextProperty(optimizerRunID.String()),
			"plan_id":       TextProperty(planID.String()),
			"slot_index":    IntegerProperty(int64(index)),
			"reason":        TextProperty(boundedShadowText(slot.Reason)),
			"ems_mode":      TextProperty(boundedShadowText(slot.EMSMode)),
		},
	})
	values := []struct {
		key, name, quantity, unit string
		value                     float64
	}{
		{"plan:battery_power", "planned_battery_power", "power", "W", slot.BatteryW},
		{"plan:grid_power", "planned_grid_power", "power", "W", slot.GridW},
		{"plan:battery_soc", "planned_battery_state_of_charge", "state_of_charge", "%", slot.SoCPct},
		{"plan:slot_cost", "planned_slot_cost", "cost", "minor_currency_unit", slot.CostOre},
		{"plan:price", "planner_input_total_price", "energy_price", "minor_currency_unit/kWh", slot.PriceOre},
		{"plan:spot", "planner_input_spot_price", "energy_price", "minor_currency_unit/kWh", slot.SpotOre},
		{"plan:confidence", "planner_input_confidence", "ratio", "1", slot.Confidence},
		{"plan:pv_power", "planner_input_pv_power", "power", "W", slot.PVW},
		{"plan:load_power", "planner_input_load_power", "power", "W", slot.LoadW},
	}
	if slot.PVLimitW != 0 {
		values = append(values, struct {
			key, name, quantity, unit string
			value                     float64
		}{"plan:pv_limit", "planned_pv_limit", "power", "W", slot.PVLimitW})
	}
	if slot.LoadpointW != 0 || slot.LoadpointSoC != 0 {
		values = append(values,
			struct {
				key, name, quantity, unit string
				value                     float64
			}{"plan:loadpoint_power", "planned_loadpoint_power", "power", "W", slot.LoadpointW},
			struct {
				key, name, quantity, unit string
				value                     float64
			}{"plan:loadpoint_soc", "planned_loadpoint_state_of_charge", "state_of_charge", "%", slot.LoadpointSoC})
	}
	for _, value := range values {
		seriesID := m.ensureSeries(m.siteID, value.key, value.name, value.quantity, value.unit,
			SeriesGauge, nil)
		if err := m.addPoint(Point{SeriesID: seriesID, ValidTime: start, ValidTimeEnd: end,
			KnowledgeTime: knowledge, ChangeTime: knowledge, RunID: decisionRunID, Value: value.value}); err != nil {
			return fmt.Errorf("%s: %w", value.name, err)
		}
	}
	if err := m.mapDiagnosticHardwareValues(decisionRunID, knowledge, start, end, "loadpoint",
		slot.LoadpointPowerW, "planned_loadpoint_power", "power", "W"); err != nil {
		return err
	}
	if err := m.mapDiagnosticHardwareValues(decisionRunID, knowledge, start, end, "loadpoint",
		slot.LoadpointSoCByID, "planned_loadpoint_state_of_charge", "state_of_charge", "%"); err != nil {
		return err
	}
	if err := m.mapDiagnosticHardwareValues(decisionRunID, knowledge, start, end, "storage",
		slot.StoragePowerW, "planned_storage_power", "power", "W"); err != nil {
		return err
	}
	if err := m.mapDiagnosticHardwareValues(decisionRunID, knowledge, start, end, "storage",
		slot.StorageEnergyWh, "planned_storage_energy", "energy", "Wh"); err != nil {
		return err
	}
	return nil
}

func (m *shadowMapper) mapDiagnosticHardwareValues(runID ID128, knowledge, start, end int64,
	role string, values map[string]float64, name, quantity, unit string) error {
	ids := make([]string, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		owner := m.hardwareEntity(role, id)
		seriesID := m.ensureSeries(owner, "plan:"+role+":"+id+":"+name,
			name, quantity, unit, SeriesGauge, nil)
		if err := m.addPoint(Point{SeriesID: seriesID, ValidTime: start, ValidTimeEnd: end,
			KnowledgeTime: knowledge, ChangeTime: knowledge, RunID: runID, Value: values[id]}); err != nil {
			return fmt.Errorf("%s %q: %w", role, id, err)
		}
	}
	return nil
}

func (m *shadowMapper) mapCommandResults(rows []state.ShadowCommandResultRow) error {
	for _, row := range rows {
		timestamp, err := shadowMillis("command completion time", row.CompletedAtMS)
		if err != nil {
			return err
		}
		owner, stable := m.driverEntity(row.Driver)
		runID := shadowID("run", "driver_command", m.options.SiteKey, row.ID)
		status, outcome, quality := shadowCommandStatus(row.Status)
		resultText, truncated := boundedShadowTextWithFlag(row.ResultJSON)
		m.addRun(Run{
			ID: runID, Kind: RunControl, Status: status,
			CreatedAt: timestamp, KnowledgeTime: timestamp,
			Workflow: "ftw.driver_command", Model: boundedShadowText(row.Command), ModelVersion: "v2",
			Attributes: map[string]PropertyValue{
				"command_id":            TextProperty(boundedShadowText(row.ID)),
				"driver":                TextProperty(boundedShadowText(row.Driver)),
				"status":                TextProperty(boundedShadowText(row.Status)),
				"code":                  TextProperty(boundedShadowText(row.Code)),
				"result_json":           TextProperty(resultText),
				"result_json_sha256":    TextProperty(shadowDigest(row.ResultJSON)),
				"result_json_truncated": BoolProperty(truncated),
			},
		})
		seriesKey := "command:" + row.Driver + "\x00" + row.Command
		if stable {
			seriesKey = "command:" + row.Command
		}
		seriesID := m.ensureSeries(owner, seriesKey,
			"command."+boundedShadowText(row.Command)+".outcome", "control_outcome", "1",
			SeriesEvent, nil)
		if err := m.addPoint(Point{SeriesID: seriesID, ValidTime: timestamp,
			ValidTimeEnd: timestamp, KnowledgeTime: timestamp, ChangeTime: timestamp,
			RunID: runID, Value: outcome, Quality: quality}); err != nil {
			return fmt.Errorf("map command result %q: %w", row.ID, err)
		}
	}
	return nil
}

func (m *shadowMapper) mapPrices(rows []state.ShadowPriceRow) error {
	for _, row := range rows {
		valid, err := shadowMillis("price slot time", row.SlotTsMS)
		if err != nil {
			return err
		}
		duration, err := shadowMinutes("price slot length", row.SlotLenMin)
		if err != nil {
			return err
		}
		if valid > math.MaxInt64-duration {
			return errors.New("map price: slot end overflows microseconds")
		}
		knowledge, err := shadowMillis("price fetch time", row.FetchedAtMS)
		if err != nil {
			return err
		}
		runID := m.importRun("price", row.Source, row.FetchedAtMS, knowledge)
		values := []struct {
			kind, name string
			value      float64
		}{
			{"spot", "spot_price", row.SpotOreKWh},
			{"total", "total_price", row.TotalOreKWh},
		}
		for _, value := range values {
			seriesID := m.ensureSeries(m.siteID, "price:"+row.Zone+":"+value.kind,
				"price."+boundedShadowText(row.Zone)+"."+value.name, "energy_price", "minor_currency_unit/kWh",
				SeriesGauge, nil)
			if err := m.addPoint(Point{SeriesID: seriesID, ValidTime: valid,
				ValidTimeEnd: valid + duration, KnowledgeTime: knowledge, ChangeTime: knowledge,
				RunID: runID, Value: value.value}); err != nil {
				return fmt.Errorf("map price %q/%s: %w", row.Zone, value.kind, err)
			}
		}
	}
	return nil
}

func (m *shadowMapper) mapForecasts(rows []state.ShadowForecastRow) error {
	for _, row := range rows {
		valid, err := shadowMillis("forecast slot time", row.SlotTsMS)
		if err != nil {
			return err
		}
		duration, err := shadowMinutes("forecast slot length", row.SlotLenMin)
		if err != nil {
			return err
		}
		if valid > math.MaxInt64-duration {
			return errors.New("map forecast: slot end overflows microseconds")
		}
		knowledge, err := shadowMillis("forecast fetch time", row.FetchedAtMS)
		if err != nil {
			return err
		}
		runID := m.forecastRun(row.Source, row.FetchedAtMS, knowledge)
		values := []struct {
			key, name, quantity, unit string
			value                     *float64
			sign                      float64
		}{
			{"forecast:cloud_cover", "forecast.cloud_cover", "cloud_cover", "%", row.CloudCoverPct, 1},
			{"forecast:temperature", "forecast.temperature", "temperature", "C", row.TempC, 1},
			{"forecast:solar_irradiance", "forecast.solar_irradiance", "irradiance", "W/m2", row.SolarWM2, 1},
			// The cache stores a non-negative generation magnitude. Convert at
			// this one boundary to FTW's site sign: generation is negative.
			{"forecast:pv_power", "forecast.pv_power", "power", "W", row.PVWEstimated, -1},
		}
		for _, value := range values {
			if value.value == nil {
				continue
			}
			seriesID := m.ensureSeries(m.siteID, value.key, value.name, value.quantity,
				value.unit, SeriesGauge, nil)
			if err := m.addPoint(Point{SeriesID: seriesID, ValidTime: valid,
				ValidTimeEnd: valid + duration, KnowledgeTime: knowledge, ChangeTime: knowledge,
				RunID: runID, Value: *value.value * value.sign}); err != nil {
				return fmt.Errorf("map forecast %s: %w", value.name, err)
			}
		}
	}
	return nil
}

func (m *shadowMapper) hardwareEntity(role, identity string) ID128 {
	id := shadowID("entity", "hardware", m.options.SiteKey, role, identity)
	parent := m.siteID
	name := strings.TrimSpace(identity)
	if name == "" {
		name = "unknown-" + role
	}
	m.addEntity(Entity{
		ID: id, Kind: "hardware", Name: boundedShadowText(name), Parent: &parent, ValidFrom: 0,
		Properties: map[string]PropertyValue{
			"ftw_identity": TextProperty(boundedShadowText(identity)),
			"ftw_role":     TextProperty(boundedShadowText(role)),
		},
	})
	return id
}

const shadowObservedConsumerAssetID = "site/observed-consumer"

func (m *shadowMapper) energyAssetEntity(row state.ShadowEnergyRow) (ID128, error) {
	assetID := row.AssetID
	if strings.TrimSpace(assetID) == "" {
		return ID128{}, errors.New("map energy: empty asset id")
	}
	deviceID := row.AssetDeviceID
	if strings.TrimSpace(deviceID) == "" {
		deviceID = ""
	}
	kind := strings.TrimSpace(row.AssetKind)
	label := strings.TrimSpace(row.AssetLabel)
	readOnly := row.AssetReadOnly
	synthetic := assetID == shadowObservedConsumerAssetID
	if synthetic {
		if row.AssetMetadataKnown && (deviceID != "" || kind != "observed_consumer" || !readOnly) {
			return ID128{}, errors.New("map energy: site/observed-consumer has invalid hardware metadata")
		}
		deviceID = ""
		kind = "observed_consumer"
		readOnly = true
		if label == "" {
			label = "Observed load"
		}
	}
	if row.AssetMetadataKnown && kind == "" {
		return ID128{}, fmt.Errorf("map energy %q: known asset has empty kind", assetID)
	}

	parent := m.siteID
	if row.AssetMetadataKnown && deviceID != "" {
		parent = m.deviceHardwareEntity(deviceID)
	}
	name := label
	if name == "" {
		name = assetID
	}
	properties := map[string]PropertyValue{
		"ftw_asset_id":             TextProperty(boundedShadowText(assetID)),
		"ftw_asset_metadata_known": BoolProperty(row.AssetMetadataKnown),
		"ftw_synthetic":            BoolProperty(synthetic),
	}
	if row.AssetMetadataKnown || synthetic {
		properties["ftw_asset_kind"] = TextProperty(boundedShadowText(kind))
		properties["ftw_label"] = TextProperty(boundedShadowText(label))
		properties["ftw_read_only"] = BoolProperty(readOnly)
	}
	if deviceID != "" {
		properties["ftw_device_id"] = TextProperty(boundedShadowText(deviceID))
	}
	id := shadowID("entity", "energy_asset", m.options.SiteKey, assetID)
	parentCopy := parent
	m.addEntity(Entity{
		ID: id, Kind: "energy_asset", Name: boundedShadowText(name), Parent: &parentCopy,
		ValidFrom: 0, Properties: properties,
	})
	return id, nil
}

func (m *shadowMapper) deviceHardwareEntity(deviceID string) ID128 {
	id := shadowID("entity", "hardware", m.options.SiteKey, "device_id", deviceID)
	parent := m.siteID
	m.addEntity(Entity{
		ID: id, Kind: "hardware", Name: boundedShadowText(deviceID), Parent: &parent, ValidFrom: 0,
		Properties: map[string]PropertyValue{
			"ftw_device_id":     TextProperty(boundedShadowText(deviceID)),
			"ftw_identity":      TextProperty(boundedShadowText(deviceID)),
			"ftw_identity_kind": TextProperty("device_id"),
			"ftw_role":          TextProperty("device"),
		},
	})
	return id
}

func shadowDriverDeviceMap(rows []state.ShadowDriverDevice) (map[string]shadowDriverIdentity, error) {
	devices := make(map[string]shadowDriverIdentity, len(rows))
	for _, row := range rows {
		deviceID := row.DeviceID
		if row.Driver == "" {
			return nil, errors.New("map shadow batch: empty driver identity")
		}
		switch row.Resolution {
		case state.ShadowDriverIdentityStable:
			if strings.TrimSpace(deviceID) == "" {
				return nil, fmt.Errorf("map shadow batch: stable driver %q has no device id", row.Driver)
			}
		case state.ShadowDriverIdentityAmbiguous, state.ShadowDriverIdentityMissing:
			if deviceID != "" {
				return nil, fmt.Errorf("map shadow batch: unresolved driver %q has a device id", row.Driver)
			}
		default:
			return nil, fmt.Errorf("map shadow batch: driver %q has invalid identity resolution", row.Driver)
		}
		if _, exists := devices[row.Driver]; exists {
			return nil, fmt.Errorf("map shadow batch: duplicate driver identity %q", row.Driver)
		}
		devices[row.Driver] = shadowDriverIdentity{deviceID: deviceID, resolution: row.Resolution}
	}
	return devices, nil
}

// driverEntity uses hardware only for one exact device_id match.
// Missing and ambiguous names stay configured-driver entities and never claim
// a hardware identity.
func (m *shadowMapper) driverEntity(driver string) (ID128, bool) {
	identity, found := m.driverDevices[driver]
	if found && identity.resolution == state.ShadowDriverIdentityStable {
		return m.deviceHardwareEntity(identity.deviceID), true
	}
	resolution := "missing"
	if found && identity.resolution == state.ShadowDriverIdentityAmbiguous {
		resolution = "ambiguous"
	}
	id := shadowID("entity", "configured_driver", m.options.SiteKey, driver)
	parent := m.siteID
	name := strings.TrimSpace(driver)
	if name == "" {
		name = "unknown-driver"
	}
	m.addEntity(Entity{
		ID: id, Kind: "configured_driver", Name: boundedShadowText(name), Parent: &parent, ValidFrom: 0,
		Properties: map[string]PropertyValue{
			"ftw_driver_name":     TextProperty(boundedShadowText(driver)),
			"ftw_identity":        TextProperty(boundedShadowText(driver)),
			"ftw_identity_kind":   TextProperty("driver_name"),
			"identity_resolution": TextProperty(resolution),
		},
	})
	return id, false
}

func (m *shadowMapper) ensureSeries(owner ID128, key, name, quantity, unit string,
	semantics SeriesSemantics, maximumGap *int64) uint64 {
	id := shadowSeriesID(m.options.SiteKey, owner, key)
	ownerCopy := owner
	m.addSeries(SeriesDefinition{
		ID: id, OwnerEntity: &ownerCopy, Name: boundedShadowKey(name),
		PhysicalQuantity: boundedShadowKey(quantity), CanonicalUnit: boundedShadowKey(unit),
		Semantics: semantics, MaximumGapMicros: maximumGap,
		// Raw retention stays unbounded during the shadow gates. A later
		// release may add rollups only after deletion and restore proof.
		RollupPolicy: RollupPolicy{},
	})
	return id
}

func (m *shadowMapper) importRun(kind, sourceName string, fetchedMS, knowledge int64) ID128 {
	id := shadowID("run", "import", kind, m.options.SiteKey, sourceName, strconv.FormatInt(fetchedMS, 10))
	m.addRun(Run{
		ID: id, Kind: RunImport, Status: RunSucceeded,
		CreatedAt: knowledge, KnowledgeTime: knowledge,
		Workflow: "ftw." + kind + "_import", Model: boundedShadowText(sourceName), ModelVersion: "source-v1",
		Attributes: map[string]PropertyValue{"source": TextProperty(boundedShadowText(sourceName))},
	})
	return id
}

func (m *shadowMapper) forecastRun(sourceName string, fetchedMS, knowledge int64) ID128 {
	id := shadowID("run", "forecast", m.options.SiteKey, sourceName, strconv.FormatInt(fetchedMS, 10))
	m.addRun(Run{
		ID: id, Kind: RunForecast, Status: RunSucceeded,
		CreatedAt: knowledge, KnowledgeTime: knowledge,
		Workflow: "ftw.weather_forecast", Model: boundedShadowText(sourceName), ModelVersion: "source-v1",
		Attributes: map[string]PropertyValue{"source": TextProperty(boundedShadowText(sourceName))},
	})
	return id
}

func (m *shadowMapper) addEntity(value Entity) {
	if previous, ok := m.entities[value.ID]; ok && !reflect.DeepEqual(previous, value) {
		m.err = errors.New("map shadow batch: entity ID collision")
		return
	}
	m.entities[value.ID] = value
}

func (m *shadowMapper) addSeries(value SeriesDefinition) {
	if previous, ok := m.series[value.ID]; ok && !reflect.DeepEqual(previous, value) {
		m.err = errors.New("map shadow batch: series ID collision")
		return
	}
	m.series[value.ID] = value
}

func (m *shadowMapper) addRun(value Run) {
	if previous, ok := m.runs[value.ID]; ok && !reflect.DeepEqual(previous, value) {
		m.err = errors.New("map shadow batch: run ID collision")
		return
	}
	m.runs[value.ID] = value
}

func (m *shadowMapper) addPlan(value Plan) {
	if previous, ok := m.plans[value.ID]; ok && !reflect.DeepEqual(previous, value) {
		m.err = errors.New("map shadow batch: plan ID collision")
		return
	}
	m.plans[value.ID] = value
}

func (m *shadowMapper) addPoint(point Point) error {
	if m.err != nil {
		return m.err
	}
	if point.SeriesID == 0 {
		return errors.New("series id is zero")
	}
	if point.ValidTime < 0 || point.ValidTimeEnd < point.ValidTime ||
		point.KnowledgeTime < 0 || point.ChangeTime < 0 {
		return errors.New("point has invalid time")
	}
	if !shadowFinite(point.Value) {
		return errors.New("point value is not finite")
	}
	m.points = append(m.points, point)
	return nil
}

func shadowID(parts ...string) ID128 {
	hash := sha256.New()
	hash.Write([]byte("ftwdb-shadow-id-v1\x00"))
	var length [4]byte
	for _, part := range parts {
		binary.LittleEndian.PutUint32(length[:], uint32(len(part)))
		hash.Write(length[:])
		hash.Write([]byte(part))
	}
	var id ID128
	copy(id[:], hash.Sum(nil))
	if id.IsZero() {
		id[0] = 1
	}
	return id
}

func shadowSeriesID(siteKey string, owner ID128, key string) uint64 {
	hash := sha256.New()
	hash.Write([]byte("ftwdb-shadow-series-v1\x00"))
	hash.Write([]byte(siteKey))
	hash.Write([]byte{0})
	hash.Write(owner[:])
	hash.Write([]byte{0})
	hash.Write([]byte(key))
	id := binary.LittleEndian.Uint64(hash.Sum(nil)[:8])
	if id == 0 {
		return 1
	}
	return id
}

func shadowMillis(field string, milliseconds int64) (int64, error) {
	if milliseconds < 0 || milliseconds > math.MaxInt64/1_000 {
		return 0, fmt.Errorf("map shadow %s: milliseconds overflow microseconds", field)
	}
	return milliseconds * 1_000, nil
}

func shadowMillisDuration(field string, milliseconds int64) (int64, error) {
	if milliseconds <= 0 || milliseconds > math.MaxInt64/1_000 {
		return 0, fmt.Errorf("map shadow %s: invalid millisecond duration", field)
	}
	return milliseconds * 1_000, nil
}

func shadowMinutes(field string, minutes int) (int64, error) {
	if minutes <= 0 || int64(minutes) > math.MaxInt64/60_000_000 {
		return 0, fmt.Errorf("map shadow %s: invalid minute duration", field)
	}
	return int64(minutes) * 60_000_000, nil
}

func shadowSlotTimes(startMS, endMS int64, lengthMinutes int) (int64, int64, int64, error) {
	start, err := shadowMillis("plan slot start", startMS)
	if err != nil {
		return 0, 0, 0, err
	}
	var end int64
	if endMS > startMS {
		end, err = shadowMillis("plan slot end", endMS)
		if err != nil {
			return 0, 0, 0, err
		}
	} else {
		duration, durationErr := shadowMinutes("plan slot length", lengthMinutes)
		if durationErr != nil {
			return 0, 0, 0, durationErr
		}
		if start > math.MaxInt64-duration {
			return 0, 0, 0, errors.New("plan slot end overflows microseconds")
		}
		end = start + duration
	}
	if end <= start {
		return 0, 0, 0, errors.New("plan slot has no positive interval")
	}
	return start, end, end - start, nil
}

func shadowGCD(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func shadowUnitAndQuantity(raw string) (string, string) {
	unit := strings.TrimSpace(raw)
	if unit == "" {
		return "1", "measurement"
	}
	switch strings.ToLower(unit) {
	case "w", "kw", "mw":
		return unit, "power"
	case "wh", "kwh", "mwh":
		return unit, "energy"
	case "v", "kv":
		return unit, "electric_potential"
	case "a", "ma", "ka":
		return unit, "electric_current"
	case "hz":
		return unit, "frequency"
	case "%", "pct":
		return unit, "ratio"
	case "c", "°c":
		return unit, "temperature"
	case "var", "kvar":
		return unit, "reactive_power"
	case "va", "kva":
		return unit, "apparent_power"
	default:
		return boundedShadowKey(unit), "measurement"
	}
}

func shadowEnergyQuality(value string) uint32 {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "measured", "integrated":
		return 0
	case "recovered":
		return 1
	case "gap":
		return 2
	case "reset":
		return 3
	case "invalid":
		return 4
	default:
		return 5
	}
}

func shadowCommandStatus(value string) (RunStatus, float64, uint32) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "applied", "defaulted":
		return RunSucceeded, 1, 0
	case "expired":
		return RunCancelled, 0, 3
	case "accepted":
		return RunPending, 0, 1
	case "rejected":
		return RunFailed, 0, 2
	default:
		return RunFailed, 0, 4
	}
}

func shadowFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func shadowDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func boundedShadowText(value string) string {
	bounded, _ := boundedShadowTextWithFlag(value)
	return bounded
}

func boundedShadowTextWithFlag(value string) (string, bool) {
	return boundedShadowUTF8(value, MaxTextBytes)
}

func boundedShadowKey(value string) string {
	bounded, _ := boundedShadowUTF8(value, maxKeyBytes)
	return bounded
}

func boundedShadowUTF8(value string, maximum int) (string, bool) {
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "�")
	}
	if len(value) <= maximum {
		return value, false
	}
	value = value[:maximum]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value, true
}

func shadowInt64Pointer(value int64) *int64 {
	return &value
}

func shadowEntityValues(values map[ID128]Entity) []Entity {
	out := make([]Entity, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].ID[:], out[j].ID[:]) < 0 })
	return out
}

func shadowSeriesValues(values map[uint64]SeriesDefinition) []SeriesDefinition {
	out := make([]SeriesDefinition, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func shadowRunValues(values map[ID128]Run) []Run {
	out := make([]Run, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].ID[:], out[j].ID[:]) < 0 })
	return out
}

func shadowPlanValues(values map[ID128]Plan) []Plan {
	out := make([]Plan, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].ID[:], out[j].ID[:]) < 0 })
	return out
}

func shadowSortPoints(points []Point) {
	sort.Slice(points, func(i, j int) bool {
		a, b := points[i], points[j]
		switch {
		case a.SeriesID != b.SeriesID:
			return a.SeriesID < b.SeriesID
		case a.ValidTime != b.ValidTime:
			return a.ValidTime < b.ValidTime
		case a.ValidTimeEnd != b.ValidTimeEnd:
			return a.ValidTimeEnd < b.ValidTimeEnd
		case a.KnowledgeTime != b.KnowledgeTime:
			return a.KnowledgeTime < b.KnowledgeTime
		case a.ChangeTime != b.ChangeTime:
			return a.ChangeTime < b.ChangeTime
		case a.RunID != b.RunID:
			return bytes.Compare(a.RunID[:], b.RunID[:]) < 0
		case math.Float64bits(a.Value) != math.Float64bits(b.Value):
			return math.Float64bits(a.Value) < math.Float64bits(b.Value)
		case a.Quality != b.Quality:
			return a.Quality < b.Quality
		default:
			return a.Flags < b.Flags
		}
	})
}
