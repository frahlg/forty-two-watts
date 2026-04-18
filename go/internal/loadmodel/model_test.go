package loadmodel

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

// TestHourOfWeekStableAcrossDST — the bucket index must not shift when
// the same absolute instant is represented in a different timezone.
// Before the UTC coercion, evening-hour Predict calls around DST
// boundaries silently drew from the wrong bucket.
func TestHourOfWeekStableAcrossDST(t *testing.T) {
	stockholm, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Skipf("Europe/Stockholm tzdata unavailable: %v", err)
	}
	instants := []time.Time{
		time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 15, 17, 0, 0, 0, time.UTC),
		time.Date(2026, 12, 15, 20, 0, 0, 0, time.UTC),
	}
	for _, inst := range instants {
		if HourOfWeek(inst) != HourOfWeek(inst.In(stockholm)) {
			t.Errorf("HourOfWeek differs: utc=%d local=%d (inst=%v)",
				HourOfWeek(inst), HourOfWeek(inst.In(stockholm)), inst)
		}
	}
}

// synthetic household: 300W baseline, morning peak 2500W around 07:30,
// evening peak 3500W around 19:00.
func synthetic(t time.Time) float64 {
	h := float64(t.Hour()) + float64(t.Minute())/60.0
	base := 300.0
	morning := 2500.0 * math.Exp(-0.5*math.Pow((h-7.5)/1.0, 2))
	midday := 800.0 * math.Exp(-0.5*math.Pow((h-13)/2.5, 2))
	evening := 3500.0 * math.Exp(-0.5*math.Pow((h-19)/1.2, 2))
	return base + morning + midday + evening
}

func TestDayOnePriorIsUsefulEverywhere(t *testing.T) {
	// Before any training: predictions at any hour should be within
	// reasonable bounds (>0 overnight, elevated at peaks). The typical
	// prior is the safety net that covers cold start.
	m := NewModel(4000)
	overnight := time.Date(2026, 3, 17, 3, 0, 0, 0, time.UTC)
	morning := time.Date(2026, 3, 17, 7, 30, 0, 0, time.UTC)
	evening := time.Date(2026, 3, 17, 19, 0, 0, 0, time.UTC)
	o := m.PredictNoTemp(overnight)
	mo := m.PredictNoTemp(morning)
	e := m.PredictNoTemp(evening)
	if o < 100 || o > 800 {
		t.Errorf("overnight should be in [100, 800], got %f", o)
	}
	if mo < 1500 {
		t.Errorf("morning peak should be >= 1500, got %f", mo)
	}
	if e < 2000 {
		t.Errorf("evening peak should be >= 2000, got %f", e)
	}
}

func TestLearnsHouseholdPattern(t *testing.T) {
	m := NewModel(4000)
	rng := rand.New(rand.NewSource(42))
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC) // Monday
	// 10 weeks × 7 days × 24 hours = 1680 hourly samples, ~10 per bucket —
	// past MinTrustSamples, bucket EMA dominates the prior.
	for d := 0; d < 70; d++ {
		for h := 0; h < 24; h++ {
			t0 := start.Add(time.Duration(d*24+h) * time.Hour)
			actual := synthetic(t0) + (rng.Float64()*2-1)*50 // tiny noise
			m.Update(t0, actual, HeatingReferenceC)          // no heating
		}
	}
	// Check weekday prediction accuracy.
	test := time.Date(2026, 3, 2, 19, 0, 0, 0, time.UTC) // Monday 19:00
	want := synthetic(test)
	got := m.Predict(test, HeatingReferenceC)
	if math.Abs(got-want)/want > 0.10 {
		t.Errorf("evening prediction off: got %.0f want %.0f", got, want)
	}
	if m.Quality() < 0.5 {
		t.Errorf("quality should be ≥0.5 after 4 weeks, got %.3f", m.Quality())
	}
}

func TestHeatingConfiguredBoostsColdDayPrediction(t *testing.T) {
	// When operator sets HeatingW_per_degC, Predict adds heating on cold
	// days. Warm days (≥ reference) are unaffected.
	m := NewModel(4000)
	m.HeatingW_per_degC = 300
	t0 := time.Date(2026, 3, 17, 12, 0, 0, 0, time.UTC)
	warm := m.Predict(t0, 20) // above reference
	freezing := m.Predict(t0, -5)
	delta := freezing - warm
	// Expected heating contribution: 300 W/°C × (18 − (−5)) = 6900 W.
	if math.Abs(delta-6900) > 100 {
		t.Errorf("heating contribution: got %.0f W, want ~6900 W", delta)
	}
}

func TestHourOfWeekDeterministic(t *testing.T) {
	// Monday 00:00 UTC → 0
	mon := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	if idx := HourOfWeek(mon); idx != 0 {
		t.Errorf("Monday 00:00 should be bucket 0, got %d", idx)
	}
	// Sunday 23:00 UTC → 167
	sun := time.Date(2026, 1, 11, 23, 0, 0, 0, time.UTC)
	if idx := HourOfWeek(sun); idx != 167 {
		t.Errorf("Sunday 23:00 should be bucket 167, got %d", idx)
	}
}

func TestRejectsNegativeLoad(t *testing.T) {
	m := NewModel(4000)
	before := m.Samples
	m.Update(time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC), -500, HeatingReferenceC)
	if m.Samples != before {
		t.Errorf("negative load should be rejected")
	}
}

func TestRejectsOutliers(t *testing.T) {
	m := NewModel(4000)
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 200; i++ {
		m.Update(start.Add(time.Duration(i)*time.Minute), 1500, HeatingReferenceC)
	}
	preMean := m.Bucket[HourOfWeek(start)].Mean
	m.Update(start.Add(500*time.Minute), 50000, HeatingReferenceC) // 33× typical
	postMean := m.Bucket[HourOfWeek(start.Add(500*time.Minute))].Mean
	if math.Abs(postMean-preMean) > 500 {
		t.Errorf("outlier should be rejected, mean drift %.0f", postMean-preMean)
	}
}

func TestPredictRespectsTrust(t *testing.T) {
	// A bucket with 0 samples returns pure prior.
	// After many samples, it returns the bucket's EMA.
	m := NewModel(4000)
	t0 := time.Date(2026, 1, 5, 19, 0, 0, 0, time.UTC)
	prior := typicalPrior(HourOfWeek(t0))
	predBefore := m.Predict(t0, HeatingReferenceC)
	if math.Abs(predBefore-prior) > 1 {
		t.Errorf("fresh bucket should return prior (%f), got %f", prior, predBefore)
	}
	// Feed 30 samples of a constant 1000W at this hour.
	for i := 0; i < 30; i++ {
		m.Update(t0.AddDate(0, 0, 7*i), 1000, HeatingReferenceC)
	}
	predAfter := m.Predict(t0, HeatingReferenceC)
	if math.Abs(predAfter-1000) > 100 {
		t.Errorf("trained bucket should be ~1000W, got %f", predAfter)
	}
}
