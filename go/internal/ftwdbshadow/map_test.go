package ftwdbshadow

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestMapShadowBatchIsDeterministicAndKeepsEnergyMeaning(t *testing.T) {
	sourceID := ID128{1, 2, 3}
	grid, pv, battery, load, soc := 120.0, -500.0, 300.0, 380.0, 0.54
	cloud, temp, solar, forecastPV := 25.0, 12.0, 450.0, 900.0
	source := state.ShadowExportBatch{
		AfterMS: 900, CutoffMS: 1_007,
		DriverDevices: []state.ShadowDriverDevice{
			{Driver: "battery-a", DeviceID: "maker:battery-serial", Resolution: state.ShadowDriverIdentityStable},
			{Driver: "meter-a", DeviceID: "maker:meter-serial", Resolution: state.ShadowDriverIdentityStable},
		},
		History: []state.ShadowHistoryRow{{
			TsMS: 1_000, GridW: &grid, PVW: &pv, BatteryW: &battery, LoadW: &load,
			BatterySoC: &soc, Targets: map[string]float64{"battery-a": -750},
		}},
		Samples: []state.ShadowSampleRow{
			{Driver: "meter-a", Metric: "voltage", Unit: "V", TsMS: 1_002, Value: 231},
			{Driver: "battery-a", Metric: "power", Unit: "W", TsMS: 1_001, Value: -710},
		},
		Energy: []state.ShadowEnergyRow{{
			SchemaVersion: 1, AssetID: "battery-a", Flow: "battery_charge",
			BucketStartMS: 500, BucketLenMS: 300_000, EnergyWh: 12.5,
			Source: "hardware_counter", Quality: "recovered", Provenance: "counter_gap",
			SampleCount: 2, ObservedAtMS: 1_003,
		}},
		Diagnostics: []state.ShadowDiagnosticRow{{
			TsMS: 1_004, Reason: "periodic", Zone: "SE3", TotalCostOre: 32.5, HorizonSlots: 1,
			JSON: `{"computed_at_ms":1004,"params":{"mode":"passive_arbitrage"},"slots":[{` +
				`"slot_start_ms":2000,"slot_end_ms":902000,"len_min":15,` +
				`"price_ore":110,"spot_ore":50,"confidence":0.8,"pv_w":-900,"load_w":600,` +
				`"battery_w":300,"grid_w":0,"soc_pct":55,"cost_ore":12,"reason":"charge",` +
				`"ems_mode":"passive_arbitrage","storage_power_w":{"battery-a":300},` +
				`"storage_energy_wh":{"battery-a":5500}}]}`,
		}},
		CommandResults: []state.ShadowCommandResultRow{{
			ID: "command-00000001", Driver: "battery-a", Command: "set_power",
			Status: "applied", Code: "ok", CompletedAtMS: 1_005,
			ResultJSON: `{"status":"applied","applied":{"power_w":300}}`,
		}},
		Prices: []state.ShadowPriceRow{{
			Zone: "SE3", SlotTsMS: 2_000, SlotLenMin: 15, SpotOreKWh: 50,
			TotalOreKWh: 110, Source: "nordpool", FetchedAtMS: 1_006,
		}},
		Forecasts: []state.ShadowForecastRow{{
			SlotTsMS: 2_000, SlotLenMin: 60, CloudCoverPct: &cloud, TempC: &temp,
			SolarWM2: &solar, PVWEstimated: &forecastPV, Source: "weather", FetchedAtMS: 1_007,
		}},
	}

	first, err := MapShadowBatch(MapOptions{SourceID: sourceID, SiteKey: "site-public-key", SiteName: "old label"}, source)
	if err != nil {
		t.Fatal(err)
	}
	// Source row order and mutable display labels must not change replay bytes.
	reordered := source
	reordered.Samples = []state.ShadowSampleRow{source.Samples[1], source.Samples[0]}
	second, err := MapShadowBatch(MapOptions{SourceID: sourceID, SiteKey: "site-public-key", SiteName: "new label"}, reordered)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("mapping changed with source row order or mutable label")
	}
	if first.Sequence != 1_007 || !first.CommitID.IsZero() {
		t.Fatalf("sequence/commit = %d/%s", first.Sequence, first.CommitID.String())
	}
	preparedFirst, err := PrepareExportCommit(first)
	if err != nil {
		t.Fatalf("mapped batch does not encode: %v", err)
	}
	preparedSecond, err := PrepareExportCommit(second)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(preparedFirst.Bytes(), preparedSecond.Bytes()) || preparedFirst.CommitID().IsZero() {
		t.Fatal("mapped replay did not produce one stable nonzero commit frame")
	}

	series := seriesByName(first.Series)
	points := pointsBySeries(first.Points)
	assertPointValue(t, points, series["grid_power"].ID, 120)
	assertPointValue(t, points, series["pv_power"].ID, -500)
	assertPointValue(t, points, series["battery_state_of_charge"].ID, 54)
	assertPointValue(t, points, series["forecast.pv_power"].ID, -900)
	if got := series["telemetry.power"]; got.CanonicalUnit != "W" || got.PhysicalQuantity != "power" {
		t.Fatalf("sample series = %#v", got)
	}
	energySeries := series["energy.battery_charge"]
	if energySeries.Semantics != SeriesIntervalTotal || energySeries.CanonicalUnit != "Wh" {
		t.Fatalf("energy series = %#v", energySeries)
	}
	energyPoint := points[energySeries.ID][0]
	if energyPoint.ValidTime != 500_000 || energyPoint.ValidTimeEnd != 300_500_000 ||
		energyPoint.KnowledgeTime != 1_003_000 || energyPoint.Quality != 1 {
		t.Fatalf("energy point = %#v", energyPoint)
	}
	pricePoint := points[series["price.SE3.total_price"].ID][0]
	if series["price.SE3.total_price"].CanonicalUnit != "minor_currency_unit/kWh" {
		t.Fatalf("price mapper invented a currency: %#v", series["price.SE3.total_price"])
	}
	if pricePoint.ValidTime != 2_000_000 || pricePoint.ValidTimeEnd != 902_000_000 ||
		pricePoint.KnowledgeTime != 1_006_000 {
		t.Fatalf("price point = %#v", pricePoint)
	}

	if len(first.Plans) != 1 || first.Plans[0].Status != PlanCandidate {
		t.Fatalf("planner diagnostic asserted deployment: %#v", first.Plans)
	}
	runs := make(map[ID128]Run, len(first.Runs))
	var optimizerRun, slotDecisionRun Run
	for _, run := range first.Runs {
		runs[run.ID] = run
		switch run.Workflow {
		case "ftw.mpc":
			optimizerRun = run
		case "ftw.mpc.slot_decision":
			slotDecisionRun = run
		}
	}
	if optimizerRun.ID.IsZero() || slotDecisionRun.ID.IsZero() {
		t.Fatalf("optimizer/slot decision runs missing: %#v", first.Runs)
	}
	if first.Plans[0].RunID != optimizerRun.ID || slotDecisionRun.ParentRun == nil ||
		*slotDecisionRun.ParentRun != optimizerRun.ID {
		t.Fatalf("plan decision lineage = plan=%#v decision=%#v", first.Plans[0], slotDecisionRun)
	}
	if got := slotDecisionRun.Attributes["parent_run_id"].Text; got != optimizerRun.ID.String() {
		t.Fatalf("slot parent_run_id = %q", got)
	}
	if got := slotDecisionRun.Attributes["plan_id"].Text; got != first.Plans[0].ID.String() {
		t.Fatalf("slot plan_id = %q", got)
	}
	if got := slotDecisionRun.Attributes["slot_index"].Integer; got != 0 {
		t.Fatalf("slot index = %d", got)
	}
	if got := slotDecisionRun.Attributes["reason"].Text; got != "charge" {
		t.Fatalf("slot reason = %q", got)
	}
	if got := slotDecisionRun.Attributes["ems_mode"].Text; got != "passive_arbitrage" {
		t.Fatalf("slot ems mode = %q", got)
	}
	seriesDefinitions := make(map[uint64]SeriesDefinition, len(first.Series))
	for _, definition := range first.Series {
		seriesDefinitions[definition.ID] = definition
	}
	for _, point := range first.Points {
		if !point.RunID.IsZero() {
			if _, ok := runs[point.RunID]; !ok {
				t.Fatalf("point refers to missing run %s", point.RunID.String())
			}
		}
		if point.ValidTimeEnd < point.ValidTime {
			t.Fatalf("negative interval: %#v", point)
		}
		name := seriesDefinitions[point.SeriesID].Name
		if (strings.HasPrefix(name, "planned_") || strings.HasPrefix(name, "planner_input_")) &&
			point.RunID != slotDecisionRun.ID {
			t.Fatalf("plan point %q has run %s, want slot decision %s",
				name, point.RunID.String(), slotDecisionRun.ID.String())
		}
	}
	for _, plan := range first.Plans {
		if run, ok := runs[plan.RunID]; !ok || run.Kind != RunOptimization {
			t.Fatalf("plan run = %#v, found=%v", run, ok)
		}
	}
	commandSeries := series["command.set_power.outcome"]
	commandPoint := points[commandSeries.ID][0]
	if commandPoint.Value != 1 || runs[commandPoint.RunID].Status != RunSucceeded {
		t.Fatalf("command outcome = %#v run=%#v", commandPoint, runs[commandPoint.RunID])
	}
	entities := make(map[ID128]bool, len(first.Entities))
	for _, entity := range first.Entities {
		entities[entity.ID] = true
	}
	for _, definition := range first.Series {
		if definition.OwnerEntity == nil || !entities[*definition.OwnerEntity] {
			t.Fatalf("series has missing owner: %#v", definition)
		}
	}
}

func TestMapShadowBatchUsesStableDeviceIdentityAcrossDriverRename(t *testing.T) {
	mapDriver := func(driver, deviceID string, resolution state.ShadowDriverIdentityResolution) CommitBatchRequest {
		request, err := MapShadowBatch(MapOptions{SourceID: ID128{1}, SiteKey: "site"}, state.ShadowExportBatch{
			AfterMS: 1, CutoffMS: 4,
			DriverDevices: []state.ShadowDriverDevice{{
				Driver: driver, DeviceID: deviceID, Resolution: resolution,
			}},
			History: []state.ShadowHistoryRow{{TsMS: 2, Targets: map[string]float64{driver: 10}}},
			Samples: []state.ShadowSampleRow{{Driver: driver, Metric: "power", Unit: "W", TsMS: 3, Value: 9}},
			CommandResults: []state.ShadowCommandResultRow{{
				ID: "command-1", Driver: driver, Command: "set_power", Status: "applied",
				Code: "ok", CompletedAtMS: 4, ResultJSON: `{}`,
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return request
	}

	beforeRename := mapDriver("battery-old", "maker:serial-1", state.ShadowDriverIdentityStable)
	afterRename := mapDriver("battery-new", "maker:serial-1", state.ShadowDriverIdentityStable)
	beforeEntity := hardwareEntityByDeviceID(t, beforeRename.Entities, "maker:serial-1")
	afterEntity := hardwareEntityByDeviceID(t, afterRename.Entities, "maker:serial-1")
	if !reflect.DeepEqual(beforeEntity, afterEntity) {
		t.Fatalf("device entity changed across rename:\nbefore=%#v\nafter=%#v", beforeEntity, afterEntity)
	}
	beforeSeries, afterSeries := seriesByName(beforeRename.Series), seriesByName(afterRename.Series)
	for _, name := range []string{"dispatch_target_power", "telemetry.power", "command.set_power.outcome"} {
		if beforeSeries[name].ID == 0 || beforeSeries[name].ID != afterSeries[name].ID {
			t.Fatalf("series %q changed across rename: %d != %d", name,
				beforeSeries[name].ID, afterSeries[name].ID)
		}
	}

	reusedByOtherDevice := mapDriver("battery-new", "maker:serial-2", state.ShadowDriverIdentityStable)
	otherEntity := hardwareEntityByDeviceID(t, reusedByOtherDevice.Entities, "maker:serial-2")
	if otherEntity.ID == afterEntity.ID ||
		seriesByName(reusedByOtherDevice.Series)["telemetry.power"].ID == afterSeries["telemetry.power"].ID {
		t.Fatal("reused driver name mixed two known device ids")
	}

	missing := mapDriver("unknown-driver", "", state.ShadowDriverIdentityMissing)
	missingEntity := configuredDriverEntity(t, missing.Entities, "missing")
	if missingEntity.ID.IsZero() || missingEntity.ID == beforeEntity.ID {
		t.Fatalf("missing configured driver = %#v", missingEntity)
	}
	ambiguous := mapDriver("reused-driver", "", state.ShadowDriverIdentityAmbiguous)
	ambiguousEntity := configuredDriverEntity(t, ambiguous.Entities, "ambiguous")
	if ambiguousEntity.ID.IsZero() || ambiguousEntity.Kind == "hardware" {
		t.Fatalf("ambiguous configured driver = %#v", ambiguousEntity)
	}
}

func TestMapShadowBatchMapsEnergyAssetsWithoutPretendingTheyAreHardware(t *testing.T) {
	request, err := MapShadowBatch(MapOptions{SourceID: ID128{1}, SiteKey: "site"}, state.ShadowExportBatch{
		AfterMS: 1_000, CutoffMS: 2_000,
		Energy: []state.ShadowEnergyRow{
			{SchemaVersion: 1, AssetID: "device-1/battery", AssetDeviceID: "device-1",
				AssetKind: "battery", AssetLabel: "Garage battery", AssetMetadataKnown: true,
				Flow: "battery_charge", BucketStartMS: 1_000, BucketLenMS: 300_000,
				EnergyWh: 12.5, Source: "counter", Quality: "measured", Provenance: "counter",
				SampleCount: 1, ObservedAtMS: 2_000},
			{SchemaVersion: 1, AssetID: "site/observed-consumer", AssetKind: "observed_consumer",
				AssetLabel: "Observed load", AssetReadOnly: true, AssetMetadataKnown: true,
				Flow: "consumer_use", BucketStartMS: 1_000, BucketLenMS: 300_000,
				EnergyWh: 50, Source: "power", Quality: "measured", Provenance: "integrated",
				SampleCount: 1, ObservedAtMS: 2_000},
			{SchemaVersion: 1, AssetID: "legacy-asset", Flow: "grid_import",
				BucketStartMS: 1_000, BucketLenMS: 300_000, EnergyWh: 2,
				Source: "legacy", Quality: "measured", Provenance: "legacy",
				SampleCount: 1, ObservedAtMS: 2_000},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	site := siteEntity(t, request.Entities)
	hardware := hardwareEntityByDeviceID(t, request.Entities, "device-1")
	battery := energyAssetEntityByID(t, request.Entities, "device-1/battery")
	consumer := energyAssetEntityByID(t, request.Entities, "site/observed-consumer")
	legacy := energyAssetEntityByID(t, request.Entities, "legacy-asset")
	if battery.Kind != "energy_asset" || battery.Parent == nil || *battery.Parent != hardware.ID ||
		battery.Properties["ftw_asset_kind"].Text != "battery" ||
		battery.Properties["ftw_label"].Text != "Garage battery" ||
		battery.Properties["ftw_read_only"].Bool {
		t.Fatalf("hardware-backed energy asset = %#v", battery)
	}
	if consumer.Kind != "energy_asset" || consumer.Parent == nil || *consumer.Parent != site.ID ||
		!consumer.Properties["ftw_synthetic"].Bool || !consumer.Properties["ftw_read_only"].Bool ||
		consumer.Properties["ftw_asset_kind"].Text != "observed_consumer" {
		t.Fatalf("synthetic consumer asset = %#v", consumer)
	}
	if legacy.Parent == nil || *legacy.Parent != site.ID || legacy.Properties["ftw_asset_metadata_known"].Bool {
		t.Fatalf("unknown legacy asset = %#v", legacy)
	}
	for _, point := range request.Points {
		if point.Value < 0 {
			t.Fatalf("directional ledger Wh changed sign: %#v", point)
		}
	}
}

func TestMapShadowBatchSiteCatalogDoesNotDependOnIngressSource(t *testing.T) {
	value := 1.0
	fast, err := MapShadowBatch(MapOptions{SourceID: ID128{1}, SiteKey: "same-site"}, state.ShadowExportBatch{
		AfterMS: 1, CutoffMS: 2,
		History: []state.ShadowHistoryRow{{TsMS: 2, GridW: &value}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := MapShadowBatch(MapOptions{SourceID: ID128{2}, SiteKey: "same-site"}, state.ShadowExportBatch{
		AfterMS: 1, CutoffMS: 3,
		Energy: []state.ShadowEnergyRow{{
			SchemaVersion: 1, AssetID: "asset", Flow: "grid_import", BucketStartMS: 1,
			BucketLenMS: 300_000, EnergyWh: 1, Source: "counter", Quality: "measured",
			Provenance: "counter", SampleCount: 1, ObservedAtMS: 3,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(siteEntity(t, fast.Entities), siteEntity(t, ledger.Entities)) {
		t.Fatalf("site catalog changed between ingress sources:\nfast=%#v\nledger=%#v",
			siteEntity(t, fast.Entities), siteEntity(t, ledger.Entities))
	}
	otherSource, err := MapShadowBatch(MapOptions{SourceID: ID128{9}, SiteKey: "same-site"}, state.ShadowExportBatch{
		AfterMS: 1, CutoffMS: 2,
		History: []state.ShadowHistoryRow{{TsMS: 2, GridW: &value}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fast.Entities, otherSource.Entities) || !reflect.DeepEqual(fast.Series, otherSource.Series) {
		t.Fatal("catalog depends on ingress source id")
	}
}

func TestMapShadowBatchKeepsMalformedTargetEvidence(t *testing.T) {
	value := 1.0
	request, err := MapShadowBatch(MapOptions{SourceID: ID128{1}, SiteKey: "site"}, state.ShadowExportBatch{
		AfterMS: 1, CutoffMS: 2,
		History: []state.ShadowHistoryRow{{TsMS: 2, GridW: &value, TargetsMalformed: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, run := range request.Runs {
		if run.Workflow == "ftw.control.dispatch" {
			found = true
			if run.Status != RunFailed || !run.Attributes["targets_malformed"].Bool {
				t.Fatalf("malformed target run = %#v", run)
			}
		}
	}
	if !found {
		t.Fatal("malformed targets left no control-run evidence")
	}
}

func TestMapShadowBatchRejectsTimeOverflowAndNonFiniteValues(t *testing.T) {
	value := 1.0
	tests := []struct {
		name  string
		batch state.ShadowExportBatch
		want  string
	}{
		{
			name: "millisecond overflow",
			batch: state.ShadowExportBatch{AfterMS: 1, CutoffMS: 2,
				History: []state.ShadowHistoryRow{{TsMS: math.MaxInt64/1_000 + 1, GridW: &value}}},
			want: "overflow",
		},
		{
			name: "non finite",
			batch: state.ShadowExportBatch{AfterMS: 1, CutoffMS: 2,
				Samples: []state.ShadowSampleRow{{Driver: "x", Metric: "x", TsMS: 2, Value: math.Inf(1)}}},
			want: "not finite",
		},
		{
			name: "zero interval",
			batch: state.ShadowExportBatch{AfterMS: 1, CutoffMS: 2,
				Forecasts: []state.ShadowForecastRow{{SlotTsMS: 2, SlotLenMin: 0, FetchedAtMS: 2}}},
			want: "invalid minute duration",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := MapShadowBatch(MapOptions{SourceID: ID128{1}, SiteKey: "site"}, test.batch)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestMapShadowBatchReturnsTypedSizeError(t *testing.T) {
	rows := make([]state.ShadowSampleRow, MaxBatchPoints+1)
	for index := range rows {
		rows[index] = state.ShadowSampleRow{
			Driver: "driver", Metric: "metric", Unit: "W", TsMS: int64(index + 2), Value: float64(index),
		}
	}
	_, err := MapShadowBatch(MapOptions{SourceID: ID128{1}, SiteKey: "site"}, state.ShadowExportBatch{
		AfterMS: 1, CutoffMS: int64(len(rows) + 2), Samples: rows,
	})
	if !errors.Is(err, ErrMappedBatchTooLarge) {
		t.Fatalf("size error = %v", err)
	}
	var sizeErr *MappedBatchTooLargeError
	if !errors.As(err, &sizeErr) || sizeErr.Points != len(rows) {
		t.Fatalf("typed size error = %#v", sizeErr)
	}
}

func seriesByName(series []SeriesDefinition) map[string]SeriesDefinition {
	out := make(map[string]SeriesDefinition, len(series))
	for _, definition := range series {
		out[definition.Name] = definition
	}
	return out
}

func pointsBySeries(points []Point) map[uint64][]Point {
	out := make(map[uint64][]Point)
	for _, point := range points {
		out[point.SeriesID] = append(out[point.SeriesID], point)
	}
	return out
}

func assertPointValue(t *testing.T, points map[uint64][]Point, seriesID uint64, want float64) {
	t.Helper()
	if seriesID == 0 || len(points[seriesID]) == 0 || points[seriesID][0].Value != want {
		t.Fatalf("series %d value = %#v, want %v", seriesID, points[seriesID], want)
	}
}

func siteEntity(t *testing.T, entities []Entity) Entity {
	t.Helper()
	for _, entity := range entities {
		if entity.Kind == "site" {
			return entity
		}
	}
	t.Fatal("site entity missing")
	return Entity{}
}

func hardwareEntityByDeviceID(t *testing.T, entities []Entity, deviceID string) Entity {
	t.Helper()
	for _, entity := range entities {
		if entity.Kind == "hardware" && entity.Properties["ftw_device_id"].Text == deviceID {
			return entity
		}
	}
	t.Fatalf("hardware device %q missing", deviceID)
	return Entity{}
}

func configuredDriverEntity(t *testing.T, entities []Entity, resolution string) Entity {
	t.Helper()
	for _, entity := range entities {
		if entity.Kind == "configured_driver" && entity.Properties["identity_resolution"].Text == resolution {
			return entity
		}
	}
	t.Fatalf("configured driver resolution %q missing", resolution)
	return Entity{}
}

func energyAssetEntityByID(t *testing.T, entities []Entity, assetID string) Entity {
	t.Helper()
	for _, entity := range entities {
		if entity.Kind == "energy_asset" && entity.Properties["ftw_asset_id"].Text == assetID {
			return entity
		}
	}
	t.Fatalf("energy asset %q missing", assetID)
	return Entity{}
}
