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
	"github.com/frahlg/forty-two-watts/go/internal/drivers"
	"github.com/frahlg/forty-two-watts/go/internal/forecast"
	"github.com/frahlg/forty-two-watts/go/internal/ha"
	"github.com/frahlg/forty-two-watts/go/internal/loadmodel"
	"github.com/frahlg/forty-two-watts/go/internal/mpc"
	"github.com/frahlg/forty-two-watts/go/internal/prices"
	"github.com/frahlg/forty-two-watts/go/internal/pvmodel"
	"github.com/frahlg/forty-two-watts/go/internal/selftune"
	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

const (
	// evPasswordKey is the state.db key for the EV charger password
	// (stored outside config.yaml for security).
	evPasswordKey = "ev_charger_password"
	// maskedPlaceholder is sent to the UI to indicate a password is set
	// without revealing the actual value.
	maskedPlaceholder = "••••••••"
)

// Deps is the full set of runtime dependencies the API handlers need.
// One instance is shared across all handlers; mutations use the contained
// mutexes from each package.
type Deps struct {
	Tel        *telemetry.Store
	Ctrl       *control.State
	CtrlMu     *sync.Mutex
	State      *state.Store
	CapMu      *sync.RWMutex
	Capacities map[string]float64 // driver → battery_capacity_wh
	CfgMu      *sync.RWMutex
	Cfg        *config.Config
	ConfigPath string
	Models     map[string]*battery.Model
	ModelsMu   *sync.Mutex
	SelfTune   *selftune.Coordinator
	DtS        float64                                   // control interval seconds (for model τ / age displays)
	SaveConfig func(path string, c *config.Config) error // injection for testability
	WebDir     string                                    // static assets root (default "web")

	// Optional: spot prices + weather forecast services. Nil if disabled.
	Prices   *prices.Service
	Forecast *forecast.Service

	// Optional: MPC planner. Nil if disabled.
	MPC *mpc.Service

	// Optional: PV digital-twin self-learner.
	PVModel *pvmodel.Service

	// Optional: load digital-twin self-learner.
	LoadModel *loadmodel.Service

	// Optional: HA MQTT bridge (nil if disabled).
	HA *ha.Bridge

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
	if deps.Version == "" {
		deps.Version = "dev"
	}
	if deps.WebDir == "" {
		deps.WebDir = "web"
	}
	s := &Server{deps: deps, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler returns the http.Handler suitable for `http.ListenAndServe`.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	// ---- JSON endpoints ----
	s.handle("GET  /api/health", s.handleHealth)
	s.handle("GET  /api/status", s.handleStatus)
	s.handle("GET  /api/config", s.handleGetConfig)
	s.handle("POST /api/config", s.handlePostConfig)
	s.handle("GET  /api/mode", s.handleGetMode)
	s.handle("POST /api/mode", s.handleSetMode)
	s.handle("POST /api/target", s.handleSetTarget)
	s.handle("POST /api/peak_limit", s.handleSetPeakLimit)
	s.handle("POST /api/ev_charging", s.handleSetEVCharging)
	s.handle("GET  /api/drivers", s.handleDrivers)
	s.handle("GET  /api/drivers/catalog", s.handleDriversCatalog)
	s.handle("GET  /api/ha/status", s.handleHAStatus)
	s.handle("GET  /api/battery_models", s.handleGetModels)
	s.handle("POST /api/battery_models/reset", s.handleResetModel)
	s.handle("POST /api/self_tune/start", s.handleSelfTuneStart)
	s.handle("GET  /api/self_tune/status", s.handleSelfTuneStatus)
	s.handle("POST /api/self_tune/cancel", s.handleSelfTuneCancel)
	s.handle("GET  /api/history", s.handleHistory)
	s.handle("GET  /api/prices", s.handlePrices)
	s.handle("GET  /api/forecast", s.handleForecast)
	s.handle("GET  /api/mpc/plan", s.handleMPCPlan)
	s.handle("POST /api/mpc/replan", s.handleMPCReplan)
	s.handle("GET  /api/pvmodel", s.handlePVModel)
	s.handle("POST /api/pvmodel/reset", s.handlePVModelReset)
	s.handle("GET  /api/loadmodel", s.handleLoadModel)
	s.handle("POST /api/loadmodel/reset", s.handleLoadModelReset)
	s.handle("GET  /api/series", s.handleSeries)
	s.handle("GET  /api/series/catalog", s.handleSeriesCatalog)
	s.handle("GET  /api/devices", s.handleDevices)

	// ---- Static web UI ----
	// Everything not matched above falls through to the static server.
	s.mux.HandleFunc("/", s.handleStatic)
}

// handle wires "METHOD path" to a handler. Uses Go 1.22+ method-scoped
// routing so GET + POST on the same path can be registered independently.
func (s *Server) handle(methodPath string, h http.HandlerFunc) {
	parts := strings.SplitN(strings.TrimSpace(methodPath), " ", 2)
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
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
	if err != nil {
		return err
	}
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
	if off > 0 {
		status = "degraded"
	}
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
	for k, v := range s.deps.Capacities {
		caps[k] = v
	}
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
	if loadW < 0 {
		loadW = 0
	}

	// Weighted average SoC by capacity
	var totalCap, weightedSoC float64
	for _, b := range s.deps.Tel.ReadingsByType(telemetry.DerBattery) {
		cap, ok := caps[b.Driver]
		if !ok {
			continue
		}
		totalCap += cap
		soc := 0.0
		if b.SoC != nil {
			soc = *b.SoC
		}
		weightedSoC += soc * cap
	}
	avgSoC := 0.0
	if totalCap > 0 {
		avgSoC = weightedSoC / totalCap
	}

	// Per-driver details
	drivers := make(map[string]any)
	for name, h := range s.deps.Tel.AllHealth() {
		d := map[string]any{
			"status":             h.Status.String(),
			"consecutive_errors": h.ConsecutiveErrors,
			"tick_count":         h.TickCount,
		}
		if h.LastError != "" {
			d["last_error"] = h.LastError
		}
		if r := s.deps.Tel.Get(name, telemetry.DerMeter); r != nil {
			d["meter_w"] = r.SmoothedW
		}
		if r := s.deps.Tel.Get(name, telemetry.DerPV); r != nil {
			d["pv_w"] = r.SmoothedW
		}
		if r := s.deps.Tel.Get(name, telemetry.DerBattery); r != nil {
			d["bat_w"] = r.SmoothedW
			if r.SoC != nil {
				d["bat_soc"] = *r.SoC
			}
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

	var pvPredictW, loadPredictW float64
	if s.deps.PVModel != nil {
		pvPredictW = -s.deps.PVModel.PredictNow() // site-sign: negative
	}
	if s.deps.LoadModel != nil {
		loadPredictW = s.deps.LoadModel.Predict(time.Now())
	}

	// Energy today: integrate history points since midnight local time.
	// Each point is ~5 s apart; multiply W × dt_hours for Wh per interval.
	var energyToday map[string]any
	if s.deps.State != nil {
		now := time.Now()
		midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		pts, err := s.deps.State.LoadHistory(midnight.UnixMilli(), now.UnixMilli(), 0)
		if err == nil && len(pts) > 1 {
			var importWh, exportWh, pvWh, chargedWh, dischargedWh, loadWh float64
			for i := 1; i < len(pts); i++ {
				dtH := float64(pts[i].TsMs-pts[i-1].TsMs) / 3_600_000.0
				g := pts[i].GridW
				if g > 0 {
					importWh += g * dtH
				} else {
					exportWh += -g * dtH
				}
				pvWh += -pts[i].PVW * dtH
				if pts[i].BatW > 0 {
					chargedWh += pts[i].BatW * dtH
				} else {
					dischargedWh += -pts[i].BatW * dtH
				}
				loadWh += pts[i].LoadW * dtH
			}
			energyToday = map[string]any{
				"import_wh":       importWh,
				"export_wh":       exportWh,
				"pv_wh":           pvWh,
				"bat_charged_wh":  chargedWh,
				"bat_discharged_wh": dischargedWh,
				"load_wh":         loadWh,
			}
		}
	}

	// Fuse + site meter details (used by the dashboard to render per-phase
	// amperage bars). We expose the fuse config verbatim so the frontend
	// doesn't need a second /api/config fetch, and pull per-phase readings
	// from the site meter driver's raw emit payload.
	s.deps.CfgMu.RLock()
	fuseCfg := map[string]any{
		"max_amps": s.deps.Cfg.Fuse.MaxAmps,
		"phases":   s.deps.Cfg.Fuse.Phases,
		"voltage":  s.deps.Cfg.Fuse.Voltage,
	}
	s.deps.CfgMu.RUnlock()

	phaseAmps := siteMeterPhaseAmps(s.deps.Tel, ctrl.SiteMeterDriver)

	resp := map[string]any{
		"version":          s.deps.Version,
		"mode":             ctrl.Mode,
		"plan_stale":       ctrl.PlanStale,
		"grid_w":           gridW,
		"pv_w":             pvW,
		"pv_w_predicted":   pvPredictW,
		"bat_w":            batW,
		"load_w":           loadW,
		"load_w_predicted": loadPredictW,
		"bat_soc":          avgSoC,
		"grid_target_w":    ctrl.GridTargetW,
		"peak_limit_w":     ctrl.PeakLimitW,
		"ev_charging_w":    ctrl.EVChargingW,
		"fuse":             fuseCfg,
		"phase_amps":       phaseAmps,
		"drivers":          drivers,
		"dispatch":         dispatch,
	}
	if energyToday != nil {
		resp["energy"] = map[string]any{"today": energyToday}
	}
	writeJSON(w, 200, resp)
}

// siteMeterPhaseAmps pulls per-phase L1/L2/L3 current (in amps) from the
// site meter driver's emit payload. Returns an empty slice if the site
// meter isn't reporting per-phase data — the frontend falls back to a
// total-amps bar in that case. Signed: negative = export on that phase.
func siteMeterPhaseAmps(tel *telemetry.Store, siteMeter string) []float64 {
	if siteMeter == "" { return nil }
	r := tel.Get(siteMeter, telemetry.DerMeter)
	if r == nil || len(r.Data) == 0 { return nil }
	var payload struct {
		L1A *float64 `json:"l1_a"`
		L2A *float64 `json:"l2_a"`
		L3A *float64 `json:"l3_a"`
	}
	if err := json.Unmarshal(r.Data, &payload); err != nil { return nil }
	out := make([]float64, 0, 3)
	if payload.L1A != nil { out = append(out, *payload.L1A) }
	if payload.L2A != nil { out = append(out, *payload.L2A) }
	if payload.L3A != nil { out = append(out, *payload.L3A) }
	return out
}

// ---- /api/config ----

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	s.deps.CfgMu.RLock()
	cfg := *s.deps.Cfg
	s.deps.CfgMu.RUnlock()
	masked := cfg.MaskSecrets()
	// EV charger password lives in state.db, not YAML. Signal to the UI
	// that a password is set by using a masked placeholder (MaskSecrets
	// blanked it to "").
	if masked.EVCharger != nil {
		if pw, ok := s.deps.State.LoadConfig(evPasswordKey); ok && pw != "" {
			cp := *masked.EVCharger
			cp.Password = maskedPlaceholder
			masked.EVCharger = &cp
		}
	}
	writeJSON(w, 200, masked)
}

func (s *Server) handlePostConfig(w http.ResponseWriter, r *http.Request) {
	var newCfg config.Config
	if err := readJSON(r, &newCfg); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid config: " + err.Error()})
		return
	}
	// Preserve secrets the UI sent back as empty (masked) values.
	s.deps.CfgMu.RLock()
	newCfg.PreserveMaskedSecrets(s.deps.Cfg)
	s.deps.CfgMu.RUnlock()

	// EV charger password: persist to state.db instead of config.yaml.
	// Empty or the masked placeholder means "keep existing"; a new value
	// means the user typed a real password.
	if newCfg.EVCharger != nil {
		pw := newCfg.EVCharger.Password
		if pw != "" && pw != maskedPlaceholder {
			if err := s.deps.State.SaveConfig(evPasswordKey, pw); err != nil {
				slog.Warn("failed to persist ev_charger_password", "err", err)
			}
		}
		// Restore the real password into the in-memory config so
		// InjectEVChargerDriver (called by the config-reload watcher)
		// sees it.
		if stored, ok := s.deps.State.LoadConfig(evPasswordKey); ok {
			newCfg.EVCharger.Password = stored
		}
	}

	if err := newCfg.Validate(); err != nil {
		writeJSON(w, 400, map[string]string{"error": "validation: " + err.Error()})
		return
	}
	// Persist atomically (Password has yaml:"-" so it won't appear in YAML)
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
	m := control.Mode(req.Mode)
	// Validate mode string
	switch m {
	case control.ModeIdle, control.ModeSelfConsumption, control.ModePeakShaving,
		control.ModeCharge, control.ModePriority, control.ModeWeighted,
		control.ModePlannerSelf, control.ModePlannerCheap, control.ModePlannerArbitrage:
		s.deps.CtrlMu.Lock()
		s.deps.Ctrl.Mode = m
		s.deps.CtrlMu.Unlock()
		if err := s.deps.State.SaveConfig("mode", req.Mode); err != nil {
			slog.Warn("failed to persist mode", "err", err)
		}
		// Propagate to MPC if switching to a planner mode. Map
		// control.ModePlanner* → mpc.Mode and force an immediate replan.
		if m.IsPlannerMode() && s.deps.MPC != nil {
			var mm mpc.Mode
			switch m {
			case control.ModePlannerSelf:
				mm = mpc.ModeSelfConsumption
			case control.ModePlannerCheap:
				mm = mpc.ModeCheapCharge
			case control.ModePlannerArbitrage:
				mm = mpc.ModeArbitrage
			}
			s.deps.MPC.SetMode(r.Context(), mm)
		}
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
	if err := s.deps.State.SaveConfig("grid_target_w", strconv.FormatFloat(req.GridTargetW, 'f', 1, 64)); err != nil {
		slog.Warn("failed to persist grid_target_w", "err", err)
	}
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

// GET /api/drivers/catalog — list of available drivers from the
// drivers/ directory, parsed from each .lua file's DRIVER metadata.
// Used by the Settings UI to offer an "Add from catalog" dropdown.
func (s *Server) handleDriversCatalog(w http.ResponseWriter, r *http.Request) {
	// Catalog lives next to the config file by convention.
	dir := filepath.Join(filepath.Dir(s.deps.ConfigPath), "drivers")
	entries, err := drivers.LoadCatalog(dir)
	if err != nil {
		writeJSON(w, 200, map[string]any{"path": dir, "entries": []any{}, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"path": dir, "entries": entries})
}

// GET /api/ha/status — is the HA MQTT bridge connected?
// Used by the Settings UI to show a live connection indicator
// instead of silently relying on "it's saved".
func (s *Server) handleHAStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps.HA == nil {
		writeJSON(w, 200, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, 200, map[string]any{
		"enabled":           true,
		"connected":         s.deps.HA.IsConnected(),
		"broker":            s.deps.HA.BrokerAddr(),
		"last_publish_ms":   s.deps.HA.LastPublishMs(),
		"sensors_announced": s.deps.HA.SensorsAnnounced(),
	})
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
				if err := s.deps.State.SaveBatteryModel(name, string(data)); err != nil {
				slog.Warn("failed to persist battery model", "battery", name, "err", err)
			}
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
	if rangeStr == "" {
		rangeStr = "5m"
	}
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
		if inner == nil {
			inner = map[string]any{}
		}
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
	case "5m":
		return 5 * 60 * 1000
	case "15m":
		return 15 * 60 * 1000
	case "1h":
		return 60 * 60 * 1000
	case "6h":
		return 6 * 60 * 60 * 1000
	case "24h":
		return 24 * 60 * 60 * 1000
	case "48h":
		return 48 * 60 * 60 * 1000
	case "3d":
		return 3 * 24 * 60 * 60 * 1000
	}
	return 5 * 60 * 1000
}

// ---- /api/prices ----
//
// Query params:
//
//	range=24h|48h|3d  — window starting NOW unless since_ms given
//	since_ms=…        — explicit start
//	until_ms=…        — explicit end (default: now + 48h)
//
// Response: {"zone": "...", "items": [{slot_ts_ms, slot_len_min, spot_ore_kwh, total_ore_kwh, ...}]}
func (s *Server) handlePrices(w http.ResponseWriter, r *http.Request) {
	if s.deps.Prices == nil {
		writeJSON(w, 200, map[string]any{"items": []any{}, "enabled": false})
		return
	}
	q := r.URL.Query()
	nowMs := time.Now().UnixMilli()
	var since, until int64
	since = nowMs - 1*3600*1000  // default 1h lookback
	until = nowMs + 48*3600*1000 // default 48h lookahead
	if v := q.Get("since_ms"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			since = n
		}
	}
	if v := q.Get("until_ms"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			until = n
		}
	}
	if rng := q.Get("range"); rng != "" {
		since = nowMs
		until = nowMs + parseRange(rng)
	}
	rows, err := s.deps.Prices.Load(since, until)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"zone":    s.deps.Prices.Zone,
		"items":   rows,
		"enabled": true,
	})
}

// ---- /api/forecast ----
//
// Query params: range=24h|48h|3d (default 48h lookahead).
// Response: {"items": [{slot_ts_ms, cloud_cover_pct, temp_c, pv_w_estimated, ...}]}
func (s *Server) handleForecast(w http.ResponseWriter, r *http.Request) {
	if s.deps.Forecast == nil {
		writeJSON(w, 200, map[string]any{"items": []any{}, "enabled": false})
		return
	}
	q := r.URL.Query()
	nowMs := time.Now().UnixMilli()
	since, until := nowMs-time.Hour.Milliseconds(), nowMs+48*3600*1000
	if rng := q.Get("range"); rng != "" {
		since = nowMs
		until = nowMs + parseRange(rng)
	}
	rows, err := s.deps.Forecast.Load(since, until)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"items": rows, "enabled": true})
}

// ---- MPC planner ----

func (s *Server) handleMPCPlan(w http.ResponseWriter, r *http.Request) {
	if s.deps.MPC == nil {
		writeJSON(w, 200, map[string]any{"enabled": false})
		return
	}
	plan := s.deps.MPC.Latest()
	at, reason := s.deps.MPC.LastReplanInfo()
	meta := map[string]any{
		"last_replan_ms":     at.UnixMilli(),
		"last_replan_reason": reason,
	}
	if plan == nil {
		writeJSON(w, 200, map[string]any{"enabled": true, "plan": nil, "meta": meta})
		return
	}
	writeJSON(w, 200, map[string]any{"enabled": true, "plan": plan, "meta": meta})
}

func (s *Server) handleMPCReplan(w http.ResponseWriter, r *http.Request) {
	if s.deps.MPC == nil {
		writeJSON(w, 400, map[string]string{"error": "mpc disabled"})
		return
	}
	plan := s.deps.MPC.Replan(r.Context())
	writeJSON(w, 200, map[string]any{"enabled": true, "plan": plan})
}

// ---- Long-format time-series ----

// handleSeries: GET /api/series?driver=ferroamp&metric=battery_w&range=1h&points=600
// Returns one metric's time series for one driver. Useful for the metric
// browser UI and for ML training data exports.
func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	driver := r.URL.Query().Get("driver")
	metric := r.URL.Query().Get("metric")
	if driver == "" || metric == "" {
		writeJSON(w, 400, map[string]string{"error": "driver and metric are required"})
		return
	}
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "1h"
	}
	points := 0
	if p := r.URL.Query().Get("points"); p != "" {
		if v, err := strconv.Atoi(p); err == nil {
			points = v
		}
	}
	windowMs := parseRange(rng)
	now := time.Now().UnixMilli()
	rows, err := s.deps.State.LoadSeries(driver, metric, now-windowMs, now, points)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	out := make([]map[string]any, len(rows))
	for i, sm := range rows {
		out[i] = map[string]any{"ts": sm.TsMs, "v": sm.Value}
	}
	writeJSON(w, 200, map[string]any{
		"driver": driver, "metric": metric, "range": rng, "points": out,
	})
}

// handleSeriesCatalog: GET /api/series/catalog
// Lists the (driver, metric) tuples that have any samples recorded. UIs
// use this to enumerate available signals for charting / debugging.
func (s *Server) handleSeriesCatalog(w http.ResponseWriter, r *http.Request) {
	drivers, err := s.deps.State.DriverNames()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	metrics, err := s.deps.State.MetricNames()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"drivers": drivers,
		"metrics": metrics,
	})
}

// handleDevices: GET /api/devices
// Returns every registered device with its hardware-stable identity. UIs
// surface this in driver cards (small "SN: ABC" line) and in Settings →
// Devices so the operator can see how each driver is identified.
func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	devs, err := s.deps.State.AllDevices()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	out := make([]map[string]any, len(devs))
	for i, d := range devs {
		out[i] = map[string]any{
			"device_id":     d.DeviceID,
			"driver_name":   d.DriverName,
			"make":          d.Make,
			"serial":        d.Serial,
			"mac":           d.MAC,
			"endpoint":      d.Endpoint,
			"first_seen_ms": d.FirstSeenMs,
			"last_seen_ms":  d.LastSeenMs,
		}
	}
	writeJSON(w, 200, map[string]any{"devices": out})
}

// ---- PV digital twin ----

func (s *Server) handlePVModel(w http.ResponseWriter, r *http.Request) {
	if s.deps.PVModel == nil {
		writeJSON(w, 200, map[string]any{"enabled": false})
		return
	}
	m := s.deps.PVModel.Model()
	writeJSON(w, 200, map[string]any{
		"enabled":    true,
		"samples":    m.Samples,
		"mae_w":      m.MAE,
		"rated_w":    m.RatedW,
		"quality":    m.Quality(),
		"last_ms":    m.LastMs,
		"forgetting": m.Forgetting,
		"beta":       m.Beta,
	})
}

func (s *Server) handlePVModelReset(w http.ResponseWriter, r *http.Request) {
	if s.deps.PVModel == nil {
		writeJSON(w, 400, map[string]string{"error": "pvmodel disabled"})
		return
	}
	s.deps.PVModel.Reset()
	writeJSON(w, 200, map[string]string{"status": "reset"})
}

// ---- Load digital twin ----

func (s *Server) handleLoadModel(w http.ResponseWriter, r *http.Request) {
	if s.deps.LoadModel == nil {
		writeJSON(w, 200, map[string]any{"enabled": false})
		return
	}
	m := s.deps.LoadModel.Model()
	// Count warmed-up buckets (≥ MinTrustSamples).
	warm := 0
	for i := 0; i < loadmodel.Buckets; i++ {
		if m.Bucket[i].Samples >= loadmodel.MinTrustSamples {
			warm++
		}
	}
	writeJSON(w, 200, map[string]any{
		"enabled":            true,
		"samples":            m.Samples,
		"mae_w":              m.MAE,
		"peak_w":             m.PeakW,
		"quality":            m.Quality(),
		"last_ms":            m.LastMs,
		"heating_w_per_degc": m.HeatingW_per_degC,
		"buckets_warm":       warm,
		"buckets_total":      loadmodel.Buckets,
	})
}

func (s *Server) handleLoadModelReset(w http.ResponseWriter, r *http.Request) {
	if s.deps.LoadModel == nil {
		writeJSON(w, 400, map[string]string{"error": "loadmodel disabled"})
		return
	}
	s.deps.LoadModel.Reset()
	writeJSON(w, 200, map[string]string{"status": "reset"})
}

// ---- static ----

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/" {
		path = "/index.html"
	}
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
