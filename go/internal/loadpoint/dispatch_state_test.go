package loadpoint

import (
	"testing"
	"time"
)

func TestHysteresisForwardOnlyDebounce(t *testing.T) {
	st := &DispatchState{}
	cfg := Settings{SurplusHysteresisW: 500, SurplusHysteresisS: 300}
	min := 4140.0
	base := time.Unix(0, 0)

	// Surplus 3900 (gap 240, within cap) for 200s — below threshold, keep min.
	got, commit := st.SurplusDecision(3900, min, cfg, base.Add(0))
	if commit {
		t.Errorf("should not commit to 0 immediately")
	}
	if got != min {
		t.Errorf("want min during grace, got %v", got)
	}

	got, commit = st.SurplusDecision(3900, min, cfg, base.Add(200*time.Second))
	if commit {
		t.Errorf("still in grace at 200s")
	}
	if got != min {
		t.Errorf("want min during grace at 200s, got %v", got)
	}

	// 301s after grace started → commit.
	got, commit = st.SurplusDecision(3900, min, cfg, base.Add(301*time.Second))
	if !commit {
		t.Errorf("grace expired → should commit")
	}
	if got != 0 {
		t.Errorf("after commit, want 0, got %v", got)
	}

	// Surplus recovers → reset immediately, resume on next tick.
	got, commit = st.SurplusDecision(4500, min, cfg, base.Add(302*time.Second))
	if commit {
		t.Errorf("post-recovery should not commit")
	}
	if got != 4500 {
		t.Errorf("recovered surplus should pass through, got %v", got)
	}
}

func TestHysteresisDeepDipImmediate(t *testing.T) {
	st := &DispatchState{}
	cfg := Settings{SurplusHysteresisW: 500, SurplusHysteresisS: 300}
	min := 4140.0
	base := time.Unix(0, 0)

	// Surplus 2000 (gap 2140, > cap of 500) → commit immediately, no grace.
	got, commit := st.SurplusDecision(2000, min, cfg, base.Add(0))
	if !commit {
		t.Errorf("deep dip should commit immediately")
	}
	if got != 0 {
		t.Errorf("deep dip → 0, got %v", got)
	}
}

func TestStarvationTimerFiresOnce(t *testing.T) {
	st := &DispatchState{}
	cfg := Settings{SurplusStarvationS: 1800}
	base := time.Unix(0, 0)

	// cmdW = 0 for 30 min.
	for s := 0; s <= 1800; s += 60 {
		got := st.StarvationTick(0, base.Add(time.Duration(s)*time.Second), cfg)
		if s < 1800 && got {
			t.Errorf("starvation fired too early at %ds", s)
		}
		if s == 1800 && !got {
			t.Errorf("starvation should fire at %ds", s)
		}
	}
	// Further ticks while still 0 do NOT re-fire (cooldown is a notifications-layer concern).
	got := st.StarvationTick(0, base.Add(1860*time.Second), cfg)
	if got {
		t.Errorf("starvation should not re-fire without reset")
	}
	// cmdW > 0 resets; next 0-run can fire again.
	st.StarvationTick(4140, base.Add(1900*time.Second), cfg)
	st.StarvationTick(0, base.Add(1901*time.Second), cfg)
	got = st.StarvationTick(0, base.Add(1901*time.Second+1800*time.Second), cfg)
	if !got {
		t.Errorf("starvation should fire on new run")
	}
}

func TestAutoClearOnCompleted(t *testing.T) {
	st := &DispatchState{}
	// prev=charging, cur=completed → clear.
	if !st.ObserveOpMode(3, 4, 4140) {
		t.Errorf("3→4 should signal clear")
	}
}

func TestAutoClearOnUnplug(t *testing.T) {
	st := &DispatchState{}
	if !st.ObserveUnplug(true, false) {
		t.Errorf("plugged true→false should signal clear")
	}
	if st.ObserveUnplug(false, true) {
		t.Errorf("false→true (plug-in) should not signal clear")
	}
}

func TestAutoClearIgnoresSurplusPause(t *testing.T) {
	st := &DispatchState{}
	// prev=charging, cur=awaiting_start, OUR last cmd was 0 → our pause, don't clear.
	if st.ObserveOpMode(3, 2, 0) {
		t.Errorf("3→2 with lastCmd=0 must NOT signal clear")
	}
	// prev=charging, cur=awaiting_start, OUR last cmd was >0 → user paused; spec says target stays.
	if st.ObserveOpMode(3, 2, 4140) {
		t.Errorf("3→2 with lastCmd>0 (user pause) must NOT signal clear")
	}
}
