package telemetry

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// ---- Kalman ----

func TestKalmanInitialMeasurement(t *testing.T) {
	k := NewKalman(100, 50)
	out := k.Update(1234)
	if out != 1234 {
		t.Errorf("first update should return measurement, got %f", out)
	}
	if k.Estimate != 1234 {
		t.Errorf("estimate: got %f", k.Estimate)
	}
}

func TestKalmanSmoothsStepChange(t *testing.T) {
	k := NewKalman(100, 50)
	k.Update(1000)
	out := k.Update(2000)
	if out <= 1000 || out >= 2000 {
		t.Errorf("expected smoothed value between 1000 and 2000, got %f", out)
	}
}

func TestKalmanConvergesOnStableSignal(t *testing.T) {
	k := NewKalman(100, 50)
	for i := 0; i < 50; i++ {
		k.Update(500)
	}
	if math.Abs(k.Estimate-500) > 1 {
		t.Errorf("expected ≈500 after convergence, got %f", k.Estimate)
	}
}

// ---- Store ----

func TestStoreUpdateAndGet(t *testing.T) {
	s := NewStore()
	s.Update("ferroamp", DerMeter, 1500, nil, nil)
	r := s.Get("ferroamp", DerMeter)
	if r == nil {
		t.Fatal("expected reading")
	}
	if r.RawW != 1500 {
		t.Errorf("raw: got %f", r.RawW)
	}
	if r.SmoothedW != 1500 {
		t.Errorf("first smoothed = raw, got %f", r.SmoothedW)
	}
}

func TestStoreKeepsSeparateFiltersPerDriver(t *testing.T) {
	s := NewStore()
	soc := 0.5
	s.Update("a", DerBattery, 100, &soc, nil)
	s.Update("b", DerBattery, 200, &soc, nil)
	s.Update("a", DerPV, 300, nil, nil)

	if r := s.Get("a", DerBattery); r == nil || r.RawW != 100 {
		t.Error("a:battery")
	}
	if r := s.Get("b", DerBattery); r == nil || r.RawW != 200 {
		t.Error("b:battery")
	}
	if r := s.Get("a", DerPV); r == nil || r.RawW != 300 {
		t.Error("a:pv")
	}
	if r := s.Get("b", DerPV); r != nil {
		t.Error("b:pv should not exist")
	}
}

func TestReadingsByTypeAggregates(t *testing.T) {
	s := NewStore()
	s.Update("a", DerBattery, 100, nil, nil)
	s.Update("b", DerBattery, 200, nil, nil)
	s.Update("a", DerMeter, 50, nil, nil)

	bats := s.ReadingsByType(DerBattery)
	if len(bats) != 2 {
		t.Errorf("expected 2 batteries, got %d", len(bats))
	}
	var total float64
	for _, b := range bats {
		total += b.RawW
	}
	if total != 300 {
		t.Errorf("total battery: got %f", total)
	}
}

func TestIsStale(t *testing.T) {
	s := NewStore()
	if !s.IsStale("unknown", DerMeter, time.Second) {
		t.Error("unknown should be stale")
	}
	s.Update("a", DerMeter, 0, nil, nil)
	if s.IsStale("a", DerMeter, time.Second) {
		t.Error("just-updated should be fresh")
	}
}

// ---- DriverHealth ----

func TestHealthDegradesAfter3Errors(t *testing.T) {
	h := &DriverHealth{Name: "t"}
	h.RecordError("e1")
	if h.Status != StatusOk {
		t.Errorf("after 1 error: %v", h.Status)
	}
	h.RecordError("e2")
	h.RecordError("e3")
	if h.Status != StatusDegraded {
		t.Errorf("after 3 errors should be Degraded, got %v", h.Status)
	}
}

func TestHealthRecoversOnSuccess(t *testing.T) {
	h := &DriverHealth{Name: "t"}
	h.RecordError("e1"); h.RecordError("e2"); h.RecordError("e3")
	h.RecordSuccess()
	if h.Status != StatusOk {
		t.Errorf("success should reset to Ok: %v", h.Status)
	}
	if h.ConsecutiveErrors != 0 {
		t.Errorf("errors should reset: %d", h.ConsecutiveErrors)
	}
}

func TestHealthOffline(t *testing.T) {
	h := &DriverHealth{Name: "t"}
	h.SetOffline()
	if h.IsOnline() {
		t.Error("offline should not be online")
	}
	h.RecordSuccess()
	if !h.IsOnline() {
		t.Error("success should return to online")
	}
}

// ---- Data pass-through ----

func TestReadingPreservesData(t *testing.T) {
	s := NewStore()
	raw := json.RawMessage(`{"hello":"world"}`)
	s.Update("a", DerMeter, 0, nil, raw)
	r := s.Get("a", DerMeter)
	if string(r.Data) != string(raw) {
		t.Errorf("data roundtrip: got %s", string(r.Data))
	}
}

// ---- DerType ----

func TestDerTypeRoundtrip(t *testing.T) {
	for _, name := range []string{"meter", "pv", "battery", "ev"} {
		d, err := ParseDerType(name)
		if err != nil {
			t.Fatal(err)
		}
		if d.String() != name {
			t.Errorf("roundtrip %q: got %q", name, d.String())
		}
	}
	if _, err := ParseDerType("nonsense"); err == nil {
		t.Error("expected parse error")
	}
}

// ---- SoC preservation ----

func TestStorePreservesSoCWhenMissing(t *testing.T) {
	// Devices like Ferroamp ESO publish SoC less often than the
	// power-flow telemetry. In-between ticks have no SoC field — the
	// store must keep the last-known value rather than dropping back
	// to nil, which would confuse the MPC and any UI display.
	s := NewStore()
	soc := 0.97
	s.Update("ferroamp", DerBattery, -1500, &soc, nil)
	if r := s.Get("ferroamp", DerBattery); r == nil || r.SoC == nil || *r.SoC != 0.97 {
		t.Fatalf("first update: SoC not stored, got %+v", r)
	}
	// Next tick: power update only, no SoC.
	s.Update("ferroamp", DerBattery, -1450, nil, nil)
	r := s.Get("ferroamp", DerBattery)
	if r == nil || r.SoC == nil || *r.SoC != 0.97 {
		t.Errorf("SoC should be preserved across nil-update, got %+v", r)
	}
	// Fresh SoC overwrites.
	soc2 := 0.95
	s.Update("ferroamp", DerBattery, -1400, &soc2, nil)
	if r := s.Get("ferroamp", DerBattery); r == nil || r.SoC == nil || *r.SoC != 0.95 {
		t.Errorf("fresh SoC should overwrite, got %+v", r)
	}
}

// ---- Load filter ----

func TestLoadFilterSmoothsNoisy(t *testing.T) {
	s := NewStore()
	vals := []float64{1000, 2000, 500, 1500, 1200, 800, 1100, 1400}
	var last float64
	for _, v := range vals {
		last = s.UpdateLoad(v)
	}
	if last < 500 || last > 2000 {
		t.Errorf("load filter should converge to middle, got %f", last)
	}
}
