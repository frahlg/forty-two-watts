package forecast

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Stub server returns a canned Open-Meteo response. Verifies that
// shortwave_radiation, cloud_cover, temperature_2m all land on the
// right RawForecast fields and that timestamps are parsed as UTC.
func TestOpenMeteo_ParsesRadiationAndCloud(t *testing.T) {
	body := `{
		"hourly": {
			"time": ["2026-04-17T12:00","2026-04-17T13:00","2026-04-17T14:00"],
			"shortwave_radiation": [712.5, 685.1, 420.0],
			"cloud_cover": [10.0, 20.0, 45.5],
			"temperature_2m": [18.2, 17.8, 15.9]
		}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "shortwave_radiation") {
			t.Errorf("expected query param shortwave_radiation, got %q", r.URL.RawQuery)
		}
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	om := &OpenMeteoProvider{Client: srv.Client(), BaseURL: srv.URL}
	rows, err := om.Fetch(context.Background(), 56.7, 16.3)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	if rows[0].SolarWm2 == nil || *rows[0].SolarWm2 != 712.5 {
		t.Errorf("SolarWm2 at 12:00 = %v, want 712.5", rows[0].SolarWm2)
	}
	if rows[0].CloudCoverPct == nil || *rows[0].CloudCoverPct != 10.0 {
		t.Errorf("CloudCoverPct = %v, want 10", rows[0].CloudCoverPct)
	}
	if rows[2].TempC == nil || *rows[2].TempC != 15.9 {
		t.Errorf("TempC[2] = %v, want 15.9", rows[2].TempC)
	}
	// PVWEstimated must be nil — this provider only emits radiation;
	// fetchAndStore derives PV from that.
	if rows[0].PVWEstimated != nil {
		t.Errorf("PVWEstimated should be nil for open-meteo, got %v", rows[0].PVWEstimated)
	}
	// Timestamps parsed as UTC.
	if rows[0].HourStart.Location().String() != "UTC" {
		t.Errorf("HourStart.Location = %s, want UTC", rows[0].HourStart.Location())
	}
}

// A row with a missing shortwave_radiation entry (null in the JSON)
// should be surfaced as a nil *float64, not silently zero.
func TestOpenMeteo_HandlesNullFields(t *testing.T) {
	body := `{
		"hourly": {
			"time": ["2026-04-17T12:00"],
			"shortwave_radiation": [null],
			"cloud_cover": [50.0],
			"temperature_2m": [10.0]
		}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	om := &OpenMeteoProvider{Client: srv.Client(), BaseURL: srv.URL}
	rows, err := om.Fetch(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	if rows[0].SolarWm2 != nil {
		t.Errorf("null shortwave should be nil, got %v", *rows[0].SolarWm2)
	}
	if rows[0].CloudCoverPct == nil || *rows[0].CloudCoverPct != 50 {
		t.Errorf("CloudCoverPct = %v, want 50", rows[0].CloudCoverPct)
	}
}

// Non-200 responses are surfaced as errors — no silent fallback to empty
// rows that would starve downstream consumers without logging.
func TestOpenMeteo_NonOKError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "blocked", http.StatusForbidden)
	}))
	defer srv.Close()
	om := &OpenMeteoProvider{Client: srv.Client(), BaseURL: srv.URL}
	if _, err := om.Fetch(context.Background(), 0, 0); err == nil {
		t.Error("expected error on 403, got nil")
	}
}
