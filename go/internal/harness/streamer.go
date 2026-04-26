package harness

import (
	"context"
	"encoding/json"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/drivers"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// streamer pulls from the in-memory telemetry store at a fixed cadence,
// diffs against last-seen state, and emits changed fields as events. The
// telemetry package exposes no push hook, but it does expose everything the
// streamer needs on the read path — and the test harness doesn't care about
// the extra 200 ms of latency, which is cheaper than patching every write
// site to fan out to a callback.
type streamer struct {
	out    *EventWriter
	tel    *telemetry.Store
	env    *drivers.HostEnv
	driver string

	lastReadingTS map[telemetry.DerType]time.Time
	lastIdentity  identitySnap
	lastTickCount uint64
	lastLastError string
	lastStatus    string
}

type identitySnap struct{ make, sn, mac, endpoint string }

func newStreamer(out *EventWriter, tel *telemetry.Store, env *drivers.HostEnv, driver string) *streamer {
	return &streamer{
		out:           out,
		tel:           tel,
		env:           env,
		driver:        driver,
		lastReadingTS: map[telemetry.DerType]time.Time{},
	}
}

const streamInterval = 200 * time.Millisecond

func (s *streamer) run(ctx context.Context) {
	t := time.NewTicker(streamInterval)
	defer t.Stop()
	s.tick() // emit initial identity/health snapshot immediately
	for {
		select {
		case <-ctx.Done():
			s.tick() // final drain so buffered samples don't get dropped
			return
		case <-t.C:
			s.tick()
		}
	}
}

func (s *streamer) tick() {
	// Metric samples — the telemetry store also auto-buffers {type}_w and
	// {type}_soc for each structured emit, so the metric stream is a
	// superset of the emit stream. Forward everything; consumers filter.
	for _, m := range s.tel.FlushSamples() {
		if m.Driver != s.driver {
			continue
		}
		s.out.Metric(m.Driver, m.Metric, m.Value, m.TsMs)
	}
	// Structured readings: emit when UpdatedAt advances.
	for _, r := range s.tel.ReadingsByDriver(s.driver) {
		if prev, ok := s.lastReadingTS[r.DerType]; ok && !r.UpdatedAt.After(prev) {
			continue
		}
		s.lastReadingTS[r.DerType] = r.UpdatedAt
		s.out.Emit(r.Driver, r.DerType.String(), r.RawW, r.SoC, json.RawMessage(r.Data), r.UpdatedAt)
	}
	// Identity — emit on any field change, including the first tick where
	// driver_init has just returned and set_make/set_sn have been called.
	if s.env != nil {
		mk, sn, mac, ep := s.env.FullIdentity()
		cur := identitySnap{mk, sn, mac, ep}
		if cur != s.lastIdentity {
			s.lastIdentity = cur
			s.out.Identity(s.driver, mk, sn, mac, ep)
		}
	}
	// Health — tick count always advances on a successful Poll (even when
	// the driver emits nothing), so watching it is how we detect a live
	// but quiet driver. LastError surfaces Modbus / MQTT failures.
	if h := s.tel.DriverHealth(s.driver); h != nil {
		status := h.Status.String()
		if h.TickCount != s.lastTickCount || h.LastError != s.lastLastError || status != s.lastStatus {
			s.lastTickCount = h.TickCount
			s.lastLastError = h.LastError
			s.lastStatus = status
			s.out.Health(s.driver, status, h.ConsecutiveErrors, h.TickCount, h.LastError)
		}
	}
}
