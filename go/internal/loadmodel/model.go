// Package loadmodel is a self-learning digital twin for household load.
//
// The MPC needs a forecast of consumption to decide when to charge
// cheaply. A flat baseline wastes most of the arbitrage opportunity:
// real homes have a morning peak, a midday dip, and a big evening peak.
// This package learns that pattern online from the same telemetry the
// control loop already produces.
//
// measured_load_w = grid_w − pv_w − bat_w
//
//	(site sign: grid import +, pv −, bat charge +  →  loads net out)
//
// The model is a linear RLS over seven features — bias plus three time-
// of-day harmonics — deliberately small so it converges from a few
// days of data and stays interpretable. Same shape as pvmodel so
// operators have one mental model.
//
// Feature vector:
//
//	x = [ 1,
//	      sin(2π·h/24), cos(2π·h/24),        // daily cycle
//	      sin(4π·h/24), cos(4π·h/24),        // morning + evening peaks
//	      sin(6π·h/24), cos(6π·h/24) ]       // sharper peak detail
//
// We don't include weekday/weekend or outdoor temperature in v1 to keep
// the surface small. Both are easy follow-ups once we see real data.
package loadmodel

import (
	"math"
	"time"
)

// NFeat is the number of features in the RLS regression.
const NFeat = 7

// Model is the learned load predictor.
type Model struct {
	Beta       [NFeat]float64         `json:"beta"`
	P          [NFeat][NFeat]float64  `json:"p"`
	Forgetting float64                `json:"forgetting"`
	Samples    int64                  `json:"samples"`
	LastMs     int64                  `json:"last_ms"`
	MAE        float64                `json:"mae"`        // EMA of |err| (W)
	PeakW      float64                `json:"peak_w"`     // reference peak for Quality()
}

// NewModel returns a model with a flat 500W baseline prior — a reasonable
// "always-on" estimate for a Swedish home. RLS will refine as data flows.
func NewModel(peakW float64) *Model {
	m := &Model{
		Forgetting: 0.995,
		PeakW:      peakW,
	}
	if m.PeakW <= 0 {
		m.PeakW = 5000 // 5 kW peak — typical single-family home
	}
	for i := 0; i < NFeat; i++ {
		m.P[i][i] = 1000.0
	}
	m.Beta[0] = 500.0 // baseline 500W — typical overnight load
	return m
}

// Features returns the feature vector for a given time.
func Features(t time.Time) [NFeat]float64 {
	hour := float64(t.Hour()) + float64(t.Minute())/60.0
	h := 2 * math.Pi * hour / 24.0
	return [NFeat]float64{
		1.0,
		math.Sin(h),
		math.Cos(h),
		math.Sin(2 * h),
		math.Cos(2 * h),
		math.Sin(3 * h),
		math.Cos(3 * h),
	}
}

// Predict returns the expected load in W (clamped to ≥ 0). Loads are
// non-negative by definition — if β produces a negative prediction,
// that's an artifact of the linear basis, not a physical scenario.
func (m Model) Predict(t time.Time) float64 {
	x := Features(t)
	var y float64
	for i := 0; i < NFeat; i++ {
		y += m.Beta[i] * x[i]
	}
	if y < 0 {
		return 0
	}
	// Cap at 3× peak as a sanity bound. A home that spikes 3× peak is
	// an anomaly (EV on top of sauna?); better to clip than trust a
	// linear extrapolation way outside training.
	if m.PeakW > 0 && y > 3*m.PeakW {
		y = 3 * m.PeakW
	}
	return y
}

// Update runs one RLS step. Rejects 10σ outliers once warmed up.
func (m *Model) Update(t time.Time, actualLoadW float64) (updated bool) {
	if actualLoadW < 0 {
		// Negative "load" means the measurement is off — probably a
		// sign-convention glitch during a driver restart. Skip.
		return false
	}
	x := Features(t)
	var yHat float64
	for i := 0; i < NFeat; i++ {
		yHat += m.Beta[i] * x[i]
	}
	err := actualLoadW - yHat
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
	for i := 0; i < NFeat; i++ {
		m.Beta[i] += K[i] * err
	}
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
	if m.Samples == 1 {
		m.MAE = math.Abs(err)
	} else {
		m.MAE = 0.99*m.MAE + 0.01*math.Abs(err)
	}
	return true
}

// Quality reports confidence in [0, 1]. 0 = untrained, 1 = MAE ≤ 5% of
// peak. Scales linearly between 5% and 50%.
func (m Model) Quality() float64 {
	if m.Samples < 30 || m.PeakW <= 0 {
		return 0
	}
	rel := m.MAE / m.PeakW
	if rel <= 0.05 {
		return 1.0
	}
	if rel >= 0.5 {
		return 0.0
	}
	return 1.0 - (rel-0.05)/0.45
}
