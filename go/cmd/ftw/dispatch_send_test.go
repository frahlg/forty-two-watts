package main

import (
	"context"
	"errors"
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
