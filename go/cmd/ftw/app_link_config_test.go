package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/api"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
)

// An upgraded config starts without app_link. The settings round trip must
// persist the default before restart instead of silently turning the relay off.
func TestAppLinkDefaultSurvivesSettingsSaveAndRestart(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	oldYAML := []byte("site:\n  name: Test\nfuse:\n  max_amps: 16\n  phases: 3\n  voltage: 230\napi:\n  port: 8080\ndrivers: []\n")
	if err := os.WriteFile(configPath, oldYAML, 0o600); err != nil {
		t.Fatalf("write old config: %v", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load old config: %v", err)
	}
	if !cfg.AppLink.On() {
		t.Fatal("old config did not default the app link on")
	}

	var cfgMu sync.RWMutex
	var ctrlMu sync.Mutex
	srv := api.New(&api.Deps{
		Ctrl: control.NewState(0, 42, ""), CtrlMu: &ctrlMu,
		Cfg: cfg, CfgMu: &cfgMu,
		ConfigPath: configPath,
		DriverDir:  dir, UserDriverDir: dir,
		SaveConfig: config.SaveAtomic,
	})

	get := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	getRR := httptest.NewRecorder()
	srv.Handler().ServeHTTP(getRR, get)
	if getRR.Code != http.StatusOK {
		t.Fatalf("GET /api/config = %d: %s", getRR.Code, getRR.Body.String())
	}
	var shown config.Config
	if err := json.Unmarshal(getRR.Body.Bytes(), &shown); err != nil {
		t.Fatalf("decode GET /api/config: %v", err)
	}
	if !shown.AppLink.On() {
		t.Fatal("GET /api/config showed the defaulted app link as off")
	}

	post := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(getRR.Body.Bytes()))
	post.Header.Set("Content-Type", "application/json")
	postRR := httptest.NewRecorder()
	srv.Handler().ServeHTTP(postRR, post)
	if postRR.Code != http.StatusOK {
		t.Fatalf("POST /api/config = %d: %s", postRR.Code, postRR.Body.String())
	}

	restarted, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("reload saved config: %v", err)
	}
	if !restarted.AppLink.On() {
		t.Fatal("settings save changed the defaulted app link to off after restart")
	}
}

func TestAppLinkOldFullConfigClientUsesEffectiveRestartState(t *testing.T) {
	cases := []struct {
		name        string
		postedValue any
		includeKey  bool
		wantOn      bool
		wantRestart bool
	}{
		{name: "omitted", wantOn: true},
		{name: "explicit null", includeKey: true, wantRestart: true},
		{name: "explicit false", postedValue: map[string]any{"enabled": false}, includeKey: true, wantRestart: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.yaml")
			oldYAML := []byte("site:\n  name: Test\nfuse:\n  max_amps: 16\n  phases: 3\n  voltage: 230\napi:\n  port: 8080\ndrivers: []\n")
			if err := os.WriteFile(configPath, oldYAML, 0o600); err != nil {
				t.Fatalf("write old config: %v", err)
			}

			cfg, err := config.Load(configPath)
			if err != nil {
				t.Fatalf("load old config: %v", err)
			}
			var cfgMu sync.RWMutex
			var ctrlMu sync.Mutex
			srv := api.New(&api.Deps{
				Ctrl: control.NewState(0, 42, ""), CtrlMu: &ctrlMu,
				Cfg: cfg, CfgMu: &cfgMu,
				ConfigPath: configPath,
				DriverDir:  dir, UserDriverDir: dir,
				SaveConfig: config.SaveAtomic,
			})

			get := httptest.NewRequest(http.MethodGet, "/api/config", nil)
			getRR := httptest.NewRecorder()
			srv.Handler().ServeHTTP(getRR, get)
			if getRR.Code != http.StatusOK {
				t.Fatalf("GET /api/config = %d: %s", getRR.Code, getRR.Body.String())
			}
			var posted map[string]any
			if err := json.Unmarshal(getRR.Body.Bytes(), &posted); err != nil {
				t.Fatalf("decode GET /api/config: %v", err)
			}
			delete(posted, "app_link")
			if tc.includeKey {
				posted["app_link"] = tc.postedValue
			}
			body, err := json.Marshal(posted)
			if err != nil {
				t.Fatalf("encode old-client config: %v", err)
			}

			post := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
			post.Header.Set("Content-Type", "application/json")
			postRR := httptest.NewRecorder()
			srv.Handler().ServeHTTP(postRR, post)
			if postRR.Code != http.StatusOK {
				t.Fatalf("POST /api/config = %d: %s", postRR.Code, postRR.Body.String())
			}
			var response struct {
				RestartRequired bool `json:"restart_required"`
			}
			if err := json.Unmarshal(postRR.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode POST /api/config: %v", err)
			}
			if response.RestartRequired != tc.wantRestart {
				t.Fatalf("restart_required = %v, want %v; response: %s", response.RestartRequired, tc.wantRestart, postRR.Body.String())
			}

			restarted, err := config.Load(configPath)
			if err != nil {
				t.Fatalf("reload saved config: %v", err)
			}
			if got := restarted.AppLink.On(); got != tc.wantOn {
				t.Fatalf("app link after restart = %v, want %v", got, tc.wantOn)
			}
		})
	}
}

func TestEmptyAppLinkSectionRemainsOff(t *testing.T) {
	cfg, err := config.Parse([]byte("site:\n  name: Test\nfuse:\n  max_amps: 16\n  phases: 3\n  voltage: 230\napi:\n  port: 8080\ndrivers: []\napp_link: {}\n"), t.TempDir())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.AppLink == nil || cfg.AppLink.On() {
		t.Fatalf("empty app_link section must remain off, got %+v", cfg.AppLink)
	}
}

func TestAppLinkNullPostedThroughAPIStaysOffAfterRestart(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := &config.Config{}
	var cfgMu sync.RWMutex
	var ctrlMu sync.Mutex
	srv := api.New(&api.Deps{
		Ctrl: control.NewState(0, 42, ""), CtrlMu: &ctrlMu,
		Cfg: cfg, CfgMu: &cfgMu,
		ConfigPath: configPath,
		DriverDir:  dir, UserDriverDir: dir,
		SaveConfig: config.SaveAtomic,
	})
	body := []byte(`{
  "site": {"name": "Test", "smoothing_alpha": 0.3},
  "fuse": {"max_amps": 16, "phases": 3, "voltage": 230},
  "api": {"port": 8080},
  "drivers": [],
  "app_link": null
}`)
	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /api/config = %d: %s", rr.Code, rr.Body.String())
	}

	restarted, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("reload saved config: %v", err)
	}
	if restarted.AppLink == nil || restarted.AppLink.On() {
		t.Fatalf("explicit JSON null did not persist as disabled: %+v", restarted.AppLink)
	}
}
