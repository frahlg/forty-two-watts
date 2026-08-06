// Package localauth provides local user accounts for the HTTP API:
// argon2id password verification and in-memory bearer sessions with
// operator/viewer roles. Persistence of accounts lives in
// go/internal/state (SQLite stays there); sessions are deliberately
// memory-only — a restart logs everyone out, which is the safe failure
// mode for a control system, and it keeps session secrets out of the
// database entirely.
package localauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// Roles.
const (
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

// ValidRole reports whether r is a known role.
func ValidRole(r string) bool { return r == RoleOperator || r == RoleViewer }

// Argon2id parameters — OWASP's minimum recommended configuration
// (t=2, m=19 MiB, p=1), chosen so a Raspberry Pi login stays subsecond
// while GPU cracking stays expensive.
const (
	argonTime    = 2
	argonMemory  = 19 * 1024 // KiB
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

// HashPassword produces a PHC-format argon2id string.
func HashPassword(password string) (string, error) {
	if len(password) < 8 {
		return "", errors.New("password must be at least 8 characters")
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks a password against a PHC argon2id string in
// constant time over the derived key.
func VerifyPassword(password, phc string) bool {
	parts := strings.Split(phc, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, key]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// Session is one live login.
type Session struct {
	Username  string
	Role      string
	ExpiresAt time.Time
}

// Sessions is the in-memory session table. Safe for concurrent use.
type Sessions struct {
	mu  sync.Mutex
	ttl time.Duration
	tab map[string]Session
}

// NewSessions builds a session table. ttl <= 0 defaults to 24 h.
func NewSessions(ttl time.Duration) *Sessions {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Sessions{ttl: ttl, tab: map[string]Session{}}
}

// Create mints a session token for a verified user.
func (s *Sessions) Create(username, role string) (string, Session, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", Session{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sess := Session{Username: username, Role: role, ExpiresAt: time.Now().Add(s.ttl)}
	s.mu.Lock()
	s.tab[token] = sess
	s.mu.Unlock()
	return token, sess, nil
}

// Lookup resolves a token, expiring lazily.
func (s *Sessions) Lookup(token string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.tab[token]
	if !ok {
		return Session{}, false
	}
	if time.Now().After(sess.ExpiresAt) {
		delete(s.tab, token)
		return Session{}, false
	}
	return sess, true
}

// Revoke removes one session (logout).
func (s *Sessions) Revoke(token string) {
	s.mu.Lock()
	delete(s.tab, token)
	s.mu.Unlock()
}

// RevokeUser removes every session belonging to a user (password
// change, disable, delete).
func (s *Sessions) RevokeUser(username string) {
	s.mu.Lock()
	for tok, sess := range s.tab {
		if sess.Username == username {
			delete(s.tab, tok)
		}
	}
	s.mu.Unlock()
}
