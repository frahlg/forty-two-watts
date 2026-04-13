use std::io::Read;
use std::sync::{Arc, Mutex};
use std::collections::HashMap;
use std::path::Path;
use tracing::{info, error};

use crate::telemetry::{TelemetryStore, DerType};
use crate::control::{ControlState, Mode};

/// Start the REST API server on a separate thread
pub fn start(
    port: u16,
    store: Arc<Mutex<TelemetryStore>>,
    control: Arc<Mutex<ControlState>>,
    driver_capacities: HashMap<String, f64>,
    state_store: Arc<crate::state::StateStore>,
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
                    ("GET", "/api/status") => handle_status(&store, &control, &driver_capacities),
                    ("GET", "/api/mode") => handle_get_mode(&control),
                    ("POST", "/api/mode") => handle_set_mode(&control, &mut request),
                    ("POST", "/api/target") => handle_set_target(&control, &mut request),
                    ("POST", "/api/peak_limit") => handle_set_peak_limit(&control, &mut request),
                    ("POST", "/api/ev_charging") => handle_set_ev_charging(&control, &mut request),
                    ("GET", "/api/drivers") => handle_drivers(&store),
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
    capacities: &HashMap<String, f64>,
) -> tiny_http::Response<std::io::Cursor<Vec<u8>>> {
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
        "mode": control.mode,
        "grid_w": grid_w,
        "pv_w": pv_w,
        "bat_w": bat_w,
        "load_w": load_w,
        "bat_soc": avg_soc,
        "grid_target_w": control.grid_target_w,
        "peak_limit_w": control.peak_limit_w,
        "ev_charging_w": control.ev_charging_w,
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
            let header = tiny_http::Header::from_bytes("Content-Type", content_type).unwrap();
            tiny_http::Response::new(
                tiny_http::StatusCode(200),
                vec![header],
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
