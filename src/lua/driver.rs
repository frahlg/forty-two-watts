//! Driver lifecycle: load Lua file, call driver_init/poll/command/cleanup,
//! error tracking with consecutive error count, watchdog timeout.

use crate::config::{DriverConfig, ModbusConnectionConfig, MqttConnectionConfig};
use crate::lua::host_api::{self, HostContext};
use crate::lua::runtime::LuaRuntime;
use crate::modbus::ModbusClient;
use crate::mqtt::client::MqttClient;
use crate::telemetry::TelemetryStore;
use mlua::{Function, HookTriggers, Table, Value};
use std::collections::HashMap;
use std::path::Path;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

/// Maximum Lua instructions per poll() call (prevents infinite loops).
const MAX_INSTRUCTIONS_PER_TICK: u32 = 50_000_000;

/// Default poll interval in milliseconds.
const DEFAULT_POLL_INTERVAL_MS: u64 = 1000;

/// Maximum poll interval after exponential backoff (60s).
const MAX_BACKOFF_MS: u64 = 60_000;

/// Maximum consecutive errors before a driver is considered degraded.
const _DEGRADED_THRESHOLD: u32 = 3;

/// Loaded driver state.
pub struct Driver {
    /// Driver name (from config).
    pub name: String,
    /// Lua runtime (one per driver for thread-safety).
    runtime: LuaRuntime,
    /// Per-driver sandbox environment.
    env: Table,
    /// Whether driver_init() has been called successfully.
    initialized: bool,
    /// Last error message.
    pub last_error: Option<String>,
    /// Total poll count.
    pub poll_count: u64,
    /// Consecutive poll errors (for exponential backoff).
    pub consecutive_errors: u32,
    /// Base poll interval in milliseconds.
    poll_interval_ms: u64,
    /// Watchdog: last successful poll time.
    last_success: Option<Instant>,
    /// Watchdog timeout in seconds.
    watchdog_timeout_s: u64,
    /// Whether watchdog fallback has been triggered.
    watchdog_triggered: bool,
    /// Current driver name (shared with host API).
    current_driver: Arc<Mutex<String>>,
    /// Shared telemetry store.
    telemetry_store: Arc<Mutex<TelemetryStore>>,
}

impl Driver {
    /// Load a driver from a Lua file. Creates a sandboxed Lua VM, registers
    /// host API functions, and loads the Lua source.
    pub fn load(
        config: &DriverConfig,
        telemetry_store: Arc<Mutex<TelemetryStore>>,
        watchdog_timeout_s: u64,
        lua_dir: &Path,
    ) -> Result<Self, String> {
        let runtime = LuaRuntime::new()
            .map_err(|e| format!("failed to create Lua runtime: {}", e))?;

        let current_driver = Arc::new(Mutex::new(config.name.clone()));
        let driver_serials = Arc::new(Mutex::new(HashMap::new()));
        let driver_makes = Arc::new(Mutex::new(HashMap::new()));

        // Auto-create Modbus connection if configured
        let modbus_client = if let Some(ref mb) = config.modbus {
            match create_modbus_client(mb) {
                Ok(client) => {
                    tracing::info!(
                        "Driver '{}': Modbus connected to {}:{} unit={}",
                        config.name, mb.host, mb.port, mb.unit_id
                    );
                    Some(Arc::new(Mutex::new(client)))
                }
                Err(e) => {
                    tracing::warn!(
                        "Driver '{}': Modbus connect failed: {}",
                        config.name, e
                    );
                    None
                }
            }
        } else {
            None
        };

        // Auto-create MQTT connection if configured
        let (mqtt_client, mqtt_queue) = if let Some(ref mq) = config.mqtt {
            match create_mqtt_client(mq, &config.name) {
                Ok(client) => {
                    let queue = client.message_queue();
                    tracing::info!(
                        "Driver '{}': MQTT connected to {}:{}",
                        config.name, mq.host, mq.port
                    );
                    (Some(Arc::new(Mutex::new(client))), Some(queue))
                }
                Err(e) => {
                    tracing::warn!(
                        "Driver '{}': MQTT connect failed: {}",
                        config.name, e
                    );
                    (None, None)
                }
            }
        } else {
            (None, None)
        };

        // Register host APIs
        let host_ctx = HostContext {
            telemetry_store: telemetry_store.clone(),
            current_driver: current_driver.clone(),
            driver_serials,
            driver_makes,
            modbus_client,
            mqtt_client,
            mqtt_queue,
        };
        host_api::register_all(runtime.lua(), &host_ctx)
            .map_err(|e| format!("failed to register host APIs: {}", e))?;

        // Create isolated environment
        let env = runtime
            .create_driver_env(&config.name)
            .map_err(|e| format!("failed to create driver env: {}", e))?;

        // Copy 'host' table into the driver environment
        let host: Table = runtime
            .lua()
            .globals()
            .get("host")
            .map_err(|e| format!("host table not registered: {}", e))?;
        env.set("host", host)
            .map_err(|e| format!("failed to set host in env: {}", e))?;

        // Load Lua source file
        let lua_path = lua_dir.join(&config.lua);
        let source = std::fs::read_to_string(&lua_path)
            .map_err(|e| format!("failed to read '{}': {}", lua_path.display(), e))?;

        let chunk = runtime.lua().load(&source).set_name(&config.name);
        let func: Function = chunk
            .into_function()
            .map_err(|e| format!("Lua parse error in '{}': {}", config.lua, e))?;
        func.set_environment(env.clone())
            .map_err(|e| format!("failed to set environment: {}", e))?;
        func.call::<()>(())
            .map_err(|e| format!("Lua exec error in '{}': {}", config.lua, e))?;

        Ok(Self {
            name: config.name.clone(),
            runtime,
            env,
            initialized: false,
            last_error: None,
            poll_count: 0,
            consecutive_errors: 0,
            poll_interval_ms: DEFAULT_POLL_INTERVAL_MS,
            last_success: None,
            watchdog_timeout_s,
            watchdog_triggered: false,
            current_driver,
            telemetry_store,
        })
    }

    /// Call driver_init(config) with the connection config as a JSON string.
    pub fn init(&mut self, config: &DriverConfig) -> Result<(), String> {
        *self.current_driver.lock().unwrap() = self.name.clone();

        if let Ok(init_fn) = self.env.get::<Function>("driver_init") {
            // Build config JSON for the driver
            let config_json = build_driver_config_json(config);
            let config_val = Value::String(
                self.runtime
                    .lua()
                    .create_string(&config_json)
                    .map_err(|e| e.to_string())?,
            );

            match init_fn.call::<()>(config_val) {
                Ok(()) => {
                    self.initialized = true;
                    tracing::info!("Driver '{}' initialized", self.name);
                    Ok(())
                }
                Err(e) => {
                    let err = format!("driver_init() failed: {}", e);
                    tracing::warn!("Driver '{}': {}", self.name, err);
                    self.last_error = Some(err.clone());
                    Err(err)
                }
            }
        } else {
            // No driver_init function -- that's okay
            self.initialized = true;
            tracing::info!("Driver '{}' loaded (no driver_init function)", self.name);
            Ok(())
        }
    }

    /// Call driver_poll(). Returns the requested poll interval in milliseconds.
    /// Tracks errors with exponential backoff.
    pub fn poll(&mut self) -> Duration {
        if !self.initialized {
            return Duration::from_millis(self.poll_interval_ms);
        }

        *self.current_driver.lock().unwrap() = self.name.clone();

        let poll_fn: Function = match self.env.get("driver_poll") {
            Ok(f) => f,
            Err(_) => return Duration::from_millis(self.poll_interval_ms),
        };

        // Set instruction limit hook to prevent infinite loops
        let driver_name = self.name.clone();
        let _ = self.runtime.lua().set_hook(
            HookTriggers::new().every_nth_instruction(MAX_INSTRUCTIONS_PER_TICK),
            move |_lua, _debug| {
                Err(mlua::Error::external(format!(
                    "driver '{}' exceeded instruction limit",
                    driver_name
                )))
            },
        );

        match poll_fn.call::<Value>(()) {
            Ok(ret) => {
                self.last_error = None;
                self.poll_count += 1;
                self.consecutive_errors = 0;
                self.last_success = Some(Instant::now());
                self.watchdog_triggered = false;

                // Record success in telemetry store
                self.telemetry_store
                    .lock()
                    .unwrap()
                    .driver_health_mut(&self.name)
                    .record_success();

                // Driver can return a custom interval in milliseconds
                if let Value::Integer(ms) = ret {
                    self.poll_interval_ms = (ms as u64).max(100);
                } else if let Value::Number(ms) = ret {
                    self.poll_interval_ms = (ms as u64).max(100);
                }
            }
            Err(e) => {
                let err = format!("driver_poll() error: {}", e);
                if self.last_error.as_deref() != Some(&err) {
                    tracing::warn!("Driver '{}': {}", self.name, err);
                }
                self.last_error = Some(err.clone());
                self.poll_count += 1;
                self.consecutive_errors = self.consecutive_errors.saturating_add(1);

                // Record error in telemetry store
                self.telemetry_store
                    .lock()
                    .unwrap()
                    .driver_health_mut(&self.name)
                    .record_error(&err);
            }
        }

        // Remove hook after poll completes
        self.runtime.lua().remove_hook();

        // GC after poll
        self.runtime.lua().gc_collect().ok();

        self.effective_interval()
    }

    /// Call driver_command(action, power_w, cmd) for EMS dispatch.
    /// action: "charge", "discharge", "idle"
    /// power_w: target power in watts
    /// cmd: JSON string with additional command parameters
    pub fn command(&mut self, action: &str, power_w: f64, cmd: &str) -> Result<(), String> {
        if !self.initialized {
            return Err("driver not initialized".to_string());
        }

        *self.current_driver.lock().unwrap() = self.name.clone();

        let cmd_fn: Function = self
            .env
            .get("driver_command")
            .map_err(|_| "driver_command not defined".to_string())?;

        cmd_fn
            .call::<()>((action, power_w, cmd))
            .map_err(|e| format!("driver_command() error: {}", e))
    }

    /// Call driver_default_mode() for watchdog fallback.
    /// This is called when the EMS control loop hasn't sent a command within
    /// the watchdog timeout, so the driver should revert to its safe default.
    pub fn default_mode(&mut self) -> Result<(), String> {
        if !self.initialized {
            return Err("driver not initialized".to_string());
        }

        *self.current_driver.lock().unwrap() = self.name.clone();

        if let Ok(f) = self.env.get::<Function>("driver_default_mode") {
            f.call::<()>(())
                .map_err(|e| format!("driver_default_mode() error: {}", e))?;
            tracing::info!("Driver '{}': reverted to default mode (watchdog)", self.name);
        }
        Ok(())
    }

    /// Call driver_cleanup() on shutdown.
    pub fn cleanup(&mut self) {
        if !self.initialized {
            return;
        }

        *self.current_driver.lock().unwrap() = self.name.clone();

        if let Ok(f) = self.env.get::<Function>("driver_cleanup") {
            if let Err(e) = f.call::<()>(()) {
                tracing::warn!("Driver '{}' cleanup error: {}", self.name, e);
            }
        }

        tracing::info!("Driver '{}' cleaned up", self.name);
    }

    /// Check if the watchdog has expired (no successful poll within timeout).
    pub fn watchdog_expired(&self) -> bool {
        if self.watchdog_triggered {
            return false; // Already triggered, don't repeat
        }
        match self.last_success {
            Some(t) => t.elapsed().as_secs() > self.watchdog_timeout_s,
            None => {
                // Never succeeded -- check if we've been trying long enough
                self.poll_count > 0
                    && self.consecutive_errors as u64 * self.poll_interval_ms / 1000
                        > self.watchdog_timeout_s
            }
        }
    }

    /// Mark the watchdog as triggered (prevents repeated fallback calls).
    pub fn mark_watchdog_triggered(&mut self) {
        self.watchdog_triggered = true;
        self.telemetry_store
            .lock()
            .unwrap()
            .driver_health_mut(&self.name)
            .set_offline();
    }

    /// Whether the driver has been initialized.
    pub fn is_initialized(&self) -> bool {
        self.initialized
    }

    /// Get the effective poll interval (with exponential backoff on errors).
    fn effective_interval(&self) -> Duration {
        if self.consecutive_errors == 0 {
            Duration::from_millis(self.poll_interval_ms)
        } else {
            let backoff = self
                .poll_interval_ms
                .saturating_mul(1 << self.consecutive_errors.min(6));
            Duration::from_millis(backoff.min(MAX_BACKOFF_MS))
        }
    }
}

/// Build a JSON config string to pass to driver_init().
fn build_driver_config_json(config: &DriverConfig) -> String {
    let mut map = serde_json::Map::new();

    map.insert("name".into(), serde_json::Value::String(config.name.clone()));
    map.insert(
        "is_site_meter".into(),
        serde_json::Value::Bool(config.is_site_meter),
    );
    map.insert(
        "battery_capacity_wh".into(),
        serde_json::json!(config.battery_capacity_wh),
    );

    if let Some(ref mb) = config.modbus {
        map.insert("protocol".into(), serde_json::Value::String("modbus".into()));
        map.insert("ip".into(), serde_json::Value::String(mb.host.clone()));
        map.insert("port".into(), serde_json::json!(mb.port));
        map.insert("unit_id".into(), serde_json::json!(mb.unit_id));
    }

    if let Some(ref mq) = config.mqtt {
        map.insert("protocol".into(), serde_json::Value::String("mqtt".into()));
        map.insert("mqtt_host".into(), serde_json::Value::String(mq.host.clone()));
        map.insert("mqtt_port".into(), serde_json::json!(mq.port));
    }

    serde_json::Value::Object(map).to_string()
}

fn create_modbus_client(cfg: &ModbusConnectionConfig) -> Result<ModbusClient, String> {
    ModbusClient::connect(&cfg.host, cfg.port, cfg.unit_id)
        .map_err(|e| format!("{}", e))
}

fn create_mqtt_client(cfg: &MqttConnectionConfig, driver_name: &str) -> Result<MqttClient, String> {
    let client_id = format!("home-ems-{}", driver_name);
    MqttClient::connect(
        &cfg.host,
        cfg.port,
        &client_id,
        cfg.username.as_deref(),
        cfg.password.as_deref(),
    )
    .map_err(|e| format!("{}", e))
}
