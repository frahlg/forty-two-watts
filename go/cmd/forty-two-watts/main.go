// forty-two-watts — Home Energy Management System.
//
// Don't Panic 🐬
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/api"
	"github.com/frahlg/forty-two-watts/go/internal/battery"
	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/configreload"
	"github.com/frahlg/forty-two-watts/go/internal/control"
	"github.com/frahlg/forty-two-watts/go/internal/currency"
	"github.com/frahlg/forty-two-watts/go/internal/arp"
	"github.com/frahlg/forty-two-watts/go/internal/drivers"
	"github.com/frahlg/forty-two-watts/go/internal/forecast"
	"github.com/frahlg/forty-two-watts/go/internal/ha"
	"github.com/frahlg/forty-two-watts/go/internal/loadmodel"
	mqttcli "github.com/frahlg/forty-two-watts/go/internal/mqtt"
	modbuscli "github.com/frahlg/forty-two-watts/go/internal/modbus"
	"github.com/frahlg/forty-two-watts/go/internal/mpc"
	"github.com/frahlg/forty-two-watts/go/internal/ocpp"
	"github.com/frahlg/forty-two-watts/go/internal/priceforecast"
	"github.com/frahlg/forty-two-watts/go/internal/prices"
	"github.com/frahlg/forty-two-watts/go/internal/pvmodel"
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
	driverDirFlag := flag.String("drivers", "", "Path to drivers directory (default: <config-dir>/drivers)")
	flag.Parse()

	// Drivers default to a sibling of the config file (historical layout:
	// config.yaml + drivers/ + seed/ + state.db all under one dir). Docker
	// breaks that convention because /app/data is a host bind mount while
	// drivers are baked into the image at /app/drivers — the flag lets the
	// CMD point at the immutable image location.
	resolveDriverDir := func() string {
		if *driverDirFlag != "" {
			return *driverDirFlag
		}
		return filepath.Join(filepath.Dir(*configPath), "drivers")
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	slog.Info("forty-two-watts starting", "version", Version, "config", *configPath)

	// Route "drivers/<name>.lua" path resolution through the drivers dir
	// (from -drivers). Picked up by both the initial Load below and every
	// subsequent reload via the file watcher.
	config.DriversDirOverride = resolveDriverDir()

	// ---- Load config ----
	cfg, err := config.Load(*configPath)
	if err != nil {
		if isConfigMissing(err) {
			runBootstrap(*configPath, *webDir, resolveDriverDir())
			return
		}
		slog.Error("load config", "err", err)
		os.Exit(1)
	}
	slog.Info("config loaded", "site", cfg.Site.Name, "drivers", len(cfg.Drivers))

	// ---- Open persistent state (SQLite) ----
	statePath := "state.db"
	coldDir := "cold"
	if cfg.State != nil {
		if cfg.State.Path != "" { statePath = cfg.State.Path }
		if cfg.State.ColdDir != "" { coldDir = cfg.State.ColdDir }
	}
	st, err := state.Open(statePath)
	if err != nil {
		slog.Error("open state", "err", err)
		os.Exit(1)
	}
	defer st.Close()
	if err := st.RecordEvent("startup"); err != nil {
		slog.Warn("failed to persist startup event", "err", err)
	}

	// ---- Restore EV charger password from state.db (not stored in YAML) ----
	if cfg.EVCharger != nil {
		if pw, ok := st.LoadConfig("ev_charger_password"); ok {
			cfg.EVCharger.Password = pw
		}
	}

	// ---- Telemetry store ----
	tel := telemetry.NewStore()

	// ---- Control state ----
	ctrl := control.NewState(cfg.Site.GridTargetW, cfg.Site.GridToleranceW, cfg.SiteMeterDriver())
	ctrl.SlewRateW = cfg.Site.SlewRateW
	ctrl.MinDispatchIntervalS = cfg.Site.MinDispatchIntervalS
	// Restore persisted mode + target if present. The planner variants
	// have to be listed too — without them the strategy the user picked in
	// the UI (planner_self / planner_cheap / planner_arbitrage) is silently
	// dropped on restart and the dashboard appears to forget the selection.
	if v, ok := st.LoadConfig("mode"); ok {
		switch control.Mode(v) {
		case control.ModeIdle, control.ModeSelfConsumption, control.ModePeakShaving,
			control.ModeCharge, control.ModePriority, control.ModeWeighted,
			control.ModePlannerSelf, control.ModePlannerCheap, control.ModePlannerArbitrage:
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

	// ---- Driver registry ----
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg := drivers.NewRegistry(tel)
	reg.MQTTFactory = func(name string, c *config.MQTTConfig) (drivers.MQTTCap, error) {
		return mqttcli.Dial(c.Host, c.Port, c.Username, c.Password, "ftw-"+name)
	}
	reg.ModbusFactory = func(name string, c *config.ModbusConfig) (drivers.ModbusCap, error) {
		return modbuscli.Dial(c.Host, c.Port, c.UnitID)
	}
	reg.ARPLookup = arp.Lookup
	// Spawn initial drivers. config.Load has already joined relative Lua
	// paths with the config directory — nothing to resolve here.
	for _, d := range cfg.Drivers {
		if d.Disabled {
			slog.Info("driver skipped (disabled)", "name", d.Name)
			continue
		}
		if err := reg.Add(ctx, d); err != nil {
			slog.Warn("failed to spawn driver", "name", d.Name, "err", err)
		}
	}
	defer reg.ShutdownAll()

	// ---- Identity bootstrap ----
	// Drivers report make/serial inside driver_init via host.set_make / set_sn,
	// and we resolved endpoint+MAC at registry-Add time. Now we wait briefly
	// for those to populate, then register each device + run the one-shot
	// migration that re-keys legacy battery_models from driver-name to
	// device_id. Subsequent runs are no-ops.
	go func() {
		time.Sleep(3 * time.Second) // let driver_init finish + first SN be reported
		registerAllDevices(st, reg)
		if migrated, err := st.MigrateBatteryModelKeys(); err != nil {
			slog.Warn("battery model key migration failed", "err", err)
		} else if migrated > 0 {
			slog.Info("battery model keys migrated to device_id", "count", migrated)
		}
	}()

	// ---- Shared mutexes for API/control/models ----
	ctrlMu := &sync.Mutex{}
	capMu := &sync.RWMutex{}
	cfgMu := &sync.RWMutex{}
	modelsMu := &sync.Mutex{}

	// Pre-declare services that the hot-reload Applier needs to touch.
	// The Applier closure captures these by reference; they're assigned
	// further down when their packages are wired, and the Applier only
	// ever fires after `watcher.Start()` — by which point everything is
	// in place.
	var pvSvc *pvmodel.Service
	var forecastSvc *forecast.Service

	// ---- Config hot-reload watcher ----
	watcher, err := configreload.New(*configPath, cfgMu, cfg, ctrlMu, ctrl,
		func(newCfg, oldCfg *config.Config) {
			// Restore EV charger password from state.db (not in YAML).
			if newCfg.EVCharger != nil {
				if pw, ok := st.LoadConfig("ev_charger_password"); ok {
					newCfg.EVCharger.Password = pw
				}
			}
			// Driver paths are already resolved by config.Load; no extra
			// work needed here.
			reg.Reload(ctx, newCfg.Drivers)
			// Refresh capacities — mutate the existing map in place so
			// Deps.Capacities (a map header captured at init) sees the
			// update. Rebinding the local variable would orphan the
			// reference the api server still holds.
			capMu.Lock()
			for k := range capacities { delete(capacities, k) }
			for k, v := range driverCapacitiesFrom(newCfg.Drivers) {
				capacities[k] = v
			}
			capMu.Unlock()

			// Weather diff → push live into the PV twin + forecast
			// fetcher without a process restart. Users adjust rated PV
			// + lat/lon from Settings and expect the change to take
			// effect right away.
			if newCfg.Weather != nil {
				oldLat, oldLon, oldRated := 0.0, 0.0, 0.0
				if oldCfg.Weather != nil {
					oldLat = oldCfg.Weather.Latitude
					oldLon = oldCfg.Weather.Longitude
					oldRated = oldCfg.Weather.PVRatedW
				}
				newRated := newCfg.Weather.PVRatedW
				if newRated > 0 && newRated != oldRated {
					if pvSvc != nil {
						pvSvc.SetRated(newRated)
					}
					if forecastSvc != nil {
						forecastSvc.RatedPVW = newRated
					}
				}
				newLat := newCfg.Weather.Latitude
				newLon := newCfg.Weather.Longitude
				if newLat != oldLat || newLon != oldLon {
					if pvSvc != nil {
						pvSvc.ClearSky = func(t time.Time) float64 { return forecast.ClearSkyW(newLat, newLon, t) }
					}
					if forecastSvc != nil {
						forecastSvc.Lat = newLat
						forecastSvc.Lon = newLon
					}
					slog.Info("weather location updated", "lat", newLat, "lon", newLon)
				}
			}
		})
	if err != nil {
		slog.Warn("could not start config watcher", "err", err)
	} else {
		watcher.Start()
		defer watcher.Stop()
	}

	// ---- Spot prices + weather forecast (optional, nil if not configured) ----
	// ---- FX rates (ECB, daily) — harmless to run even for SE-only users ----
	fxSvc := currency.New(st)
	fxSvc.Start(ctx)
	defer fxSvc.Stop()

	priceSvc := prices.FromConfig(cfg.Price, st, fxSvc)

	// ---- Price forecaster (fills in beyond day-ahead publication) ----
	zones := []string{"SE3"}
	if cfg.Price != nil && cfg.Price.Zone != "" {
		zones = []string{cfg.Price.Zone}
	}
	priceFc := priceforecast.NewService(st, zones)
	// Optional: seed from bundled CSV on first boot. Idempotent so safe
	// to call every boot — no-op once data is already in the store.
	seedPath := filepath.Join(filepath.Dir(*configPath), "seed", "prices.csv")
	if _, err := os.Stat(seedPath); err == nil {
		n, err := priceFc.SeedFromCSV(seedPath)
		if err != nil {
			slog.Warn("priceforecast seed failed", "path", seedPath, "err", err)
		} else if n > 0 {
			slog.Info("priceforecast seeded", "rows", n, "path", seedPath)
		}
	}
	priceFc.Start(ctx)
	defer priceFc.Stop()
	if priceSvc != nil {
		priceSvc.Start(ctx)
		defer priceSvc.Stop()
		slog.Info("price service started", "zone", priceSvc.Zone, "provider", priceSvc.Provider.Name())
	}

	// Sum rated PV from all drivers for the forecast estimator
	// Prefer explicit config; fall back to heuristic if unset.
	ratedPVW := 0.0
	if cfg.Weather != nil && cfg.Weather.PVRatedW > 0 {
		ratedPVW = cfg.Weather.PVRatedW
	} else {
		for _, d := range cfg.Drivers {
			if d.BatteryCapacityWh > 0 {
				ratedPVW += d.BatteryCapacityWh / 3
			}
		}
		if ratedPVW == 0 {
			ratedPVW = 10000
		}
	}
	forecastSvc = forecast.FromConfig(cfg.Weather, ratedPVW, st,
		"forty-two-watts/"+Version+" github.com/frahlg/forty-two-watts")
	if forecastSvc != nil {
		forecastSvc.Start(ctx)
		defer forecastSvc.Stop()
		slog.Info("forecast service started", "provider", forecastSvc.Provider.Name(),
			"lat", forecastSvc.Lat, "lon", forecastSvc.Lon, "rated_pv_w", ratedPVW)
	}

	// ---- Start PV digital twin (optional, requires weather config) ----
	// pvSvc is pre-declared above so the reload Applier can update it.
	if cfg.Weather != nil && cfg.Weather.Provider != "" && cfg.Weather.Provider != "none" {
		lat, lon := cfg.Weather.Latitude, cfg.Weather.Longitude
		clearSkyFn := func(t time.Time) float64 { return forecast.ClearSkyW(lat, lon, t) }
		cloudFn := func(t time.Time) (float64, bool) {
			// Look up nearest forecast row covering `t`.
			nowMs := t.UnixMilli()
			rows, err := st.LoadForecasts(nowMs-2*3600*1000, nowMs+2*3600*1000)
			if err != nil || len(rows) == 0 {
				return 0, false
			}
			for _, r := range rows {
				slotLen := r.SlotLenMin
				if slotLen <= 0 {
					slotLen = 60
				}
				end := r.SlotTsMs + int64(slotLen)*60*1000
				if nowMs >= r.SlotTsMs && nowMs < end && r.CloudCoverPct != nil {
					return *r.CloudCoverPct, true
				}
			}
			return 0, false
		}
		pvSvc = pvmodel.NewService(st, tel, clearSkyFn, cloudFn, ratedPVW)
		pvSvc.Start(ctx)
		defer pvSvc.Stop()
		slog.Info("pvmodel started", "rated_w", ratedPVW, "quality", pvSvc.Model().Quality())
	}

	// ---- Start load digital twin ----
	// Peak load proxy: use fuse power budget × 0.5 as a sane default
	// until user configures an explicit value. Users can override by
	// setting site.load_peak_w in config once we expose it.
	loadPeakW := cfg.Fuse.MaxPowerW() * 0.5
	if loadPeakW <= 0 {
		loadPeakW = 5000
	}
	loadSvc := loadmodel.NewService(st, tel, cfg.SiteMeterDriver(), loadPeakW)
	if cfg.Weather != nil && cfg.Weather.HeatingWPerDegC > 0 {
		m := loadSvc.Model()
		m.HeatingW_per_degC = cfg.Weather.HeatingWPerDegC
		// Apply without persisting raw overwrite — model is behind a sync,
		// so use the exposed setter. Simpler: push via reset+restore.
		// Just update the live field directly through a small helper.
		loadSvc.SetHeatingCoef(cfg.Weather.HeatingWPerDegC)
	}
	// Temperature source for heating-gain fit: same forecast cache.
	loadSvc.Temp = func(t time.Time) (float64, bool) {
		nowMs := t.UnixMilli()
		rows, err := st.LoadForecasts(nowMs-2*3600*1000, nowMs+2*3600*1000)
		if err != nil || len(rows) == 0 {
			return 0, false
		}
		for _, r := range rows {
			slotLen := r.SlotLenMin
			if slotLen <= 0 {
				slotLen = 60
			}
			end := r.SlotTsMs + int64(slotLen)*60*1000
			if nowMs >= r.SlotTsMs && nowMs < end && r.TempC != nil {
				return *r.TempC, true
			}
		}
		return 0, false
	}
	loadSvc.Start(ctx)
	defer loadSvc.Stop()
	slog.Info("loadmodel started", "peak_w", loadPeakW, "quality", loadSvc.Model().Quality())

	// ---- Start MPC planner (optional) ----
	mpcSvc := buildMPC(cfg, st, tel, capacities)
	if mpcSvc != nil {
		if pvSvc != nil {
			mpcSvc.PV = pvSvc.Predict
		}
		mpcSvc.Load = loadSvc.Predict
		mpcSvc.Price = priceFc.Predict
		mpcSvc.SiteMeter = cfg.SiteMeterDriver()
		if cfg.Price != nil {
			mpcSvc.ExportBonusOreKwh = cfg.Price.ExportBonusOreKwh
			mpcSvc.ExportFeeOreKwh = cfg.Price.ExportFeeOreKwh
			mpcSvc.GridTariffOreKwh = cfg.Price.GridTariffOreKwh
			mpcSvc.VATPercent = cfg.Price.VATPercent
		}
		mpcSvc.Start(ctx)
		defer mpcSvc.Stop()
		// Inject plan → control.State. Both callbacks are wired:
		//   PlanTarget — legacy grid-target path (grid_target_w, mode str)
		//   SlotDirective — new energy-allocation path (Wh per slot)
		// State.UseEnergyDispatch picks which one is actually used when a
		// planner mode is active; see docs/plan-ems-contract.md.
		ctrl.PlanTarget = mpcSvc.SlotAt
		ctrl.SlotDirective = func(now time.Time) (control.SlotDirective, bool) {
			d, ok := mpcSvc.SlotDirectiveAt(now)
			if !ok {
				return control.SlotDirective{}, false
			}
			return control.SlotDirective{
				SlotStart:       d.SlotStart,
				SlotEnd:         d.SlotEnd,
				BatteryEnergyWh: d.BatteryEnergyWh,
				SoCTargetPct:    d.SoCTargetPct,
				Strategy:        string(d.Strategy),
			}, true
		}
		ctrl.UseEnergyDispatch = cfg.Planner != nil && cfg.Planner.UseEnergyDispatch
		// If the restored control mode is a planner variant, push the
		// corresponding mpc.Mode so the plan is built with the strategy
		// the user actually picked — not whatever cfg.planner.mode says.
		if ctrl.Mode.IsPlannerMode() {
			var mm mpc.Mode
			switch ctrl.Mode {
			case control.ModePlannerSelf:
				mm = mpc.ModeSelfConsumption
			case control.ModePlannerCheap:
				mm = mpc.ModeCheapCharge
			case control.ModePlannerArbitrage:
				mm = mpc.ModeArbitrage
			}
			mpcSvc.SetMode(ctx, mm)
		}
		slog.Info("mpc planner started",
			"mode", mpcSvc.Defaults.Mode,
			"capacity_wh", mpcSvc.Defaults.CapacityWh,
			"horizon", mpcSvc.Horizon,
			"interval", mpcSvc.Interval,
			"pvtwin", pvSvc != nil)
	}

	// ---- Start HTTP API ----
	// Forward-declare haBridge so Deps can reference it; the bridge
	// gets wired further down (HA is optional + depends on reg.Names()).
	var haBridge *ha.Bridge
	deps := &api.Deps{
		Tel: tel, Ctrl: ctrl, CtrlMu: ctrlMu,
		State: st,
		CapMu: capMu, Capacities: capacities,
		CfgMu: cfgMu, Cfg: cfg, ConfigPath: *configPath,
		DriverDir: resolveDriverDir(),
		Models: models, ModelsMu: modelsMu,
		SelfTune: selfTune,
		DtS:        float64(cfg.Site.ControlIntervalS),
		SaveConfig: config.SaveAtomic,
		WebDir:     *webDir,
		Prices:     priceSvc,
		Forecast:   forecastSvc,
		MPC:        mpcSvc,
		PVModel:    pvSvc,
		LoadModel:  loadSvc,
		HA:         haBridge,
		Registry:   reg,
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
			deps.HA = haBridge // late-binding for API
		}
	}

	// ---- OCPP 1.6J Central System (EV chargers) ----
	if cfg.OCPP != nil && cfg.OCPP.Enabled {
		ocppCfg := &ocpp.Config{
			Enabled:            cfg.OCPP.Enabled,
			Bind:               cfg.OCPP.Bind,
			Port:               cfg.OCPP.Port,
			Path:               cfg.OCPP.Path,
			Username:           cfg.OCPP.Username,
			Password:           cfg.OCPP.Password,
			HeartbeatIntervalS: cfg.OCPP.HeartbeatIntervalS,
		}
		ocppSrv, err := ocpp.Start(ctx, ocppCfg, tel)
		if err != nil {
			slog.Warn("OCPP central system failed to start", "err", err)
		} else {
			defer ocppSrv.Stop()
			// API surface for /api/ev_chargers etc. lands in Unit 5.
			_ = ocppSrv
		}
	}

	// ---- Background: Parquet rolloff (>14d → cold dir) ----
	go rolloffLoop(ctx, st, coldDir)

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
			if err := st.RecordEvent("shutdown"); err != nil {
				slog.Warn("failed to persist shutdown event", "err", err)
			}
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

			// ---- Watchdog: mark stale drivers offline, revert them to autonomous ----
			watchdogTimeout := time.Duration(cfg.Site.WatchdogTimeoutS) * time.Second
			if watchdogTimeout <= 0 { watchdogTimeout = 60 * time.Second }
			for _, tr := range tel.WatchdogScan(watchdogTimeout) {
				if !tr.Online {
					slog.Warn("driver telemetry stale — marking offline + reverting to autonomous",
						"name", tr.Name, "timeout", watchdogTimeout)
					_ = reg.SendDefault(ctx, tr.Name)
				} else {
					slog.Info("driver telemetry recovered — back online", "name", tr.Name)
				}
			}

			// ---- Safety: site meter stale → idle everything this cycle ----
			// Otherwise stale grid readings cause one battery to charge another.
			ctrlMu.Lock()
			siteMeterStale := tel.IsStale(ctrl.SiteMeterDriver, telemetry.DerMeter, watchdogTimeout)
			ctrlMu.Unlock()
			if siteMeterStale {
				slog.Warn("site meter telemetry stale — idling batteries this cycle",
					"driver", ctrl.SiteMeterDriver)
				for _, n := range reg.Names() {
					_ = reg.SendDefault(ctx, n)
				}
				continue
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

			// ---- Flush per-driver metrics into long-format TS DB ----
			if samples := tel.FlushSamples(); len(samples) > 0 {
				stSamples := make([]state.Sample, len(samples))
				for i, sm := range samples {
					stSamples[i] = state.Sample{Driver: sm.Driver, Metric: sm.Metric, TsMs: sm.TsMs, Value: sm.Value}
				}
				if err := st.RecordSamples(stSamples); err != nil {
					slog.Warn("ts samples flush failed", "n", len(samples), "err", err)
				}
			}

			// ---- Periodic battery-model persistence (every 12 cycles ≈ 60s) ----
			saveCount++
			if saveCount%12 == 0 {
				modelsMu.Lock()
				for name, m := range models {
					if data, err := json.Marshal(m); err == nil {
						if err := st.SaveBatteryModel(name, string(data)); err != nil {
						slog.Warn("failed to persist battery model", "battery", name, "err", err)
					}
					}
				}
				modelsMu.Unlock()
			}
		}
	}
}

// rolloffLoop runs the SQLite → Parquet roll-off once per hour. Cheap when
// nothing is due (a single SELECT returns 0 rows); only does real work once
// data crosses the 14-day boundary into cold storage.
func rolloffLoop(ctx context.Context, st *state.Store, coldDir string) {
	tick := time.NewTicker(1 * time.Hour)
	defer tick.Stop()
	// Run once at startup so a fresh boot catches any backlog.
	doRolloff(ctx, st, coldDir)
	for {
		select {
		case <-ctx.Done(): return
		case <-tick.C: doRolloff(ctx, st, coldDir)
		}
	}
}

func doRolloff(ctx context.Context, st *state.Store, coldDir string) {
	rows, files, err := st.RolloffToParquet(ctx, coldDir)
	if err != nil {
		slog.Warn("parquet rolloff failed", "err", err)
		return
	}
	if rows > 0 {
		slog.Info("parquet rolloff", "rows", rows, "files", len(files))
	}
}

// registerAllDevices snapshots the identity HostEnv has gathered for each
// running driver and upserts a row in the devices table. Idempotent.
// Called periodically because some drivers (notably MQTT) only learn their
// serial after the first message from the device.
func registerAllDevices(st *state.Store, reg *drivers.Registry) {
	for _, name := range reg.Names() {
		env := reg.Env(name)
		if env == nil { continue }
		make, sn, mac, ep := env.FullIdentity()
		dev := state.Device{
			DriverName: name,
			Make:       make,
			Serial:     sn,
			MAC:        mac,
			Endpoint:   ep,
		}
		if id, err := st.RegisterDevice(dev); err == nil && id != "" {
			slog.Debug("device registered", "name", name, "device_id", id, "make", make, "sn", sn, "mac", mac)
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
		// Default max (de)charge = 0.5C unless overridden. Zero is a
		// legitimate one-sided constraint — `max_charge_w: 0` means
		// "forbid charging, allow discharge only" and mpc.Optimize's
		// action grid (`-MaxDischargeW…+MaxChargeW`) supports it.
		// Negative is always a config mistake.
		//
		// Only the *both-zero* case is treated as a config error (and
		// almost certainly is — it kills the planner's entire action
		// space while leaving the service running). We fall back to
		// default in that case and log a warning.
		defaultP := cap / 2
		chg := defaultP
		dis := defaultP
		if b, ok := cfg.Batteries[d.Name]; ok {
			bothZero := b.MaxChargeW != nil && *b.MaxChargeW == 0 &&
				b.MaxDischargeW != nil && *b.MaxDischargeW == 0
			if bothZero {
				slog.Warn("mpc: batteries.max_{charge,discharge}_w both 0 — treating as config error, using default 0.5C",
					"driver", d.Name, "default_w", defaultP)
			} else {
				if b.MaxChargeW != nil && *b.MaxChargeW >= 0 {
					chg = *b.MaxChargeW
				} else if b.MaxChargeW != nil {
					slog.Warn("mpc: ignoring negative batteries.max_charge_w; using default 0.5C",
						"driver", d.Name, "value", *b.MaxChargeW, "default_w", defaultP)
				}
				if b.MaxDischargeW != nil && *b.MaxDischargeW >= 0 {
					dis = *b.MaxDischargeW
				} else if b.MaxDischargeW != nil {
					slog.Warn("mpc: ignoring negative batteries.max_discharge_w; using default 0.5C",
						"driver", d.Name, "value", *b.MaxDischargeW, "default_w", defaultP)
				}
			}
		}
		maxChg += chg
		maxDis += dis
	}
	if totalCap <= 0 {
		slog.Warn("mpc: no battery capacity — skipping")
		return nil
	}
	// Clamp aggregate charge/discharge to the grid fuse capacity. The
	// control loop's fuse guard enforces this per-tick anyway, but a
	// planner that schedules 45 kW of charge through a 16 A fuse (11 kW)
	// produces SoC projections that can never be realised — the optimiser
	// "charges" to 100% in the plan while the battery barely budges in
	// reality, and every downstream decision (when to discharge, when to
	// idle, what the total cost looks like) is based on that fantasy.
	// Cheaper to keep the plan feasible up-front.
	if fuseMaxW := cfg.Fuse.MaxPowerW(); fuseMaxW > 0 {
		if maxChg > fuseMaxW {
			slog.Info("mpc: clamping MaxChargeW to fuse capacity",
				"requested_w", maxChg, "fuse_w", fuseMaxW)
			maxChg = fuseMaxW
		}
		if maxDis > fuseMaxW {
			slog.Info("mpc: clamping MaxDischargeW to fuse capacity",
				"requested_w", maxDis, "fuse_w", fuseMaxW)
			maxDis = fuseMaxW
		}
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

// isConfigMissing checks whether the error from config.Load indicates the
// config file does not exist (as opposed to a parse or validation error).
// config.Load wraps the os error with fmt.Errorf, so we use errors.Is to
// unwrap through the chain.
func isConfigMissing(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	return strings.Contains(err.Error(), "no such file")
}

func recordHistory(st *state.Store, tel *telemetry.Store, ctrl *control.State, nowMs int64) {
	gridW := 0.0
	if r := tel.Get(ctrl.SiteMeterDriver, telemetry.DerMeter); r != nil {
		gridW = r.SmoothedW
	}
	var pvW, batW, sumSoC float64
	var socCount int
	for _, r := range tel.ReadingsByType(telemetry.DerPV) { pvW += r.SmoothedW }
	for _, r := range tel.ReadingsByType(telemetry.DerBattery) {
		batW += r.SmoothedW
		if r.SoC != nil {
			sumSoC += *r.SoC
			socCount++
		}
	}
	avgSoC := 0.0
	if socCount > 0 { avgSoC = sumSoC / float64(socCount) }
	loadW := gridW - batW - pvW
	if loadW < 0 { loadW = 0 }

	// Per-driver detail packed into the JSON column. The schema is
	// schema-less by design — UI code reads what it understands and
	// ignores the rest, so drivers can add fields without a migration.
	perDriver := make(map[string]map[string]float64)
	for name, h := range tel.AllHealth() {
		row := map[string]float64{}
		if r := tel.Get(name, telemetry.DerBattery); r != nil {
			row["bat_w"] = r.SmoothedW
			if r.SoC != nil { row["soc"] = *r.SoC }
		}
		if r := tel.Get(name, telemetry.DerPV); r != nil {
			row["pv_w"] = r.SmoothedW
		}
		if r := tel.Get(name, telemetry.DerMeter); r != nil {
			row["meter_w"] = r.SmoothedW
		}
		_ = h
		perDriver[name] = row
	}
	targets := make(map[string]float64)
	for _, t := range ctrl.LastTargets {
		targets[t.Driver] = t.TargetW
	}
	jsonBlob, _ := json.Marshal(map[string]any{
		"drivers": perDriver,
		"targets": targets,
	})
	if err := st.RecordHistory(state.HistoryPoint{
		TsMs: nowMs, GridW: gridW, PVW: pvW, BatW: batW, LoadW: loadW, BatSoC: avgSoC,
		JSON: string(jsonBlob),
	}); err != nil {
		slog.Warn("failed to persist history point", "err", err)
	}
}
