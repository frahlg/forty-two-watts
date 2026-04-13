//! Per-battery self-tune: orchestrated step response sequence.
//!
//! Pauses the site control loop, drives each battery through a known step
//! pattern, fits an ARX(1) model to the response curve, and writes the result
//! to BatteryModel as a confident baseline (used later for hardware health drift).
//!
//! Sequence per battery (~3 min):
//!
//!     t=  0s  stabilize at 0W
//!     t= 15s  step UP   → +1000W   (small step, measure τ + gain at low magnitude)
//!     t= 30s  return to 0W
//!     t= 45s  step DOWN → -1000W
//!     t= 60s  return to 0W
//!     t= 75s  HIGH chg  → +3000W   (probe upper saturation)
//!     t= 95s  return to 0W
//!     t=105s  HIGH dis  → -3000W
//!     t=125s  return to 0W
//!     t=135s  fit + write to BatteryModel
//!     t=140s  next battery (or restore site control)

use serde::{Deserialize, Serialize};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use crate::battery_model::BatteryModel;
use crate::control::{ControlState, Mode};

/// Step in the self-tune sequence.
#[derive(Debug, Clone, Copy, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SelfTuneStep {
    Stabilize,      // 0W, settle
    StepUpSmall,    // +1000W
    SettleUp,       // 0W, settle
    StepDownSmall,  // -1000W
    SettleDown,     // 0W, settle
    StepUpLarge,    // +3000W (saturation probe)
    SettleHighUp,   // 0W
    StepDownLarge,  // -3000W
    SettleHighDown, // 0W
    Fit,            // analyze data + write model
    Done,
}

impl SelfTuneStep {
    /// Duration of this step
    pub fn duration_s(&self) -> u64 {
        match self {
            Self::Stabilize => 15,
            Self::StepUpSmall => 15,
            Self::SettleUp => 15,
            Self::StepDownSmall => 15,
            Self::SettleDown => 15,
            Self::StepUpLarge => 20,
            Self::SettleHighUp => 10,
            Self::StepDownLarge => 20,
            Self::SettleHighDown => 10,
            Self::Fit => 1,
            Self::Done => 0,
        }
    }

    /// Command to send during this step
    pub fn command_w(&self) -> f64 {
        match self {
            Self::StepUpSmall => 1000.0,
            Self::StepDownSmall => -1000.0,
            Self::StepUpLarge => 3000.0,
            Self::StepDownLarge => -3000.0,
            _ => 0.0,
        }
    }

    /// Should we be collecting samples in this step? (Steps with active commands.)
    pub fn collecting(&self) -> bool {
        matches!(
            self,
            Self::StepUpSmall | Self::StepDownSmall | Self::StepUpLarge | Self::StepDownLarge
        )
    }

    pub fn next(self) -> Self {
        match self {
            Self::Stabilize => Self::StepUpSmall,
            Self::StepUpSmall => Self::SettleUp,
            Self::SettleUp => Self::StepDownSmall,
            Self::StepDownSmall => Self::SettleDown,
            Self::SettleDown => Self::StepUpLarge,
            Self::StepUpLarge => Self::SettleHighUp,
            Self::SettleHighUp => Self::StepDownLarge,
            Self::StepDownLarge => Self::SettleHighDown,
            Self::SettleHighDown => Self::Fit,
            Self::Fit => Self::Done,
            Self::Done => Self::Done,
        }
    }
}

/// One observation during a tune step.
#[derive(Debug, Clone)]
pub struct TuneSample {
    pub elapsed_s: f64,
    pub command: f64,
    pub actual: f64,
    pub soc: f64,
}

/// Snapshot of model parameters before/after — used for the diff report.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ModelSnapshot {
    pub gain: f64,
    pub tau_s: f64,
    pub deadband_w: f64,
    pub n_samples: u64,
    pub confidence: f64,
}

impl ModelSnapshot {
    pub fn from_model(m: &BatteryModel, dt_s: f64) -> Self {
        Self {
            gain: m.steady_state_gain(),
            tau_s: m.time_constant_s(dt_s),
            deadband_w: m.deadband_w,
            n_samples: m.n_samples,
            confidence: m.confidence(),
        }
    }
}

/// Per-step result from fitting one step response curve.
#[derive(Debug, Clone)]
struct StepFit {
    pub gain: f64,
    pub tau_s: f64,
    pub valid: bool, // false if the response was too small to fit (e.g. saturated)
}

/// State of one battery being tuned.
#[derive(Debug, Clone)]
struct BatteryTuneState {
    pub name: String,
    pub step: SelfTuneStep,
    pub step_started_at: Instant,
    pub samples: Vec<TuneSample>,
    pub fit_results: Vec<StepFit>, // one per "active" step
}

impl BatteryTuneState {
    fn new(name: String) -> Self {
        Self {
            name,
            step: SelfTuneStep::Stabilize,
            step_started_at: Instant::now(),
            samples: Vec::new(),
            fit_results: Vec::new(),
        }
    }
}

/// Top-level self-tune coordinator. Lives in an Arc<Mutex<>> shared with API + control loop.
pub struct SelfTuneCoordinator {
    pub active: bool,
    pub batteries: Vec<String>,
    pub current_idx: usize,
    pub before: std::collections::HashMap<String, ModelSnapshot>,
    pub after: std::collections::HashMap<String, ModelSnapshot>,
    pub previous_mode: Option<Mode>,
    pub started_at: Option<Instant>,
    state: Option<BatteryTuneState>,
    pub last_error: Option<String>,
}

impl Default for SelfTuneCoordinator {
    fn default() -> Self { Self::new() }
}

impl SelfTuneCoordinator {
    pub fn new() -> Self {
        Self {
            active: false,
            batteries: Vec::new(),
            current_idx: 0,
            before: Default::default(),
            after: Default::default(),
            previous_mode: None,
            started_at: None,
            state: None,
            last_error: None,
        }
    }

    /// Start a tune for the given battery names. Captures `before` snapshots.
    pub fn start(
        &mut self,
        batteries: Vec<String>,
        models: &std::collections::HashMap<String, BatteryModel>,
        current_mode: Mode,
        dt_s: f64,
    ) -> Result<(), String> {
        if self.active {
            return Err("self-tune already running".into());
        }
        if batteries.is_empty() {
            return Err("at least one battery must be specified".into());
        }
        self.before.clear();
        self.after.clear();
        for name in &batteries {
            if let Some(m) = models.get(name) {
                self.before.insert(name.clone(), ModelSnapshot::from_model(m, dt_s));
            }
        }
        self.batteries = batteries;
        self.current_idx = 0;
        self.previous_mode = Some(current_mode);
        self.started_at = Some(Instant::now());
        self.state = Some(BatteryTuneState::new(self.batteries[0].clone()));
        self.active = true;
        self.last_error = None;
        Ok(())
    }

    pub fn cancel(&mut self) {
        self.active = false;
        self.state = None;
    }

    /// Returns Some(command_w) for the currently-tuning battery if active, else None.
    /// Other batteries continue with whatever the control loop computed.
    pub fn current_command(&self) -> Option<(String, f64)> {
        let st = self.state.as_ref()?;
        Some((st.name.clone(), st.step.command_w()))
    }

    /// Are we currently tuning a specific battery?
    pub fn is_tuning(&self, name: &str) -> bool {
        self.active && self.state.as_ref().map(|s| s.name == name).unwrap_or(false)
    }

    /// Status payload for the API.
    pub fn status_json(&self) -> serde_json::Value {
        let cur_step = self.state.as_ref().map(|s| s.step).unwrap_or(SelfTuneStep::Done);
        let cur_battery = self.state.as_ref().map(|s| s.name.clone()).unwrap_or_default();
        let elapsed = self.state
            .as_ref()
            .map(|s| s.step_started_at.elapsed().as_secs_f64())
            .unwrap_or(0.0);
        let total_elapsed = self.started_at.map(|t| t.elapsed().as_secs_f64()).unwrap_or(0.0);
        serde_json::json!({
            "active": self.active,
            "battery_index": self.current_idx,
            "battery_total": self.batteries.len(),
            "current_battery": cur_battery,
            "current_step": cur_step,
            "step_elapsed_s": elapsed,
            "total_elapsed_s": total_elapsed,
            "before": self.before,
            "after": self.after,
            "last_error": self.last_error,
        })
    }

    /// Tick — call once per control cycle. Advances the state machine, records samples,
    /// runs fit at the right time, transitions to next battery when done.
    /// Needs access to the model store so it can write fit results.
    pub fn tick(
        &mut self,
        actual_w_lookup: impl Fn(&str) -> Option<(f64, f64)>, // (actual_w, soc) per battery
        models: &mut std::collections::HashMap<String, BatteryModel>,
        dt_s: f64,
        now_ms: u64,
    ) {
        if !self.active { return; }
        let st = match self.state.as_mut() {
            Some(s) => s,
            None => { self.active = false; return; }
        };

        // Sample the active step
        if st.step.collecting() {
            if let Some((actual, soc)) = actual_w_lookup(&st.name) {
                st.samples.push(TuneSample {
                    elapsed_s: st.step_started_at.elapsed().as_secs_f64(),
                    command: st.step.command_w(),
                    actual,
                    soc,
                });
            }
        }

        // Time to advance?
        let elapsed = st.step_started_at.elapsed().as_secs();
        if elapsed < st.step.duration_s() { return; }

        // If the step we just finished was active, fit it
        if st.step.collecting() && !st.samples.is_empty() {
            let fit = fit_step_response(&st.samples);
            st.fit_results.push(fit);
            st.samples.clear();
        }

        // Advance step
        st.step = st.step.next();
        st.step_started_at = Instant::now();

        if st.step == SelfTuneStep::Fit {
            // Aggregate fits across all 4 active steps and write to model
            let valid_fits: Vec<&StepFit> = st.fit_results.iter().filter(|f| f.valid).collect();
            if valid_fits.is_empty() {
                self.last_error = Some(format!(
                    "self-tune for '{}' produced no usable step responses (battery may be saturated, offline, or the small commands fell within its deadband)",
                    st.name
                ));
            } else {
                let avg_gain = valid_fits.iter().map(|f| f.gain).sum::<f64>() / valid_fits.len() as f64;
                let avg_tau = valid_fits.iter().map(|f| f.tau_s).sum::<f64>() / valid_fits.len() as f64;
                let model = models.entry(st.name.clone()).or_insert_with(|| BatteryModel::new(&st.name));
                model.set_from_step_fit(avg_gain, avg_tau, dt_s);
                model.set_baseline(avg_gain, avg_tau, now_ms);
                tracing::info!(
                    "self-tune '{}': gain={:.3} τ={:.2}s (from {} valid step fits)",
                    st.name, avg_gain, avg_tau, valid_fits.len()
                );
                self.after.insert(st.name.clone(), ModelSnapshot::from_model(model, dt_s));
            }
        }

        if st.step == SelfTuneStep::Done {
            // Move to next battery, or finish
            self.current_idx += 1;
            if self.current_idx >= self.batteries.len() {
                self.active = false;
                self.state = None;
                tracing::info!("self-tune complete: {} batteries tuned", self.current_idx);
            } else {
                let name = self.batteries[self.current_idx].clone();
                self.state = Some(BatteryTuneState::new(name));
            }
        }
    }
}

/// Fit a first-order step response to extract τ and steady-state gain.
///
/// The step response of `y(t+1) = a·y(t) + b·u` for constant u is:
///
///     y(t) = k·u·(1 − exp(−t/τ))
///
/// where k = b/(1−a) is the steady-state gain and τ = −Δt/ln(a).
///
/// We estimate:
///   • k: from the average of the last 30% of samples (assumed near steady state)
///   • τ: time at which y reaches 63.2% of (k·u − initial_y)
fn fit_step_response(samples: &[TuneSample]) -> StepFit {
    if samples.len() < 5 {
        return StepFit { gain: 1.0, tau_s: 5.0, valid: false };
    }
    let u = samples[0].command;
    if u.abs() < 100.0 {
        return StepFit { gain: 1.0, tau_s: 5.0, valid: false };
    }

    let initial = samples[0].actual;
    // Steady-state estimate: average of last 30% of samples
    let tail_start = (samples.len() * 7) / 10;
    let tail = &samples[tail_start..];
    let y_ss: f64 = tail.iter().map(|s| s.actual).sum::<f64>() / tail.len() as f64;
    let delta = y_ss - initial;
    let gain = (y_ss / u).clamp(0.3, 1.5);

    // Reject if response is tiny (battery didn't move)
    if delta.abs() < 50.0 {
        return StepFit { gain, tau_s: 5.0, valid: false };
    }

    // Find time at 63.2% of the step
    let target = initial + 0.632 * delta;
    let mut tau = 5.0;
    for s in samples {
        // Crossing detection (sign-aware)
        if (delta > 0.0 && s.actual >= target) || (delta < 0.0 && s.actual <= target) {
            tau = s.elapsed_s.max(0.5);
            break;
        }
    }

    StepFit { gain, tau_s: tau.clamp(0.5, 30.0), valid: true }
}

/// Public alias for use in API status responses.
pub type SharedSelfTune = Arc<Mutex<SelfTuneCoordinator>>;

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    #[test]
    fn step_progression_visits_all_states_in_order() {
        let mut s = SelfTuneStep::Stabilize;
        let order = [
            SelfTuneStep::Stabilize, SelfTuneStep::StepUpSmall, SelfTuneStep::SettleUp,
            SelfTuneStep::StepDownSmall, SelfTuneStep::SettleDown,
            SelfTuneStep::StepUpLarge, SelfTuneStep::SettleHighUp,
            SelfTuneStep::StepDownLarge, SelfTuneStep::SettleHighDown,
            SelfTuneStep::Fit, SelfTuneStep::Done,
        ];
        for expected in order.iter() {
            assert_eq!(s, *expected);
            s = s.next();
        }
        // Done is terminal
        assert_eq!(s, SelfTuneStep::Done);
        assert_eq!(s.next(), SelfTuneStep::Done);
    }

    #[test]
    fn step_commands_match_expectation() {
        assert_eq!(SelfTuneStep::Stabilize.command_w(), 0.0);
        assert_eq!(SelfTuneStep::StepUpSmall.command_w(), 1000.0);
        assert_eq!(SelfTuneStep::StepDownSmall.command_w(), -1000.0);
        assert_eq!(SelfTuneStep::StepUpLarge.command_w(), 3000.0);
        assert_eq!(SelfTuneStep::StepDownLarge.command_w(), -3000.0);
    }

    #[test]
    fn collecting_only_during_active_steps() {
        assert!(!SelfTuneStep::Stabilize.collecting());
        assert!(SelfTuneStep::StepUpSmall.collecting());
        assert!(!SelfTuneStep::SettleUp.collecting());
        assert!(SelfTuneStep::StepUpLarge.collecting());
    }

    #[test]
    fn fit_step_recovers_known_gain_and_tau() {
        // Synthetic: command=1000W, gain=0.9 → ss=900W, τ≈3s
        let mut samples = Vec::new();
        let g = 0.9;
        let tau = 3.0;
        for i in 0..30 {
            let t = i as f64 * 0.5;
            let y = 0.0 + (g * 1000.0) * (1.0 - (-t / tau).exp());
            samples.push(TuneSample { elapsed_s: t, command: 1000.0, actual: y, soc: 0.5 });
        }
        let fit = fit_step_response(&samples);
        assert!(fit.valid);
        assert!((fit.gain - 0.9).abs() < 0.05, "expected gain≈0.9, got {}", fit.gain);
        assert!((fit.tau_s - 3.0).abs() < 1.0, "expected τ≈3s, got {}", fit.tau_s);
    }

    #[test]
    fn fit_step_rejects_tiny_response() {
        // Battery didn't respond — actual stays near zero
        let samples: Vec<_> = (0..20).map(|i| TuneSample {
            elapsed_s: i as f64 * 0.5,
            command: 1000.0,
            actual: 5.0, // basically zero
            soc: 0.5,
        }).collect();
        let fit = fit_step_response(&samples);
        assert!(!fit.valid);
    }

    #[test]
    fn fit_step_handles_negative_step() {
        // Discharge step
        let mut samples = Vec::new();
        let g = 0.95;
        let tau = 2.0;
        for i in 0..30 {
            let t = i as f64 * 0.5;
            let y = (g * -1000.0) * (1.0 - (-t / tau).exp());
            samples.push(TuneSample { elapsed_s: t, command: -1000.0, actual: y, soc: 0.5 });
        }
        let fit = fit_step_response(&samples);
        assert!(fit.valid);
        assert!((fit.gain - 0.95).abs() < 0.05);
    }

    #[test]
    fn coordinator_start_captures_before_snapshots() {
        let mut models = HashMap::new();
        let mut m = BatteryModel::new("a");
        m.a = 0.6;
        m.b = 0.4;
        models.insert("a".to_string(), m);

        let mut sc = SelfTuneCoordinator::new();
        sc.start(vec!["a".into()], &models, Mode::SelfConsumption, 5.0).unwrap();
        assert!(sc.active);
        assert_eq!(sc.before.len(), 1);
        assert!(sc.before.contains_key("a"));
    }

    #[test]
    fn coordinator_rejects_double_start() {
        let models = HashMap::new();
        let mut sc = SelfTuneCoordinator::new();
        sc.start(vec!["a".into()], &models, Mode::Idle, 5.0).unwrap();
        let result = sc.start(vec!["a".into()], &models, Mode::Idle, 5.0);
        assert!(result.is_err());
    }

    #[test]
    fn coordinator_rejects_empty_battery_list() {
        let models = HashMap::new();
        let mut sc = SelfTuneCoordinator::new();
        let result = sc.start(vec![], &models, Mode::Idle, 5.0);
        assert!(result.is_err());
    }

    #[test]
    fn cancel_clears_active_state() {
        let models = HashMap::new();
        let mut sc = SelfTuneCoordinator::new();
        sc.start(vec!["a".into()], &models, Mode::Idle, 5.0).unwrap();
        sc.cancel();
        assert!(!sc.active);
    }
}
