package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestFleetPingDefaultsOnForABoxThatNeverHeardOfIt(t *testing.T) {
	// Every existing box has no fleet_ping section. Absent means enabled —
	// the owner's call.
	cfg, err := Parse([]byte(minimalYAML), t.TempDir())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.FleetPing == nil || !cfg.FleetPing.Enabled {
		t.Fatalf("fleet ping = %+v, want enabled", cfg.FleetPing)
	}
	if cfg.FleetPing.Endpoint != "" {
		t.Errorf("endpoint = %q, want it left blank", cfg.FleetPing.Endpoint)
	}
	if got := cfg.FleetPing.Resolved(); got != DefaultFleetPingEndpoint {
		t.Errorf("Resolved() = %q, want %q", got, DefaultFleetPingEndpoint)
	}
}

// The collector's address must never be written into a household's config.
// A box that saved its settings once would keep the old address after
// Sourceful moved the collector, and the aggregate would quietly lose it.
func TestTheCollectorAddressIsNeverBakedIntoAHouseholdsConfig(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML), t.TempDir())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), DefaultFleetPingEndpoint) {
		t.Fatalf("the default endpoint was written into the saved config:\n%s", out)
	}
	// An endpoint somebody chose is still theirs, and is written.
	chosen, err := Parse([]byte(minimalYAML+"fleet_ping:\n  enabled: true\n  endpoint: https://elsewhere.example.test/v1/fleet\n"), t.TempDir())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err = yaml.Marshal(chosen)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), "elsewhere.example.test") {
		t.Fatalf("a configured endpoint was dropped:\n%s", out)
	}
}

func TestFleetPingOptOutSurvivesDefaults(t *testing.T) {
	// The one thing defaulting must never do is switch a household's opt-out
	// back on.
	cfg, err := Parse([]byte(minimalYAML+"fleet_ping:\n  enabled: false\n"), t.TempDir())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.FleetPing.Enabled {
		t.Fatal("an explicit enabled: false was overwritten by the default")
	}
	if cfg.FleetPing.On() {
		t.Fatal("On() read an explicit opt-out as on")
	}
}

// The nil reading has to match applyDefaults. A config posted to /api/config
// never passes through applyDefaults, so a second reading of nil somewhere
// else is a fleet ping that stops the first time a household saves settings.
func TestAnAbsentSectionReadsTheSameWayEverywhere(t *testing.T) {
	var absent *FleetPing
	if !absent.On() {
		t.Error("a nil section read as off; applyDefaults reads it as on")
	}
	if got := absent.Resolved(); got != DefaultFleetPingEndpoint {
		t.Errorf("a nil section resolved to %q, want the default", got)
	}
	defaulted := &Config{}
	applyDefaults(defaulted)
	if defaulted.FleetPing.On() != absent.On() {
		t.Error("applyDefaults and a nil section disagree about the switch")
	}
	if defaulted.FleetPing.Resolved() != absent.Resolved() {
		t.Error("applyDefaults and a nil section disagree about the endpoint")
	}
}

func TestFleetPingEndpointMustBePlainHTTPS(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		ok       bool
	}{
		{DefaultFleetPingEndpoint, true},
		{"https://telemetry.example.test/v1/fleet", true},
		{"", true}, // filled in by applyDefaults
		{"http://telemetry.sourceful.energy/v1/fleet", false},
		{"https://telemetry.sourceful.energy/v1/fleet?box=abc123", false},
		{"https://box:secret@telemetry.sourceful.energy/v1/fleet", false},
		{"https://telemetry.sourceful.energy/v1/fleet#abc123", false},
		{"telemetry.sourceful.energy", false},
	} {
		err := (&FleetPing{Endpoint: tc.endpoint}).Validate()
		if tc.ok && err != nil {
			t.Errorf("%q rejected: %v", tc.endpoint, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%q accepted; it is either readable in transit or a place to hide an identifier", tc.endpoint)
		}
	}
}

func TestFleetPingSwitchNeedsNoRestartButTheEndpointDoes(t *testing.T) {
	// The switch is read at each send, so flipping it in Settings must not
	// raise a restart prompt. The endpoint is resolved once at startup.
	on := &Config{FleetPing: &FleetPing{Enabled: true, Endpoint: DefaultFleetPingEndpoint}}
	off := &Config{FleetPing: &FleetPing{Enabled: false, Endpoint: DefaultFleetPingEndpoint}}
	if reasons := RestartRequiredFor(on, off); len(reasons) != 0 {
		t.Errorf("turning the ping off asked for a restart: %v", reasons)
	}

	moved := &Config{FleetPing: &FleetPing{Enabled: true, Endpoint: "https://elsewhere.example.test/v1/fleet"}}
	reasons := RestartRequiredFor(on, moved)
	if len(reasons) != 1 || !strings.Contains(reasons[0], "fleet_ping.endpoint") {
		t.Errorf("moving the endpoint gave %v, want one fleet_ping.endpoint reason", reasons)
	}
}
