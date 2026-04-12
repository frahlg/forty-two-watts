use std::sync::{Arc, Mutex};
use std::time::Duration;
use tracing::{info, warn, error, debug};

use crate::config::HomeAssistantConfig;
use crate::mqtt::client::MqttClient;
use crate::telemetry::{TelemetryStore, DerType};
use crate::control::{ControlState, Mode};

/// Home Assistant MQTT autodiscovery + sensor publishing + command subscription
pub fn start(
    config: HomeAssistantConfig,
    store: Arc<Mutex<TelemetryStore>>,
    control: Arc<Mutex<ControlState>>,
    driver_names: Vec<String>,
) -> std::thread::JoinHandle<()> {
    std::thread::Builder::new()
        .name("ha-mqtt".to_string())
        .spawn(move || {
            loop {
                match run_ha_bridge(&config, &store, &control, &driver_names) {
                    Ok(()) => return,
                    Err(e) => {
                        error!("HA MQTT: {}, retrying in 10s", e);
                        std::thread::sleep(Duration::from_secs(10));
                    }
                }
            }
        })
        .expect("failed to start HA MQTT thread")
}

fn run_ha_bridge(
    config: &HomeAssistantConfig,
    store: &Arc<Mutex<TelemetryStore>>,
    control: &Arc<Mutex<ControlState>>,
    driver_names: &[String],
) -> Result<(), Box<dyn std::error::Error>> {
    let mut client = MqttClient::connect(
        &config.broker,
        config.port,
        "forty-two-watts-ha",
        config.username.as_deref(),
        config.password.as_deref(),
    ).map_err(|e| format!("connect failed: {}", e))?;

    info!("HA MQTT: connected to {}:{}", config.broker, config.port);

    let prefix = "fortytwo";
    let discovery = "homeassistant";

    // Publish autodiscovery configs (retained so HA picks them up on restart)
    publish_autodiscovery(&mut client, driver_names, discovery, prefix);

    // Subscribe to command topics
    let queue = client.message_queue();
    client.subscribe(&format!("{}/command/#", prefix))
        .map_err(|e| format!("subscribe failed: {}", e))?;
    info!("HA MQTT: subscribed to {}/command/#", prefix);

    let interval = Duration::from_secs(config.publish_interval_s);

    loop {
        std::thread::sleep(interval);

        // Pump incoming messages
        let _ = client.pump();

        // Process commands
        for msg in queue.drain() {
            handle_command(&msg.topic, &msg.payload, control, prefix);
        }

        // Read telemetry — collect values then drop locks before publishing
        let (grid_w, pv_w, bat_w, avg_soc, load_w, mode, target, driver_data) = {
            let st = store.lock().unwrap();
            let ctrl = control.lock().unwrap();

            let grid_w = st.get(&ctrl.site_meter_driver, &DerType::Meter)
                .map(|m| m.smoothed_w).unwrap_or(0.0);
            let pv_w: f64 = st.readings_by_type(&DerType::Pv).iter().map(|p| p.smoothed_w).sum();
            let bat_w: f64 = st.readings_by_type(&DerType::Battery).iter().map(|b| b.smoothed_w).sum();
            let bat_readings = st.readings_by_type(&DerType::Battery);
            let avg_soc: f64 = if !bat_readings.is_empty() {
                bat_readings.iter().filter_map(|b| b.soc).sum::<f64>() / bat_readings.len() as f64
            } else { 0.0 };
            let load_w = grid_w - pv_w - bat_w;
            let mode = format!("{:?}", ctrl.mode).to_lowercase();
            let target = ctrl.grid_target_w;

            // Collect per-driver data
            let mut dd: Vec<(String, f64, f64, f64, Option<f64>, String)> = Vec::new();
            for name in driver_names.iter() {
                let meter = st.get(name, &DerType::Meter).map(|m| m.smoothed_w).unwrap_or(0.0);
                let pv = st.get(name, &DerType::Pv).map(|p| p.smoothed_w).unwrap_or(0.0);
                let bat = st.get(name, &DerType::Battery).map(|b| b.smoothed_w).unwrap_or(0.0);
                let soc = st.get(name, &DerType::Battery).and_then(|b| b.soc);
                let status = st.driver_health(name).map(|h| format!("{:?}", h.status)).unwrap_or_default();
                dd.push((name.clone(), meter, pv, bat, soc, status));
            }

            (grid_w, pv_w, bat_w, avg_soc, load_w, mode, target, dd)
        };

        // Publish site sensors
        let _ = client.publish_retained(&format!("{}/status/grid_w", prefix), false, format!("{:.0}", grid_w).as_bytes());
        let _ = client.publish_retained(&format!("{}/status/pv_w", prefix), false, format!("{:.0}", pv_w).as_bytes());
        let _ = client.publish_retained(&format!("{}/status/bat_w", prefix), false, format!("{:.0}", bat_w).as_bytes());
        let _ = client.publish_retained(&format!("{}/status/bat_soc", prefix), false, format!("{:.1}", avg_soc * 100.0).as_bytes());
        let _ = client.publish_retained(&format!("{}/status/load_w", prefix), false, format!("{:.0}", load_w).as_bytes());
        let _ = client.publish_retained(&format!("{}/status/mode", prefix), false, mode.as_bytes());
        let _ = client.publish_retained(&format!("{}/status/grid_target_w", prefix), false, format!("{:.0}", target).as_bytes());

        // Per-driver sensors
        for (name, meter, pv, bat, soc, status) in &driver_data {
            let _ = client.publish_retained(&format!("{}/drivers/{}/meter_w", prefix, name), false, format!("{:.0}", meter).as_bytes());
            let _ = client.publish_retained(&format!("{}/drivers/{}/pv_w", prefix, name), false, format!("{:.0}", pv).as_bytes());
            let _ = client.publish_retained(&format!("{}/drivers/{}/bat_w", prefix, name), false, format!("{:.0}", bat).as_bytes());
            if let Some(s) = soc {
                let _ = client.publish_retained(&format!("{}/drivers/{}/bat_soc", prefix, name), false, format!("{:.1}", s * 100.0).as_bytes());
            }
            let _ = client.publish_retained(&format!("{}/drivers/{}/status", prefix, name), false, status.as_bytes());
        }

        debug!("HA MQTT: published grid={:.0}W pv={:.0}W bat={:.0}W", grid_w, pv_w, bat_w);
    }
}

fn publish_autodiscovery(client: &mut MqttClient, driver_names: &[String], discovery: &str, prefix: &str) {
    let device = serde_json::json!({
        "identifiers": ["forty_two_watts"],
        "name": "Forty-Two Watts",
        "manufacturer": "Sourceful",
        "model": "forty-two-watts",
        "sw_version": env!("CARGO_PKG_VERSION"),
    });

    // Site sensors
    let sensors = vec![
        ("grid_power", "grid_w", "W", "power", "mdi:transmission-tower"),
        ("pv_power", "pv_w", "W", "power", "mdi:solar-power"),
        ("battery_power", "bat_w", "W", "power", "mdi:battery-charging"),
        ("battery_soc", "bat_soc", "%", "battery", "mdi:battery"),
        ("load_power", "load_w", "W", "power", "mdi:home-lightning-bolt"),
    ];

    for (id, field, unit, class, icon) in &sensors {
        let topic = format!("{}/sensor/fortytwo_{}/config", discovery, id);
        let payload = serde_json::json!({
            "name": format!("Forty-Two Watts {}", id.replace('_', " ")),
            "unique_id": format!("fortytwo_{}", id),
            "state_topic": format!("{}/status/{}", prefix, field),
            "unit_of_measurement": unit,
            "device_class": class,
            "icon": icon,
            "device": device,
            "state_class": "measurement",
        });
        let _ = client.publish_retained(&topic, true, payload.to_string().as_bytes());
    }

    // Mode select
    let mode_topic = format!("{}/select/fortytwo_mode/config", discovery);
    let mode_payload = serde_json::json!({
        "name": "Forty-Two Watts Mode",
        "unique_id": "fortytwo_mode",
        "state_topic": format!("{}/status/mode", prefix),
        "command_topic": format!("{}/command/mode", prefix),
        "options": ["idle", "self_consumption", "charge", "priority", "weighted"],
        "icon": "mdi:tune",
        "device": device,
    });
    let _ = client.publish_retained(&mode_topic, true, mode_payload.to_string().as_bytes());

    // Grid target number
    let target_topic = format!("{}/number/fortytwo_grid_target/config", discovery);
    let target_payload = serde_json::json!({
        "name": "Forty-Two Watts Grid Target",
        "unique_id": "fortytwo_grid_target",
        "state_topic": format!("{}/status/grid_target_w", prefix),
        "command_topic": format!("{}/command/grid_target_w", prefix),
        "unit_of_measurement": "W",
        "min": -5000,
        "max": 5000,
        "step": 50,
        "icon": "mdi:target",
        "device": device,
    });
    let _ = client.publish_retained(&target_topic, true, target_payload.to_string().as_bytes());

    // Per-driver sensors
    for name in driver_names {
        for (field, unit, class) in &[("meter_w", "W", "power"), ("pv_w", "W", "power"), ("bat_w", "W", "power"), ("bat_soc", "%", "battery")] {
            let id = format!("{}_{}", name, field);
            let topic = format!("{}/sensor/fortytwo_{}/config", discovery, id);
            let payload = serde_json::json!({
                "name": format!("42W {} {}", name, field.replace('_', " ")),
                "unique_id": format!("fortytwo_{}", id),
                "state_topic": format!("{}/drivers/{}/{}", prefix, name, field),
                "unit_of_measurement": unit,
                "device_class": class,
                "device": device,
                "state_class": "measurement",
            });
            let _ = client.publish_retained(&topic, true, payload.to_string().as_bytes());
        }
    }

    info!("HA MQTT: published {} autodiscovery configs", 5 + 1 + 1 + driver_names.len() * 4);
}

fn handle_command(topic: &str, payload: &str, control: &Arc<Mutex<ControlState>>, prefix: &str) {
    let cmd_prefix = format!("{}/command/", prefix);
    if let Some(cmd) = topic.strip_prefix(&cmd_prefix) {
        match cmd {
            "mode" => {
                let mode_str = payload.trim().trim_matches('"');
                if let Ok(mode) = serde_json::from_str::<Mode>(&format!("\"{}\"", mode_str)) {
                    info!("HA MQTT: mode → {:?}", mode);
                    control.lock().unwrap().mode = mode;
                } else {
                    warn!("HA MQTT: invalid mode '{}'", payload);
                }
            }
            "grid_target_w" => {
                if let Ok(target) = payload.trim().parse::<f64>() {
                    info!("HA MQTT: grid target → {}W", target);
                    control.lock().unwrap().set_grid_target(target);
                }
            }
            other => {
                debug!("HA MQTT: unknown command '{}'", other);
            }
        }
    }
}
