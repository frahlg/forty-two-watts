// Package loadmodel learns a household load profile online.
//
// Design choices driven by robustness / interpretability:
//
//  1. 168 buckets — one per (weekday, hour-of-day). An EMA per bucket.
//     Directly models the weekly pattern that dominates residential
//     load (weekend vs weekday, morning peak, evening peak, overnight
//     baseline) without having to fit non-linear basis functions.
//
//  2. Typical-home prior. Each bucket is seeded with a reasonable
//     Swedish-home default (300W overnight, 2000W morning/evening
//     peaks, 600W midday). Day-one predictions are useful; the model
//     refines from there.
//
//  3. Trust-weighted blending. Per-bucket trust = min(samples/20, 1).
//     A fresh bucket ignores its (noisy) EMA and returns the prior.
//     After ~20 samples through that bucket (3 weeks of 1-sample/min
//     yields ~60 samples per bucket per week), we trust observations.
//
//  4. Optional temperature correction. Outdoor temperature below 18°C
//     tracks heating load in homes with electric/heat-pump heating. We
//     maintain a global scalar `HeatingW_per_degC` and fit it online
//     via SGD on the prediction residual against (18 − temp_c), gated
//     on bucket trust. Houses unaffected by outdoor temperature (district
//     heating, all-electric pure-resistive baseboards on thermostats,
//     etc.) converge toward 0 W/°C. Adds 0 W when temp is unknown or
//     ≥ 18°C.
//
// The fallback-on-empty behavior makes this model safe on cold boot —
// the MPC always gets a plausible load estimate, never zero or wild.
package loadmodel

import (
	"math"
	"time"
)

// Buckets is the number of hour-of-week buckets: 7 days × 24 hours.
const Buckets = 7 * 24

// MinTrustSamples is how many samples we want in a bucket before we
// fully trust its EMA. Below this we blend with the prior. 8 ≈ two
// months of weekly observations, enough signal to outrank the prior.
const MinTrustSamples = 8

// HeatingReferenceC is the indoor setpoint the heating curve is
// relative to. Load proportional to max(setpoint − outdoor, 0).
const HeatingReferenceC = 18.0

// HeatingAlpha is the EMA weight applied to per-sample heating-slope
// estimates. ~0.01 picks up systematic bias within a few hundred cold
// samples (~1–2 weeks of every-15-min telemetry) while staying robust
// to noise and to the bucket↔coef joint-fit underdetermination.
const HeatingAlpha = 0.01

// HeatingMinDeltaT gates the online fit: a sample whose deltaT is too
// small (warm day, near reference) contributes too much noise via the
// 1/deltaT divisor in the SGD step. Skip it. Bucket EMA still updates.
const HeatingMinDeltaT = 3.0

// HeatingCoefMaxW is the physical upper bound for the learned slope.
// District-heating-replacement territory; well above any single-family
// home. Clamp prevents one anomalous sample from blowing up the fit.
const HeatingCoefMaxW = 1500.0

// implausibleLoadFactor bounds a single sample against the site's rated
// power. Above this the reading is a fault, not consumption. Same factor
// Predict clamps its output with, so training and prediction agree on
// what counts as physically possible.
const implausibleLoadFactor = 3.0

// outlierArmSamples is how much history the outlier filter needs before
// it starts rejecting anything. The old threshold of 50 armed the filter
// after 51 minutes at the 60 s sample cadence — calibrating a whole
// house's plausible-residual band on one arbitrary hour, usually a quiet
// one, because that is when a restart is least disruptive. A day of
// samples spans at least one full night-and-day cycle, so MAE reflects
// the range the house actually moves through.
const outlierArmSamples = 24 * 60

// outlierLevelShiftRun is how many consecutive same-direction rejections
// mean "the level moved" rather than "a spike". At the 60 s cadence this
// is ten minutes of the model being wrong the same way before it concedes
// and starts learning again. Short enough that a genuine shift is picked
// up within the hour, long enough that an oven or a car starting to
// charge is still filtered as the transient it is.
const outlierLevelShiftRun = 10

// Profile selects which learned occupancy profile is used for training
// and prediction.
type Profile string

const (
	ProfileHome Profile = "home"
	ProfileAway Profile = "away"
)

const awayPriorScale = 0.25

// Profiles returns the supported load-model profiles in display order.
func Profiles() []Profile {
	return []Profile{ProfileHome, ProfileAway}
}

func (p Profile) valid() bool {
	switch p {
	case ProfileHome, ProfileAway:
		return true
	default:
		return false
	}
}

// Bucket holds one hour-of-week's learned state.
type Bucket struct {
	Mean    float64 `json:"mean"` // EMA of observed load (W)
	Samples int64   `json:"samples"`
}

// Model is the hour-of-week + heating-gain predictor.
type Model struct {
	Bucket            [Buckets]Bucket `json:"bucket"`
	HeatingW_per_degC float64         `json:"heating_w_per_degc"`
	PeakW             float64         `json:"peak_w"`
	Samples           int64           `json:"samples"`
	LastMs            int64           `json:"last_ms"`
	MAE               float64         `json:"mae"`
	Alpha             float64         `json:"alpha"` // EMA coefficient for bucket updates
	PriorScale        float64         `json:"prior_scale,omitempty"`

	// RejectRun counts consecutive same-direction outlier rejections.
	// It is the model's only way to notice that it is not filtering noise
	// but refusing reality — see the outlier filter in Update.
	RejectRun         int  `json:"reject_run,omitempty"`
	RejectRunPositive bool `json:"reject_run_positive,omitempty"`
}

// typicalPrior returns an approximate W load for a given hour-of-week
// based on a generic single-family Swedish home. Peak dinner around
// 18:00–19:00, morning coffee around 07:00, weekend patterns shifted
// slightly later.
func typicalPrior(hourOfWeek int) float64 {
	weekday := hourOfWeek / 24
	hour := hourOfWeek % 24
	isWeekend := weekday >= 5 // Saturday (5), Sunday (6)
	base := 300.0             // overnight baseload
	morning := 2000.0 * math.Exp(-0.5*math.Pow(float64(hour-7)/1.2, 2))
	midday := 600.0 * math.Exp(-0.5*math.Pow(float64(hour-13)/2.5, 2))
	eveningH := 18.5
	if isWeekend {
		eveningH = 19.0
		morning *= 0.7 // sleep-in
	}
	evening := 2500.0 * math.Exp(-0.5*math.Pow((float64(hour)-eveningH)/1.3, 2))
	return base + morning + midday + evening
}

// NewModel returns a model seeded with the typical prior on every bucket.
func NewModel(peakW float64) *Model {
	return newModel(peakW, 1)
}

func newProfileModel(peakW float64, profile Profile) *Model {
	scale := 1.0
	if profile == ProfileAway {
		scale = awayPriorScale
	}
	return newModel(peakW, scale)
}

func newModel(peakW, priorScale float64) *Model {
	m := &Model{
		PeakW:      peakW,
		Alpha:      0.1, // new sample gets 10% weight in EMA
		PriorScale: priorScale,
	}
	if m.PeakW <= 0 {
		m.PeakW = 5000
	}
	for i := 0; i < Buckets; i++ {
		m.Bucket[i].Mean = m.prior(i)
		m.Bucket[i].Samples = 0
	}
	return m
}

func (m Model) prior(hourOfWeek int) float64 {
	scale := m.PriorScale
	if scale <= 0 {
		scale = 1
	}
	return typicalPrior(hourOfWeek) * scale
}

// repairPoisonedBuckets resets bucket.Mean back to the prior for any bucket
// whose stored mean has drifted below a floor of prior*poisonFloor. This
// repairs models that were trained before the heating-subtraction guard was
// in place: when heatEst exceeded actualLoad the code clamped baseSample to
// 0, causing the EMA to decay toward zero over many cold-weather samples even
// though a real baseline load (fridge, server, standby) always exists.
//
// Samples count is left intact — the data was genuinely observed, we just
// can't trust the mean it produced. Setting Samples=0 would reset trust to 0
// and re-expose the prior, but would also trigger the exact-running-mean path
// for the next 10 samples on warm days which is acceptable. Either way the
// repaired model quickly re-learns from warm-season observations.
//
// Floor is conservative (25% of prior) so we only touch buckets that are
// clearly below any plausible real consumption — a house at 75 W overnight
// would be unusual but possible, so we preserve those. A mean of 15 W for an
// overnight bucket that has prior=300 W is unambiguously poisoned.
const poisonFloor = 0.25

func (m *Model) repairPoisonedBuckets() {
	for i := 0; i < Buckets; i++ {
		p := m.prior(i)
		if m.Bucket[i].Mean < p*poisonFloor {
			m.Bucket[i].Mean = p
			m.Bucket[i].Samples = 0
		}
	}
}

// HourOfWeek computes 0..167 for a time. Monday = 0 through Sunday.
// Coerces to UTC so the bucket index stays stable across DST
// transitions (wall-clock 19:00 maps to a different bucket in summer
// vs. winter otherwise, silently misaligning the EMA).
func HourOfWeek(t time.Time) int {
	u := t.UTC()
	// time.Weekday: Sunday=0, Saturday=6. We shift so Monday=0.
	wd := (int(u.Weekday()) + 6) % 7
	return wd*24 + u.Hour()
}

// Predict returns the expected load (W, non-negative) at time t with
// outdoor temperature tempC (0 if unknown). Blends per-bucket EMA with
// the typical prior by sample count, then adds the heating correction.
func (m Model) Predict(t time.Time, tempC float64) float64 {
	idx := HourOfWeek(t)
	b := m.Bucket[idx]
	trust := float64(b.Samples) / MinTrustSamples
	if trust > 1 {
		trust = 1
	}
	prior := m.prior(idx)
	base := trust*b.Mean + (1-trust)*prior
	heating := 0.0
	if tempC < HeatingReferenceC {
		heating = m.HeatingW_per_degC * (HeatingReferenceC - tempC)
	}
	y := base + heating
	if y < 0 {
		return 0
	}
	if m.PeakW > 0 && y > 3*m.PeakW {
		y = 3 * m.PeakW
	}
	return y
}

// PredictNoTemp is a convenience that predicts without a temperature
// signal — useful when no forecast is available.
func (m Model) PredictNoTemp(t time.Time) float64 { return m.Predict(t, HeatingReferenceC) }

// Update runs one online update. Feed (now, actual_load_w, outdoor_temp_c).
// Pass 0 for tempC if unknown; we'll skip the heating fit in that case.
// Returns true when the update was applied (not filtered as an outlier).
func (m *Model) Update(t time.Time, actualLoadW, tempC float64) (updated bool) {
	if actualLoadW < 0 {
		return false
	}
	idx := HourOfWeek(t)
	b := &m.Bucket[idx]
	predicted := m.Predict(t, tempC)
	err := actualLoadW - predicted

	// ---- Online heating fit ----
	// Adapt HeatingW_per_degC from observed residuals before the outlier
	// filter so a wildly stale coefficient can recover: every cold sample
	// would otherwise look like an outlier vs the warm-day MAE, and no
	// data could ever pull the coefficient down. Bucket-trust gates the
	// fit because the residual derives the slope from the bucket
	// baseline; an untrusted bucket would feed prior error into the
	// heating estimate.
	//
	// SGD step on the squared-error loss: d/d(coef) ∝ −err · deltaT,
	// so coef ← coef + α · err / deltaT (the 1/deltaT cancels the
	// gradient's deltaT factor, giving a per-sample slope estimate).
	// HeatingMinDeltaT gates near-reference samples where 1/deltaT
	// amplifies noise. Clamp to [0, HeatingCoefMaxW]: floor at zero
	// (heating doesn't go negative physically); a household whose load
	// is unaffected by outdoor temperature gracefully settles at the
	// floor.
	if tempC < HeatingReferenceC-HeatingMinDeltaT && b.Samples >= MinTrustSamples {
		deltaT := HeatingReferenceC - tempC
		m.HeatingW_per_degC += HeatingAlpha * err / deltaT
		if m.HeatingW_per_degC < 0 {
			m.HeatingW_per_degC = 0
		}
		if m.HeatingW_per_degC > HeatingCoefMaxW {
			m.HeatingW_per_degC = HeatingCoefMaxW
		}
	}

	// Hard sanity bound, always on. A residual this far above the site's
	// rated draw is a measurement fault, not a household. It is deliberately
	// absolute — derived from configured hardware, never from what the model
	// has learned — so unlike the MAE band below it cannot be talked down by
	// a model that has mislearned, and it protects the first day before the
	// soft filter arms. Mirrors the ceiling Predict already applies.
	if m.PeakW > 0 && actualLoadW > implausibleLoadFactor*m.PeakW {
		return false
	}

	// Outlier filter: once we have some history, reject 10× MAE residuals.
	//
	// Two guards keep this from locking the model out of a load level it
	// has not seen before. A rejected sample updates neither MAE nor
	// Samples, so without them the band can never grow in response to
	// being persistently wrong, and a model calibrated on one quiet hour
	// rejects the real house forever. Measured before the fix: an hour of
	// 400 W overnight load gave MAE 57 W and a 570 W band; a subsequent
	// week at 5 kW was rejected in full, 100% of samples, and the
	// prediction never moved off 1794 W.
	if m.Samples > outlierArmSamples {
		band := math.Max(m.MAE*10, 200)
		if math.Abs(err) > band {
			// A run of same-direction rejections is not noise — it is the
			// house telling us the level moved. Spikes are short and
			// alternate in sign; a real shift is sustained. Let the run
			// through so the band can re-fit, and keep the filter's actual
			// job (rejecting the isolated spike) intact.
			sameDirection := (err > 0) == (m.RejectRunPositive)
			if m.RejectRun > 0 && sameDirection {
				m.RejectRun++
			} else {
				m.RejectRun = 1
				m.RejectRunPositive = err > 0
			}
			if m.RejectRun < outlierLevelShiftRun {
				return false
			}
			// The run is long enough to be real. Widening the band by
			// exactly enough to admit this residual is what lets the model
			// re-fit: simply accepting the one sample and resetting the run
			// leaves the band untouched, so the next nine are rejected too
			// and the model crawls in at one sample in ten. Measured that
			// way, a day at a new 3 kW level only reached 1650 W.
			//
			// max() means the band never shrinks here, and the ordinary EMA
			// below takes over from this point — so the widening is a floor
			// set by observation, not a permanent loosening.
			m.MAE = math.Max(m.MAE, math.Abs(err)/10)
			m.RejectRun = 0
		} else {
			m.RejectRun = 0
		}
	}

	// Bucket update: exact running mean for the first 10 samples (crisp
	// early convergence), EMA after (smooth drift as the home evolves).
	// Subtract the current heating-gain estimate so the bucket learns
	// the "base" load — heating varies day-to-day and shouldn't smear
	// into the hour-of-week signature.
	//
	// Guard: when the heating estimate exceeds the measured load we
	// cannot cleanly isolate the base load from the heating component.
	// Storing 0 would poison the bucket (the EMA decays toward 0 even
	// though a real baseline — fridge, server, standby — always exists).
	// Instead, skip the bucket update entirely for this sample and let
	// existing Samples + Mean stand. Global Samples and MAE still update.
	heatEst := 0.0
	if tempC < HeatingReferenceC {
		heatEst = m.HeatingW_per_degC * (HeatingReferenceC - tempC)
	}
	if heatEst < actualLoadW {
		baseSample := actualLoadW - heatEst
		if b.Samples < 10 {
			b.Mean = (b.Mean*float64(b.Samples) + baseSample) / float64(b.Samples+1)
		} else {
			b.Mean = (1-m.Alpha)*b.Mean + m.Alpha*baseSample
		}
		b.Samples++
	}
	// Heating coefficient is adapted online above. The operator value
	// (Planner.HeatingWPerDegC) seeds the initial estimate and is also
	// applied on /api/loadmodel/reset; from there observation drives the
	// fit. For a household whose load doesn't track temperature, the
	// coefficient converges toward zero — which matches the user-visible
	// guarantee "the model uses what it sees".

	m.Samples++
	m.LastMs = t.UnixMilli()
	if m.Samples == 1 {
		m.MAE = math.Abs(err)
	} else {
		m.MAE = 0.99*m.MAE + 0.01*math.Abs(err)
	}
	return true
}

// Quality reports confidence in [0, 1]. Roughly: what fraction of
// buckets have enough samples to be trusted, weighted by MAE.
func (m Model) Quality() float64 {
	if m.PeakW <= 0 {
		return 0
	}
	var warm int
	for i := 0; i < Buckets; i++ {
		if m.Bucket[i].Samples >= MinTrustSamples {
			warm++
		}
	}
	coverage := float64(warm) / float64(Buckets)
	// Accuracy factor based on MAE vs peak.
	accuracy := 0.0
	if m.Samples > 0 {
		rel := m.MAE / m.PeakW
		if rel <= 0.05 {
			accuracy = 1.0
		} else if rel < 0.5 {
			accuracy = 1 - (rel-0.05)/0.45
		}
	}
	return 0.5*coverage + 0.5*accuracy
}
