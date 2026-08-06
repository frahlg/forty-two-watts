package tariff

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

// megaflexish returns a two-season Megaflex-style config: high season
// Jun-Aug with morning+evening weekday peaks, low season otherwise.
func megaflexish() *config.Tariff {
	weekday := []config.TariffBand{
		{Band: "peak", Hours: []string{"06:00-09:00", "17:00-19:00"}, RateCtKWh: 650},
		{Band: "standard", Hours: []string{"09:00-17:00", "19:00-22:00"}, RateCtKWh: 200},
		{Band: "offpeak", Hours: []string{"22:00-06:00"}, RateCtKWh: 120},
	}
	saturday := []config.TariffBand{
		{Band: "standard", Hours: []string{"07:00-12:00", "18:00-20:00"}, RateCtKWh: 180},
		{Band: "offpeak", Hours: []string{"20:00-07:00", "12:00-18:00"}, RateCtKWh: 110},
	}
	sunday := []config.TariffBand{
		{Band: "offpeak", Hours: []string{"00:00-24:00"}, RateCtKWh: 100},
	}
	lowWeekday := []config.TariffBand{
		{Band: "peak", Hours: []string{"07:00-10:00", "18:00-20:00"}, RateCtKWh: 320},
		{Band: "standard", Hours: []string{"06:00-07:00", "10:00-18:00", "20:00-22:00"}, RateCtKWh: 150},
		{Band: "offpeak", Hours: []string{"22:00-06:00"}, RateCtKWh: 95},
	}
	return &config.Tariff{
		Timezone:              "Africa/Johannesburg",
		BillingCycleAnchorDay: 1,
		DemandChargeCtKVA:     35000,
		DemandWindowBands:     []string{"peak", "standard"},
		DemandIntegrationMin:  30,
		Holidays:              []string{"2026-12-16"},
		Seasons: []config.TariffSeason{
			{
				Name:   "high",
				Months: []int{6, 7, 8},
				Bands: map[string][]config.TariffBand{
					"weekday": weekday, "saturday": saturday, "sunday": sunday,
				},
			},
			{
				Name:   "low",
				Months: []int{1, 2, 3, 4, 5, 9, 10, 11, 12},
				Bands: map[string][]config.TariffBand{
					"weekday": lowWeekday, "saturday": saturday, "sunday": sunday,
				},
			},
		},
	}
}

func mustCompile(t *testing.T) *Schedule {
	t.Helper()
	s, err := Compile(megaflexish())
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return s
}

func at(t *testing.T, s *Schedule, value string) Resolved {
	t.Helper()
	ts, err := time.ParseInLocation("2006-01-02 15:04", value, s.Location)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Resolve(ts)
	if err != nil {
		t.Fatalf("resolve %s: %v", value, err)
	}
	return r
}

func TestResolveBands(t *testing.T) {
	s := mustCompile(t)
	cases := []struct {
		when   string
		season string
		band   Band
		rate   float64
		demand bool
	}{
		// Wed 2026-07-01 is high season.
		{"2026-07-01 07:30", "high", BandPeak, 650, true},
		{"2026-07-01 13:00", "high", BandStandard, 200, true},
		{"2026-07-01 23:00", "high", BandOffPeak, 120, false},
		{"2026-07-01 05:59", "high", BandOffPeak, 120, false}, // midnight wrap tail
		// Sat 2026-07-04.
		{"2026-07-04 08:00", "high", BandStandard, 180, true},
		{"2026-07-04 13:00", "high", BandOffPeak, 110, false},
		// Sun 2026-07-05.
		{"2026-07-05 08:00", "high", BandOffPeak, 100, false},
		// Low season Wed 2026-05-06.
		{"2026-05-06 08:00", "low", BandPeak, 320, true},
		// Season boundary: Aug 31 is high, Sep 1 is low.
		{"2026-08-31 08:00", "high", BandPeak, 650, true},
		{"2026-09-01 08:00", "low", BandPeak, 320, true},
		// Day of Reconciliation (Wed) prices as sunday.
		{"2026-12-16 08:00", "low", BandOffPeak, 100, false},
	}
	for _, c := range cases {
		r := at(t, s, c.when)
		if r.Season != c.season || r.Band != c.band || r.RateCtKWh != c.rate || r.DemandActive != c.demand {
			t.Errorf("%s: got %+v, want season=%s band=%s rate=%v demand=%v",
				c.when, r, c.season, c.band, c.rate, c.demand)
		}
	}
}

func TestSlotPricesWeightedAverageAndDemandFlag(t *testing.T) {
	s := mustCompile(t)
	// 08:30-09:30 on a high-season weekday straddles peak (650, 30 min)
	// and standard (200, 30 min): average 425, demand-active.
	from, _ := time.ParseInLocation("2006-01-02 15:04", "2026-07-01 08:30", s.Location)
	slots, err := s.SlotPrices(from, from.Add(time.Hour), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 1 {
		t.Fatalf("got %d slots, want 1", len(slots))
	}
	if got := slots[0].RateCtKWh; got != 425 {
		t.Errorf("straddling slot rate = %v, want 425", got)
	}
	if !slots[0].DemandActive {
		t.Error("straddling slot should be demand-active")
	}
}

func TestSlotPricesCoverHorizon(t *testing.T) {
	s := mustCompile(t)
	from, _ := time.ParseInLocation("2006-01-02 15:04", "2026-08-31 00:00", s.Location)
	slots, err := s.SlotPrices(from, from.AddDate(0, 0, 2), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 48 {
		t.Fatalf("got %d slots, want 48", len(slots))
	}
	// Slot 32 (Sep 1, 08:00) has crossed the season boundary.
	if got := slots[32].RateCtKWh; got != 320 {
		t.Errorf("slot 32 rate = %v, want low-season peak 320", got)
	}
}

func TestBillingCycleStart(t *testing.T) {
	s := mustCompile(t)
	mid, _ := time.ParseInLocation("2006-01-02 15:04", "2026-07-15 12:00", s.Location)
	start := s.BillingCycleStart(mid)
	if start.Day() != 1 || start.Month() != time.July {
		t.Errorf("cycle start = %v, want July 1", start)
	}
	first, _ := time.ParseInLocation("2006-01-02 15:04", "2026-07-01 00:00", s.Location)
	if got := s.BillingCycleStart(first); !got.Equal(first) {
		t.Errorf("anchor instant should start its own cycle, got %v", got)
	}
}

func TestCompileRejectsGaps(t *testing.T) {
	c := megaflexish()
	// Remove offpeak from high-season weekdays → 22:00-06:00 uncovered.
	c.Seasons[0].Bands["weekday"] = c.Seasons[0].Bands["weekday"][:2]
	if _, err := Compile(c); err == nil {
		t.Fatal("expected coverage-gap error")
	}
}

func TestCompileRejectsOverlap(t *testing.T) {
	c := megaflexish()
	c.Seasons[0].Bands["weekday"] = append(c.Seasons[0].Bands["weekday"],
		config.TariffBand{Band: "standard", Hours: []string{"08:00-10:00"}, RateCtKWh: 1})
	if _, err := Compile(c); err == nil {
		t.Fatal("expected overlap error")
	}
}

func TestCompileRejectsMonthDoubleCover(t *testing.T) {
	c := megaflexish()
	c.Seasons[1].Months = append(c.Seasons[1].Months, 6)
	if _, err := Compile(c); err == nil {
		t.Fatal("expected month double-cover error")
	}
}

func TestConfigValidateAcceptsExample(t *testing.T) {
	cfg := &config.Config{
		Site:   config.Site{SmoothingAlpha: 0.3, Profile: "commercial"},
		Fuse:   config.Fuse{MaxAmps: 400, Phases: 3, Voltage: 230},
		Tariff: megaflexish(),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}
