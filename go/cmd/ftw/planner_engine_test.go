package main

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
)

// plannerEngineConfig is the smallest config buildMPC will accept: a planner,
// a price provider and one battery with capacity.
func plannerEngineConfig(planner *config.Planner) (*config.Config, map[string]float64) {
	return &config.Config{
		Price:   &config.Price{Provider: "elprisetjustnu", Zone: "SE3"},
		Planner: planner,
		Drivers: []config.Driver{{Name: "sungrow", BatteryCapacityWh: 9600}},
	}, map[string]float64{
		"sungrow": 9600,
	}
}

func TestBuildMPCDefaultsToCoreChampionWithPythonShadow(t *testing.T) {
	t.Setenv("FTW_OPTIMIZER_TRANSPORT", "process")
	cfg, capacities := plannerEngineConfig(&config.Planner{Enabled: true})

	svc := buildMPC(cfg, nil, nil, capacities)
	if svc == nil {
		t.Fatal("buildMPC returned nil for an enabled planner")
	}
	if svc.Optimizer != nil {
		t.Fatalf("unset planner.engine gave the external optimizer the champion role: %T", svc.Optimizer)
	}
	if svc.ShadowOptimizer == nil {
		t.Fatal("unset planner.shadow_python left no comparison shadow")
	}
	if svc.EnableRecourseShadow {
		t.Fatal("recourse challenger armed without an external champion to challenge")
	}
}

func TestBuildMPCExplicitPythonKeepsExternalChampion(t *testing.T) {
	t.Setenv("FTW_OPTIMIZER_TRANSPORT", "process")
	cfg, capacities := plannerEngineConfig(&config.Planner{Enabled: true, Engine: "python"})

	svc := buildMPC(cfg, nil, nil, capacities)
	if svc == nil {
		t.Fatal("buildMPC returned nil for an enabled planner")
	}
	if svc.Optimizer == nil {
		t.Fatal("planner.engine: python did not attach the external champion")
	}
	if svc.ShadowOptimizer != nil {
		t.Fatal("the external optimizer was also attached as its own shadow")
	}
	if !svc.OptimizerIsChampion() {
		t.Fatal("OptimizerIsChampion disagrees with the attached champion")
	}
}

func TestBuildMPCShadowPythonFalseLeavesCoreAlone(t *testing.T) {
	t.Setenv("FTW_OPTIMIZER_TRANSPORT", "process")
	off := false
	cfg, capacities := plannerEngineConfig(&config.Planner{Enabled: true, ShadowPython: &off})

	svc := buildMPC(cfg, nil, nil, capacities)
	if svc == nil {
		t.Fatal("buildMPC returned nil for an enabled planner")
	}
	if svc.Optimizer != nil || svc.ShadowOptimizer != nil {
		t.Fatalf("shadow_python: false still wired an optimizer: champion=%T shadow=%T",
			svc.Optimizer, svc.ShadowOptimizer)
	}
	if svc.ConfiguredOptimizer() != nil {
		t.Fatal("ConfiguredOptimizer reported a worker that is not attached")
	}
}
