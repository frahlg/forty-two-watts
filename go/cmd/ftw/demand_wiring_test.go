package main

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/tariff"
)

func testTariffConfig() *config.Tariff {
	bands := map[string][]config.TariffBand{
		"weekday": {
			{Band: "peak", Hours: []string{"06:00-09:00", "17:00-19:00"}, RateCtKWh: 650},
			{Band: "standard", Hours: []string{"09:00-17:00", "19:00-22:00"}, RateCtKWh: 200},
			{Band: "offpeak", Hours: []string{"22:00-06:00"}, RateCtKWh: 120},
		},
		"saturday": {{Band: "offpeak", Hours: []string{"00:00-24:00"}, RateCtKWh: 110}},
		"sunday":   {{Band: "offpeak", Hours: []string{"00:00-24:00"}, RateCtKWh: 100}},
	}
	return &config.Tariff{
		Timezone:              "Africa/Johannesburg",
		BillingCycleAnchorDay: 1,
		DemandChargeCtKVA:     35000,
		DemandWindowBands:     []string{"peak", "standard"},
		DemandIntegrationMin:  30,
		Seasons: []config.TariffSeason{{
			Name: "all", Months: []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
			Bands: bands,
		}},
	}
}

func TestTariffPriceSourceRendersHorizon(t *testing.T) {
	sched, err := tariff.Compile(testTariffConfig())
	if err != nil {
		t.Fatal(err)
	}
	src := tariffPriceSource(sched, "ZA")

	// Wed 2026-07-01 05:30 SAST, 48 h horizon.
	loc := sched.Location
	from := time.Date(2026, 7, 1, 5, 30, 0, 0, loc)
	until := from.Add(48 * time.Hour)
	prices := src(from.UnixMilli(), until.UnixMilli())

	if len(prices) < 48 {
		t.Fatalf("got %d price rows, want ≥48", len(prices))
	}
	first := prices[0]
	if got := time.UnixMilli(first.SlotTsMs).In(loc); got.Minute() != 0 {
		t.Errorf("first slot not hour-aligned: %v", got)
	}
	for _, pr := range prices {
		if pr.Source != "tariff" || pr.SlotLenMin != 60 || pr.SpotOreKwh != 0 {
			t.Fatalf("bad row: %+v", pr)
		}
	}
	// 07:00 Wed is peak (650); 13:00 is standard (200); 23:00 offpeak (120).
	byHour := map[int]float64{}
	for _, pr := range prices[:24] {
		byHour[time.UnixMilli(pr.SlotTsMs).In(loc).Hour()] = pr.TotalOreKwh
	}
	if byHour[7] != 650 || byHour[13] != 200 || byHour[23] != 120 {
		t.Errorf("rates: 07=%v 13=%v 23=%v, want 650/200/120", byHour[7], byHour[13], byHour[23])
	}
}
