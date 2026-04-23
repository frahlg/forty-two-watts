package loadpoint

import (
	"testing"
	"time"
)

// TestReplanHintFiresOnSocDelta — a fresh vehicle reading with
// SoC well beyond the threshold (3 pp) from what was last hinted
// should trigger the replan callback.
func TestReplanHintFiresOnSocDelta(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{
		ID: "garage", DriverName: "easee",
		VehicleDriver: "tesla-x",
	}})
	var vs VehicleSample
	m.SetVehicleTelemetry(func(driver string) VehicleSample {
		if driver == "tesla-x" {
			return vs
		}
		return VehicleSample{}
	})
	var hints []string
	m.SetReplanHint(func(lpID string) { hints = append(hints, lpID) })

	// First observe with vehicle reading — no prior baseline, so no
	// delta yet (but socSource flip inferred→vehicle DOES hint).
	vs = VehicleSample{OK: true, SoCPct: 40, ChargeLimitPct: 80}
	m.Observe("garage", true, 0, 0, false)
	if len(hints) != 1 {
		t.Fatalf("expected 1 hint on first vehicle reading (source flip), got %d", len(hints))
	}

	// Second observe with +1 pp — below threshold, no new hint.
	// Force cooldown-bypass by waiting via internal state swap.
	vs = VehicleSample{OK: true, SoCPct: 41, ChargeLimitPct: 80}
	m.Observe("garage", true, 3000, 0, true)
	if len(hints) != 1 {
		t.Fatalf("expected still 1 hint (delta below threshold), got %d", len(hints))
	}

	// Nudge the cooldown window forward by editing the bookkeeping.
	m.mu.Lock()
	m.replanHintLast["garage"] = time.Now().Add(-2 * time.Minute)
	m.mu.Unlock()

	// +5 pp — well past threshold, should fire.
	vs = VehicleSample{OK: true, SoCPct: 46, ChargeLimitPct: 80}
	m.Observe("garage", true, 3000, 0, true)
	if len(hints) != 2 {
		t.Fatalf("expected 2 hints after SoC delta, got %d", len(hints))
	}
}

// TestReplanHintFiresOnChargeLimitChange — operator bumps the
// Tesla app slider mid-session; MPC must re-plan against the new
// effective target instead of waiting 15 min.
func TestReplanHintFiresOnChargeLimitChange(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee", VehicleDriver: "tesla-x"}})
	var vs VehicleSample
	m.SetVehicleTelemetry(func(d string) VehicleSample { return vs })
	var hints []string
	m.SetReplanHint(func(lpID string) { hints = append(hints, lpID) })

	vs = VehicleSample{OK: true, SoCPct: 40, ChargeLimitPct: 80}
	m.Observe("garage", true, 0, 0, false)
	base := len(hints)

	// Advance cooldown so limit-change isn't swallowed.
	m.mu.Lock()
	m.replanHintLast["garage"] = time.Now().Add(-2 * time.Minute)
	m.mu.Unlock()

	vs.ChargeLimitPct = 90
	m.Observe("garage", true, 3000, 0, true)
	if len(hints) != base+1 {
		t.Errorf("expected 1 new hint on charge_limit change, got %d", len(hints)-base)
	}
}

// TestReplanHintRateLimited — back-to-back material changes within
// the cooldown window produce at most one hint.
func TestReplanHintRateLimited(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee", VehicleDriver: "tesla-x"}})
	var vs VehicleSample
	m.SetVehicleTelemetry(func(d string) VehicleSample { return vs })
	var hints []string
	m.SetReplanHint(func(lpID string) { hints = append(hints, lpID) })

	vs = VehicleSample{OK: true, SoCPct: 40, ChargeLimitPct: 80}
	m.Observe("garage", true, 0, 0, false) // hint #1 (inferred → vehicle)

	// No cooldown bypass — expect zero new hints despite a big delta.
	vs.SoCPct = 60
	m.Observe("garage", true, 3000, 0, true)
	vs.SoCPct = 65
	m.Observe("garage", true, 3000, 0, true)
	vs.ChargeLimitPct = 90
	m.Observe("garage", true, 3000, 0, true)
	if len(hints) != 1 {
		t.Errorf("expected exactly 1 hint due to cooldown, got %d", len(hints))
	}
}

// TestReplanHintSkipsWhenNoCallback — nil callback means no fire,
// no crash.
func TestReplanHintSkipsWhenNoCallback(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee", VehicleDriver: "tesla-x"}})
	m.SetVehicleTelemetry(func(d string) VehicleSample {
		return VehicleSample{OK: true, SoCPct: 40, ChargeLimitPct: 80}
	})
	// no SetReplanHint
	m.Observe("garage", true, 0, 0, false) // must not panic
}
