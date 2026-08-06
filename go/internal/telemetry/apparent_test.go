package telemetry

import (
	"math"
	"testing"
	"time"
)

const meter = "site-meter"

func estimate(s *Store, realW float64) ApparentPowerEstimate {
	return SiteApparentPowerVA(s, meter, realW, 3, 230, 0.95, time.Now(), 30*time.Second)
}

func TestApparentPowerFromPhaseCurrents(t *testing.T) {
	s := NewStore()
	for _, m := range []struct {
		name string
		v    float64
	}{
		{"meter_l1_a", 100}, {"meter_l2_a", 80}, {"meter_l3_a", 120},
	} {
		s.EmitMetric(meter, m.name, m.v, "A", "", "")
	}
	got := estimate(s, 60000)
	want := 230.0 * (100 + 80 + 120)
	if got.Method != "phase_va" || math.Abs(got.VA-want) > 1 {
		t.Fatalf("got %+v, want method=phase_va va=%v", got, want)
	}
}

func TestApparentPowerPrefersMeasuredVoltage(t *testing.T) {
	s := NewStore()
	s.EmitMetric(meter, "meter_l1_a", 10, "A", "", "")
	s.EmitMetric(meter, "meter_l1_v", 240, "V", "", "")
	got := SiteApparentPowerVA(s, meter, 2300, 1, 230, 0.95, time.Now(), 30*time.Second)
	if got.Method != "phase_va" || math.Abs(got.VA-2400) > 1 {
		t.Fatalf("got %+v, want 2400 VA from measured 240 V", got)
	}
}

func TestApparentPowerPartialPhasesFallsThrough(t *testing.T) {
	s := NewStore()
	// Only two of three phases → phase method must NOT engage.
	s.EmitMetric(meter, "meter_l1_a", 100, "A", "", "")
	s.EmitMetric(meter, "meter_l2_a", 80, "A", "", "")
	got := estimate(s, 46000)
	if got.Method != "power_factor" {
		t.Fatalf("partial phase currents must fall through, got %+v", got)
	}
}

func TestApparentPowerFromReactive(t *testing.T) {
	s := NewStore()
	s.EmitMetric(meter, "meter_q_l1_var", 1000, "var", "", "")
	s.EmitMetric(meter, "meter_q_l2_var", 1000, "var", "", "")
	s.EmitMetric(meter, "meter_q_l3_var", 1000, "var", "", "")
	got := estimate(s, 4000)
	want := math.Hypot(4000, 3000)
	if got.Method != "reactive" || math.Abs(got.VA-want) > 1 {
		t.Fatalf("got %+v, want method=reactive va=%v", got, want)
	}
}

func TestApparentPowerFromDSMRSplit(t *testing.T) {
	s := NewStore()
	for n := 1; n <= 3; n++ {
		s.EmitMetric(meter, "meter_q_imp_l"+string(rune('0'+n))+"_var", 1500, "var", "", "")
		s.EmitMetric(meter, "meter_q_exp_l"+string(rune('0'+n))+"_var", 500, "var", "", "")
	}
	got := estimate(s, 4000)
	want := math.Hypot(4000, 3000)
	if got.Method != "reactive" || math.Abs(got.VA-want) > 1 {
		t.Fatalf("got %+v, want method=reactive va=%v", got, want)
	}
}

func TestApparentPowerPowerFactorFallback(t *testing.T) {
	s := NewStore()
	got := estimate(s, -9500) // exporting site
	if got.Method != "power_factor" || math.Abs(got.VA-10000) > 1 {
		t.Fatalf("got %+v, want method=power_factor va=10000", got)
	}
}

func TestApparentPowerIgnoresStaleMetrics(t *testing.T) {
	s := NewStore()
	s.EmitMetric(meter, "meter_l1_a", 100, "A", "", "")
	s.EmitMetric(meter, "meter_l2_a", 100, "A", "", "")
	s.EmitMetric(meter, "meter_l3_a", 100, "A", "", "")
	// Evaluate "one minute later" — the currents are stale.
	got := SiteApparentPowerVA(s, meter, 47500, 3, 230, 0.95, time.Now().Add(time.Minute), 30*time.Second)
	if got.Method != "power_factor" {
		t.Fatalf("stale currents must fall through to power factor, got %+v", got)
	}
}
