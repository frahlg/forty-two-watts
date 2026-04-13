use std::io::Read;
use std::sync::{Arc, Mutex, RwLock};
use std::collections::HashMap;
use std::path::{Path, PathBuf};
use tracing::{info, error};

use crate::telemetry::{TelemetryStore, DerType};
use crate::control::{ControlState, Mode};
use crate::config::Config;
use crate::driver_registry::DriverRegistry;

/// Start the REST API server on a separate thread
pub fn start(
    port: u16,
    store: Arc<Mutex<TelemetryStore>>,
    control: Arc<Mutex<ControlState>>,
    driver_capacities: Arc<RwLock<HashMap<String, f64>>>,
    state_store: Arc<crate::state::StateStore>,
    energy: Arc<Mutex<crate::energy::EnergyAccumulator>>,
    current_config: Arc<RwLock<Config>>,
    registry: DriverRegistry,
    config_path: PathBuf,
) -> std::thread::JoinHandle<()> {
    std::thread::Builder::new()
        .name("api".to_string())
        .spawn(move || {
            let addr = format!("0.0.0.0:{}", port);
            let server = match tiny_http::Server::http(&addr) {
                Ok(s) => s,
                Err(e) => {
                    error!("failed to start API server on {}: {}", addr, e);
                    return;
                }
            };
            info!("API server listening on http://{}", addr);

            for mut request in server.incoming_requests() {
                let path = request.url().to_string();
                let method = request.method().to_string();

                let response = match (method.as_str(), path.as_str()) {
                    ("GET", "/api/health") => handle_health(&store),
                    ("GET", "/api/status") => handle_status(&store, &control, &driver_capacities, &energy),
                    ("GET", "/api/energy") => handle_energy(&energy),
                    ("GET", "/api/mode") => handle_get_mode(&control),
                    ("POST", "/api/mode") => handle_set_mode(&control, &mut request),
                    ("POST", "/api/target") => handle_set_target(&control, &mut request),
                    ("POST", "/api/peak_limit") => handle_set_peak_limit(&control, &mut request),
                    ("POST", "/api/ev_charging") => handle_set_ev_charging(&control, &mut request),
                    ("GET", "/api/drivers") => handle_drivers(&store),
                    ("GET", "/api/config") => handle_get_config(&current_config),
                    ("POST", "/api/config") => handle_post_config(&current_config, &registry, &control, &config_path, &mut request),
                    ("GET", p) if p.starts_with("/api/history") => handle_history(&state_store, p),
                    ("GET", path) => serve_static(path),
                    _ => json_response(404, &serde_json::json!({"error": "not found"})),
                };

                if let Err(e) = request.respond(response) {
                    error!("failed to send response: {}", e);
                }
            }
        })
        .expect("failed to start API thread")
}

fn json_response(status: u16, body: &serde_json::Value) -> tiny_http::Response<std::io::Cursor<Vec<u8>>> {
    let body_str = serde_json::to_string(body).unwrap_or_default();
    let data = std::io::Cursor::new(body_str.into_bytes());
    let status_code = tiny_http::StatusCode(status);
    let headers = vec![
        tiny_http::Header::from_bytes("Content-Type", "application/json").unwrap(),
        tiny_http::Header::from_bytes("Access-Control-Allow-Origin", "*").unwrap(),
    ];
    tiny_http::Response::new(status_code, headers, data, None, None)
}

fn read_body(request: &mut tiny_http::Request) -> String {
    let mut body = String::new();
    let _ = request.as_reader().read_to_string(&mut body);
    body
}

fn handle_health(store: &Arc<Mutex<TelemetryStore>>) -> tiny_http::Response<std::io::Cursor<Vec<u8>>> {
    let store = store.lock().unwrap();
    let health = store.all_health();

    let drivers_ok = health.values().filter(|h| h.status == crate::telemetry::DriverStatus::Ok).count();
    let drivers_degraded = health.values().filter(|h| h.status == crate::telemetry::DriverStatus::Degraded).count();
    let drivers_offline = health.values().filter(|h| h.status == crate::telemetry::DriverStatus::Offline).count();

    let status = if drivers_offline > 0 { "degraded" } else { "ok" };

    json_response(200, &serde_json::json!({
        "status": status,
        "drivers_ok": drivers_ok,
        "drivers_degraded": drivers_degraded,
        "drivers_offline": drivers_offline,
    }))
}

fn handle_status(
    store: &Arc<Mutex<TelemetryStore>>,
    control: &Arc<Mutex<ControlState>>,
    capacities: &Arc<RwLock<HashMap<String, f64>>>,
    energy: &Arc<Mutex<crate::energy::EnergyAccumulator>>,
) -> tiny_http::Response<std::io::Cursor<Vec<u8>>> {
    let capacities = capacities.read().unwrap().clone();
    let capacities = &capacities;
    let mut store = store.lock().unwrap();
    let control = control.lock().unwrap();

    // Aggregate readings
    // Grid: only from the site meter driver (not summed — they measure the same point)
    let grid_w: f64 = store.get(&control.site_meter_driver, &DerType::Meter)
        .map(|m| m.smoothed_w)
        .unwrap_or(0.0);

    // PV and battery: sum across all drivers (each system has its own)
    let pv_w: f64 = store.readings_by_type(&DerType::Pv).iter().map(|p| p.smoothed_w).sum();
    let bat_w: f64 = store.readings_by_type(&DerType::Battery).iter().map(|b| b.smoothed_w).sum();

    // Load = house consumption (energy balance: grid - pv - bat)
    // Filtered with slow Kalman because battery dispatch changes faster
    // than the grid meter responds — raw load has transient spikes.
    // The slow filter (process_noise=20W, measurement_noise=500W) tracks
    // real house load which only changes when appliances switch on/off.
    let raw_load = grid_w - pv_w - bat_w;
    let load_w = store.update_load(raw_load);

    // Weighted average SoC by capacity
    let bat_readings = store.readings_by_type(&DerType::Battery);
    let total_cap: f64 = bat_readings.iter()
        .filter_map(|b| capacities.get(&b.driver).copied())
        .sum();
    let avg_soc = if total_cap > 0.0 {
        bat_readings.iter()
            .filter_map(|b| {
                let cap = capacities.get(&b.driver)?;
                Some(b.soc.unwrap_or(0.0) * cap)
            })
            .sum::<f64>() / total_cap
    } else {
        0.0
    };

    // Per-driver details
    let mut drivers = serde_json::Map::new();
    for (name, health) in store.all_health() {
        let mut d = serde_json::Map::new();
        d.insert("status".into(), serde_json::json!(format!("{:?}", health.status)));
        d.insert("consecutive_errors".into(), serde_json::json!(health.consecutive_errors));
        d.insert("tick_count".into(), serde_json::json!(health.tick_count));

        if let Some(err) = &health.last_error {
            d.insert("last_error".into(), serde_json::json!(err));
        }

        if let Some(r) = store.get(name, &DerType::Meter) {
            d.insert("meter_w".into(), serde_json::json!(r.smoothed_w));
        }
        if let Some(r) = store.get(name, &DerType::Pv) {
            d.insert("pv_w".into(), serde_json::json!(r.smoothed_w));
        }
        if let Some(r) = store.get(name, &DerType::Battery) {
            d.insert("bat_w".into(), serde_json::json!(r.smoothed_w));
            if let Some(soc) = r.soc {
                d.insert("bat_soc".into(), serde_json::json!(soc));
            }
        }

        drivers.insert(name.clone(), serde_json::Value::Object(d));
    }

    // Dispatch targets
    let targets: Vec<_> = control.last_targets.iter().map(|t| {
        serde_json::json!({
            "driver": t.driver,
            "target_w": t.target_w,
            "clamped": t.clamped,
        })
    }).collect();

    json_response(200, &serde_json::json!({
        "version": env!("CARGO_PKG_VERSION"),
        "mode": control.mode,
        "grid_w": grid_w,
        "pv_w": pv_w,
        "bat_w": bat_w,
        "load_w": load_w,
        "bat_soc": avg_soc,
        "grid_target_w": control.grid_target_w,
        "peak_limit_w": control.peak_limit_w,
        "ev_charging_w": control.ev_charging_w,
        "energy": energy.lock().unwrap().state,
        "drivers": drivers,
        "dispatch": targets,
    }))
}

fn handle_get_mode(control: &Arc<Mutex<ControlState>>) -> tiny_http::Response<std::io::Cursor<Vec<u8>>> {
    let control = control.lock().unwrap();
    json_response(200, &serde_json::json!({
        "mode": control.mode,
        "grid_target_w": control.grid_target_w,
    }))
}

fn handle_set_mode(
    control: &Arc<Mutex<ControlState>>,
    request: &mut tiny_http::Request,
) -> tiny_http::Response<std::io::Cursor<Vec<u8>>> {
    let body = read_body(request);
    let parsed: Result<serde_json::Value, _> = serde_json::from_str(&body);

    match parsed {
        Ok(v) => {
            let mut control = control.lock().unwrap();

            if let Some(mode_str) = v.get("mode").and_then(|m| m.as_str()) {
                match serde_json::from_value::<Mode>(serde_json::json!(mode_str)) {
                    Ok(mode) => {
                        info!("mode changed to {:?}", mode);
                        control.mode = mode;
                    }
                    Err(_) => {
                        return json_response(400, &serde_json::json!({"error": "invalid mode"}));
                    }
                }
            }

            if let Some(order) = v.get("priority_order").and_then(|o| o.as_array()) {
                control.priority_order = order.iter()
                    .filter_map(|v| v.as_str().map(String::from))
                    .collect();
            }

            if let Some(weights) = v.get("weights").and_then(|w| w.as_object()) {
                control.weights = weights.iter()
                    .filter_map(|(k, v)| v.as_f64().map(|f| (k.clone(), f)))
                    .collect();
            }

            json_response(200, &serde_json::json!({"mode": control.mode}))
        }
        Err(e) => json_response(400, &serde_json::json!({"error": e.to_string()})),
    }
}

fn handle_set_target(
    control: &Arc<Mutex<ControlState>>,
    request: &mut tiny_http::Request,
) -> tiny_http::Response<std::io::Cursor<Vec<u8>>> {
    let body = read_body(request);
    let parsed: Result<serde_json::Value, _> = serde_json::from_str(&body);

    match parsed {
        Ok(v) => {
            if let Some(target) = v.get("grid_target_w").and_then(|t| t.as_f64()) {
                let mut control = control.lock().unwrap();
                info!("grid target changed to {}W", target);
                control.set_grid_target(target);
                json_response(200, &serde_json::json!({"grid_target_w": target}))
            } else {
                json_response(400, &serde_json::json!({"error": "missing grid_target_w"}))
            }
        }
        Err(e) => json_response(400, &serde_json::json!({"error": e.to_string()})),
    }
}

fn handle_energy(
    energy: &Arc<Mutex<crate::energy::EnergyAccumulator>>,
) -> tiny_http::Response<std::io::Cursor<Vec<u8>>> {
    let en = energy.lock().unwrap();
    json_response(200, &serde_json::to_value(&en.state).unwrap_or(serde_json::Value::Null))
}

fn handle_set_peak_limit(
    control: &Arc<Mutex<ControlState>>,
    request: &mut tiny_http::Request,
) -> tiny_http::Response<std::io::Cursor<Vec<u8>>> {
    let body = read_body(request);
    match serde_json::from_str::<serde_json::Value>(&body) {
        Ok(v) => {
            if let Some(limit) = v.get("peak_limit_w").and_then(|t| t.as_f64()) {
                let mut control = control.lock().unwrap();
                info!("peak limit → {}W", limit);
                control.peak_limit_w = limit;
                json_response(200, &serde_json::json!({"peak_limit_w": limit}))
            } else {
                json_response(400, &serde_json::json!({"error": "missing peak_limit_w"}))
            }
        }
        Err(e) => json_response(400, &serde_json::json!({"error": e.to_string()})),
    }
}

fn handle_set_ev_charging(
    control: &Arc<Mutex<ControlState>>,
    request: &mut tiny_http::Request,
) -> tiny_http::Response<std::io::Cursor<Vec<u8>>> {
    let body = read_body(request);
    match serde_json::from_str::<serde_json::Value>(&body) {
        Ok(v) => {
            let power = v.get("power_w").and_then(|t| t.as_f64()).unwrap_or(0.0);
            let active = v.get("active").and_then(|a| a.as_bool()).unwrap_or(power > 0.0);
            let effective = if active { power } else { 0.0 };
            let mut control = control.lock().unwrap();
            info!("EV charging → {}W (active: {})", effective, active);
            control.ev_charging_w = effective;
            json_response(200, &serde_json::json!({"ev_charging_w": effective, "active": active}))
        }
        Err(e) => json_response(400, &serde_json::json!({"error": e.to_string()})),
    }
}

fn handle_drivers(store: &Arc<Mutex<TelemetryStore>>) -> tiny_http::Response<std::io::Cursor<Vec<u8>>> {
    let store = store.lock().unwrap();
    let drivers: Vec<_> = store.all_health().values().map(|h| {
        serde_json::json!({
            "name": h.name,
            "status": format!("{:?}", h.status),
            "consecutive_errors": h.consecutive_errors,
            "tick_count": h.tick_count,
            "last_error": h.last_error,
        })
    }).collect();

    json_response(200, &serde_json::json!(drivers))
}

/// Handle GET /api/history?range=5m|1h|24h|3d[&points=N]
fn handle_history(
    state_store: &Arc<crate::state::StateStore>,
    path: &str,
) -> tiny_http::Response<std::io::Cursor<Vec<u8>>> {
    // Parse query params
    let query = path.split('?').nth(1).unwrap_or("");
    let mut range_s: u64 = 300; // default 5 min
    let mut max_points: usize = 120;

    for param in query.split('&') {
        let mut parts = param.splitn(2, '=');
        let key = parts.next().unwrap_or("");
        let val = parts.next().unwrap_or("");
        match key {
            "range" => {
                range_s = match val {
                    "5m" => 300,
                    "15m" => 900,
                    "1h" => 3600,
                    "6h" => 21600,
                    "24h" | "1d" => 86400,
                    "3d" => 259200,
                    _ => val.parse().unwrap_or(300),
                };
                // Pick sensible point count per range
                max_points = match val {
                    "5m" => 60,
                    "15m" => 90,
                    "1h" => 120,
                    "6h" => 144,
                    "24h" | "1d" => 144,
                    "3d" => 216,
                    _ => 120,
                };
            }
            "points" => {
                max_points = val.parse().unwrap_or(120);
            }
            _ => {}
        }
    }

    let now_ms = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64;
    let since_ms = now_ms.saturating_sub(range_s * 1000);

    let entries = state_store.load_history(since_ms, now_ms, max_points);

    // Return as array of objects
    let items: Vec<serde_json::Value> = entries.into_iter()
        .filter_map(|(_, json)| serde_json::from_str(&json).ok())
        .collect();

    json_response(200, &serde_json::json!({
        "range_s": range_s,
        "count": items.len(),
        "items": items,
    }))
}

fn serve_static(url_path: &str) -> tiny_http::Response<std::io::Cursor<Vec<u8>>> {
    let web_dir = Path::new("web");

    // Map "/" to "/index.html"
    let file_path = if url_path == "/" {
        web_dir.join("index.html")
    } else {
        // Strip leading slash and resolve relative to web/
        let relative = url_path.trim_start_matches('/');
        let candidate = web_dir.join(relative);

        // Prevent path traversal
        match candidate.canonicalize() {
            Ok(abs) => {
                let web_abs = match web_dir.canonicalize() {
                    Ok(a) => a,
                    Err(_) => return json_response(404, &serde_json::json!({"error": "not found"})),
                };
                if !abs.starts_with(&web_abs) {
                    return json_response(403, &serde_json::json!({"error": "forbidden"}));
                }
                abs
            }
            Err(_) => return json_response(404, &serde_json::json!({"error": "not found"})),
        }
    };

    match std::fs::read(&file_path) {
        Ok(contents) => {
            let content_type = guess_content_type(&file_path);
            let data = std::io::Cursor::new(contents);
            let mut headers = vec![
                tiny_http::Header::from_bytes("Content-Type", content_type).unwrap(),
            ];
            // Always revalidate static assets — version bumps shouldn't require Cmd+Shift+R
            headers.push(
                tiny_http::Header::from_bytes("Cache-Control", "no-cache, must-revalidate").unwrap()
            );
            tiny_http::Response::new(
                tiny_http::StatusCode(200),
                headers,
                data,
                None,
                None,
            )
        }
        Err(_) => json_response(404, &serde_json::json!({"error": "not found"})),
    }
}

fn guess_content_type(path: &Path) -> &'static str {
    match path.extension().and_then(|e| e.to_str()) {
        Some("html") => "text/html; charset=utf-8",
        Some("css") => "text/css; charset=utf-8",
        Some("js") => "application/javascript; charset=utf-8",
        Some("json") => "application/json",
        Some("png") => "image/png",
        Some("svg") => "image/svg+xml",
        Some("ico") => "image/x-icon",
        _ => "application/octet-stream",
    }
}

/// GET /api/config — return current effective config
fn handle_get_config(
    current_config: &Arc<RwLock<Config>>,
) -> tiny_http::Response<std::io::Cursor<Vec<u8>>> {
    let cfg = current_config.read().unwrap();
    json_response(200, &serde_json::to_value(&*cfg).unwrap_or(serde_json::Value::Null))
}

/// Apply a new config: validate, save atomically, hot-reload subsystems, swap current.
/// Pure-ish — testable without HTTP. Returns Ok on success, Err with a user-facing message on failure.
pub fn apply_config_update(
    body: &str,
    current_config: &Arc<RwLock<Config>>,
    registry: &DriverRegistry,
    control: &Arc<Mutex<ControlState>>,
    config_path: &PathBuf,
) -> Result<(), (u16, String)> {
    let new_config: Config = serde_json::from_str(body)
        .map_err(|e| (400, format!("invalid config: {}", e)))?;

    crate::config_reload::save_atomic(config_path, &new_config)
        .map_err(|e| (500, format!("save failed: {}", e)))?;

    let old = current_config.read().unwrap().clone();

    {
        let mut ctrl = control.lock().unwrap();
        ctrl.set_grid_target(new_config.site.grid_target_w);
        ctrl.grid_tolerance_w = new_config.site.grid_tolerance_w;
        ctrl.slew_rate_w = new_config.site.slew_rate_w;
        ctrl.min_dispatch_interval_s = new_config.site.min_dispatch_interval_s;
    }

    registry.reload(&new_config.drivers);

    *current_config.write().unwrap() = new_config;

    info!("config updated via API (was {} drivers, now {} drivers)",
        old.drivers.len(),
        current_config.read().unwrap().drivers.len());
    Ok(())
}

/// POST /api/config — replace config, write to yaml atomically, hot-reload subsystems
fn handle_post_config(
    current_config: &Arc<RwLock<Config>>,
    registry: &DriverRegistry,
    control: &Arc<Mutex<ControlState>>,
    config_path: &PathBuf,
    request: &mut tiny_http::Request,
) -> tiny_http::Response<std::io::Cursor<Vec<u8>>> {
    let body = read_body(request);
    match apply_config_update(&body, current_config, registry, control, config_path) {
        Ok(()) => json_response(200, &serde_json::json!({"status": "ok"})),
        Err((code, msg)) => json_response(code, &serde_json::json!({"error": msg})),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::*;
    use crate::telemetry::TelemetryStore;
    use std::sync::atomic::AtomicBool;

    fn fixture() -> (
        tempfile::TempDir,
        PathBuf,
        Arc<RwLock<Config>>,
        Arc<Mutex<ControlState>>,
        DriverRegistry,
    ) {
        let dir = tempfile::tempdir().unwrap();
        let config_path = dir.path().join("config.yaml");

        let cfg = Config {
            site: SiteConfig {
                name: "Test".into(), control_interval_s: 5,
                grid_target_w: 0.0, grid_tolerance_w: 50.0,
                watchdog_timeout_s: 60, smoothing_alpha: 0.3,
                gain: 0.5, slew_rate_w: 500.0, min_dispatch_interval_s: 5,
            },
            fuse: FuseConfig { max_amps: 16.0, phases: 3, voltage: 230.0 },
            drivers: vec![DriverConfig {
                name: "a".into(), lua: "drivers/a.lua".into(),
                is_site_meter: true, battery_capacity_wh: 10000.0,
                mqtt: Some(MqttConnectionConfig {
                    host: "1.1.1.1".into(), port: 1883,
                    username: None, password: None,
                }),
                modbus: None,
            }],
            api: ApiConfig { port: 8080 },
            homeassistant: None, state: None, price: None, weather: None,
            batteries: std::collections::HashMap::new(),
        };
        crate::config_reload::save_atomic(&config_path, &cfg).unwrap();

        let current = Arc::new(RwLock::new(cfg));
        let control = Arc::new(Mutex::new(ControlState::new(0.0, 50.0, "a".into())));
        let store = Arc::new(Mutex::new(TelemetryStore::new(0.3)));
        let running = Arc::new(AtomicBool::new(true));
        let registry = DriverRegistry::new(store, 60, dir.path().to_path_buf(), running);

        (dir, config_path, current, control, registry)
    }

    #[test]
    fn apply_config_update_persists_and_applies_control_changes() {
        let (_dir, config_path, current, control, registry) = fixture();

        let mut new_cfg = current.read().unwrap().clone();
        new_cfg.site.grid_target_w = -750.0;
        new_cfg.site.grid_tolerance_w = 100.0;
        let body = serde_json::to_string(&new_cfg).unwrap();

        apply_config_update(&body, &current, &registry, &control, &config_path).expect("apply");

        // Control state updated
        let ctrl = control.lock().unwrap();
        assert_eq!(ctrl.grid_target_w, -750.0);
        assert_eq!(ctrl.grid_tolerance_w, 100.0);
        assert_eq!(ctrl.pid_controller.setpoint, -750.0);
        drop(ctrl);

        // current_config swapped
        assert_eq!(current.read().unwrap().site.grid_target_w, -750.0);

        // yaml persisted to disk
        let from_disk = Config::load(&config_path).unwrap();
        assert_eq!(from_disk.site.grid_target_w, -750.0);

        registry.shutdown_all();
    }

    #[test]
    fn apply_config_update_rejects_invalid_json() {
        let (_dir, config_path, current, control, registry) = fixture();
        let original = current.read().unwrap().clone();

        let result = apply_config_update("{not json", &current, &registry, &control, &config_path);
        let (code, msg) = result.expect_err("must reject");
        assert_eq!(code, 400);
        assert!(msg.contains("invalid config"), "msg: {}", msg);

        // current_config untouched
        assert_eq!(current.read().unwrap().site.name, original.site.name);

        registry.shutdown_all();
    }

    #[test]
    fn apply_config_update_rejects_missing_required_fields() {
        let (_dir, config_path, current, control, registry) = fixture();

        // Missing fuse field — should fail to deserialize into Config
        let bad = r#"{"site":{"name":"x"},"drivers":[],"api":{"port":8080}}"#;
        let result = apply_config_update(bad, &current, &registry, &control, &config_path);
        let (code, _) = result.expect_err("must reject");
        assert_eq!(code, 400);

        registry.shutdown_all();
    }

    #[test]
    fn get_config_returns_full_serializable_config() {
        let (_dir, _config_path, current, _control, registry) = fixture();

        let resp = handle_get_config(&current);
        // Inspect the response status — handle_get_config wraps via json_response(200,..)
        assert_eq!(resp.status_code(), tiny_http::StatusCode(200));

        // Also verify Config serializes losslessly
        let json = serde_json::to_value(&*current.read().unwrap()).unwrap();
        assert!(json.get("site").is_some());
        assert!(json.get("fuse").is_some());
        assert!(json.get("drivers").is_some());
        assert!(json.get("api").is_some());
        // Default-None sections still appear (as null)
        assert!(json.get("homeassistant").is_some());
        assert!(json.get("price").is_some());

        registry.shutdown_all();
    }

    #[test]
    fn apply_config_update_full_roundtrip_via_get_payload() {
        // Simulates the UI: GET → edit → POST. The body returned by GET must be
        // accepted by POST without modification.
        let (_dir, config_path, current, control, registry) = fixture();

        let payload = serde_json::to_string(&*current.read().unwrap()).unwrap();
        apply_config_update(&payload, &current, &registry, &control, &config_path)
            .expect("GET → POST roundtrip must succeed");

        registry.shutdown_all();
    }
}
