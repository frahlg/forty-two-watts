package drivers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func teslaWCDriverPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "testdata", "tesla_wall_connector.lua")
}

type teslaWCFake struct {
	versionHits int
	vitalsHits  int
	vitalsBody  []byte
}

func (f *teslaWCFake) handler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/1/version":
		f.versionHits++
		_ = json.NewEncoder(w).Encode(map[string]string{
			"serial_number":    "TWC-SN-1",
			"part_number":      "1529455-02-J",
			"firmware_version": "22.36.1",
		})
	case "/api/1/vitals":
		f.vitalsHits++
		if len(f.vitalsBody) > 0 {
			_, _ = w.Write(f.vitalsBody)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"contactor_closed":  true,
			"vehicle_connected": true,
			"grid_v":            230.0,
			"vehicle_current_a": 16.0,
			"currentA_a":        16.1,
			"currentB_a":        16.0,
			"currentC_a":        16.2,
			"voltageA_v":        230.0,
			"voltageB_v":        229.5,
			"voltageC_v":        230.4,
			"session_energy_wh": 4200.0,
			"evse_state":        11,
		})
	default:
		http.Error(w, "unknown "+r.URL.Path, http.StatusNotFound)
	}
}

func TestTeslaWallConnectorInitPollAndReadonlyCommands(t *testing.T) {
	fake := &teslaWCFake{}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()

	tel := telemetry.NewStore()
	env := NewHostEnv("tesla-wc", tel).WithHTTP()
	d, err := NewLuaDriver(teslaWCDriverPath(t), env)
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	defer d.Cleanup()

	if err := d.Init(context.Background(), map[string]any{
		"base_url": srv.URL,
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if fake.versionHits != 1 {
		t.Fatalf("version hits=%d, want 1", fake.versionHits)
	}

	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	reading := tel.Get("tesla-wc", telemetry.DerEV)
	if reading == nil {
		t.Fatal("no EV reading after poll")
	}
	// 230*16.1 + 229.5*16.0 + 230.4*16.2 = 3703 + 3672 + 3732.48 = 11107.48
	if reading.RawW < 11100 || reading.RawW > 11120 {
		t.Errorf("EV W=%v, want ~11107 (sum of V×A)", reading.RawW)
	}
	var extra map[string]any
	if err := json.Unmarshal(reading.Data, &extra); err != nil {
		t.Fatalf("decode EV data: %v", err)
	}
	if extra["connected"] != true {
		t.Errorf("connected=%v, want true", extra["connected"])
	}
	if extra["charging"] != true {
		t.Errorf("charging=%v, want true", extra["charging"])
	}
	if extra["session_wh"] != 4200.0 {
		t.Errorf("session_wh=%v, want 4200", extra["session_wh"])
	}

	if err := d.Command(context.Background(), []byte(`{"action":"ev_set_current","power_w":11040}`)); err != nil {
		t.Fatalf("ev_set_current should ack: %v", err)
	}
	if err := d.Command(context.Background(), []byte(`{"action":"ev_pause"}`)); err != nil {
		t.Fatalf("ev_pause should ack: %v", err)
	}
	if err := d.DefaultMode(); err != nil {
		t.Fatalf("default mode: %v", err)
	}
}

func TestTeslaWallConnectorRepairsBareNaN(t *testing.T) {
	fake := &teslaWCFake{vitalsBody: []byte(`{"vehicle_connected":false,"evse_state":1,"grid_v":nan,"session_energy_wh":0}`)}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()

	tel := telemetry.NewStore()
	env := NewHostEnv("tesla-wc", tel).WithHTTP()
	d, err := NewLuaDriver(teslaWCDriverPath(t), env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), map[string]any{"base_url": srv.URL}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if tel.Get("tesla-wc", telemetry.DerEV) == nil {
		t.Fatal("expected EV reading after repaired nan JSON")
	}
}

func TestTeslaWallConnectorHostConfig(t *testing.T) {
	fake := &teslaWCFake{}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	tel := telemetry.NewStore()
	env := NewHostEnv("tesla-wc", tel).WithHTTP()
	d, err := NewLuaDriver(teslaWCDriverPath(t), env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), map[string]any{"host": host}); err != nil {
		t.Fatalf("init with host: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if tel.Get("tesla-wc", telemetry.DerEV) == nil {
		t.Fatal("expected EV reading when configured with bare host")
	}
}
