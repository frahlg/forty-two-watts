package config

import (
	"fmt"
	"path/filepath"
	"testing"
)

const minimalYAML = `
site:
  name: Test
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

func TestLoadMinimalYAML(t *testing.T) {
	c, err := Parse([]byte(minimalYAML), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if c.Site.Name != "Test" {
		t.Errorf("site name: got %q", c.Site.Name)
	}
	// Defaults applied
	if c.Site.ControlIntervalS != 5 {
		t.Errorf("default control_interval_s: got %d", c.Site.ControlIntervalS)
	}
	if c.Site.GridToleranceW != 42 {
		t.Errorf("default grid_tolerance_w: got %f", c.Site.GridToleranceW)
	}
	if c.Fuse.Phases != 3 {
		t.Errorf("default fuse phases: got %d", c.Fuse.Phases)
	}
	if c.API.Port != 8080 {
		t.Errorf("api port: got %d", c.API.Port)
	}
	if c.Drivers[0].Capabilities.MQTT.Port != 1883 {
		t.Errorf("mqtt default port: got %d", c.Drivers[0].Capabilities.MQTT.Port)
	}
}

func TestRelativeDriverPathResolved(t *testing.T) {
	c, err := Parse([]byte(minimalYAML), "/base/dir")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/base/dir", "drivers/ferroamp.lua")
	if c.Drivers[0].Lua != want {
		t.Errorf("lua path: got %s want %s", c.Drivers[0].Lua, want)
	}
}

func TestRejectsNoDrivers(t *testing.T) {
	yaml := `
site: { name: x }
fuse: { max_amps: 16 }
drivers: []
api: { port: 8080 }
`
	if _, err := Parse([]byte(yaml), "."); err == nil {
		t.Fatal("expected error for empty drivers")
	}
}

func TestRejectsNoSiteMeter(t *testing.T) {
	yaml := `
site: { name: x }
fuse: { max_amps: 16 }
drivers:
  - name: a
    lua: a.lua
    capabilities:
      mqtt: { host: 1.1.1.1 }
api: { port: 8080 }
`
	_, err := Parse([]byte(yaml), ".")
	if err == nil {
		t.Fatal("expected error for no site meter")
	}
}

func TestRejectsDuplicateDriverNames(t *testing.T) {
	yaml := `
site: { name: x }
fuse: { max_amps: 16 }
drivers:
  - name: a
    lua: a.lua
    is_site_meter: true
    capabilities: { mqtt: { host: 1.1.1.1 } }
  - name: a
    lua: b.lua
    capabilities: { mqtt: { host: 2.2.2.2 } }
api: { port: 8080 }
`
	if _, err := Parse([]byte(yaml), "."); err == nil {
		t.Fatal("expected error for duplicate names")
	}
}

func TestRejectsDriverWithoutProtocol(t *testing.T) {
	yaml := `
site: { name: x }
fuse: { max_amps: 16 }
drivers:
  - name: a
    lua: a.lua
    is_site_meter: true
api: { port: 8080 }
`
	if _, err := Parse([]byte(yaml), "."); err == nil {
		t.Fatal("expected error for driver without protocol")
	}
}

func TestRejectsDriverWithoutLua(t *testing.T) {
	yaml := `
site: { name: x }
fuse: { max_amps: 16 }
drivers:
  - name: a
    is_site_meter: true
    capabilities: { mqtt: { host: 1.1.1.1 } }
api: { port: 8080 }
`
	if _, err := Parse([]byte(yaml), "."); err == nil {
		t.Fatal("expected error for driver without lua")
	}
}

func TestLegacyMqttFallsBackToCapabilities(t *testing.T) {
	yaml := `
site: { name: x }
fuse: { max_amps: 16 }
drivers:
  - name: a
    lua: a.lua
    is_site_meter: true
    mqtt: { host: 192.168.1.100, username: ext }
api: { port: 8080 }
`
	c, err := Parse([]byte(yaml), ".")
	if err != nil {
		t.Fatal(err)
	}
	mq := c.Drivers[0].EffectiveMQTT()
	if mq == nil || mq.Host != "192.168.1.100" || mq.Username != "ext" {
		t.Errorf("legacy mqtt fallback failed: %+v", mq)
	}
}

func TestFuseMaxPower(t *testing.T) {
	f := Fuse{MaxAmps: 16, Phases: 3, Voltage: 230}
	want := 16.0 * 230 * 3
	if f.MaxPowerW() != want {
		t.Errorf("fuse power: got %f want %f", f.MaxPowerW(), want)
	}
}

func TestSmoothingAlphaValidation(t *testing.T) {
	// alpha=0 means "use default" via applyDefaults, so only test truly invalid values
	for _, bad := range []float64{-0.1, 1.1, 2.0} {
		yaml := `
site: { name: x, smoothing_alpha: ` + pretty(bad) + ` }
fuse: { max_amps: 16 }
drivers:
  - name: a
    lua: a.lua
    is_site_meter: true
    capabilities: { mqtt: { host: 1.1.1.1 } }
api: { port: 8080 }
`
		if _, err := Parse([]byte(yaml), "."); err == nil {
			t.Errorf("alpha=%v should fail validation", bad)
		}
	}
}

func TestAllOptionalSectionsParse(t *testing.T) {
	yaml := `
site: { name: Full }
fuse: { max_amps: 16 }
drivers:
  - name: f
    lua: f.lua
    is_site_meter: true
    capabilities: { mqtt: { host: 1.1.1.1 } }
api: { port: 8080 }
homeassistant:
  enabled: true
  broker: 192.168.1.1
state:
  path: state.db
price:
  provider: elprisetjustnu
  zone: SE3
  vat_percent: 25
weather:
  provider: met_no
  latitude: 59.3293
  longitude: 18.0686
batteries:
  f:
    soc_min: 0.1
    weight: 2.0
`
	c, err := Parse([]byte(yaml), ".")
	if err != nil {
		t.Fatal(err)
	}
	if c.HomeAssistant == nil || !c.HomeAssistant.Enabled {
		t.Error("homeassistant section missing")
	}
	if c.Price == nil || c.Price.Zone != "SE3" {
		t.Error("price section missing")
	}
	if c.Weather == nil || c.Weather.Latitude != 59.3293 {
		t.Error("weather section missing")
	}
	if c.Batteries["f"].SoCMin == nil || *c.Batteries["f"].SoCMin != 0.1 {
		t.Error("battery override missing")
	}
}

func TestSiteMeterDriverReturnsName(t *testing.T) {
	c, err := Parse([]byte(minimalYAML), ".")
	if err != nil {
		t.Fatal(err)
	}
	if c.SiteMeterDriver() != "ferroamp" {
		t.Errorf("SiteMeterDriver: got %q", c.SiteMeterDriver())
	}
}

func TestSaveAtomicRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	c, _ := Parse([]byte(minimalYAML), dir)
	if err := SaveAtomic(path, c); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Site.Name != c.Site.Name {
		t.Errorf("roundtrip site.name: got %q", c2.Site.Name)
	}
}

func pretty(f float64) string {
	return fmt.Sprintf("%g", f)
}

// The path-normalization helpers pulled in with the EV cloud driver PR
// have three separate jobs that can silently conflict: stripLeadingDotDot
// removes "../" prefixes, ResolveDriverPaths joins relative paths against
// baseDir, and UnresolveDriverPaths goes back to config-relative form
// before the YAML hits disk. The interesting failure is the pair —
// Unresolve followed by Resolve must be the identity, including when the
// driver file lives OUTSIDE baseDir (Copilot #11). Without the
// out-of-tree guard, an absolute path like /opt/drivers/foo.lua round-
// trips to "../opt/drivers/foo.lua" → stripLeadingDotDot → "opt/drivers/
// foo.lua" → baseDir-joined to the wrong place.
func TestStripLeadingDotDot(t *testing.T) {
	cases := map[string]string{
		"":                       "",
		"drivers/x.lua":          "drivers/x.lua",
		"../drivers/x.lua":       "drivers/x.lua",
		"../../../drivers/x.lua": "drivers/x.lua",
		"/abs/drivers/x.lua":     "/abs/drivers/x.lua",
		"/etc/../driver/foo.lua": "/etc/../driver/foo.lua", // non-leading "../" preserved
	}
	for in, want := range cases {
		if got := stripLeadingDotDot(in); got != want {
			t.Errorf("stripLeadingDotDot(%q): got %q, want %q", in, got, want)
		}
	}
}

func TestResolveDriverPaths(t *testing.T) {
	baseDir := "/etc/ftw"
	c := &Config{Drivers: []Driver{
		{Name: "rel", Lua: "drivers/a.lua"},
		{Name: "absin", Lua: "/etc/ftw/drivers/b.lua"},
		{Name: "absout", Lua: "/opt/drivers/c.lua"},
		{Name: "escape", Lua: "../../secrets/d.lua"},
		{Name: "empty"},
	}}
	c.ResolveDriverPaths(baseDir)
	want := map[string]string{
		"rel":    "/etc/ftw/drivers/a.lua", // joined with baseDir
		"absin":  "/etc/ftw/drivers/b.lua", // already absolute, untouched
		"absout": "/opt/drivers/c.lua",     // absolute outside baseDir, untouched
		"escape": "/etc/ftw/secrets/d.lua", // leading "../" stripped, then joined
		"empty":  "",
	}
	for _, d := range c.Drivers {
		if d.Lua != want[d.Name] {
			t.Errorf("resolve %s: got %q, want %q", d.Name, d.Lua, want[d.Name])
		}
	}
}

func TestUnresolveDriverPathsRoundtrip(t *testing.T) {
	baseDir := "/etc/ftw"
	original := []Driver{
		{Name: "rel", Lua: "drivers/a.lua"},
		{Name: "absin", Lua: "/etc/ftw/drivers/b.lua"}, // absolute but inside baseDir
		{Name: "absout", Lua: "/opt/drivers/c.lua"},    // absolute outside baseDir — must stay absolute
		{Name: "empty"},
	}
	c := &Config{Drivers: append([]Driver(nil), original...)}
	c.ResolveDriverPaths(baseDir)
	c.UnresolveDriverPaths(baseDir)

	// After Unresolve, relative / in-tree absolute paths collapse back
	// to baseDir-relative; out-of-tree absolutes must stay absolute so
	// the next Resolve doesn't strip a "../" from filepath.Rel and
	// silently re-anchor the driver under baseDir (Copilot #11).
	got := map[string]string{}
	for _, d := range c.Drivers {
		got[d.Name] = d.Lua
	}
	if got["rel"] != "drivers/a.lua" {
		t.Errorf("rel: got %q, want drivers/a.lua", got["rel"])
	}
	if got["absin"] != "drivers/b.lua" {
		t.Errorf("absin: got %q, want drivers/b.lua", got["absin"])
	}
	if got["absout"] != "/opt/drivers/c.lua" {
		t.Errorf("absout: got %q, want /opt/drivers/c.lua (must remain absolute)", got["absout"])
	}
	if got["empty"] != "" {
		t.Errorf("empty: got %q, want empty string", got["empty"])
	}

	// Re-resolving must produce the same absolute paths as the first
	// Resolve — the UI save/load cycle relies on this being a fixed point.
	c.ResolveDriverPaths(baseDir)
	want := map[string]string{
		"rel":    "/etc/ftw/drivers/a.lua",
		"absin":  "/etc/ftw/drivers/b.lua",
		"absout": "/opt/drivers/c.lua",
		"empty":  "",
	}
	for _, d := range c.Drivers {
		if d.Lua != want[d.Name] {
			t.Errorf("re-resolve %s: got %q, want %q", d.Name, d.Lua, want[d.Name])
		}
	}
}
