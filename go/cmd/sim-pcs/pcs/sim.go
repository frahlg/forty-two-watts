// Package pcs simulates a multi-rack commercial battery plant: N
// PCS/rack units, each an independent Modbus unit ID with its own SoC,
// power lag, and injectable faults. It exists so go/internal/plant and
// the e2e suite can exercise per-unit allocation, derating and
// comms-loss handling without hardware.
package pcs

import (
	"math"
	"sync"
	"time"
)

// Register map, per rack (holding registers; all multi-register values
// big-endian, SunSpec-style). One rack = one Modbus unit ID (1..N).
//
//	100     status: 0=off 1=standby 2=running 3=fault
//	101     soc ×100 (0..10000)
//	102-103 int32  actual power W (site convention: + charge, − discharge)
//	104-105 uint32 usable capacity Wh
//	106-107 int32  power setpoint W (writable)
//	108-109 uint32 max charge W
//	110-111 uint32 max discharge W
//	112     heartbeat (increments every tick while healthy)
const (
	RegStatus       = 100
	RegSoC          = 101
	RegPower        = 102
	RegCapacity     = 104
	RegSetpoint     = 106
	RegMaxCharge    = 108
	RegMaxDischarge = 110
	RegHeartbeat    = 112

	RegionStart = 100
	RegionLen   = 13
)

// Rack status values.
const (
	StatusOff     = 0
	StatusStandby = 1
	StatusRunning = 2
	StatusFault   = 3
)

// RackConfig sizes one simulated rack.
type RackConfig struct {
	CapacityWh    float64
	MaxChargeW    float64
	MaxDischargeW float64
	InitialSoC    float64 // 0..1
	// ResponseTau is the first-order lag toward the setpoint.
	ResponseTau time.Duration
	// Efficiency applies on both charge and discharge (one-way).
	Efficiency float64
}

func DefaultRack() RackConfig {
	return RackConfig{
		CapacityWh:    50_000,
		MaxChargeW:    25_000,
		MaxDischargeW: 25_000,
		InitialSoC:    0.5,
		ResponseTau:   2 * time.Second,
		Efficiency:    0.96,
	}
}

// Rack is one simulated PCS unit.
type Rack struct {
	mu sync.Mutex

	cfg       RackConfig
	soc       float64
	powerW    float64 // actual, site convention
	setpointW float64
	faulted   bool
	commsLost bool
	heartbeat uint16
}

func NewRack(cfg RackConfig) *Rack {
	if cfg.Efficiency <= 0 || cfg.Efficiency > 1 {
		cfg.Efficiency = 0.96
	}
	if cfg.ResponseTau <= 0 {
		cfg.ResponseTau = 2 * time.Second
	}
	return &Rack{cfg: cfg, soc: clamp01(cfg.InitialSoC)}
}

// Tick advances the physics: first-order power response toward the
// setpoint, SoC integration with efficiency, and SoC-bound cutoffs.
func (r *Rack) Tick(dt time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.faulted {
		r.powerW = 0
		return
	}
	r.heartbeat++

	target := r.setpointW
	if target > r.cfg.MaxChargeW {
		target = r.cfg.MaxChargeW
	}
	if target < -r.cfg.MaxDischargeW {
		target = -r.cfg.MaxDischargeW
	}
	// SoC cutoffs: a full rack refuses charge, an empty one discharge.
	if r.soc >= 1 && target > 0 {
		target = 0
	}
	if r.soc <= 0 && target < 0 {
		target = 0
	}
	// First-order lag.
	alpha := 1 - math.Exp(-dt.Seconds()/r.cfg.ResponseTau.Seconds())
	r.powerW += (target - r.powerW) * alpha

	// Integrate SoC.
	dtH := dt.Hours()
	if r.cfg.CapacityWh > 0 {
		if r.powerW >= 0 {
			r.soc += r.powerW * dtH * r.cfg.Efficiency / r.cfg.CapacityWh
		} else {
			r.soc += r.powerW * dtH / r.cfg.Efficiency / r.cfg.CapacityWh
		}
		r.soc = clamp01(r.soc)
	}
}

// SetSetpoint applies a written power setpoint (site convention W).
func (r *Rack) SetSetpoint(w float64) {
	r.mu.Lock()
	r.setpointW = w
	r.mu.Unlock()
}

// SetFault toggles a rack fault: output collapses to 0 and status reads
// fault; comms stay up (that's what SetCommsLost is for).
func (r *Rack) SetFault(on bool) {
	r.mu.Lock()
	r.faulted = on
	if on {
		r.powerW = 0
		r.setpointW = 0
	}
	r.mu.Unlock()
}

// SetCommsLost makes the rack's unit ID stop answering Modbus entirely.
func (r *Rack) SetCommsLost(on bool) {
	r.mu.Lock()
	r.commsLost = on
	r.mu.Unlock()
}

// SetSoC pins the state of charge (test hook).
func (r *Rack) SetSoC(soc float64) {
	r.mu.Lock()
	r.soc = clamp01(soc)
	r.mu.Unlock()
}

// Snapshot returns the rack's externally visible state.
type RackState struct {
	Status    uint16
	SoC       float64
	PowerW    float64
	SetpointW float64
	Faulted   bool
	CommsLost bool
	Heartbeat uint16
	Config    RackConfig
}

func (r *Rack) Snapshot() RackState {
	r.mu.Lock()
	defer r.mu.Unlock()
	status := uint16(StatusRunning)
	if r.faulted {
		status = StatusFault
	} else if math.Abs(r.powerW) < 1 {
		status = StatusStandby
	}
	return RackState{
		Status: status, SoC: r.soc, PowerW: r.powerW, SetpointW: r.setpointW,
		Faulted: r.faulted, CommsLost: r.commsLost, Heartbeat: r.heartbeat,
		Config: r.cfg,
	}
}

// Registers renders the rack's register region (RegionStart..+RegionLen).
func (r *Rack) Registers() []uint16 {
	s := r.Snapshot()
	regs := make([]uint16, RegionLen)
	regs[RegStatus-RegionStart] = s.Status
	regs[RegSoC-RegionStart] = uint16(clamp01(s.SoC) * 10000)
	putI32(regs, RegPower-RegionStart, int32(s.PowerW))
	putU32(regs, RegCapacity-RegionStart, uint32(s.Config.CapacityWh))
	putI32(regs, RegSetpoint-RegionStart, int32(s.SetpointW))
	putU32(regs, RegMaxCharge-RegionStart, uint32(s.Config.MaxChargeW))
	putU32(regs, RegMaxDischarge-RegionStart, uint32(s.Config.MaxDischargeW))
	regs[RegHeartbeat-RegionStart] = s.Heartbeat
	return regs
}

func putU32(regs []uint16, at int, v uint32) {
	regs[at] = uint16(v >> 16)
	regs[at+1] = uint16(v)
}

func putI32(regs []uint16, at int, v int32) {
	putU32(regs, at, uint32(v))
}

// DecodeI32 reads a big-endian int32 from two registers.
func DecodeI32(hi, lo uint16) int32 {
	return int32(uint32(hi)<<16 | uint32(lo))
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
