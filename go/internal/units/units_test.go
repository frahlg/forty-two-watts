package units

import "testing"

func TestPVFromIrradianceSTC(t *testing.T) {
	if got := PVFromIrradiance(18960, 1000); got != 18960 {
		t.Fatalf("STC: got %v, want 18960 W", got)
	}
}

func TestPVFromIrradianceEveningIsKilowattsNotMegawatts(t *testing.T) {
	// Björn 2026-08-18: 18960 W roof, ~108 W/m² evening POA.
	// The old kWp-as-watts path produced 2046 kW. Rated watts × G/1000
	// is ~2 kW.
	got := PVFromIrradiance(18960, 108)
	if got < 1900 || got > 2200 {
		t.Fatalf("evening PV = %.1f W, want ~2048 W (not megawatts)", got)
	}
	if got > 18960 {
		t.Fatalf("evening PV %.1f W exceeds nameplate 18960 W", got)
	}
}

func TestCanonicalPowerEnergy(t *testing.T) {
	w, u := CanonicalPowerEnergy(2.5, "kW")
	if w != 2500 || u != "W" {
		t.Fatalf("2.5 kW → %v %q, want 2500 W", w, u)
	}
	wh, u := CanonicalPowerEnergy(12.5, "kWh")
	if wh != 12500 || u != "Wh" {
		t.Fatalf("12.5 kWh → %v %q, want 12500 Wh", wh, u)
	}
	w, u = CanonicalPowerEnergy(1500, "W")
	if w != 1500 || u != "W" {
		t.Fatalf("already watts: %v %q", w, u)
	}
	wh, u = CanonicalPowerEnergy(5399.9, "Wh")
	if wh != 5399.9 || u != "Wh" {
		t.Fatalf("already watt-hours: %v %q", wh, u)
	}
	c, u := CanonicalPowerEnergy(22.6, "°C")
	if c != 22.6 || u != "°C" {
		t.Fatalf("other units pass through: %v %q", c, u)
	}
}

func TestKWpRoundTrip(t *testing.T) {
	if got := WattsFromKWp(12.96); got != 12960 {
		t.Fatalf("WattsFromKWp(12.96) = %v", got)
	}
	if got := KWpFromWatts(12960); got != 12.96 {
		t.Fatalf("KWpFromWatts(12960) = %v", got)
	}
}

func TestFractionFromLegacyPercent(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{0, 0},
		{0.10, 0.10},
		{1, 1},
		{10, 0.10},
		{90, 0.90},
		{100, 1},
		{-5, 0},
	}
	for _, c := range cases {
		if got := FractionFromLegacyPercent(c.in); got != c.want {
			t.Errorf("FractionFromLegacyPercent(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRatedWattsFromLegacyKWp(t *testing.T) {
	if got := RatedWattsFromLegacyKWp(12.96); got != 12960 {
		t.Fatalf("12.96 kWp → %v W, want 12960", got)
	}
	if got := RatedWattsFromLegacyKWp(18960); got != 18960 {
		t.Fatalf("pasted watts 18960 → %v, want 18960 (not 18.96 MW)", got)
	}
	if got := RatedWattsFromLegacyKWp(6); got != 6000 {
		t.Fatalf("6 kWp → %v W, want 6000", got)
	}
	if got := RatedWattsFromLegacyKWp(0); got != 0 {
		t.Fatalf("empty → %v", got)
	}
}

func TestPermilleDoor(t *testing.T) {
	if got := PermilleFromFraction(0.624); got != 624 {
		t.Fatalf("permille = %d, want 624", got)
	}
	if got := FractionFromPermille(624); got != 0.624 {
		t.Fatalf("fraction = %v, want 0.624", got)
	}
}

func TestDecodeJSONFraction(t *testing.T) {
	if got := DecodeJSONFraction(0.80, 0); got != 0.80 {
		t.Fatalf("canonical 0.80 = %v", got)
	}
	if got := DecodeJSONFraction(0, 80); got != 0.80 {
		t.Fatalf("legacy 80 = %v", got)
	}
	if got := DecodeJSONFraction(0.50, 80); got != 0.50 {
		t.Fatalf("canonical wins = %v", got)
	}
}

func TestValidFraction(t *testing.T) {
	if !ValidFraction(0) || !ValidFraction(1) || !ValidFraction(0.55) {
		t.Fatal("expected 0, 1, 0.55 valid")
	}
	if ValidFraction(1.01) || ValidFraction(-0.01) || ValidFraction(50) {
		t.Fatal("1.01, -0.01, 50 must be invalid")
	}
}
