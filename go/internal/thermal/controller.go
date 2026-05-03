package thermal

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"
)

// SenderFunc forwards a JSON command payload to a driver. Matches
// drivers.Registry.Send so the loadpoint package's adapter pattern
// can be reused — main.go wires this to reg.Send.
type SenderFunc func(ctx context.Context, driver string, payload []byte) error

// Controller owns the per-driver block schedules and emits
// commands every tick. One Controller serves all configured
// thermal drivers; concurrency-safe so SetSchedule (called from
// the MPC replan goroutine) can race with Tick (called from the
// dispatch goroutine).
//
// State machine per driver: at each tick the controller looks up
// what BlockW the current schedule says. If it differs from the
// last command sent, it sends a new `{action:"battery", power_w:-W}`
// command to the driver and remembers the new value. This means a
// drifting block-W (partial-budget slots) re-issues every tick
// the value changes, which Lua drivers de-bounce internally.
type Controller struct {
	mu        sync.Mutex
	send      SenderFunc
	schedules map[string][]SlotBlock // driver name → ordered slots
	lastCmd   map[string]float64     // driver name → last commanded block-W
}

// NewController returns an empty controller. Wire schedules with
// SetSchedule and start ticking via Tick. Passing nil send
// disables actuation — useful in tests that want to inspect
// what would have been sent without forwarding to drivers.
func NewController(send SenderFunc) *Controller {
	return &Controller{
		send:      send,
		schedules: map[string][]SlotBlock{},
		lastCmd:   map[string]float64{},
	}
}

// SetSchedule replaces the schedule for one driver. Called from
// the MPC replan callback whenever a new price forecast lands.
// Empty/nil schedules clear the schedule (and the next Tick will
// release any held block).
func (c *Controller) SetSchedule(driver string, schedule []SlotBlock) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(schedule) == 0 {
		delete(c.schedules, driver)
	} else {
		c.schedules[driver] = schedule
	}
}

// Tick runs one dispatch cycle: for each scheduled driver, look
// up the current slot's BlockW and send a command if it differs
// from the last value commanded. Idempotent — calling Tick twice
// in the same moment produces zero or one command (the first
// changes lastCmd; the second sees no change).
func (c *Controller) Tick(ctx context.Context, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	// Snapshot the schedule list under the lock so we don't hold
	// it across the c.send call (which can block on network I/O).
	snapshot := make(map[string][]SlotBlock, len(c.schedules))
	for k, v := range c.schedules {
		snapshot[k] = v
	}
	c.mu.Unlock()

	for driver, sched := range snapshot {
		blockW := BlockAt(sched, now)
		c.mu.Lock()
		lastW, hadLast := c.lastCmd[driver]
		c.mu.Unlock()

		// First-tick or change → send. Threshold of 1 W avoids
		// re-emitting commands for floating-point dither in the
		// partial-budget case.
		if hadLast && abs(lastW-blockW) < 1.0 {
			continue
		}

		// Driver expects negative power_w to mean "discharge"
		// (i.e. block); positive would mean charge which thermal
		// drivers reject. 0 = release any held block.
		var cmdW float64
		if blockW > 0 {
			cmdW = -blockW
		}
		payload, err := json.Marshal(map[string]any{
			"action":  "battery",
			"power_w": cmdW,
		})
		if err != nil {
			continue
		}
		if c.send != nil {
			if err := c.send(ctx, driver, payload); err != nil {
				slog.Warn("thermal dispatch", "driver", driver, "err", err)
				continue
			}
		}
		c.mu.Lock()
		c.lastCmd[driver] = blockW
		c.mu.Unlock()
		slog.Info("thermal block command",
			"driver", driver, "block_w", blockW, "cmd_w", cmdW)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
