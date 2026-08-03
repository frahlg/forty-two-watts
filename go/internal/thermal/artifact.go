package thermal

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

const (
	ArtifactKind           = "ftw.thermal_twin"
	ArtifactSchemaVersion  = 2
	ModelType1R1C          = "ftw-1r1c-v1"
	ModelType2R2C          = "ftw-2r2c-v1"
	CalibratorVersion      = "ftw-thermal-calibrator-v2"
	PromotionPolicyVersion = "ftw-thermal-promotion-v2"
)

var approvedResamplingRecipes = map[string]bool{
	"aligned-boundary-zoh-v2":   true,
	"synthetic-ground-truth-v2": true,
}

// COPCurve maps outdoor temperature to heat delivered per electrical watt.
// Drivers only report electrical power; the artifact owns this conversion.
type COPCurve struct {
	ReferenceTemperatureC float64 `json:"reference_temperature_c"`
	COPAtReference        float64 `json:"cop_at_reference"`
	SlopePerC             float64 `json:"slope_per_c"`
	MinimumCOP            float64 `json:"minimum_cop"`
	MaximumCOP            float64 `json:"maximum_cop"`
}

func (c COPCurve) At(outdoorC float64) float64 {
	raw := c.COPAtReference + c.SlopePerC*(outdoorC-c.ReferenceTemperatureC)
	return math.Min(c.MaximumCOP, math.Max(c.MinimumCOP, raw))
}

func (c COPCurve) validate() error {
	values := []float64{c.ReferenceTemperatureC, c.COPAtReference, c.SlopePerC, c.MinimumCOP, c.MaximumCOP}
	for _, value := range values {
		if !finite(value) {
			return errors.New("cop curve contains a non-finite value")
		}
	}
	if c.MinimumCOP <= 0 {
		return errors.New("cop curve minimum must be positive")
	}
	if c.MaximumCOP < c.MinimumCOP {
		return errors.New("cop curve maximum must not be below its minimum")
	}
	if c.COPAtReference < c.MinimumCOP || c.COPAtReference > c.MaximumCOP {
		return errors.New("cop at reference must lie within the curve bounds")
	}
	return nil
}

// CalibrationEvidence records enough provenance and validation data for Core
// to apply its own promotion policy. The producer's Promotable field is only
// one input to that decision.
type CalibrationEvidence struct {
	Source                      string   `json:"source"`
	DatasetSHA256               string   `json:"dataset_sha256"`
	ResamplingRecipe            string   `json:"resampling_recipe"`
	CalibratorVersion           string   `json:"calibrator_version"`
	PromotionPolicyVersion      string   `json:"promotion_policy_version"`
	SampleCount                 int      `json:"sample_count"`
	TransitionCount             int      `json:"transition_count"`
	StartTimestampS             float64  `json:"start_timestamp_s"`
	EndTimestampS               float64  `json:"end_timestamp_s"`
	StepS                       float64  `json:"step_s"`
	TrainTransitionCount        int      `json:"train_transition_count"`
	ValidationTransitionCount   int      `json:"validation_transition_count"`
	StandardizedConditionNumber float64  `json:"standardized_condition_number"`
	OneStepRMSEC                float64  `json:"one_step_rmse_c"`
	RolloutRMSEC                float64  `json:"rollout_rmse_c"`
	PersistenceRMSEC            float64  `json:"persistence_rmse_c"`
	SolverConverged             bool     `json:"solver_converged"`
	ParameterBoundsHit          bool     `json:"parameter_bounds_hit"`
	ObservedOutdoorMinC         float64  `json:"observed_outdoor_min_c"`
	ObservedOutdoorMaxC         float64  `json:"observed_outdoor_max_c"`
	ObservedPowerMinW           float64  `json:"observed_power_min_w"`
	ObservedPowerMaxW           float64  `json:"observed_power_max_w"`
	Promotable                  bool     `json:"promotable"`
	PromotionReasons            []string `json:"promotion_reasons"`
}

func validSHA256(value string, allowEmpty bool) bool {
	if value == "" {
		return allowEmpty
	}
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character < '0' || (character > '9' && character < 'a') || character > 'f' {
			return false
		}
	}
	return true
}

func (e CalibrationEvidence) validate() error {
	if strings.TrimSpace(e.Source) == "" {
		return errors.New("calibration source is empty")
	}
	if !validSHA256(e.DatasetSHA256, true) {
		return errors.New("calibration dataset_sha256 must be lowercase SHA-256 or empty")
	}
	if e.SampleCount < 0 || e.TransitionCount < 0 || e.TrainTransitionCount < 0 || e.ValidationTransitionCount < 0 {
		return errors.New("calibration counts must be non-negative")
	}
	if e.SampleCount != e.TransitionCount+1 {
		return errors.New("calibration sample_count must equal transition_count + 1")
	}
	if e.TrainTransitionCount+e.ValidationTransitionCount != e.TransitionCount {
		return errors.New("calibration train and validation counts must cover transitions")
	}
	if !finite(e.StartTimestampS) || !finite(e.EndTimestampS) || e.EndTimestampS <= e.StartTimestampS {
		return errors.New("calibration timestamps must be finite and increasing")
	}
	if !finite(e.StepS) || e.StepS <= 0 {
		return errors.New("calibration step must be positive")
	}
	if !finite(e.StandardizedConditionNumber) || e.StandardizedConditionNumber <= 0 {
		return errors.New("calibration condition number must be finite and positive")
	}
	for name, value := range map[string]float64{
		"one-step RMSE":    e.OneStepRMSEC,
		"rollout RMSE":     e.RolloutRMSEC,
		"persistence RMSE": e.PersistenceRMSEC,
	} {
		if !finite(value) || value < 0 {
			return fmt.Errorf("calibration %s must be finite and non-negative", name)
		}
	}
	for name, value := range map[string]float64{
		"outdoor minimum": e.ObservedOutdoorMinC,
		"outdoor maximum": e.ObservedOutdoorMaxC,
		"power minimum":   e.ObservedPowerMinW,
		"power maximum":   e.ObservedPowerMaxW,
	} {
		if !finite(value) {
			return fmt.Errorf("calibration observed %s must be finite", name)
		}
	}
	if e.ObservedOutdoorMinC > e.ObservedOutdoorMaxC {
		return errors.New("calibration observed outdoor range is reversed")
	}
	if e.ObservedPowerMinW < 0 || e.ObservedPowerMinW > e.ObservedPowerMaxW {
		return errors.New("calibration observed power range is invalid")
	}
	if e.Promotable && len(e.PromotionReasons) > 0 {
		return errors.New("a promotable calibration cannot have rejection reasons")
	}
	return nil
}

type Artifact struct {
	SchemaVersion    int                 `json:"schema_version"`
	Kind             string              `json:"kind"`
	ModelType        string              `json:"model_type"`
	SiteID           string              `json:"site_id"`
	HomeSpecRevision string              `json:"home_spec_revision"`
	ModelID          string              `json:"model_id"`
	Revision         string              `json:"revision"`
	Physics          ArtifactPhysics     `json:"physics"`
	Residual         ArtifactResidual    `json:"residual"`
	Calibration      CalibrationEvidence `json:"calibration"`
}

type ArtifactPhysics struct {
	HeatLossWPerK         float64  `json:"heat_loss_w_per_k"`
	ThermalCapacityWhPerK *float64 `json:"thermal_capacity_wh_per_k,omitempty"`
	MassCouplingWPerK     *float64 `json:"mass_coupling_w_per_k,omitempty"`
	AirCapacityWhPerK     *float64 `json:"air_capacity_wh_per_k,omitempty"`
	MassCapacityWhPerK    *float64 `json:"mass_capacity_wh_per_k,omitempty"`
	COPCurve              COPCurve `json:"cop_curve"`
}

type ArtifactResidual struct {
	ConstantHeatW float64 `json:"constant_heat_w"`
}

func ParseArtifact(data []byte) (*Artifact, error) {
	var artifact Artifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return nil, fmt.Errorf("decode thermal artifact: %w", err)
	}
	if err := artifact.Validate(); err != nil {
		return nil, err
	}
	return &artifact, nil
}

func inRange(value, minimum, maximum float64) bool {
	return finite(value) && value >= minimum && value <= maximum
}

func (a Artifact) Validate() error {
	if a.SchemaVersion != ArtifactSchemaVersion {
		return fmt.Errorf("thermal artifact schema_version is %d, want %d", a.SchemaVersion, ArtifactSchemaVersion)
	}
	if a.Kind != ArtifactKind {
		return fmt.Errorf("thermal artifact kind is %q, want %q", a.Kind, ArtifactKind)
	}
	if a.ModelType != ModelType1R1C && a.ModelType != ModelType2R2C {
		return fmt.Errorf("unsupported thermal model type %q", a.ModelType)
	}
	if strings.TrimSpace(a.SiteID) == "" || !validSHA256(a.HomeSpecRevision, false) {
		return errors.New("thermal artifact needs site_id and a valid home_spec_revision")
	}
	if strings.TrimSpace(a.ModelID) == "" {
		return errors.New("thermal artifact model_id is empty")
	}
	if !inRange(a.Physics.HeatLossWPerK, 5, 5_000) {
		return errors.New("thermal heat loss must be within 5..5000 W/K")
	}
	if !inRange(a.Residual.ConstantHeatW, -20_000, 20_000) {
		return errors.New("thermal residual heat must be within -20000..20000 W")
	}
	if err := a.Physics.COPCurve.validate(); err != nil {
		return fmt.Errorf("thermal artifact: %w", err)
	}
	if err := a.Calibration.validate(); err != nil {
		return fmt.Errorf("thermal artifact: %w", err)
	}
	check := func(name string, value *float64, minimum, maximum float64) error {
		if value == nil || !inRange(*value, minimum, maximum) {
			return fmt.Errorf("thermal %s must be within %g..%g", name, minimum, maximum)
		}
		return nil
	}
	switch a.ModelType {
	case ModelType1R1C:
		if err := check("capacity", a.Physics.ThermalCapacityWhPerK, 200, 1_000_000); err != nil {
			return err
		}
		if a.Physics.MassCouplingWPerK != nil || a.Physics.AirCapacityWhPerK != nil || a.Physics.MassCapacityWhPerK != nil {
			return errors.New("1R1C artifact contains 2R2C-only parameters")
		}
	case ModelType2R2C:
		if err := check("mass coupling", a.Physics.MassCouplingWPerK, 5, 10_000); err != nil {
			return err
		}
		if err := check("air capacity", a.Physics.AirCapacityWhPerK, 1, 600_000); err != nil {
			return err
		}
		if err := check("mass capacity", a.Physics.MassCapacityWhPerK, 1, 1_000_000); err != nil {
			return err
		}
		if a.Physics.ThermalCapacityWhPerK != nil {
			return errors.New("2R2C artifact contains a 1R1C-only capacity")
		}
	}
	want, err := a.contentRevision()
	if err != nil {
		return fmt.Errorf("compute thermal artifact revision: %w", err)
	}
	if a.Revision != want {
		return errors.New("thermal artifact revision does not match its contents")
	}
	return nil
}

func (a Artifact) promotionReasons() []string {
	e := a.Calibration
	reasons := append([]string(nil), e.PromotionReasons...)
	add := func(condition bool, reason string) {
		if condition {
			reasons = append(reasons, reason)
		}
	}
	add(e.Source != "heat_pump_submeter" && e.Source != "validated_component_balance", "calibration source is not approved")
	add(!validSHA256(e.DatasetSHA256, false), "dataset digest is missing or invalid")
	add(!approvedResamplingRecipes[e.ResamplingRecipe], "resampling recipe is not approved")
	add(e.CalibratorVersion != CalibratorVersion, "calibrator version is not approved")
	add(e.PromotionPolicyVersion != PromotionPolicyVersion, "promotion policy version is not approved")
	add(e.SampleCount != e.TransitionCount+1, "sample and transition counts are inconsistent")
	add(e.TrainTransitionCount+e.ValidationTransitionCount != e.TransitionCount, "calibration split counts are inconsistent")
	add(e.TrainTransitionCount < 32, "fewer than 32 training transitions")
	add(e.ValidationTransitionCount < 8, "fewer than eight validation transitions")
	durationS := e.EndTimestampS - e.StartTimestampS
	add(durationS < 72*3_600, "less than 72 hours of evidence")
	expectedDurationS := float64(e.TransitionCount) * e.StepS
	add(math.Abs(durationS-expectedDurationS) > math.Max(1, expectedDurationS*0.02), "calibration timestamps and step are inconsistent")
	conditionLimit := 1_000_000.0
	if a.ModelType == ModelType1R1C {
		conditionLimit = 100
	}
	add(e.StandardizedConditionNumber > conditionLimit, "calibration condition number exceeds policy")
	add(e.OneStepRMSEC > 0.5, "one-step validation RMSE exceeds 0.5 C")
	add(e.RolloutRMSEC > 1, "rollout validation RMSE exceeds 1.0 C")
	add(e.OneStepRMSEC >= e.PersistenceRMSEC, "one-step validation does not beat temperature persistence")
	add(!e.SolverConverged, "calibration solver did not converge")
	add(e.ParameterBoundsHit, "a fitted parameter reached its search bound")

	unique := make([]string, 0, len(reasons))
	seen := make(map[string]bool, len(reasons))
	for _, reason := range reasons {
		if reason != "" && !seen[reason] {
			seen[reason] = true
			unique = append(unique, reason)
		}
	}
	return unique
}

func (a Artifact) Promotable() error {
	reasons := a.promotionReasons()
	if a.Calibration.Promotable && len(reasons) == 0 {
		return nil
	}
	if len(reasons) == 0 {
		return errors.New("thermal artifact is not promotable")
	}
	return fmt.Errorf("thermal artifact is not promotable: %s", strings.Join(reasons, ", "))
}

func fingerprintCOP(w *fingerprintWriter, curve COPCurve) {
	w.Float(curve.ReferenceTemperatureC)
	w.Float(curve.COPAtReference)
	w.Float(curve.SlopePerC)
	w.Float(curve.MinimumCOP)
	w.Float(curve.MaximumCOP)
}

func fingerprintCalibration(w *fingerprintWriter, evidence CalibrationEvidence) {
	w.String(evidence.Source)
	w.String(evidence.DatasetSHA256)
	w.String(evidence.ResamplingRecipe)
	w.String(evidence.CalibratorVersion)
	w.String(evidence.PromotionPolicyVersion)
	w.Int(evidence.SampleCount)
	w.Int(evidence.TransitionCount)
	w.Float(evidence.StartTimestampS)
	w.Float(evidence.EndTimestampS)
	w.Float(evidence.StepS)
	w.Int(evidence.TrainTransitionCount)
	w.Int(evidence.ValidationTransitionCount)
	w.Float(evidence.StandardizedConditionNumber)
	w.Float(evidence.OneStepRMSEC)
	w.Float(evidence.RolloutRMSEC)
	w.Float(evidence.PersistenceRMSEC)
	w.Bool(evidence.SolverConverged)
	w.Bool(evidence.ParameterBoundsHit)
	w.Float(evidence.ObservedOutdoorMinC)
	w.Float(evidence.ObservedOutdoorMaxC)
	w.Float(evidence.ObservedPowerMinW)
	w.Float(evidence.ObservedPowerMaxW)
	w.Bool(evidence.Promotable)
	w.StringList(evidence.PromotionReasons)
}

func (a Artifact) contentRevision() (string, error) {
	w := newFingerprint("ftw.thermal_twin.v2")
	w.String(a.SiteID)
	w.String(a.HomeSpecRevision)
	w.String(a.ModelType)
	w.String(a.ModelID)
	w.Float(a.Physics.HeatLossWPerK)
	if a.ModelType == ModelType1R1C {
		if a.Physics.ThermalCapacityWhPerK == nil {
			return "", errors.New("1R1C capacity is missing")
		}
		w.Float(*a.Physics.ThermalCapacityWhPerK)
	} else {
		if a.Physics.MassCouplingWPerK == nil || a.Physics.AirCapacityWhPerK == nil || a.Physics.MassCapacityWhPerK == nil {
			return "", errors.New("2R2C parameter is missing")
		}
		w.Float(*a.Physics.MassCouplingWPerK)
		w.Float(*a.Physics.AirCapacityWhPerK)
		w.Float(*a.Physics.MassCapacityWhPerK)
	}
	fingerprintCOP(w, a.Physics.COPCurve)
	w.Float(a.Residual.ConstantHeatW)
	fingerprintCalibration(w, a.Calibration)
	return w.Sum(), nil
}
