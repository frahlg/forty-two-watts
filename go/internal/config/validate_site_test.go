package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadSiteValidationConfig(t *testing.T, yaml string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	return err
}

func TestLoadRejectsMoreThanThreeFusePhases(t *testing.T) {
	err := loadSiteValidationConfig(t, strings.Replace(minimalYAML, "max_amps: 16", "max_amps: 16\n  phases: 4", 1))
	if err == nil || err.Error() != "fuse.phases must be 1, 2 or 3" {
		t.Errorf("Load error = %v, want fuse phase validation error", err)
	}
}

func TestValidateAcceptsOneToThreePhases(t *testing.T) {
	for phases := 1; phases <= 3; phases++ {
		c := &Config{
			Site: Site{SmoothingAlpha: 0.3},
			Fuse: Fuse{MaxAmps: 16, Phases: phases, Voltage: 230},
		}
		if err := c.Validate(); err != nil {
			t.Errorf("phases=%d: unexpected error: %v", phases, err)
		}
	}
}

func meterDriver(name string, siteMeter bool) Driver {
	return Driver{
		Name:        name,
		Lua:         "drivers/test.lua",
		IsSiteMeter: siteMeter,
		Capabilities: Capabilities{
			Modbus: &ModbusConfig{Host: "192.168.1.10", Port: 502},
		},
	}
}

func TestLoadRejectsDuplicateSiteMeters(t *testing.T) {
	yaml := strings.Replace(minimalYAML, "api:\n", `
  - name: second-meter
    lua: drivers/second-meter.lua
    is_site_meter: true
    capabilities:
      mqtt:
        host: 192.168.1.154
api:
`, 1)
	err := loadSiteValidationConfig(t, yaml)
	if err == nil || err.Error() != "exactly one driver may set is_site_meter: true (found 2)" {
		t.Errorf("Load error = %v, want duplicate site meter validation error", err)
	}
}

func TestValidateAcceptsSingleSiteMeter(t *testing.T) {
	c := &Config{
		Site:    Site{SmoothingAlpha: 0.3},
		Fuse:    Fuse{MaxAmps: 16, Phases: 3, Voltage: 230},
		Drivers: []Driver{meterDriver("a", true), meterDriver("b", false)},
	}
	if err := c.Validate(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
