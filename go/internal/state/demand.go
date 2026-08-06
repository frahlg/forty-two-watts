package state

import "database/sql"

// Billing-demand persistence for the kVA demand-charge tracker
// (go/internal/demand). Written once per completed demand-integration
// interval; read at startup so a restart mid-billing-cycle keeps the
// peak-so-far.

// DemandInterval is one completed demand-integration window.
type DemandInterval struct {
	CycleStartMs    int64   `json:"cycle_start_ms"`
	IntervalStartMs int64   `json:"interval_start_ms"`
	AvgKVA          float64 `json:"avg_kva"`
	AvgKW           float64 `json:"avg_kw"`
	Band            string  `json:"band"`
	Counted         bool    `json:"counted"`
}

// SaveDemandInterval upserts one completed interval and, when it is a
// counted interval that exceeds the cycle's stored peak, advances the
// billing peak in the same transaction.
func (s *Store) SaveDemandInterval(iv DemandInterval) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		INSERT INTO billing_demand (cycle_start_ms, interval_start_ms, avg_kva, avg_kw, band, counted)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(cycle_start_ms, interval_start_ms) DO UPDATE SET
			avg_kva = excluded.avg_kva,
			avg_kw = excluded.avg_kw,
			band = excluded.band,
			counted = excluded.counted`,
		iv.CycleStartMs, iv.IntervalStartMs, iv.AvgKVA, iv.AvgKW, iv.Band, boolToInt(iv.Counted)); err != nil {
		return err
	}
	if iv.Counted {
		if _, err := tx.Exec(`
			INSERT INTO billing_peak (cycle_start_ms, peak_kva, interval_start_ms)
			VALUES (?, ?, ?)
			ON CONFLICT(cycle_start_ms) DO UPDATE SET
				peak_kva = excluded.peak_kva,
				interval_start_ms = excluded.interval_start_ms
			WHERE excluded.peak_kva > billing_peak.peak_kva`,
			iv.CycleStartMs, iv.AvgKVA, iv.IntervalStartMs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// BillingPeak returns the stored peak for a billing cycle. ok=false when
// no counted interval has completed yet.
func (s *Store) BillingPeak(cycleStartMs int64) (peakKVA float64, intervalStartMs int64, ok bool, err error) {
	row := s.db.QueryRow(`SELECT peak_kva, interval_start_ms FROM billing_peak WHERE cycle_start_ms = ?`, cycleStartMs)
	switch err = row.Scan(&peakKVA, &intervalStartMs); err {
	case nil:
		return peakKVA, intervalStartMs, true, nil
	case sql.ErrNoRows:
		return 0, 0, false, nil
	default:
		return 0, 0, false, err
	}
}

// DemandIntervals returns a cycle's completed intervals, newest first,
// capped at limit (0 = all).
func (s *Store) DemandIntervals(cycleStartMs int64, limit int) ([]DemandInterval, error) {
	q := `SELECT cycle_start_ms, interval_start_ms, avg_kva, avg_kw, band, counted
		FROM billing_demand WHERE cycle_start_ms = ?
		ORDER BY interval_start_ms DESC`
	args := []any{cycleStartMs}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DemandInterval
	for rows.Next() {
		var iv DemandInterval
		var counted int
		if err := rows.Scan(&iv.CycleStartMs, &iv.IntervalStartMs, &iv.AvgKVA, &iv.AvgKW, &iv.Band, &counted); err != nil {
			return nil, err
		}
		iv.Counted = counted != 0
		out = append(out, iv)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
