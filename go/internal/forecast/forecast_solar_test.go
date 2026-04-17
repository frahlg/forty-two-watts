package forecast

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Stub server returns a canned Forecast.Solar response. Verifies that
// per-timestamp watts get bucketed to UTC hours and materialised on
// PVWEstimated. We don't assert exact hour-count because the API's
// dawn/dusk sampling is irregular; we assert the peak-hour value is
// preserved and cloud/temp are not fabricated.
func TestForecastSolar_BucketsToHours(t *testing.T) {
	body := `{
		"result": {
			"watts": {
				"2026-04-17 06:53:38": 0.0,
				"2026-04-17 07:00:00": 150.0,
				"2026-04-17 07:30:00": 450.0,
				"2026-04-17 12:00:00": 8200.0,
				"2026-04-17 12:30:00": 8450.0,
				"2026-04-17 13:00:00": 8100.0
			}
		},
		"message": {"code": 0, "type": "success"}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify geometry lands in the URL.
		if !strings.Contains(r.URL.Path, "/estimate/56.7") {
			t.Errorf("missing lat in path: %s", r.URL.Path)
		}
		if !strings.Contains(r.URL.RawQuery, "time=utc") {
			t.Errorf("expected time=utc, got %q", r.URL.RawQuery)
		}
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	fs := &ForecastSolarProvider{
		Client: srv.Client(), BaseURL: srv.URL,
		TiltDeg: 35, AzimuthDeg: 180, KWp: 10,
	}
	rows, err := fs.Fetch(context.Background(), 56.7, 16.3)
	if err != nil {
		t.Fatal(err)
	}
	// Buckets: 06:53:38 → 06; 07:00 + 07:30 → 07; 12:00 + 12:30 → 12; 13:00 → 13.
	if len(rows) != 4 {
		t.Errorf("want 4 hour buckets (06/07/12/13), got %d", len(rows))
	}
	// Find the 12:00 UTC bucket — its max in-hour sample is 8450.
	var noonPV *float64
	for _, r := range rows {
		if r.HourStart.Hour() == 12 {
			noonPV = r.PVWEstimated
		}
	}
	if noonPV == nil || *noonPV != 8450.0 {
		t.Errorf("12:00 bucket PV = %v, want 8450 (max-in-hour)", noonPV)
	}
	// No fabrication of cloud/temp.
	for _, r := range rows {
		if r.CloudCoverPct != nil {
			t.Error("forecast.solar shouldn't emit CloudCoverPct")
		}
		if r.TempC != nil {
			t.Error("forecast.solar shouldn't emit TempC")
		}
	}
	// Sorted by time.
	for i := 1; i < len(rows); i++ {
		if rows[i-1].HourStart.After(rows[i].HourStart) {
			t.Errorf("rows not sorted: %s > %s", rows[i-1].HourStart, rows[i].HourStart)
		}
	}
}

// 429 rate-limit surfaces with a clear error — not a silent zero-row
// response that would look like "no sun ever".
func TestForecastSolar_RateLimitedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Ratelimit-Reset", "3600")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"message":{"code":429,"type":"error","text":"rate limit"}}`)
	}))
	defer srv.Close()
	fs := &ForecastSolarProvider{Client: srv.Client(), BaseURL: srv.URL, KWp: 10}
	_, err := fs.Fetch(context.Background(), 0, 0)
	if err == nil || !strings.Contains(err.Error(), "rate") {
		t.Errorf("want rate-limit error, got %v", err)
	}
}

// Zero or negative kWp is a config mistake — fail fast instead of
// emitting a nonsense URL.
func TestForecastSolar_RejectsZeroKWp(t *testing.T) {
	fs := NewForecastSolar(35, 180, 0)
	if _, err := fs.Fetch(context.Background(), 0, 0); err == nil {
		t.Error("expected error for kWp=0, got nil")
	}
}

// Verifies sortByHour stays stable even with already-sorted input.
func TestForecastSolarSortByHour(t *testing.T) {
	base := time.Date(2026, 4, 17, 10, 0, 0, 0, time.UTC)
	rows := []RawForecast{
		{HourStart: base.Add(2 * time.Hour)},
		{HourStart: base},
		{HourStart: base.Add(time.Hour)},
	}
	sortByHour(rows)
	if !rows[0].HourStart.Equal(base) {
		t.Errorf("first row = %v, want %v", rows[0].HourStart, base)
	}
	if !rows[2].HourStart.Equal(base.Add(2*time.Hour)) {
		t.Errorf("last row = %v, want %v", rows[2].HourStart, base.Add(2*time.Hour))
	}
}
