package demand

import (
	"testing"
	"time"
)

type memPersist struct {
	saved []Interval
}

func (m *memPersist) SaveDemandInterval(iv Interval) error {
	m.saved = append(m.saved, iv)
	return nil
}

func alwaysPeak(time.Time) Classification {
	return Classification{Band: "peak", Counted: true}
}

func monthCycle(t time.Time) time.Time {
	y, m, _ := t.Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, t.Location())
}

func TestTrackerIntegratesFlatLoad(t *testing.T) {
	p := &memPersist{}
	tr := New(30*time.Minute, alwaysPeak, monthCycle, p)
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	// Constant 100 kVA / 95 kW for a full window, sampled every 2 s.
	for ts := start; !ts.After(start.Add(30 * time.Minute)); ts = ts.Add(2 * time.Second) {
		tr.Observe(ts, 100_000, 95_000)
	}
	if len(p.saved) != 1 {
		t.Fatalf("saved %d intervals, want 1", len(p.saved))
	}
	iv := p.saved[0]
	if iv.AvgKVA < 99.9 || iv.AvgKVA > 100.1 {
		t.Errorf("avg kVA = %v, want ~100", iv.AvgKVA)
	}
	if iv.AvgKW < 94.9 || iv.AvgKW > 95.1 {
		t.Errorf("avg kW = %v, want ~95", iv.AvgKW)
	}
	if !iv.Counted || iv.Band != "peak" {
		t.Errorf("interval classification: %+v", iv)
	}
	if got := tr.PeakSoFarKVA(start.Add(31 * time.Minute)); got < 99.9 {
		t.Errorf("peak = %v, want ~100", got)
	}
}

func TestTrackerTimeWeightsSpike(t *testing.T) {
	p := &memPersist{}
	tr := New(30*time.Minute, alwaysPeak, monthCycle, p)
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	// 15 min at 200 kVA, 15 min at 0 → average 100.
	tr.Observe(start, 200_000, 200_000)
	tr.Observe(start.Add(15*time.Minute), 0, 0)
	tr.Observe(start.Add(30*time.Minute), 0, 0) // rolls the window
	if len(p.saved) != 1 {
		t.Fatalf("saved %d intervals, want 1", len(p.saved))
	}
	if got := p.saved[0].AvgKVA; got < 99.9 || got > 100.1 {
		t.Errorf("avg kVA = %v, want ~100 (time-weighted)", got)
	}
}

func TestTrackerUncountedIntervalNeverSetsPeak(t *testing.T) {
	p := &memPersist{}
	offpeak := func(time.Time) Classification {
		return Classification{Band: "offpeak", Counted: false}
	}
	tr := New(30*time.Minute, offpeak, monthCycle, p)
	start := time.Date(2026, 7, 1, 2, 0, 0, 0, time.UTC)
	tr.Observe(start, 500_000, 500_000)
	tr.Observe(start.Add(30*time.Minute), 0, 0)
	if len(p.saved) != 1 || p.saved[0].Counted {
		t.Fatalf("interval should persist as uncounted: %+v", p.saved)
	}
	if got := tr.PeakSoFarKVA(start.Add(time.Hour)); got != 0 {
		t.Errorf("off-peak demand set the billing peak: %v", got)
	}
}

func TestTrackerPeakSurvivesViaSeed(t *testing.T) {
	tr := New(30*time.Minute, alwaysPeak, monthCycle, nil)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	tr.SeedPeak(monthCycle(now), 180, now.Add(-24*time.Hour).UnixMilli())
	if got := tr.PeakSoFarKVA(now); got != 180 {
		t.Errorf("seeded peak = %v, want 180", got)
	}
	// A new cycle forgets the old peak.
	aug := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	if got := tr.PeakSoFarKVA(aug); got != 0 {
		t.Errorf("new cycle peak = %v, want 0", got)
	}
}

func TestTrackerGapDoesNotFabricateDemand(t *testing.T) {
	p := &memPersist{}
	tr := New(30*time.Minute, alwaysPeak, monthCycle, p)
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	tr.Observe(start, 100_000, 100_000)
	tr.Observe(start.Add(10*time.Minute), 100_000, 100_000)
	// Process asleep for 3 windows; wakes mid-later-window.
	tr.Observe(start.Add(100*time.Minute), 50_000, 50_000)
	// First window closed with only its covered portion; gap windows
	// produced nothing.
	if len(p.saved) != 1 {
		t.Fatalf("saved %d intervals, want 1 (gap must not synthesize windows)", len(p.saved))
	}
	if got := p.saved[0].AvgKVA; got < 99.9 || got > 100.1 {
		t.Errorf("avg = %v, want ~100 over the covered 30 min", got)
	}
}

func TestSnapshotProjectsRunningWindow(t *testing.T) {
	tr := New(30*time.Minute, alwaysPeak, monthCycle, nil)
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	tr.Observe(start, 120_000, 110_000)
	tr.Observe(start.Add(10*time.Minute), 120_000, 110_000)
	snap := tr.Snapshot(start.Add(10 * time.Minute))
	if snap.WindowAvgKVA < 119.9 || snap.WindowAvgKVA > 120.1 {
		t.Errorf("window avg = %v, want ~120", snap.WindowAvgKVA)
	}
	if snap.ProjectedPeakKVA < 119.9 {
		t.Errorf("projected peak = %v, want ≥ window avg", snap.ProjectedPeakKVA)
	}
	if !snap.WindowCounted || snap.WindowBand != "peak" {
		t.Errorf("snapshot classification: %+v", snap)
	}
}
