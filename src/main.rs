mod config;
mod lua;
mod modbus;
mod mqtt;
mod telemetry;
mod control;
mod api;
mod ha;
mod state;
mod energy;
mod driver_registry;
mod config_reload;
mod battery_model;
mod self_tune;

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, RwLock};
use std::time::Duration;
use tracing::{info, warn, error};

use driver_registry::{DriverRegistry, DriverCommand};

fn main() {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::from_default_env()
                .add_directive(tracing::Level::INFO.into()),
        )
        .init();

    info!("forty-two-watts v{} — The Answer to Grid Balancing", env!("CARGO_PKG_VERSION"));

    let config_path_str = std::env::args()
        .nth(1)
        .unwrap_or_else(|| "config.yaml".to_string());
    let config_path = PathBuf::from(&config_path_str);

    // Load config
    let initial_config = match config::Config::load(&config_path) {
        Ok(c) => c,
        Err(e) => {
            error!("failed to load config '{}': {}", config_path_str, e);
            std::process::exit(1);
        }
    };

    info!("site: {}", initial_config.site.name);
    info!("fuse: {}A / {}ph (max {:.0}W)",
        initial_config.fuse.max_amps, initial_config.fuse.phases, initial_config.fuse.max_power_w());
    info!("control: {}s interval, target {}W, tolerance {}W",
        initial_config.site.control_interval_s,
        initial_config.site.grid_target_w,
        initial_config.site.grid_tolerance_w);

    // Open persistent state
    let state_path = initial_config.state.as_ref()
        .map(|s| s.path.clone())
        .unwrap_or_else(|| "state.redb".to_string());
    let state_store = match state::StateStore::open(&state_path) {
        Ok(s) => Arc::new(s),
        Err(e) => {
            error!("failed to open state store '{}': {}", state_path, e);
            std::process::exit(1);
        }
    };

    // Telemetry store
    let store = Arc::new(Mutex::new(
        telemetry::TelemetryStore::new(initial_config.site.smoothing_alpha)
    ));

    // Site meter (which driver is the grid connection point)
    let site_meter_driver = initial_config.drivers.iter()
        .find(|d| d.is_site_meter)
        .map(|d| d.name.clone())
        .unwrap_or_else(|| initial_config.drivers[0].name.clone());
    info!("site meter: {}", site_meter_driver);

    // Control state
    let mut control_state = control::ControlState::new(
        initial_config.site.grid_target_w,
        initial_config.site.grid_tolerance_w,
        site_meter_driver,
    );
    control_state.slew_rate_w = initial_config.site.slew_rate_w;
    control_state.min_dispatch_interval_s = initial_config.site.min_dispatch_interval_s;
    if let Some(mode_str) = state_store.load_config("mode") {
        if let Ok(mode) = serde_json::from_str::<control::Mode>(&format!("\"{}\"", mode_str)) {
            info!("restored mode: {:?}", mode);
            control_state.mode = mode;
        }
    }
    info!("control: PI(Kp=0.5,Ki=0.1) + Kalman + slew={}W + holdoff={}s",
        control_state.slew_rate_w, control_state.min_dispatch_interval_s);
    let control = Arc::new(Mutex::new(control_state));

    // Energy accumulator
    let energy_state: energy::EnergyState = state_store.load_config("energy")
        .and_then(|s| serde_json::from_str(&s).ok())
        .unwrap_or_default();
    if !energy_state.today_date.is_empty() {
        info!("restored energy: today={:.2}kWh PV, {:.2}kWh load",
            energy_state.today.pv_wh / 1000.0,
            energy_state.today.load_wh / 1000.0);
    }
    let energy = Arc::new(Mutex::new(energy::EnergyAccumulator::new(energy_state)));

    // Battery models — load from redb if present, else start with fresh defaults per driver
    let mut models_map: HashMap<String, battery_model::BatteryModel> = HashMap::new();
    for (name, json) in state_store.load_all_battery_models() {
        match serde_json::from_str::<battery_model::BatteryModel>(&json) {
            Ok(m) => {
                info!("restored battery model '{}': τ={:.2}s gain={:.3} samples={}",
                    name, m.time_constant_s(initial_config.site.control_interval_s as f64),
                    m.steady_state_gain(), m.n_samples);
                models_map.insert(name, m);
            }
            Err(e) => warn!("failed to parse stored model for {}: {}", name, e),
        }
    }
    // Ensure every configured driver has a model entry
    for d in &initial_config.drivers {
        if d.battery_capacity_wh > 0.0 && !models_map.contains_key(&d.name) {
            models_map.insert(d.name.clone(), battery_model::BatteryModel::new(&d.name));
        }
    }
    let battery_models = Arc::new(RwLock::new(models_map));

    // Self-tune coordinator
    let self_tune_state = Arc::new(Mutex::new(self_tune::SelfTuneCoordinator::new()));

    // Graceful shutdown
    let running = Arc::new(std::sync::atomic::AtomicBool::new(true));
    let r = running.clone();
    ctrlc::set_handler(move || {
        info!("shutdown signal received");
        r.store(false, std::sync::atomic::Ordering::SeqCst);
    }).expect("failed to set ctrl-c handler");

    // Resolve lua directory
    let lua_dir = config_path.parent().unwrap_or(Path::new(".")).to_path_buf();

    // Driver registry — manages driver lifecycle dynamically
    let registry = DriverRegistry::new(
        store.clone(),
        initial_config.site.watchdog_timeout_s,
        lua_dir,
        running.clone(),
    );

    // Spawn initial drivers
    for dc in &initial_config.drivers {
        registry.add(dc.clone());
    }

    // Driver capacities map (used by control loop) — kept as snapshot, refreshed on reload
    let driver_capacities = Arc::new(RwLock::new(driver_capacities_from(&initial_config.drivers)));

    // Shared current config — single source of truth
    let current_config = Arc::new(RwLock::new(initial_config.clone()));

    // Start REST API
    let api_port = initial_config.api.port;
    let _api_handle = api::start(
        api_port,
        store.clone(),
        control.clone(),
        driver_capacities.clone(),
        state_store.clone(),
        energy.clone(),
        current_config.clone(),
        registry.clone(),
        config_path.clone(),
        battery_models.clone(),
        self_tune_state.clone(),
    );

    // Start HA MQTT bridge
    if let Some(ha_config) = &initial_config.homeassistant {
        if ha_config.enabled {
            let driver_names: Vec<String> = registry.names();
            let _ha_handle = ha::start(
                ha_config.clone(),
                store.clone(),
                control.clone(),
                driver_names,
            );
        }
    }

    // Start config file watcher — hot reload on yaml changes
    let _watcher_handle = config_reload::start_watcher(
        config_path.clone(),
        current_config.clone(),
        registry.clone(),
        control.clone(),
    );

    info!("forty-two-watts running on http://0.0.0.0:{}", api_port);
    state_store.record_event("startup");

    // Control loop on main thread
    let control_interval = Duration::from_secs(initial_config.site.control_interval_s);
    let fuse_max_w = initial_config.fuse.max_power_w();
    let dt_s = initial_config.site.control_interval_s as f64;
    let mut model_save_counter: u64 = 0;

    while running.load(std::sync::atomic::Ordering::SeqCst) {
        std::thread::sleep(control_interval);

        // Refresh capacities from current config (in case drivers were added/removed)
        {
            let cfg = current_config.read().unwrap();
            *driver_capacities.write().unwrap() = driver_capacities_from(&cfg.drivers);
        }
        let capacities_snap = driver_capacities.read().unwrap().clone();

        let now_ms = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis() as u64;

        // ---- Continuous learning: feed last (command, actual) pairs to RLS ----
        // Use last_targets from previous cycle as the commands; current actuals from store.
        // CRITICAL: skip during self-tune. Self-tune overrides commands AFTER compute_dispatch
        // sets last_targets, so RLS would see the cascade target paired with the actual that
        // resulted from the override command — completely uncorrelated. That corrupts the
        // model. The self-tune produces its own clean baseline at the end anyway.
        {
            let tune_active = self_tune_state.lock().unwrap().active;
            if !tune_active {
                let store_lock = store.lock().unwrap();
                let last_targets = control.lock().unwrap().last_targets.clone();
                let mut models = battery_models.write().unwrap();
                for target in &last_targets {
                    if let Some(reading) = store_lock.get(&target.driver, &telemetry::DerType::Battery) {
                        let model = models.entry(target.driver.clone())
                            .or_insert_with(|| battery_model::BatteryModel::new(&target.driver));
                        let soc = reading.soc.unwrap_or(0.5);
                        model.update(target.target_w, reading.smoothed_w, soc, dt_s, now_ms);
                    }
                }
            }
        }

        // ---- Self-tune tick: advance state machine, fit step responses ----
        {
            let mut tune = self_tune_state.lock().unwrap();
            if tune.active {
                let store_lock = store.lock().unwrap();
                let mut models = battery_models.write().unwrap();
                tune.tick(
                    |name| store_lock.get(name, &telemetry::DerType::Battery)
                        .map(|r| (r.smoothed_w, r.soc.unwrap_or(0.5))),
                    &mut models,
                    dt_s,
                    now_ms,
                );
            }
        }

        // Run one control cycle (with cascade if models present)
        let targets = {
            let store_lock = store.lock().unwrap();
            let mut control_lock = control.lock().unwrap();
            let models = battery_models.read().unwrap();
            control::compute_dispatch_with_models(
                &store_lock, &mut control_lock, &capacities_snap, fuse_max_w,
                &models, dt_s,
            )
        };

        // ---- Self-tune override: replace command for the battery being tuned ----
        let tune_override = self_tune_state.lock().unwrap().current_command();
        let final_targets: Vec<_> = if let Some((tune_battery, tune_cmd)) = tune_override {
            // For non-tuning batteries: use computed target. For tuning battery: use tune cmd.
            // Other batteries during tune: hold at idle (0W) — we don't want them counter-balancing
            let names: Vec<String> = registry.names();
            names.into_iter().map(|name| {
                if name == tune_battery {
                    control::DispatchTarget { driver: name, target_w: tune_cmd, clamped: false }
                } else {
                    control::DispatchTarget { driver: name, target_w: 0.0, clamped: false }
                }
            }).collect()
        } else {
            targets
        };

        // Dispatch targets to drivers via registry
        for target in &final_targets {
            let _ = registry.send(&target.driver, DriverCommand::Battery { power_w: target.target_w });
        }

        // Persist battery models periodically (every 12 cycles ≈ 60s at 5s)
        model_save_counter += 1;
        if model_save_counter % 12 == 0 {
            let models = battery_models.read().unwrap();
            for (name, model) in models.iter() {
                if let Ok(json) = serde_json::to_string(model) {
                    state_store.save_battery_model(name, &json);
                }
            }
        }

        // Watchdog per driver
        let watchdog_s = current_config.read().unwrap().site.watchdog_timeout_s;
        {
            let store_lock = store.lock().unwrap();
            for name in &registry.names() {
                if let Some(health) = store_lock.driver_health(name) {
                    if health.status == telemetry::DriverStatus::Offline { continue; }
                    if let Some(last) = health.last_success {
                        if last.elapsed().as_secs() > watchdog_s {
                            warn!("driver '{}' watchdog expired, reverting to default mode", name);
                            let _ = registry.send(name, DriverCommand::DefaultMode);
                        }
                    }
                }
            }
        }

        // Persist control + energy state
        {
            let control_lock = control.lock().unwrap();
            state_store.save_config("mode", &serde_json::to_string(&control_lock.mode).unwrap_or_default().trim_matches('"'));
            state_store.save_config("grid_target_w", &control_lock.grid_target_w.to_string());
        }
        {
            let store_lock = store.lock().unwrap();
            let ctrl_lock = control.lock().unwrap();
            let grid_w = store_lock.get(&ctrl_lock.site_meter_driver, &telemetry::DerType::Meter)
                .map(|m| m.smoothed_w).unwrap_or(0.0);
            let pv_w: f64 = store_lock.readings_by_type(&telemetry::DerType::Pv).iter().map(|p| p.smoothed_w).sum();
            let bat_w: f64 = store_lock.readings_by_type(&telemetry::DerType::Battery).iter().map(|b| b.smoothed_w).sum();
            drop(ctrl_lock);
            drop(store_lock);

            let mut en = energy.lock().unwrap();
            en.integrate(grid_w, pv_w, bat_w);
            if let Ok(json) = serde_json::to_string(&en.state) {
                state_store.save_config("energy", &json);
            }
        }

        // Record history snapshot
        let driver_names = registry.names();
        record_history_snapshot(&store, &control, &driver_names, &state_store, &energy);

        // Periodic pruning (every ~150s)
        if std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default()
            .as_secs() % 150 < initial_config.site.control_interval_s
        {
            state_store.prune_history(state::HISTORY_RETENTION_S);
        }
    }

    // Shutdown
    info!("shutting down drivers...");
    registry.shutdown_all();
    state_store.record_event("shutdown");
    info!("forty-two-watts stopped");
}

fn driver_capacities_from(drivers: &[config::DriverConfig]) -> HashMap<String, f64> {
    drivers.iter().map(|d| (d.name.clone(), d.battery_capacity_wh)).collect()
}

/// Record a telemetry snapshot to history database
fn record_history_snapshot(
    store: &Arc<Mutex<telemetry::TelemetryStore>>,
    control: &Arc<Mutex<control::ControlState>>,
    driver_names: &[String],
    state_store: &Arc<state::StateStore>,
    energy: &Arc<Mutex<energy::EnergyAccumulator>>,
) {
    let now_ms = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64;

    let snapshot = {
        let st = store.lock().unwrap();
        let ctrl = control.lock().unwrap();

        let grid_w = st.get(&ctrl.site_meter_driver, &telemetry::DerType::Meter)
            .map(|m| m.smoothed_w).unwrap_or(0.0);
        let pv_w: f64 = st.readings_by_type(&telemetry::DerType::Pv).iter().map(|p| p.smoothed_w).sum();
        let bat_w: f64 = st.readings_by_type(&telemetry::DerType::Battery).iter().map(|b| b.smoothed_w).sum();
        let bat_readings = st.readings_by_type(&telemetry::DerType::Battery);
        let avg_soc: f64 = if !bat_readings.is_empty() {
            bat_readings.iter().filter_map(|b| b.soc).sum::<f64>() / bat_readings.len() as f64
        } else { 0.0 };
        let load_w = grid_w - pv_w - bat_w;

        let mut drivers = serde_json::Map::new();
        for name in driver_names {
            let mut d = serde_json::Map::new();
            if let Some(m) = st.get(name, &telemetry::DerType::Meter) {
                d.insert("meter_w".into(), serde_json::json!(m.smoothed_w));
            }
            if let Some(p) = st.get(name, &telemetry::DerType::Pv) {
                d.insert("pv_w".into(), serde_json::json!(p.smoothed_w));
            }
            if let Some(b) = st.get(name, &telemetry::DerType::Battery) {
                d.insert("bat_w".into(), serde_json::json!(b.smoothed_w));
                if let Some(soc) = b.soc {
                    d.insert("bat_soc".into(), serde_json::json!(soc));
                }
            }
            drivers.insert(name.clone(), serde_json::Value::Object(d));
        }

        let mut targets = serde_json::Map::new();
        for t in &ctrl.last_targets {
            targets.insert(t.driver.clone(), serde_json::json!(t.target_w));
        }

        let e = energy.lock().unwrap();
        let today = &e.state.today;
        let total = &e.state.total;

        serde_json::json!({
            "ts": now_ms,
            "grid_w": grid_w,
            "pv_w": pv_w,
            "bat_w": bat_w,
            "load_w": load_w,
            "bat_soc": avg_soc,
            "drivers": drivers,
            "targets": targets,
            "energy_today": {
                "import_wh": today.import_wh,
                "export_wh": today.export_wh,
                "pv_wh": today.pv_wh,
                "bat_charged_wh": today.bat_charged_wh,
                "bat_discharged_wh": today.bat_discharged_wh,
                "load_wh": today.load_wh,
            },
            "energy_total": {
                "import_wh": total.import_wh,
                "export_wh": total.export_wh,
                "pv_wh": total.pv_wh,
                "bat_charged_wh": total.bat_charged_wh,
                "bat_discharged_wh": total.bat_discharged_wh,
                "load_wh": total.load_wh,
            },
        })
    };

    state_store.record_history(now_ms, &snapshot.to_string());
}
