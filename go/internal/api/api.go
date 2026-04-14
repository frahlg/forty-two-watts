// Package api is the HTTP surface for forty-two-watts: control endpoints,
// telemetry queries, config get/set, battery-model introspection, self-tune
// orchestration, static file serving for the web UI.
//
// All responses are JSON (or raw file content for /static). All mutation
// endpoints accept JSON bodies. No WebSockets yet — clients poll.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/battery"
	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/control"
	"github.com/frahlg/forty-two-watts/go/internal/selftune"
	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// Deps is the full set of runtime dependencies the API handlers need.
// One instance is shared across all handlers; mutations use the contained
// mutexes from each package.
type Deps struct {
	Tel             *telemetry.Store
	Ctrl            *control.State
	CtrlMu          *sync.Mutex
	State           *state.Store
	CapMu           *sync.RWMutex
	Capacities      map[string]float64 // driver → battery_capacity_wh
	CfgMu           *sync.RWMutex
	Cfg             *config.Config
	ConfigPath      string
	Models          map[string]*battery.Model
	ModelsMu        *sync.Mutex
	SelfTune        *selftune.Coordinator
	DtS             float64 // control interval seconds (for model τ / age displays)
	SaveConfig      func(path string, c *config.Config) error // injection for testability
	WebDir          string // static assets root (default "web")

	Version string
}

// Server wraps the http.ServeMux and adds shared middleware (logging,
// no-cache headers on static assets).
type Server struct {
	deps *Deps
	mux  *http.ServeMux
}

// New creates a new API server.
func New(deps *Deps) *Server {
	if deps.Version == "" { deps.Version = "dev" }
	if deps.WebDir == "" { deps.WebDir = "web" }
	s := &Server{deps: deps, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler returns the http.Handler suitable for `http.ListenAndServe`.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	// ---- JSON endpoints ----
	s.handle("GET  /api/health",              s.handleHealth)
	s.handle("GET  /api/status",              s.handleStatus)
	s.handle("GET  /api/config",              s.handleGetConfig)
	s.handle("POST /api/config",              s.handlePostConfig)
	s.handle("GET  /api/mode",                s.handleGetMode)
	s.handle("POST /api/mode",                s.handleSetMode)
	s.handle("POST /api/target",              s.handleSetTarget)
	s.handle("POST /api/peak_limit",          s.handleSetPeakLimit)
	s.handle("POST /api/ev_charging",         s.handleSetEVCharging)
	s.handle("GET  /api/drivers",             s.handleDrivers)
	s.handle("GET  /api/battery_models",      s.handleGetModels)
	s.handle("POST /api/battery_models/reset", s.handleResetModel)
	s.handle("POST /api/self_tune/start",     s.handleSelfTuneStart)
	s.handle("GET  /api/self_tune/status",    s.handleSelfTuneStatus)
	s.handle("POST /api/self_tune/cancel",    s.handleSelfTuneCancel)
	s.handle("GET  /api/history",             s.handleHistory)

	// ---- Static web UI ----
	// Everything not matched above falls through to the static server.
	s.mux.HandleFunc("/", s.handleStatic)
}

// handle wires "METHOD path" to a handler. Uses Go 1.22+ method-scoped
// routing so GET + POST on the same path can be registered independently.
func (s *Server) handle(methodPath string, h http.HandlerFunc) {
	parts := strings.SplitN(strings.TrimSpace(methodPath), " ", 2)
	for i := range parts { parts[i] = strings.TrimSpace(parts[i]) }
	method, path := parts[0], parts[1]
	s.mux.HandleFunc(method+" "+path, h)
	_ = fmt.Sprintf // keep fmt import used elsewhere
}

// ---- Common helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MB cap
	if err != nil { return err }
	if len(body) == 0 {
		return errors.New("empty body")
	}
	return json.Unmarshal(body, v)
}

// ---- /api/health ----

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	health := s.deps.Tel.AllHealth()
	var ok, deg, off int
	for _, h := range health {
		switch h.Status {
		case telemetry.StatusOk:
			ok++
		case telemetry.StatusDegraded:
			deg++
		case telemetry.StatusOffline:
			off++
		}
	}
	status := "ok"
	if off > 0 { status = "degraded" }
	writeJSON(w, 200, map[string]any{
		"status":           status,
		"drivers_ok":       ok,
		"drivers_degraded": deg,
		"drivers_offline":  off,
	})
}

// ---- /api/status ----

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.deps.CtrlMu.Lock()
	ctrl := *s.deps.Ctrl // copy for consistency
	lastTargets := append([]control.DispatchTarget{}, s.deps.Ctrl.LastTargets...)
	s.deps.CtrlMu.Unlock()

	s.deps.CapMu.RLock()
	caps := make(map[string]float64, len(s.deps.Capacities))
	for k, v := range s.deps.Capacities { caps[k] = v }
	s.deps.CapMu.RUnlock()

	// Aggregate readings
	gridW := 0.0
	if r := s.deps.Tel.Get(ctrl.SiteMeterDriver, telemetry.DerMeter); r != nil {
		gridW = r.SmoothedW
	}
	var pvW, batW float64
	for _, r := range s.deps.Tel.ReadingsByType(telemetry.DerPV) {
		pvW += r.SmoothedW
	}
	for _, r := range s.deps.Tel.ReadingsByType(telemetry.DerBattery) {
		batW += r.SmoothedW
	}

	// Load = grid + bat + pv?  Under site convention (+ into site):
	//   grid = load - (bat discharge) - pv_gen ... signs work out to:
	//   load = grid - bat - pv (all in site convention signs)
	//   but load is always positive. If calc goes negative it's a sign issue.
	rawLoad := gridW - batW - pvW
	loadW := s.deps.Tel.UpdateLoad(rawLoad)
	if loadW < 0 { loadW = 0 }

	// Weighted average SoC by capacity
	var totalCap, weightedSoC float64
	for _, b := range s.deps.Tel.ReadingsByType(telemetry.DerBattery) {
		cap, ok := caps[b.Driver]
		if !ok { continue }
		totalCap += cap
		soc := 0.0
		if b.SoC != nil { soc = *b.SoC }
		weightedSoC += soc * cap
	}
	avgSoC := 0.0
	if totalCap > 0 { avgSoC = weightedSoC / totalCap }

	// Per-driver details
	drivers := make(map[string]any)
	for name, h := range s.deps.Tel.AllHealth() {
		d := map[string]any{
			"status":             h.Status.String(),
			"consecutive_errors": h.ConsecutiveErrors,
			"tick_count":         h.TickCount,
		}
		if h.LastError != "" { d["last_error"] = h.LastError }
		if r := s.deps.Tel.Get(name, telemetry.DerMeter); r != nil {
			d["meter_w"] = r.SmoothedW
		}
		if r := s.deps.Tel.Get(name, telemetry.DerPV); r != nil {
			d["pv_w"] = r.SmoothedW
		}
		if r := s.deps.Tel.Get(name, telemetry.DerBattery); r != nil {
			d["bat_w"] = r.SmoothedW
			if r.SoC != nil { d["bat_soc"] = *r.SoC }
		}
		drivers[name] = d
	}

	// Dispatch targets
	dispatch := make([]map[string]any, 0, len(lastTargets))
	for _, t := range lastTargets {
		dispatch = append(dispatch, map[string]any{
			"driver":   t.Driver,
			"target_w": t.TargetW,
			"clamped":  t.Clamped,
		})
	}

	writeJSON(w, 200, map[string]any{
		"version":         s.deps.Version,
		"mode":            ctrl.Mode,
		"grid_w":          gridW,
		"pv_w":            pvW,
		"bat_w":           batW,
		"load_w":          loadW,
		"bat_soc":         avgSoC,
		"grid_target_w":   ctrl.GridTargetW,
		"peak_limit_w":    ctrl.PeakLimitW,
		"ev_charging_w":   ctrl.EVChargingW,
		"drivers":         drivers,
		"dispatch":        dispatch,
	})
}

// ---- /api/config ----

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	s.deps.CfgMu.RLock()
	cfg := *s.deps.Cfg
	s.deps.CfgMu.RUnlock()
	writeJSON(w, 200, cfg)
}

func (s *Server) handlePostConfig(w http.ResponseWriter, r *http.Request) {
	var newCfg config.Config
	if err := readJSON(r, &newCfg); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid config: " + err.Error()})
		return
	}
	if err := newCfg.Validate(); err != nil {
		writeJSON(w, 400, map[string]string{"error": "validation: " + err.Error()})
		return
	}
	// Persist atomically
	if err := s.deps.SaveConfig(s.deps.ConfigPath, &newCfg); err != nil {
		writeJSON(w, 500, map[string]string{"error": "save failed: " + err.Error()})
		return
	}
	// Apply control-level changes immediately (file watcher will also pick
	// this up but we're snappier).
	s.deps.CtrlMu.Lock()
	s.deps.Ctrl.SetGridTarget(newCfg.Site.GridTargetW)
	s.deps.Ctrl.GridToleranceW = newCfg.Site.GridToleranceW
	s.deps.Ctrl.SlewRateW = newCfg.Site.SlewRateW
	s.deps.Ctrl.MinDispatchIntervalS = newCfg.Site.MinDispatchIntervalS
	s.deps.CtrlMu.Unlock()
	// Update shared cfg pointer
	s.deps.CfgMu.Lock()
	*s.deps.Cfg = newCfg
	s.deps.CfgMu.Unlock()
	slog.Info("config updated via API")
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// ---- /api/mode ----

func (s *Server) handleGetMode(w http.ResponseWriter, r *http.Request) {
	s.deps.CtrlMu.Lock()
	defer s.deps.CtrlMu.Unlock()
	writeJSON(w, 200, map[string]any{
		"mode":          s.deps.Ctrl.Mode,
		"grid_target_w": s.deps.Ctrl.GridTargetW,
	})
}

func (s *Server) handleSetMode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.deps.CtrlMu.Lock()
	defer s.deps.CtrlMu.Unlock()
	// Validate mode string
	switch control.Mode(req.Mode) {
	case control.ModeIdle, control.ModeSelfConsumption, control.ModePeakShaving,
		control.ModeCharge, control.ModePriority, control.ModeWeighted:
		s.deps.Ctrl.Mode = control.Mode(req.Mode)
		_ = s.deps.State.SaveConfig("mode", req.Mode)
		writeJSON(w, 200, map[string]string{"status": "ok", "mode": req.Mode})
	default:
		writeJSON(w, 400, map[string]string{"error": "unknown mode: " + req.Mode})
	}
}

// ---- /api/target ----

func (s *Server) handleSetTarget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GridTargetW float64 `json:"grid_target_w"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.deps.CtrlMu.Lock()
	s.deps.Ctrl.SetGridTarget(req.GridTargetW)
	s.deps.CtrlMu.Unlock()
	_ = s.deps.State.SaveConfig("grid_target_w", strconv.FormatFloat(req.GridTargetW, 'f', 1, 64))
	slog.Info("grid target changed", "w", req.GridTargetW)
	writeJSON(w, 200, map[string]any{"status": "ok", "grid_target_w": req.GridTargetW})
}

// ---- /api/peak_limit ----

func (s *Server) handleSetPeakLimit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PeakLimitW float64 `json:"peak_limit_w"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.deps.CtrlMu.Lock()
	s.deps.Ctrl.PeakLimitW = req.PeakLimitW
	s.deps.CtrlMu.Unlock()
	writeJSON(w, 200, map[string]any{"status": "ok", "peak_limit_w": req.PeakLimitW})
}

// ---- /api/ev_charging ----

func (s *Server) handleSetEVCharging(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PowerW float64 `json:"power_w"`
		Active bool    `json:"active"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.deps.CtrlMu.Lock()
	if req.Active {
		s.deps.Ctrl.EVChargingW = req.PowerW
	} else {
		s.deps.Ctrl.EVChargingW = 0
	}
	s.deps.CtrlMu.Unlock()
	writeJSON(w, 200, map[string]any{"status": "ok", "ev_charging_w": req.PowerW})
}

// ---- /api/drivers ----

func (s *Server) handleDrivers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.deps.Tel.AllHealth())
}

// ---- /api/battery_models ----

func (s *Server) handleGetModels(w http.ResponseWriter, r *http.Request) {
	s.deps.ModelsMu.Lock()
	defer s.deps.ModelsMu.Unlock()
	out := make(map[string]any, len(s.deps.Models))
	for name, m := range s.deps.Models {
		out[name] = map[string]any{
			"tau_s":                 m.TimeConstantS(s.deps.DtS),
			"gain":                  m.SteadyStateGain(),
			"deadband_w":            m.DeadbandW,
			"n_samples":             m.NSamples,
			"confidence":            m.Confidence(),
			"health_score":          m.HealthScore(),
			"health_drift_per_day":  m.HealthDriftPerDay(),
			"baseline_gain":         m.BaselineGain,
			"baseline_tau_s":        m.BaselineTauS,
			"last_calibrated_ts_ms": m.LastCalibrated,
			"last_updated_ts_ms":    m.LastUpdatedMs,
			"max_charge_curve":      m.MaxChargeCurve,
			"max_discharge_curve":   m.MaxDischargeCurve,
			"a":                     m.A,
			"b":                     m.B,
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleResetModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Battery string `json:"battery"`
		All     bool   `json:"all"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.deps.ModelsMu.Lock()
	defer s.deps.ModelsMu.Unlock()
	var reset []string
	if req.All {
		for name := range s.deps.Models {
			s.deps.Models[name] = battery.New(name)
			reset = append(reset, name)
		}
	} else if req.Battery != "" {
		if _, ok := s.deps.Models[req.Battery]; !ok {
			writeJSON(w, 404, map[string]string{"error": "battery not found: " + req.Battery})
			return
		}
		s.deps.Models[req.Battery] = battery.New(req.Battery)
		reset = append(reset, req.Battery)
	} else {
		writeJSON(w, 400, map[string]string{"error": "provide 'battery' or 'all'"})
		return
	}
	// Persist fresh models
	for _, name := range reset {
		if m, ok := s.deps.Models[name]; ok {
			if data, err := json.Marshal(m); err == nil {
				_ = s.deps.State.SaveBatteryModel(name, string(data))
			}
		}
	}
	writeJSON(w, 200, map[string]any{"reset": reset})
}

// ---- /api/self_tune/* ----

func (s *Server) handleSelfTuneStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Batteries []string `json:"batteries"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.deps.ModelsMu.Lock()
	err := s.deps.SelfTune.Start(req.Batteries, s.deps.Models, s.deps.DtS)
	s.deps.ModelsMu.Unlock()
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	slog.Info("self-tune started", "batteries", req.Batteries)
	writeJSON(w, 200, map[string]any{"status": "started", "batteries": req.Batteries})
}

func (s *Server) handleSelfTuneStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.deps.SelfTune.Status())
}

func (s *Server) handleSelfTuneCancel(w http.ResponseWriter, r *http.Request) {
	s.deps.SelfTune.Cancel()
	slog.Info("self-tune cancelled")
	writeJSON(w, 200, map[string]string{"status": "cancelled"})
}

// ---- /api/history ----

// handleHistory returns time-series points from state DB.
// Query params: range=5m|15m|1h|6h|24h|3d, points=N
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	rangeStr := r.URL.Query().Get("range")
	if rangeStr == "" { rangeStr = "5m" }
	pointsStr := r.URL.Query().Get("points")
	points := 200
	if pointsStr != "" {
		if n, err := strconv.Atoi(pointsStr); err == nil && n > 0 {
			points = n
		}
	}

	windowMs := parseRange(rangeStr)
	nowMs := time.Now().UnixMilli()
	since := nowMs - windowMs
	rows, err := s.deps.State.LoadHistory(since, nowMs, points)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		// Row.JSON is a blob from record_history; deserialize if valid
		var inner map[string]any
		if row.JSON != "" {
			_ = json.Unmarshal([]byte(row.JSON), &inner)
		}
		if inner == nil { inner = map[string]any{} }
		inner["ts"] = row.TsMs
		// Fill from flat columns for charting
		inner["grid_w"] = row.GridW
		inner["pv_w"] = row.PVW
		inner["bat_w"] = row.BatW
		inner["load_w"] = row.LoadW
		inner["bat_soc"] = row.BatSoC
		items = append(items, inner)
	}
	writeJSON(w, 200, map[string]any{"items": items, "range": rangeStr})
}

func parseRange(s string) int64 {
	switch s {
	case "5m": return 5 * 60 * 1000
	case "15m": return 15 * 60 * 1000
	case "1h": return 60 * 60 * 1000
	case "6h": return 6 * 60 * 60 * 1000
	case "24h": return 24 * 60 * 60 * 1000
	case "3d": return 3 * 24 * 60 * 60 * 1000
	}
	return 5 * 60 * 1000
}

// ---- static ----

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/" { path = "/index.html" }
	// Prevent path traversal
	clean := filepath.Clean(filepath.Join(s.deps.WebDir, path))
	absWeb, _ := filepath.Abs(s.deps.WebDir)
	absPath, _ := filepath.Abs(clean)
	if !strings.HasPrefix(absPath, absWeb) {
		writeJSON(w, 403, map[string]string{"error": "forbidden"})
		return
	}
	// Always-revalidate so version bumps land immediately
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	http.ServeFile(w, r, clean)
}
