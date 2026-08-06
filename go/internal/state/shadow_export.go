package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	DefaultShadowSettleDelay        = 5 * time.Second
	DefaultShadowMaxRows            = 4_096
	DefaultShadowMaxBytes           = 16 * 1024 * 1024
	DefaultShadowQueryTimeout       = 2 * time.Second
	MinimumShadowLedgerPollInterval = 5 * time.Minute
	MaxShadowExportRows             = 16_384
	MaxShadowExportBytes            = 64 * 1024 * 1024
	MaxShadowRowTextBytes           = 1024 * 1024
)

var ErrShadowMillisecondTooLarge = errors.New("shadow export millisecond group exceeds row limit")
var ErrShadowBatchTooLarge = errors.New("shadow export source batch exceeds byte limit")

// ShadowSource reads the existing FTW stores without joining their write path.
// It never migrates, heals, compacts, creates a cache, or writes a marker.
type ShadowSource struct {
	state       *sql.DB
	cache       *sql.DB
	stateTables map[string]bool
	cacheTables map[string]bool
}

// ShadowReadOptions selects a settled source window. AfterMS is the durable
// FTWDB source cursor. MaxRows counts source rows, not mapped FTWDB points.
type ShadowReadOptions struct {
	AfterMS      int64
	Now          time.Time
	SettleDelay  time.Duration
	MaxRows      int
	MaxBytes     int
	QueryTimeout time.Duration
	// IncludeEnergyLedger opts into the unindexed observed_at_ms scan. Poll
	// this source less often until Core adds an index in a later migration.
	IncludeEnergyLedger bool
	// EnergyLedgerOnly supports a separate slow source ID and durable cursor.
	// It prevents the fast stream from rescanning the unindexed ledger table.
	EnergyLedgerOnly   bool
	LedgerPollInterval time.Duration
}

// ShadowExportBatch is a stable snapshot of every supported source row in
// (AfterMS, CutoffMS]. A cutoff never splits rows that share one millisecond.
type ShadowExportBatch struct {
	AfterMS          int64
	CutoffMS         int64
	SettledThroughMS int64
	HasMore          bool
	ApproxBytes      int

	History        []ShadowHistoryRow
	Samples        []ShadowSampleRow
	Energy         []ShadowEnergyRow
	Diagnostics    []ShadowDiagnosticRow
	CommandResults []ShadowCommandResultRow
	Prices         []ShadowPriceRow
	Forecasts      []ShadowForecastRow
	// DriverDevices contains identity resolution for each driver referenced by
	// this batch. The source reads it in the same state.db transaction as
	// History, Samples, and CommandResults. The devices table has no alias
	// history, so old names that no longer occur there resolve as missing.
	DriverDevices []ShadowDriverDevice
}

func (b ShadowExportBatch) RowCount() int {
	return len(b.History) + len(b.Samples) + len(b.Energy) + len(b.Diagnostics) +
		len(b.CommandResults) + len(b.Prices) + len(b.Forecasts)
}

type ShadowHistoryRow struct {
	TsMS             int64
	GridW            *float64
	PVW              *float64
	BatteryW         *float64
	LoadW            *float64
	BatterySoC       *float64
	Targets          map[string]float64
	TargetsMalformed bool
}

type ShadowSampleRow struct {
	Driver string
	Metric string
	Unit   string
	TsMS   int64
	Value  float64
}

type ShadowDriverIdentityResolution uint8

const (
	ShadowDriverIdentityStable ShadowDriverIdentityResolution = iota + 1
	ShadowDriverIdentityAmbiguous
	ShadowDriverIdentityMissing
)

// ShadowDriverDevice records identity resolution for one referenced driver.
// DeviceID is set only when exactly one distinct devices row matches Driver.
// The source emits one row per referenced driver, sorted by Driver.
type ShadowDriverDevice struct {
	Driver     string
	DeviceID   string
	Resolution ShadowDriverIdentityResolution
}

type ShadowEnergyRow struct {
	SchemaVersion      int
	AssetID            string
	AssetDeviceID      string
	AssetKind          string
	AssetLabel         string
	AssetReadOnly      bool
	AssetMetadataKnown bool
	Flow               string
	BucketStartMS      int64
	BucketLenMS        int64
	EnergyWh           float64
	Source             string
	Quality            string
	Provenance         string
	SampleCount        int64
	ObservedAtMS       int64
}

type ShadowDiagnosticRow struct {
	TsMS         int64
	Reason       string
	Zone         string
	TotalCostOre float64
	HorizonSlots int
	JSON         string
}

type ShadowCommandResultRow struct {
	ID            string
	Driver        string
	Command       string
	Status        string
	Code          string
	CompletedAtMS int64
	ResultJSON    string
}

type ShadowPriceRow struct {
	Zone        string
	SlotTsMS    int64
	SlotLenMin  int
	SpotOreKWh  float64
	TotalOreKWh float64
	Source      string
	FetchedAtMS int64
}

type ShadowForecastRow struct {
	SlotTsMS      int64
	SlotLenMin    int
	CloudCoverPct *float64
	TempC         *float64
	SolarWM2      *float64
	PVWEstimated  *float64
	Source        string
	FetchedAtMS   int64
}

// OpenShadowSource opens state.db and, when present, its sibling cache.db in
// SQLite read-only and query-only modes. A missing disposable cache is valid.
func OpenShadowSource(statePath string) (*ShadowSource, error) {
	stateDB, err := openShadowDB(statePath, false)
	if err != nil {
		return nil, fmt.Errorf("open shadow state source: %w", err)
	}
	source := &ShadowSource{state: stateDB}
	closeOnError := func(err error) (*ShadowSource, error) {
		_ = source.Close()
		return nil, err
	}

	inspectCtx, cancel := context.WithTimeout(context.Background(), DefaultShadowQueryTimeout)
	defer cancel()
	source.stateTables, err = shadowTableSet(inspectCtx, stateDB)
	if err != nil {
		return closeOnError(fmt.Errorf("inspect shadow state schema: %w", err))
	}
	cachePath := filepath.Join(filepath.Dir(statePath), "cache.db")
	source.cache, err = openShadowDB(cachePath, true)
	if err != nil {
		return closeOnError(fmt.Errorf("open shadow cache source: %w", err))
	}
	if source.cache != nil {
		source.cacheTables, err = shadowTableSet(inspectCtx, source.cache)
		if err != nil {
			return closeOnError(fmt.Errorf("inspect shadow cache schema: %w", err))
		}
	}
	return source, nil
}

func openShadowDB(path string, optional bool) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		if optional && errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", abs)
	}
	query := url.Values{}
	query.Set("mode", "ro")
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", "busy_timeout(1000)")
	u := url.URL{Scheme: "file", Path: abs, RawQuery: query.Encode()}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	pingCtx, cancel := context.WithTimeout(context.Background(), DefaultShadowQueryTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func shadowTableSet(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type = 'table' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables[name] = true
	}
	return tables, rows.Err()
}

func (s *ShadowSource) Close() error {
	if s == nil {
		return nil
	}
	var err error
	if s.cache != nil {
		err = s.cache.Close()
		s.cache = nil
	}
	if s.state != nil {
		err = errors.Join(err, s.state.Close())
		s.state = nil
	}
	return err
}

type shadowTimestampColumn struct {
	table  string
	column string
}

var shadowStateCursors = []shadowTimestampColumn{
	{"history_hot", "ts_ms"},
	{"ts_samples", "ts_ms"},
	{"energy_ledger_entries", "observed_at_ms"},
	{"planner_diagnostics", "ts_ms"},
	{"driver_command_results", "completed_at_ms"},
}

var shadowCacheCursors = []shadowTimestampColumn{
	{"prices", "fetched_at_ms"},
	{"forecasts", "fetched_at_ms"},
}

// ReadAfter reads one bounded settled window. It first finds a common cutoff,
// then closes that scan before opening short read transactions for row data.
func (s *ShadowSource) ReadAfter(ctx context.Context, options ShadowReadOptions) (ShadowExportBatch, error) {
	if s == nil || s.state == nil {
		return ShadowExportBatch{}, errors.New("shadow source is closed")
	}
	if options.AfterMS < 0 {
		return ShadowExportBatch{}, errors.New("shadow cursor must not be negative")
	}
	if options.MaxRows == 0 {
		options.MaxRows = DefaultShadowMaxRows
	}
	if options.MaxRows < 1 || options.MaxRows > MaxShadowExportRows {
		return ShadowExportBatch{}, fmt.Errorf("shadow max rows must be in [1,%d]", MaxShadowExportRows)
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = DefaultShadowMaxBytes
	}
	if options.MaxBytes < 1 || options.MaxBytes > MaxShadowExportBytes {
		return ShadowExportBatch{}, fmt.Errorf("shadow max bytes must be in [1,%d]", MaxShadowExportBytes)
	}
	if options.SettleDelay == 0 {
		options.SettleDelay = DefaultShadowSettleDelay
	}
	if options.SettleDelay < 0 {
		return ShadowExportBatch{}, errors.New("shadow settle delay must not be negative")
	}
	if options.Now.IsZero() {
		options.Now = time.Now()
	}
	if options.QueryTimeout == 0 {
		options.QueryTimeout = DefaultShadowQueryTimeout
	}
	if options.QueryTimeout < 0 {
		return ShadowExportBatch{}, errors.New("shadow query timeout must not be negative")
	}
	if (options.IncludeEnergyLedger || options.EnergyLedgerOnly) &&
		options.LedgerPollInterval < MinimumShadowLedgerPollInterval {
		return ShadowExportBatch{}, fmt.Errorf("shadow energy ledger poll interval must be at least %s",
			MinimumShadowLedgerPollInterval)
	}
	queryCtx, cancel := context.WithTimeout(ctx, options.QueryTimeout)
	defer cancel()
	settleMS := options.SettleDelay.Milliseconds()
	if options.SettleDelay%time.Millisecond != 0 {
		settleMS++
	}
	settledThrough := options.Now.UnixMilli()
	if settleMS > 0 {
		if settledThrough < math.MinInt64+settleMS {
			return ShadowExportBatch{}, errors.New("shadow settled cutoff underflows milliseconds")
		}
		settledThrough -= settleMS
	}
	batch := ShadowExportBatch{
		AfterMS:          options.AfterMS,
		CutoffMS:         options.AfterMS,
		SettledThroughMS: settledThrough,
	}
	if settledThrough <= options.AfterMS {
		return batch, nil
	}

	stateCursors := shadowStateCursors
	if options.EnergyLedgerOnly {
		stateCursors = shadowStateCursors[2:3]
	} else if !options.IncludeEnergyLedger {
		stateCursors = append(append([]shadowTimestampColumn(nil), shadowStateCursors[:2]...), shadowStateCursors[3:]...)
	}
	stateTimes, err := shadowCandidateTimes(queryCtx, s.state, s.stateTables, stateCursors,
		options.AfterMS, settledThrough, options.MaxRows+1)
	if err != nil {
		return ShadowExportBatch{}, fmt.Errorf("scan shadow state cursors: %w", err)
	}
	var cacheTimes []int64
	if !options.EnergyLedgerOnly {
		cacheTimes, err = shadowCandidateTimes(queryCtx, s.cache, s.cacheTables, shadowCacheCursors,
			options.AfterMS, settledThrough, options.MaxRows+1)
		if err != nil {
			return ShadowExportBatch{}, fmt.Errorf("scan shadow cache cursors: %w", err)
		}
	}
	times := append(stateTimes, cacheTimes...)
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	cutoff, hasMore, err := shadowFullGroupCutoff(times, options.AfterMS, options.MaxRows)
	if err != nil {
		return ShadowExportBatch{}, err
	}
	batch.CutoffMS, batch.HasMore = cutoff, hasMore
	if cutoff == options.AfterMS {
		return batch, nil
	}

	if err := s.readShadowState(queryCtx, &batch, options.MaxRows, options.MaxBytes,
		options.IncludeEnergyLedger || options.EnergyLedgerOnly, options.EnergyLedgerOnly); err != nil {
		return ShadowExportBatch{}, err
	}
	if !options.EnergyLedgerOnly {
		if err := s.readShadowCache(queryCtx, &batch, options.MaxRows, options.MaxBytes); err != nil {
			return ShadowExportBatch{}, err
		}
	}
	if batch.RowCount() > options.MaxRows {
		return ShadowExportBatch{}, fmt.Errorf("%w: cutoff %d has %d rows, limit %d",
			ErrShadowMillisecondTooLarge, cutoff, batch.RowCount(), options.MaxRows)
	}
	return batch, nil
}

func shadowCandidateTimes(ctx context.Context, db *sql.DB, tables map[string]bool,
	columns []shadowTimestampColumn, afterMS, throughMS int64, limit int) ([]int64, error) {
	if db == nil {
		return nil, nil
	}
	parts := make([]string, 0, len(columns))
	args := make([]any, 0, len(columns)*2+1)
	for _, cursor := range columns {
		if !tables[cursor.table] {
			continue
		}
		parts = append(parts, "SELECT "+cursor.column+" AS cursor_ms FROM "+cursor.table+
			" WHERE "+cursor.column+" > ? AND "+cursor.column+" <= ?")
		args = append(args, afterMS, throughMS)
	}
	if len(parts) == 0 {
		return nil, nil
	}
	args = append(args, limit)
	query := "SELECT cursor_ms FROM (" + strings.Join(parts, " UNION ALL ") + ") ORDER BY cursor_ms LIMIT ?"
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = tx.Rollback()
		}
	}()
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, min(limit, 64))
	for rows.Next() {
		var timestamp int64
		if err := rows.Scan(&timestamp); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, timestamp)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if rollbackErr := tx.Rollback(); err == nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
		err = rollbackErr
	}
	closed = true
	return out, err
}

func shadowFullGroupCutoff(timestamps []int64, afterMS int64, limit int) (int64, bool, error) {
	if len(timestamps) == 0 {
		return afterMS, false, nil
	}
	if len(timestamps) <= limit {
		return timestamps[len(timestamps)-1], false, nil
	}
	if timestamps[limit-1] != timestamps[limit] {
		return timestamps[limit-1], true, nil
	}
	boundary := timestamps[limit-1]
	first := sort.Search(len(timestamps), func(i int) bool { return timestamps[i] >= boundary })
	if first == 0 {
		return afterMS, true, fmt.Errorf("%w: timestamp %d has more than %d rows",
			ErrShadowMillisecondTooLarge, boundary, limit)
	}
	return timestamps[first-1], true, nil
}

func (s *ShadowSource) readShadowState(ctx context.Context, batch *ShadowExportBatch, maxRows, maxBytes int,
	includeEnergy, energyOnly bool) error {
	tx, err := s.state.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin shadow state read: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = tx.Rollback()
		}
	}()
	rangeArgs := []any{batch.AfterMS, batch.CutoffMS}
	if !energyOnly && s.stateTables["history_hot"] {
		rows, err := tx.QueryContext(ctx, `SELECT ts_ms, grid_w, pv_w, bat_w, load_w, bat_soc,
			length(CAST(json AS BLOB)), CASE WHEN length(CAST(json AS BLOB)) <= ? THEN json END
			FROM history_hot WHERE ts_ms > ? AND ts_ms <= ? ORDER BY ts_ms`,
			MaxShadowRowTextBytes, batch.AfterMS, batch.CutoffMS)
		if err != nil {
			return err
		}
		for rows.Next() {
			if err := shadowRowCapacity(batch, maxRows); err != nil {
				_ = rows.Close()
				return err
			}
			var row ShadowHistoryRow
			var grid, pv, battery, load, soc sql.NullFloat64
			var rawBytes int64
			var raw sql.NullString
			if err := rows.Scan(&row.TsMS, &grid, &pv, &battery, &load, &soc, &rawBytes, &raw); err != nil {
				_ = rows.Close()
				return err
			}
			if !raw.Valid {
				_ = rows.Close()
				return shadowTextLimitError("history json", rawBytes)
			}
			row.GridW, row.PVW = shadowFloat(grid), shadowFloat(pv)
			row.BatteryW, row.LoadW, row.BatterySoC = shadowFloat(battery), shadowFloat(load), shadowFloat(soc)
			row.Targets, row.TargetsMalformed = shadowTargets(raw.String)
			if err := shadowAddBytes(batch, maxBytes, 64+len(raw.String)+len(row.Targets)*64); err != nil {
				_ = rows.Close()
				return err
			}
			batch.History = append(batch.History, row)
		}
		if err := closeShadowRows(rows); err != nil {
			return err
		}
	}
	if !energyOnly && s.stateTables["ts_samples"] && s.stateTables["ts_drivers"] && s.stateTables["ts_metrics"] {
		rows, err := tx.QueryContext(ctx, `SELECT d.name, m.name, COALESCE(m.unit, ''), s.ts_ms, s.value
			FROM ts_samples s JOIN ts_drivers d ON d.id = s.driver_id JOIN ts_metrics m ON m.id = s.metric_id
			WHERE s.ts_ms > ? AND s.ts_ms <= ? ORDER BY s.ts_ms, d.name, m.name`, rangeArgs...)
		if err != nil {
			return err
		}
		for rows.Next() {
			if err := shadowRowCapacity(batch, maxRows); err != nil {
				_ = rows.Close()
				return err
			}
			var row ShadowSampleRow
			if err := rows.Scan(&row.Driver, &row.Metric, &row.Unit, &row.TsMS, &row.Value); err != nil {
				_ = rows.Close()
				return err
			}
			if err := shadowAddBytes(batch, maxBytes, 64+len(row.Driver)+len(row.Metric)+len(row.Unit)); err != nil {
				_ = rows.Close()
				return err
			}
			batch.Samples = append(batch.Samples, row)
		}
		if err := closeShadowRows(rows); err != nil {
			return err
		}
	}
	if includeEnergy && s.stateTables["energy_ledger_entries"] {
		query := `SELECT e.schema_version, e.asset_id, '', '', '', 0, 0,
			e.flow, e.bucket_start_ms, e.bucket_len_ms, e.energy_wh, e.source,
			e.quality, e.provenance, e.sample_count, e.observed_at_ms
			FROM energy_ledger_entries e
			WHERE e.observed_at_ms > ? AND e.observed_at_ms <= ?
			ORDER BY e.observed_at_ms, e.bucket_start_ms, e.asset_id, e.flow, e.bucket_len_ms,
			e.source, e.quality, e.provenance, e.schema_version`
		if s.stateTables["energy_assets"] {
			query = `SELECT e.schema_version, e.asset_id,
				COALESCE(a.device_id, ''), COALESCE(a.kind, ''), COALESCE(a.label, ''),
				COALESCE(a.read_only, 0), CASE WHEN a.asset_id IS NULL THEN 0 ELSE 1 END,
				e.flow, e.bucket_start_ms, e.bucket_len_ms, e.energy_wh, e.source,
				e.quality, e.provenance, e.sample_count, e.observed_at_ms
				FROM energy_ledger_entries e
				LEFT JOIN energy_assets a ON a.asset_id = e.asset_id
				WHERE e.observed_at_ms > ? AND e.observed_at_ms <= ?
				ORDER BY e.observed_at_ms, e.bucket_start_ms, e.asset_id, e.flow, e.bucket_len_ms,
				e.source, e.quality, e.provenance, e.schema_version`
		}
		rows, err := tx.QueryContext(ctx, query, rangeArgs...)
		if err != nil {
			return err
		}
		for rows.Next() {
			if err := shadowRowCapacity(batch, maxRows); err != nil {
				_ = rows.Close()
				return err
			}
			var row ShadowEnergyRow
			var assetReadOnly, assetKnown int64
			if err := rows.Scan(&row.SchemaVersion, &row.AssetID, &row.AssetDeviceID,
				&row.AssetKind, &row.AssetLabel, &assetReadOnly, &assetKnown, &row.Flow,
				&row.BucketStartMS, &row.BucketLenMS, &row.EnergyWh, &row.Source,
				&row.Quality, &row.Provenance, &row.SampleCount, &row.ObservedAtMS); err != nil {
				_ = rows.Close()
				return err
			}
			row.AssetReadOnly = assetReadOnly != 0
			row.AssetMetadataKnown = assetKnown != 0
			if err := shadowAddBytes(batch, maxBytes, 128+len(row.AssetID)+len(row.Flow)+
				len(row.AssetDeviceID)+len(row.AssetKind)+len(row.AssetLabel)+len(row.Source)+
				len(row.Quality)+len(row.Provenance)); err != nil {
				_ = rows.Close()
				return err
			}
			batch.Energy = append(batch.Energy, row)
		}
		if err := closeShadowRows(rows); err != nil {
			return err
		}
	}
	if !energyOnly && s.stateTables["planner_diagnostics"] {
		rows, err := tx.QueryContext(ctx, `SELECT ts_ms, reason, zone, total_cost_ore, horizon_slots,
			length(CAST(json AS BLOB)), CASE WHEN length(CAST(json AS BLOB)) <= ? THEN json END
			FROM planner_diagnostics WHERE ts_ms > ? AND ts_ms <= ? ORDER BY ts_ms`,
			MaxShadowRowTextBytes, batch.AfterMS, batch.CutoffMS)
		if err != nil {
			return err
		}
		for rows.Next() {
			if err := shadowRowCapacity(batch, maxRows); err != nil {
				_ = rows.Close()
				return err
			}
			var row ShadowDiagnosticRow
			var rawBytes int64
			var raw sql.NullString
			if err := rows.Scan(&row.TsMS, &row.Reason, &row.Zone, &row.TotalCostOre,
				&row.HorizonSlots, &rawBytes, &raw); err != nil {
				_ = rows.Close()
				return err
			}
			if !raw.Valid {
				_ = rows.Close()
				return shadowTextLimitError("planner diagnostic json", rawBytes)
			}
			row.JSON = raw.String
			if err := shadowAddBytes(batch, maxBytes, 96+len(row.Reason)+len(row.Zone)+len(row.JSON)); err != nil {
				_ = rows.Close()
				return err
			}
			batch.Diagnostics = append(batch.Diagnostics, row)
		}
		if err := closeShadowRows(rows); err != nil {
			return err
		}
	}
	if !energyOnly && s.stateTables["driver_command_results"] {
		rows, err := tx.QueryContext(ctx, `SELECT id, driver_name, command, status, code, completed_at_ms,
			length(CAST(result_json AS BLOB)),
			CASE WHEN length(CAST(result_json AS BLOB)) <= ? THEN result_json END
			FROM driver_command_results WHERE completed_at_ms > ? AND completed_at_ms <= ?
			ORDER BY completed_at_ms, id`, MaxShadowRowTextBytes, batch.AfterMS, batch.CutoffMS)
		if err != nil {
			return err
		}
		for rows.Next() {
			if err := shadowRowCapacity(batch, maxRows); err != nil {
				_ = rows.Close()
				return err
			}
			var row ShadowCommandResultRow
			var rawBytes int64
			var raw sql.NullString
			if err := rows.Scan(&row.ID, &row.Driver, &row.Command, &row.Status, &row.Code,
				&row.CompletedAtMS, &rawBytes, &raw); err != nil {
				_ = rows.Close()
				return err
			}
			if !raw.Valid {
				_ = rows.Close()
				return shadowTextLimitError("driver command result json", rawBytes)
			}
			row.ResultJSON = raw.String
			if err := shadowAddBytes(batch, maxBytes, 96+len(row.ID)+len(row.Driver)+len(row.Command)+
				len(row.Status)+len(row.Code)+len(row.ResultJSON)); err != nil {
				_ = rows.Close()
				return err
			}
			batch.CommandResults = append(batch.CommandResults, row)
		}
		if err := closeShadowRows(rows); err != nil {
			return err
		}
	}
	if !energyOnly {
		if err := readShadowDriverDevices(ctx, tx, batch, maxBytes, s.stateTables["devices"]); err != nil {
			return err
		}
	}
	if batch.RowCount() > maxRows {
		return fmt.Errorf("%w: state snapshot has %d rows, limit %d",
			ErrShadowMillisecondTooLarge, batch.RowCount(), maxRows)
	}
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("close shadow state read: %w", err)
	}
	closed = true
	return nil
}

const shadowDeviceLookupChunk = 500

// readShadowDriverDevices resolves only driver names referenced by this
// batch. Keeping this query in the existing transaction makes the identity
// map part of the same SQLite snapshot as the exported source rows. A name is
// stable only when exactly one distinct device_id matches it.
func readShadowDriverDevices(ctx context.Context, tx *sql.Tx, batch *ShadowExportBatch,
	maxBytes int, haveDevices bool) error {
	names := shadowReferencedDrivers(batch)
	for start := 0; start < len(names); start += shadowDeviceLookupChunk {
		end := start + shadowDeviceLookupChunk
		if end > len(names) {
			end = len(names)
		}
		chunk := names[start:end]
		if !haveDevices {
			for _, name := range chunk {
				if err := shadowAddBytes(batch, maxBytes, 16+len(name)); err != nil {
					return err
				}
				batch.DriverDevices = append(batch.DriverDevices, ShadowDriverDevice{
					Driver: name, Resolution: ShadowDriverIdentityMissing,
				})
			}
			continue
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		query := `SELECT DISTINCT d.driver_name,
			length(CAST(d.device_id AS BLOB)) AS device_id_bytes,
			CASE WHEN length(CAST(d.device_id AS BLOB)) <= ? THEN d.device_id END AS device_id
			FROM devices d
			WHERE d.driver_name IN (` + placeholders + `) AND trim(d.device_id) <> ''
			ORDER BY d.driver_name, d.device_id`
		args := make([]any, 0, len(chunk)+1)
		args = append(args, MaxShadowRowTextBytes)
		for _, name := range chunk {
			args = append(args, name)
		}
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("read shadow device identities: %w", err)
		}
		matches := make(map[string][]string, len(chunk))
		for rows.Next() {
			var driver string
			var deviceIDBytes int64
			var deviceID sql.NullString
			if err := rows.Scan(&driver, &deviceIDBytes, &deviceID); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan shadow device identity: %w", err)
			}
			if !deviceID.Valid {
				_ = rows.Close()
				return shadowTextLimitError("device id", deviceIDBytes)
			}
			if strings.TrimSpace(deviceID.String) == "" {
				continue
			}
			if err := shadowAddBytes(batch, maxBytes, 32+len(driver)+len(deviceID.String)); err != nil {
				_ = rows.Close()
				return err
			}
			matches[driver] = append(matches[driver], deviceID.String)
		}
		if err := closeShadowRows(rows); err != nil {
			return err
		}
		for _, name := range chunk {
			mapping := ShadowDriverDevice{Driver: name}
			switch len(matches[name]) {
			case 0:
				mapping.Resolution = ShadowDriverIdentityMissing
			case 1:
				mapping.DeviceID = matches[name][0]
				mapping.Resolution = ShadowDriverIdentityStable
			default:
				mapping.Resolution = ShadowDriverIdentityAmbiguous
			}
			if err := shadowAddBytes(batch, maxBytes, 16+len(mapping.Driver)); err != nil {
				return err
			}
			batch.DriverDevices = append(batch.DriverDevices, mapping)
		}
	}
	return nil
}

func shadowReferencedDrivers(batch *ShadowExportBatch) []string {
	set := make(map[string]struct{})
	for _, row := range batch.History {
		for driver := range row.Targets {
			if driver != "" {
				set[driver] = struct{}{}
			}
		}
	}
	for _, row := range batch.Samples {
		if row.Driver != "" {
			set[row.Driver] = struct{}{}
		}
	}
	for _, row := range batch.CommandResults {
		if row.Driver != "" {
			set[row.Driver] = struct{}{}
		}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *ShadowSource) readShadowCache(ctx context.Context, batch *ShadowExportBatch, maxRows, maxBytes int) error {
	if s.cache == nil {
		return nil
	}
	tx, err := s.cache.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin shadow cache read: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = tx.Rollback()
		}
	}()
	rangeArgs := []any{batch.AfterMS, batch.CutoffMS}
	if s.cacheTables["prices"] {
		rows, err := tx.QueryContext(ctx, `SELECT zone, slot_ts_ms, slot_len_min, spot_ore_kwh,
			total_ore_kwh, source, fetched_at_ms FROM prices
			WHERE fetched_at_ms > ? AND fetched_at_ms <= ? ORDER BY fetched_at_ms, zone, slot_ts_ms`, rangeArgs...)
		if err != nil {
			return err
		}
		for rows.Next() {
			if err := shadowRowCapacity(batch, maxRows); err != nil {
				_ = rows.Close()
				return err
			}
			var row ShadowPriceRow
			if err := rows.Scan(&row.Zone, &row.SlotTsMS, &row.SlotLenMin, &row.SpotOreKWh,
				&row.TotalOreKWh, &row.Source, &row.FetchedAtMS); err != nil {
				_ = rows.Close()
				return err
			}
			if err := shadowAddBytes(batch, maxBytes, 96+len(row.Zone)+len(row.Source)); err != nil {
				_ = rows.Close()
				return err
			}
			batch.Prices = append(batch.Prices, row)
		}
		if err := closeShadowRows(rows); err != nil {
			return err
		}
	}
	if s.cacheTables["forecasts"] {
		rows, err := tx.QueryContext(ctx, `SELECT slot_ts_ms, slot_len_min, cloud_cover_pct, temp_c,
			solar_wm2, pv_w_estimated, source, fetched_at_ms FROM forecasts
			WHERE fetched_at_ms > ? AND fetched_at_ms <= ? ORDER BY fetched_at_ms, slot_ts_ms`, rangeArgs...)
		if err != nil {
			return err
		}
		for rows.Next() {
			if err := shadowRowCapacity(batch, maxRows); err != nil {
				_ = rows.Close()
				return err
			}
			var row ShadowForecastRow
			var cloud, temp, solar, pv sql.NullFloat64
			if err := rows.Scan(&row.SlotTsMS, &row.SlotLenMin, &cloud, &temp, &solar, &pv,
				&row.Source, &row.FetchedAtMS); err != nil {
				_ = rows.Close()
				return err
			}
			row.CloudCoverPct, row.TempC = shadowFloat(cloud), shadowFloat(temp)
			row.SolarWM2, row.PVWEstimated = shadowFloat(solar), shadowFloat(pv)
			if err := shadowAddBytes(batch, maxBytes, 112+len(row.Source)); err != nil {
				_ = rows.Close()
				return err
			}
			batch.Forecasts = append(batch.Forecasts, row)
		}
		if err := closeShadowRows(rows); err != nil {
			return err
		}
	}
	if batch.RowCount() > maxRows {
		return fmt.Errorf("%w: combined snapshot has %d rows, limit %d",
			ErrShadowMillisecondTooLarge, batch.RowCount(), maxRows)
	}
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("close shadow cache read: %w", err)
	}
	closed = true
	return nil
}

func closeShadowRows(rows *sql.Rows) error {
	err := rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	return err
}

func shadowRowCapacity(batch *ShadowExportBatch, maxRows int) error {
	if batch.RowCount() < maxRows {
		return nil
	}
	return fmt.Errorf("%w: source changed while reading cutoff %d, limit %d",
		ErrShadowMillisecondTooLarge, batch.CutoffMS, maxRows)
}

func shadowAddBytes(batch *ShadowExportBatch, maximum, amount int) error {
	if amount < 0 || batch.ApproxBytes > maximum-amount {
		return fmt.Errorf("%w: cutoff %d would exceed %d bytes",
			ErrShadowBatchTooLarge, batch.CutoffMS, maximum)
	}
	batch.ApproxBytes += amount
	return nil
}

func shadowTextLimitError(field string, size int64) error {
	return fmt.Errorf("%w: %s has %d bytes; maximum is %d",
		ErrShadowBatchTooLarge, field, size, MaxShadowRowTextBytes)
}

func shadowFloat(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	v := value.Float64
	return &v
}

func shadowTargets(raw string) (map[string]float64, bool) {
	var body struct {
		Targets map[string]json.RawMessage `json:"targets"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		return nil, true
	}
	if len(body.Targets) == 0 {
		return nil, false
	}
	targets := make(map[string]float64, len(body.Targets))
	malformed := false
	for driver, rawValue := range body.Targets {
		var value float64
		if err := json.Unmarshal(rawValue, &value); err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			malformed = true
			continue
		}
		targets[driver] = value
	}
	return targets, malformed
}
