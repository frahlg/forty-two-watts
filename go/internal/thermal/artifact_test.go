package thermal

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

const pythonArtifact1R1C = `{
  "schema_version": 2,
  "kind": "ftw.thermal_twin",
  "model_type": "ftw-1r1c-v1",
  "site_id": "home",
  "home_spec_revision": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "model_id": "home-zone",
  "revision": "741b9a13eb50ff407a14ea109b5c2b7468c2d99ecca4addbf225ef07a13ec17e",
  "physics": {
    "heat_loss_w_per_k": 180,
    "thermal_capacity_wh_per_k": 12000,
    "cop_curve": {
      "reference_temperature_c": 7,
      "cop_at_reference": 3.4,
      "slope_per_c": 0.05,
      "minimum_cop": 1.5,
      "maximum_cop": 5.5
    }
  },
  "residual": {"constant_heat_w": 350},
  "calibration": {
    "source": "heat_pump_submeter",
    "dataset_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "resampling_recipe": "synthetic-ground-truth-v2",
    "calibrator_version": "ftw-thermal-calibrator-v2",
    "promotion_policy_version": "ftw-thermal-promotion-v2",
    "sample_count": 673,
    "transition_count": 672,
    "start_timestamp_s": 0.0,
    "end_timestamp_s": 604800.0,
    "step_s": 900.0,
    "train_transition_count": 504,
    "validation_transition_count": 168,
    "standardized_condition_number": 2.0,
    "one_step_rmse_c": 0.0,
    "rollout_rmse_c": 0.0,
    "persistence_rmse_c": 0.1,
    "solver_converged": true,
    "parameter_bounds_hit": false,
    "observed_outdoor_min_c": -20.0,
    "observed_outdoor_max_c": 20.0,
    "observed_power_min_w": 0.0,
    "observed_power_max_w": 4000.0,
    "promotable": true,
    "promotion_reasons": []
  }
}`

const pythonArtifact2R2C = `{
  "schema_version": 2,
  "kind": "ftw.thermal_twin",
  "model_type": "ftw-2r2c-v1",
  "site_id": "home",
  "home_spec_revision": "54bf3e8b6d369d71c214057354a1c8066ea5414e31f73b349c5a8f22cf059cfe",
  "model_id": "main",
  "revision": "9ad73367a0dbadf370befe27e673e532f99a23e2cce717cd5669b2a07ec63a3b",
  "physics": {
    "heat_loss_w_per_k": 160.0,
    "mass_coupling_w_per_k": 900.0,
    "air_capacity_wh_per_k": 1200.0,
    "mass_capacity_wh_per_k": 14000.0,
    "cop_curve": {
      "reference_temperature_c": 7.0,
      "cop_at_reference": 3.4,
      "slope_per_c": 0.05,
      "minimum_cop": 1.5,
      "maximum_cop": 5.5
    }
  },
  "residual": {"constant_heat_w": 250.0},
  "calibration": {
    "source": "heat_pump_submeter",
    "dataset_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "resampling_recipe": "synthetic-ground-truth-v2",
    "calibrator_version": "ftw-thermal-calibrator-v2",
    "promotion_policy_version": "ftw-thermal-promotion-v2",
    "sample_count": 673,
    "transition_count": 672,
    "start_timestamp_s": 0.0,
    "end_timestamp_s": 604800.0,
    "step_s": 900.0,
    "train_transition_count": 504,
    "validation_transition_count": 168,
    "standardized_condition_number": 2.0,
    "one_step_rmse_c": 0.0,
    "rollout_rmse_c": 0.0,
    "persistence_rmse_c": 0.1,
    "solver_converged": true,
    "parameter_bounds_hit": false,
    "observed_outdoor_min_c": -20.0,
    "observed_outdoor_max_c": 20.0,
    "observed_power_min_w": 0.0,
    "observed_power_max_w": 4000.0,
    "promotable": true,
    "promotion_reasons": []
  }
}`

func mustArtifact(t *testing.T) Artifact {
	t.Helper()
	artifact, err := ParseArtifact([]byte(pythonArtifact1R1C))
	if err != nil {
		t.Fatal(err)
	}
	return *artifact
}

func TestParseArtifactAcceptsPythonRevision(t *testing.T) {
	artifact := mustArtifact(t)
	if artifact.ModelType != ModelType1R1C || artifact.ModelID != "home-zone" {
		t.Fatalf("unexpected artifact: %+v", artifact)
	}
}

func TestParseTwoR2CArtifactAcceptsPythonRevision(t *testing.T) {
	artifact, err := ParseArtifact([]byte(pythonArtifact2R2C))
	if err != nil {
		t.Fatal(err)
	}
	if artifact.ModelType != ModelType2R2C || artifact.Physics.MassCapacityWhPerK == nil {
		t.Fatalf("unexpected artifact: %+v", artifact)
	}
}

func TestTypedRevisionMatchesPythonForUnicodeAndHTMLCharacters(t *testing.T) {
	artifact := mustArtifact(t)
	artifact.SiteID = "hem-å<&"
	artifact.ModelID = "zon-å<&"
	revision, err := artifact.contentRevision()
	if err != nil {
		t.Fatal(err)
	}
	const pythonRevision = "1e05dfec3d201f6c7c2442bfd763956716cd0a420a35f625ac57fe8610267eb0"
	if revision != pythonRevision {
		t.Fatalf("revision = %s, want Python revision %s", revision, pythonRevision)
	}
}

func TestCoreRecomputesPromotionPolicy(t *testing.T) {
	artifact := mustArtifact(t)
	artifact.Calibration.ResamplingRecipe = "series-bucket-average-v1"
	revision, err := artifact.contentRevision()
	if err != nil {
		t.Fatal(err)
	}
	artifact.Revision = revision
	if err := artifact.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := artifact.Promotable(); err == nil || !strings.Contains(err.Error(), "resampling recipe") {
		t.Fatalf("Promotable error = %v, want independent recipe rejection", err)
	}
}

func TestCoreRejectsEveryPromotionGateIndependently(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CalibrationEvidence)
	}{
		{"source", func(e *CalibrationEvidence) { e.Source = "grid_meter" }},
		{"dataset digest", func(e *CalibrationEvidence) { e.DatasetSHA256 = "" }},
		{"resampling recipe", func(e *CalibrationEvidence) { e.ResamplingRecipe = "unknown" }},
		{"calibrator version", func(e *CalibrationEvidence) { e.CalibratorVersion = "old" }},
		{"policy version", func(e *CalibrationEvidence) { e.PromotionPolicyVersion = "old" }},
		{"training count", func(e *CalibrationEvidence) {
			e.TrainTransitionCount, e.ValidationTransitionCount = 31, e.TransitionCount-31
		}},
		{"validation count", func(e *CalibrationEvidence) {
			e.ValidationTransitionCount, e.TrainTransitionCount = 7, e.TransitionCount-7
		}},
		{"duration", func(e *CalibrationEvidence) { e.StepS, e.EndTimestampS = 300, float64(e.TransitionCount)*300 }},
		{"time grid", func(e *CalibrationEvidence) { e.EndTimestampS += 20_000 }},
		{"condition number", func(e *CalibrationEvidence) { e.StandardizedConditionNumber = 101 }},
		{"one-step error", func(e *CalibrationEvidence) { e.OneStepRMSEC = 0.51 }},
		{"rollout error", func(e *CalibrationEvidence) { e.RolloutRMSEC = 1.01 }},
		{"persistence baseline", func(e *CalibrationEvidence) { e.OneStepRMSEC = e.PersistenceRMSEC }},
		{"solver status", func(e *CalibrationEvidence) { e.SolverConverged = false }},
		{"parameter bound", func(e *CalibrationEvidence) { e.ParameterBoundsHit = true }},
		{"producer decision", func(e *CalibrationEvidence) { e.Promotable = false }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			artifact := mustArtifact(t)
			test.mutate(&artifact.Calibration)
			revision, err := artifact.contentRevision()
			if err != nil {
				t.Fatal(err)
			}
			artifact.Revision = revision
			if err := artifact.Validate(); err != nil {
				t.Fatalf("mutated artifact is structurally invalid: %v", err)
			}
			if err := artifact.Promotable(); err == nil {
				t.Fatal("Core accepted evidence that failed a promotion gate")
			}
		})
	}
}

func TestArtifactRejectsImpossibleConditionNumber(t *testing.T) {
	artifact := mustArtifact(t)
	artifact.Calibration.StandardizedConditionNumber = 0
	revision, err := artifact.contentRevision()
	if err != nil {
		t.Fatal(err)
	}
	artifact.Revision = revision
	if err := artifact.Validate(); err == nil || !strings.Contains(err.Error(), "condition number") {
		t.Fatalf("Validate error = %v, want condition-number rejection", err)
	}
}

func TestParseArtifactRejectsTamperedPhysics(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(pythonArtifact1R1C), &raw); err != nil {
		t.Fatal(err)
	}
	raw["physics"].(map[string]any)["heat_loss_w_per_k"] = 181
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseArtifact(data); err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatalf("ParseArtifact error = %v, want revision mismatch", err)
	}
}

func TestOptimizerLoadAllowsColdInitialRoom(t *testing.T) {
	artifact := mustArtifact(t)
	load, err := artifact.OptimizerLoad(OptimizerLoadInput{
		InitialTemperatureC:   18,
		MinimumTemperatureC:   19,
		MaximumTemperatureC:   23,
		OutsideTemperatureC:   []float64{-5, -4},
		MaximumElectricPowerW: 4_000,
		AllowedStepsW:         []float64{4_000, 0, 2_000, 2_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(load.AllowedStepsW) != 3 || load.AllowedStepsW[0] != 0 || load.InitialTempC != 18 {
		t.Fatalf("unexpected optimizer load: %+v", load)
	}
}

func TestOptimizerLoadRejectsWeatherOutsideEvidenceRange(t *testing.T) {
	artifact := mustArtifact(t)
	_, err := artifact.OptimizerLoad(OptimizerLoadInput{
		InitialTemperatureC:   20,
		MinimumTemperatureC:   19,
		MaximumTemperatureC:   23,
		OutsideTemperatureC:   []float64{-21},
		MaximumElectricPowerW: 4_000,
	})
	if err == nil || !strings.Contains(err.Error(), "operating range") {
		t.Fatalf("OptimizerLoad error = %v, want operating-range rejection", err)
	}
}

func TestOneR1CTransitionMatchesClosedForm(t *testing.T) {
	artifact := mustArtifact(t)
	load, err := artifact.OptimizerLoad(OptimizerLoadInput{
		InitialTemperatureC:   20,
		MinimumTemperatureC:   19,
		MaximumTemperatureC:   23,
		OutsideTemperatureC:   []float64{0},
		MaximumElectricPowerW: 4_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	next, err := load.NextState(0, ModelState{AirC: 20, MassC: 20}, 1_200, 0.25)
	if err != nil {
		t.Fatal(err)
	}
	// The Python reference uses the same exact exponential transition.
	if math.Abs(next.AirC-20.00852567104245) > 1e-12 {
		t.Fatalf("next air = %.14f", next.AirC)
	}
}

func TestTwoR2CTransitionKeepsEquilibriumFixed(t *testing.T) {
	artifact := mustArtifact(t)
	artifact.ModelType = ModelType2R2C
	artifact.Physics.ThermalCapacityWhPerK = nil
	massCoupling, airCapacity, massCapacity := 900.0, 1_200.0, 14_000.0
	artifact.Physics.MassCouplingWPerK = &massCoupling
	artifact.Physics.AirCapacityWhPerK = &airCapacity
	artifact.Physics.MassCapacityWhPerK = &massCapacity
	revision, err := artifact.contentRevision()
	if err != nil {
		t.Fatal(err)
	}
	artifact.Revision = revision
	load, err := artifact.OptimizerLoad(OptimizerLoadInput{
		InitialTemperatureC:   10,
		MinimumTemperatureC:   5,
		MaximumTemperatureC:   30,
		OutsideTemperatureC:   []float64{0},
		MaximumElectricPowerW: 4_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	equilibrium := load.OutsideTempC[0] + load.DisturbanceHeatW[0]/load.HeatLossWPerK
	next, err := load.NextState(0, ModelState{AirC: equilibrium, MassC: equilibrium}, 0, time.Hour.Hours())
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(next.AirC-equilibrium) > 1e-12 || math.Abs(next.MassC-equilibrium) > 1e-12 {
		t.Fatalf("equilibrium moved: %+v, want %.12f", next, equilibrium)
	}
}

func TestTwoR2CTransitionStaysFiniteForStiffValidModel(t *testing.T) {
	artifact := mustArtifact(t)
	artifact.ModelType = ModelType2R2C
	artifact.Physics.ThermalCapacityWhPerK = nil
	massCoupling, airCapacity, massCapacity := 4_000.0, 20.0, 30_000.0
	artifact.Physics.MassCouplingWPerK = &massCoupling
	artifact.Physics.AirCapacityWhPerK = &airCapacity
	artifact.Physics.MassCapacityWhPerK = &massCapacity
	revision, err := artifact.contentRevision()
	if err != nil {
		t.Fatal(err)
	}
	artifact.Revision = revision
	load, err := artifact.OptimizerLoad(OptimizerLoadInput{
		InitialTemperatureC: 20, MinimumTemperatureC: 15, MaximumTemperatureC: 25,
		OutsideTemperatureC: []float64{-10}, MaximumElectricPowerW: 4_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	next, err := load.NextState(0, ModelState{AirC: 20, MassC: 20}, 2_000, 4)
	if err != nil {
		t.Fatal(err)
	}
	if math.IsNaN(next.AirC) || math.IsInf(next.AirC, 0) || math.IsNaN(next.MassC) || math.IsInf(next.MassC, 0) {
		t.Fatalf("stiff transition is not finite: %+v", next)
	}
}

func TestOptimizerLoadRejectsNonHexRevisionAndMixedModelParameters(t *testing.T) {
	artifact := mustArtifact(t)
	load, err := artifact.OptimizerLoad(OptimizerLoadInput{
		InitialTemperatureC: 20, MinimumTemperatureC: 19, MaximumTemperatureC: 23,
		OutsideTemperatureC: []float64{0}, MaximumElectricPowerW: 4_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	load.SourceRevision = strings.Repeat("z", 64)
	if err := load.Validate(1); err == nil || !strings.Contains(err.Error(), "non-hex") {
		t.Fatalf("non-hex revision error = %v", err)
	}
	load.SourceRevision = artifact.Revision
	extra := 900.0
	load.MassCouplingWPerK = &extra
	if err := load.Validate(1); err == nil || !strings.Contains(err.Error(), "1R1C") {
		t.Fatalf("mixed parameter error = %v", err)
	}
}
