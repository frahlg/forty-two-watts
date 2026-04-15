package sunpos

import (
	"math"
	"testing"
	"time"
)

// Stockholm noon-ish around summer solstice — sun should be roughly south,
// well above horizon (zenith well below 90° at lat 59).
func TestNoonSummerStockholm(t *testing.T) {
	tt := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC) // ~13:00 local
	p := At(tt, 59.33, 18.07)
	if p.ZenithDeg > 50 || p.ZenithDeg < 30 {
		t.Errorf("noon zenith should be ~36°, got %.1f", p.ZenithDeg)
	}
	if p.AzimuthDeg < 150 || p.AzimuthDeg > 210 {
		t.Errorf("noon azimuth should be near south (180°), got %.1f", p.AzimuthDeg)
	}
}

// Night → zenith ≥ 90.
func TestMidnightBelow(t *testing.T) {
	tt := time.Date(2026, 12, 21, 23, 0, 0, 0, time.UTC) // ~midnight local
	p := At(tt, 59.33, 18.07)
	if p.ZenithDeg < 90 {
		t.Errorf("expected sun below horizon, got zenith %.1f", p.ZenithDeg)
	}
	if cs := ClearSkyW(tt, 59.33, 18.07); cs != 0 {
		t.Errorf("clearsky at midnight should be 0, got %.1f", cs)
	}
}

// AOI: south-facing 30° panel at solar noon → low AOI (sun roughly normal).
func TestAOISouthAtNoon(t *testing.T) {
	tt := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	sun := At(tt, 59.33, 18.07)
	a := AOI(sun, 30, 180) // south-facing 30° tilt
	if a > 30 {
		t.Errorf("AOI should be small near solar noon, got %.1f", a)
	}
}

// East-facing panel: morning AOI < afternoon AOI (sun in east in morning).
func TestAOIEastVsAfternoon(t *testing.T) {
	morning := time.Date(2026, 6, 21, 5, 0, 0, 0, time.UTC) // ~07 local
	evening := time.Date(2026, 6, 21, 17, 0, 0, 0, time.UTC) // ~19 local
	sunM := At(morning, 59.33, 18.07)
	sunE := At(evening, 59.33, 18.07)
	if sunM.ZenithDeg >= 90 || sunE.ZenithDeg >= 90 {
		t.Skip("sun below horizon — test invalid for this date/place")
	}
	aM := AOI(sunM, 30, 90)  // east-facing
	aE := AOI(sunE, 30, 90)
	if aM >= aE {
		t.Errorf("east panel should see lower AOI in morning (%.1f) than evening (%.1f)", aM, aE)
	}
}

// POA on flat ground = clear-sky horizontal irradiance (within rounding).
func TestPOAFlatEqualsGHI(t *testing.T) {
	tt := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	ghi := ClearSkyW(tt, 59.33, 18.07)
	poa := POA(tt, 59.33, 18.07, 0, 180) // tilt 0 = flat
	if math.Abs(ghi-poa) > 1.0 {
		t.Errorf("flat POA should equal GHI: ghi=%.1f poa=%.1f", ghi, poa)
	}
}
