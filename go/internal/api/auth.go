package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/srcfl/ftw/go/internal/localauth"
	"github.com/srcfl/ftw/go/internal/state"
)

// sessionCookie is the login session cookie name.
const sessionCookie = "ftw_session"

// AuthPolicy is the login/role layer above SecureMutations. Mode
// semantics (see config.API.Auth):
//
//	open        — pass-through; audit still records mutation principals.
//	local_trust — local clients unchanged; non-local requests need a
//	              session (viewer to read, operator to mutate). The
//	              mutation bearer token remains valid for automation.
//	required    — every /api request needs a session, local included.
//	              /api/auth/login, /api/health and non-/api paths
//	              (the login page's static assets) stay reachable.
type AuthPolicy struct {
	Mode     string
	Sessions *localauth.Sessions
	// Users fetches an account for login. Nil disables login (mode open).
	Users interface {
		UserByName(string) (state.User, bool, error)
	}
	// MutationToken mirrors MutationPolicy.Token so automation bearer
	// tokens keep working for mutations in local_trust mode.
	MutationToken string
	// Audit records mutation attempts. Nil disables persistence.
	Audit interface {
		AppendAudit(state.AuditEntry) error
	}
}

func (p AuthPolicy) enabled() bool {
	return p.Mode == "local_trust" || p.Mode == "required"
}

// sessionFrom resolves the request's login session, if any.
func (p AuthPolicy) sessionFrom(r *http.Request) (localauth.Session, bool) {
	if p.Sessions == nil {
		return localauth.Session{}, false
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return localauth.Session{}, false
	}
	return p.Sessions.Lookup(c.Value)
}

// RequireAuth enforces the auth mode and records the mutation audit
// trail. It wraps OUTSIDE SecureMutations: identity first, then the
// CSRF/token/content-type checks.
func RequireAuth(next http.Handler, p AuthPolicy, st interface {
	AppendAudit(state.AuditEntry) error
}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, hasSession := p.sessionFrom(r)
		isMutation := requiresMutationProtection(r)

		// Audit every mutation attempt, before the verdict.
		if isMutation && st != nil && r.URL.Path != "/api/auth/login" {
			principal := "local"
			if hasSession {
				principal = sess.Username
			} else if validBearerToken(r.Header.Get("Authorization"), p.MutationToken) && p.MutationToken != "" {
				principal = "token"
			}
			if err := st.AppendAudit(state.AuditEntry{
				Principal: principal, Method: r.Method, Path: r.URL.Path,
				RemoteAddr: r.RemoteAddr,
			}); err != nil {
				slog.Warn("audit append failed", "err", err)
			}
		}

		if !p.enabled() {
			next.ServeHTTP(w, r)
			return
		}

		// Always-reachable paths: login, health probe, static assets.
		if r.URL.Path == "/api/auth/login" || r.URL.Path == "/api/health" ||
			len(r.URL.Path) < 5 || r.URL.Path[:5] != "/api/" {
			next.ServeHTTP(w, r)
			return
		}

		local := false
		if authority, err := parseAuthority(r.Host); err == nil {
			local = isLocalAuthority(authority) && isLocalClient(r.RemoteAddr)
		}
		if p.Mode == "local_trust" && local {
			next.ServeHTTP(w, r)
			return
		}

		// Automation bearer tokens keep working for mutations.
		if isMutation && p.MutationToken != "" &&
			validBearerToken(r.Header.Get("Authorization"), p.MutationToken) {
			next.ServeHTTP(w, r)
			return
		}

		if !hasSession {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "login required"})
			return
		}
		if isMutation && sess.Role != localauth.RoleOperator {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "operator role required"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- Login endpoints ----

// POST /api/auth/login {"username": "...", "password": "..."}
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	p := s.deps.Auth
	if p.Users == nil || p.Sessions == nil {
		http.Error(w, "local auth not configured", http.StatusNotFound)
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username and password required"})
		return
	}
	u, ok, err := p.Users.UserByName(body.Username)
	authOK := err == nil && ok && !u.Disabled && localauth.VerifyPassword(body.Password, u.PasswordHash)
	if s.deps.State != nil {
		principal := "login-failed:" + body.Username
		if authOK {
			principal = u.Username
		}
		_ = s.deps.State.AppendAudit(state.AuditEntry{
			Principal: principal, Method: r.Method, Path: r.URL.Path, RemoteAddr: r.RemoteAddr,
		})
	}
	if !authOK {
		// Uniform error: no username oracle.
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	token, sess, err := p.Sessions.Create(u.Username, u.Role)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "session create failed"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: int(time.Until(sess.ExpiresAt).Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"username": sess.Username, "role": sess.Role,
		"expires_at": sess.ExpiresAt,
	})
}

// POST /api/auth/logout
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && s.deps.Auth.Sessions != nil {
		s.deps.Auth.Sessions.Revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

// GET /api/auth/session — who am I (also how the UI decides to show
// the login screen).
func (s *Server) handleAuthSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.deps.Auth.sessionFrom(r)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": false, "mode": s.deps.Auth.Mode,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true, "mode": s.deps.Auth.Mode,
		"username": sess.Username, "role": sess.Role,
	})
}

// GET /api/audit?limit=N — operators only (enforced in-handler so the
// endpoint is protected even in open mode).
func (s *Server) handleAuditLog(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil {
		http.Error(w, "state unavailable", http.StatusServiceUnavailable)
		return
	}
	if s.deps.Auth.enabled() {
		sess, ok := s.deps.Auth.sessionFrom(r)
		if !ok || sess.Role != localauth.RoleOperator {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "operator role required"})
			return
		}
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	entries, err := s.deps.State.AuditEntries(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}
