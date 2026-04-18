package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner records the compose commands the server attempted to run so
// tests can assert on arg order without touching docker.
type fakeRunner struct {
	mu     sync.Mutex
	calls  [][]string
	fail   bool
	failOn string
}

func (f *fakeRunner) run(ctx context.Context, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{}, args...))
	if f.fail {
		return errors.New("forced failure")
	}
	if f.failOn != "" {
		for _, a := range args {
			if a == f.failOn {
				return errors.New("forced failure on " + f.failOn)
			}
		}
	}
	return nil
}

func (f *fakeRunner) snapshot() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	copy(out, f.calls)
	return out
}

func newTestServer(t *testing.T) (*server, *fakeRunner) {
	t.Helper()
	dir := t.TempDir()
	runner := &fakeRunner{}
	s := &server{
		composeFile: filepath.Join(dir, "docker-compose.yml"),
		statusPath:  filepath.Join(dir, "state.json"),
		runner:      runner.run,
	}
	return s, runner
}

func TestSkipPull_BypassesPullStep(t *testing.T) {
	s, runner := newTestServer(t)
	s.skipPull = true

	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"update"}`))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	waitForState(t, s, "done")
	calls := runner.snapshot()
	if len(calls) != 1 {
		t.Fatalf("skip-pull should yield 1 call (up only), got %d: %v", len(calls), calls)
	}
	if !strings.Contains(strings.Join(calls[0], " "), "up -d") {
		t.Errorf("single call should be `up -d`: %v", calls[0])
	}
}

func waitForState(t *testing.T, s *server, want string) State {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st := s.readState(); st.State == want {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("state never reached %q (last=%+v)", want, s.readState())
	return State{}
}

func TestHandleUpdate_HappyPath(t *testing.T) {
	s, runner := newTestServer(t)
	body := bytes.NewBufferString(`{"action":"update","target":"v1.2.3"}`)
	req := httptest.NewRequest(http.MethodPost, "/update", body)
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	if rr.Code != 202 {
		t.Fatalf("status = %d", rr.Code)
	}
	st := waitForState(t, s, "done")
	if st.Action != "update" || st.Target != "v1.2.3" {
		t.Errorf("unexpected final state: %+v", st)
	}
	calls := runner.snapshot()
	if len(calls) != 2 {
		t.Fatalf("expected 2 docker calls, got %d: %v", len(calls), calls)
	}
	if calls[0][0] != "compose" || !strings.Contains(strings.Join(calls[0], " "), "pull") {
		t.Errorf("first call should be pull: %v", calls[0])
	}
	up := strings.Join(calls[1], " ")
	if !strings.Contains(up, "up -d") || strings.Contains(up, "--force-recreate") {
		t.Errorf("update path should NOT force-recreate: %v", calls[1])
	}
}

func TestHandleUpdate_RestartForceRecreates(t *testing.T) {
	s, runner := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"restart"}`))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	if rr.Code != 202 {
		t.Fatalf("status = %d", rr.Code)
	}
	waitForState(t, s, "done")
	up := strings.Join(runner.snapshot()[1], " ")
	if !strings.Contains(up, "--force-recreate") {
		t.Errorf("restart path must force-recreate: %v", up)
	}
}

func TestHandleUpdate_PullFailure(t *testing.T) {
	s, runner := newTestServer(t)
	runner.fail = true

	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"update"}`))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	st := waitForState(t, s, "failed")
	if !strings.Contains(st.Message, "pull") {
		t.Errorf("failure should mention pull: %+v", st)
	}
}

func TestHandleUpdate_UpFailure(t *testing.T) {
	s, runner := newTestServer(t)
	runner.failOn = "up"

	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"update"}`))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	st := waitForState(t, s, "failed")
	if !strings.Contains(st.Message, "up") {
		t.Errorf("failure should mention up: %+v", st)
	}
}

func TestHandleUpdate_RejectsBadAction(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"rm -rf"}`))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	if rr.Code != 400 {
		t.Fatalf("bad action should 400, got %d", rr.Code)
	}
}

func TestHandleUpdate_ConcurrentRejected(t *testing.T) {
	s, _ := newTestServer(t)
	// Swap the runner for a blocking one so the first job lingers and the
	// second request arrives while runMu is held.
	block := make(chan struct{})
	s.runner = func(ctx context.Context, _ ...string) error {
		<-block
		return nil
	}

	req1 := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"update"}`))
	rr1 := httptest.NewRecorder()
	s.handleUpdate(rr1, req1)
	if rr1.Code != 202 {
		t.Fatalf("first call = %d", rr1.Code)
	}

	// Second call while the first is holding the lock → 409.
	req2 := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"restart"}`))
	rr2 := httptest.NewRecorder()
	s.handleUpdate(rr2, req2)
	if rr2.Code != 409 {
		t.Fatalf("second call should 409, got %d", rr2.Code)
	}
	close(block)
	waitForState(t, s, "done")
}

func TestHandleStatus_ReadsFile(t *testing.T) {
	s, _ := newTestServer(t)
	s.writeState(State{State: "pulling", Action: "update", UpdatedAt: time.Now()})
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rr := httptest.NewRecorder()
	s.handleStatus(rr, req)
	var st State
	if err := json.NewDecoder(rr.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.State != "pulling" {
		t.Errorf("state = %q", st.State)
	}
}

func TestDiscoverOverridesAndComposeArgs(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(base, []byte("version: '3'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No override yet → no extra -f flags.
	if got := discoverOverrides(base); len(got) != 0 {
		t.Errorf("no override yet, got %v", got)
	}
	override := filepath.Join(dir, "docker-compose.override.yml")
	if err := os.WriteFile(override, []byte("version: '3'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{composeFile: base, overrideFiles: discoverOverrides(base)}
	args := s.composeArgs("up", "-d", "svc")
	want := []string{"compose", "-f", base, "-f", override, "up", "-d", "svc"}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Errorf("composeArgs =\n  %v\nwant\n  %v", args, want)
	}
}

func TestRecoverCrashedState(t *testing.T) {
	s, _ := newTestServer(t)
	s.writeState(State{State: "pulling", UpdatedAt: time.Now().Add(-10 * time.Minute)})
	s.recoverCrashedState()
	if st := s.readState(); st.State != "failed" {
		t.Errorf("recovery should have flipped state to failed, got %q", st.State)
	}
}
