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
    /// Peak shaving: cap grid import at peak_limit_w, no action within [0, limit]
    PeakShaving,
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

    // Peak shaving: max acceptable grid import (only enforced in PeakShaving mode)
    pub peak_limit_w: f64,

    // EV charging signal: batteries ignore this much of grid import
    // (the EV is drawing it, batteries shouldn't cover it)
    pub ev_charging_w: f64,

    // PI controller
    pid_controller: Pid<f64>,

    pub slew_rate_w: f64,
    pub min_dispatch_interval_s: u64,
    pub last_dispatch: Option<Instant>,
    prev_targets: HashMap<String, f64>,
}

impl ControlState {
    pub fn new(grid_target_w: f64, grid_tolerance_w: f64, site_meter_driver: String) -> Self {
        // PI controller: steady over 15-minute energy periods, not twitchy
        // - Kp = 0.5: correct half the error each cycle — smooth, no overshoot
        // - Ki = 0.1: steady integral buildup ensures energy balance over time.
        //   If we undershoot for minutes, the I-term accumulates and catches up
        // - I limit: ±3000W — enough headroom for full load coverage
        let mut pid = Pid::new(grid_target_w, 10000.0);
        pid.p(0.5, 10000.0);
        pid.i(0.1, 3000.0);
        pid.d(0.0, 0.0);

        Self {
            mode: Mode::SelfConsumption,
            grid_target_w,
            grid_tolerance_w,
            site_meter_driver,
            priority_order: Vec::new(),
            weights: HashMap::new(),
            last_targets: Vec::new(),
            peak_limit_w: 5000.0,   // default peak limit: 5kW
            ev_charging_w: 0.0,     // no EV charging by default
            pid_controller: pid,
            slew_rate_w: 500.0,
            min_dispatch_interval_s: 5,
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
    let raw_grid_w: f64 = store.get(&state.site_meter_driver, &DerType::Meter)
        .map(|m| m.smoothed_w)
        .unwrap_or(0.0);

    // EV charging signal: subtract EV load from grid so batteries don't try to cover it.
    // EV gets electricity directly from grid, house load gets covered by PV+batteries.
    let grid_w = raw_grid_w - state.ev_charging_w;

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

    // Compute error based on mode
    let error = match state.mode {
        Mode::PeakShaving => {
            // Only act when grid import exceeds peak_limit
            // (allow any amount of export, allow import up to peak_limit)
            if grid_w > state.peak_limit_w {
                grid_w - state.peak_limit_w
            } else if grid_w < 0.0 {
                grid_w // exporting: charge batteries with surplus
            } else {
                0.0 // within acceptable band
            }
        }
        _ => grid_w - state.grid_target_w,
    };

    // Deadband — don't adjust if within 42W of The Answer
    if error.abs() < state.grid_tolerance_w {
        debug!("grid={:.0}W — Don't Panic. Within {}W of The Answer.", grid_w, state.grid_tolerance_w as i64);
        return Vec::new();
    }

    // PI controller: feed the computed error directly as (setpoint=0, measurement=error)
    // This lets PeakShaving mode use the same PI tuning
    let pid_output = state.pid_controller.next_control_output(
        state.grid_target_w + error
    );
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

/// Distribute total correction proportionally by battery capacity.
/// Computes the total desired battery power for the site, then splits it
/// across batteries by capacity. Both batteries converge to the same
/// proportional state instead of drifting independently.
fn distribute_proportional(
    batteries: &[BatteryInfo],
    total_correction: f64,
    _capacities: &HashMap<String, f64>,
) -> Vec<DispatchTarget> {
    let total_cap: f64 = batteries.iter().map(|b| b.capacity_wh).sum();
    if total_cap == 0.0 { return Vec::new(); }

    // Total battery power desired for the site
    let current_total: f64 = batteries.iter().map(|b| b.current_w).sum();
    let desired_total = current_total + total_correction;

    batteries.iter().map(|bat| {
        // Each battery gets its proportional share of the TOTAL desired power
        let target = desired_total * (bat.capacity_wh / total_cap);
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

    // Total desired battery power = current total + correction
    let current_total: f64 = batteries.iter().map(|b| b.current_w).sum();
    let desired_total = current_total + total_correction;

    batteries.iter().map(|bat| {
        let w = weights.get(&bat.driver).copied().unwrap_or(1.0);
        let target = desired_total * (w / total_weight);
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
