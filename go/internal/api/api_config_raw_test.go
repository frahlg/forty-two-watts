package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/control"
	"github.com/frahlg/forty-two-watts/go/internal/state"
	"gopkg.in/yaml.v3"
)

// minimalValidConfig returns a config that passes Validate() with
// as little shape as the validator permits. Tests layer their own
// fields on top.
func minimalValidConfig(t *testing.T) *config.Config {
	t.Helper()
	mqtt := config.MQTTConfig{Host: "localhost", Port: 1883}
	return &config.Config{
		Site: config.Site{
			Name:                 "test-site",
			GridTargetW:          0,
			GridToleranceW:       100,
			SlewRateW:            500,
			SmoothingAlpha:       0.5,
			MinDispatchIntervalS: 5,
			WatchdogTimeoutS:     60,
		},
		Fuse: config.Fuse{MaxAmps: 16, Phases: 3, Voltage: 230},
		Drivers: []config.Driver{{
			Name:         "meter",
			Lua:          "drivers/fake.lua",
			IsSiteMeter:  true,
			Capabilities: config.Capabilities{MQTT: &mqtt},
		}},
	}
}

// newRawConfigTestServer wires up a Server with the minimum Deps the
// /api/config/raw + /validate handlers exercise: Cfg + mutex, State
// (in-memory SQLite for state.db keys), Ctrl, SaveConfig capture.
func newRawConfigTestServer(t *testing.T) (*Server, *config.Config, *[]byte) {
	t.Helper()
	cfg := minimalValidConfig(t)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")

	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var saved []byte
	saveCfg := func(path string, c *config.Config) error {
		data, mErr := yaml.Marshal(c)
		if mErr != nil {
			return mErr
		}
		saved = data
		return nil
	}

	ctrlMu := &sync.Mutex{}
	cfgMu := &sync.RWMutex{}
	ctrl := control.NewState(0, 100, "meter")

	deps := &Deps{
		Ctrl:       ctrl,
		CtrlMu:     ctrlMu,
		State:      st,
		CfgMu:      cfgMu,
		Cfg:        cfg,
		ConfigPath: cfgPath,
		SaveConfig: saveCfg,
	}
	return New(deps), cfg, &saved
}

// TestGetConfigRawReturnsYAML — canonical YAML body with correct Content-Type.
func TestGetConfigRawReturnsYAML(t *testing.T) {
	srv, _, _ := newRawConfigTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/config/raw", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "yaml") {
		t.Errorf("Content-Type = %q, want *yaml*", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "name: test-site") {
		t.Errorf("YAML missing expected key:\n%s", body)
	}
	if !strings.Contains(body, "max_amps: 16") {
		t.Errorf("YAML missing fuse section:\n%s", body)
	}
}

// TestGetConfigRawResolvesEVPassword — GET should inject the plaintext
// EV charger password from state.db so the editor can show/modify it.
func TestGetConfigRawResolvesEVPassword(t *testing.T) {
	srv, cfg, _ := newRawConfigTestServer(t)
	cfg.EVCharger = &config.EVCharger{Provider: "easee", Email: "u@example.com"}
	// Stash a "real" password in state.db (what the running system
	// would have done via a previous form save).
	if err := srv.deps.State.SaveConfig(evPasswordKey, "s3cr3t"); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/config/raw", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, "s3cr3t") {
		t.Errorf("GET raw should include real EV password; body:\n%s", body)
	}
}

// TestPostConfigRawRoundTrip — post a valid YAML payload, expect 200
// and the injected SaveConfig to have been called with the new data.
func TestPostConfigRawRoundTrip(t *testing.T) {
	srv, _, saved := newRawConfigTestServer(t)
	yamlBody := `site:
    name: new-site
    grid_target_w: -500
    grid_tolerance_w: 100
    slew_rate_w: 500
    min_dispatch_interval_s: 5
    watchdog_timeout_s: 60
    smoothing_alpha: 0.5
drivers:
    - name: meter
      lua: drivers/fake.lua
      is_site_meter: true
      capabilities:
        mqtt:
          host: localhost
          port: 1883
fuse:
    max_amps: 20
    phases: 3
    voltage: 230
`
	req := httptest.NewRequest(http.MethodPost, "/api/config/raw", strings.NewReader(yamlBody))
	req.Header.Set("Content-Type", "application/yaml")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(string(*saved), "name: new-site") {
		t.Errorf("SaveConfig didn't see the new site name: %s", string(*saved))
	}
	if !strings.Contains(string(*saved), "max_amps: 20") {
		t.Errorf("SaveConfig didn't see the new fuse amps: %s", string(*saved))
	}
}

// TestPostConfigRawParseErrorReturns400 — malformed YAML short-circuits
// without touching disk, with an error message that includes the line.
func TestPostConfigRawParseErrorReturns400(t *testing.T) {
	srv, _, saved := newRawConfigTestServer(t)
	// Mapping value without key → yaml.v3 reports a line.
	yamlBody := "site:\n  name: ok\n  : oops\n"
	req := httptest.NewRequest(http.MethodPost, "/api/config/raw", strings.NewReader(yamlBody))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
	if len(*saved) != 0 {
		t.Errorf("parse error still wrote to disk: %s", string(*saved))
	}
}

// TestPostConfigRawValidationErrorReturns400 — valid YAML, invalid
// config (e.g. fuse.max_amps == 0) must not land on disk.
func TestPostConfigRawValidationErrorReturns400(t *testing.T) {
	srv, _, saved := newRawConfigTestServer(t)
	yamlBody := `site:
    name: t
    grid_target_w: 0
    grid_tolerance_w: 100
    slew_rate_w: 500
    min_dispatch_interval_s: 5
    watchdog_timeout_s: 60
    smoothing_alpha: 0.5
drivers:
    - name: meter
      lua: drivers/fake.lua
      is_site_meter: true
      capabilities:
        mqtt:
          host: localhost
          port: 1883
fuse:
    max_amps: 0
    phases: 3
    voltage: 230
`
	req := httptest.NewRequest(http.MethodPost, "/api/config/raw", strings.NewReader(yamlBody))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if !strings.Contains(body["error"], "validation") {
		t.Errorf("expected 'validation' in error, got %q", body["error"])
	}
	if len(*saved) != 0 {
		t.Errorf("invalid config still wrote to disk")
	}
}

// TestPostConfigRawPersistsEVPassword — when the user types a real
// password in the YAML editor, it goes to state.db, not config.yaml.
func TestPostConfigRawPersistsEVPassword(t *testing.T) {
	srv, _, saved := newRawConfigTestServer(t)
	yamlBody := `site:
    name: t
    grid_target_w: 0
    grid_tolerance_w: 100
    slew_rate_w: 500
    min_dispatch_interval_s: 5
    watchdog_timeout_s: 60
    smoothing_alpha: 0.5
drivers:
    - name: meter
      lua: drivers/fake.lua
      is_site_meter: true
      capabilities:
        mqtt:
          host: localhost
          port: 1883
fuse:
    max_amps: 16
    phases: 3
    voltage: 230
ev_charger:
    provider: easee
    email: u@example.com
    password: hunter2
`
	req := httptest.NewRequest(http.MethodPost, "/api/config/raw", strings.NewReader(yamlBody))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}

	stored, ok := srv.deps.State.LoadConfig(evPasswordKey)
	if !ok || stored != "hunter2" {
		t.Errorf("expected password persisted to state.db, got (%v, %v)", stored, ok)
	}
	// It must also NOT be the masked placeholder or the real password
	// in the in-memory config returned by Save — we only check that
	// the saved YAML doesn't contain the secret. yaml:"-" strips it
	// from Marshal output, so nothing should leak into saved bytes.
	if strings.Contains(string(*saved), "hunter2") {
		t.Errorf("real password leaked into config.yaml: %s", string(*saved))
	}
}

// TestValidateConfigRawProducesDiff — the dry-run endpoint returns a
// unified diff without touching disk.
func TestValidateConfigRawProducesDiff(t *testing.T) {
	srv, _, saved := newRawConfigTestServer(t)
	yamlBody := `site:
    name: changed-name
    grid_target_w: 0
    grid_tolerance_w: 100
    slew_rate_w: 500
    min_dispatch_interval_s: 5
    watchdog_timeout_s: 60
    smoothing_alpha: 0.5
drivers:
    - name: meter
      lua: drivers/fake.lua
      is_site_meter: true
      capabilities:
        mqtt:
          host: localhost
          port: 1883
fuse:
    max_amps: 16
    phases: 3
    voltage: 230
`
	req := httptest.NewRequest(http.MethodPost, "/api/config/validate", strings.NewReader(yamlBody))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if ok, _ := body["ok"].(bool); !ok {
		t.Fatalf("expected ok:true, got %+v", body)
	}
	diff, _ := body["diff"].(string)
	if !strings.Contains(diff, "changed-name") {
		t.Errorf("diff didn't show the changed name:\n%s", diff)
	}
	if len(*saved) != 0 {
		t.Errorf("/validate must not write to disk, got: %s", string(*saved))
	}
}

// TestValidateConfigRawReportsLineForParseError — parse errors come
// back with a line number the editor can highlight.
func TestValidateConfigRawReportsLineForParseError(t *testing.T) {
	srv, _, _ := newRawConfigTestServer(t)
	// 4 lines, error on line 4.
	yamlBody := "site:\n  name: ok\n  mode: self_consumption\n  : oops\n"
	req := httptest.NewRequest(http.MethodPost, "/api/config/validate", strings.NewReader(yamlBody))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if ok, _ := body["ok"].(bool); ok {
		t.Fatalf("expected ok:false for parse error, got %+v", body)
	}
	if line, ok := body["line"].(float64); !ok || line <= 0 {
		t.Errorf("expected positive line number, got %v", body["line"])
	}
}

// TestPostConfigRawBodyTooLarge — bodies over the 64 KB cap are
// rejected before parsing.
func TestPostConfigRawBodyTooLarge(t *testing.T) {
	srv, _, _ := newRawConfigTestServer(t)
	big := strings.Repeat("# comment line with some filler content here\n", 2000) // ~90 KB
	req := httptest.NewRequest(http.MethodPost, "/api/config/raw", strings.NewReader(big))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
}
