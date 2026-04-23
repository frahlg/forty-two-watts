package loadpoint

// vehicle_discovery.go contains the types + scoring function the
// Manager uses to bind a plugged-in EV charger to a vehicle data
// driver (e.g. tesla_vehicle.lua → TeslaBLEProxy). Kept separate
// from loadpoint.go so the core Config / State / Manager types
// stay readable.

// VehicleSample is the subset of a DerVehicle reading the manager
// consumes: the real SoC from the car's BMS, the vehicle-configured
// charge limit (from e.g. the Tesla app), and a freshness flag set
// by the driver when no poll has refreshed the data within the
// configured stale_after_s. `OK` is false when no reading exists
// yet — the manager falls back to the inferred SoC path.
type VehicleSample struct {
	OK             bool
	SoCPct         float64
	ChargeLimitPct float64
	ChargingState  string
	TimeToFullMin  int
	Stale          bool
}

// VehicleTelemetryFunc returns the latest vehicle reading for a
// driver name. Callers (main.go) wire this to telemetry.Store and
// parse the Data JSON of the DerVehicle reading.
type VehicleTelemetryFunc func(driver string) VehicleSample

// VehicleSnapshotFunc returns EVERY currently-known vehicle reading,
// keyed by driver name. Used by plug-in auto-discovery: when the
// charger flips to connected and the loadpoint has no explicit
// `vehicle_driver` binding, the manager scores each candidate and
// picks the one most likely to be the plugged-in car. nil disables
// auto-discovery; the loadpoint falls back to the inferred-SoC path.
type VehicleSnapshotFunc func() map[string]VehicleSample

// VehiclePokeFunc forces an out-of-cycle poll on the named vehicle
// driver. Manager calls it on plug-in transition for every known
// vehicle driver so the discovery scoring sees fresh BMS readings
// instead of up-to-60-s-stale ones. Best-effort + non-blocking; the
// poke just hurries the next regular poll along.
type VehiclePokeFunc func(driver string)

// pickPluggedVehicle scores every live DerVehicle sample and returns
// the driver name most likely to belong to the just-plugged car.
// Returns "" when no good candidate exists. Scoring:
//
//   - "Charging"                     → 100 (definitely this one)
//   - "Starting" / "NoPower" /
//     "Stopped" / "Complete" / ""    → 40  (could be this one; plug
//                                            just happened so the
//                                            car may not have
//                                            started the session yet)
//   - "Disconnected"                 → 0   (explicitly not this one)
//   - Stale reading                  → halved
//
// Ties broken by driver name (alphabetical) for determinism.
func pickPluggedVehicle(samples map[string]VehicleSample) string {
	type cand struct {
		name  string
		score int
	}
	best := cand{}
	for name, vs := range samples {
		if !vs.OK {
			continue
		}
		var score int
		switch vs.ChargingState {
		case "Charging":
			score = 100
		case "Disconnected":
			score = 0
		case "Starting", "NoPower", "Stopped", "Complete", "":
			score = 40
		default:
			score = 20
		}
		if vs.Stale {
			score /= 2
		}
		if score == 0 {
			continue
		}
		if score > best.score || (score == best.score && name < best.name) {
			best = cand{name: name, score: score}
		}
	}
	return best.name
}
