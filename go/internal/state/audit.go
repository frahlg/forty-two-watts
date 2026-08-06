package state

import "time"

// AuditEntry is one recorded API mutation attempt.
type AuditEntry struct {
	ID         int64  `json:"id"`
	TsMs       int64  `json:"ts_ms"`
	Principal  string `json:"principal"` // username, "token", or "local"
	Method     string `json:"method"`
	Path       string `json:"path"`
	RemoteAddr string `json:"remote_addr"`
}

// AppendAudit records one entry.
func (s *Store) AppendAudit(e AuditEntry) error {
	if e.TsMs == 0 {
		e.TsMs = time.Now().UnixMilli()
	}
	_, err := s.db.Exec(`
		INSERT INTO audit_log (ts_ms, principal, method, path, remote_addr)
		VALUES (?, ?, ?, ?, ?)`,
		e.TsMs, e.Principal, e.Method, e.Path, e.RemoteAddr)
	return err
}

// AuditEntries returns the newest entries, capped at limit (default 200).
func (s *Store) AuditEntries(limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 5000 {
		limit = 200
	}
	rows, err := s.db.Query(`
		SELECT id, ts_ms, principal, method, path, remote_addr
		FROM audit_log ORDER BY ts_ms DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.TsMs, &e.Principal, &e.Method, &e.Path, &e.RemoteAddr); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneAudit drops entries older than keep.
func (s *Store) PruneAudit(keep time.Duration) error {
	cutoff := time.Now().Add(-keep).UnixMilli()
	_, err := s.db.Exec(`DELETE FROM audit_log WHERE ts_ms < ?`, cutoff)
	return err
}
