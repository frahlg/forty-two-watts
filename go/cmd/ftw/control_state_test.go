package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/mpc"
)

func TestControlStateFromConfigAppliesSiteGain(t *testing.T) {
	cfg := &config.Config{
		Site: config.Site{
			Gain:                 0.8,
			GridToleranceW:       50,
			SlewRateW:            500,
			MinDispatchIntervalS: 5,
		},
	}
	ctrl := newControlStateFromConfig(cfg)
	if ctrl.PI.Kp != 0.8 {
		t.Fatalf("PI.Kp = %f, want configured site.gain", ctrl.PI.Kp)
	}
}

func TestControlSlotDirectiveFromMPCPreservesDecisionIdentity(t *testing.T) {
	start := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	loadpoints := map[string]float64{"ev": 250}
	in := mpc.SlotDirective{
		DecisionID: "00000000-0000-4000-8000-000000000123",
		SlotStart:  start, SlotEnd: start.Add(15 * time.Minute),
		BatteryEnergyWh: 100, SoCTargetPct: 42, Strategy: mpc.ModePassiveArbitrage,
		PVLimitW: 7000, GridW: -6400, LivePVSurplusSoCCapPct: 65,
		LoadpointEnergyWh: loadpoints,
	}

	got := controlSlotDirectiveFromMPC(in)
	if got.DecisionID != in.DecisionID || !got.SlotStart.Equal(in.SlotStart) || !got.SlotEnd.Equal(in.SlotEnd) {
		t.Fatalf("identity or timing changed across adapter: %+v", got)
	}
	if got.BatteryEnergyWh != in.BatteryEnergyWh || got.PlannedGridW != in.GridW || !got.HasPlannedGridW {
		t.Fatalf("power allocation changed across adapter: %+v", got)
	}
	if !reflect.DeepEqual(got.LoadpointEnergyWh, loadpoints) {
		t.Fatalf("loadpoint allocation changed across adapter: %+v", got.LoadpointEnergyWh)
	}
}
