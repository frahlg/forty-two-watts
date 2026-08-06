package plant

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Config sizes the plant controller.
type Config struct {
	Units []UnitConfig `yaml:"units" json:"units"`
	// PollInterval is the per-unit Modbus poll cadence. Default 1 s.
	PollInterval time.Duration
	// ControlInterval is how often allocation is recomputed and written.
	// Default 1 s.
	ControlInterval time.Duration
	// DefaultLeaseTTL bounds a setpoint that arrives without an explicit
	// TTL. Default 10 s — five core control ticks.
	DefaultLeaseTTL time.Duration
	// StaleAfter marks a unit offline when its last successful poll is
	// older than this. Default 5 s.
	StaleAfter time.Duration
}

func (c Config) withDefaults() Config {
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.ControlInterval <= 0 {
		c.ControlInterval = time.Second
	}
	if c.DefaultLeaseTTL <= 0 {
		c.DefaultLeaseTTL = 10 * time.Second
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = 5 * time.Second
	}
	return c
}

// pollable abstracts *Unit for tests.
type pollable interface {
	Poll() error
	WriteSetpoint(w float64) error
	State() (UnitState, time.Time, error)
}

// Controller owns the poll loops, the setpoint lease, and allocation.
type Controller struct {
	cfg   Config
	units []pollable

	mu           sync.Mutex
	targetW      float64
	leaseExpires time.Time
	lastWritten  map[string]float64
}

func NewController(cfg Config) *Controller {
	cfg = cfg.withDefaults()
	c := &Controller{cfg: cfg, lastWritten: map[string]float64{}}
	for _, uc := range cfg.Units {
		c.units = append(c.units, NewUnit(uc))
	}
	return c
}

// newControllerWithUnits is the test seam.
func newControllerWithUnits(cfg Config, units []pollable) *Controller {
	c := &Controller{cfg: cfg.withDefaults(), lastWritten: map[string]float64{}}
	c.units = units
	return c
}

// SetTarget applies an aggregate setpoint under a lease. ttl <= 0 uses
// the default. Returns the lease expiry.
func (c *Controller) SetTarget(w float64, ttl time.Duration) time.Time {
	if ttl <= 0 {
		ttl = c.cfg.DefaultLeaseTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.targetW = w
	c.leaseExpires = time.Now().Add(ttl)
	return c.leaseExpires
}

// Run blocks, driving poll and control loops until ctx is done. On exit
// every unit is commanded to zero — the plant never keeps moving power
// after its controller stops.
func (c *Controller) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, u := range c.units {
		wg.Add(1)
		go func(u pollable) {
			defer wg.Done()
			t := time.NewTicker(c.cfg.PollInterval)
			defer t.Stop()
			for {
				if err := u.Poll(); err != nil {
					slog.Debug("plant unit poll", "err", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}(u)
	}

	t := time.NewTicker(c.cfg.ControlInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			c.rampAllToZero()
			wg.Wait()
			return
		case <-t.C:
			c.step(time.Now())
		}
	}
}

// step recomputes allocation and writes changed setpoints. Exported to
// the tests via the lowercase seam; Run is just the ticker around it.
func (c *Controller) step(now time.Time) {
	states := c.unitStates(now)

	c.mu.Lock()
	target := c.targetW
	if now.After(c.leaseExpires) {
		// Lease expired: core stopped talking (or never granted one).
		// The safe state for a grid-scale battery is zero power — the
		// racks' own BMS handles everything below that.
		target = 0
		c.targetW = 0
	}
	c.mu.Unlock()

	alloc := Allocate(states, target)
	for _, u := range c.units {
		st, _, _ := u.State()
		want := alloc[st.ID]
		c.mu.Lock()
		last, had := c.lastWritten[st.ID]
		c.mu.Unlock()
		// Re-write on any change ≥1 W, and periodically re-assert zero
		// on units we've never written (fresh connection).
		if had && absDiff(last, want) < 1 {
			continue
		}
		if !st.Online && want == 0 {
			continue // no point poking a dead unit with zeros
		}
		if err := u.WriteSetpoint(want); err != nil {
			slog.Warn("plant setpoint write", "unit", st.ID, "w", want, "err", err)
			continue
		}
		c.mu.Lock()
		c.lastWritten[st.ID] = want
		c.mu.Unlock()
	}
}

func (c *Controller) rampAllToZero() {
	for _, u := range c.units {
		_ = u.WriteSetpoint(0)
	}
}

func (c *Controller) unitStates(now time.Time) []UnitState {
	out := make([]UnitState, 0, len(c.units))
	for _, u := range c.units {
		st, seen, _ := u.State()
		if now.Sub(seen) > c.cfg.StaleAfter {
			st.Online = false
			st.PowerW = 0
		}
		out = append(out, st)
	}
	return out
}

// Status is the /v1/status document.
type Status struct {
	SchemaVersion int         `json:"schema_version"`
	Features      []string    `json:"features"`
	Aggregate     Aggregate   `json:"aggregate"`
	TargetW       float64     `json:"target_w"`
	LeaseExpires  *time.Time  `json:"lease_expires,omitempty"`
	Units         []UnitState `json:"units"`
}

// Status snapshots the plant for the driver.
func (c *Controller) Status(now time.Time) Status {
	states := c.unitStates(now)
	c.mu.Lock()
	target := c.targetW
	var lease *time.Time
	if !c.leaseExpires.IsZero() && now.Before(c.leaseExpires) {
		le := c.leaseExpires
		lease = &le
	} else {
		target = 0
	}
	c.mu.Unlock()
	return Status{
		SchemaVersion: 1,
		Features:      []string{},
		Aggregate:     Summarize(states),
		TargetW:       target,
		LeaseExpires:  lease,
		Units:         states,
	}
}

func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}
