package loadpoint

import (
	"testing"
)

// TestObservePrefersVehicleSoCWhenFresh — when a vehicle driver is
// configured AND the injected getter returns a fresh reading, the
// manager's currentSoCPct comes from the vehicle's own BMS, not the
// pluginSoC + deliveredWh inference.
func TestObservePrefersVehicleSoCWhenFresh(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{
		ID:                "garage",
		DriverName:        "easee",
		VehicleDriver:     "tesla-garage",
		VehicleCapacityWh: 60000,
		PluginSoCPct:      20,
	}})
	m.SetVehicleTelemetry(func(driver string) VehicleSample {
		if driver != "tesla-garage" {
			return VehicleSample{}
		}
		return VehicleSample{
			OK: true, SoCPct: 64, ChargeLimitPct: 80,
			ChargingState: "Charging", TimeToFullMin: 45,
			Stale: false,
		}
	})

	// Delivered 6 kWh → inferred SoC would be 20 + 6000/60000*100 = 30 %.
	// Vehicle reports 64 %. Manager must prefer 64.
	m.Observe("garage", true, 7400, 6000, false)

	st, ok := m.State("garage")
	if !ok {
		t.Fatal("state missing")
	}
	if st.CurrentSoCPct < 63 || st.CurrentSoCPct > 65 {
		t.Errorf("CurrentSoCPct = %.1f; want ~64 (from vehicle)", st.CurrentSoCPct)
	}
	if st.SoCSource != "vehicle" {
		t.Errorf("SoCSource = %q; want 'vehicle'", st.SoCSource)
	}
	if st.VehicleChargeLimitPct != 80 {
		t.Errorf("VehicleChargeLimitPct = %.0f; want 80", st.VehicleChargeLimitPct)
	}
	if st.VehicleChargingState != "Charging" {
		t.Errorf("VehicleChargingState = %q", st.VehicleChargingState)
	}
}

// TestObserveFallsBackWhenVehicleStale — stale vehicle readings are
// stored for display but do NOT override the inferred SoC. Operators
// need a recent ground truth, not a week-old cached value.
func TestObserveFallsBackWhenVehicleStale(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{
		ID:                "garage",
		DriverName:        "easee",
		VehicleDriver:     "tesla-garage",
		VehicleCapacityWh: 60000,
		PluginSoCPct:      20,
	}})
	m.SetVehicleTelemetry(func(driver string) VehicleSample {
		return VehicleSample{
			OK: true, SoCPct: 90, ChargeLimitPct: 80,
			Stale: true,
		}
	})

	m.Observe("garage", true, 7400, 6000, false) // inferred: 30 %

	st, _ := m.State("garage")
	if st.SoCSource != "inferred" {
		t.Errorf("stale vehicle should fall back to inferred, got source %q", st.SoCSource)
	}
	if st.CurrentSoCPct < 29 || st.CurrentSoCPct > 31 {
		t.Errorf("CurrentSoCPct = %.1f; want ~30 (inferred)", st.CurrentSoCPct)
	}
	// Stale reading still surfaces on state so UI can render it with ★.
	if st.VehicleSoCPct != 90 || !st.VehicleStale {
		t.Errorf("stale reading not surfaced: soc=%.0f stale=%v",
			st.VehicleSoCPct, st.VehicleStale)
	}
}

// TestObserveNoVehicleDriverUsesInference — legacy loadpoints without
// VehicleDriver keep their pre-Tesla behaviour. SoCSource is
// "inferred" whenever plugged-in; blank when not.
func TestObserveNoVehicleDriverUsesInference(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{
		ID:                "garage",
		DriverName:        "easee",
		VehicleCapacityWh: 60000,
		PluginSoCPct:      40,
	}})
	m.SetVehicleTelemetry(func(string) VehicleSample { return VehicleSample{} })

	m.Observe("garage", true, 7400, 12000, false)
	st, _ := m.State("garage")
	if st.SoCSource != "inferred" {
		t.Errorf("no VehicleDriver should yield inferred, got %q", st.SoCSource)
	}
	// 40 + 12000/60000*100 = 60
	if st.CurrentSoCPct < 59 || st.CurrentSoCPct > 61 {
		t.Errorf("CurrentSoCPct = %.1f; want ~60", st.CurrentSoCPct)
	}
	if st.VehicleDriver != "" {
		t.Errorf("VehicleDriver leaked: %q", st.VehicleDriver)
	}
}
