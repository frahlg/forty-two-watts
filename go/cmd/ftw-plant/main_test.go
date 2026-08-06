package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "plant.yaml")
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValidConfig(t *testing.T) {
	fc, err := load(writeCfg(t, `
listen: 127.0.0.1:9300
poll_interval_ms: 500
units:
  - {id: rack-1, host: 10.0.0.5, port: 502, unit_id: 1}
  - {id: rack-2, host: 10.0.0.5, port: 502, unit_id: 2}
`))
	if err != nil {
		t.Fatal(err)
	}
	if fc.Listen != "127.0.0.1:9300" || len(fc.Units) != 2 || fc.Units[1].UnitID != 2 {
		t.Fatalf("config: %+v", fc)
	}
}

func TestLoadRejectsBadConfigs(t *testing.T) {
	cases := map[string]string{
		"no units":      `listen: 127.0.0.1:9200`,
		"missing host":  `units: [{id: r1, port: 502, unit_id: 1}]`,
		"duplicate ids": "units:\n  - {id: r1, host: h, port: 502, unit_id: 1}\n  - {id: r1, host: h, port: 502, unit_id: 2}",
	}
	for name, body := range cases {
		if _, err := load(writeCfg(t, body)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestLoadDefaultsListen(t *testing.T) {
	fc, err := load(writeCfg(t, `units: [{id: r1, host: h, port: 502, unit_id: 1}]`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fc.Listen, "127.0.0.1:") {
		t.Fatalf("default listen should be loopback: %q", fc.Listen)
	}
}
