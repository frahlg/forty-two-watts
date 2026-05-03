package control

import (
	"sort"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// stubSlotDirective returns a fixed directive whenever called. Lets us
// pin PVLimitW for the test without standing up a full mpc.Service.
func stubSlotDirective(d SlotDirective) SlotDirectiveFunc {
	return func(now time.Time) (SlotDirective, bool) {
		return d, true
	}
}

func staleSlotDirective() SlotDirectiveFunc {
	return func(now time.Time) (SlotDirective, bool) {
		return SlotDirective{}, false
	}
}

// emitPV pushes a fresh pv reading into the store via the public Update
// API, matching the path the lua host uses. RawW must be site-signed:
// negative = generation.
func emitPV(t *testing.T, s *telemetry.Store, driver string, w float64) {
	t.Helper()
	s.DriverHealthMut(driver).RecordSuccess()
	s.Update(driver, telemetry.DerPV, w, nil, nil)
}

// findCurtail returns the per-driver LimitW from a CurtailTarget slice
// for stable assertions independent of slice order.
func findCurtail(targets []CurtailTarget) map[string]float64 {
	out := map[string]float64{}
	for _, t := range targets {
		out[t.Driver] = t.LimitW
	}
	return out
}

func TestComputePVCurtail_NoDirective_DoesNothing(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SlotDirective = staleSlotDirective()
	st.SupportsPVCurtail = map[string]bool{"sungrow": true}
	store := telemetry.NewStore()
	emitPV(t, store, "sungrow", -5000)
	if got := ComputePVCurtail(st, store); got != nil {
		t.Errorf("expected no targets when plan is stale; got %+v", got)
	}
}

func TestComputePVCurtail_LimitZero_DoesNothing(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 0})
	st.SupportsPVCurtail = map[string]bool{"sungrow": true}
	store := telemetry.NewStore()
	emitPV(t, store, "sungrow", -5000)
	if got := ComputePVCurtail(st, store); got != nil {
		t.Errorf("expected no targets when PVLimitW=0; got %+v", got)
	}
}

func TestComputePVCurtail_AllocatesLimitProportionally(t *testing.T) {
	// Two PV drivers: sungrow producing 6 kW, ferroamp producing 4 kW.
	// Total = 10 kW; plan caps at 1500 W. Expect:
	//   sungrow  → 1500 × 6/10 = 900 W
	//   ferroamp → 1500 × 4/10 = 600 W
	st := NewState(0, 100, "meter")
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 1500})
	st.SupportsPVCurtail = map[string]bool{
		"sungrow":  true,
		"ferroamp": true,
	}
	store := telemetry.NewStore()
	emitPV(t, store, "sungrow", -6000)
	emitPV(t, store, "ferroamp", -4000)

	got := findCurtail(ComputePVCurtail(st, store))
	if len(got) != 2 {
		t.Fatalf("want 2 targets, got %d: %+v", len(got), got)
	}
	if abs(got["sungrow"]-900) > 1e-3 {
		t.Errorf("sungrow limit: want 900, got %.2f", got["sungrow"])
	}
	if abs(got["ferroamp"]-600) > 1e-3 {
		t.Errorf("ferroamp limit: want 600, got %.2f", got["ferroamp"])
	}
}

func TestComputePVCurtail_SkipsNonSupportingDriver(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 1000})
	st.SupportsPVCurtail = map[string]bool{"sungrow": true}
	store := telemetry.NewStore()
	emitPV(t, store, "sungrow", -3000)
	emitPV(t, store, "easee", -2000) // hypothetical PV reading on a non-curtail driver

	got := findCurtail(ComputePVCurtail(st, store))
	if len(got) != 1 {
		t.Fatalf("only sungrow should be curtailed; got %+v", got)
	}
	if abs(got["sungrow"]-1000) > 1e-3 {
		// 100% of limit (only one supporting driver in the pool).
		t.Errorf("sungrow limit: want 1000, got %.2f", got["sungrow"])
	}
}

func TestComputePVCurtail_SkipsDriverNotGenerating(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 800})
	st.SupportsPVCurtail = map[string]bool{
		"sungrow":  true,
		"ferroamp": true,
	}
	store := telemetry.NewStore()
	emitPV(t, store, "sungrow", -2000)
	emitPV(t, store, "ferroamp", 0) // night, not generating

	got := findCurtail(ComputePVCurtail(st, store))
	if _, ok := got["ferroamp"]; ok {
		t.Errorf("ferroamp not generating; should not receive curtail: %+v", got)
	}
	if abs(got["sungrow"]-800) > 1e-3 {
		t.Errorf("sungrow should get full limit; got %.2f", got["sungrow"])
	}
}

// Regression: when the plan stops asking for curtailment, the driver
// that was previously curtailed must receive a one-shot LimitW=0
// (translated to `curtail_disable` by main.go) — otherwise the cap
// stays applied silently after the slot rolls over.
func TestComputePVCurtail_ReleasesPreviouslyCurtailedDriver(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SupportsPVCurtail = map[string]bool{"sungrow": true}
	store := telemetry.NewStore()
	emitPV(t, store, "sungrow", -3000)

	// Tick 1: plan caps PV at 1000 W → sungrow gets curtailed.
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 1000})
	tick1 := findCurtail(ComputePVCurtail(st, store))
	if abs(tick1["sungrow"]-1000) > 1e-3 {
		t.Fatalf("tick1: want sungrow=1000, got %+v", tick1)
	}
	if !st.LastCurtailedDrivers["sungrow"] {
		t.Fatalf("tick1: state should remember sungrow as curtailed")
	}

	// Tick 2: plan no longer caps (slot rolled over). Expect a
	// release target for sungrow with LimitW=0.
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 0})
	tick2 := ComputePVCurtail(st, store)
	if len(tick2) != 1 {
		t.Fatalf("tick2: want one release target, got %+v", tick2)
	}
	if tick2[0].Driver != "sungrow" || tick2[0].LimitW != 0 {
		t.Errorf("tick2: want {sungrow, 0}, got %+v", tick2[0])
	}
	if len(st.LastCurtailedDrivers) != 0 {
		t.Errorf("tick2: state should have cleared LastCurtailedDrivers; got %+v",
			st.LastCurtailedDrivers)
	}

	// Tick 3: still no curtail, no driver to release. Expect nil.
	if got := ComputePVCurtail(st, store); got != nil {
		t.Errorf("tick3: want nil (idempotent release), got %+v", got)
	}
}

// Sanity: target slice is deterministic enough that callers can sort
// by driver name without relying on map iteration order.
func TestComputePVCurtail_DeterministicSortable(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 1000})
	st.SupportsPVCurtail = map[string]bool{"a": true, "b": true, "c": true}
	store := telemetry.NewStore()
	emitPV(t, store, "a", -1000)
	emitPV(t, store, "b", -1000)
	emitPV(t, store, "c", -1000)
	got := ComputePVCurtail(st, store)
	if len(got) != 3 {
		t.Fatalf("want 3 targets, got %d", len(got))
	}
	sort.Slice(got, func(i, j int) bool { return got[i].Driver < got[j].Driver })
	for i, d := range []string{"a", "b", "c"} {
		if got[i].Driver != d {
			t.Errorf("idx %d: want driver %q, got %q", i, d, got[i].Driver)
		}
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
