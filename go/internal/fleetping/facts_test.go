package fleetping

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

func TestFactsFromConfigUsesTheCatalogueNameNotTheOperatorsName(t *testing.T) {
	// A real site's config. `name` is what the household typed and reads like
	// an address; `lua` is the catalogue file the driver came from.
	cfg := &config.Config{
		Drivers: []config.Driver{
			{Name: "sungrow-vasagatan-12", Lua: "drivers/sungrow.lua", BatteryCapacityWh: 9600},
			{Name: "pappas laddbox", Lua: "/home/fredrik/.ftw/drivers/easee_cloud.lua"},
			{Name: "gammal ferroamp", Lua: "drivers/ferroamp.lua", BatteryCapacityWh: 15000, Disabled: true},
		},
		Price: &config.Price{Zone: "SE3"},
	}
	catalogue := []string{"sungrow", "easee_cloud", "ferroamp"}

	f := FactsFromConfig(cfg, "v1.4.0", catalogue, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	if got := strings.Join(f.Drivers, ","); got != "sungrow,easee_cloud" {
		t.Fatalf("drivers = %q, want the catalogue names of the two live drivers", got)
	}
	if f.BatteryWh != 9600 {
		t.Errorf("battery = %v Wh; a disabled driver's battery is not installed capacity", f.BatteryWh)
	}
	if f.PriceZone != "SE3" {
		t.Errorf("zone = %q", f.PriceZone)
	}

	// End to end: the household's own words must not survive into the message.
	raw, err := json.Marshal(Build(f, time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := strings.ToLower(string(raw))
	for _, forbidden := range []string{"vasagatan", "pappas", "fredrik", "gammal", ".ftw", ".lua"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("ping contains %q: %s", forbidden, raw)
		}
	}
}

func TestFactsFromConfigSurvivesAnEmptyBox(t *testing.T) {
	// A box mid-setup: no drivers, no price provider. It still counts, and it
	// must not panic on the way.
	f := FactsFromConfig(&config.Config{}, "v1.4.0", nil, time.Time{})
	got := marshal(t, Build(f, time.Now()))
	if got["price_zone"] != "unknown" || got["battery_kwh"] != "none" || got["install_age"] != "unknown" {
		t.Fatalf("empty box reports %v", got)
	}
	if _, ok := got["drivers"]; !ok {
		t.Fatal("drivers key is missing entirely")
	}
}

func TestDriverFileTypeDropsThePath(t *testing.T) {
	// The directory a driver was installed in says where the box is and who
	// set it up. Only the file's own name is a type.
	for _, tc := range []struct{ in, want string }{
		{"drivers/sungrow.lua", "sungrow"},
		{"/home/fredrik/.ftw/drivers/easee_cloud.lua", "easee_cloud"},
		{`C:\Users\Fredrik\ftw\drivers\deye.lua`, "deye"},
		{"  drivers/zap.lua  ", "zap"},
		{"", ""},
	} {
		if got := driverFileType(tc.in); got != tc.want {
			t.Errorf("driverFileType(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
