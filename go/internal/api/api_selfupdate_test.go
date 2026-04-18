package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/selfupdate"
)

// memStore satisfies selfupdate.Store for the wiring tests.
type memStore struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemStore() *memStore { return &memStore{m: map[string]string{}} }
func (s *memStore) SaveConfig(k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[k] = v
	return nil
}
func (s *memStore) LoadConfig(k string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[k]
	return v, ok
}

// newCheckerAgainst returns a Checker that fetches from the supplied fake
// GH response and has already primed the cache with one Check so subsequent
// handler calls see the right state.
func newCheckerAgainst(t *testing.T, tag, current string) *selfupdate.Checker {
	t.Helper()
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name":     tag,
			"html_url":     "https://example/releases/" + tag,
			"published_at": time.Now().Format(time.RFC3339),
		})
	}))
	t.Cleanup(ghSrv.Close)
	c := selfupdate.New(selfupdate.Config{
		CurrentVersion: current,
		ReleasesURL:    ghSrv.URL,
		CheckInterval:  time.Hour,
	}, newMemStore())
	if _, err := c.Check(t.Context(), true); err != nil {
		t.Fatalf("priming check: %v", err)
	}
	return c
}

func TestVersionCheck_Disabled(t *testing.T) {
	srv := New(&Deps{})
	req := httptest.NewRequest(http.MethodGet, "/api/version/check", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled should be 503, got %d", rr.Code)
	}
}

func TestVersionCheck_ReturnsInfo(t *testing.T) {
	c := newCheckerAgainst(t, "v1.5.0", "v1.4.0")
	srv := New(&Deps{SelfUpdate: c})

	req := httptest.NewRequest(http.MethodGet, "/api/version/check", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d", rr.Code)
	}
	var info selfupdate.Info
	if err := json.Unmarshal(rr.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Latest != "v1.5.0" || !info.UpdateAvailable {
		t.Errorf("unexpected info: %+v", info)
	}
}

func TestVersionSkip_RoundTrip(t *testing.T) {
	c := newCheckerAgainst(t, "v1.5.0", "v1.4.0")
	srv := New(&Deps{SelfUpdate: c})

	// Skip v1.5.0 — should now report Skipped=true.
	body := strings.NewReader(`{"version":"v1.5.0"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/version/skip", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("skip status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !c.Info().Skipped {
		t.Error("Skipped should be true after POST /skip")
	}

	// Unskip — should clear.
	req = httptest.NewRequest(http.MethodPost, "/api/version/unskip", nil)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("unskip status = %d", rr.Code)
	}
	if c.Info().Skipped {
		t.Error("Skipped should be false after POST /unskip")
	}
}

func TestVersionSkip_EmptyVersionRejected(t *testing.T) {
	c := newCheckerAgainst(t, "v1.5.0", "v1.4.0")
	srv := New(&Deps{SelfUpdate: c})
	req := httptest.NewRequest(http.MethodPost, "/api/version/skip", strings.NewReader(`{"version":""}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty version should be 400, got %d", rr.Code)
	}
}

func TestVersionUpdate_NoSidecar502(t *testing.T) {
	// Checker has no socket configured — Trigger returns an error and the
	// handler surfaces it as 502 so the UI can show the sidecar is missing.
	c := newCheckerAgainst(t, "v1.5.0", "v1.4.0")
	srv := New(&Deps{SelfUpdate: c})

	for _, path := range []string{"/api/version/update", "/api/version/restart"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusBadGateway {
			t.Errorf("%s without sidecar = %d, want 502", path, rr.Code)
		}
	}
}

func TestVersionUpdateStatus_Idle(t *testing.T) {
	c := newCheckerAgainst(t, "v1.5.0", "v1.4.0")
	srv := New(&Deps{SelfUpdate: c})
	req := httptest.NewRequest(http.MethodGet, "/api/version/update/status", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d", rr.Code)
	}
	var out selfupdate.UpdateStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.State != "idle" {
		t.Errorf("state = %q, want idle (no StatusPath configured)", out.State)
	}
}
