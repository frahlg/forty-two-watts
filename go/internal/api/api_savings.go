package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/mpc"
	"github.com/frahlg/forty-two-watts/go/internal/savings"
)

// savingsCache memoizes per-local-day savings reconstructions. Past days
// are immutable once the day ends, so they're computed at most once per
// process. Today is always recomputed (the day is in flight). The cache
// is process-wide; nothing persists to disk in this PR — if it turns out
// reconstructing 30 days at startup matters in practice, a daily_savings
// table is the natural follow-up.
type savingsCache struct {
	mu  sync.Mutex
	day map[string]savings.DaySavings
}

func newSavingsCache() *savingsCache {
	return &savingsCache{day: make(map[string]savings.DaySavings)}
}

func (c *savingsCache) get(key string) (savings.DaySavings, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.day[key]
	return v, ok
}

func (c *savingsCache) set(key string, v savings.DaySavings) {
	c.mu.Lock()
	c.day[key] = v
	c.mu.Unlock()
}

// historyLeadinMs is how far before the requested window we widen the
// LoadHistory query so the first slot's integration has a prev_ts row
// (otherwise the row at exactly slotStart contributes zero by left-Riemann
// convention, leaving the first ~5 s of the day uncosted). One MaxGapMs
// is overkill but cheap and matches the integrator's own staleness cap.
const historyLeadinMs int64 = int64(savings.MaxGapMs)

// ---- /api/savings/daily ----
//
// Query params: days=N (default 30, capped at 90)
// Response:
//
//	{ "enabled": true, "tz": "Local", "zone": "SE3",
//	  "days": [
//	    { "day": "YYYY-MM-DD",
//	      "actual_ore": ..., "no_battery_ore": ..., "savings_ore": ...,
//	      "import_kwh": ..., "export_kwh": ..., "pv_kwh": ...,
//	      "load_kwh": ..., "bat_charged_kwh": ..., "bat_discharged_kwh": ...,
//	      "coverage_pct": 0.98 } ]}
//
// Per-day reconstruction: load that day's history + prices, call
// savings.ComputeDay with the planner's current export terms.
func (s *Server) handleSavingsDaily(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil {
		writeJSON(w, 200, map[string]any{"enabled": false})
		return
	}
	zone := s.zoneOrEmpty()
	if zone == "" {
		writeJSON(w, 200, map[string]any{"enabled": false, "reason": "prices not configured"})
		return
	}

	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	if days > 90 {
		days = 90
	}

	now := time.Now()
	loc := now.Location()
	todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	params := s.savingsParams()
	out := make([]map[string]any, 0, days)
	for i := days - 1; i >= 0; i-- {
		dayStart := todayMidnight.AddDate(0, 0, -i)
		dayKey := dayStart.Format("2006-01-02")
		isToday := i == 0
		ds, ok, err := s.computeDayCached(dayKey, dayStart, isToday, zone, params, now)
		if err != nil {
			slog.Error("handleSavingsDaily: compute failed", "err", err, "day", dayKey)
			http.Error(w, "savings reconstruction failed", http.StatusInternalServerError)
			return
		}
		if !ok {
			continue
		}
		out = append(out, map[string]any{
			"day":                dayKey,
			"actual_ore":         ds.ActualOre,
			"no_battery_ore":     ds.NoBatteryOre,
			"savings_ore":        ds.SavingsOre,
			"import_kwh":         ds.ImportKWh,
			"export_kwh":         ds.ExportKWh,
			"pv_kwh":             ds.PVKWh,
			"load_kwh":           ds.LoadKWh,
			"bat_charged_kwh":    ds.BatChargedKWh,
			"bat_discharged_kwh": ds.BatDischargedKWh,
			"coverage_pct":       ds.CoveragePct,
		})
	}
	writeJSON(w, 200, map[string]any{
		"enabled": true,
		"tz":      loc.String(),
		"zone":    zone,
		"days":    out,
	})
}

// ---- /api/savings/intraday ----
//
// Query params: date=YYYY-MM-DD (default today, local tz)
// Response: full DaySavings including per-slot breakdown + flow
// decomposition + (when available) predicted-vs-actual overlay from
// planner_diagnostics.
//
// Predicted overlay: for each slot, look up the planner snapshot active
// at the slot start (LoadDiagnosticAt) and find the matching Action.
// CostOre. If found, attached as `predicted_ore` and the slot's
// `prediction_error_ore` = actual − predicted. Slots with no matching
// snapshot omit both fields.
func (s *Server) handleSavingsIntraday(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil {
		writeJSON(w, 200, map[string]any{"enabled": false})
		return
	}
	zone := s.zoneOrEmpty()
	if zone == "" {
		writeJSON(w, 200, map[string]any{"enabled": false, "reason": "prices not configured"})
		return
	}

	now := time.Now()
	loc := now.Location()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	if v := r.URL.Query().Get("date"); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, loc)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "date must be YYYY-MM-DD"})
			return
		}
		dayStart = t
	}
	dayKey := dayStart.Format("2006-01-02")
	isToday := dayStart.Equal(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc))
	params := s.savingsParams()

	ds, ok, err := s.computeDayCached(dayKey, dayStart, isToday, zone, params, now)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, 200, map[string]any{
			"enabled": true,
			"day":     dayKey,
			"slots":   []any{},
		})
		return
	}

	// Predicted-vs-actual overlay. We do this on read (not at compute
	// time) so the cache stays purely a function of measured data —
	// new planner snapshots arriving between compute and view will
	// still be picked up.
	withPredicted := s.attachPredictedOverlay(ds.Slots)

	writeJSON(w, 200, map[string]any{
		"enabled":        true,
		"day":            dayKey,
		"start_ms":       ds.StartMs,
		"end_ms":         ds.EndMs,
		"actual_ore":     ds.ActualOre,
		"no_battery_ore": ds.NoBatteryOre,
		"savings_ore":    ds.SavingsOre,
		"coverage_pct":   ds.CoveragePct,
		"slots":          withPredicted,
	})
}

// ---- /api/savings/summary ----
//
// Returns headline totals: today, last 7 days, last 30 days. Cheap KPI
// for a dashboard card. Re-uses the same per-day cache as /daily.
func (s *Server) handleSavingsSummary(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil {
		writeJSON(w, 200, map[string]any{"enabled": false})
		return
	}
	zone := s.zoneOrEmpty()
	if zone == "" {
		writeJSON(w, 200, map[string]any{"enabled": false, "reason": "prices not configured"})
		return
	}

	now := time.Now()
	loc := now.Location()
	todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	params := s.savingsParams()

	// Walk 30 days. today_ore is bucket 0; last_7 sums buckets 0..6;
	// last_30 sums all 30. One pass, three accumulators.
	var todayOre, last7Ore, last30Ore float64
	for i := 0; i < 30; i++ {
		dayStart := todayMidnight.AddDate(0, 0, -i)
		dayKey := dayStart.Format("2006-01-02")
		isToday := i == 0
		ds, ok, err := s.computeDayCached(dayKey, dayStart, isToday, zone, params, now)
		if err != nil {
			slog.Error("handleSavingsSummary: compute failed", "err", err, "day", dayKey)
			http.Error(w, "savings reconstruction failed", http.StatusInternalServerError)
			return
		}
		if !ok {
			continue
		}
		if i == 0 {
			todayOre = ds.SavingsOre
		}
		if i < 7 {
			last7Ore += ds.SavingsOre
		}
		last30Ore += ds.SavingsOre
	}

	writeJSON(w, 200, map[string]any{
		"enabled":          true,
		"tz":               loc.String(),
		"zone":             zone,
		"today_ore":        todayOre,
		"last_7_days_ore":  last7Ore,
		"last_30_days_ore": last30Ore,
	})
}

// computeDayCached reconstructs one local day's savings, returning ok=false
// when no priced slots overlap it (a freshly installed system with no
// price history yet). Past days are memoized; today always recomputes.
func (s *Server) computeDayCached(
	dayKey string, dayStart time.Time, isToday bool,
	zone string, params mpc.Params, now time.Time,
) (savings.DaySavings, bool, error) {
	if !isToday {
		if cached, ok := s.savingsCache.get(dayKey); ok {
			if len(cached.Slots) == 0 {
				return cached, false, nil
			}
			return cached, true, nil
		}
	}

	dayEnd := dayStart.AddDate(0, 0, 1)
	startMs := dayStart.UnixMilli()
	endMs := dayEnd.UnixMilli()
	if isToday {
		// In-progress day: don't try to score future slots. Cap at now.
		if nowMs := now.UnixMilli(); nowMs < endMs {
			endMs = nowMs
		}
	}
	if endMs <= startMs {
		empty := savings.DaySavings{StartMs: startMs, EndMs: endMs}
		return empty, false, nil
	}

	// Widen the history query slightly on the leading edge so the first
	// slot's integration finds a prev_ts; the trailing edge is naturally
	// bounded by endMs.
	hist, err := s.deps.State.LoadHistory(startMs-historyLeadinMs, endMs, 0)
	if err != nil {
		return savings.DaySavings{}, false, err
	}
	prices, err := s.deps.State.LoadPrices(zone, startMs-historyLeadinMs, endMs+historyLeadinMs)
	if err != nil {
		return savings.DaySavings{}, false, err
	}

	ds := savings.ComputeDay(hist, prices, params, startMs, endMs)
	if !isToday {
		// Cache even empty results so repeat calls don't re-query the DB.
		s.savingsCache.set(dayKey, ds)
	}
	if len(ds.Slots) == 0 {
		return ds, false, nil
	}
	return ds, true, nil
}

// attachPredictedOverlay walks the slot list once, asks the diagnostics
// store for the active snapshot at each slot start, and unmarshals just
// the Actions array to find a matching SlotStartMs. Snapshots typically
// cover 100+ slots each so we cache by snapshot ts_ms within this call —
// reading the same Plan JSON 96 times for one day's drill-down would be
// silly.
func (s *Server) attachPredictedOverlay(slots []savings.SlotSavings) []map[string]any {
	out := make([]map[string]any, 0, len(slots))
	type planActions struct {
		Actions []struct {
			SlotStartMs int64   `json:"slot_start_ms"`
			CostOre     float64 `json:"cost_ore"`
			BatteryW    float64 `json:"battery_w"`
			GridW       float64 `json:"grid_w"`
		} `json:"actions"`
	}
	cache := make(map[int64]*planActions)

	for _, ss := range slots {
		row := map[string]any{
			"start_ms":            ss.StartMs,
			"len_min":             ss.LenMin,
			"price_ore":           ss.PriceOre,
			"spot_ore":            ss.SpotOre,
			"coverage_ms":         ss.CoverageMs,
			"load_kwh":            ss.LoadKWh,
			"pv_kwh":              ss.PVKWh,
			"grid_import_kwh":     ss.GridImportKWh,
			"grid_export_kwh":     ss.GridExportKWh,
			"bat_charged_kwh":     ss.BatChargedKWh,
			"bat_discharged_kwh":  ss.BatDischargedKWh,
			"no_battery_grid_kwh": ss.NoBatteryGridKWh,
			"actual_ore":          ss.ActualOre,
			"no_battery_ore":      ss.NoBatteryOre,
			"savings_ore":         ss.SavingsOre,
			"flows":               ss.Flows,
		}

		// Predicted overlay — best-effort. Every failure is silent; the
		// row still ships without the fields.
		if s.deps.State != nil {
			diag, err := s.deps.State.LoadDiagnosticAt(ss.StartMs)
			if err == nil && diag != nil {
				pa, ok := cache[diag.TsMs]
				if !ok {
					pa = &planActions{}
					if err := json.Unmarshal([]byte(diag.JSON), pa); err == nil {
						cache[diag.TsMs] = pa
					} else {
						pa = nil
					}
				}
				if pa != nil {
					for _, a := range pa.Actions {
						if a.SlotStartMs == ss.StartMs {
							row["predicted_ore"] = a.CostOre
							row["prediction_error_ore"] = ss.ActualOre - a.CostOre
							row["predicted_battery_w"] = a.BatteryW
							row["predicted_grid_w"] = a.GridW
							break
						}
					}
				}
			}
		}
		out = append(out, row)
	}
	return out
}

// zoneOrEmpty returns the configured price zone, "" if Prices isn't
// wired (savings can't price a slot without one).
func (s *Server) zoneOrEmpty() string {
	if s.deps.Prices == nil {
		return ""
	}
	return s.deps.Prices.Zone
}

// savingsParams returns the export-terms snapshot the savings handlers
// re-cost slots with. Falls back to a zero-value Params when MPC isn't
// running — SlotGridCostOre still works (default export = SpotOre with
// no bonus or fee), the result is just less aligned with whatever
// custom feed-in tariff the operator set.
func (s *Server) savingsParams() mpc.Params {
	if s.deps.MPC == nil {
		return mpc.Params{}
	}
	return s.deps.MPC.LatestParams()
}

