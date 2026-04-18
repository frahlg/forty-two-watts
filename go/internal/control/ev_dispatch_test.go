package control

import "testing"

func TestSnapChargeWSnapsToNearest(t *testing.T) {
	steps := []float64{0, 1400, 4100, 7400, 11000}
	cases := []struct {
		want     float64
		expected float64
		note     string
	}{
		{0, 0, "zero → off, short-circuits"},
		{1000, 1400, "below min step snaps up to lowest non-zero"},
		{1300, 1400, "near min → min"},
		{5500, 4100, "midway rounds to closer"},
		{6000, 7400, "midway rounds to closer (upper)"},
		{11000, 11000, "at max step"},
		{15000, 11000, "above max clamps to max"},
	}
	for _, tc := range cases {
		t.Run(tc.note, func(t *testing.T) {
			got := SnapChargeW(tc.want, 1400, 11000, steps)
			if got != tc.expected {
				t.Errorf("SnapChargeW(%f) = %f, want %f", tc.want, got, tc.expected)
			}
		})
	}
}

func TestSnapChargeWNoStepsReturnsClamped(t *testing.T) {
	got := SnapChargeW(5000, 1400, 11000, nil)
	if got != 5000 {
		t.Errorf("want continuous passthrough; got %.0f", got)
	}
	got = SnapChargeW(500, 1400, 11000, nil)
	if got != 1400 {
		t.Errorf("want clamp to min; got %.0f", got)
	}
	got = SnapChargeW(20000, 1400, 11000, nil)
	if got != 11000 {
		t.Errorf("want clamp to max; got %.0f", got)
	}
}

func TestEnergyBudgetToPowerW(t *testing.T) {
	// 1 kWh over 1 hour = 1000 W
	if got := EnergyBudgetToPowerW(1000, 3600); got != 1000 {
		t.Errorf("1 kWh/h should be 1000 W, got %.0f", got)
	}
	// 500 Wh over 15 minutes → 2000 W
	if got := EnergyBudgetToPowerW(500, 900); got != 2000 {
		t.Errorf("500 Wh / 15 min should be 2000 W, got %.0f", got)
	}
	// Negative / zero remaining → 0
	if got := EnergyBudgetToPowerW(-50, 600); got != 0 {
		t.Errorf("negative remaining should stop charging, got %.0f", got)
	}
	if got := EnergyBudgetToPowerW(500, 0); got != 0 {
		t.Errorf("zero remaining time should return 0 (safety), got %.0f", got)
	}
}
