package state

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestShadowSourceReadsSettledStableRowsWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")
	cachePath := filepath.Join(dir, "cache.db")
	createShadowStateFixture(t, statePath)
	createShadowCacheFixture(t, cachePath)

	beforeNames := directoryNames(t, dir)
	beforeState := fileSnapshot(t, statePath)
	beforeCache := fileSnapshot(t, cachePath)

	source, err := OpenShadowSource(statePath)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := source.ReadAfter(context.Background(), ShadowReadOptions{
		AfterMS: 900, Now: time.UnixMilli(2_000), SettleDelay: time.Millisecond, MaxRows: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if batch.CutoffMS != 1_004 || batch.HasMore || batch.RowCount() != 7 {
		t.Fatalf("unexpected batch cutoff=%d more=%v rows=%d: %#v",
			batch.CutoffMS, batch.HasMore, batch.RowCount(), batch)
	}
	if batch.ApproxBytes <= 0 || batch.ApproxBytes > DefaultShadowMaxBytes {
		t.Fatalf("unbounded source byte estimate = %d", batch.ApproxBytes)
	}
	if len(batch.Energy) != 0 {
		t.Fatalf("fast stream read unindexed energy ledger: %#v", batch.Energy)
	}
	if got := batch.History[0].Targets; !reflect.DeepEqual(got, map[string]float64{"battery-a": -750}) {
		t.Fatalf("targets = %#v", got)
	}
	if batch.History[0].TargetsMalformed {
		t.Fatal("valid targets marked malformed")
	}
	if got := []string{batch.Samples[0].Driver + "/" + batch.Samples[0].Metric,
		batch.Samples[1].Driver + "/" + batch.Samples[1].Metric}; !reflect.DeepEqual(got, []string{"driver-a/power", "driver-b/voltage"}) {
		t.Fatalf("sample order = %v", got)
	}
	if _, err := source.state.Exec(`INSERT INTO history_hot(ts_ms, json) VALUES (3000, '{}')`); err == nil {
		t.Fatal("query-only source accepted a write")
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	if got := directoryNames(t, dir); !reflect.DeepEqual(got, beforeNames) {
		t.Fatalf("shadow source changed directory entries: before=%v after=%v", beforeNames, got)
	}
	if got := fileSnapshot(t, statePath); got != beforeState {
		t.Fatalf("shadow source changed state.db: before=%+v after=%+v", beforeState, got)
	}
	if got := fileSnapshot(t, cachePath); got != beforeCache {
		t.Fatalf("shadow source changed cache.db: before=%+v after=%+v", beforeCache, got)
	}
}

func TestShadowSourceUsesFullMillisecondGroupsAndClosesTransactions(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")
	db := createSQLiteFile(t, statePath)
	mustShadowExec(t, db, `CREATE TABLE history_hot (
		ts_ms INTEGER PRIMARY KEY, grid_w REAL, pv_w REAL, bat_w REAL, load_w REAL, bat_soc REAL, json TEXT NOT NULL)`)
	mustShadowExec(t, db, `CREATE TABLE ts_drivers(id INTEGER PRIMARY KEY, name TEXT NOT NULL)`)
	mustShadowExec(t, db, `CREATE TABLE ts_metrics(id INTEGER PRIMARY KEY, name TEXT NOT NULL, unit TEXT)`)
	mustShadowExec(t, db, `CREATE TABLE ts_samples(driver_id INTEGER, metric_id INTEGER, ts_ms INTEGER, value REAL)`)
	mustShadowExec(t, db, `CREATE TABLE planner_diagnostics (
		ts_ms INTEGER PRIMARY KEY, reason TEXT, zone TEXT, total_cost_ore REAL, horizon_slots INTEGER, json TEXT)`)
	mustShadowExec(t, db, `INSERT INTO history_hot VALUES (1000, 1, 2, 3, 4, 0.5, '{}')`)
	mustShadowExec(t, db, `INSERT INTO ts_drivers VALUES (1, 'driver')`)
	mustShadowExec(t, db, `INSERT INTO ts_metrics VALUES (1, 'power', 'W')`)
	mustShadowExec(t, db, `INSERT INTO ts_samples VALUES (1, 1, 1000, 6)`)
	mustShadowExec(t, db, `INSERT INTO planner_diagnostics VALUES (1001, 'tick', 'SE3', 2, 1, '{}')`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	source, err := OpenShadowSource(statePath)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := source.ReadAfter(context.Background(), ShadowReadOptions{
		AfterMS: 999, Now: time.UnixMilli(2_000), SettleDelay: time.Millisecond, MaxRows: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if batch.CutoffMS != 1_000 || !batch.HasMore || batch.RowCount() != 2 {
		t.Fatalf("split group: cutoff=%d more=%v rows=%d", batch.CutoffMS, batch.HasMore, batch.RowCount())
	}
	_, err = source.ReadAfter(context.Background(), ShadowReadOptions{
		AfterMS: 999, Now: time.UnixMilli(2_000), SettleDelay: time.Millisecond, MaxRows: 1,
	})
	if !errors.Is(err, ErrShadowMillisecondTooLarge) {
		t.Fatalf("oversize group error = %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	writer, err := sql.Open("sqlite", statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec(`UPDATE history_hot SET grid_w = 9 WHERE ts_ms = 1000`); err != nil {
		t.Fatalf("read transaction remained open: %v", err)
	}
}

func TestShadowSourceRequiresOneDistinctDeviceIDPerReferencedDriver(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")
	db := createSQLiteFile(t, statePath)
	mustShadowExec(t, db, `CREATE TABLE ts_drivers(id INTEGER PRIMARY KEY, name TEXT NOT NULL)`)
	mustShadowExec(t, db, `CREATE TABLE ts_metrics(id INTEGER PRIMARY KEY, name TEXT NOT NULL, unit TEXT)`)
	mustShadowExec(t, db, `CREATE TABLE ts_samples(driver_id INTEGER, metric_id INTEGER, ts_ms INTEGER, value REAL)`)
	mustShadowExec(t, db, `CREATE TABLE devices(
		device_id TEXT PRIMARY KEY, driver_name TEXT NOT NULL, last_seen_ms INTEGER NOT NULL)`)
	mustShadowExec(t, db, `INSERT INTO ts_drivers VALUES
		(1, 'reused'), (2, 'tie'), (3, 'legacy'), (4, 'unique')`)
	mustShadowExec(t, db, `INSERT INTO ts_metrics VALUES (1, 'power', 'W')`)
	mustShadowExec(t, db, `INSERT INTO ts_samples VALUES
		(1, 1, 1000, 1), (2, 1, 1000, 2), (3, 1, 1000, 3), (4, 1, 1000, 4)`)
	mustShadowExec(t, db, `INSERT INTO devices VALUES
		('device-old', 'reused', 100),
		('device-current', 'reused', 200),
		('device-tie-b', 'tie', 300),
		('device-tie-a', 'tie', 300),
		('device-unique', 'unique', 500),
		('device-unused', 'unused', 400)`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	source, err := OpenShadowSource(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	batch, err := source.ReadAfter(context.Background(), ShadowReadOptions{
		AfterMS: 999, Now: time.UnixMilli(2_000), SettleDelay: time.Millisecond, MaxRows: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []ShadowDriverDevice{
		{Driver: "legacy", Resolution: ShadowDriverIdentityMissing},
		{Driver: "reused", Resolution: ShadowDriverIdentityAmbiguous},
		{Driver: "tie", Resolution: ShadowDriverIdentityAmbiguous},
		{Driver: "unique", DeviceID: "device-unique", Resolution: ShadowDriverIdentityStable},
	}
	if !reflect.DeepEqual(batch.DriverDevices, want) {
		t.Fatalf("driver device map = %#v, want %#v", batch.DriverDevices, want)
	}
	if batch.RowCount() != 4 {
		t.Fatalf("device catalog changed source row boundary: rows=%d", batch.RowCount())
	}
}

func TestShadowSourceLedgerNeedsExplicitSlowStream(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")
	db := createSQLiteFile(t, statePath)
	mustShadowExec(t, db, `CREATE TABLE history_hot (
		ts_ms INTEGER PRIMARY KEY, grid_w REAL, pv_w REAL, bat_w REAL, load_w REAL, bat_soc REAL, json TEXT NOT NULL)`)
	// The bad shape proves the fast query does not even reference this table.
	mustShadowExec(t, db, `CREATE TABLE energy_ledger_entries(wrong_column INTEGER)`)
	mustShadowExec(t, db, `INSERT INTO history_hot VALUES (1000, 1, 2, 3, 4, 0.5, '{}')`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := OpenShadowSource(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.ReadAfter(context.Background(), ShadowReadOptions{
		AfterMS: 999, Now: time.UnixMilli(2_000), SettleDelay: time.Millisecond, MaxRows: 2,
	}); err != nil {
		t.Fatalf("fast stream touched ledger: %v", err)
	}
	_, err = source.ReadAfter(context.Background(), ShadowReadOptions{
		AfterMS: 999, Now: time.UnixMilli(2_000), SettleDelay: time.Millisecond, MaxRows: 2,
		EnergyLedgerOnly: true,
	})
	if err == nil || err.Error() != "shadow energy ledger poll interval must be at least 5m0s" {
		t.Fatalf("ledger stream without slow poll = %v", err)
	}
}

func TestShadowSourceReadsLedgerOnlyAndDoesNotCreateMissingCache(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")
	db := createSQLiteFile(t, statePath)
	mustShadowExec(t, db, `CREATE TABLE energy_ledger_entries (
		schema_version INTEGER, asset_id TEXT, flow TEXT, bucket_start_ms INTEGER, bucket_len_ms INTEGER,
		energy_wh REAL, source TEXT, quality TEXT, provenance TEXT, sample_count INTEGER, observed_at_ms INTEGER)`)
	mustShadowExec(t, db, `INSERT INTO energy_ledger_entries VALUES
		(1, 'battery-1', 'battery_charge', 1000, 300000, 12.5, 'hardware_counter', 'measured', 'counter', 2, 1200)`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	source, err := OpenShadowSource(statePath)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := source.ReadAfter(context.Background(), ShadowReadOptions{
		AfterMS: 1_000, Now: time.UnixMilli(2_000), SettleDelay: time.Millisecond, MaxRows: 2,
		EnergyLedgerOnly: true, LedgerPollInterval: MinimumShadowLedgerPollInterval,
	})
	if err != nil {
		t.Fatal(err)
	}
	if batch.RowCount() != 1 || len(batch.Energy) != 1 || batch.CutoffMS != 1_200 {
		t.Fatalf("ledger batch = %#v", batch)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cache.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing cache was created: %v", err)
	}
}

func TestShadowSourceJoinsEnergyAssetCatalogInLedgerSnapshot(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")
	db := createSQLiteFile(t, statePath)
	mustShadowExec(t, db, `CREATE TABLE energy_assets(
		asset_id TEXT PRIMARY KEY, device_id TEXT NOT NULL, kind TEXT NOT NULL,
		label TEXT NOT NULL, read_only INTEGER NOT NULL)`)
	mustShadowExec(t, db, `CREATE TABLE energy_ledger_entries (
		schema_version INTEGER, asset_id TEXT, flow TEXT, bucket_start_ms INTEGER, bucket_len_ms INTEGER,
		energy_wh REAL, source TEXT, quality TEXT, provenance TEXT, sample_count INTEGER, observed_at_ms INTEGER)`)
	mustShadowExec(t, db, `INSERT INTO energy_assets VALUES
		('device-1/battery', 'device-1', 'battery', 'Garage battery', 0),
		('site/observed-consumer', '', 'observed_consumer', 'Observed load', 1)`)
	mustShadowExec(t, db, `INSERT INTO energy_ledger_entries VALUES
		(1, 'device-1/battery', 'battery_charge', 1000, 300000, 12.5, 'counter', 'measured', 'counter', 2, 1200),
		(1, 'site/observed-consumer', 'consumer_use', 1000, 300000, 50, 'power', 'measured', 'integrated', 2, 1200)`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	source, err := OpenShadowSource(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	batch, err := source.ReadAfter(context.Background(), ShadowReadOptions{
		AfterMS: 1_000, Now: time.UnixMilli(2_000), SettleDelay: time.Millisecond, MaxRows: 4,
		EnergyLedgerOnly: true, LedgerPollInterval: MinimumShadowLedgerPollInterval,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Energy) != 2 {
		t.Fatalf("energy rows = %#v", batch.Energy)
	}
	byAsset := make(map[string]ShadowEnergyRow, len(batch.Energy))
	for _, row := range batch.Energy {
		byAsset[row.AssetID] = row
	}
	battery := byAsset["device-1/battery"]
	if !battery.AssetMetadataKnown || battery.AssetDeviceID != "device-1" ||
		battery.AssetKind != "battery" || battery.AssetLabel != "Garage battery" || battery.AssetReadOnly {
		t.Fatalf("battery asset metadata = %#v", battery)
	}
	consumer := byAsset["site/observed-consumer"]
	if !consumer.AssetMetadataKnown || consumer.AssetDeviceID != "" ||
		consumer.AssetKind != "observed_consumer" || consumer.AssetLabel != "Observed load" || !consumer.AssetReadOnly {
		t.Fatalf("consumer asset metadata = %#v", consumer)
	}
}

func TestOpenShadowSourceRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "real.db")
	db := createSQLiteFile(t, realPath)
	mustShadowExec(t, db, `CREATE TABLE history_hot(ts_ms INTEGER PRIMARY KEY, json TEXT NOT NULL)`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(dir, "state.db")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenShadowSource(linkPath); err == nil {
		t.Fatal("symlink state source was accepted")
	}
}

func TestShadowSourceReadsRealMigratedStoreBesideLiveWALWriter(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")
	store, err := Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := int64(1_800_000_000_000)
	if err := store.RecordHistory(HistoryPoint{TsMs: base + 1, GridW: 100, BatSoC: 0.5,
		JSON: `{"targets":{"battery":50}}`}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSamples([]Sample{{Driver: "battery", Metric: "power", Unit: "W", TsMs: base + 2, Value: 50}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDiagnostic(base+3, "test", "SE3", 1, 1, `{"slots":[]}`); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordDriverCommandResult("command-00000001", "battery", "set_power",
		"applied", "ok", base+4, []byte(`{"status":"applied"}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePrices([]PricePoint{{Zone: "SE3", SlotTsMs: base + 60_000,
		SlotLenMin: 15, SpotOreKwh: 10, TotalOreKwh: 20, Source: "test", FetchedAtMs: base + 5}}); err != nil {
		t.Fatal(err)
	}
	pv := 500.0
	if err := store.SaveForecasts([]ForecastPoint{{SlotTsMs: base + 60_000,
		SlotLenMin: 60, PVWEstimated: &pv, Source: "test", FetchedAtMs: base + 6}}); err != nil {
		t.Fatal(err)
	}
	mustShadowExec(t, store.db, `INSERT INTO energy_ledger_entries(
		schema_version, asset_id, flow, bucket_start_ms, bucket_len_ms, energy_wh,
		source, quality, provenance, sample_count, observed_at_ms
	) VALUES (1, 'battery', 'battery_charge', ?, 300000, 1, 'counter', 'measured', 'counter', 1, ?)`,
		base, base+7)

	// The active Store owns WAL/SHM. The shadow reader may use SQLite's shared
	// read marks, but it must add no cursor, outbox, marker, or other file.
	beforeNames := directoryNames(t, dir)
	source, err := OpenShadowSource(statePath)
	if err != nil {
		t.Fatal(err)
	}
	fast, err := source.ReadAfter(context.Background(), ShadowReadOptions{
		AfterMS: base, Now: time.UnixMilli(base + 100), SettleDelay: time.Millisecond, MaxRows: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fast.RowCount() != 6 || len(fast.Energy) != 0 {
		t.Fatalf("real migrated fast batch = %#v", fast)
	}
	ledger, err := source.ReadAfter(context.Background(), ShadowReadOptions{
		AfterMS: base, Now: time.UnixMilli(base + 100), SettleDelay: time.Millisecond, MaxRows: 20,
		EnergyLedgerOnly: true, LedgerPollInterval: MinimumShadowLedgerPollInterval,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ledger.RowCount() != 1 || len(ledger.Energy) != 1 {
		t.Fatalf("real migrated ledger batch = %#v", ledger)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if got := directoryNames(t, dir); !reflect.DeepEqual(got, beforeNames) {
		t.Fatalf("shadow reader added a persistent file beside live WAL: before=%v after=%v", beforeNames, got)
	}
	if err := store.RecordHistory(HistoryPoint{TsMs: base + 200, GridW: 101, JSON: `{}`}); err != nil {
		t.Fatalf("shadow read left the live writer blocked: %v", err)
	}
}

func TestShadowSourceHonorsSettleAndContext(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")
	db := createSQLiteFile(t, statePath)
	mustShadowExec(t, db, `CREATE TABLE history_hot (
		ts_ms INTEGER PRIMARY KEY, grid_w REAL, pv_w REAL, bat_w REAL, load_w REAL, bat_soc REAL, json TEXT NOT NULL)`)
	mustShadowExec(t, db, `INSERT INTO history_hot VALUES (1001, 1, 2, 3, 4, 5, '{bad json')`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := OpenShadowSource(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	batch, err := source.ReadAfter(context.Background(), ShadowReadOptions{
		AfterMS: 1_000, Now: time.UnixMilli(1_002), SettleDelay: 2 * time.Millisecond, MaxRows: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if batch.RowCount() != 0 || batch.SettledThroughMS != 1_000 {
		t.Fatalf("unsettled row leaked: %#v", batch)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.ReadAfter(ctx, ShadowReadOptions{
		AfterMS: 1_000, Now: time.UnixMilli(2_000), SettleDelay: time.Millisecond, MaxRows: 2,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled query error = %v", err)
	}
}

func TestShadowSourceCapsBatchAndJSONBytes(t *testing.T) {
	t.Run("batch bytes", func(t *testing.T) {
		dir := t.TempDir()
		statePath := filepath.Join(dir, "state.db")
		db := createSQLiteFile(t, statePath)
		mustShadowExec(t, db, `CREATE TABLE history_hot (
			ts_ms INTEGER PRIMARY KEY, grid_w REAL, pv_w REAL, bat_w REAL, load_w REAL, bat_soc REAL, json TEXT NOT NULL)`)
		mustShadowExec(t, db, `INSERT INTO history_hot VALUES (1000, 1, 2, 3, 4, 0.5, '{}')`)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		source, err := OpenShadowSource(statePath)
		if err != nil {
			t.Fatal(err)
		}
		defer source.Close()
		_, err = source.ReadAfter(context.Background(), ShadowReadOptions{
			AfterMS: 999, Now: time.UnixMilli(2_000), SettleDelay: time.Millisecond,
			MaxRows: 2, MaxBytes: 32,
		})
		if !errors.Is(err, ErrShadowBatchTooLarge) {
			t.Fatalf("batch byte error = %v", err)
		}
	})

	t.Run("json cell", func(t *testing.T) {
		dir := t.TempDir()
		statePath := filepath.Join(dir, "state.db")
		db := createSQLiteFile(t, statePath)
		mustShadowExec(t, db, `CREATE TABLE planner_diagnostics (
			ts_ms INTEGER PRIMARY KEY, reason TEXT, zone TEXT, total_cost_ore REAL, horizon_slots INTEGER, json TEXT)`)
		mustShadowExec(t, db, `INSERT INTO planner_diagnostics VALUES (1000, 'test', 'SE3', 1, 1, ?)`,
			strings.Repeat("x", MaxShadowRowTextBytes+1))
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		source, err := OpenShadowSource(statePath)
		if err != nil {
			t.Fatal(err)
		}
		defer source.Close()
		_, err = source.ReadAfter(context.Background(), ShadowReadOptions{
			AfterMS: 999, Now: time.UnixMilli(2_000), SettleDelay: time.Millisecond, MaxRows: 2,
		})
		if !errors.Is(err, ErrShadowBatchTooLarge) {
			t.Fatalf("JSON cell byte error = %v", err)
		}
	})
}

type shadowFileSnapshot struct {
	Size    int64
	Mode    os.FileMode
	ModTime int64
}

func fileSnapshot(t *testing.T, path string) shadowFileSnapshot {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return shadowFileSnapshot{Size: info.Size(), Mode: info.Mode(), ModTime: info.ModTime().UnixNano()}
}

func directoryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func createSQLiteFile(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	mustShadowExec(t, db, `PRAGMA journal_mode=DELETE`)
	return db
}

func mustShadowExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func createShadowStateFixture(t *testing.T, path string) {
	t.Helper()
	db := createSQLiteFile(t, path)
	defer db.Close()
	mustShadowExec(t, db, `CREATE TABLE history_hot (
		ts_ms INTEGER PRIMARY KEY, grid_w REAL, pv_w REAL, bat_w REAL, load_w REAL, bat_soc REAL, json TEXT NOT NULL)`)
	mustShadowExec(t, db, `CREATE TABLE ts_drivers(id INTEGER PRIMARY KEY, name TEXT NOT NULL)`)
	mustShadowExec(t, db, `CREATE TABLE ts_metrics(id INTEGER PRIMARY KEY, name TEXT NOT NULL, unit TEXT)`)
	mustShadowExec(t, db, `CREATE TABLE ts_samples(driver_id INTEGER, metric_id INTEGER, ts_ms INTEGER, value REAL)`)
	mustShadowExec(t, db, `CREATE TABLE energy_ledger_entries (
		schema_version INTEGER, asset_id TEXT, flow TEXT, bucket_start_ms INTEGER, bucket_len_ms INTEGER,
		energy_wh REAL, source TEXT, quality TEXT, provenance TEXT, sample_count INTEGER, observed_at_ms INTEGER)`)
	mustShadowExec(t, db, `CREATE TABLE planner_diagnostics (
		ts_ms INTEGER PRIMARY KEY, reason TEXT, zone TEXT, total_cost_ore REAL, horizon_slots INTEGER, json TEXT)`)
	mustShadowExec(t, db, `CREATE TABLE driver_command_results (
		id TEXT PRIMARY KEY, driver_name TEXT, command TEXT, status TEXT, code TEXT, completed_at_ms INTEGER, result_json TEXT)`)
	mustShadowExec(t, db, `INSERT INTO history_hot VALUES
		(1000, 100, -200, 300, 400, 0.5, '{"targets":{"battery-a":-750}}')`)
	mustShadowExec(t, db, `INSERT INTO ts_drivers VALUES (1, 'driver-b'), (2, 'driver-a')`)
	mustShadowExec(t, db, `INSERT INTO ts_metrics VALUES (1, 'voltage', 'V'), (2, 'power', 'W')`)
	mustShadowExec(t, db, `INSERT INTO ts_samples VALUES (1, 1, 1001, 230), (2, 2, 1001, -500)`)
	mustShadowExec(t, db, `INSERT INTO energy_ledger_entries VALUES
		(1, 'battery-a', 'battery_charge', 900, 300000, 10, 'counter', 'measured', 'counter', 1, 1001)`)
	mustShadowExec(t, db, `INSERT INTO planner_diagnostics VALUES
		(1002, 'periodic', 'SE3', 12.5, 1, '{"slots":[]}')`)
	mustShadowExec(t, db, `INSERT INTO driver_command_results VALUES
		('cmd-1', 'driver-a', 'set_power', 'completed', 'ok', 1003, '{}')`)
}

func createShadowCacheFixture(t *testing.T, path string) {
	t.Helper()
	db := createSQLiteFile(t, path)
	defer db.Close()
	mustShadowExec(t, db, `CREATE TABLE prices (
		zone TEXT, slot_ts_ms INTEGER, slot_len_min INTEGER, spot_ore_kwh REAL,
		total_ore_kwh REAL, source TEXT, fetched_at_ms INTEGER)`)
	mustShadowExec(t, db, `CREATE TABLE forecasts (
		slot_ts_ms INTEGER, slot_len_min INTEGER, cloud_cover_pct REAL, temp_c REAL,
		solar_wm2 REAL, pv_w_estimated REAL, source TEXT, fetched_at_ms INTEGER)`)
	mustShadowExec(t, db, `INSERT INTO prices VALUES ('SE3', 2000, 15, 25, 100, 'nordpool', 1003)`)
	mustShadowExec(t, db, `INSERT INTO forecasts VALUES (2000, 60, 20, 10, 500, 4000, 'weather', 1004)`)
}
