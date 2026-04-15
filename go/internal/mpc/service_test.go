package mpc

import (
	"math"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
)

func TestBuildSlotsFallsBackToForecastWhenTwinCollapses(t *testing.T) {
	ts := time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC).UnixMilli()
	cloud := 48.1
	forecastPV := 1488.5770353837524
	slots := buildSlots(
		[]state.PricePoint{{
			SlotTsMs:    ts,
			SlotLenMin:  15,
			SpotOreKwh:  120,
			TotalOreKwh: 180,
		}},
		[]state.ForecastPoint{{
			SlotTsMs:      ts,
			SlotLenMin:    60,
			CloudCoverPct: &cloud,
			PVWEstimated:  &forecastPV,
		}},
		2500,
		ts,
		func(time.Time, float64) float64 { return 0 },
		nil,
	)
	if len(slots) != 1 {
		t.Fatalf("got %d slots, want 1", len(slots))
	}
	if got := slots[0].PVW; math.Abs(got+forecastPV) > 1e-6 {
		t.Fatalf("slot PVW = %f, want %f", got, -forecastPV)
	}
}

func TestBuildSlotsKeepsTwinWhenPredictionIsSane(t *testing.T) {
	ts := time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC).UnixMilli()
	cloud := 48.1
	forecastPV := 1488.5770353837524
	twinPV := 1180.0
	slots := buildSlots(
		[]state.PricePoint{{
			SlotTsMs:    ts,
			SlotLenMin:  15,
			SpotOreKwh:  120,
			TotalOreKwh: 180,
		}},
		[]state.ForecastPoint{{
			SlotTsMs:      ts,
			SlotLenMin:    60,
			CloudCoverPct: &cloud,
			PVWEstimated:  &forecastPV,
		}},
		2500,
		ts,
		func(time.Time, float64) float64 { return twinPV },
		nil,
	)
	if len(slots) != 1 {
		t.Fatalf("got %d slots, want 1", len(slots))
	}
	if got := slots[0].PVW; math.Abs(got+twinPV) > 1e-6 {
		t.Fatalf("slot PVW = %f, want %f", got, -twinPV)
	}
}
