package plant

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeUnit implements pollable in memory.
type fakeUnit struct {
	mu       sync.Mutex
	state    UnitState
	seen     time.Time
	setpoint float64
	writes   int
	writeErr error
}

func (f *fakeUnit) Poll() error { return nil }
func (f *fakeUnit) WriteSetpoint(w float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	f.setpoint = w
	f.writes++
	return nil
}
func (f *fakeUnit) State() (UnitState, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.seen, nil
}
func (f *fakeUnit) sp() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setpoint
}

func testController() (*Controller, []*fakeUnit) {
	now := time.Now()
	fakes := []*fakeUnit{
		{state: UnitState{ID: "r1", Online: true, SoC: 0.5, CapacityWh: 50000, MaxChargeW: 25000, MaxDischargeW: 25000}, seen: now},
		{state: UnitState{ID: "r2", Online: true, SoC: 0.5, CapacityWh: 50000, MaxChargeW: 25000, MaxDischargeW: 25000}, seen: now},
	}
	units := make([]pollable, len(fakes))
	for i, f := range fakes {
		units[i] = f
	}
	return newControllerWithUnits(Config{}, units), fakes
}

func TestControllerAllocatesUnderLease(t *testing.T) {
	c, fakes := testController()
	c.SetTarget(20000, 30*time.Second)
	c.step(time.Now())
	total := fakes[0].sp() + fakes[1].sp()
	if math.Abs(total-20000) > 1 {
		t.Fatalf("written setpoints sum %v, want 20000", total)
	}
}

func TestControllerLeaseExpiryRampsToZero(t *testing.T) {
	c, fakes := testController()
	c.SetTarget(20000, 10*time.Millisecond)
	c.step(time.Now())
	time.Sleep(20 * time.Millisecond)
	c.step(time.Now())
	if fakes[0].sp() != 0 || fakes[1].sp() != 0 {
		t.Fatalf("lease expired but setpoints are %v/%v", fakes[0].sp(), fakes[1].sp())
	}
}

func TestControllerNoLeaseMeansZero(t *testing.T) {
	c, fakes := testController()
	c.step(time.Now())
	if fakes[0].sp() != 0 || fakes[1].sp() != 0 {
		t.Fatalf("never-leased plant moved power: %v/%v", fakes[0].sp(), fakes[1].sp())
	}
}

func TestControllerDeratesStaleUnit(t *testing.T) {
	c, fakes := testController()
	fakes[1].mu.Lock()
	fakes[1].seen = time.Now().Add(-time.Minute) // stale poll
	fakes[1].mu.Unlock()
	c.SetTarget(20000, 30*time.Second)
	now := time.Now()
	c.step(now)
	if fakes[0].sp() != 20000 {
		t.Fatalf("healthy unit should take the whole target, got %v", fakes[0].sp())
	}
	st := c.Status(now)
	if st.Aggregate.UnitsOnline != 1 || st.Aggregate.AvailableChargeW != 25000 {
		t.Fatalf("stale unit still counted: %+v", st.Aggregate)
	}
}

func TestControllerSkipsUnchangedWrites(t *testing.T) {
	c, fakes := testController()
	c.SetTarget(20000, time.Minute)
	c.step(time.Now())
	c.step(time.Now())
	c.step(time.Now())
	if fakes[0].writes != 1 {
		t.Fatalf("unchanged setpoint rewritten %d times", fakes[0].writes)
	}
}

func TestServeMuxContract(t *testing.T) {
	c, fakes := testController()
	srv := httptest.NewServer(NewServeMux(c))
	defer srv.Close()

	res, err := http.Post(srv.URL+"/v1/setpoint", "application/json",
		strings.NewReader(`{"power_w": 15000, "ttl_ms": 30000}`))
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("setpoint: %v %v", err, res.Status)
	}
	c.step(time.Now())
	if total := fakes[0].sp() + fakes[1].sp(); math.Abs(total-15000) > 1 {
		t.Fatalf("setpoint not applied: %v", total)
	}

	res, err = http.Get(srv.URL + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.SchemaVersion != 1 || st.TargetW != 15000 || st.Aggregate.UnitsOnline != 2 {
		t.Fatalf("status: %+v", st)
	}
	if st.LeaseExpires == nil {
		t.Fatal("active lease missing from status")
	}

	// Missing power_w is a 400.
	res, _ = http.Post(srv.URL+"/v1/setpoint", "application/json", strings.NewReader(`{"ttl_ms": 1}`))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing power_w: %v", res.Status)
	}
}
