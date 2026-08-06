package main

import (
	"context"
	"log/slog"
	"sync"
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

// dispatchTickBudget caps one dispatch phase as a whole. Sends within a
// phase run concurrently (each driver has its own runLoop and command
// queue, so cross-driver parallelism is safe), but the phase must still
// fit inside the 2 s control tick with room for persistence — several
// drivers all hitting their individual 2 s deadline would otherwise be
// indistinguishable from a stalled tick.
const dispatchTickBudget = 1500 * time.Millisecond

type driverSend struct {
	name    string
	kind    string
	payload []byte
}

// dispatchCommands fans a phase's sends out concurrently and waits for
// all of them, bounded by dispatchTickBudget. Phase ordering is the
// caller's: battery targets and PV curtailment stay separate calls so
// their relative order is preserved.
func dispatchCommands(ctx context.Context, reg commandSender, sends []driverSend) {
	if len(sends) == 0 {
		return
	}
	budgetCtx, cancel := context.WithTimeout(ctx, dispatchTickBudget)
	defer cancel()
	var wg sync.WaitGroup
	for _, s := range sends {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			sendDriverCommand(budgetCtx, reg, s.name, s.kind, s.payload)
			slog.Debug("dispatch send", "name", s.name, "kind", s.kind, "elapsed", time.Since(start))
		}()
	}
	wg.Wait()
}
