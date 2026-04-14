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
    wasm: drivers/ferroamp.wasm
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
	want := filepath.Join("/base/dir", "drivers/ferroamp.wasm")
	if c.Drivers[0].WASM != want {
		t.Errorf("wasm path: got %s want %s", c.Drivers[0].WASM, want)
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
    wasm: a.wasm
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
    wasm: a.wasm
    is_site_meter: true
    capabilities: { mqtt: { host: 1.1.1.1 } }
  - name: a
    wasm: b.wasm
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
    wasm: a.wasm
    is_site_meter: true
api: { port: 8080 }
`
	if _, err := Parse([]byte(yaml), "."); err == nil {
		t.Fatal("expected error for driver without protocol")
	}
}

func TestRejectsDriverWithoutWasmOrLua(t *testing.T) {
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
		t.Fatal("expected error for driver without wasm/lua")
	}
}

func TestLegacyMqttFallsBackToCapabilities(t *testing.T) {
	yaml := `
site: { name: x }
fuse: { max_amps: 16 }
drivers:
  - name: a
    wasm: a.wasm
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
    wasm: a.wasm
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
    wasm: f.wasm
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
