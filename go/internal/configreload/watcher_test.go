package configreload

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
)

// minimalYAML is the smallest config that passes config.Load validation.
const minimalYAML = `
site:
  name: Test
  grid_target_w: 0
fuse:
  max_amps: 16
drivers:
  - name: ferroamp
    lua: drivers/ferroamp.lua
    is_site_meter: true
    capabilities:
      mqtt:
        host: 192.168.1.153
api:
  port: 8080
`

// writeConfig writes YAML content to the config file.
func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func waitForWatcherStart(t *testing.T, w *Watcher) {
	t.Helper()
	select {
	case <-w.started:
	case <-time.After(time.Second):
		t.Fatal("watcher loop did not start")
	}
}

// newTestWatcher creates a Watcher wired to track applier invocations.
// Returns the watcher plus an atomic counter and a channel that receives
// each (new, old) pair delivered to the applier.
func newTestWatcher(t *testing.T, cfgPath string, cfg *config.Config) (
	*Watcher, *atomic.Int32, chan [2]*config.Config,
) {
	t.Helper()
	var cfgMu sync.RWMutex
	var ctrlMu sync.Mutex
	ctrl := control.NewState(cfg.Site.GridTargetW, cfg.Site.GridToleranceW, cfg.SiteMeterDriver())

	var calls atomic.Int32
	applyCh := make(chan [2]*config.Config, 8)

	w, err := New(cfgPath, &cfgMu, cfg, &ctrlMu, ctrl, func(newCfg, oldCfg *config.Config) {
		calls.Add(1)
		applyCh <- [2]*config.Config{newCfg, oldCfg}
	})
	if err != nil {
		t.Fatal(err)
	}
	return w, &calls, applyCh
}

func TestWatcherFiresOnChange(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeConfig(t, cfgPath, minimalYAML)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	w, _, applyCh := newTestWatcher(t, cfgPath, cfg)
	w.Start()
	waitForWatcherStart(t, w)
	defer w.Stop()

	// Modify the config: change grid_target_w from 0 to 100.
	updatedYAML := `
site:
  name: Test
  grid_target_w: 100
fuse:
  max_amps: 16
drivers:
  - name: ferroamp
    lua: drivers/ferroamp.lua
    is_site_meter: true
    capabilities:
      mqtt:
        host: 192.168.1.153
api:
  port: 8080
`
	writeConfig(t, cfgPath, updatedYAML)

	select {
	case pair := <-applyCh:
		newCfg := pair[0]
		if newCfg.Site.GridTargetW != 100 {
			t.Errorf("expected grid_target_w=100, got %f", newCfg.Site.GridTargetW)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("applier not called within 3 s after config change")
	}
}

func TestWatcherIgnoresInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeConfig(t, cfgPath, minimalYAML)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	w, calls, _ := newTestWatcher(t, cfgPath, cfg)
	w.Start()
	waitForWatcherStart(t, w)
	defer w.Stop()

	// Write invalid YAML — config.Load will fail, reload() returns early,
	// and the applier should NOT be called.
	writeConfig(t, cfgPath, "{{{{not: valid: yaml: [")

	// Wait long enough for debounce (500 ms) + some margin.
	time.Sleep(1500 * time.Millisecond)

	if n := calls.Load(); n != 0 {
		t.Errorf("applier called %d times on invalid YAML; expected 0", n)
	}
}

func TestWatcherUpdatesSiteMeterDriverOnReload(t *testing.T) {
	// Operator moves `is_site_meter: true` from `ferroamp` to
	// `zap-p1` (typical when commissioning a real meter alongside
	// the sim). Without this change the dispatcher kept reading
	// from the old driver — grid_w pegged at 0 once the old
	// driver stopped emitting. The fix updates ctrl.SiteMeterDriver
	// inside the same ctrlMu block that gates dispatch reads of it.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeConfig(t, cfgPath, minimalYAML)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	var cfgMu sync.RWMutex
	var ctrlMu sync.Mutex
	ctrl := control.NewState(cfg.Site.GridTargetW, cfg.Site.GridToleranceW, cfg.SiteMeterDriver())
	if ctrl.SiteMeterDriver != "ferroamp" {
		t.Fatalf("setup precondition: ctrl.SiteMeterDriver = %q, want ferroamp", ctrl.SiteMeterDriver)
	}

	applierCh := make(chan struct{}, 1)
	w, err := New(cfgPath, &cfgMu, cfg, &ctrlMu, ctrl, func(_, _ *config.Config) {
		applierCh <- struct{}{}
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Start()
	waitForWatcherStart(t, w)
	defer w.Stop()

	// Two-driver YAML with the site-meter flag moved to zap-p1.
	updatedYAML := `
site:
  name: Test
  grid_target_w: 0
fuse:
  max_amps: 16
drivers:
  - name: ferroamp
    lua: drivers/ferroamp.lua
    capabilities:
      mqtt:
        host: 192.168.1.153
  - name: zap-p1
    lua: drivers/esphome_dsmr.lua
    is_site_meter: true
    capabilities:
      http:
        allowed_hosts: ["192.168.1.147"]
    config:
      host: "192.168.1.147"
api:
  port: 8080
`
	writeConfig(t, cfgPath, updatedYAML)

	select {
	case <-applierCh:
	case <-time.After(3 * time.Second):
		t.Fatal("applier not called within 3 s after config change")
	}

	ctrlMu.Lock()
	got := ctrl.SiteMeterDriver
	ctrlMu.Unlock()
	if got != "zap-p1" {
		t.Errorf("after hot reload, ctrl.SiteMeterDriver = %q, want zap-p1", got)
	}
}

func TestWatcherStopIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeConfig(t, cfgPath, minimalYAML)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	w, _, _ := newTestWatcher(t, cfgPath, cfg)
	w.Start()
	waitForWatcherStart(t, w)

	// First Stop should succeed normally.
	w.Stop()
	select {
	case <-w.done:
	default:
		t.Fatal("Stop returned before watcher loop exited")
	}

	// Second Stop must not panic (guarded by sync.Once).
	w.Stop()
}

func TestWatcherStopBeforeStart(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeConfig(t, cfgPath, minimalYAML)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	w, _, _ := newTestWatcher(t, cfgPath, cfg)
	stopped := make(chan struct{})
	go func() {
		w.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop blocked before watcher started")
	}

	w.Start()
	w.lifecycleMu.Lock()
	loopStarted := w.loopStarted
	w.lifecycleMu.Unlock()
	if loopStarted {
		t.Fatal("Start launched watcher after Stop")
	}
}

func TestWatcherStartIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeConfig(t, cfgPath, minimalYAML)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	w, calls, applyCh := newTestWatcher(t, cfgPath, cfg)
	w.Start()
	waitForWatcherStart(t, w)
	w.Start()
	defer w.Stop()

	updatedYAML := `
site:
  name: Test
  grid_target_w: 100
fuse:
  max_amps: 16
drivers:
  - name: ferroamp
    lua: drivers/ferroamp.lua
    is_site_meter: true
    capabilities:
      mqtt:
        host: 192.168.1.153
api:
  port: 8080
`

	writeConfig(t, cfgPath, updatedYAML)

	select {
	case pair := <-applyCh:
		newCfg := pair[0]
		if newCfg.Site.GridTargetW != 100 {
			t.Errorf("expected grid_target_w=100, got %f", newCfg.Site.GridTargetW)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("applier not called within 3 s after config change")
	}

	select {
	case <-applyCh:
		t.Fatal("applier called more than once after duplicate Start")
	case <-time.After(750 * time.Millisecond):
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("applier called %d times after duplicate Start; expected exactly 1", n)
	}

	w.Stop()
	w.Stop()
}

// Apply is the one shared apply path (#760): POST /api/config calls it
// directly with the config it just saved, so a site meter set for the
// first time must reach the controller without any fsnotify round trip.
func TestApplyFirstSiteMeterWithoutAWatcher(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	const noDriversYAML = `
site:
  name: Test
  grid_target_w: 0
fuse:
  max_amps: 16
drivers: []
api:
  port: 8080
`
	writeConfig(t, path, noDriversYAML)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	writeConfig(t, path, minimalYAML)
	newCfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	var cfgMu sync.RWMutex
	var ctrlMu sync.Mutex
	ctrl := control.NewState(0, 0, cfg.SiteMeterDriver())

	var gotNew, gotOld *config.Config
	Apply(&cfgMu, cfg, &ctrlMu, ctrl, newCfg, func(n, o *config.Config) {
		gotNew, gotOld = n, o
	})

	if ctrl.SiteMeterDriver != "ferroamp" {
		t.Fatalf("Ctrl.SiteMeterDriver = %q, want %q", ctrl.SiteMeterDriver, "ferroamp")
	}
	if cfg.SiteMeterDriver() != "ferroamp" {
		t.Fatalf("shared cfg not swapped: SiteMeterDriver() = %q", cfg.SiteMeterDriver())
	}
	if gotNew == nil || gotNew.SiteMeterDriver() != "ferroamp" {
		t.Fatal("applier did not receive the new config")
	}
	if gotOld == nil || gotOld.SiteMeterDriver() != "" {
		t.Fatal("applier did not receive the pre-apply snapshot as old")
	}
}
