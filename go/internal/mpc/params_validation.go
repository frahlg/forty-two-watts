package mpc

import (
	"fmt"
	"math"
)

func validatePlanningSlots(slots []Slot) error {
	if len(slots) == 0 {
		return fmt.Errorf("slots must be non-empty")
	}
	for i, slot := range slots {
		field := fmt.Sprintf("slots[%d]", i)
		for _, value := range []struct {
			name string
			v    float64
		}{
			{"price_ore", slot.PriceOre},
			{"spot_ore", slot.SpotOre},
			{"pv_w", slot.PVW},
			{"load_w", slot.LoadW},
			{"confidence", slot.Confidence},
			{"max_import_w", slot.Limits.MaxImportW},
			{"max_export_w", slot.Limits.MaxExportW},
		} {
			if !finite(value.v) {
				return fmt.Errorf("%s.%s must be finite", field, value.name)
			}
		}
		if slot.PVW > 0 {
			return fmt.Errorf("%s.pv_w must be non-positive in the site sign convention", field)
		}
		if slot.LoadW < 0 {
			return fmt.Errorf("%s.load_w must be non-negative in the site sign convention", field)
		}
		if slot.Confidence <= 0 || slot.Confidence > 1 {
			return fmt.Errorf("%s.confidence must be within (0, 1]", field)
		}
		if slot.Limits.MaxImportW < 0 || slot.Limits.MaxExportW < 0 {
			return fmt.Errorf("%s grid limits must be non-negative", field)
		}
	}
	return nil
}

// validateServiceGridLimits checks the raw site limits before slot clamping.
// The clamp treats non-positive values as unset and compares export limits to
// the fuse, so invalid values must not be allowed to disappear in that logic.
func validateServiceGridLimits(fuseMaxW, maxExportW float64) error {
	if err := requireNonNegativePlanningValue("fuse_max_w", fuseMaxW); err != nil {
		return err
	}
	return requireNonNegativePlanningValue("max_export_w", maxExportW)
}

func validateBatteryFleetMembers(fleet []BatteryFleetMember) error {
	seen := make(map[string]struct{}, len(fleet))
	for i, battery := range fleet {
		field := fmt.Sprintf("battery_fleet[%d]", i)
		if battery.Driver == "" {
			return fmt.Errorf("%s.driver must be non-empty", field)
		}
		if _, exists := seen[battery.Driver]; exists {
			return fmt.Errorf("%s.driver %q is duplicated", field, battery.Driver)
		}
		seen[battery.Driver] = struct{}{}
		if err := requirePositivePlanningValue(field+".capacity_wh", battery.CapacityWh); err != nil {
			return err
		}
		if err := requireNonNegativePlanningValue(field+".max_charge_w", battery.MaxChargeW); err != nil {
			return err
		}
		if err := requireNonNegativePlanningValue(field+".max_discharge_w", battery.MaxDischargeW); err != nil {
			return err
		}
	}
	return nil
}

// planningParamsRequireRecovery marks states the external model can replay but
// the discrete Go DP cannot yet represent. The DP snaps its initial SoC onto
// the operating grid, so using it here would plan from energy the site does not
// have (or discard energy it does have).
func planningParamsRequireRecovery(p Params) bool {
	if p.InitialSoCPct < p.SoCMinPct || p.InitialSoCPct > p.SoCMaxPct {
		return true
	}
	for _, storage := range p.Storages {
		if storage.InitialEnergyWh < storage.MinEnergyWh || storage.InitialEnergyWh > storage.MaxEnergyWh {
			return true
		}
	}
	return false
}

// validatePlanningParams checks the effective inputs that both planning
// engines receive. Service calls it after live battery and loadpoint state has
// been applied, so an invalid value cannot reach either the external optimizer
// or the Go fallback with different defaulting or failure semantics.
func validatePlanningParams(p Params) error {
	switch p.Mode {
	case ModeSelfConsumption, ModeCheapCharge, ModePassiveArbitrage, ModeArbitrage:
	default:
		return fmt.Errorf("unsupported mode %q", p.Mode)
	}

	if p.SoCLevels < 3 {
		return fmt.Errorf("soc_levels must be at least 3, got %d", p.SoCLevels)
	}
	if p.ActionLevels < 3 {
		return fmt.Errorf("action_levels must be at least 3, got %d", p.ActionLevels)
	}
	if err := requirePositivePlanningValue("capacity_wh", p.CapacityWh); err != nil {
		return err
	}
	if !finite(p.SoCMinPct) || !finite(p.SoCMaxPct) ||
		p.SoCMinPct < 0 || p.SoCMinPct >= p.SoCMaxPct || p.SoCMaxPct > 100 {
		return fmt.Errorf("soc bounds must satisfy 0 <= min < max <= 100, got %.6g..%.6g",
			p.SoCMinPct, p.SoCMaxPct)
	}
	// A live battery may start outside the configured operating band and
	// recover toward it. Only the physical 0..100 percent range is hard here.
	if !finite(p.InitialSoCPct) || p.InitialSoCPct < 0 || p.InitialSoCPct > 100 {
		return fmt.Errorf("initial_soc_pct must be within 0..100, got %.6g", p.InitialSoCPct)
	}
	if err := requireNonNegativePlanningValue("max_charge_w", p.MaxChargeW); err != nil {
		return err
	}
	if err := requireNonNegativePlanningValue("max_discharge_w", p.MaxDischargeW); err != nil {
		return err
	}
	if err := requirePlanningEfficiency("charge_efficiency", p.ChargeEfficiency, false); err != nil {
		return err
	}
	if err := requirePlanningEfficiency("discharge_efficiency", p.DischargeEfficiency, false); err != nil {
		return err
	}

	for _, value := range []struct {
		name string
		v    float64
	}{
		{"terminal_soc_price", p.TerminalSoCPrice},
		{"export_ore_per_kwh", p.ExportOrePerKWh},
		{"export_bonus_ore_kwh", p.ExportBonusOreKwh},
		{"export_fee_ore_kwh", p.ExportFeeOreKwh},
	} {
		if !finite(value.v) {
			return fmt.Errorf("%s must be finite", value.name)
		}
	}
	if p.ExportFloorOreKwh != nil && !finite(*p.ExportFloorOreKwh) {
		return fmt.Errorf("export_floor_ore_kwh must be finite")
	}
	for _, value := range []struct {
		name string
		v    float64
	}{
		{"pv_charge_bonus_ore_kwh", p.PVChargeBonusOreKwh},
		{"min_arbitrage_spread_ore_kwh", p.MinArbitrageSpreadOreKwh},
		{"pv_uncertainty_w", p.PVUncertaintyW},
		{"pv_forecast_safety_k", p.PVForecastSafetyK},
	} {
		if err := requireNonNegativePlanningValue(value.name, value.v); err != nil {
			return err
		}
	}

	assetIDs := make(map[string]string, len(p.Storages)+len(p.Loadpoints)+1)
	if err := validateStorageSpecs(p, assetIDs); err != nil {
		return err
	}
	storageIDs := make(map[string]string, len(assetIDs))
	for id, field := range assetIDs {
		storageIDs[id] = field
	}
	if err := validateLoadpointSpecs(planningLoadpointSpecs(p), assetIDs); err != nil {
		return err
	}
	activeList := activeLoadpointSpecs(p.Loadpoints)
	if len(p.Loadpoints) > 0 && p.Loadpoint != nil && p.Loadpoint.active() {
		// The external planner consumes Loadpoints while the Go fallback consumes
		// Loadpoint. Validate the fallback on its own so a stale copy cannot hide
		// behind a valid list and reach only the fallback engine.
		if err := validateLoadpointSpecs([]*LoadpointSpec{p.Loadpoint}, storageIDs); err != nil {
			return fmt.Errorf("loadpoint fallback: %w", err)
		}
	}
	if len(activeList) > 0 && !planningLoadpointsEquivalent(p.Loadpoint, activeList[0]) {
		return fmt.Errorf("loadpoint fallback must match first active loadpoint %q", activeList[0].ID)
	}
	return nil
}

func validateStorageSpecs(p Params, assetIDs map[string]string) error {
	var totalCapacityWh, totalInitialWh, totalMinWh, totalMaxWh float64
	var totalChargeW, totalDischargeW float64
	for i, storage := range p.Storages {
		field := fmt.Sprintf("storages[%d]", i)
		if storage.ID == "" {
			return fmt.Errorf("%s.id must be non-empty", field)
		}
		if previous, exists := assetIDs[storage.ID]; exists {
			return fmt.Errorf("%s.id %q duplicates %s", field, storage.ID, previous)
		}
		assetIDs[storage.ID] = field
		if err := requirePositivePlanningValue(field+".capacity_wh", storage.CapacityWh); err != nil {
			return err
		}
		if !finite(storage.MinEnergyWh) || !finite(storage.MaxEnergyWh) ||
			storage.MinEnergyWh < 0 || storage.MinEnergyWh > storage.MaxEnergyWh ||
			storage.MaxEnergyWh > storage.CapacityWh {
			return fmt.Errorf("%s energy bounds must satisfy 0 <= min <= max <= capacity", field)
		}
		// Initial energy outside the operating band is recoverable, but energy
		// outside the physical battery is not.
		if !finite(storage.InitialEnergyWh) || storage.InitialEnergyWh < 0 ||
			storage.InitialEnergyWh > storage.CapacityWh {
			return fmt.Errorf("%s.initial_energy_wh must be within 0..capacity", field)
		}
		if err := requireNonNegativePlanningValue(field+".max_charge_w", storage.MaxChargeW); err != nil {
			return err
		}
		if err := requireNonNegativePlanningValue(field+".max_discharge_w", storage.MaxDischargeW); err != nil {
			return err
		}
		if err := requirePlanningEfficiency(field+".charge_efficiency", storage.ChargeEfficiency, false); err != nil {
			return err
		}
		if err := requirePlanningEfficiency(field+".discharge_efficiency", storage.DischargeEfficiency, false); err != nil {
			return err
		}
		if !planningValuesEqual(storage.ChargeEfficiency, p.ChargeEfficiency) ||
			!planningValuesEqual(storage.DischargeEfficiency, p.DischargeEfficiency) {
			return fmt.Errorf("%s efficiencies must match aggregate fallback efficiencies", field)
		}

		totalCapacityWh += storage.CapacityWh
		totalInitialWh += storage.InitialEnergyWh
		totalMinWh += storage.MinEnergyWh
		totalMaxWh += storage.MaxEnergyWh
		totalChargeW += storage.MaxChargeW
		totalDischargeW += storage.MaxDischargeW
	}
	if len(p.Storages) == 0 {
		return nil
	}

	energyToleranceWh := math.Max(1, p.CapacityWh*0.0002)
	checks := []struct {
		name string
		got  float64
		want float64
		tol  float64
	}{
		{"capacity_wh", totalCapacityWh, p.CapacityWh, 1},
		{"initial_energy_wh", totalInitialWh, p.CapacityWh * p.InitialSoCPct / 100, energyToleranceWh},
		{"min_energy_wh", totalMinWh, p.CapacityWh * p.SoCMinPct / 100, energyToleranceWh},
		{"max_energy_wh", totalMaxWh, p.CapacityWh * p.SoCMaxPct / 100, energyToleranceWh},
		{"max_charge_w", totalChargeW, p.MaxChargeW, 2},
		{"max_discharge_w", totalDischargeW, p.MaxDischargeW, 2},
	}
	for _, check := range checks {
		if math.Abs(check.got-check.want) > check.tol {
			return fmt.Errorf("storage aggregate %s %.6g does not match fallback %.6g",
				check.name, check.got, check.want)
		}
	}
	return nil
}

func planningLoadpointSpecs(p Params) []*LoadpointSpec {
	if len(p.Loadpoints) > 0 {
		return p.Loadpoints
	}
	if p.Loadpoint != nil {
		return []*LoadpointSpec{p.Loadpoint}
	}
	return nil
}

func activeLoadpointSpecs(loadpoints []*LoadpointSpec) []*LoadpointSpec {
	active := make([]*LoadpointSpec, 0, len(loadpoints))
	for _, loadpoint := range loadpoints {
		if loadpoint.active() {
			active = append(active, loadpoint)
		}
	}
	return active
}

func planningLoadpointsEquivalent(fallback, primary *LoadpointSpec) bool {
	if fallback == nil || primary == nil || fallback.PluggedIn != primary.PluggedIn ||
		fallback.ID != primary.ID || fallback.Levels != primary.Levels ||
		fallback.TargetSlotIdx != primary.TargetSlotIdx ||
		fallback.SurplusOnly != primary.SurplusOnly ||
		fallback.NoBatteryToEV != primary.NoBatteryToEV {
		return false
	}
	fallbackMin, fallbackMax := planningLoadpointBounds(fallback)
	primaryMin, primaryMax := planningLoadpointBounds(primary)
	for _, values := range [][2]float64{
		{fallback.CapacityWh, primary.CapacityWh},
		{fallbackMin, primaryMin},
		{fallbackMax, primaryMax},
		{fallback.InitialSoCPct, primary.InitialSoCPct},
		{fallback.TargetSoCPct, primary.TargetSoCPct},
		{fallback.MaxChargeW, primary.MaxChargeW},
		{planningLoadpointEfficiency(fallback), planningLoadpointEfficiency(primary)},
	} {
		if !planningValuesEqual(values[0], values[1]) {
			return false
		}
	}
	fallbackSteps := fallback.normalizedSteps()
	primarySteps := primary.normalizedSteps()
	if len(fallbackSteps) != len(primarySteps) {
		return false
	}
	for i := range fallbackSteps {
		if !planningValuesEqual(fallbackSteps[i], primarySteps[i]) {
			return false
		}
	}
	return true
}

func planningLoadpointBounds(loadpoint *LoadpointSpec) (float64, float64) {
	minPct, maxPct := loadpoint.MinPct, loadpoint.MaxPct
	if minPct == 0 && maxPct == 0 {
		maxPct = 100
	}
	return minPct, maxPct
}

func planningLoadpointEfficiency(loadpoint *LoadpointSpec) float64 {
	if loadpoint.ChargeEfficiency == 0 {
		return 0.9
	}
	return loadpoint.ChargeEfficiency
}

func validateLoadpointSpecs(loadpoints []*LoadpointSpec, assetIDs map[string]string) error {
	for i, loadpoint := range loadpoints {
		if loadpoint == nil || !loadpoint.PluggedIn {
			continue
		}
		field := fmt.Sprintf("loadpoints[%d]", i)
		if loadpoint.ID == "" {
			return fmt.Errorf("%s.id must be non-empty", field)
		}
		if previous, exists := assetIDs[loadpoint.ID]; exists {
			return fmt.Errorf("%s.id %q duplicates %s", field, loadpoint.ID, previous)
		}
		assetIDs[loadpoint.ID] = field
		if err := requirePositivePlanningValue(field+".capacity_wh", loadpoint.CapacityWh); err != nil {
			return err
		}
		if loadpoint.Levels < 2 {
			return fmt.Errorf("%s.levels must be at least 2, got %d", field, loadpoint.Levels)
		}
		minPct, maxPct := planningLoadpointBounds(loadpoint)
		if !finite(minPct) || !finite(maxPct) ||
			minPct < 0 || minPct >= maxPct || maxPct > 100 {
			return fmt.Errorf("%s SoC bounds must satisfy 0 <= min < max <= 100", field)
		}
		if !finite(loadpoint.InitialSoCPct) || loadpoint.InitialSoCPct < minPct ||
			loadpoint.InitialSoCPct > maxPct {
			return fmt.Errorf("%s.initial_soc_pct must be within the loadpoint SoC bounds", field)
		}
		if !finite(loadpoint.TargetSoCPct) || loadpoint.TargetSoCPct < 0 ||
			loadpoint.TargetSoCPct > 100 ||
			(loadpoint.TargetSoCPct != 0 &&
				(loadpoint.TargetSoCPct < minPct || loadpoint.TargetSoCPct > maxPct)) {
			return fmt.Errorf("%s.target_soc_pct must be zero or within the loadpoint SoC bounds", field)
		}
		if loadpoint.TargetSoCPct > 0 && loadpoint.TargetSlotIdx < 0 {
			return fmt.Errorf("%s.target_slot_idx must be non-negative when target_soc_pct is set", field)
		}
		if err := requireNonNegativePlanningValue(field+".max_charge_w", loadpoint.MaxChargeW); err != nil {
			return err
		}
		if err := requirePlanningEfficiency(field+".charge_efficiency", loadpoint.ChargeEfficiency, true); err != nil {
			return err
		}
		for stepIdx, stepW := range loadpoint.AllowedStepsW {
			if !finite(stepW) || stepW < 0 {
				return fmt.Errorf("%s.allowed_steps_w[%d] must be finite and non-negative", field, stepIdx)
			}
			if loadpoint.MaxChargeW > 0 && stepW > loadpoint.MaxChargeW {
				return fmt.Errorf("%s.allowed_steps_w[%d] exceeds max_charge_w", field, stepIdx)
			}
		}
	}
	return nil
}

func requirePositivePlanningValue(name string, value float64) error {
	if !finite(value) || value <= 0 {
		return fmt.Errorf("%s must be finite and greater than zero", name)
	}
	return nil
}

func requireNonNegativePlanningValue(name string, value float64) error {
	if !finite(value) || value < 0 {
		return fmt.Errorf("%s must be finite and non-negative", name)
	}
	return nil
}

func requirePlanningEfficiency(name string, value float64, allowDefaultZero bool) error {
	if !finite(value) || value < 0 || value > 1 || (!allowDefaultZero && value == 0) {
		if allowDefaultZero {
			return fmt.Errorf("%s must be zero or within (0, 1]", name)
		}
		return fmt.Errorf("%s must be within (0, 1]", name)
	}
	return nil
}

func planningValuesEqual(a, b float64) bool {
	tolerance := math.Max(1e-6, math.Max(math.Abs(a), math.Abs(b))*1e-9)
	return math.Abs(a-b) <= tolerance
}
