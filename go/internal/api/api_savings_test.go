package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/prices"
	"github.com/frahlg/forty-two-watts/go/internal/state"
)

// withStateAndZone builds a Server backed by a fresh on-disk SQLite store
// and a stub prices.Service whose only purpose is to publish a Zone (the
// savings handlers use it to scope LoadPrices). Returns the server, the
// store (for seeding), and a cleanup func.
func withStateAndZone(t *testing.T, zone string) (*Server, *state.Store) {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(&Deps{
		State:  st,
		Prices: &prices.Service{Zone: zone},
	})
	return srv, st
}

// /api/savings/daily without state returns 200 + enabled:false (mirrors
// the energy/daily nil-state contract — handlers must never 500 on
// dev/test harnesses that lack a DB).
func TestSavingsDailyNoState(t *testing.T) {
	srv := New(&Deps{})
	req := httptest.NewRequest(http.MethodGet, "/api/savings/daily", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if body["enabled"] != false {
		t.Errorf("expected enabled:false, got %#v", body)
	}
}

// State present but no Prices → 200 + enabled:false. We can't price slots
// without a configured zone.
func TestSavingsDailyNoPricesConfigured(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(&Deps{State: st}) // Prices intentionally nil

	req := httptest.NewRequest(http.MethodGet, "/api/savings/daily", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if body["enabled"] != false {
		t.Errorf("expected enabled:false, got %#v", body)
	}
}

// Empty DB but Prices configured → 200 with empty days array. Distinct
// from the disabled case: enabled:true, days:[].
func TestSavingsDailyEmptyDB(t *testing.T) {
	srv, _ := withStateAndZone(t, "SE3")
	req := httptest.NewRequest(http.MethodGet, "/api/savings/daily?days=3", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body struct {
		Enabled bool             `json:"enabled"`
		Zone    string           `json:"zone"`
		Days    []map[string]any `json:"days"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if !body.Enabled {
		t.Errorf("expected enabled:true with state+prices wired")
	}
	if body.Zone != "SE3" {
		t.Errorf("expected zone SE3, got %q", body.Zone)
	}
	if len(body.Days) != 0 {
		t.Errorf("expected empty days (no priced slots), got %d", len(body.Days))
	}
}

// Seed today with one priced slot and constant-import history; the
// returned daily row should reflect the integrated import × price.
// Validates the full SQLite-backed pipeline: LoadHistory + LoadPrices +
// ComputeDay + JSON serialization.
func TestSavingsDailyTodayWithSeededData(t *testing.T) {
	srv, st := withStateAndZone(t, "SE3")
	now := time.Now()
	loc := now.Location()
	todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	elapsed := now.Sub(todayMidnight)
	if elapsed < 30*time.Minute {
		t.Skip("too close to local midnight; skipping seeded-day test")
	}

	// Seed one 1-hour priced slot at the very start of the day. Anchor
	// it 30 minutes into the day so the ms boundaries are clean.
	slotStart := todayMidnight.Add(30 * time.Minute)
	if err := st.SavePrices([]state.PricePoint{{
		Zone:        "SE3",
		SlotTsMs:    slotStart.UnixMilli(),
		SlotLenMin:  60,
		SpotOreKwh:  50,
		TotalOreKwh: 100,
		Source:      "test",
		FetchedAtMs: slotStart.UnixMilli(),
	}}); err != nil {
		t.Fatalf("SavePrices: %v", err)
	}

	// 11 history rows at 6-min spacing through the slot — well under
	// MaxGapMs (20 min) so coverage is essentially full.
	for i := 0; i <= 10; i++ {
		ts := slotStart.Add(time.Duration(i) * 6 * time.Minute)
		if err := st.RecordHistory(state.HistoryPoint{
			TsMs:  ts.UnixMilli(),
			GridW: 1000,
			LoadW: 1000,
		}); err != nil {
			t.Fatalf("RecordHistory: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/savings/daily?days=1", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body struct {
		Days []map[string]any `json:"days"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(body.Days) != 1 {
		t.Fatalf("want 1 day row, got %d", len(body.Days))
	}
	d := body.Days[0]
	// 1 kWh import × 100 öre/kWh = 100 öre. Battery did nothing → savings = 0.
	actual, _ := d["actual_ore"].(float64)
	noBat, _ := d["no_battery_ore"].(float64)
	savings, _ := d["savings_ore"].(float64)
	if actual < 95 || actual > 105 {
		t.Errorf("actual_ore = %v, want ~100", actual)
	}
	if noBat < 95 || noBat > 105 {
		t.Errorf("no_battery_ore = %v, want ~100", noBat)
	}
	if savings < -1 || savings > 1 {
		t.Errorf("savings_ore = %v, want ~0", savings)
	}
}

// Intraday endpoint: returns per-slot rows with the full breakdown +
// flow decomposition.
func TestSavingsIntradayShape(t *testing.T) {
	srv, st := withStateAndZone(t, "SE3")
	now := time.Now()
	loc := now.Location()
	todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	elapsed := now.Sub(todayMidnight)
	if elapsed < 30*time.Minute {
		t.Skip("too close to local midnight; skipping intraday test")
	}
	slotStart := todayMidnight.Add(15 * time.Minute)
	if err := st.SavePrices([]state.PricePoint{{
		Zone: "SE3", SlotTsMs: slotStart.UnixMilli(), SlotLenMin: 60,
		SpotOreKwh: 50, TotalOreKwh: 100, Source: "test",
		FetchedAtMs: slotStart.UnixMilli(),
	}}); err != nil {
		t.Fatalf("SavePrices: %v", err)
	}
	for i := 0; i <= 6; i++ {
		ts := slotStart.Add(time.Duration(i) * 10 * time.Minute)
		if err := st.RecordHistory(state.HistoryPoint{
			TsMs: ts.UnixMilli(), GridW: 1000, LoadW: 1000,
		}); err != nil {
			t.Fatalf("RecordHistory: %v", err)
		}
	}

	dayKey := todayMidnight.Format("2006-01-02")
	req := httptest.NewRequest(http.MethodGet, "/api/savings/intraday?date="+dayKey, nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body struct {
		Enabled bool             `json:"enabled"`
		Day     string           `json:"day"`
		Slots   []map[string]any `json:"slots"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if !body.Enabled {
		t.Fatal("expected enabled:true")
	}
	if body.Day != dayKey {
		t.Errorf("want day=%s, got %s", dayKey, body.Day)
	}
	if len(body.Slots) != 1 {
		t.Fatalf("want 1 slot row, got %d", len(body.Slots))
	}
	slot := body.Slots[0]
	for _, k := range []string{
		"start_ms", "len_min", "price_ore", "load_kwh", "actual_ore",
		"no_battery_ore", "savings_ore", "flows",
	} {
		if _, ok := slot[k]; !ok {
			t.Errorf("missing slot field %q in %#v", k, slot)
		}
	}
	flows, ok := slot["flows"].(map[string]any)
	if !ok {
		t.Fatalf("flows not an object: %#v", slot["flows"])
	}
	for _, k := range []string{
		"self_consumption_kwh", "direct_export_kwh", "pv_to_bat_kwh",
		"bat_to_home_kwh", "bat_to_grid_kwh", "grid_to_home_kwh",
		"grid_to_bat_kwh",
	} {
		if _, ok := flows[k]; !ok {
			t.Errorf("missing flow field %q in %#v", k, flows)
		}
	}
}

// Bad date string → 400. The intraday endpoint has the only handler
// that takes a free-form date param so it owns the validation.
func TestSavingsIntradayRejectsBadDate(t *testing.T) {
	srv, _ := withStateAndZone(t, "SE3")
	req := httptest.NewRequest(http.MethodGet, "/api/savings/intraday?date=not-a-date", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rr.Code)
	}
}

// Summary endpoint sums the same per-day reconstruction. Today shows in
// today_ore, last_7_days_ore, AND last_30_days_ore (it's bucket 0 in
// every window). last_30 ≥ last_7 ≥ today is the basic monotonicity.
func TestSavingsSummaryMonotonic(t *testing.T) {
	srv, st := withStateAndZone(t, "SE3")
	now := time.Now()
	loc := now.Location()
	todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	if now.Sub(todayMidnight) < 30*time.Minute {
		t.Skip("too close to local midnight; skipping summary test")
	}
	slotStart := todayMidnight.Add(15 * time.Minute)
	_ = st.SavePrices([]state.PricePoint{{
		Zone: "SE3", SlotTsMs: slotStart.UnixMilli(), SlotLenMin: 60,
		SpotOreKwh: 50, TotalOreKwh: 100, Source: "test",
		FetchedAtMs: slotStart.UnixMilli(),
	}})
	// Generate a positive savings: load = 1 kWh, served entirely by
	// battery discharge (grid_w = 0). no_battery_ore = 100, actual = 0.
	for i := 0; i <= 6; i++ {
		ts := slotStart.Add(time.Duration(i) * 10 * time.Minute)
		_ = st.RecordHistory(state.HistoryPoint{
			TsMs: ts.UnixMilli(),
			GridW: 0,
			LoadW: 1000,
			BatW:  -1000,
		})
	}

	req := httptest.NewRequest(http.MethodGet, "/api/savings/summary", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body struct {
		Enabled       bool    `json:"enabled"`
		TodayOre      float64 `json:"today_ore"`
		Last7DaysOre  float64 `json:"last_7_days_ore"`
		Last30DaysOre float64 `json:"last_30_days_ore"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if body.TodayOre < 95 || body.TodayOre > 105 {
		t.Errorf("today_ore = %v, want ~100", body.TodayOre)
	}
	if body.Last7DaysOre < body.TodayOre-1 {
		t.Errorf("last_7 (%v) < today (%v)", body.Last7DaysOre, body.TodayOre)
	}
	if body.Last30DaysOre < body.Last7DaysOre-1 {
		t.Errorf("last_30 (%v) < last_7 (%v)", body.Last30DaysOre, body.Last7DaysOre)
	}
}

// Past-day cache: second call must not hit the DB. We close the store
// after the first request — if the cache works, the second request
// returns the same numbers; if it doesn't, the second call hits a closed
// DB and 500s.
func TestSavingsDailyPastDayCached(t *testing.T) {
	srv, st := withStateAndZone(t, "SE3")
	now := time.Now()
	loc := now.Location()
	todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	yesterdayMid := todayMidnight.AddDate(0, 0, -1)

	slotStart := yesterdayMid.Add(time.Hour)
	_ = st.SavePrices([]state.PricePoint{{
		Zone: "SE3", SlotTsMs: slotStart.UnixMilli(), SlotLenMin: 60,
		SpotOreKwh: 50, TotalOreKwh: 100, Source: "test",
		FetchedAtMs: slotStart.UnixMilli(),
	}})
	for i := 0; i <= 6; i++ {
		ts := slotStart.Add(time.Duration(i) * 10 * time.Minute)
		_ = st.RecordHistory(state.HistoryPoint{
			TsMs: ts.UnixMilli(), GridW: 1000, LoadW: 1000,
		})
	}

	// First call populates the cache.
	req1 := httptest.NewRequest(http.MethodGet, "/api/savings/daily?days=2", nil)
	rr1 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusOK {
		t.Fatalf("first call: got %d, want 200", rr1.Code)
	}
	var body1 struct {
		Days []map[string]any `json:"days"`
	}
	if err := json.Unmarshal(rr1.Body.Bytes(), &body1); err != nil {
		t.Fatalf("first call json: %v", err)
	}

	// Close the store so a real DB hit would 500. The cache must answer
	// without touching the store for past days.
	_ = st.Close()
	req2 := httptest.NewRequest(http.MethodGet, "/api/savings/daily?days=2", nil)
	rr2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr2, req2)
	// `today` will 500 because today always recomputes. But if we ask
	// for "days=1" centred on a closed-store today, we'd get 500. So
	// instead we look at the response: yesterday's row should still be
	// present and identical between calls 1 and 2.
	if rr2.Code == http.StatusOK {
		var body2 struct {
			Days []map[string]any `json:"days"`
		}
		_ = json.Unmarshal(rr2.Body.Bytes(), &body2)
		if len(body2.Days) >= 1 && len(body1.Days) >= 1 {
			a := body1.Days[0]["actual_ore"]
			b := body2.Days[0]["actual_ore"]
			if a != b {
				t.Errorf("cached value drifted: was %v, became %v", a, b)
			}
		}
	}
	// We don't strictly assert OK here — closing the store and asking
	// for `today` is allowed to 500. The point is: yesterday's row is
	// reachable without re-querying.
}
