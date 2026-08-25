package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
)

func TestDriverSecretKeysIncludePortableDriverPathAlias(t *testing.T) {
	driverDir := filepath.Join(t.TempDir(), "custom-driver-dir")
	if err := os.MkdirAll(driverDir, 0755); err != nil {
		t.Fatal(err)
	}
	luaPath := filepath.Join(driverDir, "sonnen.lua")
	if err := os.WriteFile(luaPath, []byte(`
DRIVER = {
  id = "sonnen",
  name = "sonnen",
  protocols = { "http" },
  config_secrets = { "api_token" },
}
`), 0644); err != nil {
		t.Fatal(err)
	}

	srv := New(&Deps{
		DriverDir:  driverDir,
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
	})
	secrets := srv.driverSecretKeys()
	cfg := &config.Config{Drivers: []config.Driver{{
		Name: "sonnen",
		Lua:  "drivers/sonnen.lua",
		Config: map[string]any{
			"api_token": "secret-token",
		},
	}}}

	maskDriverConfigSecrets(cfg, secrets)

	if got := cfg.Drivers[0].Config["api_token"]; got != maskedPlaceholder {
		t.Fatalf("api_token = %q, want masked placeholder", got)
	}

	incoming := &config.Config{Drivers: []config.Driver{{
		Name: "sonnen",
		Lua:  "drivers/sonnen.lua",
		Config: map[string]any{
			"api_token": maskedPlaceholder,
		},
	}}}
	existing := &config.Config{Drivers: []config.Driver{{
		Name: "sonnen",
		Config: map[string]any{
			"api_token": "secret-token",
		},
	}}}
	restoreDriverConfigSecrets(incoming, existing, secrets)
	if got := incoming.Drivers[0].Config["api_token"]; got != "secret-token" {
		t.Fatalf("restored api_token = %q, want original secret", got)
	}
}

func TestMaskDriverConfigSecretsFailsClosedWhenCatalogMissing(t *testing.T) {
	cfg := &config.Config{Drivers: []config.Driver{{
		Name: "sonnen",
		Lua:  "drivers/sonnen.lua",
		Config: map[string]any{
			"api_token": "secret-token",
			"host":      "192.168.1.10",
		},
	}}}
	maskDriverConfigSecrets(cfg, nil)
	if got := cfg.Drivers[0].Config["api_token"]; got != maskedPlaceholder {
		t.Fatalf("api_token = %q, want masked when catalog is unreadable", got)
	}
	if got := cfg.Drivers[0].Config["host"]; got != maskedPlaceholder {
		t.Fatalf("host = %q, want masked when catalog is unreadable", got)
	}

	incoming := &config.Config{Drivers: []config.Driver{{
		Name: "sonnen",
		Config: map[string]any{
			"api_token": maskedPlaceholder,
			"host":      maskedPlaceholder,
		},
	}}}
	existing := &config.Config{Drivers: []config.Driver{{
		Name: "sonnen",
		Config: map[string]any{
			"api_token": "secret-token",
			"host":      "192.168.1.10",
		},
	}}}
	restoreDriverConfigSecrets(incoming, existing, nil)
	if got := incoming.Drivers[0].Config["api_token"]; got != "secret-token" {
		t.Fatalf("restored api_token = %q", got)
	}
	if got := incoming.Drivers[0].Config["host"]; got != "192.168.1.10" {
		t.Fatalf("restored host = %q", got)
	}
}

func TestMaskDriverConfigSecretsFailsClosedWhenCatalogSkipsDriver(t *testing.T) {
	cfg := &config.Config{Drivers: []config.Driver{{
		Name: "sonnen",
		Lua:  "drivers/sonnen.lua",
		Config: map[string]any{
			"api_token": "secret-token",
		},
	}}}
	// Another driver loaded; this lua file was skipped.
	maskDriverConfigSecrets(cfg, map[string][]string{"drivers/other.lua": {"api_token"}})
	if got := cfg.Drivers[0].Config["api_token"]; got != maskedPlaceholder {
		t.Fatalf("api_token = %q, want masked when the catalog skipped this driver", got)
	}

	emptyCatalog := &config.Config{Drivers: []config.Driver{{
		Name:   "sonnen",
		Lua:    "drivers/sonnen.lua",
		Config: map[string]any{"api_token": "secret-token"},
	}}}
	maskDriverConfigSecrets(emptyCatalog, map[string][]string{})
	if got := emptyCatalog.Drivers[0].Config["api_token"]; got != maskedPlaceholder {
		t.Fatalf("api_token = %q, want masked when the catalog loaded nothing", got)
	}
}

func TestRejectUnsafeProbeHost(t *testing.T) {
	if err := rejectUnsafeProbeHost("127.0.0.1"); err == nil {
		t.Fatal("loopback should be refused")
	}
	if err := rejectUnsafeProbeHost("::1"); err == nil {
		t.Fatal("IPv6 loopback should be refused")
	}
	if err := rejectUnsafeProbeHost("[::1]"); err == nil {
		t.Fatal("bracketed IPv6 loopback should be refused")
	}
	if err := rejectUnsafeProbeHost("localhost"); err == nil {
		t.Fatal("localhost should be refused")
	}
	if err := rejectUnsafeProbeHost("169.254.1.1"); err == nil {
		t.Fatal("link-local should be refused")
	}
	if err := rejectUnsafeProbeHost("fe80::1"); err == nil {
		t.Fatal("IPv6 link-local should be refused")
	}
	if err := rejectUnsafeProbeHost("0.0.0.0"); err == nil {
		t.Fatal("unspecified should be refused")
	}
	if err := rejectUnsafeProbeHost("192.168.1.10"); err != nil {
		t.Fatalf("private IP refused: %v", err)
	}
	if err := rejectUnsafeProbeHost("zap.local"); err != nil {
		t.Fatalf("hostname refused: %v", err)
	}
}

func TestRejectUnsafeProbeTargetsCoversHTTP(t *testing.T) {
	cfg := config.Driver{
		Config: map[string]any{"host": "127.0.0.1"},
		Capabilities: config.Capabilities{
			HTTP: &config.HTTPCapability{AllowedHosts: []string{"127.0.0.1"}},
		},
	}
	if err := rejectUnsafeProbeTargets(cfg); err == nil {
		t.Fatal("HTTP loopback probe should be refused")
	}
	cfg.Config["host"] = "192.168.1.10"
	cfg.Capabilities.HTTP.AllowedHosts = []string{"inverter.local"}
	if err := rejectUnsafeProbeTargets(cfg); err != nil {
		t.Fatalf("LAN HTTP probe refused: %v", err)
	}
}
