package state

import "testing"

func TestDemandIntervalRoundTrip(t *testing.T) {
	st := openTestStore(t)
	defer st.Close()

	cycle := int64(1_780_000_000_000)
	ivs := []DemandInterval{
		{CycleStartMs: cycle, IntervalStartMs: cycle, AvgKVA: 120, AvgKW: 114, Band: "peak", Counted: true},
		{CycleStartMs: cycle, IntervalStartMs: cycle + 1_800_000, AvgKVA: 180, AvgKW: 171, Band: "standard", Counted: true},
		{CycleStartMs: cycle, IntervalStartMs: cycle + 3_600_000, AvgKVA: 300, AvgKW: 290, Band: "offpeak", Counted: false},
	}
	for _, iv := range ivs {
		if err := st.SaveDemandInterval(iv); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	// Peak follows the highest COUNTED interval (180), not the higher
	// uncounted off-peak spike (300).
	peak, ivMs, ok, err := st.BillingPeak(cycle)
	if err != nil || !ok {
		t.Fatalf("peak: ok=%v err=%v", ok, err)
	}
	if peak != 180 || ivMs != cycle+1_800_000 {
		t.Fatalf("peak = %v @ %d, want 180 @ %d", peak, ivMs, cycle+1_800_000)
	}

	// A lower counted interval never regresses the peak.
	if err := st.SaveDemandInterval(DemandInterval{
		CycleStartMs: cycle, IntervalStartMs: cycle + 5_400_000,
		AvgKVA: 90, AvgKW: 85, Band: "peak", Counted: true,
	}); err != nil {
		t.Fatal(err)
	}
	peak, _, _, _ = st.BillingPeak(cycle)
	if peak != 180 {
		t.Fatalf("peak regressed to %v", peak)
	}

	got, err := st.DemandIntervals(cycle, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d intervals, want 4", len(got))
	}
	if got[0].IntervalStartMs != cycle+5_400_000 {
		t.Fatalf("intervals not newest-first: %+v", got[0])
	}

	// Re-saving an interval upserts rather than erroring.
	if err := st.SaveDemandInterval(ivs[0]); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Unknown cycle: ok=false, no error.
	if _, _, ok, err := st.BillingPeak(1); ok || err != nil {
		t.Fatalf("unknown cycle: ok=%v err=%v", ok, err)
	}
}
