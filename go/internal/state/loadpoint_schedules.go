package state

import (
	"database/sql"
	"fmt"
	"time"
)

// LoadpointSchedule is one persisted EV charging intent. The legacy
// single-target API maps onto one row per loadpoint with Name="primary"
// and Priority=0; future multi-schedule UIs can add more rows with
// richer Recurrence strings.
type LoadpointSchedule struct {
	ID           int64
	LoadpointID  string
	Name         string
	TargetSoCPct float64
	TargetTimeMs int64
	Enabled      bool
	Priority     int
	Recurrence   string
	// Energy-source policy flags. Operators will eventually set
	// these per-schedule from the UI; today's single-target API
	// creates rows with AllowGrid=true + AllowBatterySupport=true +
	// OnlySurplus=false, which is the unconstrained default.
	AllowGrid            bool
	AllowBatterySupport  bool
	OnlySurplus          bool
	CreatedAtMs          int64
	UpdatedAtMs          int64
}

// SaveLoadpointSchedule inserts or updates a schedule. When s.ID == 0
// a new row is created and the assigned ID is returned. When s.ID > 0
// the row with that ID is updated in place. The updated_at_ms column
// is touched on every save; created_at_ms is only set on insert.
func (s *Store) SaveLoadpointSchedule(sched *LoadpointSchedule) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("state: nil store")
	}
	if sched == nil {
		return 0, fmt.Errorf("state: nil schedule")
	}
	if sched.LoadpointID == "" {
		return 0, fmt.Errorf("state: loadpoint_id required")
	}
	now := time.Now().UnixMilli()
	if sched.ID == 0 {
		if sched.CreatedAtMs == 0 {
			sched.CreatedAtMs = now
		}
		sched.UpdatedAtMs = now
		res, err := s.db.Exec(`
			INSERT INTO loadpoint_schedules
				(loadpoint_id, name, target_soc_pct, target_time_ms,
				 enabled, priority, recurrence,
				 allow_grid, allow_battery_support, only_surplus,
				 created_at_ms, updated_at_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sched.LoadpointID, sched.Name, sched.TargetSoCPct, sched.TargetTimeMs,
			boolToInt(sched.Enabled), sched.Priority, sched.Recurrence,
			boolToInt(sched.AllowGrid), boolToInt(sched.AllowBatterySupport), boolToInt(sched.OnlySurplus),
			sched.CreatedAtMs, sched.UpdatedAtMs)
		if err != nil {
			return 0, fmt.Errorf("insert schedule: %w", err)
		}
		id, _ := res.LastInsertId()
		sched.ID = id
		return id, nil
	}
	sched.UpdatedAtMs = now
	_, err := s.db.Exec(`
		UPDATE loadpoint_schedules SET
			loadpoint_id          = ?,
			name                  = ?,
			target_soc_pct        = ?,
			target_time_ms        = ?,
			enabled               = ?,
			priority              = ?,
			recurrence            = ?,
			allow_grid            = ?,
			allow_battery_support = ?,
			only_surplus          = ?,
			updated_at_ms         = ?
		WHERE id = ?`,
		sched.LoadpointID, sched.Name, sched.TargetSoCPct, sched.TargetTimeMs,
		boolToInt(sched.Enabled), sched.Priority, sched.Recurrence,
		boolToInt(sched.AllowGrid), boolToInt(sched.AllowBatterySupport), boolToInt(sched.OnlySurplus),
		sched.UpdatedAtMs, sched.ID)
	if err != nil {
		return sched.ID, fmt.Errorf("update schedule: %w", err)
	}
	return sched.ID, nil
}

// UpsertPrimaryLoadpointSchedule is the helper the legacy single-target
// API uses: there is exactly one "primary" row per loadpoint, and
// POST /api/loadpoints/{id}/target overwrites it. When no primary
// exists yet, one is created. Returns the row after the upsert.
//
// Target time zero means "no deadline" (opportunistic); stored as-is
// since downstream consumers already treat target_time_ms==0 that way.
func (s *Store) UpsertPrimaryLoadpointSchedule(loadpointID string, socPct float64, targetTimeMs int64) (*LoadpointSchedule, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("state: nil store")
	}
	existing, err := s.loadPrimarySchedule(loadpointID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		existing = &LoadpointSchedule{
			LoadpointID:         loadpointID,
			Name:                "primary",
			Enabled:             true,
			AllowGrid:           true,
			AllowBatterySupport: true,
			OnlySurplus:         false,
		}
	}
	existing.TargetSoCPct = socPct
	existing.TargetTimeMs = targetTimeMs
	if _, err := s.SaveLoadpointSchedule(existing); err != nil {
		return nil, err
	}
	return existing, nil
}

func (s *Store) loadPrimarySchedule(loadpointID string) (*LoadpointSchedule, error) {
	row := s.db.QueryRow(`
		SELECT id, loadpoint_id, name, target_soc_pct, target_time_ms,
		       enabled, priority, recurrence,
		       allow_grid, allow_battery_support, only_surplus,
		       created_at_ms, updated_at_ms
		FROM loadpoint_schedules
		WHERE loadpoint_id = ? AND name = 'primary'
		LIMIT 1`, loadpointID)
	sched := &LoadpointSchedule{}
	var enabled, allowGrid, allowBattery, onlySurplus int
	err := row.Scan(&sched.ID, &sched.LoadpointID, &sched.Name,
		&sched.TargetSoCPct, &sched.TargetTimeMs,
		&enabled, &sched.Priority, &sched.Recurrence,
		&allowGrid, &allowBattery, &onlySurplus,
		&sched.CreatedAtMs, &sched.UpdatedAtMs)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load primary schedule: %w", err)
	}
	sched.Enabled = enabled != 0
	sched.AllowGrid = allowGrid != 0
	sched.AllowBatterySupport = allowBattery != 0
	sched.OnlySurplus = onlySurplus != 0
	return sched, nil
}

// ListLoadpointSchedules returns every stored schedule for the given
// loadpoint, ordered by priority ASC then target_time_ms ASC (the
// selection order the manager uses for "which schedule is active now").
func (s *Store) ListLoadpointSchedules(loadpointID string) ([]LoadpointSchedule, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("state: nil store")
	}
	rows, err := s.db.Query(`
		SELECT id, loadpoint_id, name, target_soc_pct, target_time_ms,
		       enabled, priority, recurrence,
		       allow_grid, allow_battery_support, only_surplus,
		       created_at_ms, updated_at_ms
		FROM loadpoint_schedules
		WHERE loadpoint_id = ?
		ORDER BY priority ASC, target_time_ms ASC`, loadpointID)
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	defer rows.Close()
	var out []LoadpointSchedule
	for rows.Next() {
		var sched LoadpointSchedule
		var enabled, allowGrid, allowBattery, onlySurplus int
		if err := rows.Scan(&sched.ID, &sched.LoadpointID, &sched.Name,
			&sched.TargetSoCPct, &sched.TargetTimeMs,
			&enabled, &sched.Priority, &sched.Recurrence,
			&allowGrid, &allowBattery, &onlySurplus,
			&sched.CreatedAtMs, &sched.UpdatedAtMs); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		sched.Enabled = enabled != 0
		sched.AllowGrid = allowGrid != 0
		sched.AllowBatterySupport = allowBattery != 0
		sched.OnlySurplus = onlySurplus != 0
		out = append(out, sched)
	}
	return out, rows.Err()
}

// AllLoadpointSchedules returns every stored schedule across all
// loadpoints. Used by main.go on startup to restore the in-memory
// Manager state in one query instead of one-per-loadpoint.
func (s *Store) AllLoadpointSchedules() ([]LoadpointSchedule, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("state: nil store")
	}
	rows, err := s.db.Query(`
		SELECT id, loadpoint_id, name, target_soc_pct, target_time_ms,
		       enabled, priority, recurrence,
		       allow_grid, allow_battery_support, only_surplus,
		       created_at_ms, updated_at_ms
		FROM loadpoint_schedules
		ORDER BY loadpoint_id ASC, priority ASC, target_time_ms ASC`)
	if err != nil {
		return nil, fmt.Errorf("list all schedules: %w", err)
	}
	defer rows.Close()
	var out []LoadpointSchedule
	for rows.Next() {
		var sched LoadpointSchedule
		var enabled, allowGrid, allowBattery, onlySurplus int
		if err := rows.Scan(&sched.ID, &sched.LoadpointID, &sched.Name,
			&sched.TargetSoCPct, &sched.TargetTimeMs,
			&enabled, &sched.Priority, &sched.Recurrence,
			&allowGrid, &allowBattery, &onlySurplus,
			&sched.CreatedAtMs, &sched.UpdatedAtMs); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		sched.Enabled = enabled != 0
		sched.AllowGrid = allowGrid != 0
		sched.AllowBatterySupport = allowBattery != 0
		sched.OnlySurplus = onlySurplus != 0
		out = append(out, sched)
	}
	return out, rows.Err()
}

// DeleteLoadpointSchedule removes a schedule by ID. No-op when the
// row doesn't exist.
func (s *Store) DeleteLoadpointSchedule(id int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("state: nil store")
	}
	_, err := s.db.Exec(`DELETE FROM loadpoint_schedules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete schedule: %w", err)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
