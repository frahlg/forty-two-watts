//! Per-battery online learning: RLS estimator for ARX(1) model.
//!
//! Each battery is modelled as a first-order ARX system:
//!
//!     y(t+1) = a · y(t) + b · u(t) + ε
//!
//! From which we derive:
//!
//!     τ (time constant)        = -Δt / ln(a)
//!     k (steady-state gain)    = b / (1 − a)
//!
//! Parameters `a` and `b` are estimated online with **Recursive Least Squares**
//! using a forgetting factor (~0.99 → ~100-cycle effective memory).
//!
//! The estimator runs every control cycle from observed (last_command, current_actual,
//! soc) tuples that the system already produces. No extra hardware probing.
//!
//! Saturation curves (max_charge_w(SoC) and max_discharge_w(SoC)) are tracked in
//! 5%-SoC buckets via `max(observed, decay·current)` so they slowly forget old
//! over-optimistic readings.
//!
//! A baseline (gain, τ) can be set by the self-tune flow; deviation of current
//! estimates from baseline is used as the **hardware health score**.

use serde::{Deserialize, Serialize};
use std::collections::VecDeque;

/// Forgetting factor for RLS — 0.99 ≈ 100-cycle effective window (~8 min at 5s)
const DEFAULT_FORGETTING: f64 = 0.99;

/// Initial covariance (high = trust new observations heavily until we have data)
const INITIAL_COV: f64 = 1000.0;

/// SoC bucket size (5% buckets → 21 entries from 0 to 1)
const SOC_BUCKET: f64 = 0.05;

/// Decay applied per update to saturation curve so old high-water marks fade
const SAT_DECAY: f64 = 0.9999;

/// Minimum command magnitude to update RLS (below this, signal is too small)
const MIN_COMMAND_FOR_UPDATE: f64 = 100.0;

/// Minimum actual change to update RLS (below this, no information)
const MIN_DELTA_FOR_UPDATE: f64 = 20.0;

/// Outlier rejection threshold (residual > N·σ → skip update)
const OUTLIER_SIGMA: f64 = 5.0;

/// How many gain history points to keep (for trend / health drift detection)
const GAIN_HISTORY_LEN: usize = 2000;

/// One ARX(1) model per battery. Updated online by RLS, persisted to redb.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BatteryModel {
    pub name: String,

    // ARX(1) parameters: y(t+1) = a·y(t) + b·u(t)
    pub a: f64,
    pub b: f64,

    // RLS state — covariance matrix P (2x2) flattened for serde simplicity
    pub p: [[f64; 2]; 2],
    pub n_samples: u64,
    pub residual_var_ema: f64,

    // Last observation (regressor for next update)
    pub last_u: f64,
    pub last_y: f64,
    pub has_prev: bool,

    // Saturation curves: (soc, max_observed_w). Sparse, stored as sorted vec.
    pub max_charge_curve: Vec<(f64, f64)>,
    pub max_discharge_curve: Vec<(f64, f64)>,

    // Smallest command magnitude that produced response — empirical deadband
    pub deadband_w: f64,

    // Hardware health: baseline set by self-tune; current vs baseline gives drift
    pub baseline_gain: Option<f64>,
    pub baseline_tau_s: Option<f64>,
    pub last_calibrated_ts_ms: Option<u64>,

    // Gain history for trend detection (degradation over weeks)
    pub gain_history: VecDeque<(u64, f64)>,

    pub last_updated_ts_ms: u64,
    pub forgetting_factor: f64,
}

impl BatteryModel {
    /// Sensible defaults for a fresh battery — assumes near-instantaneous response
    /// with unity gain. RLS will refine over the first few hundred samples.
    pub fn new(name: &str) -> Self {
        Self {
            name: name.to_string(),
            a: 0.7,                  // moderate memory (τ ≈ 14s at 5s sample)
            b: 0.3,                  // moderate gain (steady-state ≈ 1.0)
            p: [[INITIAL_COV, 0.0], [0.0, INITIAL_COV]],
            n_samples: 0,
            residual_var_ema: 1000.0, // start uncertain
            last_u: 0.0,
            last_y: 0.0,
            has_prev: false,
            max_charge_curve: Vec::new(),
            max_discharge_curve: Vec::new(),
            deadband_w: 50.0,
            baseline_gain: None,
            baseline_tau_s: None,
            last_calibrated_ts_ms: None,
            gain_history: VecDeque::new(),
            last_updated_ts_ms: 0,
            forgetting_factor: DEFAULT_FORGETTING,
        }
    }

    /// Raw steady-state gain k = b / (1 − a). May be unphysical if model hasn't
    /// converged or has been corrupted — callers should sanity-check.
    pub fn steady_state_gain_raw(&self) -> f64 {
        let denom = 1.0 - self.a;
        if denom.abs() < 1e-3 { return 1.0; }
        self.b / denom
    }

    /// Steady-state gain k clamped to physically plausible range [0.3, 1.5].
    /// Use this for display / control where a sane number is required.
    pub fn steady_state_gain(&self) -> f64 {
        self.steady_state_gain_raw().clamp(0.3, 1.5)
    }

    /// Time constant τ = -Δt / ln(a). At dt=5s: a=0.5 → τ=7.2s, a=0.9 → τ=47s.
    /// Very low `a` (set by clean self-tune of fast batteries) gives τ < dt, which
    /// is physically meaningful: "responds within one sample period."
    pub fn time_constant_s(&self, dt_s: f64) -> f64 {
        if self.a >= 0.999 || self.a <= 0.0 { return 999.0; }
        (-dt_s / self.a.ln()).clamp(0.05, 60.0)
    }

    /// Confidence 0..1 — combines sample count + residual variance
    pub fn confidence(&self) -> f64 {
        let n_factor = (self.n_samples as f64 / 200.0).min(1.0);
        // Residual variance below 100 W² is excellent, above 10000 W² is poor
        let var_factor = (1.0 - (self.residual_var_ema / 10_000.0).min(1.0)).max(0.0);
        (n_factor * var_factor).clamp(0.0, 1.0)
    }

    /// Hardware health 0..1 — based on gain drift from baseline (set by self-tune)
    /// 1.0 = matches baseline, 0.0 = gain has drifted >50% from baseline
    pub fn health_score(&self) -> f64 {
        match self.baseline_gain {
            Some(baseline) if baseline.abs() > 0.1 => {
                let drift = (self.steady_state_gain() - baseline).abs() / baseline.abs();
                (1.0 - drift * 2.0).clamp(0.0, 1.0)
            }
            _ => 1.0, // no baseline yet → neutral healthy
        }
    }

    /// Estimated drift per day (slope of recent gain history). Negative = degrading.
    pub fn health_drift_per_day(&self) -> f64 {
        if self.gain_history.len() < 10 { return 0.0; }
        // Linear regression on (ts_ms, gain)
        let n = self.gain_history.len() as f64;
        let mean_t: f64 = self.gain_history.iter().map(|(t, _)| *t as f64).sum::<f64>() / n;
        let mean_g: f64 = self.gain_history.iter().map(|(_, g)| *g).sum::<f64>() / n;
        let mut num = 0.0;
        let mut den = 0.0;
        for (t, g) in &self.gain_history {
            let dt = *t as f64 - mean_t;
            num += dt * (g - mean_g);
            den += dt * dt;
        }
        if den < 1.0 { return 0.0; }
        let slope_per_ms = num / den;
        slope_per_ms * 86_400_000.0 // per ms → per day
    }

    /// Inverse model: given desired actual power, what command to send to compensate
    /// for steady-state gain. Falls back to passthrough if the raw learned gain is
    /// implausible (model hasn't converged, plant misbehaving, or RLS diverged).
    pub fn inverse(&self, target_actual: f64) -> f64 {
        let g_raw = self.steady_state_gain_raw();
        if g_raw.abs() < 0.3 || g_raw.abs() > 2.0 {
            return target_actual; // unhealthy → don't trust the model
        }
        let g = self.steady_state_gain(); // clamped, safe
        target_actual / g
    }

    /// Clamp a target to the empirically observed saturation envelope at this SoC.
    /// Returns (clamped_target, was_clamped).
    pub fn clamp_to_saturation(&self, target_w: f64, soc: f64) -> (f64, bool) {
        if target_w > 0.0 {
            let max = interpolate_curve(&self.max_charge_curve, soc).unwrap_or(5000.0);
            if target_w > max { (max, true) } else { (target_w, false) }
        } else if target_w < 0.0 {
            let max = interpolate_curve(&self.max_discharge_curve, soc).unwrap_or(5000.0);
            if -target_w > max { (-max, true) } else { (target_w, false) }
        } else {
            (0.0, false)
        }
    }

    /// Update RLS with a new (command, actual) observation. `dt_s` is the period
    /// between samples (typically the control_interval_s from config).
    /// Returns true if the model was updated, false if observation was filtered out.
    pub fn update(&mut self, command: f64, actual: f64, soc: f64, dt_s: f64, now_ms: u64) -> bool {
        // Always update saturation curves (independent of RLS gating)
        update_saturation_curves(self, actual, soc);

        // Need a previous observation to form the regressor
        if !self.has_prev {
            self.last_u = command;
            self.last_y = actual;
            self.has_prev = true;
            return false;
        }

        // Gate: low signal → no update
        if self.last_u.abs() < MIN_COMMAND_FOR_UPDATE {
            self.last_u = command;
            self.last_y = actual;
            return false;
        }
        if (actual - self.last_y).abs() < MIN_DELTA_FOR_UPDATE && self.n_samples > 50 {
            // After warmup, ignore stationary points (no info)
            self.last_u = command;
            self.last_y = actual;
            return false;
        }

        // Regressor φ = [y(t-1), u(t-1)]
        let phi = [self.last_y, self.last_u];

        // Prediction
        let pred = self.a * phi[0] + self.b * phi[1];
        let err = actual - pred;

        // Outlier rejection
        let std = self.residual_var_ema.sqrt().max(10.0);
        if err.abs() > OUTLIER_SIGMA * std && self.n_samples > 20 {
            self.last_u = command;
            self.last_y = actual;
            return false;
        }

        // Standard RLS update:
        //   K  = P φ / (λ + φᵀ P φ)
        //   θ ← θ + K · err
        //   P ← (P − K φᵀ P) / λ
        let p = &self.p;
        let pp = [
            p[0][0] * phi[0] + p[0][1] * phi[1],
            p[1][0] * phi[0] + p[1][1] * phi[1],
        ];
        let denom = self.forgetting_factor + phi[0] * pp[0] + phi[1] * pp[1];
        if denom.abs() < 1e-9 {
            self.last_u = command;
            self.last_y = actual;
            return false;
        }
        let k = [pp[0] / denom, pp[1] / denom];

        let mut new_a = self.a + k[0] * err;
        let mut new_b = self.b + k[1] * err;

        // Stability bounds — keep parameters in sane physical range
        new_a = new_a.clamp(0.1, 0.99);
        new_b = new_b.clamp(-1.5, 1.5);

        self.a = new_a;
        self.b = new_b;

        // P update: P_new = (P - K · ppᵀ) / λ. Note pp = P φ, so K φᵀ P = K · ppᵀ.
        let new_p = [
            [
                (p[0][0] - k[0] * pp[0]) / self.forgetting_factor,
                (p[0][1] - k[0] * pp[1]) / self.forgetting_factor,
            ],
            [
                (p[1][0] - k[1] * pp[0]) / self.forgetting_factor,
                (p[1][1] - k[1] * pp[1]) / self.forgetting_factor,
            ],
        ];
        self.p = new_p;

        // EMA of squared residuals → for confidence + outlier rejection
        self.residual_var_ema = 0.95 * self.residual_var_ema + 0.05 * err * err;

        self.last_u = command;
        self.last_y = actual;
        self.n_samples += 1;
        self.last_updated_ts_ms = now_ms;

        // Track gain history for drift detection
        let g = self.steady_state_gain();
        self.gain_history.push_back((now_ms, g));
        while self.gain_history.len() > GAIN_HISTORY_LEN {
            self.gain_history.pop_front();
        }

        true
    }

    /// Set baseline from a clean self-tune fit. Future drift is measured relative to this.
    pub fn set_baseline(&mut self, gain: f64, tau_s: f64, now_ms: u64) {
        self.baseline_gain = Some(gain);
        self.baseline_tau_s = Some(tau_s);
        self.last_calibrated_ts_ms = Some(now_ms);
    }

    /// Directly set ARX parameters from a self-tune fit (overrides RLS state).
    /// Uses a wider lower bound on `a` than online updates because clean step-fit
    /// data justifies trusting fast time constants (τ < dt is physically possible).
    pub fn set_from_step_fit(&mut self, gain: f64, tau_s: f64, dt_s: f64) {
        let a = (-dt_s / tau_s.max(0.1)).exp().clamp(0.001, 0.99);
        let b = (1.0 - a) * gain;
        self.a = a;
        self.b = b.clamp(-1.5, 1.5);
        // Reset covariance — tells RLS we're confident in these new values
        self.p = [[10.0, 0.0], [0.0, 10.0]];
        self.residual_var_ema = 100.0;
    }
}

fn update_saturation_curves(m: &mut BatteryModel, actual: f64, soc: f64) {
    let bucket = (soc / SOC_BUCKET).round() * SOC_BUCKET;
    let bucket = bucket.clamp(0.0, 1.0);

    if actual > 0.0 {
        update_one_curve(&mut m.max_charge_curve, bucket, actual);
    } else if actual < 0.0 {
        update_one_curve(&mut m.max_discharge_curve, bucket, -actual);
    }

    // Decay all entries slightly each call so old over-optimistic peaks fade
    for entry in m.max_charge_curve.iter_mut() { entry.1 *= SAT_DECAY; }
    for entry in m.max_discharge_curve.iter_mut() { entry.1 *= SAT_DECAY; }
}

fn update_one_curve(curve: &mut Vec<(f64, f64)>, bucket: f64, value: f64) {
    match curve.binary_search_by(|e| e.0.partial_cmp(&bucket).unwrap_or(std::cmp::Ordering::Equal)) {
        Ok(idx) => {
            if value > curve[idx].1 {
                curve[idx].1 = value;
            }
        }
        Err(idx) => {
            curve.insert(idx, (bucket, value));
        }
    }
}

/// Linear interpolate `(soc, value)` curve. Returns None if curve is empty.
fn interpolate_curve(curve: &[(f64, f64)], soc: f64) -> Option<f64> {
    if curve.is_empty() { return None; }
    if soc <= curve[0].0 { return Some(curve[0].1); }
    if soc >= curve[curve.len() - 1].0 { return Some(curve[curve.len() - 1].1); }
    for w in curve.windows(2) {
        if soc >= w[0].0 && soc <= w[1].0 {
            let t = (soc - w[0].0) / (w[1].0 - w[0].0).max(1e-6);
            return Some(w[0].1 + t * (w[1].1 - w[0].1));
        }
    }
    Some(curve[curve.len() - 1].1)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Helper: run a synthetic ARX(1) plant `y(t+1) = a·y(t) + b·u(t)` with a known
    /// (a, b), feed RLS the observations, return the final estimated (a, b).
    fn simulate_and_fit(true_a: f64, true_b: f64, n: usize, command_pattern: impl Fn(usize) -> f64) -> BatteryModel {
        let mut m = BatteryModel::new("test");
        let mut y = 0.0;
        let mut now: u64 = 0;
        for i in 0..n {
            let u = command_pattern(i);
            // Plant step
            let new_y = true_a * y + true_b * u;
            // Feed RLS the (command, observed) pair
            m.update(u, new_y, 0.5, 5.0, now);
            y = new_y;
            now += 5000;
        }
        m
    }

    #[test]
    fn rls_converges_on_synthetic_arx() {
        // True system: a=0.6, b=0.4 → gain = 0.4/0.4 = 1.0, τ = -5/ln(0.6) ≈ 9.8s
        let m = simulate_and_fit(0.6, 0.4, 500, |i| {
            // Mix of step inputs at varying magnitudes
            let phase = (i / 30) % 4;
            match phase {
                0 => 1000.0,
                1 => -1000.0,
                2 => 500.0,
                _ => -500.0,
            }
        });
        // Allow some tolerance — RLS is noisy
        assert!((m.a - 0.6).abs() < 0.1, "expected a≈0.6, got {}", m.a);
        assert!((m.b - 0.4).abs() < 0.15, "expected b≈0.4, got {}", m.b);
        let g = m.steady_state_gain();
        assert!((g - 1.0).abs() < 0.15, "expected gain≈1.0, got {}", g);
    }

    #[test]
    fn rls_recovers_lower_gain() {
        // Battery with 0.85 effective gain (typical efficiency loss)
        // True a=0.5, b=0.425 → gain = 0.425/0.5 = 0.85
        let m = simulate_and_fit(0.5, 0.425, 500, |i| if (i / 20) % 2 == 0 { 1500.0 } else { -1500.0 });
        let g = m.steady_state_gain();
        assert!((g - 0.85).abs() < 0.15, "expected gain≈0.85, got {}", g);
    }

    #[test]
    fn parameters_stay_in_bounds_under_pure_noise() {
        // Random walk inputs — RLS should stay bounded
        let mut m = BatteryModel::new("noisy");
        let mut now = 0u64;
        for i in 0..1000 {
            let u = ((i * 137) % 5000) as f64 - 2500.0;
            let y = u * 0.9 + ((i * 71) % 200) as f64 - 100.0; // gain 0.9 + noise
            m.update(u, y, 0.5, 5.0, now);
            now += 5000;
        }
        assert!(m.a >= 0.1 && m.a <= 0.99, "a out of bounds: {}", m.a);
        assert!(m.b >= -1.5 && m.b <= 1.5, "b out of bounds: {}", m.b);
    }

    #[test]
    fn skips_low_signal_observations() {
        let mut m = BatteryModel::new("quiet");
        // Tiny commands → no RLS updates
        for _ in 0..100 {
            m.update(20.0, 18.0, 0.5, 5.0, 1000);
        }
        assert_eq!(m.n_samples, 0, "should skip low-signal samples");
    }

    #[test]
    fn outlier_rejection_kicks_in() {
        // Build up some residual variance with normal data first
        let mut m = simulate_and_fit(0.6, 0.4, 200, |i| if i % 2 == 0 { 1000.0 } else { -1000.0 });
        let n_before = m.n_samples;
        // Inject an obvious outlier — actual=10000 when prediction would be ~600
        m.last_u = 1000.0;
        m.last_y = 600.0;
        m.update(1000.0, 10_000.0, 0.5, 5.0, 9999);
        // Outlier is rejected so n_samples shouldn't bump (but last_u/y still update)
        assert_eq!(m.n_samples, n_before, "outlier should be rejected");
    }

    #[test]
    fn saturation_curves_grow_correctly() {
        let mut m = BatteryModel::new("sat");
        // Push observations across SoC range
        let mut now = 0u64;
        for i in 0..100 {
            let soc = (i as f64) / 100.0;
            let actual = if soc < 0.95 { 4500.0 } else { 1000.0 * (1.0 - soc) * 20.0 };
            m.update(5000.0, actual, soc, 5.0, now);
            now += 5000;
        }
        assert!(!m.max_charge_curve.is_empty());
        // Low SoC → high charge limit
        let low = interpolate_curve(&m.max_charge_curve, 0.5).unwrap();
        let high = interpolate_curve(&m.max_charge_curve, 0.98).unwrap();
        assert!(low > high, "expected derating at high SoC: low={} high={}", low, high);
    }

    #[test]
    fn inverse_model_compensates_gain() {
        let mut m = BatteryModel::new("inv");
        m.a = 0.5;
        m.b = 0.4; // gain = 0.8
        // Want actual = 1000W → command = 1000 / 0.8 = 1250W
        let cmd = m.inverse(1000.0);
        assert!((cmd - 1250.0).abs() < 1.0, "expected ≈1250W, got {}", cmd);
    }

    #[test]
    fn inverse_model_falls_back_when_unhealthy() {
        let mut m = BatteryModel::new("broken");
        m.a = 0.99;
        m.b = 0.001; // gain ≈ 0.1 → unhealthy
        let cmd = m.inverse(1000.0);
        // Should fall back to direct passthrough
        assert!((cmd - 1000.0).abs() < 1.0, "expected fallback to 1000, got {}", cmd);
    }

    #[test]
    fn clamp_to_saturation_uses_curve() {
        let mut m = BatteryModel::new("clamp");
        m.max_charge_curve = vec![(0.0, 5000.0), (0.5, 5000.0), (0.95, 1000.0), (1.0, 0.0)];
        let (c, was) = m.clamp_to_saturation(3000.0, 0.95);
        assert!(was);
        assert!((c - 1000.0).abs() < 1.0);
        let (c2, was2) = m.clamp_to_saturation(3000.0, 0.5);
        assert!(!was2);
        assert!((c2 - 3000.0).abs() < 1.0);
    }

    #[test]
    fn health_score_neutral_without_baseline() {
        let m = BatteryModel::new("nobaseline");
        assert_eq!(m.health_score(), 1.0);
    }

    #[test]
    fn health_score_degrades_with_drift() {
        let mut m = BatteryModel::new("drift");
        m.a = 0.5;
        m.b = 0.5; // current gain = 1.0
        m.baseline_gain = Some(1.0);
        assert!((m.health_score() - 1.0).abs() < 0.01);
        m.b = 0.4; // gain now 0.8 = 20% drift → health = 1 - 0.4 = 0.6
        let h = m.health_score();
        assert!((h - 0.6).abs() < 0.01, "expected ≈0.6, got {}", h);
    }

    #[test]
    fn time_constant_and_gain_relationship() {
        let mut m = BatteryModel::new("relations");
        m.a = 0.6;
        m.b = 0.4;
        let tau = m.time_constant_s(5.0);
        let g = m.steady_state_gain();
        // a=0.6 → τ = -5/ln(0.6) ≈ 9.79s
        assert!((tau - 9.79).abs() < 0.1);
        // b/(1-a) = 0.4/0.4 = 1.0
        assert!((g - 1.0).abs() < 0.01);
    }

    #[test]
    fn confidence_grows_with_samples() {
        let mut m = BatteryModel::new("conf");
        let conf_initial = m.confidence();
        // Simulate 300 good samples to maximize n_factor
        for i in 0..300 {
            m.n_samples = i;
            m.residual_var_ema = 50.0;
        }
        let conf_after = m.confidence();
        assert!(conf_after > conf_initial);
        assert!(conf_after > 0.9);
    }

    #[test]
    fn set_from_step_fit_updates_a_and_b() {
        let mut m = BatteryModel::new("calib");
        m.set_from_step_fit(0.92, 1.5, 5.0);
        let g = m.steady_state_gain();
        let tau = m.time_constant_s(5.0);
        assert!((g - 0.92).abs() < 0.05, "expected gain≈0.92, got {}", g);
        assert!((tau - 1.5).abs() < 0.3, "expected τ≈1.5s, got {}", tau);
    }

    #[test]
    fn baseline_gain_affects_health() {
        let mut m = BatteryModel::new("baseline");
        m.set_from_step_fit(1.0, 2.0, 5.0);
        m.set_baseline(1.0, 2.0, 1000);
        assert!((m.health_score() - 1.0).abs() < 0.05);
        // Drift the gain
        m.set_from_step_fit(0.7, 2.0, 5.0); // 30% drop → 60% health
        let h = m.health_score();
        assert!(h < 0.5, "drifted gain should drop health, got {}", h);
    }

    #[test]
    fn serializes_roundtrip_through_serde_json() {
        let mut m = BatteryModel::new("rt");
        m.update(1000.0, 950.0, 0.5, 5.0, 1000);
        m.update(1000.0, 960.0, 0.5, 5.0, 6000);
        let json = serde_json::to_string(&m).unwrap();
        let back: BatteryModel = serde_json::from_str(&json).unwrap();
        assert_eq!(back.name, "rt");
        assert!((back.a - m.a).abs() < 1e-9);
        assert!((back.b - m.b).abs() < 1e-9);
        assert_eq!(back.n_samples, m.n_samples);
    }

    #[test]
    fn drift_per_day_is_zero_with_flat_history() {
        let mut m = BatteryModel::new("flat");
        for i in 0..50 {
            m.gain_history.push_back((1_000_000 + i * 5000, 0.95));
        }
        let drift = m.health_drift_per_day();
        assert!(drift.abs() < 0.001, "flat history → ~0 drift, got {}", drift);
    }

    #[test]
    fn drift_per_day_detects_falling_gain() {
        let mut m = BatteryModel::new("falling");
        // Falling gain over 10 days: 1.0 → 0.9
        let day_ms = 86_400_000u64;
        for i in 0..100 {
            let t = i as u64 * day_ms / 10;
            let g = 1.0 - (i as f64 / 1000.0); // 1.0 → 0.9 over 100 points
            m.gain_history.push_back((t, g));
        }
        let drift = m.health_drift_per_day();
        // Expect ≈ -0.01/day
        assert!(drift < -0.005 && drift > -0.02, "expected ~-0.01/day drift, got {}", drift);
    }
}
