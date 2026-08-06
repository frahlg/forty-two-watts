// Package demand tracks utility billing demand: apparent power
// integrated over clock-aligned windows (30 min for South African
// utilities), with the maximum counted window per billing cycle forming
// the demand-charge peak. Pure control flow — classification (band,
// demand-window membership) and billing-cycle anchoring are injected
// callbacks so this package depends on neither the tariff schedule nor
// SQLite.
package demand

import (
	"log/slog"
	"sync"
	"time"
)

// Classification of one instant by the tariff schedule.
type Classification struct {
	Band    string
	Counted bool // inside the tariff's demand window
}

// Persister stores completed intervals. Implemented by state.Store.
type Persister interface {
	SaveDemandInterval(iv Interval) error
}

// Interval mirrors state.DemandInterval without importing state.
type Interval struct {
	CycleStartMs    int64
	IntervalStartMs int64
	AvgKVA          float64
	AvgKW           float64
	Band            string
	Counted         bool
}

// Tracker integrates apparent-power samples into demand intervals.
// Sample-and-hold integration: each Observe extends the previous sample
// over the elapsed time, matching how utility meters integrate between
// register reads. Not safe for concurrent use; the control tick is the
// single caller.
type Tracker struct {
	mu sync.Mutex

	windowLen time.Duration
	classify  func(time.Time) Classification
	cycleOf   func(time.Time) time.Time
	persist   Persister

	// Current window accumulation.
	windowStart time.Time
	energyVAs   float64 // ∫ VA dt, volt-ampere-seconds
	energyWs    float64 // ∫ W dt, watt-seconds
	covered     time.Duration

	// Previous sample (sample-and-hold).
	lastAt time.Time
	lastVA float64
	lastW  float64

	// Billing-cycle peak cache (persisted view).
	peakCycleStart time.Time
	peakKVA        float64
	peakIntervalMs int64
}

// New builds a tracker. windowLen is the utility's demand-integration
// window; classify and cycleOf come from the compiled tariff schedule.
func New(windowLen time.Duration, classify func(time.Time) Classification, cycleOf func(time.Time) time.Time, persist Persister) *Tracker {
	return &Tracker{
		windowLen: windowLen,
		classify:  classify,
		cycleOf:   cycleOf,
		persist:   persist,
	}
}

// SeedPeak restores the peak-so-far after a restart.
func (t *Tracker) SeedPeak(cycleStart time.Time, peakKVA float64, intervalStartMs int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.peakCycleStart = cycleStart
	t.peakKVA = peakKVA
	t.peakIntervalMs = intervalStartMs
}

// Observe feeds one control-tick sample. va/w are the site apparent and
// real power at `now`.
func (t *Tracker) Observe(now time.Time, va, w float64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	ws := t.windowStartFor(now)
	switch {
	case t.windowStart.IsZero():
		// First sample (startup, possibly mid-window): begin partial
		// integration from now. The partial window persists with the
		// time it actually covered — conservative, and honest after a
		// restart.
		t.windowStart = ws
	case !ws.Equal(t.windowStart):
		// Window rolled over (possibly several if we were asleep).
		// Extend the last sample to the end of its own window, close
		// it, and start fresh — gaps between are unknown, not zero, so
		// they are simply not covered.
		end := t.windowStart.Add(t.windowLen)
		if !t.lastAt.IsZero() && t.lastAt.Before(end) {
			dt := end.Sub(t.lastAt)
			t.energyVAs += t.lastVA * dt.Seconds()
			t.energyWs += t.lastW * dt.Seconds()
			t.covered += dt
		}
		t.closeWindowLocked()
		t.windowStart = ws
		t.lastAt = time.Time{}
	default:
		if !t.lastAt.IsZero() && now.After(t.lastAt) {
			dt := now.Sub(t.lastAt)
			t.energyVAs += t.lastVA * dt.Seconds()
			t.energyWs += t.lastW * dt.Seconds()
			t.covered += dt
		}
	}
	t.lastAt = now
	t.lastVA = va
	t.lastW = w
}

// Snapshot is the live view for the API/UI.
type Snapshot struct {
	CycleStartMs     int64   `json:"cycle_start_ms"`
	PeakKVA          float64 `json:"peak_kva"`
	PeakIntervalMs   int64   `json:"peak_interval_ms,omitempty"`
	WindowStartMs    int64   `json:"window_start_ms"`
	WindowLenMs      int64   `json:"window_len_ms"`
	WindowAvgKVA     float64 `json:"window_avg_kva"`
	WindowAvgKW      float64 `json:"window_avg_kw"`
	WindowCoveredMs  int64   `json:"window_covered_ms"`
	WindowBand       string  `json:"window_band"`
	WindowCounted    bool    `json:"window_counted"`
	ProjectedPeakKVA float64 `json:"projected_peak_kva"` // max(peak, current window avg)
}

// Snapshot returns the live demand state at `now`.
func (t *Tracker) Snapshot(now time.Time) Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	cls := t.classify(now)
	var avgVA, avgW float64
	if t.covered > 0 {
		avgVA = t.energyVAs / t.covered.Seconds()
		avgW = t.energyWs / t.covered.Seconds()
	}
	cycle := t.cycleOf(now)
	peak := t.peakKVA
	if !t.peakCycleStart.Equal(cycle) {
		peak = 0 // new cycle since the peak was recorded
	}
	projected := peak
	if cls.Counted && avgVA/1000 > projected {
		projected = avgVA / 1000
	}
	return Snapshot{
		CycleStartMs:     cycle.UnixMilli(),
		PeakKVA:          peak,
		PeakIntervalMs:   t.peakIntervalMs,
		WindowStartMs:    t.windowStart.UnixMilli(),
		WindowLenMs:      t.windowLen.Milliseconds(),
		WindowAvgKVA:     avgVA / 1000,
		WindowAvgKW:      avgW / 1000,
		WindowCoveredMs:  t.covered.Milliseconds(),
		WindowBand:       cls.Band,
		WindowCounted:    cls.Counted,
		ProjectedPeakKVA: projected,
	}
}

// PeakSoFarKVA returns the billing peak for the cycle containing now.
func (t *Tracker) PeakSoFarKVA(now time.Time) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.peakCycleStart.Equal(t.cycleOf(now)) {
		return 0
	}
	return t.peakKVA
}

func (t *Tracker) windowStartFor(now time.Time) time.Time {
	return now.Truncate(t.windowLen)
}

// closeWindowLocked finalizes the current window: persists it and
// advances the in-memory peak when it is a counted interval.
func (t *Tracker) closeWindowLocked() {
	if t.covered <= 0 {
		t.resetWindowLocked()
		return
	}
	avgVA := t.energyVAs / t.covered.Seconds()
	avgW := t.energyWs / t.covered.Seconds()
	// Classify by the middle of the window: unambiguous even when the
	// window starts exactly on a band boundary.
	mid := t.windowStart.Add(t.windowLen / 2)
	cls := t.classify(mid)
	cycle := t.cycleOf(t.windowStart)

	if !t.peakCycleStart.Equal(cycle) {
		t.peakCycleStart = cycle
		t.peakKVA = 0
		t.peakIntervalMs = 0
	}
	if cls.Counted && avgVA/1000 > t.peakKVA {
		t.peakKVA = avgVA / 1000
		t.peakIntervalMs = t.windowStart.UnixMilli()
	}
	if t.persist != nil {
		iv := Interval{
			CycleStartMs:    cycle.UnixMilli(),
			IntervalStartMs: t.windowStart.UnixMilli(),
			AvgKVA:          avgVA / 1000,
			AvgKW:           avgW / 1000,
			Band:            cls.Band,
			Counted:         cls.Counted,
		}
		if err := t.persist.SaveDemandInterval(iv); err != nil {
			slog.Warn("demand interval persist failed", "err", err)
		}
	}
	t.resetWindowLocked()
}

func (t *Tracker) resetWindowLocked() {
	t.energyVAs = 0
	t.energyWs = 0
	t.covered = 0
}
