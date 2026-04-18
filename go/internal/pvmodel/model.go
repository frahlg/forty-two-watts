// Package pvmodel is a self-learning digital twin for a PV array.
// It turns (clear_sky_w, cloud_cover, time-of-day) + measured AC output
// into a linear RLS model that captures system-specific effects the
// generic clear-sky formula misses:
//
//   - Orientation / tilt: a south-facing array and a west-facing one
//     with the same rated kW produce wildly different curves.
//   - Shading: a tree or chimney attenuates output at specific hours.
//   - Module degradation: old panels produce less than nameplate.
//   - Cloud-enhancement bias: light clouds sometimes increase output
//     (reflection), which the (1−cloud)^1.5 heuristic can't see.
//   - Snow / soiling persistence (slow baseline drift).
//
// We use RLS because it has exact-by-construction SGD-like behavior
// with guaranteed convergence, tolerates low sample rates (minutes),
// and matches the approach already used for battery dynamics in this
// codebase — so operators have one mental model instead of two.
//
// Feature vector (7 terms — 1st + 2nd time-of-day harmonic):
//
//	x = [ 1,
//	      clearsky_w,
//	      clearsky_w × (1 − cloud/100)^1.5,
//	      clearsky_w × sin(2π·hour/24), clearsky_w × cos(2π·hour/24),
//	      clearsky_w × sin(4π·hour/24), clearsky_w × cos(4π·hour/24) ]
//
// β=[0,0,rated/1000,0,0,0,0] reproduces the naive physics baseline, so
// starting the model there gives "as good as before" on day one while
// the remaining terms learn orientation + shading asymmetry — including
// sharper patterns (morning-only shade, afternoon tree line) thanks to
// the 2nd harmonic.
package pvmodel

import (
	"math"
	"time"
)

// NFeat is the number of features in the RLS regression.
const NFeat = 7

// Model is the learned PV predictor.
type Model struct {
	Beta       [NFeat]float64         `json:"beta"`
	P          [NFeat][NFeat]float64  `json:"p"` // covariance
	Forgetting float64                `json:"forgetting"`
	Samples    int64                  `json:"samples"`
	LastMs     int64                  `json:"last_ms"`
	MAE        float64                `json:"mae"`        // EMA of |err| (W)
	RatedW     float64                `json:"rated_w"`    // nominal plate rating (prior)
}

// NewModel returns a model anchored on the naive clear-sky prior.
func NewModel(ratedW float64) *Model {
	m := &Model{
		Forgetting: 0.995, // ~200-sample effective window
		RatedW:     ratedW,
	}
	// Large initial covariance → model quickly fits new evidence.
	for i := 0; i < NFeat; i++ {
		m.P[i][i] = 1000.0
	}
	// β[2] = rated / 1000 gives naive: P ≈ clearsky × cloudFactor × rated/1000.
	// Scale this: pv = rated × (clearsky/1000) × cloud_factor
	// So coefficient on clearsky*cloud_factor is rated/1000.
	if ratedW > 0 {
		m.Beta[2] = ratedW / 1000.0
	} else {
		m.Beta[2] = 1.0
	}
	return m
}

// Features returns the feature vector for a given forecast sample.
//
// Hour-of-day uses UTC so the harmonic phase is stable across DST
// transitions and matches sunpos's UTC convention (see sunpos.go).
func Features(clearSkyW, cloudPct float64, t time.Time) [NFeat]float64 {
	cloudFrac := cloudPct / 100.0
	if cloudFrac < 0 {
		cloudFrac = 0
	}
	if cloudFrac > 1 {
		cloudFrac = 1
	}
	cf := math.Pow(1-cloudFrac, 1.5)
	u := t.UTC()
	hour := float64(u.Hour()) + float64(u.Minute())/60.0
	h := 2 * math.Pi * hour / 24.0
	return [NFeat]float64{
		1.0,
		clearSkyW,
		clearSkyW * cf,
		clearSkyW * math.Sin(h),
		clearSkyW * math.Cos(h),
		clearSkyW * math.Sin(2*h),
		clearSkyW * math.Cos(2*h),
	}
}

// Predict returns the expected AC output in W (non-negative). Cold-start
// behavior: during the first WarmupSamples we blend the learned β with
// the naive physics prior so a wild β coefficient (which RLS can take a
// few samples to tame) doesn't produce an unreasonable forecast.
//
// After ~warmup samples we trust the learned model fully.
const WarmupSamples = 50

func (m Model) Predict(clearSkyW, cloudPct float64, t time.Time) float64 {
	// Learned prediction.
	x := Features(clearSkyW, cloudPct, t)
	var learned float64
	for i := 0; i < NFeat; i++ {
		learned += m.Beta[i] * x[i]
	}

	// Naive physics prior: rated × (clear_sky / 1000) × (1-cloud)^1.5.
	// Same formula forecast.EstimatePVW uses; kept local to avoid a
	// package dep and because we evaluate it on every predict call.
	cf := 1.0
	if cloudPct > 0 {
		c := cloudPct / 100.0
		if c > 1 {
			c = 1
		}
		cf = math.Pow(1-c, 1.5)
	}
	prior := m.RatedW * (clearSkyW / 1000.0) * cf

	// Trust = samples / WarmupSamples, clipped to [0, 1].
	// samples=0  → 100% prior.
	// samples≥50 → 100% learned.
	trust := float64(m.Samples) / float64(WarmupSamples)
	if trust > 1 {
		trust = 1
	}
	y := trust*learned + (1-trust)*prior

	if y < 0 {
		return 0
	}
	// Hard cap at 105% of nameplate. Anything above is RLS having a bad
	// day — fall back to the physics prior, which is bounded by construction.
	if m.RatedW > 0 && y > 1.05*m.RatedW {
		return prior
	}
	return y
}

// Update runs one RLS step. Skipped when clearSky < threshold (night /
// near-night — little signal, mostly noise), or when the residual is a
// large-σ outlier (sensor glitch, inverter restart).
func (m *Model) Update(clearSkyW, cloudPct float64, t time.Time, actualPVW float64) (updated bool) {
	if clearSkyW < 50 {
		return false
	}
	if actualPVW < 0 {
		return false
	}
	// Physical sanity envelope: anything wildly above nameplate is sensor
	// noise (inverter restart, transient) — never feed it to RLS.
	if m.RatedW > 0 && actualPVW > 1.2*m.RatedW {
		return false
	}
	x := Features(clearSkyW, cloudPct, t)
	var yHat float64
	for i := 0; i < NFeat; i++ {
		yHat += m.Beta[i] * x[i]
	}
	err := actualPVW - yHat
	// Cold-start outlier guard: before the MAE-based filter kicks in, reject
	// samples where the predicted value is already absurd (>2× rated). This
	// stops a single bad sample from cascading into wild β coefficients.
	if m.RatedW > 0 && math.Abs(yHat) > 2*m.RatedW {
		return false
	}
	// After warm-up, reject 10σ outliers. MAE is in W; use it as a proxy
	// for σ (scales with system size, unlike a hard-coded threshold).
	if m.Samples > 50 {
		band := math.Max(m.MAE*10, 200)
		if math.Abs(err) > band {
			return false
		}
	}

	// K = P·x / (λ + x^T·P·x)
	var Px [NFeat]float64
	for i := 0; i < NFeat; i++ {
		var s float64
		for j := 0; j < NFeat; j++ {
			s += m.P[i][j] * x[j]
		}
		Px[i] = s
	}
	var xPx float64
	for i := 0; i < NFeat; i++ {
		xPx += x[i] * Px[i]
	}
	denom := m.Forgetting + xPx
	var K [NFeat]float64
	for i := 0; i < NFeat; i++ {
		K[i] = Px[i] / denom
	}

	// β += K · err
	for i := 0; i < NFeat; i++ {
		m.Beta[i] += K[i] * err
	}

	// P = (P − K·xᵀ·P) / λ
	var newP [NFeat][NFeat]float64
	for i := 0; i < NFeat; i++ {
		for j := 0; j < NFeat; j++ {
			var kxTP float64
			for k := 0; k < NFeat; k++ {
				kxTP += K[i] * x[k] * m.P[k][j]
			}
			newP[i][j] = (m.P[i][j] - kxTP) / m.Forgetting
		}
	}
	m.P = newP

	m.Samples++
	m.LastMs = t.UnixMilli()
	// MAE EMA: gives a ~99-sample window; good for outlier banding.
	if m.Samples == 1 {
		m.MAE = math.Abs(err)
	} else {
		m.MAE = 0.99*m.MAE + 0.01*math.Abs(err)
	}
	return true
}

// Quality reports how confident we are in the model. 0 = untrained,
// 1.0+ = fully converged (matches rated_w → 5% MAE threshold).
func (m Model) Quality() float64 {
	if m.Samples < 30 || m.RatedW <= 0 {
		return 0
	}
	// Relative MAE vs. rated → inverse (lower MAE = higher quality).
	rel := m.MAE / m.RatedW
	if rel <= 0.05 {
		return 1.0
	}
	if rel >= 0.5 {
		return 0.0
	}
	return 1.0 - (rel-0.05)/0.45
}
