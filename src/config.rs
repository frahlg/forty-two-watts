use serde::Deserialize;
use std::path::Path;

#[derive(Debug, Deserialize)]
pub struct Config {
    pub site: SiteConfig,
    pub fuse: FuseConfig,
    pub drivers: Vec<DriverConfig>,
    pub api: ApiConfig,
    #[serde(default)]
    pub homeassistant: Option<HomeAssistantConfig>,
    #[serde(default)]
    pub state: Option<StateConfig>,
}

#[derive(Debug, Deserialize)]
pub struct SiteConfig {
    pub name: String,
    #[serde(default = "default_control_interval")]
    pub control_interval_s: u64,
    #[serde(default)]
    pub grid_target_w: f64,
    #[serde(default = "default_tolerance")]
    pub grid_tolerance_w: f64,
    #[serde(default = "default_watchdog")]
    pub watchdog_timeout_s: u64,
    #[serde(default = "default_alpha")]
    pub smoothing_alpha: f64,
}

#[derive(Debug, Deserialize)]
pub struct FuseConfig {
    pub max_amps: f64,
    #[serde(default = "default_phases")]
    pub phases: u8,
    #[serde(default = "default_voltage")]
    pub voltage: f64,
}

impl FuseConfig {
    pub fn max_power_w(&self) -> f64 {
        self.max_amps * self.voltage * self.phases as f64
    }
}

#[derive(Debug, Clone, Deserialize)]
pub struct DriverConfig {
    pub name: String,
    pub lua: String,
    #[serde(default)]
    pub is_site_meter: bool,
    #[serde(default)]
    pub battery_capacity_wh: f64,
    #[serde(default)]
    pub mqtt: Option<MqttConnectionConfig>,
    #[serde(default)]
    pub modbus: Option<ModbusConnectionConfig>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct MqttConnectionConfig {
    pub host: String,
    #[serde(default = "default_mqtt_port")]
    pub port: u16,
    #[serde(default)]
    pub username: Option<String>,
    #[serde(default)]
    pub password: Option<String>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct ModbusConnectionConfig {
    pub host: String,
    #[serde(default = "default_modbus_port")]
    pub port: u16,
    #[serde(default = "default_unit_id")]
    pub unit_id: u8,
}

#[derive(Debug, Deserialize)]
pub struct ApiConfig {
    #[serde(default = "default_api_port")]
    pub port: u16,
}

#[derive(Debug, Deserialize)]
pub struct HomeAssistantConfig {
    #[serde(default = "default_true")]
    pub enabled: bool,
    pub broker: String,
    #[serde(default = "default_mqtt_port")]
    pub port: u16,
    #[serde(default)]
    pub username: Option<String>,
    #[serde(default)]
    pub password: Option<String>,
    #[serde(default = "default_control_interval")]
    pub publish_interval_s: u64,
}

#[derive(Debug, Deserialize)]
pub struct StateConfig {
    #[serde(default = "default_state_path")]
    pub path: String,
}

// Defaults
fn default_control_interval() -> u64 { 5 }
fn default_tolerance() -> f64 { 50.0 }
fn default_watchdog() -> u64 { 60 }
fn default_alpha() -> f64 { 0.3 }
fn default_phases() -> u8 { 3 }
fn default_voltage() -> f64 { 230.0 }
fn default_mqtt_port() -> u16 { 1883 }
fn default_modbus_port() -> u16 { 502 }
fn default_unit_id() -> u8 { 1 }
fn default_api_port() -> u16 { 8080 }
fn default_true() -> bool { true }
fn default_state_path() -> String { "state.redb".to_string() }

impl Config {
    pub fn load(path: &Path) -> Result<Self, Box<dyn std::error::Error>> {
        let contents = std::fs::read_to_string(path)?;
        let config: Config = serde_yaml::from_str(&contents)?;
        config.validate()?;
        Ok(config)
    }

    fn validate(&self) -> Result<(), Box<dyn std::error::Error>> {
        if self.drivers.is_empty() {
            return Err("at least one driver must be configured".into());
        }

        let site_meters: Vec<_> = self.drivers.iter().filter(|d| d.is_site_meter).collect();
        if site_meters.is_empty() {
            return Err("at least one driver must be marked as is_site_meter".into());
        }

        for driver in &self.drivers {
            if driver.mqtt.is_none() && driver.modbus.is_none() {
                return Err(format!("driver '{}' must have either mqtt or modbus config", driver.name).into());
            }
        }

        if self.site.smoothing_alpha <= 0.0 || self.site.smoothing_alpha > 1.0 {
            return Err("smoothing_alpha must be between 0 (exclusive) and 1 (inclusive)".into());
        }

        Ok(())
    }
}
