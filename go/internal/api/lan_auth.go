package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/srcfl/ftw/go/internal/state"
)

const (
	lanAuthPasswordKey = "lan_auth_password"
	minLANPasswordLen  = 10

	lanGuessLimit    = 5
	lanGuessCooldown = 30 * time.Second

	// Encoded in the stored hash so a later bump still verifies old rows.
	lanArgonTime    uint32 = 3
	lanArgonMemory  uint32 = 64 * 1024
	lanArgonThreads uint8  = 4
	lanArgonKeyLen  uint32 = 32
	lanArgonSaltLen        = 16
)

type lanSecretContextKey struct{}

func withLANSecret(ctx context.Context, ok bool) context.Context {
	return context.WithValue(ctx, lanSecretContextKey{}, ok)
}

func lanSecretFrom(ctx context.Context) (ok bool, checked bool) {
	v := ctx.Value(lanSecretContextKey{})
	if v == nil {
		return false, false
	}
	ok, _ = v.(bool)
	return ok, true
}

var (
	lanGuessMu          sync.Mutex
	lanGuessFailures    int
	lanGuessLockedUntil time.Time
	lanGuessNow         = time.Now
)

// admitLANSecret is the process-global guess limiter for the house password.
// Five failed VerifyLANSecret calls lock every further attempt, including
// the right password, for 30s. The clock is swapped in tests.
func admitLANSecret(verify func(string) bool, secret string) bool {
	lanGuessMu.Lock()
	defer lanGuessMu.Unlock()
	now := lanGuessNow()
	if !lanGuessLockedUntil.IsZero() && now.Before(lanGuessLockedUntil) {
		return false
	}
	if !lanGuessLockedUntil.IsZero() {
		lanGuessFailures = 0
		lanGuessLockedUntil = time.Time{}
	}
	if verify == nil || !verify(secret) {
		lanGuessFailures++
		if lanGuessFailures >= lanGuessLimit {
			lanGuessLockedUntil = now.Add(lanGuessCooldown)
		}
		return false
	}
	lanGuessFailures = 0
	lanGuessLockedUntil = time.Time{}
	return true
}

// VerifyStoredLANSecret reports whether secret matches the argon2id hash
// in state.db. It does not rate-limit; Authenticate does that.
func VerifyStoredLANSecret(st *state.Store, secret string) bool {
	if st == nil || secret == "" {
		return false
	}
	encoded, ok := st.LoadConfig(lanAuthPasswordKey)
	if !ok || encoded == "" {
		return false
	}
	return verifyLANPassword(encoded, secret)
}

func lanPasswordConfigured(st *state.Store) bool {
	if st == nil {
		return false
	}
	encoded, ok := st.LoadConfig(lanAuthPasswordKey)
	return ok && encoded != ""
}

func hashLANPassword(password string) (string, error) {
	salt := make([]byte, lanArgonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, lanArgonTime, lanArgonMemory, lanArgonThreads, lanArgonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, lanArgonMemory, lanArgonTime, lanArgonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

func verifyLANPassword(encoded, password string) bool {
	salt, want, timeCost, memory, threads, keyLen, ok := parseLANPasswordHash(encoded)
	if !ok {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, timeCost, memory, threads, keyLen)
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

func parseLANPasswordHash(encoded string) (salt, key []byte, timeCost, memory uint32, threads uint8, keyLen uint32, ok bool) {
	// $argon2id$v=19$m=65536,t=3,p=4$<salt>$<key>
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return nil, nil, 0, 0, 0, 0, false
	}
	if parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return nil, nil, 0, 0, 0, 0, false
	}
	var t, m, p uint64
	for _, field := range strings.Split(parts[3], ",") {
		name, raw, found := strings.Cut(field, "=")
		if !found {
			return nil, nil, 0, 0, 0, 0, false
		}
		n, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			return nil, nil, 0, 0, 0, 0, false
		}
		switch name {
		case "m":
			m = n
		case "t":
			t = n
		case "p":
			p = n
		default:
			return nil, nil, 0, 0, 0, 0, false
		}
	}
	if t == 0 || m == 0 || p == 0 || p > 255 {
		return nil, nil, 0, 0, 0, 0, false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return nil, nil, 0, 0, 0, 0, false
	}
	key, err = base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return nil, nil, 0, 0, 0, 0, false
	}
	return salt, key, uint32(t), uint32(m), uint8(p), uint32(len(key)), true
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, _ *http.Request) {
	lanAuth := false
	if s.deps.Cfg != nil && s.deps.CfgMu != nil {
		s.deps.CfgMu.RLock()
		lanAuth = s.deps.Cfg.API.LANAuth
		s.deps.CfgMu.RUnlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"lan_auth":   lanAuth,
		"configured": lanPasswordConfigured(s.deps.State),
	})
}

type lanAuthPasswordRequest struct {
	Password string `json:"password"`
	Enabled  *bool  `json:"enabled"`
}

func (s *Server) handleAuthPassword(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil || s.deps.Cfg == nil || s.deps.CfgMu == nil || s.deps.SaveConfig == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "config store unavailable"})
		return
	}
	var req lanAuthPasswordRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
		return
	}
	if req.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enabled is required"})
		return
	}

	enabled := *req.Enabled
	if enabled {
		already := lanPasswordConfigured(s.deps.State)
		switch {
		case req.Password == "":
			if !already {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "password required to enable LAN auth"})
				return
			}
		case len(req.Password) < minLANPasswordLen:
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("password must be at least %d characters", minLANPasswordLen),
			})
			return
		default:
			encoded, err := hashLANPassword(req.Password)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not hash password"})
				return
			}
			if err := s.deps.State.SaveConfig(lanAuthPasswordKey, encoded); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "save failed: " + err.Error()})
				return
			}
		}
	}

	s.deps.CfgMu.Lock()
	s.deps.Cfg.API.LANAuth = enabled
	cfgCopy := *s.deps.Cfg
	s.deps.CfgMu.Unlock()
	if err := s.deps.SaveConfig(s.deps.ConfigPath, &cfgCopy); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "save failed: " + err.Error()})
		return
	}
	if !enabled {
		if err := s.deps.State.SaveConfig(lanAuthPasswordKey, ""); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "save failed: " + err.Error()})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"lan_auth":   enabled,
		"configured": lanPasswordConfigured(s.deps.State),
	})
}
