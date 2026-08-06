package state

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Local user accounts for api.auth.mode (go/internal/localauth owns the
// hashing and sessions; this file is pure persistence).

// User is one local account.
type User struct {
	Username     string `json:"username"`
	Role         string `json:"role"` // operator | viewer
	PasswordHash string `json:"-"`
	CreatedMs    int64  `json:"created_ms"`
	Disabled     bool   `json:"disabled"`
}

var ErrUserExists = errors.New("user already exists")

// CreateUser inserts a new account.
func (s *Store) CreateUser(u User) error {
	if u.CreatedMs == 0 {
		u.CreatedMs = time.Now().UnixMilli()
	}
	_, err := s.db.Exec(`
		INSERT INTO users (username, role, password_hash, created_ms, disabled)
		VALUES (?, ?, ?, ?, ?)`,
		u.Username, u.Role, u.PasswordHash, u.CreatedMs, intFromBool(u.Disabled))
	if err != nil && isUniqueViolation(err) {
		return ErrUserExists
	}
	return err
}

// UserByName fetches one account. ok=false when it does not exist.
func (s *Store) UserByName(username string) (User, bool, error) {
	row := s.db.QueryRow(`
		SELECT username, role, password_hash, created_ms, disabled
		FROM users WHERE username = ?`, username)
	var u User
	var disabled int
	switch err := row.Scan(&u.Username, &u.Role, &u.PasswordHash, &u.CreatedMs, &disabled); err {
	case nil:
		u.Disabled = disabled != 0
		return u, true, nil
	case sql.ErrNoRows:
		return User{}, false, nil
	default:
		return User{}, false, err
	}
}

// ListUsers returns every account, without password hashes cleared —
// callers expose User via its JSON shape, which omits the hash.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`
		SELECT username, role, password_hash, created_ms, disabled
		FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var disabled int
		if err := rows.Scan(&u.Username, &u.Role, &u.PasswordHash, &u.CreatedMs, &disabled); err != nil {
			return nil, err
		}
		u.Disabled = disabled != 0
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUserPassword replaces the stored hash.
func (s *Store) UpdateUserPassword(username, hash string) error {
	res, err := s.db.Exec(`UPDATE users SET password_hash = ? WHERE username = ?`, hash, username)
	if err != nil {
		return err
	}
	return requireOneRow(res)
}

// SetUserDisabled toggles an account.
func (s *Store) SetUserDisabled(username string, disabled bool) error {
	res, err := s.db.Exec(`UPDATE users SET disabled = ? WHERE username = ?`, intFromBool(disabled), username)
	if err != nil {
		return err
	}
	return requireOneRow(res)
}

// DeleteUser removes an account.
func (s *Store) DeleteUser(username string) error {
	res, err := s.db.Exec(`DELETE FROM users WHERE username = ?`, username)
	if err != nil {
		return err
	}
	return requireOneRow(res)
}

// CountOperators returns how many enabled operator accounts exist —
// used to refuse deleting/disabling the last one.
func (s *Store) CountOperators() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'operator' AND disabled = 0`).Scan(&n)
	return n, err
}

func requireOneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("no such user")
	}
	return nil
}

func isUniqueViolation(err error) bool {
	// modernc.org/sqlite surfaces SQLITE_CONSTRAINT_* in the message;
	// matching the message keeps us off driver-internal types.
	return err != nil && (strings.Contains(err.Error(), "UNIQUE constraint") ||
		strings.Contains(err.Error(), "constraint failed"))
}

// intFromBool avoids clashing with sibling helpers in this package
// across branches (demand.go declares boolToInt).
func intFromBool(b bool) int {
	if b {
		return 1
	}
	return 0
}
