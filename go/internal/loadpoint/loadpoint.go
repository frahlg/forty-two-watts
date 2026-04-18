// Package loadpoint models an EV charge point as a first-class entity
// the planner can reason about. A loadpoint couples a physical charger
// driver (Easee, Zap, OCPP, …) with a specific vehicle and user intent
// (target SoC by target time).
//
// This package currently hosts the config-facing types and a read-only
// manager that surfaces configured loadpoints through the API. Phase 3
// of the planner overhaul introduces the skeleton without wiring it to
// the MPC's decision surface — that comes in Phase 4, where the DP is
// extended with EV-SoC state and the dispatch layer gains a per-
// loadpoint energy-budget path that mirrors the battery energy path.
//
// Keeping it lightweight is intentional: EVCC ships ~20 kLOC of
// loadpoint machinery (hysteresis, enable/disable delays, phase
// switching). We don't need most of that because the energy-budget
// contract is continuous by construction (no flap-flapping). Phase
// switching is a driver-local heuristic, not a planner concern.
package loadpoint

import (
	"sort"
	"sync"
	"time"
)

// Config is the YAML-facing definition of one loadpoint. Wired into
// config.Config under "loadpoints". All electrical fields are
// optional with sensible defaults for a typical single-phase /
// three-phase residential EV charger.
type Config struct {
	ID         string `yaml:"id" json:"id"`                   // stable identifier ("garage", "street")
	DriverName string `yaml:"driver_name" json:"driver_name"` // which driver controls the charger

	// Elektriska gränser
	MinChargeW    float64   `yaml:"min_charge_w,omitempty" json:"min_charge_w,omitempty"`       // e.g. 1400 (1-phase 6 A)
	MaxChargeW    float64   `yaml:"max_charge_w,omitempty" json:"max_charge_w,omitempty"`       // e.g. 11000 (3-phase 16 A)
	AllowedStepsW []float64 `yaml:"allowed_steps_w,omitempty" json:"allowed_steps_w,omitempty"` // discrete Wh levels supported

	// Battery capacity in Wh (used to translate SoC% ↔ Wh and to
	// validate target-SoC feasibility given a deadline). 0 falls
	// back to a typical 60 kWh assumption.
	VehicleCapacityWh float64 `yaml:"vehicle_capacity_wh,omitempty" json:"vehicle_capacity_wh,omitempty"`

	// Assumed EV SoC % at plug-in. Chargers like Easee don't report
	// the vehicle's SoC directly — only cumulative session energy.
	// Current SoC is then estimated as `PluginSoCPct + delivered / cap`.
	// 0 defaults to 20 % (conservative). Operators who care can
	// override per-loadpoint or pre-plug-in.
	PluginSoCPct float64 `yaml:"plugin_soc_pct,omitempty" json:"plugin_soc_pct,omitempty"`
}

// State is the observable snapshot of one loadpoint at a point in time.
// Read-only for consumers — only the Manager or dispatch paths mutate
// it under lock.
type State struct {
	ID                 string    `json:"id"`
	DriverName         string    `json:"driver_name"`
	PluggedIn          bool      `json:"plugged_in"`
	CurrentSoCPct      float64   `json:"current_soc_pct"`       // observed or estimated
	CurrentPowerW      float64   `json:"current_power_w"`       // actual draw (site sign: + = charging)
	DeliveredWhSession float64   `json:"delivered_wh_session"`  // since plug-in
	TargetSoCPct       float64   `json:"target_soc_pct"`        // user intent
	TargetTime         time.Time `json:"target_time,omitempty"` // user intent
	UpdatedAtMs        int64     `json:"updated_at_ms"`
	// MinChargeW / MaxChargeW / AllowedStepsW are repeated here so the
	// UI has everything for rendering in one fetch.
	MinChargeW    float64   `json:"min_charge_w"`
	MaxChargeW    float64   `json:"max_charge_w"`
	AllowedStepsW []float64 `json:"allowed_steps_w,omitempty"`
}

// Manager holds the running set of loadpoints. Thread-safe.
type Manager struct {
	mu     sync.RWMutex
	byID   map[string]*loadpointRuntime
	order  []string // insertion-preserving id list for deterministic listing
}

// loadpointRuntime is the in-memory representation. Its fields are the
// union of configured parameters and observed state. Lives behind
// Manager so consumers access it via the public State snapshot.
type loadpointRuntime struct {
	Config

	pluggedIn          bool
	currentSoCPct      float64
	currentPowerW      float64
	deliveredWhSession float64
	targetSoCPct       float64
	targetTime         time.Time
	updatedAtMs        int64

	// Plug-in anchor: the SoC we believe the vehicle was at when
	// this session began. Persisted across Observe() calls so SoC
	// inference (pluginSoC + deliveredWh/capacity) stays stable
	// even as session_wh grows. Reset to Config.PluginSoCPct on
	// every plug-in transition (prev !pluggedIn → now pluggedIn).
	sessionPluginSoCPct float64
}

// NewManager returns an empty manager. Configure with Load().
func NewManager() *Manager {
	return &Manager{byID: map[string]*loadpointRuntime{}}
}

// Load replaces the configured set. Idempotent: existing state is
// carried across when the ID is kept; removed IDs are dropped.
func (m *Manager) Load(cfgs []Config) {
	m.mu.Lock()
	defer m.mu.Unlock()

	newByID := make(map[string]*loadpointRuntime, len(cfgs))
	newOrder := make([]string, 0, len(cfgs))
	for _, c := range cfgs {
		if c.ID == "" {
			continue
		}
		lp := &loadpointRuntime{Config: c}
		if existing, ok := m.byID[c.ID]; ok {
			// Preserve observed state across reload. The session
			// plug-in anchor is carried too — otherwise a config
			// hot-reload during a charging session would drop our
			// SoC reference and reset the estimate back to
			// PluginSoCPct even though delivered_wh has grown.
			lp.pluggedIn = existing.pluggedIn
			lp.currentSoCPct = existing.currentSoCPct
			lp.currentPowerW = existing.currentPowerW
			lp.deliveredWhSession = existing.deliveredWhSession
			lp.targetSoCPct = existing.targetSoCPct
			lp.targetTime = existing.targetTime
			lp.updatedAtMs = existing.updatedAtMs
			lp.sessionPluginSoCPct = existing.sessionPluginSoCPct
		}
		newByID[c.ID] = lp
		newOrder = append(newOrder, c.ID)
	}
	m.byID = newByID
	m.order = newOrder
}

// IDs returns configured loadpoint IDs in insertion order.
func (m *Manager) IDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.order))
	copy(out, m.order)
	return out
}

// State returns an immutable snapshot. Returns (State{}, false) when ID
// is unknown.
func (m *Manager) State(id string) (State, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	lp, ok := m.byID[id]
	if !ok {
		return State{}, false
	}
	return lp.snapshot(), true
}

// States returns snapshots of every configured loadpoint, sorted by
// the configured ID order. Useful for GET /api/loadpoints.
func (m *Manager) States() []State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]State, 0, len(m.order))
	for _, id := range m.order {
		if lp, ok := m.byID[id]; ok {
			out = append(out, lp.snapshot())
		}
	}
	return out
}

// Observe updates the measurement side of a loadpoint from raw driver
// telemetry. The manager derives current SoC internally from the
// session's plug-in anchor + delivered energy (chargers like Easee
// don't report the vehicle's actual SoC).
//
// Plug-in transitions (prev !pluggedIn → now pluggedIn) reset the
// session anchor to Config.PluginSoCPct (default 20 %) so the
// inference is stable across plug cycles even if the underlying
// charger's session counter wraps or resets.
//
// No-op for unknown IDs — a misconfigured driver shouldn't crash the
// manager.
func (m *Manager) Observe(id string, pluggedIn bool, powerW, deliveredWh float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lp, ok := m.byID[id]
	if !ok {
		return
	}
	if pluggedIn && !lp.pluggedIn {
		// Plug-in transition: seed the session anchor.
		anchor := lp.PluginSoCPct
		if anchor <= 0 {
			anchor = 20 // conservative default
		}
		lp.sessionPluginSoCPct = anchor
	}
	lp.pluggedIn = pluggedIn
	lp.currentPowerW = powerW
	lp.deliveredWhSession = deliveredWh
	if pluggedIn {
		lp.currentSoCPct = estimateSoCPct(lp.sessionPluginSoCPct,
			deliveredWh, lp.VehicleCapacityWh)
	} else {
		lp.currentSoCPct = 0
	}
	lp.updatedAtMs = time.Now().UnixMilli()
}

// estimateSoCPct returns the vehicle SoC % inferred from the session
// anchor + energy delivered. Chargers like Easee don't expose the
// car's BMS; this is the best-effort estimate the MPC uses.
//
// Clamps to [0, 100]. Falls back to the anchor when capacity is
// unknown (can't translate Wh → %).
func estimateSoCPct(pluginSoCPct, deliveredWh, capacityWh float64) float64 {
	if capacityWh <= 0 {
		return pluginSoCPct
	}
	soc := pluginSoCPct + deliveredWh/capacityWh*100.0
	if soc < 0 {
		return 0
	}
	if soc > 100 {
		return 100
	}
	return soc
}

// SetTarget updates the user-intent fields for an existing loadpoint.
// targetTime zero = no deadline. Returns false for unknown IDs.
func (m *Manager) SetTarget(id string, socPct float64, targetTime time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	lp, ok := m.byID[id]
	if !ok {
		return false
	}
	if socPct < 0 {
		socPct = 0
	}
	if socPct > 100 {
		socPct = 100
	}
	lp.targetSoCPct = socPct
	lp.targetTime = targetTime
	return true
}

func (lp *loadpointRuntime) snapshot() State {
	steps := make([]float64, len(lp.AllowedStepsW))
	copy(steps, lp.AllowedStepsW)
	sort.Float64s(steps)
	return State{
		ID:                 lp.ID,
		DriverName:         lp.DriverName,
		PluggedIn:          lp.pluggedIn,
		CurrentSoCPct:      lp.currentSoCPct,
		CurrentPowerW:      lp.currentPowerW,
		DeliveredWhSession: lp.deliveredWhSession,
		TargetSoCPct:       lp.targetSoCPct,
		TargetTime:         lp.targetTime,
		UpdatedAtMs:        lp.updatedAtMs,
		MinChargeW:         lp.MinChargeW,
		MaxChargeW:         lp.MaxChargeW,
		AllowedStepsW:      steps,
	}
}
