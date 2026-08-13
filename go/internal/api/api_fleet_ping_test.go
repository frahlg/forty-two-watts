package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/fleetping"
)

type silentProvider struct{}

func (silentProvider) Send(context.Context, fleetping.Payload) error { return nil }
func (silentProvider) EndpointURL() string                           { return config.DefaultFleetPingEndpoint }

func fleetPingServer(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	pinger, err := fleetping.New(fleetping.Options{
		Provider: silentProvider{},
		Facts: func() fleetping.Facts {
			return fleetping.FactsFromConfig(cfg, "v1.4.0",
				[]string{"sungrow", "easee_cloud"},
				time.Now().AddDate(0, -2, 0))
		},
	})
	if err != nil {
		t.Fatalf("fleetping.New: %v", err)
	}
	return New(&Deps{CfgMu: &sync.RWMutex{}, Cfg: cfg, FleetPing: pinger})
}

func TestFleetPingShowsTheRealPayload(t *testing.T) {
	// The Settings screen makes a claim about what leaves the house. It is
	// only checkable if this endpoint renders the message itself, built from
	// this box's own values.
	cfg := &config.Config{
		FleetPing: &config.FleetPing{Enabled: true, Endpoint: config.DefaultFleetPingEndpoint},
		Drivers: []config.Driver{
			{Name: "sungrow-vasagatan-12", Lua: "drivers/sungrow.lua", BatteryCapacityWh: 9600},
		},
		Price: &config.Price{Zone: "SE3"},
	}

	w := httptest.NewRecorder()
	fleetPingServer(t, cfg).Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/fleet-ping", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Enabled  bool           `json:"enabled"`
		Endpoint string         `json:"endpoint"`
		Payload  map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Enabled || got.Endpoint != config.DefaultFleetPingEndpoint {
		t.Errorf("enabled=%v endpoint=%q", got.Enabled, got.Endpoint)
	}
	if got.Payload["price_zone"] != "SE3" || got.Payload["battery_kwh"] != "5-15" {
		t.Errorf("payload = %v, want this box's own values", got.Payload)
	}
	drivers, ok := got.Payload["drivers"].([]any)
	if !ok || len(drivers) != 1 || drivers[0] != "sungrow" {
		t.Errorf("drivers = %v, want the catalogue name, never the operator's", got.Payload["drivers"])
	}
}

func TestFleetPingStillShowsThePayloadWhenSwitchedOff(t *testing.T) {
	// Seeing the message while it is off is most of the point: somebody
	// deciding whether to turn it on needs to read it first.
	cfg := &config.Config{
		FleetPing: &config.FleetPing{Enabled: false, Endpoint: config.DefaultFleetPingEndpoint},
		Price:     &config.Price{Zone: "SE3"},
	}

	w := httptest.NewRecorder()
	fleetPingServer(t, cfg).Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/fleet-ping", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Enabled bool           `json:"enabled"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Enabled {
		t.Error("reported enabled while switched off")
	}
	if got.Payload["schema"] != fleetping.Schema {
		t.Errorf("payload = %v, want the message it would send", got.Payload)
	}
}

func TestFleetPingShowsTheRunningEndpointUntilRestart(t *testing.T) {
	cfg := &config.Config{
		FleetPing: &config.FleetPing{Enabled: true, Endpoint: "https://saved.example.test/fleet"},
	}

	w := httptest.NewRecorder()
	fleetPingServer(t, cfg).Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/fleet-ping", nil))

	var got fleetPingView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Endpoint != config.DefaultFleetPingEndpoint {
		t.Errorf("endpoint = %q, want the running provider %q", got.Endpoint, config.DefaultFleetPingEndpoint)
	}
}

func TestFleetPingSaysSoWhenItIsNotRunning(t *testing.T) {
	s := New(&Deps{CfgMu: &sync.RWMutex{}, Cfg: &config.Config{}})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/fleet-ping", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", w.Code)
	}
}
