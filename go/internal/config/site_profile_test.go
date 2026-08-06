package config

import (
	"strings"
	"testing"
)

func siteBase() *Config {
	return &Config{
		Site: Site{SmoothingAlpha: 0.3},
		Fuse: Fuse{MaxAmps: 63, Phases: 3, Voltage: 230},
	}
}

func TestValidateRejectsUnknownProfile(t *testing.T) {
	c := siteBase()
	c.Site.Profile = "industrial"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "site.profile") {
		t.Errorf("expected site.profile error, got %v", err)
	}
}

func TestValidateAcceptsKnownProfiles(t *testing.T) {
	for _, p := range []string{"", "residential", "commercial"} {
		c := siteBase()
		c.Site.Profile = p
		if err := c.Validate(); err != nil {
			t.Errorf("profile %q: unexpected error: %v", p, err)
		}
	}
}

func TestMaxCommandWAboveFiveKWRequiresCommercial(t *testing.T) {
	c := siteBase()
	c.Site.MaxCommandW = 25000
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "max_command_w") {
		t.Errorf("expected max_command_w error without commercial profile, got %v", err)
	}
	c.Site.Profile = "commercial"
	if err := c.Validate(); err != nil {
		t.Errorf("commercial profile should allow 25 kW cap: %v", err)
	}
}

func TestMaxCommandWWithinHomeRangeNeedsNoProfile(t *testing.T) {
	c := siteBase()
	c.Site.MaxCommandW = 4000
	if err := c.Validate(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestMaxCommandWNegativeRejected(t *testing.T) {
	c := siteBase()
	c.Site.MaxCommandW = -1
	if err := c.Validate(); err == nil {
		t.Error("expected error for negative max_command_w")
	}
}
