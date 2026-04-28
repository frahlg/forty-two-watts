package telemetry

import (
	"encoding/json"
	"time"
)

// VehicleMaxAge is the freshness window past which a DerVehicle reading
// is considered stale enough to ignore for control decisions. Picked
// conservatively at 5 min — TeslaBLEProxy polls cached every ~60 s, and
// the driver itself force-wakes every 30 min. Anything older than 5 min
// is either a sleeping car the proxy hasn't woken or a proxy outage,
// either way not safe to act on as ground truth.
const VehicleMaxAge = 5 * time.Minute

// VehiclePick is the "best matching" DerVehicle reading for a loadpoint:
// the one most likely to be the car physically connected right now.
// Empty Driver means "no usable reading" — the caller should fall back
// to whatever inferred SoC was already in place.
type VehiclePick struct {
	Driver         string
	SoCPct         float64 // bounded [0,100]
	ChargeLimitPct float64 // bounded [0,100]
	ChargingState  string
	Stale          bool      // proxy says "this is last-known, vehicle asleep"
	UpdatedAt      time.Time // wall-clock of the underlying reading
}

// VehicleConnectedRank scores how likely a DerVehicle driver is to be the
// one physically plugged into the loadpoint right now, based on Tesla
// Owner-API charging_state semantics (other vendors use the same
// vocabulary). Higher rank = more likely connected. Negative = explicitly
// not connected; caller should skip.
//
// Single source of truth for the rank table — both main.go (MPC plan
// inputs) and api.go (loadpoint decoration) call this so multi-vehicle
// pick decisions stay consistent.
func VehicleConnectedRank(chargingState string) int {
	switch chargingState {
	case "Charging", "Starting":
		return 3 // actively pulling power — definitely this car
	case "NoPower":
		return 2 // plugged but wallbox not delivering yet
	case "Stopped", "Complete":
		return 1 // plugged + idle (charge limit reached, paused, etc.)
	case "Disconnected":
		return -1 // explicitly unplugged — never pick this one
	default:
		return 0 // unknown/missing — usable but de-prioritised
	}
}

// PickBestVehicle scans the store for the single DerVehicle reading
// most likely to be the car connected right now: highest
// VehicleConnectedRank, tiebreak by freshness. Returns a zero-value
// VehiclePick if no usable reading exists.
//
// Defenses applied here (do NOT skip — the BLE proxy is a network
// trust boundary):
//   - SoC bounded to [0,100] — a misbehaving proxy reporting 200 % or
//     -50 % must not be able to overcharge or freeze EV charging.
//   - ChargeLimitPct bounded to [0,100] — same risk.
//   - Stale by `now − UpdatedAt > VehicleMaxAge` — wallclock check on
//     the reading's own timestamp, even when the proxy didn't set the
//     `stale` flag. A proxy that stops responding mustn't keep the
//     last-known SoC live forever.
//   - Driver health-online check — offline drivers contribute nothing.
//
// Lives in telemetry/ rather than api/ or cmd/ because both packages
// need it and the dependency direction otherwise cycles.
func PickBestVehicle(s *Store, now time.Time) VehiclePick {
	if s == nil {
		return VehiclePick{}
	}
	var best VehiclePick
	bestRank := -1
	for _, vr := range s.ReadingsByType(DerVehicle) {
		if vr.SoC == nil {
			continue
		}
		if h := s.DriverHealth(vr.Driver); h == nil || !h.IsOnline() {
			continue
		}
		if !vr.UpdatedAt.IsZero() && now.Sub(vr.UpdatedAt) > VehicleMaxAge {
			// Reading is older than we're willing to trust as ground
			// truth — proxy probably stopped publishing. Skip rather
			// than risk acting on a stale SoC.
			continue
		}
		var meta struct {
			ChargingState  string  `json:"charging_state"`
			ChargeLimitPct float64 `json:"charge_limit_pct"`
			Stale          bool    `json:"stale"`
		}
		if len(vr.Data) > 0 {
			_ = json.Unmarshal(vr.Data, &meta)
		}
		rank := VehicleConnectedRank(meta.ChargingState)
		if rank < 0 {
			continue
		}
		if rank < bestRank || (rank == bestRank && !vr.UpdatedAt.After(best.UpdatedAt)) {
			continue
		}
		soc := *vr.SoC
		if soc < 0 {
			soc = 0
		} else if soc > 100 {
			soc = 100
		}
		limit := meta.ChargeLimitPct
		if limit < 0 {
			limit = 0
		} else if limit > 100 {
			limit = 100
		}
		best = VehiclePick{
			Driver:         vr.Driver,
			SoCPct:         soc,
			ChargeLimitPct: limit,
			ChargingState:  meta.ChargingState,
			Stale:          meta.Stale,
			UpdatedAt:      vr.UpdatedAt,
		}
		bestRank = rank
	}
	return best
}
