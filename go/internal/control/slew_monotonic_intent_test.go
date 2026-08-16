package control

import (
	"math"
	"testing"
	"time"
)

// Regression for #816. A non-following battery can report charge above the
// command cap for hours. The meter clamp computes the absolute target that
// closes the live grid error, but slew then rebuilds a larger charge command
// from the unchanged measurement and the final 5 kW cap hides that mismatch.
func TestSlewNeverCommandsMoreChargeThanTheMeterClampIdeal(t *testing.T) {
	const (
		liveBatteryW = 6551.0
		gridW        = 4494.0
		gridTargetW  = -1122.0
	)
	store := seedStore(gridW, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", liveBatteryW, 0.79},
	})
	st := NewState(gridTargetW, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewEnabled = true
	st.SlewRateW = 500

	// Force enough same-direction integral that the live-meter clamp binds at
	// idealTarget rather than the PI stopping short of it.
	for i := 0; i < 200; i++ {
		st.PI.Update(gridW)
	}

	targets := ComputeDispatch(store, st, map[string]float64{"ferroamp": 15200}, 11040)
	if len(targets) != 1 {
		t.Fatalf("targets = %+v, want one battery target", targets)
	}

	errW := gridW - gridTargetW
	idealTargetW := liveBatteryW - errW
	if got := targets[0].TargetW; got < idealTargetW-1 || got > idealTargetW+1 {
		t.Fatalf("target = %.0f W, meter-clamp ideal = %.0f W; slew did not preserve the absolute meter clamp",
			got, idealTargetW)
	}
}

func TestSlewNeverCommandsMoreDischargeThanTheMeterClampIdeal(t *testing.T) {
	const (
		liveBatteryW = -6551.0
		gridW        = -4494.0
		gridTargetW  = 1122.0
	)
	store := seedStore(gridW, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", liveBatteryW, 0.21},
	})
	st := NewState(gridTargetW, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewEnabled = true
	st.SlewRateW = 500

	for i := 0; i < 200; i++ {
		st.PI.Update(gridW)
	}

	targets := ComputeDispatch(store, st, map[string]float64{"ferroamp": 15200}, 11040)
	if len(targets) != 1 {
		t.Fatalf("targets = %+v, want one battery target", targets)
	}

	errW := gridW - gridTargetW
	idealTargetW := liveBatteryW - errW
	if got := targets[0].TargetW; got < idealTargetW-1 || got > idealTargetW+1 {
		t.Fatalf("target = %.0f W, meter-clamp ideal = %.0f W; slew did not preserve the absolute meter clamp",
			got, idealTargetW)
	}
}

func TestMeterClampSlewKeepsMixedFleetOnAggregateIdeal(t *testing.T) {
	tests := []struct {
		name                       string
		liveFirstW, gridW, soc     float64
		wantTotalW, wantSecondSlew float64
	}{
		{
			name:       "reduce charge",
			liveFirstW: 6551, gridW: 4000, soc: 0.79,
			wantTotalW: 2551, wantSecondSlew: 500,
		},
		{
			name:       "reduce discharge",
			liveFirstW: -6551, gridW: -4000, soc: 0.21,
			wantTotalW: -2551, wantSecondSlew: -500,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := seedStore(tt.gridW, []struct {
				name          string
				currentW, soc float64
			}{
				{"ferroamp", tt.liveFirstW, tt.soc},
				{"sungrow", 0, tt.soc},
			})
			st := NewState(0, 50, "ferroamp")
			st.Mode = ModeSelfConsumption
			st.SlewEnabled = true
			st.SlewRateW = 500
			for i := 0; i < 200; i++ {
				st.PI.Update(tt.gridW)
			}

			targets := ComputeDispatch(store, st, map[string]float64{
				"ferroamp": 15200,
				"sungrow":  15200,
			}, 11040)
			if len(targets) != 2 {
				t.Fatalf("targets = %+v, want two battery targets", targets)
			}
			var totalW, secondW float64
			for _, target := range targets {
				totalW += target.TargetW
				if target.Driver == "sungrow" {
					secondW = target.TargetW
				}
			}
			if math.Abs(totalW-tt.wantTotalW) > 1 {
				t.Fatalf("fleet target = %.1f W, want aggregate meter-clamp ideal %.1f W; per-driver slew crossed the fleet boundary",
					totalW, tt.wantTotalW)
			}
			if secondW != tt.wantSecondSlew {
				t.Fatalf("ordinary sibling slew = %.1f W, want %.1f W", secondW, tt.wantSecondSlew)
			}
		})
	}
}

func TestProjectSlewToFleetClampIsBoundedAndOrderIndependent(t *testing.T) {
	type values struct {
		liveW, preSlewW, ordinaryW, snappedW, wantW float64
	}
	base := map[string]values{
		"alpha":   {liveW: 4000, preSlewW: 1000, ordinaryW: 3500, snappedW: 1000, wantW: 1937.5},
		"bravo":   {liveW: 3000, preSlewW: 1000, ordinaryW: 2500, snappedW: 1000, wantW: 1562.5},
		"charlie": {liveW: 0, preSlewW: 2000, ordinaryW: 500, snappedW: 500, wantW: 500},
	}
	orders := []struct {
		name    string
		drivers []string
	}{
		{name: "alphabetical", drivers: []string{"alpha", "bravo", "charlie"}},
		{name: "rotated", drivers: []string{"charlie", "alpha", "bravo"}},
		{name: "rotated again", drivers: []string{"bravo", "charlie", "alpha"}},
	}
	for _, sign := range []float64{1, -1} {
		for _, order := range orders {
			name := "charge"
			if sign < 0 {
				name = "discharge"
			}
			t.Run(name+"/"+order.name, func(t *testing.T) {
				targets := make([]DispatchTarget, 0, len(order.drivers))
				points := make([]slewFleetPoint, 0, len(order.drivers))
				for index, driver := range order.drivers {
					v := base[driver]
					targets = append(targets, DispatchTarget{Driver: driver, TargetW: sign * v.snappedW})
					points = append(points, slewFleetPoint{
						index: index,
						liveW: sign * v.liveW, preSlewW: sign * v.preSlewW, ordinaryW: sign * v.ordinaryW,
					})
				}

				got := projectSlewToFleetClamp(targets, points)
				var totalW float64
				for _, target := range got {
					v := base[target.Driver]
					wantW := sign * v.wantW
					if math.Abs(target.TargetW-wantW) > 1e-6 {
						t.Errorf("%s = %.3f W, want %.3f W", target.Driver, target.TargetW, wantW)
					}
					lowerW := math.Min(sign*v.liveW, sign*v.preSlewW)
					upperW := math.Max(sign*v.liveW, sign*v.preSlewW)
					if target.TargetW < lowerW-1e-6 || target.TargetW > upperW+1e-6 {
						t.Errorf("%s = %.3f W outside [%.3f, %.3f]", target.Driver, target.TargetW, lowerW, upperW)
					}
					totalW += target.TargetW
				}
				if wantTotalW := sign * 4000; math.Abs(totalW-wantTotalW) > 1e-6 {
					t.Errorf("fleet target = %.3f W, want boundary %.3f W", totalW, wantTotalW)
				}
			})
		}
	}
}

func TestProjectSlewToFleetClampPreservesSafeOrdinaryVector(t *testing.T) {
	targets := []DispatchTarget{
		{Driver: "alpha", TargetW: 3500},
		{Driver: "bravo", TargetW: 2500},
		{Driver: "charlie", TargetW: 500},
	}
	points := []slewFleetPoint{
		{index: 0, liveW: 4000, preSlewW: 1000, ordinaryW: 3500},
		{index: 1, liveW: 3000, preSlewW: 1000, ordinaryW: 2500},
		{index: 2, liveW: 0, preSlewW: 2000, ordinaryW: 500},
	}

	got := projectSlewToFleetClamp(targets, points)
	for i, want := range []float64{3500, 2500, 500} {
		if got[i].TargetW != want || got[i].Clamped {
			t.Errorf("target[%d] = %+v, want unchanged %.0f W ordinary slew", i, got[i], want)
		}
	}
}

func TestOrdinarySlewStillMovesOneStepFromLiveOutput(t *testing.T) {
	tests := []struct {
		name, driver        string
		liveW, holdW, wantW float64
		soc                 float64
	}{
		{name: "reduce charge", driver: "charge-battery", liveW: 2000, holdW: 1000, wantW: 1500, soc: 0.5},
		{name: "reduce discharge", driver: "discharge-battery", liveW: -2000, holdW: -1000, wantW: -1500, soc: 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := seedStore(0, []struct {
				name          string
				currentW, soc float64
			}{{tt.driver, tt.liveW, tt.soc}})
			st := NewState(0, 50, "meter")
			st.Mode = ModeSelfConsumption
			st.SlewEnabled = true
			st.SlewRateW = 500
			st.SetBatteryManualHold(BatteryManualHold{
				PowerW:    tt.holdW,
				ExpiresAt: time.Now().Add(time.Minute),
			})

			targets := ComputeDispatch(store, st, map[string]float64{tt.driver: 15200}, 11040)
			if len(targets) != 1 {
				t.Fatalf("targets = %+v, want one battery target", targets)
			}
			if got := targets[0].TargetW; got != tt.wantW {
				t.Fatalf("target = %.0f W, want one 500 W slew step to %.0f W", got, tt.wantW)
			}
		})
	}
}
