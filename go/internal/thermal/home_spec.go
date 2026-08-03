package thermal

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

const (
	HomeSpecKind          = "ftw.home_thermal_spec"
	HomeSpecSchemaVersion = 2
)

// SeriesRef points at one scalar metric in FTW's telemetry store. Scale and
// offset let the site spec correct a vendor unit without teaching Core that
// vendor's naming scheme.
type SeriesRef struct {
	Driver string  `json:"driver"`
	Metric string  `json:"metric"`
	Scale  float64 `json:"scale,omitempty"`
	Offset float64 `json:"offset,omitempty"`
}

func (r *SeriesRef) UnmarshalJSON(data []byte) error {
	type wire struct {
		Driver string   `json:"driver"`
		Metric string   `json:"metric"`
		Scale  *float64 `json:"scale"`
		Offset *float64 `json:"offset"`
	}
	var value wire
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	r.Driver = strings.TrimSpace(value.Driver)
	r.Metric = strings.TrimSpace(value.Metric)
	r.Scale = 1
	if value.Scale != nil {
		r.Scale = *value.Scale
	}
	if value.Offset != nil {
		r.Offset = *value.Offset
	}
	return nil
}

func (r SeriesRef) validate(name string) error {
	if r.Driver == "" || r.Metric == "" {
		return fmt.Errorf("thermal sensor %s needs driver and metric", name)
	}
	if !finite(r.Scale) || r.Scale == 0 || !finite(r.Offset) {
		return fmt.Errorf("thermal sensor %s has an invalid scale or offset", name)
	}
	return nil
}

func (r SeriesRef) apply(value float64) float64 { return value*r.Scale + r.Offset }

type HomeZone struct {
	ID          string   `json:"id"`
	FloorAreaM2 *float64 `json:"floor_area_m2,omitempty"`
	VolumeM3    *float64 `json:"volume_m3,omitempty"`
	Comfort     struct {
		MinimumTemperatureC float64 `json:"minimum_temperature_c"`
		MaximumTemperatureC float64 `json:"maximum_temperature_c"`
	} `json:"comfort"`
}

func (z *HomeZone) UnmarshalJSON(data []byte) error {
	type comfortWire struct {
		Minimum *float64 `json:"minimum_temperature_c"`
		Maximum *float64 `json:"maximum_temperature_c"`
	}
	type zoneWire struct {
		ID          string       `json:"id"`
		FloorAreaM2 *float64     `json:"floor_area_m2"`
		VolumeM3    *float64     `json:"volume_m3"`
		Comfort     *comfortWire `json:"comfort"`
	}
	var value zoneWire
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	z.ID, z.FloorAreaM2, z.VolumeM3 = value.ID, value.FloorAreaM2, value.VolumeM3
	z.Comfort.MinimumTemperatureC = 19
	z.Comfort.MaximumTemperatureC = 23
	if value.Comfort != nil {
		if value.Comfort.Minimum != nil {
			z.Comfort.MinimumTemperatureC = *value.Comfort.Minimum
		}
		if value.Comfort.Maximum != nil {
			z.Comfort.MaximumTemperatureC = *value.Comfort.Maximum
		}
	}
	return nil
}

type HomeHeating struct {
	Source                string   `json:"source"`
	Emitters              string   `json:"emitters"`
	MaximumElectricPowerW float64  `json:"maximum_electric_power_w"`
	BufferTankL           *float64 `json:"buffer_tank_l,omitempty"`
	HotWaterTankL         *float64 `json:"hot_water_tank_l,omitempty"`
	COPCurve              COPCurve `json:"cop_curve"`
}

type ParameterRange struct {
	Minimum float64
	Maximum float64
}

func (r *ParameterRange) UnmarshalJSON(data []byte) error {
	var values []float64
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	if len(values) != 2 {
		return errors.New("thermal parameter range needs two values")
	}
	r.Minimum, r.Maximum = values[0], values[1]
	return nil
}

func (r ParameterRange) validate(name string) error {
	if !finite(r.Minimum) || !finite(r.Maximum) || r.Minimum >= r.Maximum {
		return fmt.Errorf("home thermal prior %s must be finite and increasing", name)
	}
	return nil
}

type HomePriors struct {
	HeatLossWPerK       ParameterRange `json:"heat_loss_w_per_k"`
	TotalCapacityWhPerK ParameterRange `json:"total_capacity_wh_per_k"`
	MassCouplingWPerK   ParameterRange `json:"mass_coupling_w_per_k"`
	AirCapacityFraction ParameterRange `json:"air_capacity_fraction"`
	DisturbanceHeatW    ParameterRange `json:"disturbance_heat_w"`
}

func defaultHomePriors() HomePriors {
	return HomePriors{
		HeatLossWPerK:       ParameterRange{5, 5_000},
		TotalCapacityWhPerK: ParameterRange{200, 1_000_000},
		MassCouplingWPerK:   ParameterRange{5, 10_000},
		AirCapacityFraction: ParameterRange{0.01, 0.6},
		DisturbanceHeatW:    ParameterRange{-20_000, 20_000},
	}
}

func (p *HomePriors) UnmarshalJSON(data []byte) error {
	defaults := defaultHomePriors()
	if string(data) == "null" {
		*p = defaults
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*p = defaults
	targets := map[string]*ParameterRange{
		"heat_loss_w_per_k":       &p.HeatLossWPerK,
		"total_capacity_wh_per_k": &p.TotalCapacityWhPerK,
		"mass_coupling_w_per_k":   &p.MassCouplingWPerK,
		"air_capacity_fraction":   &p.AirCapacityFraction,
		"disturbance_heat_w":      &p.DisturbanceHeatW,
	}
	for name, target := range targets {
		if value, ok := raw[name]; ok {
			if err := json.Unmarshal(value, target); err != nil {
				return fmt.Errorf("decode thermal prior %s: %w", name, err)
			}
		}
	}
	return nil
}

type HomeModelSelection struct {
	Candidates                 []string `json:"candidates"`
	TrainFraction              float64  `json:"train_fraction"`
	MinimumRolloutImprovementC float64  `json:"minimum_rollout_improvement_c"`
	MinimumRelativeImprovement float64  `json:"minimum_relative_improvement"`
}

func defaultHomeModelSelection() HomeModelSelection {
	return HomeModelSelection{
		Candidates:                 []string{ModelType1R1C, ModelType2R2C},
		TrainFraction:              0.75,
		MinimumRolloutImprovementC: 0.05,
		MinimumRelativeImprovement: 0.10,
	}
}

func (m *HomeModelSelection) UnmarshalJSON(data []byte) error {
	type wire struct {
		Candidates                 *[]string `json:"candidates"`
		TrainFraction              *float64  `json:"train_fraction"`
		MinimumRolloutImprovementC *float64  `json:"minimum_rollout_improvement_c"`
		MinimumRelativeImprovement *float64  `json:"minimum_relative_improvement"`
	}
	defaults := defaultHomeModelSelection()
	var value wire
	if string(data) != "null" {
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
	}
	*m = defaults
	if value.Candidates != nil {
		m.Candidates = append([]string(nil), (*value.Candidates)...)
	}
	if value.TrainFraction != nil {
		m.TrainFraction = *value.TrainFraction
	}
	if value.MinimumRolloutImprovementC != nil {
		m.MinimumRolloutImprovementC = *value.MinimumRolloutImprovementC
	}
	if value.MinimumRelativeImprovement != nil {
		m.MinimumRelativeImprovement = *value.MinimumRelativeImprovement
	}
	return nil
}

// HomeSpec is the versioned semantic input shared with Python. Core does not
// use calibration priors or model-selection settings at run time, but it reads,
// validates, and fingerprints them so an artifact cannot bind to a changed
// home description.
type HomeSpec struct {
	SchemaVersion  int                  `json:"schema_version"`
	Kind           string               `json:"kind"`
	Revision       string               `json:"revision,omitempty"`
	SiteID         string               `json:"site_id"`
	PrimaryZoneID  string               `json:"primary_zone_id"`
	Zones          []HomeZone           `json:"zones"`
	Heating        HomeHeating          `json:"heating"`
	Sensors        map[string]SeriesRef `json:"sensors"`
	Priors         HomePriors           `json:"priors"`
	ModelSelection HomeModelSelection   `json:"model_selection"`
}

// ParseHomeSpec validates the part of HomeSpec that Core consumes. Python
// remains the owner of calibration priors and model-family selection.
func ParseHomeSpec(data []byte) (*HomeSpec, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("decode home thermal spec: %w", err)
	}
	var spec HomeSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("decode home thermal spec: %w", err)
	}
	// Match Python's HomeSpec parser before computing the shared revision.
	spec.SiteID = strings.TrimSpace(spec.SiteID)
	spec.PrimaryZoneID = strings.TrimSpace(spec.PrimaryZoneID)
	spec.Heating.Source = strings.TrimSpace(spec.Heating.Source)
	spec.Heating.Emitters = strings.TrimSpace(spec.Heating.Emitters)
	for index := range spec.Zones {
		spec.Zones[index].ID = strings.TrimSpace(spec.Zones[index].ID)
	}
	if _, ok := fields["priors"]; !ok {
		spec.Priors = defaultHomePriors()
	}
	if _, ok := fields["model_selection"]; !ok {
		spec.ModelSelection = defaultHomeModelSelection()
	}
	revision := spec.contentRevision()
	if spec.Revision != "" && spec.Revision != revision {
		return nil, errors.New("home thermal spec revision does not match its contents")
	}
	spec.Revision = revision
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return &spec, nil
}

func (s HomeSpec) Validate() error {
	if s.SchemaVersion != HomeSpecSchemaVersion {
		return fmt.Errorf("home thermal spec schema_version is %d, want %d", s.SchemaVersion, HomeSpecSchemaVersion)
	}
	if s.Kind != HomeSpecKind {
		return fmt.Errorf("home thermal spec kind is %q, want %q", s.Kind, HomeSpecKind)
	}
	if strings.TrimSpace(s.SiteID) == "" || strings.TrimSpace(s.PrimaryZoneID) == "" {
		return errors.New("home thermal spec needs site_id and primary_zone_id")
	}
	seen := make(map[string]bool, len(s.Zones))
	primaryFound := false
	for _, zone := range s.Zones {
		if strings.TrimSpace(zone.ID) == "" || seen[zone.ID] {
			return errors.New("home thermal zones need unique non-empty ids")
		}
		seen[zone.ID] = true
		for name, value := range map[string]*float64{"floor area": zone.FloorAreaM2, "volume": zone.VolumeM3} {
			if value != nil && (!finite(*value) || *value <= 0) {
				return fmt.Errorf("home thermal zone %s has invalid %s", zone.ID, name)
			}
		}
		if !finite(zone.Comfort.MinimumTemperatureC) || !finite(zone.Comfort.MaximumTemperatureC) || zone.Comfort.MinimumTemperatureC >= zone.Comfort.MaximumTemperatureC {
			return fmt.Errorf("home thermal zone %s has invalid comfort bounds", zone.ID)
		}
		if zone.ID == s.PrimaryZoneID {
			primaryFound = true
		}
	}
	if !primaryFound {
		return errors.New("home thermal primary_zone_id does not name a zone")
	}
	if strings.TrimSpace(s.Heating.Source) == "" || strings.TrimSpace(s.Heating.Emitters) == "" {
		return errors.New("home thermal heating needs source and emitters")
	}
	if !finite(s.Heating.MaximumElectricPowerW) || s.Heating.MaximumElectricPowerW <= 0 {
		return errors.New("home thermal maximum electric power must be positive")
	}
	if err := s.Heating.COPCurve.validate(); err != nil {
		return fmt.Errorf("home thermal spec: %w", err)
	}
	for name, value := range map[string]*float64{"buffer tank": s.Heating.BufferTankL, "hot-water tank": s.Heating.HotWaterTankL} {
		if value != nil && (!finite(*value) || *value <= 0) {
			return fmt.Errorf("home thermal %s must be positive", name)
		}
	}
	for name, value := range map[string]ParameterRange{
		"heat_loss_w_per_k":       s.Priors.HeatLossWPerK,
		"total_capacity_wh_per_k": s.Priors.TotalCapacityWhPerK,
		"mass_coupling_w_per_k":   s.Priors.MassCouplingWPerK,
		"air_capacity_fraction":   s.Priors.AirCapacityFraction,
		"disturbance_heat_w":      s.Priors.DisturbanceHeatW,
	} {
		if err := value.validate(name); err != nil {
			return err
		}
	}
	if s.Priors.AirCapacityFraction.Minimum <= 0 || s.Priors.AirCapacityFraction.Maximum >= 1 {
		return errors.New("home thermal air-capacity fraction must lie within (0, 1)")
	}
	if len(s.ModelSelection.Candidates) == 0 {
		return errors.New("home thermal model candidates must not be empty")
	}
	candidates := make(map[string]bool, len(s.ModelSelection.Candidates))
	for _, candidate := range s.ModelSelection.Candidates {
		if candidate != ModelType1R1C && candidate != ModelType2R2C {
			return fmt.Errorf("unsupported home thermal model candidate %q", candidate)
		}
		if candidates[candidate] {
			return errors.New("home thermal model candidates must be unique")
		}
		candidates[candidate] = true
	}
	if !finite(s.ModelSelection.TrainFraction) || s.ModelSelection.TrainFraction < 0.5 || s.ModelSelection.TrainFraction > 0.9 ||
		!finite(s.ModelSelection.MinimumRolloutImprovementC) || s.ModelSelection.MinimumRolloutImprovementC < 0 ||
		!finite(s.ModelSelection.MinimumRelativeImprovement) || s.ModelSelection.MinimumRelativeImprovement < 0 || s.ModelSelection.MinimumRelativeImprovement >= 1 {
		return errors.New("home thermal model-selection settings are invalid")
	}
	for _, name := range []string{"indoor_temperature", "outdoor_temperature"} {
		ref, ok := s.Sensors[name]
		if !ok {
			return fmt.Errorf("home thermal sensor %s is required", name)
		}
		if err := ref.validate(name); err != nil {
			return err
		}
	}
	for name, ref := range s.Sensors {
		switch name {
		case "indoor_temperature", "outdoor_temperature", "heat_pump_power", "supply_temperature", "return_temperature", "hot_water_temperature", "solar_irradiance":
		default:
			return fmt.Errorf("unsupported home thermal sensor %q", name)
		}
		if err := ref.validate(name); err != nil {
			return err
		}
	}
	if s.Revision != s.contentRevision() {
		return errors.New("home thermal spec revision does not match its contents")
	}
	return nil
}

func (s HomeSpec) contentRevision() string {
	w := newFingerprint("ftw.home_thermal_spec.v2")
	w.String(s.SiteID)
	w.String(s.PrimaryZoneID)
	w.Int(len(s.Zones))
	for _, zone := range s.Zones {
		w.String(zone.ID)
		w.OptionalFloat(zone.FloorAreaM2)
		w.OptionalFloat(zone.VolumeM3)
		w.Float(zone.Comfort.MinimumTemperatureC)
		w.Float(zone.Comfort.MaximumTemperatureC)
	}
	w.String(s.Heating.Source)
	w.String(s.Heating.Emitters)
	w.Float(s.Heating.MaximumElectricPowerW)
	w.OptionalFloat(s.Heating.BufferTankL)
	w.OptionalFloat(s.Heating.HotWaterTankL)
	w.Float(s.Heating.COPCurve.ReferenceTemperatureC)
	w.Float(s.Heating.COPCurve.COPAtReference)
	w.Float(s.Heating.COPCurve.SlopePerC)
	w.Float(s.Heating.COPCurve.MinimumCOP)
	w.Float(s.Heating.COPCurve.MaximumCOP)
	sensorNames := make([]string, 0, len(s.Sensors))
	for name := range s.Sensors {
		sensorNames = append(sensorNames, name)
	}
	sort.Strings(sensorNames)
	w.Int(len(sensorNames))
	for _, name := range sensorNames {
		ref := s.Sensors[name]
		w.String(name)
		w.String(ref.Driver)
		w.String(ref.Metric)
		w.Float(ref.Scale)
		w.Float(ref.Offset)
	}
	for _, value := range []ParameterRange{
		s.Priors.HeatLossWPerK,
		s.Priors.TotalCapacityWhPerK,
		s.Priors.MassCouplingWPerK,
		s.Priors.AirCapacityFraction,
		s.Priors.DisturbanceHeatW,
	} {
		w.Float(value.Minimum)
		w.Float(value.Maximum)
	}
	w.StringList(s.ModelSelection.Candidates)
	w.Float(s.ModelSelection.TrainFraction)
	w.Float(s.ModelSelection.MinimumRolloutImprovementC)
	w.Float(s.ModelSelection.MinimumRelativeImprovement)
	return w.Sum()
}

func (s HomeSpec) PrimaryZone() HomeZone {
	for _, zone := range s.Zones {
		if zone.ID == s.PrimaryZoneID {
			return zone
		}
	}
	return HomeZone{}
}

func copCurvesEqual(a, b COPCurve) bool {
	const tolerance = 1e-9
	values := [][2]float64{
		{a.ReferenceTemperatureC, b.ReferenceTemperatureC},
		{a.COPAtReference, b.COPAtReference},
		{a.SlopePerC, b.SlopePerC},
		{a.MinimumCOP, b.MinimumCOP},
		{a.MaximumCOP, b.MaximumCOP},
	}
	for _, pair := range values {
		if math.Abs(pair[0]-pair[1]) > tolerance {
			return false
		}
	}
	return true
}
