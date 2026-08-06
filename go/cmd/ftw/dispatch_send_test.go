package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// blockingSender blocks every Send until its context expires, like a driver
// wedged mid-poll whose cmdCh is full.
type blockingSender struct {
	sawDeadline chan bool
}

func (b *blockingSender) Send(ctx context.Context, name string, payload []byte) error {
	_, ok := ctx.Deadline()
	b.sawDeadline <- ok
	<-ctx.Done()
	return ctx.Err()
}

func TestSendDriverCommandBoundsBlockedDriver(t *testing.T) {
	s := &blockingSender{sawDeadline: make(chan bool, 1)}
	done := make(chan struct{})
	start := time.Now()
	go func() {
		sendDriverCommand(context.Background(), s, "stuck", "driver", []byte(`{"action":"battery","power_w":0}`))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(driverCommandTimeout + 2*time.Second):
		t.Fatalf("sendDriverCommand did not return; blocked driver stalls dispatch")
	}
	if elapsed := time.Since(start); elapsed < driverCommandTimeout/2 {
		t.Fatalf("returned after %v, before the driver had its full window", elapsed)
	}
	if !<-s.sawDeadline {
		t.Fatal("Send received a context without a deadline")
	}
}

type recordingSender struct {
	name    string
	payload []byte
	err     error
}

func (r *recordingSender) Send(ctx context.Context, name string, payload []byte) error {
	r.name = name
	r.payload = append([]byte(nil), payload...)
	return r.err
}

func TestSendDriverCommandPassesThrough(t *testing.T) {
	s := &recordingSender{}
	sendDriverCommand(context.Background(), s, "bat1", "driver", []byte(`{"action":"battery","power_w":1500}`))
	if s.name != "bat1" {
		t.Fatalf("sent to %q, want bat1", s.name)
	}
	if string(s.payload) != `{"action":"battery","power_w":1500}` {
		t.Fatalf("payload = %s", s.payload)
	}
}

func TestSendDriverCommandLogsButSwallowsErrors(t *testing.T) {
	s := &recordingSender{err: errors.New("boom")}
	// Must not panic or propagate: recovery belongs to watchdog/staleness.
	sendDriverCommand(context.Background(), s, "bat1", "driver", []byte(`{}`))
}

// slowSender delays each Send by a fixed amount (or until ctx expires) and
// records which drivers were reached.
type slowSender struct {
	delay time.Duration

	mu   sync.Mutex
	seen []string
}

func (s *slowSender) Send(ctx context.Context, name string, payload []byte) error {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	s.seen = append(s.seen, name)
	s.mu.Unlock()
	return nil
}

func TestDispatchCommandsRunsConcurrently(t *testing.T) {
	s := &slowSender{delay: 200 * time.Millisecond}
	sends := []driverSend{
		{name: "a", kind: "driver", payload: []byte(`{}`)},
		{name: "b", kind: "driver", payload: []byte(`{}`)},
		{name: "c", kind: "driver", payload: []byte(`{}`)},
		{name: "d", kind: "driver", payload: []byte(`{}`)},
	}
	start := time.Now()
	dispatchCommands(context.Background(), s, sends)
	elapsed := time.Since(start)
	// Serial would take 4×200 ms; concurrent should be close to one delay.
	if elapsed > 3*s.delay {
		t.Fatalf("dispatch took %v; sends appear serialized", elapsed)
	}
	if len(s.seen) != len(sends) {
		t.Fatalf("reached %d drivers, want %d", len(s.seen), len(sends))
	}
}

func TestDispatchCommandsHonorsTickBudget(t *testing.T) {
	// Delay far beyond both the per-command timeout and the tick budget.
	s := &slowSender{delay: time.Minute}
	sends := []driverSend{
		{name: "stuck1", kind: "driver", payload: []byte(`{}`)},
		{name: "stuck2", kind: "driver", payload: []byte(`{}`)},
	}
	start := time.Now()
	dispatchCommands(context.Background(), s, sends)
	elapsed := time.Since(start)
	if elapsed > dispatchTickBudget+time.Second {
		t.Fatalf("dispatch took %v, budget is %v", elapsed, dispatchTickBudget)
	}
}

func TestDispatchCommandsEmptyIsNoop(t *testing.T) {
	dispatchCommands(context.Background(), &recordingSender{}, nil)
}
