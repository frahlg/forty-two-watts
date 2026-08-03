package thermal

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"
)

const (
	DefaultMetricMaxAge  = 15 * time.Minute
	MaximumMetricMaxAge  = time.Hour
	metricFutureGrace    = 30 * time.Second
	thermalPowerHeadroom = 1.25
)

// MetricReader is the small telemetry surface needed by the model runtime.
type MetricReader interface {
	LatestMetric(driver, name string) (float64, time.Time, bool)
}

type metricHealthReader interface {
	DriverHealthy(driver string) bool
}

type RuntimeOptions struct {
	MaxMetricAge  time.Duration
	AllowedStepsW []float64
}

// Runtime joins one HomeSpec, one promoted artifact, and current telemetry.
// It only prepares optimizer proposals and model checks; it cannot dispatch a
// heat-pump command.
type Runtime struct {
	Spec          HomeSpec
	Artifact      Artifact
	Metrics       MetricReader
	MaxMetricAge  time.Duration
	AllowedStepsW []float64
}

func LoadRuntime(homeSpecPath, artifactPath string, metrics MetricReader, options RuntimeOptions) (*Runtime, error) {
	homeData, err := os.ReadFile(homeSpecPath)
	if err != nil {
		return nil, fmt.Errorf("read home thermal spec: %w", err)
	}
	artifactData, err := os.ReadFile(artifactPath)
	if err != nil {
		return nil, fmt.Errorf("read thermal artifact: %w", err)
	}
	spec, err := ParseHomeSpec(homeData)
	if err != nil {
		return nil, err
	}
	artifact, err := ParseArtifact(artifactData)
	if err != nil {
		return nil, err
	}
	return NewRuntime(*spec, *artifact, metrics, options)
}

func NewRuntime(spec HomeSpec, artifact Artifact, metrics MetricReader, options RuntimeOptions) (*Runtime, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := artifact.Validate(); err != nil {
		return nil, err
	}
	if err := artifact.Promotable(); err != nil {
		return nil, err
	}
	if artifact.SiteID != spec.SiteID {
		return nil, fmt.Errorf("thermal artifact site_id %q does not match home spec site_id %q", artifact.SiteID, spec.SiteID)
	}
	if artifact.HomeSpecRevision != spec.Revision {
		return nil, errors.New("thermal artifact was calibrated for a different home spec revision")
	}
	if artifact.ModelID != spec.PrimaryZoneID {
		return nil, fmt.Errorf("thermal artifact model_id %q does not match primary zone %q", artifact.ModelID, spec.PrimaryZoneID)
	}
	if !copCurvesEqual(spec.Heating.COPCurve, artifact.Physics.COPCurve) {
		return nil, errors.New("thermal artifact COP curve does not match the home spec")
	}
	maxAge := options.MaxMetricAge
	if maxAge <= 0 {
		maxAge = DefaultMetricMaxAge
	}
	if maxAge > MaximumMetricMaxAge {
		return nil, fmt.Errorf("thermal metric maximum age must not exceed %s", MaximumMetricMaxAge)
	}
	steps, err := normalizePowerSteps(options.AllowedStepsW, spec.Heating.MaximumElectricPowerW)
	if err != nil {
		return nil, err
	}
	return &Runtime{
		Spec:          spec,
		Artifact:      artifact,
		Metrics:       metrics,
		MaxMetricAge:  maxAge,
		AllowedStepsW: steps,
	}, nil
}

// MetricValue includes enough detail to explain why Core accepted or rejected
// a live value.
type MetricValue struct {
	Driver    string    `json:"driver"`
	Metric    string    `json:"metric"`
	Value     float64   `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
	Valid     bool      `json:"valid"`
	Reason    string    `json:"reason,omitempty"`
}

type RuntimeSnapshot struct {
	At             time.Time              `json:"at"`
	Metrics        map[string]MetricValue `json:"metrics"`
	PlanningReady  bool                   `json:"planning_ready"`
	LiveCheckReady bool                   `json:"live_check_ready"`
	Reasons        []string               `json:"reasons,omitempty"`
}

func (s RuntimeSnapshot) metric(name string) (MetricValue, bool) {
	value, ok := s.Metrics[name]
	return value, ok && value.Valid
}

// Snapshot reads all configured thermal metrics and applies broad physical
// sensor bounds. Comfort bounds stay separate: a cold room is valid data and
// must not disappear just because it needs heat.
func (r *Runtime) Snapshot(now time.Time) RuntimeSnapshot {
	if now.IsZero() {
		now = time.Now()
	}
	out := RuntimeSnapshot{At: now, Metrics: make(map[string]MetricValue)}
	if r == nil || r.Metrics == nil {
		out.Reasons = []string{"thermal telemetry is unavailable"}
		return out
	}
	bounds := map[string][2]float64{
		"indoor_temperature":    {-10, 50},
		"outdoor_temperature":   {-60, 60},
		"heat_pump_power":       {0, r.Spec.Heating.MaximumElectricPowerW * thermalPowerHeadroom},
		"supply_temperature":    {-20, 120},
		"return_temperature":    {-20, 120},
		"hot_water_temperature": {0, 100},
		"solar_irradiance":      {0, 2_000},
	}
	for name, ref := range r.Spec.Sensors {
		metric := MetricValue{Driver: ref.Driver, Metric: ref.Metric}
		if health, ok := r.Metrics.(metricHealthReader); ok && !health.DriverHealthy(ref.Driver) {
			metric.Reason = "driver is offline"
			out.Metrics[name] = metric
			continue
		}
		value, timestamp, ok := r.Metrics.LatestMetric(ref.Driver, ref.Metric)
		metric.UpdatedAt = timestamp
		if !ok {
			metric.Reason = "missing"
		} else {
			metric.Value = ref.apply(value)
			limit := bounds[name]
			switch {
			case !finite(metric.Value):
				metric.Reason = "non-finite"
			case timestamp.After(now.Add(metricFutureGrace)):
				metric.Reason = "timestamp is in the future"
			case now.Sub(timestamp) > r.MaxMetricAge:
				metric.Reason = "stale"
			case metric.Value < limit[0] || metric.Value > limit[1]:
				metric.Reason = fmt.Sprintf("outside plausible range [%.1f, %.1f]", limit[0], limit[1])
			default:
				metric.Valid = true
			}
		}
		out.Metrics[name] = metric
	}
	_, indoorOK := out.metric("indoor_temperature")
	out.PlanningReady = indoorOK
	_, outdoorOK := out.metric("outdoor_temperature")
	_, powerOK := out.metric("heat_pump_power")
	out.LiveCheckReady = indoorOK && outdoorOK && powerOK
	if !indoorOK {
		out.Reasons = append(out.Reasons, metricReason(out.Metrics["indoor_temperature"], "indoor temperature"))
	}
	if !out.LiveCheckReady {
		if !outdoorOK {
			out.Reasons = append(out.Reasons, metricReason(out.Metrics["outdoor_temperature"], "outdoor temperature"))
		}
		if !powerOK {
			out.Reasons = append(out.Reasons, metricReason(out.Metrics["heat_pump_power"], "heat-pump power"))
		}
	}
	return out
}

func metricReason(value MetricValue, label string) string {
	if value.Reason == "" {
		return label + " is not configured"
	}
	return label + " is " + value.Reason
}

// ForecastSlot is the weather part of one MPC slot.
type ForecastSlot struct {
	Start        time.Time
	Duration     time.Duration
	OutdoorTempC *float64
}

// OptimizerLoads returns one load only when current indoor telemetry and the
// whole outdoor forecast pass validation. Callers keep their full-load
// fallback when this method returns an error.
func (r *Runtime) OptimizerLoads(now time.Time, slots []ForecastSlot) ([]OptimizerLoad, error) {
	if r == nil {
		return nil, errors.New("thermal runtime is nil")
	}
	if len(slots) == 0 {
		return nil, errors.New("thermal optimizer horizon is empty")
	}
	if r.Artifact.ModelType == ModelType2R2C {
		return nil, errors.New("2R2C planning requires a persistent mass-temperature observer")
	}
	snapshot := r.Snapshot(now)
	if !snapshot.PlanningReady {
		return nil, fmt.Errorf("thermal model is not ready: %s", strings.Join(snapshot.Reasons, "; "))
	}
	indoor, _ := snapshot.metric("indoor_temperature")
	initialTemperatureC := indoor.Value
	if slots[0].Start.IsZero() {
		return nil, errors.New("thermal forecast has no state-aligned start time")
	}
	if slots[0].Start.Before(now.Add(-metricFutureGrace)) {
		return nil, errors.New("thermal forecast starts before the live state timestamp")
	}
	if gap := slots[0].Start.Sub(now); gap > 0 {
		if gap > 2*time.Hour {
			return nil, errors.New("thermal forecast starts too far after the live state")
		}
		if !snapshot.LiveCheckReady {
			return nil, fmt.Errorf("thermal state cannot be advanced to the first full slot: %s", strings.Join(snapshot.Reasons, "; "))
		}
		outdoor, _ := snapshot.metric("outdoor_temperature")
		power, _ := snapshot.metric("heat_pump_power")
		bridge, err := r.Artifact.OptimizerLoad(OptimizerLoadInput{
			InitialTemperatureC:   initialTemperatureC,
			MinimumTemperatureC:   -10,
			MaximumTemperatureC:   50,
			OutsideTemperatureC:   []float64{outdoor.Value},
			MaximumElectricPowerW: r.Spec.Heating.MaximumElectricPowerW,
		})
		if err != nil {
			return nil, fmt.Errorf("prepare thermal state bridge: %w", err)
		}
		next, err := bridge.NextState(0, ModelState{AirC: initialTemperatureC, MassC: initialTemperatureC}, power.Value, gap.Hours())
		if err != nil {
			return nil, fmt.Errorf("advance thermal state to first full slot: %w", err)
		}
		initialTemperatureC = next.AirC
	}
	outside := make([]float64, len(slots))
	for index, slot := range slots {
		if slot.Start.IsZero() || slot.Duration <= 0 {
			return nil, fmt.Errorf("thermal forecast slot %d has invalid timing", index)
		}
		if index > 0 {
			previousEnd := slots[index-1].Start.Add(slots[index-1].Duration)
			if delta := slot.Start.Sub(previousEnd); delta < -time.Second || delta > time.Second {
				return nil, fmt.Errorf("thermal forecast is not contiguous at slot %d", index)
			}
		}
		if slot.OutdoorTempC == nil || !finite(*slot.OutdoorTempC) || *slot.OutdoorTempC < -60 || *slot.OutdoorTempC > 60 {
			return nil, fmt.Errorf("thermal outdoor forecast is missing or implausible at slot %d", index)
		}
		outside[index] = *slot.OutdoorTempC
	}
	zone := r.Spec.PrimaryZone()
	load, err := r.Artifact.OptimizerLoad(OptimizerLoadInput{
		InitialTemperatureC:   initialTemperatureC,
		MinimumTemperatureC:   zone.Comfort.MinimumTemperatureC,
		MaximumTemperatureC:   zone.Comfort.MaximumTemperatureC,
		OutsideTemperatureC:   outside,
		MaximumElectricPowerW: r.Spec.Heating.MaximumElectricPowerW,
		AllowedStepsW:         r.AllowedStepsW,
	})
	if err != nil {
		return nil, err
	}
	return []OptimizerLoad{load}, nil
}

// TransitionObservation is an aligned interval used for a live model check.
// PowerW and OutdoorC must be interval means, not the latest point sample.
type TransitionObservation struct {
	InitialAirC  float64
	InitialMassC *float64
	ObservedAirC float64
	OutdoorC     float64
	PowerW       float64
	Duration     time.Duration
}

type TransitionAssessment struct {
	Reasonable     bool    `json:"reasonable"`
	PredictedAirC  float64 `json:"predicted_air_c"`
	PredictedMassC float64 `json:"predicted_mass_c,omitempty"`
	ObservedAirC   float64 `json:"observed_air_c"`
	AbsoluteErrorC float64 `json:"absolute_error_c"`
	AllowedErrorC  float64 `json:"allowed_error_c"`
	Reason         string  `json:"reason,omitempty"`
}

// AssessTransition compares an aligned live interval with the artifact. Four
// calibration RMSEs form the evidence band, with a 1 °C floor for sensor
// quantisation and gains the small model does not represent.
func (r *Runtime) AssessTransition(observation TransitionObservation) TransitionAssessment {
	assessment := TransitionAssessment{ObservedAirC: observation.ObservedAirC}
	if r == nil || !finite(observation.InitialAirC) || !finite(observation.ObservedAirC) || !finite(observation.OutdoorC) || !finite(observation.PowerW) || observation.PowerW < 0 || observation.Duration <= 0 {
		assessment.Reason = "transition input is invalid"
		return assessment
	}
	initialMass := observation.InitialAirC
	if observation.InitialMassC != nil {
		initialMass = *observation.InitialMassC
	}
	load, err := r.Artifact.OptimizerLoad(OptimizerLoadInput{
		InitialTemperatureC:     observation.InitialAirC,
		InitialMassTemperatureC: &initialMass,
		MinimumTemperatureC:     -10,
		MaximumTemperatureC:     50,
		OutsideTemperatureC:     []float64{observation.OutdoorC},
		MaximumElectricPowerW:   r.Spec.Heating.MaximumElectricPowerW,
	})
	if err != nil {
		assessment.Reason = err.Error()
		return assessment
	}
	next, err := load.NextState(0, ModelState{AirC: observation.InitialAirC, MassC: initialMass}, observation.PowerW, observation.Duration.Hours())
	if err != nil {
		assessment.Reason = err.Error()
		return assessment
	}
	assessment.PredictedAirC = next.AirC
	assessment.PredictedMassC = next.MassC
	assessment.AbsoluteErrorC = math.Abs(observation.ObservedAirC - next.AirC)
	assessment.AllowedErrorC = math.Max(1, 4*r.Artifact.Calibration.OneStepRMSEC)
	assessment.Reasonable = assessment.AbsoluteErrorC <= assessment.AllowedErrorC
	if !assessment.Reasonable {
		assessment.Reason = "observed indoor temperature is outside the calibrated prediction band"
	}
	return assessment
}
