package loadpoint_test

import (
	"math"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestConfirmedBatteryLevelSurvivesDatabaseCloseAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := []loadpoint.Config{{ID: "garage", DriverName: "charger", VehicleCapacityWh: 60000}}
	m := loadpoint.NewManager()
	m.Load(cfg)
	m.SetSessionStore(store)
	m.ObserveSession("garage", true, 4300, 9000, true, "easee:TEST", "728:2026-01-01T08:00:00Z")
	if !m.SetCurrentSoC("garage", .84) {
		t.Fatal("level refused")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m = loadpoint.NewManager()
	m.Load(cfg)
	m.SetSessionStore(store)
	m.ObserveSession("garage", true, 4300, 9600, true, "easee:TEST", "728:2026-01-01T08:00:00Z")
	s, _ := m.State("garage")
	if math.Abs(s.CurrentSoC-.85) > 1e-9 || s.SoCSource == "assumed" || s.SoCRetention != "session" {
		t.Fatalf("restart did not retain level: %+v", s)
	}
}
