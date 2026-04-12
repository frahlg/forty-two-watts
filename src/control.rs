use std::collections::HashMap;
use std::time::Instant;
use pid::Pid;
use tracing::{info, warn, debug};

use crate::telemetry::{TelemetryStore, DerType};

/// EMS operating mode
#[derive(Debug, Clone, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Mode {
    Idle,
    SelfConsumption,
    Charge,
    Priority,
    Weighted,
}

impl Default for Mode {
    fn default() -> Self {
        Self::SelfConsumption
    }
}

/// Per-driver battery dispatch target
#[derive(Debug, Clone, serde::Serialize)]
pub struct DispatchTarget {
    pub driver: String,
    pub target_w: f64,
    pub clamped: bool,
}

/// Control loop state with PI controller and anti-oscillation
pub struct ControlState {
    pub mode: Mode,
    pub grid_target_w: f64,
    pub grid_tolerance_w: f64,
    pub site_meter_driver: String,
    pub priority_order: Vec<String>,
    pub weights: HashMap<String, f64>,
    pub last_targets: Vec<DispatchTarget>,

    // PI controller (replaces manual proportional gain)
    pid_controller: Pid<f64>,

    // Anti-oscillation: max watts change per dispatch cycle
    pub slew_rate_w: f64,
    // Anti-oscillation: minimum seconds between dispatches
    pub min_dispatch_interval_s: u64,
    // Track when we last dispatched
    pub last_dispatch: Option<Instant>,
    // Previous per-driver targets for slew rate limiting
    prev_targets: HashMap<String, f64>,
}

impl ControlState {
    pub fn new(grid_target_w: f64, grid_tolerance_w: f64, site_meter_driver: String) -> Self {
        // PI controller tuned for home battery systems:
        // - Kp = 0.4: moderate proportional response (40% of error per cycle)
        // - Ki = 0.05: slow integral to eliminate steady-state offset
        // - No derivative (Kd = 0): batteries have inherent lag, D would amplify noise
        // - Output limit: ±10kW total dispatch (both batteries combined)
        // - I limit: ±2000W to prevent integral windup during large transients
        let mut pid = Pid::new(grid_target_w, 10000.0);
        pid.p(0.4, 10000.0);   // Kp with proportional limit
        pid.i(0.05, 2000.0);   // Ki with anti-windup limit
        pid.d(0.0, 0.0);       // No derivative

        Self {
            mode: Mode::SelfConsumption,
            grid_target_w,
            grid_tolerance_w,
            site_meter_driver,
            priority_order: Vec::new(),
            weights: HashMap::new(),
            last_targets: Vec::new(),
            pid_controller: pid,
            slew_rate_w: 300.0,
            min_dispatch_interval_s: 10,
            last_dispatch: None,
            prev_targets: HashMap::new(),
        }
    }

    /// Update the grid target (also updates PID setpoint)
    pub fn set_grid_target(&mut self, target: f64) {
        self.grid_target_w = target;
        self.pid_controller.setpoint = target;
    }
}

/// Battery info for dispatch calculation
struct BatteryInfo {
    driver: String,
    capacity_wh: f64,
    current_w: f64,
    soc: f64,
    online: bool,
}

/// Compute dispatch targets for one control cycle
pub fn compute_dispatch(
    store: &TelemetryStore,
    state: &mut ControlState,
    driver_capacities: &HashMap<String, f64>,
    fuse_max_w: f64,
) -> Vec<DispatchTarget> {
    match state.mode {
        Mode::Idle => {
            debug!("mode=idle, no dispatch");
            state.last_targets = Vec::new();
            return Vec::new();
        }
        Mode::Charge => {
            let targets = compute_charge_all(store, driver_capacities);
            state.last_targets = targets.clone();
            return targets;
        }
        _ => {}
    }

    // Command holdoff — wait for batteries to settle
    if let Some(last) = state.last_dispatch {
        if last.elapsed().as_secs() < state.min_dispatch_interval_s {
            return Vec::new();
        }
    }

    // Read site meter (Kalman-filtered)
    let grid_w: f64 = store.get(&state.site_meter_driver, &DerType::Meter)
        .map(|m| m.smoothed_w)
        .unwrap_or(0.0);

    // Read batteries
    let batteries: Vec<BatteryInfo> = driver_capacities.iter()
        .filter_map(|(name, cap)| {
            let health = store.driver_health(name)?;
            let bat = store.get(name, &DerType::Battery)?;
            Some(BatteryInfo {
                driver: name.clone(),
                capacity_wh: *cap,
                current_w: bat.smoothed_w,
                soc: bat.soc.unwrap_or(0.5),
                online: health.is_online(),
            })
        })
        .filter(|b| b.online)
        .collect();

    if batteries.is_empty() {
        warn!("no online batteries, skipping dispatch");
        state.last_targets = Vec::new();
        return Vec::new();
    }

    // Deadband — don't adjust if within tolerance
    let error = grid_w - state.grid_target_w;
    if error.abs() < state.grid_tolerance_w {
        debug!("grid={:.0}W within tolerance, holding", grid_w);
        return Vec::new();
    }

    // PI controller: measurement=grid_w, setpoint=grid_target_w
    // When importing (grid > target): PID output is NEGATIVE → add to battery = discharge
    // When exporting (grid < target): PID output is POSITIVE → add to battery = charge
    // No negation needed — PID output IS the battery correction directly
    let pid_output = state.pid_controller.next_control_output(grid_w);
    let total_correction = pid_output.output;

    debug!("PI: grid={:.0}W target={:.0}W error={:.0}W P={:.0} I={:.0} correction={:.0}W",
        grid_w, state.grid_target_w, error,
        pid_output.p, pid_output.i, total_correction);

    // Distribute correction across batteries based on mode
    let mut targets = match &state.mode {
        Mode::SelfConsumption => distribute_proportional(&batteries, total_correction, driver_capacities),
        Mode::Priority => distribute_priority(&batteries, total_correction, &state.priority_order),
        Mode::Weighted => distribute_weighted(&batteries, total_correction, &state.weights),
        _ => Vec::new(),
    };

    // Apply slew rate limit per driver
    for target in &mut targets {
        if let Some(&prev) = state.prev_targets.get(&target.driver) {
            let delta = target.target_w - prev;
            if delta.abs() > state.slew_rate_w {
                target.target_w = prev + delta.signum() * state.slew_rate_w;
                target.clamped = true;
            }
        }
    }

    // Apply fuse guard
    let targets = apply_fuse_guard(targets, store, fuse_max_w);

    // Update state
    state.last_dispatch = Some(Instant::now());
    for t in &targets {
        state.prev_targets.insert(t.driver.clone(), t.target_w);
    }
    state.last_targets = targets.clone();

    info!("dispatch: grid={:.0}W → {}",
        grid_w,
        targets.iter().map(|t| format!("{}={:.0}W", t.driver, t.target_w)).collect::<Vec<_>>().join(" "));

    targets
}

/// Distribute total correction proportionally by battery capacity
fn distribute_proportional(
    batteries: &[BatteryInfo],
    total_correction: f64,
    _capacities: &HashMap<String, f64>,
) -> Vec<DispatchTarget> {
    let total_cap: f64 = batteries.iter().map(|b| b.capacity_wh).sum();
    if total_cap == 0.0 { return Vec::new(); }

    batteries.iter().map(|bat| {
        let share = total_correction * (bat.capacity_wh / total_cap);
        let target = bat.current_w + share;
        let (clamped_target, was_clamped) = clamp_with_soc(target, bat.soc);
        DispatchTarget {
            driver: bat.driver.clone(),
            target_w: clamped_target,
            clamped: was_clamped,
        }
    }).collect()
}

/// Primary battery handles all, secondary fills remainder
fn distribute_priority(
    batteries: &[BatteryInfo],
    total_correction: f64,
    priority_order: &[String],
) -> Vec<DispatchTarget> {
    let mut remaining = total_correction;
    let mut targets = Vec::new();

    for name in priority_order {
        if let Some(bat) = batteries.iter().find(|b| &b.driver == name) {
            let target = bat.current_w + remaining;
            let (clamped_target, was_clamped) = clamp_with_soc(target, bat.soc);
            remaining -= clamped_target - bat.current_w;
            targets.push(DispatchTarget {
                driver: bat.driver.clone(),
                target_w: clamped_target,
                clamped: was_clamped,
            });
        }
    }

    for bat in batteries {
        if !targets.iter().any(|t| t.driver == bat.driver) {
            targets.push(DispatchTarget {
                driver: bat.driver.clone(),
                target_w: bat.current_w,
                clamped: false,
            });
        }
    }

    targets
}

/// Custom weights for distribution
fn distribute_weighted(
    batteries: &[BatteryInfo],
    total_correction: f64,
    weights: &HashMap<String, f64>,
) -> Vec<DispatchTarget> {
    let total_weight: f64 = batteries.iter()
        .map(|b| weights.get(&b.driver).copied().unwrap_or(1.0))
        .sum();
    if total_weight == 0.0 { return Vec::new(); }

    batteries.iter().map(|bat| {
        let w = weights.get(&bat.driver).copied().unwrap_or(1.0);
        let share = total_correction * (w / total_weight);
        let target = bat.current_w + share;
        let (clamped_target, was_clamped) = clamp_with_soc(target, bat.soc);
        DispatchTarget {
            driver: bat.driver.clone(),
            target_w: clamped_target,
            clamped: was_clamped,
        }
    }).collect()
}

/// Force charge all batteries
fn compute_charge_all(
    store: &TelemetryStore,
    capacities: &HashMap<String, f64>,
) -> Vec<DispatchTarget> {
    capacities.iter().filter_map(|(name, _)| {
        let health = store.driver_health(name)?;
        if !health.is_online() { return None; }
        Some(DispatchTarget {
            driver: name.clone(),
            target_w: 5000.0,
            clamped: false,
        })
    }).collect()
}

/// Clamp target power with SoC guards
fn clamp_with_soc(target_w: f64, soc: f64) -> (f64, bool) {
    let mut clamped = target_w;
    let mut was_clamped = false;

    // Don't discharge below 10% SoC
    if soc < 0.10 && target_w < 0.0 {
        clamped = 0.0;
        was_clamped = true;
    }

    // Don't charge above 95% SoC
    if soc > 0.95 && target_w > 0.0 {
        clamped = 0.0;
        was_clamped = true;
    }

    // Power limits
    let max_power = 5000.0;
    if clamped.abs() > max_power {
        clamped = clamped.signum() * max_power;
        was_clamped = true;
    }

    (clamped, was_clamped)
}

/// Ensure total power doesn't exceed fuse limit
fn apply_fuse_guard(
    mut targets: Vec<DispatchTarget>,
    store: &TelemetryStore,
    fuse_max_w: f64,
) -> Vec<DispatchTarget> {
    let pvs = store.readings_by_type(&DerType::Pv);
    let total_pv_w: f64 = pvs.iter().map(|p| p.smoothed_w.abs()).sum();

    let total_discharge_w: f64 = targets.iter()
        .filter(|t| t.target_w < 0.0)
        .map(|t| t.target_w.abs())
        .sum();

    let total_generation = total_pv_w + total_discharge_w;

    if total_generation > fuse_max_w {
        let scale = fuse_max_w / total_generation;
        warn!("fuse guard: {:.0}W > {:.0}W limit, scaling by {:.2}",
            total_generation, fuse_max_w, scale);
        for target in &mut targets {
            if target.target_w < 0.0 {
                target.target_w *= scale;
                target.clamped = true;
            }
        }
    }

    targets
}
