package drivers

import (
	"testing"
)

// Verification status is what lets the UI distinguish "production-ready"
// drivers from "ported but unproven" ones. This test parses the real
// drivers/ dir and asserts the expected status labels for each driver
// we've manually annotated. Every other driver in the tree is expected
// to parse as "experimental" (the normalized default for missing /
// unknown values).
func TestCatalogVerificationStatus(t *testing.T) {
	entries, err := LoadCatalog("../../../drivers")
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	byID := make(map[string]CatalogEntry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}

	cases := []struct {
		id     string
		status string
	}{
		{"ferroamp", "production"},
		{"sungrow-shx", "production"},
		{"easee-cloud", "production"},
		{"ferroamp-modbus", "experimental"},
		{"sourceful-zap", "beta"},
		{"deye", "experimental"},
		{"solis", "experimental"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			e, ok := byID[tc.id]
			if !ok {
				t.Fatalf("driver %q missing from catalog (got %d entries)", tc.id, len(entries))
			}
			if e.VerificationStatus != tc.status {
				t.Errorf("%s: VerificationStatus=%q, want %q", tc.id, e.VerificationStatus, tc.status)
			}
		})
	}
}

// Drivers at production status must also have a non-empty VerifiedBy
// list — otherwise the label is hearsay. Beta is fuzzier; experimental
// needs nothing. This check runs against the real catalog so
// adding a new "production" annotation without also filling in
// VerifiedBy fails loud at CI.
func TestCatalogProductionDriversHaveVerifier(t *testing.T) {
	entries, err := LoadCatalog("../../../drivers")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.VerificationStatus != "production" {
			continue
		}
		if len(e.VerifiedBy) == 0 {
			t.Errorf("%s (%s): marked production but no VerifiedBy entries — who tested it?",
				e.ID, e.Filename)
		}
		if e.VerifiedAt == "" {
			t.Errorf("%s (%s): marked production but no VerifiedAt date", e.ID, e.Filename)
		}
	}
}

// Unknown / garbage values in the Lua file must normalize to
// "experimental" rather than propagate an invalid label to the UI.
func TestNormalizeVerificationStatus(t *testing.T) {
	cases := map[string]string{
		"production":     "production",
		"PRODUCTION":     "production",
		"Beta":           "beta",
		"experimental":   "experimental",
		"":               "experimental",
		"  ":             "experimental",
		"prod":           "experimental", // typo → safest default
		"alpha":          "experimental", // non-canonical → safest default
	}
	for in, want := range cases {
		if got := normalizeVerificationStatus(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}
