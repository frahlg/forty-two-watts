package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/localauth"
	"github.com/srcfl/ftw/go/internal/state"
)

type memUsers map[string]state.User

func (m memUsers) UserByName(name string) (state.User, bool, error) {
	u, ok := m[name]
	return u, ok, nil
}

type memAudit struct {
	mu      sync.Mutex
	entries []state.AuditEntry
}

func (m *memAudit) AppendAudit(e state.AuditEntry) error {
	m.mu.Lock()
	m.entries = append(m.entries, e)
	m.mu.Unlock()
	return nil
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func newAuthFixture(mode string) (AuthPolicy, *memAudit, string) {
	sessions := localauth.NewSessions(time.Hour)
	hash, _ := localauth.HashPassword("hunter22hunter22")
	users := memUsers{
		"op":     {Username: "op", Role: "operator", PasswordHash: hash},
		"viewer": {Username: "viewer", Role: "viewer", PasswordHash: hash},
	}
	p := AuthPolicy{Mode: mode, Sessions: sessions, Users: users}
	opToken, _, _ := sessions.Create("op", localauth.RoleOperator)
	return p, &memAudit{}, opToken
}

func doReq(t *testing.T, h http.Handler, method, path, host, remote, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(""))
	r.Host = host
	r.RemoteAddr = remote
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestOpenModeIsPassThrough(t *testing.T) {
	p, audit, _ := newAuthFixture("open")
	h := RequireAuth(okHandler(), p, audit)
	if w := doReq(t, h, "GET", "/api/status", "example.com:8080", "203.0.113.9:1234", ""); w.Code != 200 {
		t.Fatalf("open-mode remote read: %d", w.Code)
	}
	if w := doReq(t, h, "POST", "/api/mode", "example.com:8080", "203.0.113.9:1234", ""); w.Code != 200 {
		t.Fatalf("open-mode mutation must pass to inner layers: %d", w.Code)
	}
	// Mutation was audited even in open mode.
	if len(audit.entries) != 1 || audit.entries[0].Path != "/api/mode" {
		t.Fatalf("audit: %+v", audit.entries)
	}
}

func TestLocalTrustGatesRemoteOnly(t *testing.T) {
	p, audit, opToken := newAuthFixture("local_trust")
	h := RequireAuth(okHandler(), p, audit)

	// Local client: unchanged.
	if w := doReq(t, h, "GET", "/api/status", "192.168.1.10:8080", "192.168.1.50:999", ""); w.Code != 200 {
		t.Fatalf("local read: %d", w.Code)
	}
	if w := doReq(t, h, "POST", "/api/mode", "192.168.1.10:8080", "192.168.1.50:999", ""); w.Code != 200 {
		t.Fatalf("local mutation: %d", w.Code)
	}
	// Remote without session: 401.
	if w := doReq(t, h, "GET", "/api/status", "site.example.com", "203.0.113.9:1234", ""); w.Code != 401 {
		t.Fatalf("remote read without session: %d", w.Code)
	}
	// Remote with operator session: pass.
	if w := doReq(t, h, "POST", "/api/mode", "site.example.com", "203.0.113.9:1234", opToken); w.Code != 200 {
		t.Fatalf("remote operator mutation: %d", w.Code)
	}
}

func TestViewerCannotMutate(t *testing.T) {
	p, audit, _ := newAuthFixture("required")
	viewerToken, _, _ := p.Sessions.Create("viewer", localauth.RoleViewer)
	h := RequireAuth(okHandler(), p, audit)

	if w := doReq(t, h, "GET", "/api/status", "192.168.1.10:8080", "192.168.1.50:999", viewerToken); w.Code != 200 {
		t.Fatalf("viewer read: %d", w.Code)
	}
	if w := doReq(t, h, "POST", "/api/mode", "192.168.1.10:8080", "192.168.1.50:999", viewerToken); w.Code != 403 {
		t.Fatalf("viewer mutation should 403: %d", w.Code)
	}
}

func TestRequiredModeGatesLocalToo(t *testing.T) {
	p, audit, opToken := newAuthFixture("required")
	h := RequireAuth(okHandler(), p, audit)

	if w := doReq(t, h, "GET", "/api/status", "192.168.1.10:8080", "192.168.1.50:999", ""); w.Code != 401 {
		t.Fatalf("required-mode local read without session: %d", w.Code)
	}
	if w := doReq(t, h, "GET", "/api/status", "192.168.1.10:8080", "192.168.1.50:999", opToken); w.Code != 200 {
		t.Fatalf("required-mode read with session: %d", w.Code)
	}
	// Exempt paths stay reachable.
	if w := doReq(t, h, "POST", "/api/auth/login", "192.168.1.10:8080", "192.168.1.50:999", ""); w.Code != 200 {
		t.Fatalf("login must be reachable: %d", w.Code)
	}
	if w := doReq(t, h, "GET", "/api/health", "192.168.1.10:8080", "192.168.1.50:999", ""); w.Code != 200 {
		t.Fatalf("health must be reachable: %d", w.Code)
	}
	if w := doReq(t, h, "GET", "/index.html", "192.168.1.10:8080", "192.168.1.50:999", ""); w.Code != 200 {
		t.Fatalf("static assets must be reachable: %d", w.Code)
	}
}

func TestBearerTokenStillWorksForAutomation(t *testing.T) {
	p, audit, _ := newAuthFixture("local_trust")
	p.MutationToken = strings.Repeat("t", 32)
	h := RequireAuth(okHandler(), p, audit)

	r := httptest.NewRequest("POST", "/api/mode", strings.NewReader(""))
	r.Host = "site.example.com"
	r.RemoteAddr = "203.0.113.9:1234"
	r.Header.Set("Authorization", "Bearer "+p.MutationToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("bearer automation mutation: %d", w.Code)
	}
	// A forged token does not pass.
	r.Header.Set("Authorization", "Bearer wrong")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("forged bearer: %d", w.Code)
	}
}

func TestAuditRecordsPrincipals(t *testing.T) {
	p, audit, opToken := newAuthFixture("required")
	h := RequireAuth(okHandler(), p, audit)
	doReq(t, h, "POST", "/api/mode", "192.168.1.10:8080", "192.168.1.50:999", opToken)
	doReq(t, h, "POST", "/api/mode", "192.168.1.10:8080", "192.168.1.50:999", "")
	if len(audit.entries) != 2 {
		t.Fatalf("audit count: %d", len(audit.entries))
	}
	if audit.entries[0].Principal != "op" || audit.entries[1].Principal != "local" {
		t.Fatalf("principals: %+v", audit.entries)
	}
}
