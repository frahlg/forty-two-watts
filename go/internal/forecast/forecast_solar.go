package forecast

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ForecastSolarProvider uses the free tier of api.forecast.solar, which
// combines the EU PVGIS clear-sky model with a weather forecast and
// returns site-specific PV output (watts) per timestamp for the next
// few days. No authentication required. Free-tier rate limit is
// 12 calls/hour over a rolling window; forecast.Service's 3-hour
// refresh loop uses a tiny fraction of that.
//
// Why it's better than cloud-fraction-only providers: the response is
// already calibrated for our site (lat/lon/tilt/azimuth/kWp are part
// of the URL), so the cloud_area_fraction → PV conversion that we
// approximate with (1 - cloud)^1.5 is done for us using their model.
// The RLS twin becomes a thin correction on top of an already-good
// prediction, not the sole calibration mechanism.
//
// Limitations:
//   - Doesn't return cloud cover or temperature; we set both nil so
//     downstream code falls back to whatever it uses when these are
//     absent (pvmodel: neutral 50%, fuse/thermal: no-op).
//   - The response is per-timestamp at irregular intervals (typically
//     minutely around dawn/dusk, larger during the flat part of the
//     day). We bucket to UTC hours by picking the sample closest to
//     each hour's start.
type ForecastSolarProvider struct {
	Client  *http.Client
	BaseURL string

	// Site geometry. Tilt is the panels' angle from horizontal (0 = flat
	// roof, 90 = wall). Azimuth is compass heading the panels face in
	// degrees (180 = south). kWp is nameplate DC capacity.
	TiltDeg    float64
	AzimuthDeg float64
	KWp        float64
}

// NewForecastSolar returns a configured provider. Pass the site's
// roof geometry + total installed capacity. Free tier requires no key.
func NewForecastSolar(tiltDeg, azimuthDeg, kWp float64) *ForecastSolarProvider {
	return &ForecastSolarProvider{
		Client:     &http.Client{Timeout: 15 * time.Second},
		BaseURL:    "https://api.forecast.solar",
		TiltDeg:    tiltDeg,
		AzimuthDeg: azimuthDeg,
		KWp:        kWp,
	}
}

// Name implements Provider.
func (f *ForecastSolarProvider) Name() string { return "forecast_solar" }

// Fetch implements Provider. Returns one RawForecast per UTC hour with
// PVWEstimated populated; cloud/temperature stay nil.
func (f *ForecastSolarProvider) Fetch(ctx context.Context, lat, lon float64) ([]RawForecast, error) {
	if f.KWp <= 0 {
		return nil, fmt.Errorf("forecast.solar: kWp must be > 0 (got %f)", f.KWp)
	}
	// /estimate/lat/lon/tilt/azimuth/kwp. ?time=utc forces UTC timestamps
	// in the response so we don't have to guess the server's locale.
	url := fmt.Sprintf(
		"%s/estimate/%.4f/%.4f/%.1f/%.1f/%.2f?time=utc",
		f.BaseURL, lat, lon, f.TiltDeg, f.AzimuthDeg, f.KWp,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		return nil, fmt.Errorf("forecast.solar: rate limited (429); reset at %s",
			resp.Header.Get("X-Ratelimit-Reset"))
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("forecast.solar: status %d: %s", resp.StatusCode, string(body))
	}
	var doc struct {
		Result struct {
			Watts map[string]float64 `json:"watts"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("forecast.solar: decode: %w", err)
	}

	// Bucket to UTC hours. The API returns timestamps like
	// "2026-04-17 12:00:00" (UTC when time=utc); we pick the first
	// sample whose timestamp falls inside each hour-aligned bucket and
	// use its watts value for the whole hour. Fine-grained dawn/dusk
	// transitions get smoothed to the hour, which is what the downstream
	// schema (SlotLenMin=60) expects anyway.
	buckets := make(map[int64]float64, len(doc.Result.Watts))
	for tsStr, w := range doc.Result.Watts {
		t, err := time.Parse("2006-01-02 15:04:05", tsStr)
		if err != nil {
			continue
		}
		// The API documents "local time unless ?time=utc"; we sent
		// ?time=utc so treat the string as UTC.
		t = t.UTC()
		hour := t.Truncate(time.Hour)
		hourMs := hour.UnixMilli()
		// Keep the MAX within the hour so a brief peak (e.g. sunrise
		// crossing the first minute) isn't washed out by the zero
		// timestamps the API emits right before dawn.
		if cur, ok := buckets[hourMs]; !ok || w > cur {
			buckets[hourMs] = w
		}
	}
	out := make([]RawForecast, 0, len(buckets))
	for hourMs, watts := range buckets {
		w := watts
		out = append(out, RawForecast{
			HourStart:    time.UnixMilli(hourMs).UTC(),
			PVWEstimated: &w,
		})
	}
	// Sort by HourStart so consumers (state.SaveForecasts, the MPC
	// lookup) see monotone timestamps.
	sortByHour(out)
	return out, nil
}

func sortByHour(rows []RawForecast) {
	// Insertion sort — the list is ~200 rows for 8 days at hourly
	// resolution, not worth importing sort.Slice.
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j-1].HourStart.After(rows[j].HourStart); j-- {
			rows[j-1], rows[j] = rows[j], rows[j-1]
		}
	}
}
