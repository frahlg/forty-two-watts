package thermal

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// OptimizerLoad is the typed Go form of one thermal_loads entry in the
// external optimizer protocol.
type OptimizerLoad struct {
	ID                    string    `json:"id"`
	ModelType             string    `json:"model_type"`
	SourceRevision        string    `json:"source_revision"`
	InitialTempC          float64   `json:"initial_temp_c"`
	InitialMassTempC      *float64  `json:"initial_mass_temp_c,omitempty"`
	MinTempC              float64   `json:"min_temp_c"`
	MaxTempC              float64   `json:"max_temp_c"`
	OutsideTempC          []float64 `json:"outside_temp_c"`
	MaxPowerW             float64   `json:"max_power_w"`
	AllowedStepsW         []float64 `json:"allowed_steps_w,omitempty"`
	HeatLossWPerK         float64   `json:"heat_loss_w_per_k"`
	ThermalCapacityWhPerK *float64  `json:"thermal_capacity_wh_per_k,omitempty"`
	MassCouplingWPerK     *float64  `json:"mass_coupling_w_per_k,omitempty"`
	AirCapacityWhPerK     *float64  `json:"air_capacity_wh_per_k,omitempty"`
	MassCapacityWhPerK    *float64  `json:"mass_capacity_wh_per_k,omitempty"`
	COP                   []float64 `json:"cop"`
	DisturbanceHeatW      []float64 `json:"disturbance_heat_w"`
}

type OptimizerLoadInput struct {
	InitialTemperatureC     float64
	InitialMassTemperatureC *float64
	MinimumTemperatureC     float64
	MaximumTemperatureC     float64
	OutsideTemperatureC     []float64
	MaximumElectricPowerW   float64
	AllowedStepsW           []float64
}

// OptimizerLoad builds a promoted, typed protocol record from the artifact.
func (a Artifact) OptimizerLoad(input OptimizerLoadInput) (OptimizerLoad, error) {
	if err := a.Validate(); err != nil {
		return OptimizerLoad{}, err
	}
	if err := a.Promotable(); err != nil {
		return OptimizerLoad{}, err
	}
	if !finite(input.InitialTemperatureC) {
		return OptimizerLoad{}, errors.New("thermal initial temperature must be finite")
	}
	if !finite(input.MinimumTemperatureC) || !finite(input.MaximumTemperatureC) || input.MinimumTemperatureC >= input.MaximumTemperatureC {
		return OptimizerLoad{}, errors.New("thermal comfort bounds must be finite and increasing")
	}
	if !finite(input.MaximumElectricPowerW) || input.MaximumElectricPowerW <= 0 {
		return OptimizerLoad{}, errors.New("thermal maximum electric power must be positive")
	}
	if len(input.OutsideTemperatureC) == 0 {
		return OptimizerLoad{}, errors.New("thermal outdoor forecast is empty")
	}
	outside := append([]float64(nil), input.OutsideTemperatureC...)
	cop := make([]float64, len(outside))
	disturbance := make([]float64, len(outside))
	for index, value := range outside {
		if !finite(value) {
			return OptimizerLoad{}, fmt.Errorf("thermal outdoor forecast %d is not finite", index)
		}
		if value < a.Calibration.ObservedOutdoorMinC || value > a.Calibration.ObservedOutdoorMaxC {
			return OptimizerLoad{}, fmt.Errorf("thermal outdoor forecast %d exceeds the calibrated operating range", index)
		}
		cop[index] = a.Physics.COPCurve.At(value)
		disturbance[index] = a.Residual.ConstantHeatW
	}
	steps, err := normalizePowerSteps(input.AllowedStepsW, input.MaximumElectricPowerW)
	if err != nil {
		return OptimizerLoad{}, err
	}
	load := OptimizerLoad{
		ID:                    a.ModelID,
		ModelType:             a.ModelType,
		SourceRevision:        a.Revision,
		InitialTempC:          input.InitialTemperatureC,
		MinTempC:              input.MinimumTemperatureC,
		MaxTempC:              input.MaximumTemperatureC,
		OutsideTempC:          outside,
		MaxPowerW:             input.MaximumElectricPowerW,
		AllowedStepsW:         steps,
		HeatLossWPerK:         a.Physics.HeatLossWPerK,
		ThermalCapacityWhPerK: cloneFloat(a.Physics.ThermalCapacityWhPerK),
		MassCouplingWPerK:     cloneFloat(a.Physics.MassCouplingWPerK),
		AirCapacityWhPerK:     cloneFloat(a.Physics.AirCapacityWhPerK),
		MassCapacityWhPerK:    cloneFloat(a.Physics.MassCapacityWhPerK),
		COP:                   cop,
		DisturbanceHeatW:      disturbance,
	}
	if a.ModelType == ModelType2R2C {
		initialMass := input.InitialTemperatureC
		if input.InitialMassTemperatureC != nil {
			initialMass = *input.InitialMassTemperatureC
		}
		if !finite(initialMass) {
			return OptimizerLoad{}, errors.New("thermal initial mass temperature must be finite")
		}
		load.InitialMassTempC = &initialMass
	}
	if err := load.Validate(len(outside)); err != nil {
		return OptimizerLoad{}, err
	}
	return load, nil
}

func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func normalizePowerSteps(raw []float64, maximumW float64) ([]float64, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := make(map[float64]bool, len(raw))
	steps := make([]float64, 0, len(raw))
	for index, value := range raw {
		if !finite(value) || value < 0 || value > maximumW {
			return nil, fmt.Errorf("thermal allowed power step %d must be within [0, max_power_w]", index)
		}
		if !seen[value] {
			seen[value] = true
			steps = append(steps, value)
		}
	}
	sort.Float64s(steps)
	if len(steps) == 0 || steps[0] != 0 {
		return nil, errors.New("thermal allowed power steps must contain 0")
	}
	return steps, nil
}

// Validate checks one load against the optimizer horizon.
func (l OptimizerLoad) Validate(horizon int) error {
	if strings.TrimSpace(l.ID) == "" {
		return errors.New("thermal optimizer load id is empty")
	}
	if l.ModelType != ModelType1R1C && l.ModelType != ModelType2R2C {
		return fmt.Errorf("thermal optimizer load %s has unsupported model type %q", l.ID, l.ModelType)
	}
	if len(l.SourceRevision) != 64 {
		return fmt.Errorf("thermal optimizer load %s has an invalid source revision", l.ID)
	}
	if _, err := hex.DecodeString(l.SourceRevision); err != nil {
		return fmt.Errorf("thermal optimizer load %s has a non-hex source revision", l.ID)
	}
	if !finite(l.InitialTempC) || !finite(l.MinTempC) || !finite(l.MaxTempC) || l.MinTempC >= l.MaxTempC {
		return fmt.Errorf("thermal optimizer load %s has invalid temperatures", l.ID)
	}
	if !finite(l.MaxPowerW) || l.MaxPowerW <= 0 || !finite(l.HeatLossWPerK) || l.HeatLossWPerK <= 0 {
		return fmt.Errorf("thermal optimizer load %s has invalid power or heat loss", l.ID)
	}
	if len(l.OutsideTempC) != horizon || len(l.COP) != horizon || len(l.DisturbanceHeatW) != horizon {
		return fmt.Errorf("thermal optimizer load %s vectors do not match horizon %d", l.ID, horizon)
	}
	for index := 0; index < horizon; index++ {
		if !finite(l.OutsideTempC[index]) || !finite(l.COP[index]) || l.COP[index] <= 0 || !finite(l.DisturbanceHeatW[index]) {
			return fmt.Errorf("thermal optimizer load %s has invalid vector value at slot %d", l.ID, index)
		}
	}
	if _, err := normalizePowerSteps(l.AllowedStepsW, l.MaxPowerW); err != nil {
		return fmt.Errorf("thermal optimizer load %s: %w", l.ID, err)
	}
	positive := func(value *float64) bool { return value != nil && finite(*value) && *value > 0 }
	switch l.ModelType {
	case ModelType1R1C:
		if !positive(l.ThermalCapacityWhPerK) || l.InitialMassTempC != nil ||
			l.MassCouplingWPerK != nil || l.AirCapacityWhPerK != nil || l.MassCapacityWhPerK != nil {
			return fmt.Errorf("thermal optimizer load %s has invalid 1R1C parameters", l.ID)
		}
	case ModelType2R2C:
		if l.ThermalCapacityWhPerK != nil || !positive(l.MassCouplingWPerK) || !positive(l.AirCapacityWhPerK) || !positive(l.MassCapacityWhPerK) || l.InitialMassTempC == nil || !finite(*l.InitialMassTempC) {
			return fmt.Errorf("thermal optimizer load %s has invalid 2R2C parameters", l.ID)
		}
	}
	return nil
}

// ModelState is the state needed to replay either supported thermal model.
// MassC is ignored by 1R1C.
type ModelState struct {
	AirC  float64
	MassC float64
}

// NextState replays one exact optimizer transition. Go uses this to distrust
// solver output in the same way it replays battery and EV energy.
func (l OptimizerLoad) NextState(slot int, state ModelState, powerW, durationH float64) (ModelState, error) {
	if slot < 0 || slot >= len(l.OutsideTempC) {
		return ModelState{}, errors.New("thermal transition slot is out of range")
	}
	if !finite(state.AirC) || !finite(state.MassC) || !finite(powerW) || powerW < 0 || powerW > l.MaxPowerW+2 || !finite(durationH) || durationH <= 0 {
		return ModelState{}, errors.New("thermal transition input is invalid")
	}
	if len(l.AllowedStepsW) > 0 {
		allowed := false
		for _, step := range l.AllowedStepsW {
			if math.Abs(powerW-step) <= 2 {
				allowed = true
				break
			}
		}
		if !allowed {
			return ModelState{}, fmt.Errorf("thermal power %.3f is not an allowed step", powerW)
		}
	}
	outdoor := l.OutsideTempC[slot]
	heatW := l.COP[slot]*powerW + l.DisturbanceHeatW[slot]
	equilibriumC := outdoor + heatW/l.HeatLossWPerK
	if l.ModelType == ModelType1R1C {
		if l.ThermalCapacityWhPerK == nil || !finite(*l.ThermalCapacityWhPerK) || *l.ThermalCapacityWhPerK <= 0 {
			return ModelState{}, errors.New("thermal 1R1C capacity is invalid")
		}
		decay := math.Exp(-l.HeatLossWPerK * durationH / *l.ThermalCapacityWhPerK)
		next := equilibriumC + decay*(state.AirC-equilibriumC)
		if !finite(next) {
			return ModelState{}, errors.New("thermal 1R1C transition produced a non-finite state")
		}
		return ModelState{AirC: next, MassC: next}, nil
	}

	if l.MassCouplingWPerK == nil || l.AirCapacityWhPerK == nil || l.MassCapacityWhPerK == nil {
		return ModelState{}, errors.New("thermal 2R2C parameters are missing")
	}
	h := l.HeatLossWPerK
	hm := *l.MassCouplingWPerK
	ca := *l.AirCapacityWhPerK
	cm := *l.MassCapacityWhPerK
	a11 := -(h + hm) / ca
	a12 := hm / ca
	a21 := hm / cm
	a22 := -hm / cm
	halfTrace := (a11 + a22) / 2
	delta := math.Sqrt(math.Pow((a11-a22)/2, 2) + a12*a21)
	meanExp := math.Exp(halfTrace * durationH)
	factor := meanExp * durationH
	if delta > 1e-12 {
		fast := math.Exp((halfTrace + delta) * durationH)
		slow := math.Exp((halfTrace - delta) * durationH)
		meanExp = (fast + slow) / 2
		factor = (fast - slow) / (2 * delta)
	}
	phi11 := meanExp + factor*(a11-halfTrace)
	phi12 := factor * a12
	phi21 := factor * a21
	phi22 := meanExp + factor*(a22-halfTrace)
	airDelta := state.AirC - equilibriumC
	massDelta := state.MassC - equilibriumC
	next := ModelState{
		AirC:  equilibriumC + phi11*airDelta + phi12*massDelta,
		MassC: equilibriumC + phi21*airDelta + phi22*massDelta,
	}
	if !finite(next.AirC) || !finite(next.MassC) {
		return ModelState{}, errors.New("thermal 2R2C transition produced a non-finite state")
	}
	return next, nil
}
