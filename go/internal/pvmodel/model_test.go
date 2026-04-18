package pvmodel

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

// TestFeaturesStableAcrossDST — the time-of-day harmonic phase must
// not shift when the same instant is passed in a different timezone.
// Otherwise DST transitions silently re-score the learned β coefficients.
func TestFeaturesStableAcrossDST(t *testing.T) {
	stockholm, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Skipf("Europe/Stockholm tzdata unavailable: %v", err)
	}
	instants := []time.Time{
		time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 15, 10, 30, 0, 0, time.UTC),
	}
	for _, inst := range instants {
		fUTC := Features(500, 30, inst)
		fLocal := Features(500, 30, inst.In(stockholm))
		for i := range fUTC {
			if fUTC[i] != fLocal[i] {
				t.Errorf("Features[%d] diverges: utc=%.4f local=%.4f (inst=%v)",
					i, fUTC[i], fLocal[i], inst)
			}
		}
	}
}

// synthetic simulates a real rooftop: rated 10 kW south-facing with a
// morning shading lobe (soft -30% around 08:00, tapering to 1.0 by 11:00),
// a 92% of rated peak (module aging), and a slightly stronger cloud
// response than the naive (1-c)^1.5 heuristic.
func synthetic(clearSkyW, cloudPct float64, t time.Time) float64 {
	hour := float64(t.Hour()) + float64(t.Minute())/60.0
	// Smooth shade lobe centered at 8 with width 3h (cosine window).
	shade := 1.0
	if hour < 11 {
		dx := (hour - 8) / 3.0
		if math.Abs(dx) < 1 {
			shade = 1 - 0.3*(0.5+0.5*math.Cos(math.Pi*dx))
		}
	}
	cf := math.Pow(1-cloudPct/100.0, 1.6)
	y := clearSkyW * cf * (10000 * 0.92 / 1000.0) * shade
	if y < 0 {
		y = 0
	}
	return y
}

// naivePredict replicates the pre-twin forecast so we can show the twin
// is actually an improvement.
func naivePredict(clearSkyW, cloudPct float64, ratedW float64) float64 {
	cf := math.Pow(1-cloudPct/100.0, 1.5)
	return clearSkyW * cf * ratedW / 1000.0
}

// clearSkySim: triangular curve peaking at 1000 W/m² at solar noon, 0
// at 06:00 and 18:00. Good enough for learning tests.
func clearSkySim(t time.Time) float64 {
	hour := float64(t.Hour()) + float64(t.Minute())/60.0
	if hour <= 6 || hour >= 18 {
		return 0
	}
	peak := 12.0
	return 1000.0 * (1 - math.Abs(hour-peak)/6.0)
}

func TestLearnsSyntheticSystem(t *testing.T) {
	m := NewModel(10000)
	r := rand.New(rand.NewSource(42))
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Train 30 days × 24h × 4 samples/hour = 2880 samples
	for d := 0; d < 30; d++ {
		for h := 0; h < 24; h++ {
			for q := 0; q < 4; q++ {
				t0 := start.Add(time.Duration(d*24+h)*time.Hour + time.Duration(q*15)*time.Minute)
				cs := clearSkySim(t0)
				cloud := 20 + r.Float64()*60 // 20–80% cloud
				actual := synthetic(cs, cloud, t0)
				// Add 2% measurement noise
				actual += (r.Float64()*2 - 1) * 0.02 * actual
				m.Update(cs, cloud, t0, actual)
			}
		}
	}
	// Compare twin vs. naive across 24 hourly buckets at 20% cloud. Twin
	// must be at LEAST as accurate as naive on average — ideally better.
	var twinErr, naiveErr float64
	var samples int
	for h := 6; h <= 18; h++ {
		testT := time.Date(2026, 2, 1, h, 0, 0, 0, time.UTC)
		cs := clearSkySim(testT)
		if cs < 50 {
			continue
		}
		want := synthetic(cs, 20, testT)
		twinErr += math.Abs(m.Predict(cs, 20, testT) - want)
		naiveErr += math.Abs(naivePredict(cs, 20, 10000) - want)
		samples++
	}
	twinErr /= float64(samples)
	naiveErr /= float64(samples)
	t.Logf("twin MAE %.0fW, naive MAE %.0fW, improvement %.1f%%", twinErr, naiveErr, (1-twinErr/naiveErr)*100)
	if twinErr >= naiveErr {
		t.Errorf("twin should beat naive on average: twin=%.0fW naive=%.0fW", twinErr, naiveErr)
	}
	// Quality should cross 0.3 for a well-fit 10kW system (MAE ≲ 1.5kW).
	if m.Quality() < 0.3 {
		t.Errorf("quality should be >0.3 after 2880 samples, got %.2f (MAE=%.0fW)", m.Quality(), m.MAE)
	}
}

func TestSkipsNightSamples(t *testing.T) {
	m := NewModel(5000)
	before := m.Samples
	m.Update(10, 50, time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC), 0)
	if m.Samples != before {
		t.Errorf("should skip low-clearsky samples")
	}
}

func TestRejectsOutliers(t *testing.T) {
	m := NewModel(5000)
	// Converge with clean data
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 200; i++ {
		t0 := start.Add(time.Duration(i) * time.Minute)
		m.Update(800, 30, t0, 3000)
	}
	preBeta := m.Beta
	// Inject a 10× outlier
	m.Update(800, 30, start.Add(500*time.Minute), 30000)
	// β shouldn't move meaningfully
	var drift float64
	for i := 0; i < NFeat; i++ {
		drift += math.Abs(m.Beta[i] - preBeta[i])
	}
	if drift > 0.1 {
		t.Errorf("outlier should be rejected, but β drifted %.4f", drift)
	}
}

func TestInitialPredictionMatchesNaive(t *testing.T) {
	// Fresh model should give predictions close to naive clear-sky × cloud × rated/1000.
	m := NewModel(5000)
	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cs := 900.0
	cloud := 20.0
	want := cs * math.Pow(0.8, 1.5) * 5000.0 / 1000.0
	got := m.Predict(cs, cloud, t0)
	if math.Abs(got-want) > 1 {
		t.Errorf("initial prediction should match naive: got %f want %f", got, want)
	}
}

func TestQualityStartsLow(t *testing.T) {
	m := NewModel(5000)
	if q := m.Quality(); q != 0 {
		t.Errorf("untrained quality should be 0, got %f", q)
	}
}

func TestCapsAtDoubleRated(t *testing.T) {
	m := NewModel(5000)
	// Force bad β that would predict huge value
	m.Beta[1] = 100
	got := m.Predict(1000, 0, time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	if got > 1.3*5000 {
		t.Errorf("should cap at 1.3× rated, got %f", got)
	}
}
