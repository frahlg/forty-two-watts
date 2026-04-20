package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

func TestParseRangeSupports48h(t *testing.T) {
	const want = 48 * 60 * 60 * 1000
	if got := parseRange("48h"); got != want {
		t.Fatalf("parseRange(48h) = %d, want %d", got, want)
	}
}

// handleEVCommand rejects anything not in the allowlist with 400. The Lua
// driver's command hook silently returns nil for unknown actions, so
// without this gate the API would 200-OK typos.
func TestHandleEVCommandRejectsUnknownActions(t *testing.T) {
	srv := New(&Deps{}) // registry/tel unset — the 400 branches run first

	cases := []struct {
		name string
		body string
		want int
	}{
		{"unknown action", `{"action":"ev_nuke"}`, http.StatusBadRequest},
		{"empty action", `{"action":""}`, http.StatusBadRequest},
		{"missing action field", `{}`, http.StatusBadRequest},
		{"malformed json", `{`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/ev/command", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("body=%s: got status %d, want %d (body: %s)", tc.body, rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

// Allowlisted actions pass the action-validation gate. With a nil
// Registry they then short-circuit to 503, which confirms they got past
// the 400 branch without us needing a real driver registry.
func TestHandleEVCommandAcceptsAllowlistedActions(t *testing.T) {
	srv := New(&Deps{})

	for action := range validEVActions {
		t.Run(action, func(t *testing.T) {
			body := `{"action":"` + action + `"}`
			req := httptest.NewRequest(http.MethodPost, "/api/ev/command", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("action=%q: got status %d, want 503 (body: %s)", action, rr.Code, rr.Body.String())
			}
		})
	}
}

// Unknown driver in the JSON body must 404 before we touch the registry —
// otherwise the UI would silently fan a command to whichever charger
// happened to be first in the readings slice (the pre-fix behavior).
func TestHandleEVCommandRejectsUnknownDriver(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("easee-1", telemetry.DerEV, 0, nil, nil)
	srv := New(&Deps{Tel: tel}) // Registry nil — known driver would 503

	req := httptest.NewRequest(http.MethodPost, "/api/ev/command",
		strings.NewReader(`{"action":"ev_start","driver":"does-not-exist"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown driver: got %d, want 404 (body: %s)", rr.Code, rr.Body.String())
	}
}

// Unknown driver on GET /api/ev/status must 404 — multi-EV UI needs to
// surface the mismatch rather than silently fall back to readings[0].
func TestHandleEVStatusRejectsUnknownDriver(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("easee-1", telemetry.DerEV, 0, nil, nil)
	srv := New(&Deps{Tel: tel})

	req := httptest.NewRequest(http.MethodGet, "/api/ev/status?driver=does-not-exist", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown driver: got %d, want 404 (body: %s)", rr.Code, rr.Body.String())
	}
}

// With no state backing, /api/energy/daily returns an empty payload at
// 200 (the "history is optional" branch). Anything else would break
// dev/test harnesses that run without a DB.
func TestHandleEnergyDailyNoState(t *testing.T) {
	srv := New(&Deps{})
	req := httptest.NewRequest(http.MethodGet, "/api/energy/daily?days=7", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("nil state: got %d, want 200", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v (body: %s)", err, rr.Body.String())
	}
	days, ok := body["days"].([]any)
	if !ok {
		t.Fatalf("missing days array: %#v", body)
	}
	if len(days) != 0 {
		t.Fatalf("expected empty days, got %d entries", len(days))
	}
}

// An empty history DB must still return N pre-seeded day buckets with
// zero-valued fields — that's what lets the UI distinguish "no data
// yet" (zeros) from a backend failure (500).
func TestHandleEnergyDailyEmptyDBReturnsZeroedDays(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(&Deps{State: st})

	req := httptest.NewRequest(http.MethodGet, "/api/energy/daily?days=5", nil)
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
	if len(body.Days) != 5 {
		t.Fatalf("want 5 day buckets, got %d", len(body.Days))
	}
	for i, d := range body.Days {
		if d["day"] == "" {
			t.Errorf("day[%d] missing date", i)
		}
		for _, f := range []string{"import_wh", "export_wh", "pv_wh", "bat_charged_wh", "bat_discharged_wh", "load_wh"} {
			if v, _ := d[f].(float64); v != 0 {
				t.Errorf("day[%d].%s = %v, want 0", i, f, v)
			}
		}
	}
}

// Dropping real history into a few buckets and confirming the
// integration math lands the expected Wh in the expected day buckets.
// This is the site-convention regression net: any future sign flip
// inside the driver layer will show up here.
func TestHandleEnergyDailyBucketsByLocalDay(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Drop two samples inside today, separated by `gap`, both strictly
	// between todayMidnight and now. The two-sample slice integrates to
	// GridW * gap == 1000 * gapHours Wh of import attributed to today.
	// Sizing the gap off `elapsed` keeps the test robust when CI runs
	// early in the morning (e.g. 01:46 local — the original hard-coded
	// "now - 1h, now - 2h" scheme fell before midnight and got filtered
	// out by LoadHistory's [firstDayStart, now] range).
	now := time.Now()
	loc := now.Location()
	todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	elapsed := now.Sub(todayMidnight)
	if elapsed < 15*time.Minute {
		t.Skip("too close to local midnight; skipping bucket test")
	}
	gap := elapsed / 3
	t0 := todayMidnight.Add(gap)
	t1 := t0.Add(gap)
	gapHours := gap.Seconds() / 3600.0
	expectedImport := 1000.0 * gapHours
	for _, p := range []state.HistoryPoint{
		{TsMs: t0.UnixMilli(), GridW: 1000},
		{TsMs: t1.UnixMilli(), GridW: 1000},
	} {
		if err := st.RecordHistory(p); err != nil {
			t.Fatalf("RecordHistory: %v", err)
		}
	}

	srv := New(&Deps{State: st})
	req := httptest.NewRequest(http.MethodGet, "/api/energy/daily?days=3", nil)
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
	if len(body.Days) != 3 {
		t.Fatalf("want 3 buckets, got %d", len(body.Days))
	}
	// Today is the last entry; the earlier two must be zero.
	for i, d := range body.Days[:2] {
		if v, _ := d["import_wh"].(float64); v != 0 {
			t.Errorf("day[%d].import_wh = %v, want 0 (older bucket should be empty)", i, v)
		}
	}
	todayImport, _ := body.Days[2]["import_wh"].(float64)
	// Allow 1% slop: SQLite ms-precision vs Go's time.Sub can differ.
	tolerance := 0.01 * expectedImport
	if tolerance < 1 {
		tolerance = 1
	}
	if diff := todayImport - expectedImport; diff < -tolerance || diff > tolerance {
		t.Errorf("today import_wh = %v, want ~%v (gap=%v)", todayImport, expectedImport, gap)
	}
}

// Closing the state store mid-flight turns LoadHistory into an error
// path. The old handler silently returned zeroed days (indistinguishable
// from a real 0 kWh day); the new handler returns 500 so operators see
// the failure.
func TestHandleEnergyDailyReturns500OnDBError(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	_ = st.Close() // force subsequent LoadHistory to fail
	srv := New(&Deps{State: st})

	req := httptest.NewRequest(http.MethodGet, "/api/energy/daily?days=3", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500 (body: %s)", rr.Code, rr.Body.String())
	}
}

// days= parsing: garbage/0/negative fall through to default 7; >90 caps.
// Mirrors the silent-default convention used by parseRange elsewhere.
func TestHandleEnergyDailyDaysClamping(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(&Deps{State: st})

	cases := []struct {
		q    string
		want int
	}{
		{"", 7},
		{"abc", 7},
		{"-5", 7},
		{"0", 7},
		{"14", 14},
		{"150", 90},
	}
	for _, tc := range cases {
		t.Run(tc.q, func(t *testing.T) {
			url := "/api/energy/daily"
			if tc.q != "" {
				url += "?days=" + tc.q
			}
			req := httptest.NewRequest(http.MethodGet, url, nil)
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("q=%q: got %d, want 200", tc.q, rr.Code)
			}
			var body struct {
				Days []map[string]any `json:"days"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("q=%q: invalid json: %v", tc.q, err)
			}
			if len(body.Days) != tc.want {
				t.Fatalf("q=%q: got %d days, want %d", tc.q, len(body.Days), tc.want)
			}
		})
	}
}
