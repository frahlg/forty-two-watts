package main

import (
	"context"
	"log/slog"
	"time"
)

// driverCommandTimeout bounds one control-loop command delivery. Registry.Send
// blocks until the driver's runLoop processes the payload, so without a
// deadline a single driver wedged mid-poll would stall dispatch to every
// other driver for the rest of the tick (and beyond). 2 s matches
// driverDefaultTimeout: the same driver-side work is being waited on, and
// anything slower is indistinguishable from a stuck driver at the default
// 2 s control interval.
const driverCommandTimeout = 2 * time.Second

// commandSender is the slice of drivers.Registry the dispatch path needs.
type commandSender interface {
	Send(ctx context.Context, name string, payload []byte) error
}

// sendDriverCommand delivers one dispatch payload with a bounded deadline.
// Errors (including deadline expiry) are logged, not returned: the control
// loop treats a failed send like any other driver hiccup — the watchdog and
// staleness paths own recovery.
func sendDriverCommand(ctx context.Context, reg commandSender, name, kind string, payload []byte) {
	cmdCtx, cancel := context.WithTimeout(ctx, driverCommandTimeout)
	defer cancel()
	if err := reg.Send(cmdCtx, name, payload); err != nil {
		slog.Warn(kind+" send", "name", name, "timeout", driverCommandTimeout, "err", err)
	}
}
