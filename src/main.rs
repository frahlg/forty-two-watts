mod config;
mod lua;
mod modbus;
mod mqtt;
mod telemetry;
mod control;
mod api;
mod ha;
mod state;

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::mpsc;
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tracing::{info, warn, error};

/// Command sent from the control loop to a driver thread
#[derive(Debug)]
enum DriverCommand {
    /// Call driver_command("battery", power_w, cmd_json)
    Battery { power_w: f64 },
    /// Call driver_default_mode() (watchdog fallback)
    DefaultMode,
    /// Shutdown the driver thread
    Shutdown,
}

fn main() {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::from_default_env()
                .add_directive(tracing::Level::INFO.into()),
        )
        .init();

    info!("home-ems v{}", env!("CARGO_PKG_VERSION"));

    let config_path = std::env::args()
        .nth(1)
        .unwrap_or_else(|| "config.yaml".to_string());

    let config = match config::Config::load(Path::new(&config_path)) {
        Ok(c) => c,
        Err(e) => {
            error!("failed to load config '{}': {}", config_path, e);
            std::process::exit(1);
        }
    };

    info!("site: {}", config.site.name);
    info!("fuse: {}A / {}ph (max {:.0}W)",
        config.fuse.max_amps, config.fuse.phases, config.fuse.max_power_w());
    info!("control: {}s interval, target {}W, tolerance {}W",
        config.site.control_interval_s, config.site.grid_target_w, config.site.grid_tolerance_w);

    // Open persistent state
    let state_path = config.state.as_ref()
        .map(|s| s.path.clone())
        .unwrap_or_else(|| "state.redb".to_string());
    let state_store = match state::StateStore::open(&state_path) {
        Ok(s) => Arc::new(s),
        Err(e) => {
            error!("failed to open state store '{}': {}", state_path, e);
            std::process::exit(1);
        }
    };

    // Initialize shared state
    let store = Arc::new(Mutex::new(
        telemetry::TelemetryStore::new(config.site.smoothing_alpha)
    ));

    let mut control_state = control::ControlState::new(
        config.site.grid_target_w,
        config.site.grid_tolerance_w,
    );
    if let Some(mode_str) = state_store.load_config("mode") {
        if let Ok(mode) = serde_json::from_str::<control::Mode>(&format!("\"{}\"", mode_str)) {
            info!("restored mode: {:?}", mode);
            control_state.mode = mode;
        }
    }
    let control = Arc::new(Mutex::new(control_state));

    let driver_capacities: HashMap<String, f64> = config.drivers.iter()
        .map(|d| (d.name.clone(), d.battery_capacity_wh))
        .collect();
    let driver_names: Vec<String> = config.drivers.iter().map(|d| d.name.clone()).collect();

    // Graceful shutdown
    let running = Arc::new(std::sync::atomic::AtomicBool::new(true));
    let r = running.clone();
    ctrlc::set_handler(move || {
        info!("shutdown signal received");
        r.store(false, std::sync::atomic::Ordering::SeqCst);
    }).expect("failed to set ctrl-c handler");

    // Start REST API
    let _api_handle = api::start(
        config.api.port,
        store.clone(),
        control.clone(),
        driver_capacities.clone(),
    );

    // Start HA MQTT bridge
    if let Some(ha_config) = config.homeassistant {
        if ha_config.enabled {
            let _ha_handle = ha::start(
                ha_config,
                store.clone(),
                control.clone(),
                driver_names.clone(),
            );
        }
    }

    // Resolve lua directory (relative to config file)
    let config_dir = Path::new(&config_path).parent().unwrap_or(Path::new("."));
    let lua_dir = config_dir.to_path_buf();

    // Start driver threads
    let mut cmd_senders: HashMap<String, mpsc::Sender<DriverCommand>> = HashMap::new();
    let mut driver_handles = Vec::new();

    for driver_config in &config.drivers {
        let (tx, rx) = mpsc::channel::<DriverCommand>();
        cmd_senders.insert(driver_config.name.clone(), tx);

        let dc = driver_config.clone();
        let store_clone = store.clone();
        let watchdog_s = config.site.watchdog_timeout_s;
        let lua_dir_clone = lua_dir.clone();
        let running_clone = running.clone();

        let handle = std::thread::Builder::new()
            .name(format!("driver-{}", driver_config.name))
            .spawn(move || {
                run_driver_thread(dc, store_clone, watchdog_s, lua_dir_clone, rx, running_clone);
            })
            .expect("failed to spawn driver thread");

        driver_handles.push((driver_config.name.clone(), handle));
    }

    info!("home-ems running on http://0.0.0.0:{}", config.api.port);
    state_store.record_event("startup");

    // Control loop on main thread
    let control_interval = Duration::from_secs(config.site.control_interval_s);
    let fuse_max_w = config.fuse.max_power_w();

    while running.load(std::sync::atomic::Ordering::SeqCst) {
        std::thread::sleep(control_interval);

        // Run one control cycle
        let targets = {
            let store_lock = store.lock().unwrap();
            let mut control_lock = control.lock().unwrap();
            control::compute_dispatch(&store_lock, &mut control_lock, &driver_capacities, fuse_max_w)
        };

        // Dispatch targets to drivers via channels
        for target in &targets {
            if let Some(tx) = cmd_senders.get(&target.driver) {
                let cmd = DriverCommand::Battery { power_w: target.target_w };
                if let Err(e) = tx.send(cmd) {
                    warn!("failed to send command to {}: {}", target.driver, e);
                }
            }
        }

        // Check watchdog for each driver
        {
            let store_lock = store.lock().unwrap();
            for name in &driver_names {
                if let Some(health) = store_lock.driver_health(name) {
                    if health.status == telemetry::DriverStatus::Offline {
                        continue; // already handled
                    }
                    if let Some(last) = health.last_success {
                        if last.elapsed().as_secs() > config.site.watchdog_timeout_s {
                            warn!("driver '{}' watchdog expired, reverting to default mode", name);
                            if let Some(tx) = cmd_senders.get(name) {
                                let _ = tx.send(DriverCommand::DefaultMode);
                            }
                        }
                    }
                }
            }
        }

        // Persist state
        {
            let control_lock = control.lock().unwrap();
            state_store.save_config("mode", &serde_json::to_string(&control_lock.mode).unwrap_or_default().trim_matches('"'));
            state_store.save_config("grid_target_w", &control_lock.grid_target_w.to_string());
        }
    }

    // Shutdown: send shutdown to all drivers
    info!("shutting down drivers...");
    for (name, tx) in &cmd_senders {
        let _ = tx.send(DriverCommand::DefaultMode);
        let _ = tx.send(DriverCommand::Shutdown);
        info!("sent shutdown to driver '{}'", name);
    }

    // Wait for driver threads to finish
    for (name, handle) in driver_handles {
        if let Err(e) = handle.join() {
            error!("driver '{}' thread panicked: {:?}", name, e);
        }
    }

    state_store.record_event("shutdown");
    info!("home-ems stopped");
}

/// Run a driver thread: load Lua, init, poll loop, handle commands
fn run_driver_thread(
    config: config::DriverConfig,
    store: Arc<Mutex<telemetry::TelemetryStore>>,
    watchdog_timeout_s: u64,
    lua_dir: PathBuf,
    cmd_rx: mpsc::Receiver<DriverCommand>,
    running: Arc<std::sync::atomic::AtomicBool>,
) {
    info!("driver '{}': starting", config.name);

    // Load driver
    let mut driver = match lua::driver::Driver::load(&config, store.clone(), watchdog_timeout_s, &lua_dir) {
        Ok(d) => d,
        Err(e) => {
            error!("driver '{}': failed to load: {}", config.name, e);
            store.lock().unwrap().driver_health_mut(&config.name).record_error(&e);
            store.lock().unwrap().driver_health_mut(&config.name).set_offline();
            return;
        }
    };

    // Initialize
    if let Err(e) = driver.init(&config) {
        error!("driver '{}': init failed: {}", config.name, e);
        // Continue anyway — some drivers work without init
    }

    info!("driver '{}': entering poll loop", config.name);

    // Poll loop
    while running.load(std::sync::atomic::Ordering::SeqCst) {
        // Check for commands (non-blocking)
        while let Ok(cmd) = cmd_rx.try_recv() {
            match cmd {
                DriverCommand::Battery { power_w } => {
                    let cmd_json = r#"{"id":"ems"}"#;
                    if let Err(e) = driver.command("battery", power_w, cmd_json) {
                        warn!("driver '{}': command error: {}", config.name, e);
                    } else {
                        info!("driver '{}': battery -> {:.0}W", config.name, power_w);
                    }
                }
                DriverCommand::DefaultMode => {
                    if let Err(e) = driver.default_mode() {
                        warn!("driver '{}': default_mode error: {}", config.name, e);
                    }
                    driver.mark_watchdog_triggered();
                }
                DriverCommand::Shutdown => {
                    info!("driver '{}': shutdown received", config.name);
                    driver.cleanup();
                    return;
                }
            }
        }

        // Poll the driver
        let interval = driver.poll();

        // Sleep for the requested interval
        std::thread::sleep(interval);
    }

    // Cleanup on exit
    driver.default_mode().ok();
    driver.cleanup();
}
