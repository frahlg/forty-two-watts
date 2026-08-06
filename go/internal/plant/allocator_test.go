package plant

import (
	"math"
	"testing"
)

func fleet() []UnitState {
	return []UnitState{
		{ID: "r1", Online: true, SoC: 0.40, CapacityWh: 50000, MaxChargeW: 25000, MaxDischargeW: 25000},
		{ID: "r2", Online: true, SoC: 0.50, CapacityWh: 50000, MaxChargeW: 25000, MaxDischargeW: 25000},
		{ID: "r3", Online: true, SoC: 0.60, CapacityWh: 50000, MaxChargeW: 25000, MaxDischargeW: 25000},
	}
}

func sum(m map[string]float64) float64 {
	var s float64
	for _, v := range m {
		s += v
	}
	return s
}

func TestAllocateSumsToAggregate(t *testing.T) {
	for _, target := range []float64{30000, -30000, 500, -500, 74999} {
		alloc := Allocate(fleet(), target)
		if math.Abs(sum(alloc)-clampMag(target, 75000)) > 1 {
			t.Errorf("target %v: sum %v", target, sum(alloc))
		}
	}
}

func clampMag(v, m float64) float64 {
	if v > m {
		return m
	}
	if v < -m {
		return -m
	}
	return v
}

func TestAllocateClampsToFleetHeadroom(t *testing.T) {
	alloc := Allocate(fleet(), 500000)
	if math.Abs(sum(alloc)-75000) > 1 {
		t.Fatalf("over-headroom target should pin at 75 kW, got %v", sum(alloc))
	}
	for id, w := range alloc {
		if w > 25000+1 {
			t.Errorf("%s exceeds its cap: %v", id, w)
		}
	}
}

func TestAllocateBiasesChargeTowardLowSoC(t *testing.T) {
	alloc := Allocate(fleet(), 30000)
	if !(alloc["r1"] > alloc["r2"] && alloc["r2"] > alloc["r3"]) {
		t.Errorf("charge should favor low SoC: %v", alloc)
	}
	// Discharge is the mirror.
	alloc = Allocate(fleet(), -30000)
	if !(-alloc["r3"] > -alloc["r2"] && -alloc["r2"] > -alloc["r1"]) {
		t.Errorf("discharge should favor high SoC: %v", alloc)
	}
}

func TestAllocateExcludesOfflineAndPinnedUnits(t *testing.T) {
	units := fleet()
	units[1].Online = false
	alloc := Allocate(units, 30000)
	if alloc["r2"] != 0 {
		t.Fatalf("offline unit got %v", alloc["r2"])
	}
	if math.Abs(sum(alloc)-30000) > 1 {
		t.Fatalf("healthy units should absorb the full target: %v", sum(alloc))
	}

	// A full unit never charges; an empty one never discharges.
	units = fleet()
	units[0].SoC = 1.0
	alloc = Allocate(units, 30000)
	if alloc["r1"] != 0 {
		t.Errorf("full unit charging: %v", alloc["r1"])
	}
	units = fleet()
	units[2].SoC = 0.0
	alloc = Allocate(units, -30000)
	if alloc["r3"] != 0 {
		t.Errorf("empty unit discharging: %v", alloc["r3"])
	}
}

func TestAllocateZeroAndNoCandidates(t *testing.T) {
	if s := sum(Allocate(fleet(), 0)); s != 0 {
		t.Errorf("zero target: %v", s)
	}
	units := fleet()
	for i := range units {
		units[i].Online = false
	}
	if s := sum(Allocate(units, 30000)); s != 0 {
		t.Errorf("all offline: %v", s)
	}
}

func TestSummarizeCountsOnlyOnlineUnits(t *testing.T) {
	units := fleet()
	units[2].Online = false
	agg := Summarize(units)
	if agg.UnitsOnline != 2 || agg.UnitsTotal != 3 {
		t.Fatalf("counts: %+v", agg)
	}
	if agg.CapacityWh != 100000 {
		t.Errorf("offline capacity leaked in: %v", agg.CapacityWh)
	}
	if math.Abs(agg.SoC-0.45) > 1e-9 {
		t.Errorf("soc %v, want 0.45", agg.SoC)
	}
	if agg.AvailableChargeW != 50000 || agg.AvailableDischargeW != 50000 {
		t.Errorf("headroom: %+v", agg)
	}
}
