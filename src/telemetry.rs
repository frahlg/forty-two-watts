use std::collections::HashMap;
use std::time::Instant;

/// DER type classification
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub enum DerType {
    Meter,
    Pv,
    Battery,
}

impl DerType {
    pub fn from_str(s: &str) -> Option<Self> {
        match s {
            "meter" => Some(Self::Meter),
            "pv" => Some(Self::Pv),
            "battery" => Some(Self::Battery),
            _ => None,
        }
    }

    pub fn as_str(&self) -> &'static str {
        match self {
            Self::Meter => "meter",
            Self::Pv => "pv",
            Self::Battery => "battery",
        }
    }
}

/// Driver health status
#[derive(Debug, Clone, PartialEq)]
pub enum DriverStatus {
    Ok,
    Degraded,
    Offline,
}

/// 1D Kalman filter for power signal smoothing.
/// Adapts automatically to signal noise — noisy signals get smoothed more,
/// stable signals pass through faster.
#[derive(Debug, Clone)]
pub struct KalmanFilter1D {
    /// Current state estimate (watts)
    pub estimate: f64,
    /// Estimation uncertainty
    pub uncertainty: f64,
    /// Process noise (how much we expect the true value to change per step)
    process_noise: f64,
    /// Measurement noise (how noisy the sensor readings are)
    measurement_noise: f64,
    /// Whether this filter has been initialized
    initialized: bool,
}

impl KalmanFilter1D {
    /// Create a new Kalman filter.
    /// - process_noise: expected change between readings (e.g. 100 for power that varies ~100W/s)
    /// - measurement_noise: sensor noise (e.g. 50 for a meter with ±50W jitter)
    pub fn new(process_noise: f64, measurement_noise: f64) -> Self {
        Self {
            estimate: 0.0,
            uncertainty: 1000.0, // start uncertain
            process_noise,
            measurement_noise,
            initialized: false,
        }
    }

    /// Update with a new measurement. Returns the filtered estimate.
    pub fn update(&mut self, measurement: f64) -> f64 {
        if !self.initialized {
            self.estimate = measurement;
            self.uncertainty = self.measurement_noise;
            self.initialized = true;
            return measurement;
        }

        // Predict step: uncertainty grows by process noise
        let predicted_uncertainty = self.uncertainty + self.process_noise;

        // Kalman gain: how much to trust the measurement vs prediction
        // High gain = trust measurement more (noisy estimate or stable measurement)
        // Low gain = trust prediction more (stable estimate or noisy measurement)
        let gain = predicted_uncertainty / (predicted_uncertainty + self.measurement_noise);

        // Update step
        self.estimate = self.estimate + gain * (measurement - self.estimate);
        self.uncertainty = (1.0 - gain) * predicted_uncertainty;

        self.estimate
    }
}

/// A single DER reading with Kalman-filtered smoothing
#[derive(Debug, Clone)]
pub struct DerReading {
    pub driver: String,
    pub der_type: DerType,
    pub raw_w: f64,
    pub smoothed_w: f64,
    pub soc: Option<f64>,
    pub data: serde_json::Value,
    pub updated_at: Instant,
}

/// Per-driver health tracking
#[derive(Debug, Clone)]
pub struct DriverHealth {
    pub name: String,
    pub status: DriverStatus,
    pub last_success: Option<Instant>,
    pub consecutive_errors: u32,
    pub last_error: Option<String>,
    pub tick_count: u64,
}

impl DriverHealth {
    pub fn new(name: &str) -> Self {
        Self {
            name: name.to_string(),
            status: DriverStatus::Ok,
            last_success: None,
            consecutive_errors: 0,
            last_error: None,
            tick_count: 0,
        }
    }

    pub fn record_success(&mut self) {
        self.last_success = Some(Instant::now());
        self.consecutive_errors = 0;
        self.last_error = None;
        self.status = DriverStatus::Ok;
        self.tick_count += 1;
    }

    pub fn record_error(&mut self, err: &str) {
        self.consecutive_errors += 1;
        self.last_error = Some(err.to_string());
        self.tick_count += 1;

        if self.consecutive_errors >= 3 {
            self.status = DriverStatus::Degraded;
        }
    }

    pub fn set_offline(&mut self) {
        self.status = DriverStatus::Offline;
    }

    pub fn is_online(&self) -> bool {
        self.status != DriverStatus::Offline
    }
}

/// Central telemetry store with Kalman-filtered smoothing
pub struct TelemetryStore {
    readings: HashMap<String, DerReading>,
    filters: HashMap<String, KalmanFilter1D>,
    health: HashMap<String, DriverHealth>,
    process_noise: f64,
    measurement_noise: f64,
    /// Separate slow Kalman filter for load — house load changes slowly
    /// (appliances), but the computed value is noisy due to timing mismatch
    /// between grid meter and battery dispatch
    load_filter: KalmanFilter1D,
}

impl TelemetryStore {
    pub fn new(_alpha: f64) -> Self {
        Self {
            readings: HashMap::new(),
            filters: HashMap::new(),
            health: HashMap::new(),
            process_noise: 100.0,   // DER signals change ~100W between samples
            measurement_noise: 50.0, // meter jitter ~50W
            // Load filter: very slow — house load only changes when appliances
            // switch on/off (process_noise=20W), but the computed value is very
            // noisy due to timing mismatch between grid and battery (noise=500W)
            load_filter: KalmanFilter1D::new(20.0, 500.0),
        }
    }

    fn key(driver: &str, der_type: &DerType) -> String {
        format!("{}:{}", driver, der_type.as_str())
    }

    /// Update a DER reading with Kalman filtering
    pub fn update(&mut self, driver: &str, der_type: &DerType, data: serde_json::Value, raw_w: f64, soc: Option<f64>) {
        let key = Self::key(driver, der_type);

        let filter = self.filters.entry(key.clone())
            .or_insert_with(|| KalmanFilter1D::new(self.process_noise, self.measurement_noise));
        let smoothed_w = filter.update(raw_w);

        self.readings.insert(key, DerReading {
            driver: driver.to_string(),
            der_type: der_type.clone(),
            raw_w,
            smoothed_w,
            soc,
            data,
            updated_at: Instant::now(),
        });
    }

    pub fn get(&self, driver: &str, der_type: &DerType) -> Option<&DerReading> {
        self.readings.get(&Self::key(driver, der_type))
    }

    pub fn readings_by_type(&self, der_type: &DerType) -> Vec<&DerReading> {
        self.readings.values()
            .filter(|r| &r.der_type == der_type)
            .collect()
    }

    pub fn readings_by_driver(&self, driver: &str) -> Vec<&DerReading> {
        self.readings.values()
            .filter(|r| r.driver == driver)
            .collect()
    }

    pub fn driver_health_mut(&mut self, name: &str) -> &mut DriverHealth {
        self.health.entry(name.to_string())
            .or_insert_with(|| DriverHealth::new(name))
    }

    pub fn driver_health(&self, name: &str) -> Option<&DriverHealth> {
        self.health.get(name)
    }

    pub fn all_health(&self) -> &HashMap<String, DriverHealth> {
        &self.health
    }

    /// Update load estimate with slow Kalman filter
    /// Raw load = grid - pv - bat (correct but noisy during battery transients)
    /// Filtered load tracks real house consumption smoothly
    pub fn update_load(&mut self, raw_load: f64) -> f64 {
        self.load_filter.update(raw_load)
    }

    pub fn is_stale(&self, driver: &str, der_type: &DerType, timeout_s: u64) -> bool {
        match self.get(driver, der_type) {
            Some(reading) => reading.updated_at.elapsed().as_secs() > timeout_s,
            None => true,
        }
    }
}
