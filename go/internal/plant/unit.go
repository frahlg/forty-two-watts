package plant

import (
	"fmt"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/modbus"
)

// Register map shared with cmd/sim-pcs (SunSpec-style, big-endian).
// Real PCS hardware gets its own map via UnitConfig register overrides
// in a later iteration; the layout below is the reference contract.
const (
	regStatus       = 100
	regSoC          = 101
	regPower        = 102
	regCapacity     = 104
	regSetpoint     = 106
	regMaxCharge    = 108
	regMaxDischarge = 110
	regionStart     = 100
	regionLen       = 13

	statusFault = 3
)

// UnitConfig identifies one PCS/rack.
type UnitConfig struct {
	ID     string `yaml:"id" json:"id"`
	Host   string `yaml:"host" json:"host"`
	Port   int    `yaml:"port" json:"port"`
	UnitID int    `yaml:"unit_id" json:"unit_id"`
}

// modbusConn is the slice of modbus.Capability the unit needs; a seam
// for tests.
type modbusConn interface {
	Read(addr, count uint16, kind int32) ([]uint16, error)
	WriteMulti(addr uint16, values []uint16) error
	Close() error
}

// Unit polls one rack and writes its setpoint.
type Unit struct {
	Cfg UnitConfig

	mu       sync.Mutex
	conn     modbusConn
	dial     func() (modbusConn, error)
	state    UnitState
	lastSeen time.Time
	lastErr  error
}

// NewUnit builds a unit with the production Modbus dialer.
func NewUnit(cfg UnitConfig) *Unit {
	u := &Unit{Cfg: cfg}
	u.dial = func() (modbusConn, error) {
		return modbus.Dial(cfg.Host, cfg.Port, cfg.UnitID)
	}
	u.state = UnitState{ID: cfg.ID}
	return u
}

// Poll reads the rack's register region and refreshes the unit state.
// Any error marks the unit offline — the allocator then derates around
// it and its share flows to healthy siblings.
func (u *Unit) Poll() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.conn == nil {
		conn, err := u.dial()
		if err != nil {
			return u.failLocked(fmt.Errorf("dial: %w", err))
		}
		u.conn = conn
	}
	regs, err := u.conn.Read(regionStart, regionLen, 3)
	if err != nil {
		// Drop the connection so the next poll re-dials cleanly.
		_ = u.conn.Close()
		u.conn = nil
		return u.failLocked(fmt.Errorf("read: %w", err))
	}
	if len(regs) < regionLen {
		return u.failLocked(fmt.Errorf("short read: %d regs", len(regs)))
	}
	at := func(reg int) uint16 { return regs[reg-regionStart] }
	i32 := func(reg int) int32 {
		return int32(uint32(at(reg))<<16 | uint32(at(reg+1)))
	}
	status := at(regStatus)
	u.state = UnitState{
		ID:            u.Cfg.ID,
		Online:        status != statusFault,
		SoC:           float64(at(regSoC)) / 10000,
		CapacityWh:    float64(uint32(i32(regCapacity))),
		MaxChargeW:    float64(uint32(i32(regMaxCharge))),
		MaxDischargeW: float64(uint32(i32(regMaxDischarge))),
		PowerW:        float64(i32(regPower)),
	}
	u.lastSeen = time.Now()
	u.lastErr = nil
	return nil
}

func (u *Unit) failLocked(err error) error {
	u.state.Online = false
	u.state.PowerW = 0
	u.lastErr = err
	return err
}

// WriteSetpoint sends a power target (site convention W) to the rack.
func (u *Unit) WriteSetpoint(w float64) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.conn == nil {
		return fmt.Errorf("unit %s offline", u.Cfg.ID)
	}
	v := uint32(int32(w))
	err := u.conn.WriteMulti(regSetpoint, []uint16{uint16(v >> 16), uint16(v)})
	if err != nil {
		_ = u.conn.Close()
		u.conn = nil
		u.state.Online = false
		u.lastErr = err
	}
	return err
}

// State returns the last polled view plus health metadata.
func (u *Unit) State() (UnitState, time.Time, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.state, u.lastSeen, u.lastErr
}
