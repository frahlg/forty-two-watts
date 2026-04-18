package selfupdate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// memStore is an in-memory Store for tests.
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

func fakeGHServer(t *testing.T, tag, url string, published time.Time) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name":     tag,
			"html_url":     url,
			"published_at": published.Format(time.RFC3339),
		})
	}))
}

func TestCheck_UpdateAvailable(t *testing.T) {
	published := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	srv := fakeGHServer(t, "v1.3.0", "https://example/releases/1.3.0", published)
	defer srv.Close()

	c := New(Config{
		CurrentVersion: "v1.2.4",
		ReleasesURL:    srv.URL,
		CheckInterval:  time.Hour,
	}, newMemStore())

	info, err := c.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.Latest != "v1.3.0" {
		t.Errorf("latest = %q, want v1.3.0", info.Latest)
	}
	if !info.UpdateAvailable {
		t.Error("UpdateAvailable should be true")
	}
	if info.ReleaseNotesURL != "https://example/releases/1.3.0" {
		t.Errorf("notes url = %q", info.ReleaseNotesURL)
	}
	if info.PublishedAt.IsZero() {
		t.Error("PublishedAt not parsed")
	}
}

func TestCheck_SameVersion(t *testing.T) {
	srv := fakeGHServer(t, "v2.0.0", "", time.Now())
	defer srv.Close()
	c := New(Config{CurrentVersion: "v2.0.0", ReleasesURL: srv.URL}, newMemStore())

	info, err := c.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.UpdateAvailable {
		t.Error("UpdateAvailable should be false when same version")
	}
}

func TestCheck_DevCurrent(t *testing.T) {
	srv := fakeGHServer(t, "v0.17.1", "", time.Now())
	defer srv.Close()
	c := New(Config{CurrentVersion: "dev", ReleasesURL: srv.URL}, newMemStore())
	info, _ := c.Check(context.Background(), false)
	if !info.UpdateAvailable {
		t.Error("dev builds should always see an upgrade as available")
	}
}

func TestCheck_CacheRespected(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.0.0"})
	}))
	defer srv.Close()

	c := New(Config{
		CurrentVersion: "v0.9.0",
		ReleasesURL:    srv.URL,
		CheckInterval:  time.Hour,
	}, newMemStore())

	// First probe hits the server.
	if _, err := c.Check(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	// Second probe within cache window — should skip.
	if _, err := c.Check(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("expected cache to suppress 2nd call; calls=%d", calls)
	}
	// Forced probe bypasses cache.
	if _, err := c.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force=true should call again; calls=%d", calls)
	}
}

func TestCheck_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte("upstream down"))
	}))
	defer srv.Close()
	c := New(Config{CurrentVersion: "v1.0.0", ReleasesURL: srv.URL}, newMemStore())
	_, err := c.Check(context.Background(), false)
	if err == nil {
		t.Fatal("expected error for 503")
	}
	if c.Info().Err == "" {
		t.Error("error should be recorded in Info.Err")
	}
}

func TestSkipAndUnskip(t *testing.T) {
	srv := fakeGHServer(t, "v1.3.0", "", time.Now())
	defer srv.Close()
	st := newMemStore()
	c := New(Config{CurrentVersion: "v1.2.0", ReleasesURL: srv.URL}, st)
	if _, err := c.Check(context.Background(), false); err != nil {
		t.Fatal(err)
	}

	if err := c.Skip("v1.3.0"); err != nil {
		t.Fatal(err)
	}
	if !c.Info().Skipped {
		t.Error("Skipped should be true after skipping latest")
	}
	if v, _ := st.LoadConfig("update.skipped_version"); v != "v1.3.0" {
		t.Errorf("persisted key = %q, want v1.3.0", v)
	}

	// Skipping a non-latest version persists but does NOT hide the latest.
	if err := c.Skip("v1.2.5"); err != nil {
		t.Fatal(err)
	}
	if c.Info().Skipped {
		t.Error("Skipping a non-latest version should not hide latest")
	}

	if err := c.Unskip(); err != nil {
		t.Fatal(err)
	}
	if c.Info().Skipped {
		t.Error("Skipped should be false after Unskip")
	}
}

func TestSkipResurfacesOnNewerRelease(t *testing.T) {
	published := time.Now()
	tag := "v1.3.0"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "published_at": published.Format(time.RFC3339)})
	}))
	defer srv.Close()

	st := newMemStore()
	c := New(Config{CurrentVersion: "v1.2.0", ReleasesURL: srv.URL, CheckInterval: time.Hour}, st)
	_, _ = c.Check(context.Background(), false)
	_ = c.Skip("v1.3.0")
	if !c.Info().Skipped {
		t.Fatal("precondition: skip should hide v1.3.0")
	}
	// New release drops — check again (forced to bypass cache).
	tag = "v1.4.0"
	_, _ = c.Check(context.Background(), true)
	if c.Info().Skipped {
		t.Error("v1.4.0 is newer than skipped v1.3.0; it should NOT be skipped")
	}
}

func TestStatus_MissingFileReturnsIdle(t *testing.T) {
	c := New(Config{StatusPath: "/nonexistent/state.json"}, newMemStore())
	if s := c.Status(); s.State != "idle" {
		t.Errorf("missing status file = %q, want idle", s.State)
	}
}

func TestStatus_ReadsAndDetectsStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	// Write a fresh "pulling" state.
	fresh := UpdateStatus{State: "pulling", Action: "update", UpdatedAt: time.Now()}
	writeJSON(t, path, fresh)

	c := New(Config{StatusPath: path}, newMemStore())
	if s := c.Status(); s.State != "pulling" {
		t.Errorf("fresh state = %q, want pulling", s.State)
	}

	// Now stale — back-date the heartbeat.
	stale := UpdateStatus{State: "pulling", Action: "update", UpdatedAt: time.Now().Add(-10 * time.Minute)}
	writeJSON(t, path, stale)
	if s := c.Status(); s.State != "failed" {
		t.Errorf("stale state = %q, want failed", s.State)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(v); err != nil {
		t.Fatal(err)
	}
}

func TestIsNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v1.2.3", "v1.2.2", true},
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.3", "v1.3.0", false},
		{"v2.0.0", "v1.99.99", true},
		{"v1.2.3", "dev", true},
		{"", "v1.2.3", false},
		{"v1.2.3-rc1", "v1.2.2", true}, // pre-release suffix tolerated by parser
		{"1.2.3", "1.2.2", true},       // no v prefix
	}
	for _, tc := range cases {
		if got := isNewer(tc.latest, tc.current); got != tc.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", tc.latest, tc.current, got, tc.want)
		}
	}
}

func TestTrigger_ValidatesAction(t *testing.T) {
	c := New(Config{SocketPath: "/tmp/notreal.sock"}, newMemStore())
	if err := c.Trigger(context.Background(), "delete-everything", ""); err == nil {
		t.Error("expected invalid action error")
	}
}

func TestTrigger_NoSocket(t *testing.T) {
	c := New(Config{}, newMemStore())
	if err := c.Trigger(context.Background(), "update", ""); err == nil {
		t.Error("expected 'socket not configured' error")
	}
}
