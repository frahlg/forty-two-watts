use std::collections::HashMap;
use tracing::{info, warn, debug};

use crate::telemetry::{TelemetryStore, DerType, DriverStatus};

/// EMS operating mode
#[derive(Debug, Clone, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Mode {
    /// No dispatch — both systems run autonomously
    Idle,
    /// Target grid_target_w (default 0 = self-consumption)
    SelfConsumption,
    /// Force all batteries to max charge
    Charge,
    /// One battery is primary, secondary only when primary saturated
    Priority,
    /// Custom weights instead of capacity-proportional
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
    pub clamped: bool, // was the target clamped by limits?
}

/// Control loop state with anti-oscillation parameters
pub struct ControlState {
    pub mode: Mode,
    pub grid_target_w: f64,
    pub grid_tolerance_w: f64,
    pub site_meter_driver: String,
    pub priority_order: Vec<String>,
    pub weights: HashMap<String, f64>,
    pub last_targets: Vec<DispatchTarget>,

    // Anti-oscillation: proportional gain (0-1, lower = less aggressive)
    pub gain: f64,
    // Anti-oscillation: max watts change per dispatch cycle
    pub slew_rate_w: f64,
    // Anti-oscillation: minimum seconds between dispatches
    pub min_dispatch_interval_s: u64,
    // Track when we last dispatched
    pub last_dispatch: Option<std::time::Instant>,
    // Previous per-driver targets (for slew rate limiting)
    prev_targets: HashMap<String, f64>,
}

impl ControlState {
    pub fn new(grid_target_w: f64, grid_tolerance_w: f64, site_meter_driver: String) -> Self {
        Self {
            mode: Mode::SelfConsumption,
            grid_target_w,
            grid_tolerance_w,
            site_meter_driver,
            priority_order: Vec::new(),
            weights: HashMap::new(),
            last_targets: Vec::new(),
            // Anti-oscillation defaults:
            // Only correct 40% of error each cycle — converges in ~3 cycles without overshoot
            gain: 0.4,
            // Max 300W change per cycle — prevents step changes that cause ringing
            slew_rate_w: 300.0,
            // Wait 10s between dispatches — batteries need time to settle
            min_dispatch_interval_s: 10,
            last_dispatch: None,
            prev_targets: HashMap::new(),
        }
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
            return compute_charge_all(store, driver_capacities);
        }
        _ => {}
    }

    // Read site meter (only the designated site meter driver, not all meters)
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

    // Command holdoff — don't re-dispatch until previous command has settled
    if let Some(last) = state.last_dispatch {
        if last.elapsed().as_secs() < state.min_dispatch_interval_s {
            debug!("grid={:.0}W, holdoff ({:.0}s since last dispatch)",
                grid_w, last.elapsed().as_secs_f32());
            return Vec::new(); // return empty = no dispatch this cycle
        }
    }

    // Compute error
    let error = grid_w - state.grid_target_w;

    // Deadband — don't adjust if within tolerance
    if error.abs() < state.grid_tolerance_w {
        debug!("grid={:.0}W, target={:.0}W, within tolerance ({:.0}W), holding",
            grid_w, state.grid_target_w, state.grid_tolerance_w);
        return Vec::new();
    }

    // Apply proportional gain — only correct a fraction of the error
    // This prevents overshoot: gain=0.4 means we correct 40% per cycle,
    // converging smoothly in ~3 cycles instead of oscillating
    let corrected_error = error * state.gain;

    debug!("grid={:.0}W, target={:.0}W, error={:.0}W, corrected={:.0}W (gain={:.1})",
        grid_w, state.grid_target_w, error, corrected_error, state.gain);

    // Compute raw per-battery targets based on mode
    let mut targets = match &state.mode {
        Mode::SelfConsumption => compute_proportional(&batteries, corrected_error, driver_capacities),
        Mode::Priority => compute_priority(&batteries, corrected_error, &state.priority_order),
        Mode::Weighted => compute_weighted(&batteries, corrected_error, &state.weights),
        _ => Vec::new(),
    };

    // Apply slew rate limit — don't change any target by more than slew_rate_w per cycle
    for target in &mut targets {
        if let Some(&prev) = state.prev_targets.get(&target.driver) {
            let delta = target.target_w - prev;
            if delta.abs() > state.slew_rate_w {
                let limited = prev + delta.signum() * state.slew_rate_w;
                debug!("slew limit {}: {:.0}W → {:.0}W (wanted {:.0}W)",
                    target.driver, prev, limited, target.target_w);
                target.target_w = limited;
                target.clamped = true;
            }
        }
    }

    // Apply fuse guard
    let targets = apply_fuse_guard(targets, store, fuse_max_w);

    // Update state
    state.last_dispatch = Some(std::time::Instant::now());
    for t in &targets {
        state.prev_targets.insert(t.driver.clone(), t.target_w);
    }
    state.last_targets = targets.clone();
    targets
}

/// Self-consumption: split correction proportionally by battery capacity.
/// Correction is negated error: positive error (importing) → negative correction (discharge).
fn compute_proportional(
    batteries: &[BatteryInfo],
    error: f64,
    capacities: &HashMap<String, f64>,
) -> Vec<DispatchTarget> {
    let total_cap: f64 = batteries.iter().map(|b| b.capacity_wh).sum();
    if total_cap == 0.0 {
        return Vec::new();
    }

    // Negate: positive error means importing → need negative (discharge) to compensate
    let correction = -error;

    batteries.iter().map(|bat| {
        let share = correction * (bat.capacity_wh / total_cap);
        let target = bat.current_w + share;
        let (clamped_target, was_clamped) = clamp_with_soc(target, bat.soc, bat.capacity_wh);
        DispatchTarget {
            driver: bat.driver.clone(),
            target_w: clamped_target,
            clamped: was_clamped,
        }
    }).collect()
}

/// Priority: primary battery handles all, secondary only when saturated
fn compute_priority(
    batteries: &[BatteryInfo],
    error: f64,
    priority_order: &[String],
) -> Vec<DispatchTarget> {
    let mut remaining = -error; // negate: import → discharge
    let mut targets = Vec::new();

    // Process in priority order
    for name in priority_order {
        if let Some(bat) = batteries.iter().find(|b| &b.driver == name) {
            let target = bat.current_w + remaining;
            let (clamped_target, was_clamped) = clamp_with_soc(target, bat.soc, bat.capacity_wh);
            let absorbed = clamped_target - bat.current_w;
            remaining -= absorbed;

            targets.push(DispatchTarget {
                driver: bat.driver.clone(),
                target_w: clamped_target,
                clamped: was_clamped,
            });
        }
    }

    // Any batteries not in priority order get 0 adjustment
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

/// Weighted: custom weights instead of capacity-proportional
fn compute_weighted(
    batteries: &[BatteryInfo],
    error: f64,
    weights: &HashMap<String, f64>,
) -> Vec<DispatchTarget> {
    let total_weight: f64 = batteries.iter()
        .map(|b| weights.get(&b.driver).copied().unwrap_or(1.0))
        .sum();

    if total_weight == 0.0 {
        return Vec::new();
    }

    let correction = -error; // negate: import → discharge

    batteries.iter().map(|bat| {
        let w = weights.get(&bat.driver).copied().unwrap_or(1.0);
        let share = correction * (w / total_weight);
        let target = bat.current_w + share;
        let (clamped_target, was_clamped) = clamp_with_soc(target, bat.soc, bat.capacity_wh);
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
    capacities.iter().filter_map(|(name, _cap)| {
        let health = store.driver_health(name)?;
        if !health.is_online() { return None; }
        // Positive = charging. Use a reasonable max charge power.
        Some(DispatchTarget {
            driver: name.clone(),
            target_w: 5000.0, // 5 kW charge (will be clamped by driver)
            clamped: false,
        })
    }).collect()
}

/// Clamp target power with SoC guards
/// Returns (clamped_value, was_clamped)
fn clamp_with_soc(target_w: f64, soc: f64, _capacity_wh: f64) -> (f64, bool) {
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

    // General power limits (reasonable defaults for home batteries)
    let max_power = 10000.0; // 10 kW
    if clamped.abs() > max_power {
        clamped = clamped.signum() * max_power;
        was_clamped = true;
    }

    (clamped, was_clamped)
}

/// Ensure total discharge + PV doesn't exceed fuse limit
fn apply_fuse_guard(
    mut targets: Vec<DispatchTarget>,
    store: &TelemetryStore,
    fuse_max_w: f64,
) -> Vec<DispatchTarget> {
    // Sum PV generation (negative convention in telemetry)
    let pvs = store.readings_by_type(&DerType::Pv);
    let total_pv_w: f64 = pvs.iter().map(|p| p.smoothed_w.abs()).sum();

    // Sum discharge power from targets (negative = discharge)
    let total_discharge_w: f64 = targets.iter()
        .filter(|t| t.target_w < 0.0)
        .map(|t| t.target_w.abs())
        .sum();

    let total_generation = total_pv_w + total_discharge_w;

    if total_generation > fuse_max_w {
        let scale = fuse_max_w / total_generation;
        warn!("fuse guard: total generation {:.0}W exceeds limit {:.0}W, scaling discharge by {:.2}",
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
