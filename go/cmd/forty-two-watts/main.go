// forty-two-watts — Home Energy Management System.
// Go + WASM driver port. See /MIGRATION_PLAN.md.
//
// Don't Panic 🐬
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/api"
	"github.com/frahlg/forty-two-watts/go/internal/battery"
	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/configreload"
	"github.com/frahlg/forty-two-watts/go/internal/control"
	"github.com/frahlg/forty-two-watts/go/internal/drivers"
	"github.com/frahlg/forty-two-watts/go/internal/forecast"
	"github.com/frahlg/forty-two-watts/go/internal/ha"
	mqttcli "github.com/frahlg/forty-two-watts/go/internal/mqtt"
	modbuscli "github.com/frahlg/forty-two-watts/go/internal/modbus"
	"github.com/frahlg/forty-two-watts/go/internal/mpc"
	"github.com/frahlg/forty-two-watts/go/internal/prices"
	"github.com/frahlg/forty-two-watts/go/internal/selftune"
	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// Version gets injected at build time via -ldflags. Defaults to "dev" for
// local runs.
var Version = "dev"

func main() {
	configPath := flag.String("config", "config.yaml", "Path to config.yaml")
	webDir := flag.String("web", "web", "Path to static web UI directory")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	slog.Info("forty-two-watts starting", "version", Version, "config", *configPath)

	// ---- Load config ----
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}
	slog.Info("config loaded", "site", cfg.Site.Name, "drivers", len(cfg.Drivers))

	// ---- Open persistent state (SQLite) ----
	statePath := "state.db"
	if cfg.State != nil && cfg.State.Path != "" { statePath = cfg.State.Path }
	st, err := state.Open(statePath)
	if err != nil {
		slog.Error("open state", "err", err)
		os.Exit(1)
	}
	defer st.Close()
	_ = st.RecordEvent("startup")

	// ---- Telemetry store ----
	tel := telemetry.NewStore()

	// ---- Control state ----
	ctrl := control.NewState(cfg.Site.GridTargetW, cfg.Site.GridToleranceW, cfg.SiteMeterDriver())
	ctrl.SlewRateW = cfg.Site.SlewRateW
	ctrl.MinDispatchIntervalS = cfg.Site.MinDispatchIntervalS
	// Restore persisted mode + target if present
	if v, ok := st.LoadConfig("mode"); ok {
		switch control.Mode(v) {
		case control.ModeIdle, control.ModeSelfConsumption, control.ModePeakShaving,
			control.ModeCharge, control.ModePriority, control.ModeWeighted:
			ctrl.Mode = control.Mode(v)
		}
	}
	if v, ok := st.LoadConfig("grid_target_w"); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			ctrl.SetGridTarget(f)
		}
	}

	// ---- Driver capacities (site, for control + fuse guard) ----
	capacities := driverCapacitiesFrom(cfg.Drivers)

	// ---- Battery models — restore from SQLite + ensure one per driver ----
	models := make(map[string]*battery.Model)
	if stored, err := st.LoadAllBatteryModels(); err == nil {
		for name, js := range stored {
			m := &battery.Model{}
			if err := json.Unmarshal([]byte(js), m); err == nil {
				models[name] = m
				slog.Info("restored battery model",
					"name", name, "τ", m.TimeConstantS(float64(cfg.Site.ControlIntervalS)),
					"gain", m.SteadyStateGain(), "samples", m.NSamples)
			}
		}
	}
	for _, d := range cfg.Drivers {
		if d.BatteryCapacityWh > 0 && models[d.Name] == nil {
			models[d.Name] = battery.New(d.Name)
		}
	}

	// ---- Self-tune coordinator ----
	selfTune := selftune.NewCoordinator()

	// ---- WASM driver registry ----
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt := drivers.NewRuntime(ctx)
	defer rt.Close(ctx)
	reg := drivers.NewRegistry(rt, tel)
	reg.MQTTFactory = func(name string, c *config.MQTTConfig) (drivers.MQTTCap, error) {
		return mqttcli.Dial(c.Host, c.Port, c.Username, c.Password, "ftw-"+name)
	}
	reg.ModbusFactory = func(name string, c *config.ModbusConfig) (drivers.ModbusCap, error) {
		return modbuscli.Dial(c.Host, c.Port, c.UnitID)
	}
	// Spawn initial drivers
	for _, d := range cfg.Drivers {
		// Resolve relative WASM paths against config dir
		if d.WASM != "" && !filepath.IsAbs(d.WASM) {
			d.WASM = filepath.Join(filepath.Dir(*configPath), d.WASM)
		}
		if err := reg.Add(ctx, d); err != nil {
			slog.Warn("failed to spawn driver", "name", d.Name, "err", err)
		}
	}
	defer reg.ShutdownAll()

	// ---- Shared mutexes for API/control/models ----
	ctrlMu := &sync.Mutex{}
	capMu := &sync.RWMutex{}
	cfgMu := &sync.RWMutex{}
	modelsMu := &sync.Mutex{}

	// ---- Config hot-reload watcher ----
	watcher, err := configreload.New(*configPath, cfgMu, cfg, ctrlMu, ctrl,
		func(newCfg, oldCfg *config.Config) {
			// Resolve relative paths
			for i := range newCfg.Drivers {
				if newCfg.Drivers[i].WASM != "" && !filepath.IsAbs(newCfg.Drivers[i].WASM) {
					newCfg.Drivers[i].WASM = filepath.Join(filepath.Dir(*configPath), newCfg.Drivers[i].WASM)
				}
			}
			reg.Reload(ctx, newCfg.Drivers)
			// Refresh capacities
			capMu.Lock()
			capacities = driverCapacitiesFrom(newCfg.Drivers)
			_ = capacities // intentional — keep reference in scope
			capMu.Unlock()
		})
	if err != nil {
		slog.Warn("could not start config watcher", "err", err)
	} else {
		watcher.Start()
		defer watcher.Stop()
	}

	// ---- Spot prices + weather forecast (optional, nil if not configured) ----
	priceSvc := prices.FromConfig(cfg.Price, st)
	if priceSvc != nil {
		priceSvc.Start(ctx)
		defer priceSvc.Stop()
		slog.Info("price service started", "zone", priceSvc.Zone, "provider", priceSvc.Provider.Name())
	}

	// Sum rated PV from all drivers for the forecast estimator
	ratedPVW := 0.0
	for _, d := range cfg.Drivers {
		if d.BatteryCapacityWh > 0 {
			// crude: use battery capacity / 3 as rated PV proxy
			// users should set a real value via cfg.Weather if they care
			ratedPVW += d.BatteryCapacityWh / 3
		}
	}
	if ratedPVW == 0 { ratedPVW = 10000 } // 10 kW default
	forecastSvc := forecast.FromConfig(cfg.Weather, ratedPVW, st,
		"forty-two-watts/"+Version+" github.com/frahlg/forty-two-watts")
	if forecastSvc != nil {
		forecastSvc.Start(ctx)
		defer forecastSvc.Stop()
		slog.Info("forecast service started", "provider", forecastSvc.Provider.Name(),
			"lat", forecastSvc.Lat, "lon", forecastSvc.Lon, "rated_pv_w", ratedPVW)
	}

	// ---- Start MPC planner (optional) ----
	mpcSvc := buildMPC(cfg, st, tel, capacities)
	if mpcSvc != nil {
		mpcSvc.Start(ctx)
		defer mpcSvc.Stop()
		slog.Info("mpc planner started",
			"mode", mpcSvc.Defaults.Mode,
			"capacity_wh", mpcSvc.Defaults.CapacityWh,
			"horizon", mpcSvc.Horizon,
			"interval", mpcSvc.Interval)
	}

	// ---- Start HTTP API ----
	deps := &api.Deps{
		Tel: tel, Ctrl: ctrl, CtrlMu: ctrlMu,
		State: st,
		CapMu: capMu, Capacities: capacities,
		CfgMu: cfgMu, Cfg: cfg, ConfigPath: *configPath,
		Models: models, ModelsMu: modelsMu,
		SelfTune: selfTune,
		DtS:        float64(cfg.Site.ControlIntervalS),
		SaveConfig: config.SaveAtomic,
		WebDir:     *webDir,
		Prices:     priceSvc,
		Forecast:   forecastSvc,
		MPC:        mpcSvc,
		Version:    Version,
	}
	srv := api.New(deps)
	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.API.Port),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		slog.Info("HTTP API listening", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server", "err", err)
		}
	}()
	defer func() {
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	// ---- HA MQTT bridge (optional) ----
	var haBridge *ha.Bridge
	if cfg.HomeAssistant != nil && cfg.HomeAssistant.Enabled {
		cb := ha.CommandCallbacks{
			SetMode: func(m string) error {
				ctrlMu.Lock()
				defer ctrlMu.Unlock()
				switch control.Mode(m) {
				case control.ModeIdle, control.ModeSelfConsumption, control.ModePeakShaving,
					control.ModeCharge, control.ModePriority, control.ModeWeighted:
					ctrl.Mode = control.Mode(m)
					return st.SaveConfig("mode", m)
				}
				return fmt.Errorf("unknown mode: %s", m)
			},
			SetGridTarget: func(w float64) error {
				ctrlMu.Lock()
				defer ctrlMu.Unlock()
				ctrl.SetGridTarget(w)
				return st.SaveConfig("grid_target_w", strconv.FormatFloat(w, 'f', 1, 64))
			},
			SetPeakLimit: func(w float64) error {
				ctrlMu.Lock()
				defer ctrlMu.Unlock()
				ctrl.PeakLimitW = w
				return nil
			},
			SetEVCharging: func(w float64, active bool) error {
				ctrlMu.Lock()
				defer ctrlMu.Unlock()
				if active { ctrl.EVChargingW = w } else { ctrl.EVChargingW = 0 }
				return nil
			},
		}
		bridge, err := ha.Start(cfg.HomeAssistant, tel, ctrl, ctrlMu, reg.Names(), cb)
		if err != nil {
			slog.Warn("HA MQTT bridge failed to start", "err", err)
		} else {
			haBridge = bridge
			defer haBridge.Stop()
		}
	}

	// ---- Control loop ----
	controlInterval := time.Duration(cfg.Site.ControlIntervalS) * time.Second
	fuseMaxW := cfg.Fuse.MaxPowerW()
	dtS := float64(cfg.Site.ControlIntervalS)

	// Graceful shutdown
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)

	ticker := time.NewTicker(controlInterval)
	defer ticker.Stop()
	var saveCount uint64
	for {
		select {
		case <-sigc:
			slog.Info("shutting down")
			_ = st.RecordEvent("shutdown")
			return
		case <-ticker.C:
			nowMs := time.Now().UnixMilli()

			// ---- Continuous learning: feed (last_command, actual) per battery ----
			// Skip while self-tune is active — the override would corrupt RLS.
			if !selfTune.Status().Active {
				modelsMu.Lock()
				ctrlMu.Lock()
				lastTargets := append([]control.DispatchTarget{}, ctrl.LastTargets...)
				ctrlMu.Unlock()
				for _, t := range lastTargets {
					r := tel.Get(t.Driver, telemetry.DerBattery)
					if r == nil { continue }
					m, ok := models[t.Driver]
					if !ok { continue }
					soc := 0.5
					if r.SoC != nil { soc = *r.SoC }
					m.Update(t.TargetW, r.SmoothedW, soc, dtS, nowMs)
				}
				modelsMu.Unlock()
			}

			// ---- Self-tune tick ----
			if selfTune.Status().Active {
				modelsMu.Lock()
				selfTune.Tick(func(name string) (float64, float64, bool) {
					r := tel.Get(name, telemetry.DerBattery)
					if r == nil { return 0, 0, false }
					soc := 0.5
					if r.SoC != nil { soc = *r.SoC }
					return r.SmoothedW, soc, true
				}, models, dtS, nowMs)
				modelsMu.Unlock()
			}

			// ---- Compute dispatch ----
			capMu.RLock()
			capsSnap := make(map[string]float64, len(capacities))
			for k, v := range capacities { capsSnap[k] = v }
			capMu.RUnlock()

			ctrlMu.Lock()
			targets := control.ComputeDispatch(tel, ctrl, capsSnap, fuseMaxW)
			ctrlMu.Unlock()

			// ---- Self-tune override: force commanded battery, hold others at 0 ----
			finalTargets := targets
			if name, cmd, active := selfTune.CurrentCommand(); active {
				finalTargets = make([]control.DispatchTarget, 0, len(reg.Names()))
				for _, n := range reg.Names() {
					if n == name {
						finalTargets = append(finalTargets, control.DispatchTarget{Driver: n, TargetW: cmd})
					} else {
						finalTargets = append(finalTargets, control.DispatchTarget{Driver: n, TargetW: 0})
					}
				}
			}

			// ---- Dispatch to drivers ----
			for _, t := range finalTargets {
				payload, _ := json.Marshal(map[string]any{"action": "battery", "power_w": t.TargetW})
				if err := reg.Send(ctx, t.Driver, payload); err != nil {
					slog.Warn("driver send", "name", t.Driver, "err", err)
				}
			}

			// ---- Record history snapshot ----
			recordHistory(st, tel, ctrl, nowMs)

			// ---- Periodic battery-model persistence (every 12 cycles ≈ 60s) ----
			saveCount++
			if saveCount%12 == 0 {
				modelsMu.Lock()
				for name, m := range models {
					if data, err := json.Marshal(m); err == nil {
						_ = st.SaveBatteryModel(name, string(data))
					}
				}
				modelsMu.Unlock()
			}
		}
	}
}

func driverCapacitiesFrom(drivers []config.Driver) map[string]float64 {
	out := make(map[string]float64, len(drivers))
	for _, d := range drivers {
		if d.BatteryCapacityWh > 0 {
			out[d.Name] = d.BatteryCapacityWh
		}
	}
	return out
}

// buildMPC constructs a planner from config. Returns nil if disabled,
// if prices aren't configured, or if there are no batteries with capacity.
func buildMPC(cfg *config.Config, st *state.Store, tel *telemetry.Store, capacities map[string]float64) *mpc.Service {
	if cfg.Planner == nil || !cfg.Planner.Enabled {
		return nil
	}
	if cfg.Price == nil || cfg.Price.Provider == "" || cfg.Price.Provider == "none" {
		slog.Warn("mpc requires price provider — skipping")
		return nil
	}
	var totalCap, maxChg, maxDis float64
	for _, d := range cfg.Drivers {
		cap := capacities[d.Name]
		if cap <= 0 {
			continue
		}
		totalCap += cap
		// Default max (de)charge = 0.5C unless overridden
		defaultP := cap / 2
		chg := defaultP
		dis := defaultP
		if b, ok := cfg.Batteries[d.Name]; ok {
			if b.MaxChargeW != nil {
				chg = *b.MaxChargeW
			}
			if b.MaxDischargeW != nil {
				dis = *b.MaxDischargeW
			}
		}
		maxChg += chg
		maxDis += dis
	}
	if totalCap <= 0 {
		slog.Warn("mpc: no battery capacity — skipping")
		return nil
	}
	pl := cfg.Planner
	zone := "SE3"
	if cfg.Price != nil && cfg.Price.Zone != "" {
		zone = cfg.Price.Zone
	}
	mode := mpc.Mode(pl.Mode)
	if mode == "" {
		mode = mpc.ModeSelfConsumption
	}
	socMin := pl.SoCMinPct
	if socMin <= 0 {
		socMin = 10
	}
	socMax := pl.SoCMaxPct
	if socMax <= 0 || socMax > 100 {
		socMax = 95
	}
	chgEff := pl.ChargeEfficiency
	if chgEff <= 0 {
		chgEff = 0.95
	}
	disEff := pl.DischargeEfficiency
	if disEff <= 0 {
		disEff = 0.95
	}
	params := mpc.Params{
		Mode:                mode,
		SoCLevels:           41,
		CapacityWh:          totalCap,
		SoCMinPct:           socMin,
		SoCMaxPct:           socMax,
		InitialSoCPct:       50,
		ActionLevels:        21,
		MaxChargeW:          maxChg,
		MaxDischargeW:       maxDis,
		ChargeEfficiency:    chgEff,
		DischargeEfficiency: disEff,
		ExportOrePerKWh:     pl.ExportOrePerKWh,
	}
	svc := mpc.New(st, tel, zone, params)
	svc.BaseLoad = pl.BaseLoadW
	if pl.HorizonHours > 0 {
		svc.Horizon = time.Duration(pl.HorizonHours) * time.Hour
	}
	if pl.IntervalMin > 0 {
		svc.Interval = time.Duration(pl.IntervalMin) * time.Minute
	}
	return svc
}

func recordHistory(st *state.Store, tel *telemetry.Store, ctrl *control.State, nowMs int64) {
	gridW := 0.0
	if r := tel.Get(ctrl.SiteMeterDriver, telemetry.DerMeter); r != nil {
		gridW = r.SmoothedW
	}
	var pvW, batW, sumSoC, totalCap float64
	var socCount int
	for _, r := range tel.ReadingsByType(telemetry.DerPV) { pvW += r.SmoothedW }
	for _, r := range tel.ReadingsByType(telemetry.DerBattery) {
		batW += r.SmoothedW
		if r.SoC != nil {
			sumSoC += *r.SoC
			socCount++
		}
		_ = totalCap
	}
	avgSoC := 0.0
	if socCount > 0 { avgSoC = sumSoC / float64(socCount) }
	loadW := gridW - batW - pvW
	if loadW < 0 { loadW = 0 }
	_ = st.RecordHistory(state.HistoryPoint{
		TsMs: nowMs, GridW: gridW, PVW: pvW, BatW: batW, LoadW: loadW, BatSoC: avgSoC,
		JSON: "{}",
	})
}
