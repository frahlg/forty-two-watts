package control

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestClampWithSoCRespectsReserveFloor(t *testing.T) {
	b := batteryInfo{soc: 0.30, reserveFloorSoC: 0.40, capacityWh: 10000}
	if got, clamped := clampWithSoC(-2000, b); got != 0 || !clamped {
		t.Errorf("discharge below reserve floor: got %v (clamped=%v), want 0", got, clamped)
	}
	// Charging toward recovery stays allowed.
	if got, _ := clampWithSoC(2000, b); got != 2000 {
		t.Errorf("charge below reserve floor should pass, got %v", got)
	}
	// Above the floor, discharge passes.
	b.soc = 0.45
	if got, _ := clampWithSoC(-2000, b); got != -2000 {
		t.Errorf("discharge above reserve floor should pass, got %v", got)
	}
	// No reserve: only the 5% empty floor applies.
	b = batteryInfo{soc: 0.30}
	if got, _ := clampWithSoC(-2000, b); got != -2000 {
		t.Errorf("no-reserve battery at 30%% should discharge, got %v", got)
	}
}

func TestEffectiveImportCeilingIncludesNMD(t *testing.T) {
	s := &State{SiteFuseAmps: 100, SiteFuseVoltage: 230, SiteFusePhases: 3}
	fuseMax := 100.0 * 230 * 3
	base := s.effectiveImportCeilingW(fuseMax)

	s.NMDImportCeilingW = 40000
	if got := s.effectiveImportCeilingW(fuseMax); got != 40000 {
		t.Errorf("NMD should bind: got %v, want 40000", got)
	}
	// NMD above the fuse never loosens the fuse ceiling.
	s.NMDImportCeilingW = fuseMax * 2
	if got := s.effectiveImportCeilingW(fuseMax); got != base {
		t.Errorf("NMD above fuse must not loosen: got %v, want %v", got, base)
	}
	// Tighter of peak and NMD wins.
	s.NMDImportCeilingW = 40000
	s.PeakImportCeilingW = 30000
	if got := s.effectiveImportCeilingW(fuseMax); got != 30000 {
		t.Errorf("tighter peak should bind: got %v", got)
	}
}

// A billing-only overage (NMD) must not force-discharge a fleet that is
// holding its backup reserve; a genuine fuse overage may.
func TestForceFuseDischargeRespectsReserveUnlessFuseEmergency(t *testing.T) {
	newState := func() (*State, *telemetry.Store, map[string]float64) {
		tel := telemetry.NewStore()
		soc := 0.30
		tel.Update("meter", telemetry.DerMeter, 45000, nil, nil)
		tel.Update("bat", telemetry.DerBattery, 0, &soc, nil)
		s := NewState(0, 42, "meter")
		s.SiteFuseAmps = 100
		s.SiteFuseVoltage = 230
		s.SiteFusePhases = 3
		s.BackupReserveWh = 4000 // 40% of 10 kWh
		return s, tel, map[string]float64{"bat": 10000}
	}

	// Import 45 kW, fuse 69 kW → no fuse emergency. NMD 40 kW → billing
	// overage of ~5 kW, but the pack sits below the 40% reserve floor.
	s, tel, caps := newState()
	s.NMDImportCeilingW = 40000
	targets := []DispatchTarget{{Driver: "bat", TargetW: 0}}
	out := forceFuseDischarge(targets, tel, s, caps, s.SiteFuseAmps*s.SiteFuseVoltage*3)
	if out[0].TargetW != 0 {
		t.Errorf("billing overage drained through the backup reserve: %v", out[0].TargetW)
	}

	// Same pack, but a genuine fuse overage (import 80 kW > 69 kW fuse):
	// hardware wins, the pack discharges through the reserve.
	s, tel, caps = newState()
	tel.Update("meter", telemetry.DerMeter, 80000, nil, nil)
	out = forceFuseDischarge([]DispatchTarget{{Driver: "bat", TargetW: 0}}, tel, s, caps,
		s.SiteFuseAmps*s.SiteFuseVoltage*3)
	if out[0].TargetW >= 0 {
		t.Errorf("fuse emergency must discharge through the reserve, got %v", out[0].TargetW)
	}
}
