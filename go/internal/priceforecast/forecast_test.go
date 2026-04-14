package priceforecast

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
)

func TestFreshModelReturnsNeutralPrior(t *testing.T) {
	m := NewZoneModel("SE3")
	// Arbitrary time — fresh model has all buckets at 80, month modifier 1.
	t0 := time.Date(2026, 6, 15, 14, 0, 0, 0, time.UTC)
	got := m.Predict(t0)
	if got != 80 {
		t.Errorf("fresh model should return 80, got %f", got)
	}
}

func TestFitsHourOfWeekPattern(t *testing.T) {
	// Synthetic: SE3 prices with morning peak 150, midday trough 30,
	// evening peak 200. 6 weeks of data.
	var pts []state.PricePoint
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC) // Monday
	for d := 0; d < 42; d++ {
		for h := 0; h < 24; h++ {
			ts := start.Add(time.Duration(d*24+h) * time.Hour)
			var price float64
			switch {
			case h >= 7 && h <= 9:
				price = 150
			case h >= 11 && h <= 14:
				price = 30
			case h >= 17 && h <= 20:
				price = 200
			default:
				price = 80
			}
			pts = append(pts, state.PricePoint{
				Zone:       "SE3",
				SlotTsMs:   ts.UnixMilli(),
				SlotLenMin: 60,
				SpotOreKwh: price,
			})
		}
	}
	m := NewZoneModel("SE3")
	m.FitFromHistory(pts)

	// Test: Monday 08:00 → morning peak
	mornMon := time.Date(2026, 3, 2, 8, 0, 0, 0, time.UTC)
	if got := m.Predict(mornMon); math.Abs(got-150) > 5 {
		t.Errorf("Mon 08:00 peak: got %f, want ~150", got)
	}
	// Wed 13:00 → trough
	trough := time.Date(2026, 3, 4, 13, 0, 0, 0, time.UTC)
	if got := m.Predict(trough); math.Abs(got-30) > 5 {
		t.Errorf("Wed 13:00 trough: got %f, want ~30", got)
	}
	// Fri 19:00 → evening peak
	eve := time.Date(2026, 3, 6, 19, 0, 0, 0, time.UTC)
	if got := m.Predict(eve); math.Abs(got-200) > 5 {
		t.Errorf("Fri 19:00 peak: got %f, want ~200", got)
	}
}

func TestSeedFromCSVIngestsAndFits(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	csv := `zone,slot_ts_ms,slot_len_min,spot_ore_kwh
SE3,1735689600000,60,50.0
SE3,1735693200000,60,60.0
SE3,1735696800000,60,70.0
SE3,1735700400000,60,80.0
SE4,1735689600000,60,90.0
`
	// Write to a tempfile so SeedFromCSV sees a real path.
	s := NewService(st, []string{"SE3", "SE4"})
	n, err := s.ingestCSV(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if n != 5 {
		t.Errorf("want 5 rows imported, got %d", n)
	}
	// Verify SE3 data landed in the store.
	rows, err := st.LoadPrices("SE3", 0, 3000000000000)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Errorf("SE3 rows: got %d want 4", len(rows))
	}
}

func TestSeedFromCSVRejectsMissingColumns(t *testing.T) {
	st, _ := state.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	s := NewService(st, []string{"SE3"})
	_, err := s.ingestCSV(strings.NewReader("zone,timestamp\nSE3,1000\n"))
	if err == nil {
		t.Error("expected error for missing spot_ore_kwh column")
	}
}
