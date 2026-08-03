package thermal

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const runtimeHomeSpec = `{
  "schema_version": 2,
  "kind": "ftw.home_thermal_spec",
  "site_id": "home",
  "primary_zone_id": "home-zone",
  "zones": [{
    "id": "home-zone",
    "comfort": {"minimum_temperature_c": 19, "maximum_temperature_c": 23}
  }],
  "heating": {
    "source": "air_water_heat_pump",
    "emitters": "radiators",
    "maximum_electric_power_w": 4000,
    "cop_curve": {
      "reference_temperature_c": 7,
      "cop_at_reference": 3.4,
      "slope_per_c": 0.05,
      "minimum_cop": 1.5,
      "maximum_cop": 5.5
    }
  },
  "sensors": {
    "indoor_temperature": {"driver": "hp", "metric": "hp_indoor_temp_c"},
    "outdoor_temperature": {"driver": "hp", "metric": "hp_outdoor_temp_c"},
    "heat_pump_power": {"driver": "hp", "metric": "hp_power_w"}
  }
}`

type fakeMetric struct {
	value float64
	at    time.Time
}

type fakeMetricReader map[string]fakeMetric

func (f fakeMetricReader) LatestMetric(driver, name string) (float64, time.Time, bool) {
	value, ok := f[driver+":"+name]
	return value.value, value.at, ok
}

type healthMetricReader struct {
	values  fakeMetricReader
	healthy bool
}

func (r healthMetricReader) LatestMetric(driver, name string) (float64, time.Time, bool) {
	return r.values.LatestMetric(driver, name)
}

func (r healthMetricReader) DriverHealthy(string) bool { return r.healthy }

func TestHomeSpecTypedRevisionMatchesPython(t *testing.T) {
	spec, err := ParseHomeSpec([]byte(runtimeHomeSpec))
	if err != nil {
		t.Fatal(err)
	}
	const pythonRevision = "056e0fba681c69194b58af49b2eb470f04dc1909d13c825e8b1b2fcc37177000"
	if spec.Revision != pythonRevision {
		t.Fatalf("revision = %s, want Python revision %s", spec.Revision, pythonRevision)
	}
}

func mustRuntime(t *testing.T, metrics fakeMetricReader) *Runtime {
	t.Helper()
	spec, err := ParseHomeSpec([]byte(runtimeHomeSpec))
	if err != nil {
		t.Fatal(err)
	}
	artifact := mustArtifact(t)
	artifact.SiteID = spec.SiteID
	artifact.HomeSpecRevision = spec.Revision
	revision, err := artifact.contentRevision()
	if err != nil {
		t.Fatal(err)
	}
	artifact.Revision = revision
	runtime, err := NewRuntime(*spec, artifact, metrics, RuntimeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func TestRuntimeBuildsLoadFromFreshCanonicalMetrics(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runtime := mustRuntime(t, fakeMetricReader{
		"hp:hp_indoor_temp_c":  {value: 18, at: now.Add(-time.Minute)},
		"hp:hp_outdoor_temp_c": {value: -4, at: now.Add(-time.Minute)},
		"hp:hp_power_w":        {value: 1_500, at: now.Add(-time.Minute)},
	})
	outdoor := -5.0
	loads, err := runtime.OptimizerLoads(now, []ForecastSlot{{Start: now, Duration: 15 * time.Minute, OutdoorTempC: &outdoor}})
	if err != nil {
		t.Fatal(err)
	}
	if len(loads) != 1 || loads[0].InitialTempC != 18 || loads[0].OutsideTempC[0] != -5 {
		t.Fatalf("unexpected loads: %+v", loads)
	}
	snapshot := runtime.Snapshot(now)
	if !snapshot.PlanningReady || !snapshot.LiveCheckReady {
		t.Fatalf("unexpected readiness: %+v", snapshot)
	}
}

func TestRuntimeFailsClosedToWholeLoadWhenIndoorMetricIsStale(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runtime := mustRuntime(t, fakeMetricReader{
		"hp:hp_indoor_temp_c":  {value: 20, at: now.Add(-time.Hour)},
		"hp:hp_outdoor_temp_c": {value: -4, at: now},
		"hp:hp_power_w":        {value: 1_500, at: now},
	})
	outdoor := -5.0
	_, err := runtime.OptimizerLoads(now, []ForecastSlot{{OutdoorTempC: &outdoor}})
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("OptimizerLoads error = %v, want stale", err)
	}
}

func TestRuntimeRejectsImplausiblePowerForLiveCheckOnly(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runtime := mustRuntime(t, fakeMetricReader{
		"hp:hp_indoor_temp_c":  {value: 20, at: now},
		"hp:hp_outdoor_temp_c": {value: -4, at: now},
		"hp:hp_power_w":        {value: 6_000, at: now},
	})
	snapshot := runtime.Snapshot(now)
	if !snapshot.PlanningReady || snapshot.LiveCheckReady {
		t.Fatalf("unexpected readiness: %+v", snapshot)
	}
	if got := snapshot.Metrics["heat_pump_power"].Reason; !strings.Contains(got, "plausible range") {
		t.Fatalf("power reason = %q", got)
	}
}

func TestRuntimeRejectsFreshCacheFromOfflineDriver(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	spec, err := ParseHomeSpec([]byte(runtimeHomeSpec))
	if err != nil {
		t.Fatal(err)
	}
	artifact := mustArtifact(t)
	artifact.SiteID = spec.SiteID
	artifact.HomeSpecRevision = spec.Revision
	artifact.Revision, err = artifact.contentRevision()
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(*spec, artifact, healthMetricReader{
		healthy: false,
		values: fakeMetricReader{
			"hp:hp_indoor_temp_c":  {value: 20, at: now},
			"hp:hp_outdoor_temp_c": {value: -4, at: now},
			"hp:hp_power_w":        {value: 1_500, at: now},
		},
	}, RuntimeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := runtime.Snapshot(now)
	if snapshot.PlanningReady || snapshot.Metrics["indoor_temperature"].Reason != "driver is offline" {
		t.Fatalf("offline driver's cached value was accepted: %+v", snapshot)
	}
}

func TestRuntimeKeepsTwoR2COutOfPlanningWithoutMassObserver(t *testing.T) {
	runtime := mustRuntime(t, fakeMetricReader{})
	runtime.Artifact.ModelType = ModelType2R2C
	runtime.Artifact.Physics.ThermalCapacityWhPerK = nil
	coupling, airCapacity, massCapacity := 900.0, 1_200.0, 14_000.0
	runtime.Artifact.Physics.MassCouplingWPerK = &coupling
	runtime.Artifact.Physics.AirCapacityWhPerK = &airCapacity
	runtime.Artifact.Physics.MassCapacityWhPerK = &massCapacity
	revision, err := runtime.Artifact.contentRevision()
	if err != nil {
		t.Fatal(err)
	}
	runtime.Artifact.Revision = revision
	outdoor := 0.0
	_, err = runtime.OptimizerLoads(time.Now(), []ForecastSlot{{
		Start: time.Now(), Duration: 15 * time.Minute, OutdoorTempC: &outdoor,
	}})
	if err == nil || !strings.Contains(err.Error(), "mass-temperature observer") {
		t.Fatalf("OptimizerLoads error = %v, want observer rejection", err)
	}
}

func TestRuntimeRequiresCompleteOutdoorForecast(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runtime := mustRuntime(t, fakeMetricReader{
		"hp:hp_indoor_temp_c": {value: 20, at: now},
	})
	_, err := runtime.OptimizerLoads(now, []ForecastSlot{{OutdoorTempC: nil}})
	if err == nil || !strings.Contains(err.Error(), "forecast") {
		t.Fatalf("OptimizerLoads error = %v, want forecast failure", err)
	}
}

func TestRuntimeRejectsUnsafeMetricAge(t *testing.T) {
	spec, err := ParseHomeSpec([]byte(runtimeHomeSpec))
	if err != nil {
		t.Fatal(err)
	}
	artifact := mustArtifact(t)
	artifact.SiteID = spec.SiteID
	artifact.HomeSpecRevision = spec.Revision
	revision, revisionErr := artifact.contentRevision()
	if revisionErr != nil {
		t.Fatal(revisionErr)
	}
	artifact.Revision = revision
	_, err = NewRuntime(*spec, artifact, fakeMetricReader{}, RuntimeOptions{MaxMetricAge: 2 * time.Hour})
	if err == nil || !strings.Contains(err.Error(), "must not exceed") {
		t.Fatalf("NewRuntime error = %v, want maximum-age rejection", err)
	}
}

func TestRuntimeRejectsArtifactForAnotherSiteOrHomeSpec(t *testing.T) {
	spec, err := ParseHomeSpec([]byte(runtimeHomeSpec))
	if err != nil {
		t.Fatal(err)
	}
	artifact := mustArtifact(t)
	artifact.SiteID = "another-home"
	artifact.HomeSpecRevision = spec.Revision
	revision, err := artifact.contentRevision()
	if err != nil {
		t.Fatal(err)
	}
	artifact.Revision = revision
	if _, err := NewRuntime(*spec, artifact, fakeMetricReader{}, RuntimeOptions{}); err == nil || !strings.Contains(err.Error(), "site_id") {
		t.Fatalf("NewRuntime error = %v, want site binding rejection", err)
	}

	artifact.SiteID = spec.SiteID
	artifact.HomeSpecRevision = strings.Repeat("c", 64)
	revision, err = artifact.contentRevision()
	if err != nil {
		t.Fatal(err)
	}
	artifact.Revision = revision
	if _, err := NewRuntime(*spec, artifact, fakeMetricReader{}, RuntimeOptions{}); err == nil || !strings.Contains(err.Error(), "different home spec") {
		t.Fatalf("NewRuntime error = %v, want HomeSpec binding rejection", err)
	}
}

func TestRuntimeTransitionAssessmentUsesCalibrationBand(t *testing.T) {
	runtime := mustRuntime(t, fakeMetricReader{})
	base := TransitionObservation{
		InitialAirC: 20,
		OutdoorC:    0,
		PowerW:      1_200,
		Duration:    15 * time.Minute,
	}
	probe, err := runtime.Artifact.OptimizerLoad(OptimizerLoadInput{
		InitialTemperatureC: 20, MinimumTemperatureC: -10, MaximumTemperatureC: 50,
		OutsideTemperatureC: []float64{0}, MaximumElectricPowerW: 4_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	predicted, err := probe.NextState(0, ModelState{AirC: 20, MassC: 20}, 1_200, 0.25)
	if err != nil {
		t.Fatal(err)
	}
	base.ObservedAirC = predicted.AirC + 0.2
	good := runtime.AssessTransition(base)
	if !good.Reasonable {
		t.Fatalf("reasonable transition rejected: %+v", good)
	}
	base.ObservedAirC = predicted.AirC + 2
	bad := runtime.AssessTransition(base)
	if bad.Reasonable || bad.Reason == "" {
		t.Fatalf("bad transition accepted: %+v", bad)
	}
	if got := fmt.Sprintf("%.1f", bad.AllowedErrorC); got != "1.0" {
		t.Fatalf("allowed error = %s", got)
	}
}
