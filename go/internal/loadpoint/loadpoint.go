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

	// VehicleDriver is the optional name of a DerVehicle-emitting
	// driver (tesla_vehicle.lua + a TeslaBLEProxy today; future
	// OEM drivers possible) whose readings should override the
	// PluginSoCPct inference with the vehicle's real BMS SoC and
	// expose charge_limit_pct in the UI. When empty, the manager
	// auto-discovers the plugged-in vehicle on each connected:
	// false→true transition by scoring every live DerVehicle
	// reading — leave blank for multi-car households. Set explicitly
	// to force a particular binding.
	VehicleDriver string `yaml:"vehicle_driver,omitempty" json:"vehicle_driver,omitempty"`

	// Auto-charge-on-plug-in. When Enabled and a plug-in transition
	// finds no existing target, the manager posts one automatically:
	// reach AutoChargeTargetSoCPct by AutoChargeTargetTimeLocal
	// (HH:MM in host timezone, next occurrence). If a vehicle driver
	// is bound AND reports a tighter charge_limit_soc, the target
	// uses min(AutoChargeTargetSoCPct, vehicle_limit). Existing
	// manual targets are respected (not overwritten).
	AutoChargeEnabled         bool    `yaml:"auto_charge_enabled,omitempty" json:"auto_charge_enabled,omitempty"`
	AutoChargeTargetSoCPct    float64 `yaml:"auto_charge_target_soc_pct,omitempty" json:"auto_charge_target_soc_pct,omitempty"`
	AutoChargeTargetTimeLocal string  `yaml:"auto_charge_target_time_local,omitempty" json:"auto_charge_target_time_local,omitempty"`

	// PhaseMode selects how the controller picks between 1Φ and 3Φ
	// delivery each tick. "3p" (default) and "1p" lock the install to
	// one mode and filter AllowedStepsW accordingly. "auto" lets the
	// controller switch based on the MPC budget, with a hysteresis
	// timer (MinPhaseHoldS) preventing flap. Empty == "3p" for
	// backward compatibility with pre-switching configs.
	PhaseMode string `yaml:"phase_mode,omitempty" json:"phase_mode,omitempty"`

	// PhaseSplitW is the wantW threshold below which "auto" picks 1Φ.
	// Zero defaults to 3680 W — the ceiling a 16 A fuse can sustain
	// on a single phase. Also used to classify AllowedStepsW entries:
	// steps ≤ split are 1Φ-eligible, > split are 3Φ-eligible.
	PhaseSplitW float64 `yaml:"phase_split_w,omitempty" json:"phase_split_w,omitempty"`

	// MinPhaseHoldS is the minimum dwell time before the controller
	// will flip phase again. Easee's cloud API + contactor transition
	// is not instantaneous (~5-10 s observed), and MPC slots can flap
	// across the split threshold on noisy wantW. Default 60 s.
	MinPhaseHoldS int `yaml:"min_phase_hold_s,omitempty" json:"min_phase_hold_s,omitempty"`
}

// SiteFuse describes the shared grid-boundary breaker in terms the
// loadpoint controller needs: max amps per phase (the rated trip
// current) and nominal voltage. Zero MaxAmps disables clamping —
// used in tests that don't care about the clamp.
type SiteFuse struct {
	MaxAmps float64
	Voltage float64
}

// PerPhaseMaxW is the maximum sustained power per phase under this
// fuse. 16 A @ 230 V = 3680 W. Multiply by phase count to get the
// total three-phase ceiling.
func (f SiteFuse) PerPhaseMaxW() float64 {
	v := f.Voltage
	if v <= 0 {
		v = 230
	}
	return f.MaxAmps * v
}

// phaseFor returns the phase count chosen for wantW given the mode
// and split threshold. "auto" below split → 1Φ, above → 3Φ. Unknown
// modes fall back to 3Φ for safety (the pre-switching default).
func phaseFor(mode string, wantW, splitW float64) int {
	if splitW <= 0 {
		splitW = 3680
	}
	switch mode {
	case "1p":
		return 1
	case "auto":
		if wantW < splitW {
			return 1
		}
		return 3
	default: // "", "3p"
		return 3
	}
}

// filterStepsByPhase narrows AllowedStepsW to only the entries that
// match the chosen phase count. The classifier is purely magnitude-
// based: step ≤ splitW → 1Φ, step > splitW → 3Φ. 0 (off) is always
// included. Returns nil when the input is nil so downstream
// SnapChargeW falls through to its continuous-passthrough behaviour.
func filterStepsByPhase(steps []float64, phases int, splitW float64) []float64 {
	if len(steps) == 0 {
		return nil
	}
	if splitW <= 0 {
		splitW = 3680
	}
	out := make([]float64, 0, len(steps))
	out = append(out, 0)
	for _, s := range steps {
		if s <= 0 {
			continue
		}
		if phases == 1 && s <= splitW {
			out = append(out, s)
		} else if phases == 3 && s > splitW {
			out = append(out, s)
		}
	}
	return out
}

// VehicleSample, VehicleTelemetryFunc, VehicleSnapshotFunc and
// VehiclePokeFunc live in vehicle_discovery.go alongside the
// scoring helper that consumes them.

// Policy captures the energy-source constraints the operator set on
// the active schedule. Stored per-schedule in state.db and lifted
// onto the runtime so dispatch + planner can consult it without an
// extra DB round-trip each tick. Zero-value is "unrestricted" (all
// sources allowed; no surplus-only clamp) — matches today's
// behaviour when nothing was explicitly configured.
type Policy struct {
	AllowGrid           bool `json:"allow_grid"`
	AllowBatterySupport bool `json:"allow_battery_support"`
	OnlySurplus         bool `json:"only_surplus"`
}

// DefaultPolicy is the permissive policy used when no schedule row
// exists yet. Equivalent to today's unrestricted behaviour.
func DefaultPolicy() Policy {
	return Policy{AllowGrid: true, AllowBatterySupport: true, OnlySurplus: false}
}

// State is the observable snapshot of one loadpoint at a point in time.
// Read-only for consumers — only the Manager or dispatch paths mutate
// it under lock.
type State struct {
	ID                 string    `json:"id"`
	DriverName         string    `json:"driver_name"`
	VehicleDriver      string    `json:"vehicle_driver,omitempty"`
	PluggedIn          bool      `json:"plugged_in"`
	CurrentSoCPct      float64   `json:"current_soc_pct"`       // effective (vehicle or inferred)
	CurrentPowerW      float64   `json:"current_power_w"`       // actual draw (site sign: + = charging)
	DeliveredWhSession float64   `json:"delivered_wh_session"`  // since plug-in
	TargetSoCPct       float64   `json:"target_soc_pct"`        // user intent
	TargetTime         time.Time `json:"target_time,omitempty"` // user intent
	Policy             Policy    `json:"policy"`
	// Vehicle-side telemetry (populated when a DerVehicle driver is
	// configured on VehicleDriver and emitting). Fields are zero when
	// unavailable. SoCSource is "vehicle" when CurrentSoCPct comes
	// from the car's BMS, "inferred" when it's the pluginSoC +
	// deliveredWh path, or "" when not plugged in.
	VehicleSoCPct         float64 `json:"vehicle_soc_pct,omitempty"`
	VehicleChargeLimitPct float64 `json:"vehicle_charge_limit_pct,omitempty"`
	VehicleChargingState  string  `json:"vehicle_charging_state,omitempty"`
	VehicleTimeToFullMin  int     `json:"vehicle_time_to_full_min,omitempty"`
	VehicleStale          bool    `json:"vehicle_stale,omitempty"`
	SoCSource             string  `json:"soc_source,omitempty"`
	UpdatedAtMs           int64   `json:"updated_at_ms"`
	// MinChargeW / MaxChargeW / AllowedStepsW are repeated here so the
	// UI has everything for rendering in one fetch.
	MinChargeW    float64   `json:"min_charge_w"`
	MaxChargeW    float64   `json:"max_charge_w"`
	AllowedStepsW []float64 `json:"allowed_steps_w,omitempty"`
}

// TargetPersister is the callback the manager invokes whenever a
// loadpoint target changes so the new intent survives restarts.
// Main wires this to state.Store.UpsertPrimaryLoadpointSchedule.
// nil is a valid value — in-memory-only mode (useful in tests).
type TargetPersister func(loadpointID string, socPct float64, targetTime time.Time, policy Policy) error

// Manager holds the running set of loadpoints. Thread-safe.
type Manager struct {
	mu     sync.RWMutex
	byID   map[string]*loadpointRuntime
	order  []string // insertion-preserving id list for deterministic listing
	// onTarget is invoked from SetTarget to persist user intent to
	// state.db. Failures are logged (by main.go's wrapper) but do not
	// prevent the in-memory state from advancing — the runtime
	// behaviour is unaffected, only persistence across restart is.
	onTarget TargetPersister
	// vehicleTel returns the most recent DerVehicle reading for a
	// driver name. Optional — when nil, every loadpoint falls back
	// to the pluginSoC + deliveredWh/capacity SoC inference even if
	// it has VehicleDriver configured. Wired by main.go.
	vehicleTel VehicleTelemetryFunc
	// vehicleSnap returns every currently-known vehicle reading
	// keyed by driver name. Used for plug-in auto-discovery. nil
	// disables it; YAML-configured VehicleDriver still works.
	vehicleSnap VehicleSnapshotFunc
	// vehiclePoke forces a fresh poll on a vehicle driver. Called
	// for every known vehicle driver on plug-in transition so the
	// auto-discovery scoring uses up-to-date BMS data.
	vehiclePoke VehiclePokeFunc
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
	policy             Policy
	// Vehicle telemetry, populated by Observe when VehicleDriver is
	// wired. socSource records which channel produced currentSoCPct
	// so the UI can distinguish measured vs inferred.
	vehicleSoCPct       float64
	vehicleChargeLimitPct float64
	vehicleChargingState  string
	vehicleTimeToFullMin  int
	vehicleStale        bool
	socSource           string
	// discoveredVehicleDriver holds the runtime auto-discovery result
	// when the YAML Config.VehicleDriver is empty. Set on plug-in
	// transition by scoring every live DerVehicle reading. Cleared
	// on unplug so a next plug-in re-runs discovery (multi-car
	// households). Does NOT survive restart — YAML override is the
	// only persistent binding today.
	discoveredVehicleDriver string
	updatedAtMs             int64

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

// SetTargetPersister installs the callback used to persist target
// changes. nil clears it (in-memory-only). Safe to call at any time,
// including after loadpoints have been Loaded.
func (m *Manager) SetTargetPersister(p TargetPersister) {
	m.mu.Lock()
	m.onTarget = p
	m.mu.Unlock()
}

// SetVehicleTelemetry installs the getter used to fetch vehicle-side
// readings (SoC, charge limit) from the DerVehicle telemetry store.
// nil clears it — manager reverts to the inferred-SoC-only path.
func (m *Manager) SetVehicleTelemetry(f VehicleTelemetryFunc) {
	m.mu.Lock()
	m.vehicleTel = f
	m.mu.Unlock()
}

// SetVehicleSnapshot installs the getter used for plug-in auto-
// discovery. nil disables the feature.
func (m *Manager) SetVehicleSnapshot(f VehicleSnapshotFunc) {
	m.mu.Lock()
	m.vehicleSnap = f
	m.mu.Unlock()
}

// SetVehiclePoke installs the callback used to force fresh polls on
// vehicle drivers when an EV plug-in transition fires. nil leaves
// the manager relying on the next regular poll cycle (up to 60 s
// stale on startup-without-recent-poll).
func (m *Manager) SetVehiclePoke(f VehiclePokeFunc) {
	m.mu.Lock()
	m.vehiclePoke = f
	m.mu.Unlock()
}

// RestoreTarget seeds a target + policy without invoking the
// persistence callback — used at startup when rehydrating from
// state.db so we don't write back what we just read. socPct and
// targetTime are the canonical "what the user last asked for"
// values; policy is the schedule's active energy-source flags.
func (m *Manager) RestoreTarget(id string, socPct float64, targetTime time.Time, policy Policy) bool {
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
	lp.policy = policy
	return true
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
		lp := &loadpointRuntime{Config: c, policy: DefaultPolicy()}
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
			lp.policy = existing.policy
			lp.vehicleSoCPct = existing.vehicleSoCPct
			lp.vehicleChargeLimitPct = existing.vehicleChargeLimitPct
			lp.vehicleChargingState = existing.vehicleChargingState
			lp.vehicleTimeToFullMin = existing.vehicleTimeToFullMin
			lp.vehicleStale = existing.vehicleStale
			lp.socSource = existing.socSource
			lp.discoveredVehicleDriver = existing.discoveredVehicleDriver
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

// Configs returns a snapshot of the currently-configured loadpoints
// in insertion order. Used by Controller.Tick to drive dispatch
// without needing a second copy of the YAML source of truth — the
// manager is already the authoritative in-memory view after Load().
func (m *Manager) Configs() []Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Config, 0, len(m.order))
	for _, id := range m.order {
		if lp, ok := m.byID[id]; ok {
			out = append(out, lp.Config)
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
	lp, ok := m.byID[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	var pluginTransition, unplugTransition bool
	if pluggedIn && !lp.pluggedIn {
		// Plug-in transition: seed the session anchor + arm the
		// auto-discovery + auto-schedule flags.
		anchor := lp.PluginSoCPct
		if anchor <= 0 {
			anchor = 20 // conservative default
		}
		lp.sessionPluginSoCPct = anchor
		pluginTransition = true
		// Clear stale runtime binding from the previous session so a
		// different car plugging in re-runs discovery.
		lp.discoveredVehicleDriver = ""
	}
	if !pluggedIn && lp.pluggedIn {
		// Unplug transition. The current schedule belonged to the car
		// that just left — clear it so the NEXT plug-in (potentially a
		// different vehicle) starts from "no schedule" and the
		// auto-charge logic re-fires from scratch. The cleared target
		// is persisted via the same TargetPersister callback so the
		// dashboard reflects the empty state immediately.
		unplugTransition = true
		lp.targetSoCPct = 0
		lp.targetTime = time.Time{}
	}
	if !pluggedIn {
		// Fully clear the runtime binding on unplug so the NEXT
		// plug-in re-discovers. YAML binding is untouched.
		lp.discoveredVehicleDriver = ""
	}
	lp.pluggedIn = pluggedIn
	lp.currentPowerW = powerW
	lp.deliveredWhSession = deliveredWh

	// Auto-discovery: fire on plug-in transition, AND on every tick
	// when the loadpoint is plugged but has no binding yet (covers
	// the "container restarted while plugged" / "operator just
	// removed YAML binding without re-plugging" cases). YAML
	// binding always wins; we only auto-discover when both YAML and
	// previous runtime binding are empty.
	wantsDiscovery := lp.VehicleDriver == "" &&
		lp.discoveredVehicleDriver == "" &&
		pluggedIn && m.vehicleSnap != nil
	if wantsDiscovery {
		snap := m.vehicleSnap()
		// Poke once on plug-in transition so subsequent re-discovery
		// ticks see fresh data; don't spam pokes every observe.
		if pluginTransition && m.vehiclePoke != nil {
			for driverName := range snap {
				m.vehiclePoke(driverName)
			}
		}
		lp.discoveredVehicleDriver = pickPluggedVehicle(snap)
	}

	// Resolve the effective vehicle driver. YAML wins; runtime
	// discovery fills in when YAML is empty.
	effectiveDriver := lp.VehicleDriver
	if effectiveDriver == "" {
		effectiveDriver = lp.discoveredVehicleDriver
	}

	// Vehicle-side telemetry overrides the inferred SoC path when
	// available and fresh. Even stale readings are stored so the UI
	// can render the last-known value with a "stale" indicator —
	// better than a sudden blank. The freshness flag is already set
	// by the driver (stale_after_s elapsed since last successful
	// HTTP refresh). A nil getter → same as empty VehicleDriver:
	// fall back to the inferred path.
	var vs VehicleSample
	if effectiveDriver != "" && m.vehicleTel != nil {
		vs = m.vehicleTel(effectiveDriver)
	}
	lp.vehicleSoCPct = vs.SoCPct
	lp.vehicleChargeLimitPct = vs.ChargeLimitPct
	lp.vehicleChargingState = vs.ChargingState
	lp.vehicleTimeToFullMin = vs.TimeToFullMin
	lp.vehicleStale = vs.Stale

	switch {
	case !pluggedIn:
		lp.currentSoCPct = 0
		lp.socSource = ""
	case vs.OK && !vs.Stale && vs.SoCPct > 0:
		// Trust the vehicle's BMS over our delivered-Wh inference.
		lp.currentSoCPct = vs.SoCPct
		lp.socSource = "vehicle"
	default:
		lp.currentSoCPct = estimateSoCPct(lp.sessionPluginSoCPct,
			deliveredWh, lp.VehicleCapacityWh)
		lp.socSource = "inferred"
	}
	lp.updatedAtMs = time.Now().UnixMilli()

	// Capture everything we need for the optional auto-schedule below
	// BEFORE releasing the lock. SetTarget re-acquires it, so we must
	// drop ours first to avoid reentrant-lock deadlock.
	autoArm := pluginTransition && lp.AutoChargeEnabled && lp.targetSoCPct <= 0
	lpID := lp.ID
	autoSoc := lp.AutoChargeTargetSoCPct
	autoTimeLocal := lp.AutoChargeTargetTimeLocal
	vehLimit := lp.vehicleChargeLimitPct
	policySnap := lp.policy
	persist := m.onTarget
	m.mu.Unlock()

	if unplugTransition && persist != nil {
		// Persist the cleared target so the schedule is gone from
		// state.db too. Reuses the standard TargetPersister callback
		// (state.UpsertPrimaryLoadpointSchedule via main.go); a 0
		// soc + zero target_time is treated as "no schedule" by every
		// downstream consumer.
		_ = persist(lpID, 0, time.Time{}, policySnap)
	}
	if autoArm {
		m.applyAutoSchedule(lpID, autoSoc, autoTimeLocal, vehLimit)
	}
}

// applyAutoSchedule, nextLocalTimeOfDay and pickPluggedVehicle now
// live in auto_charge.go and vehicle_discovery.go respectively.

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

// SetTarget updates target + optionally policy for an existing
// loadpoint. A nil policy preserves the loadpoint's current policy
// (used by the legacy endpoint that only sends soc + target_time).
// Returns false for unknown IDs. Invokes the persistence callback
// (if any); failures are swallowed — in-memory state advances
// regardless so dispatch keeps working even when the DB is readonly.
func (m *Manager) SetTarget(id string, socPct float64, targetTime time.Time, policy *Policy) bool {
	m.mu.Lock()
	lp, ok := m.byID[id]
	if !ok {
		m.mu.Unlock()
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
	if policy != nil {
		lp.policy = *policy
	}
	effectivePolicy := lp.policy
	onTarget := m.onTarget
	m.mu.Unlock()
	if onTarget != nil {
		_ = onTarget(id, socPct, targetTime, effectivePolicy)
	}
	return true
}

// SetCurrentSoC lets an operator correct the inferred vehicle SoC
// mid-session. Chargers like Easee don't report the vehicle's actual
// BMS state, so the manager defaults to
// `plugin_soc_pct + session_wh / capacity` — which drifts if the
// plug-in anchor was wrong. This resets the session anchor so the
// CURRENT estimate equals `socPct` and future observations accumulate
// from there. Only applies while plugged in; no-op otherwise.
//
// Returns false for unknown IDs or when the loadpoint is unplugged.
func (m *Manager) SetCurrentSoC(id string, socPct float64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	lp, ok := m.byID[id]
	if !ok {
		return false
	}
	if !lp.pluggedIn {
		return false
	}
	if socPct < 0 {
		socPct = 0
	}
	if socPct > 100 {
		socPct = 100
	}
	// Re-anchor: new_anchor + delivered/capacity*100 == socPct.
	// → new_anchor = socPct − delivered/capacity*100
	deliveredPct := 0.0
	if lp.VehicleCapacityWh > 0 {
		deliveredPct = lp.deliveredWhSession / lp.VehicleCapacityWh * 100.0
	}
	anchor := socPct - deliveredPct
	if anchor < 0 {
		anchor = 0
	}
	if anchor > 100 {
		anchor = 100
	}
	lp.sessionPluginSoCPct = anchor
	lp.currentSoCPct = estimateSoCPct(anchor, lp.deliveredWhSession, lp.VehicleCapacityWh)
	lp.updatedAtMs = time.Now().UnixMilli()
	return true
}

func (lp *loadpointRuntime) snapshot() State {
	steps := make([]float64, len(lp.AllowedStepsW))
	copy(steps, lp.AllowedStepsW)
	sort.Float64s(steps)
	// Effective vehicle driver: YAML wins, else runtime-discovered.
	effectiveDriver := lp.VehicleDriver
	if effectiveDriver == "" {
		effectiveDriver = lp.discoveredVehicleDriver
	}
	return State{
		ID:                    lp.ID,
		DriverName:            lp.DriverName,
		VehicleDriver:         effectiveDriver,
		PluggedIn:             lp.pluggedIn,
		CurrentSoCPct:         lp.currentSoCPct,
		CurrentPowerW:         lp.currentPowerW,
		DeliveredWhSession:    lp.deliveredWhSession,
		TargetSoCPct:          lp.targetSoCPct,
		TargetTime:            lp.targetTime,
		Policy:                lp.policy,
		VehicleSoCPct:         lp.vehicleSoCPct,
		VehicleChargeLimitPct: lp.vehicleChargeLimitPct,
		VehicleChargingState:  lp.vehicleChargingState,
		VehicleTimeToFullMin:  lp.vehicleTimeToFullMin,
		VehicleStale:          lp.vehicleStale,
		SoCSource:             lp.socSource,
		UpdatedAtMs:           lp.updatedAtMs,
		MinChargeW:            lp.MinChargeW,
		MaxChargeW:            lp.MaxChargeW,
		AllowedStepsW:         steps,
	}
}
