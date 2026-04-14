package loadmodel

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

// synthetic household: 300W baseline, morning peak 2500W around 07:30,
// evening peak 3500W around 19:00, broader midday usage during WFH.
// Cosine lobes keep it smooth — real homes are bumpier but this is
// realistic enough to validate the learning pipeline.
func synthetic(t time.Time) float64 {
	h := float64(t.Hour()) + float64(t.Minute())/60.0
	base := 300.0
	morning := 2500.0 * math.Exp(-0.5*math.Pow((h-7.5)/1.0, 2))
	midday := 800.0 * math.Exp(-0.5*math.Pow((h-13)/2.5, 2))
	evening := 3500.0 * math.Exp(-0.5*math.Pow((h-19)/1.2, 2))
	return base + morning + midday + evening
}

// naivePredict uses a flat constant — what BaseLoad in config does today.
func naivePredict(avg float64) float64 { return avg }

func TestLearnsHouseholdPattern(t *testing.T) {
	m := NewModel(4000)
	rng := rand.New(rand.NewSource(42))
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 30 days × 24h × 4 samples/hour = 2880 samples
	for d := 0; d < 30; d++ {
		for h := 0; h < 24; h++ {
			for q := 0; q < 4; q++ {
				t0 := start.Add(time.Duration(d*24+h)*time.Hour + time.Duration(q*15)*time.Minute)
				actual := synthetic(t0)
				// 3% measurement noise
				actual += (rng.Float64()*2 - 1) * 0.03 * actual
				m.Update(t0, actual)
			}
		}
	}
	// Compare twin vs flat baseline across 24 hours.
	// naive baseline = training-set mean (best flat constant).
	var trainSum float64
	var n int
	for d := 0; d < 30; d++ {
		for h := 0; h < 24; h++ {
			t0 := start.Add(time.Duration(d*24+h) * time.Hour)
			trainSum += synthetic(t0)
			n++
		}
	}
	mean := trainSum / float64(n)

	var twinErr, naiveErr float64
	samples := 0
	for h := 0; h < 24; h++ {
		testT := time.Date(2026, 2, 1, h, 0, 0, 0, time.UTC)
		want := synthetic(testT)
		twinErr += math.Abs(m.Predict(testT) - want)
		naiveErr += math.Abs(naivePredict(mean) - want)
		samples++
	}
	twinErr /= float64(samples)
	naiveErr /= float64(samples)
	t.Logf("twin MAE %.0fW, naive (mean) MAE %.0fW, improvement %.1f%%",
		twinErr, naiveErr, (1-twinErr/naiveErr)*100)
	// Twin should beat flat baseline by a meaningful margin — with 3
	// harmonics capturing sharp 07:30 / 19:00 peaks we expect ≥30%.
	if twinErr >= naiveErr*0.7 {
		t.Errorf("twin should beat flat baseline by >30%%: twin=%.0fW naive=%.0fW", twinErr, naiveErr)
	}
	if m.Quality() < 0.3 {
		t.Errorf("quality should be >0.3 after 2880 samples, got %.2f (MAE=%.0fW)", m.Quality(), m.MAE)
	}
}

func TestPredictsEveningPeak(t *testing.T) {
	// Specific test: model must identify the 19:00 peak.
	m := NewModel(4000)
	rng := rand.New(rand.NewSource(1))
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for d := 0; d < 20; d++ {
		for h := 0; h < 24; h++ {
			t0 := start.Add(time.Duration(d*24+h) * time.Hour)
			actual := synthetic(t0) + (rng.Float64()*2-1)*50
			m.Update(t0, actual)
		}
	}
	evening := time.Date(2026, 2, 1, 19, 0, 0, 0, time.UTC)
	noon := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	overnight := time.Date(2026, 2, 1, 3, 0, 0, 0, time.UTC)
	pe := m.Predict(evening)
	pn := m.Predict(noon)
	po := m.Predict(overnight)
	if !(pe > pn && pn > po) {
		t.Errorf("expected evening > noon > overnight, got e=%.0f n=%.0f o=%.0f", pe, pn, po)
	}
	if pe < 2500 {
		t.Errorf("evening peak should be ≥2500W, got %.0f", pe)
	}
	if po > 800 {
		t.Errorf("overnight should be <800W, got %.0f", po)
	}
}

func TestRejectsNegativeLoad(t *testing.T) {
	m := NewModel(4000)
	before := m.Samples
	m.Update(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), -500)
	if m.Samples != before {
		t.Errorf("negative load should be skipped")
	}
}

func TestRejectsOutliers(t *testing.T) {
	m := NewModel(4000)
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 200; i++ {
		m.Update(start.Add(time.Duration(i)*time.Minute), 1500)
	}
	preBeta := m.Beta
	m.Update(start.Add(500*time.Minute), 50000) // 33× the typical
	var drift float64
	for i := 0; i < NFeat; i++ {
		drift += math.Abs(m.Beta[i] - preBeta[i])
	}
	if drift > 1 {
		t.Errorf("outlier should be rejected, β drift %.4f", drift)
	}
}

func TestInitialPredictionNonZero(t *testing.T) {
	m := NewModel(4000)
	got := m.Predict(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	if got != 500 {
		t.Errorf("initial prediction should be 500W baseline, got %.0f", got)
	}
}
