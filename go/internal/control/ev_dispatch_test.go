package control

import (
	"testing"
	"time"
)

func TestSnapChargeWSnapsToNearest(t *testing.T) {
	steps := []float64{0, 1400, 4100, 7400, 11000}
	cases := []struct {
		want     float64
		expected float64
		note     string
	}{
		{0, 0, "zero → off, short-circuits"},
		{1000, 1400, "below min step snaps up to lowest non-zero"},
		{1300, 1400, "near min → min"},
		{5500, 4100, "midway rounds to closer"},
		{6000, 7400, "midway rounds to closer (upper)"},
		{11000, 11000, "at max step"},
		{15000, 11000, "above max clamps to max"},
	}
	for _, tc := range cases {
		t.Run(tc.note, func(t *testing.T) {
			got := SnapChargeW(tc.want, 1400, 11000, steps)
			if got != tc.expected {
				t.Errorf("SnapChargeW(%f) = %f, want %f", tc.want, got, tc.expected)
			}
		})
	}
}

func TestSnapChargeWNoStepsReturnsClamped(t *testing.T) {
	got := SnapChargeW(5000, 1400, 11000, nil)
	if got != 5000 {
		t.Errorf("want continuous passthrough; got %.0f", got)
	}
	got = SnapChargeW(500, 1400, 11000, nil)
	if got != 1400 {
		t.Errorf("want clamp to min; got %.0f", got)
	}
	got = SnapChargeW(20000, 1400, 11000, nil)
	if got != 11000 {
		t.Errorf("want clamp to max; got %.0f", got)
	}
}

func TestEnergyBudgetToPowerW(t *testing.T) {
	// 1 kWh over 1 hour = 1000 W
	if got := EnergyBudgetToPowerW(1000, 3600); got != 1000 {
		t.Errorf("1 kWh/h should be 1000 W, got %.0f", got)
	}
	// 500 Wh over 15 minutes → 2000 W
	if got := EnergyBudgetToPowerW(500, 900); got != 2000 {
		t.Errorf("500 Wh / 15 min should be 2000 W, got %.0f", got)
	}
	// Negative / zero remaining → 0
	if got := EnergyBudgetToPowerW(-50, 600); got != 0 {
		t.Errorf("negative remaining should stop charging, got %.0f", got)
	}
	if got := EnergyBudgetToPowerW(500, 0); got != 0 {
		t.Errorf("zero remaining time should return 0 (safety), got %.0f", got)
	}
}

// TestEVDispatchTickAccumulatorIntegratesPower asserts that deliveredWh
// is built from a real ∫P dt across ticks, not the powerW × elapsed
// approximation that broke the prior implementation.
func TestEVDispatchTickAccumulatorIntegratesPower(t *testing.T) {
	steps := []float64{0, 1400, 4100, 7400, 11000}
	slotStart := time.Date(2026, 4, 20, 22, 0, 0, 0, time.UTC)
	slotEnd := slotStart.Add(15 * time.Minute)
	// 4 kWh budget over 15 min = 16 kW average — well above charger max,
	// so wantW always saturates at maxW.
	const budgetWh = 4000

	s := &EVDispatchState{}

	// t = 0 s, no power yet. First tick → command full max (slow plan,
	// big budget, no holdoff dwell on first tick).
	cmd, send := s.Tick(slotStart, slotStart, slotEnd, budgetWh, 0,
		1400, 11000, steps, 30*time.Second, 5*time.Minute)
	if !send || cmd != 11000 {
		t.Fatalf("first tick: want send=true cmd=11000, got send=%v cmd=%.0f", send, cmd)
	}

	// t = 60 s, power has been at 11 kW for the last minute. Real
	// integral ⇒ deliveredWh ≈ 11000 × 60/3600 ≈ 183 Wh.
	// Old (broken) approximation: powerW × elapsed = 11000 × 60/3600 = 183 Wh.
	// Identical here because power was constant — the bug only fires
	// when power changes mid-slot, tested next.
	now := slotStart.Add(60 * time.Second)
	cmd, _ = s.Tick(now, slotStart, slotEnd, budgetWh, 11000,
		1400, 11000, steps, 30*time.Second, 5*time.Minute)
	if got := s.deliveredWh; got < 180 || got > 186 {
		t.Errorf("deliveredWh after 60s @ 11kW: want ~183, got %.1f", got)
	}
	_ = cmd

	// t = 120 s, power dropped to 0 W (EV finished a packet). Real
	// integral adds 0 × 60s = 0 to deliveredWh. The old approximation
	// would have computed alreadyWh = 0 × 120 = 0, RESETTING the running
	// total — which is the exact bug that caused oscillation. Verify the
	// accumulator preserves history across the power drop.
	now = slotStart.Add(120 * time.Second)
	_, _ = s.Tick(now, slotStart, slotEnd, budgetWh, 0,
		1400, 11000, steps, 30*time.Second, 5*time.Minute)
	if got := s.deliveredWh; got < 180 || got > 186 {
		t.Errorf("deliveredWh preserved across power drop: want ~183, got %.1f", got)
	}
}

// TestEVDispatchTickResetsOnSlotRollover asserts that crossing into a
// new slot zeroes the accumulator (so each slot's budget is honoured
// independently).
func TestEVDispatchTickResetsOnSlotRollover(t *testing.T) {
	steps := []float64{0, 1400, 4100, 7400, 11000}
	slot1Start := time.Date(2026, 4, 20, 22, 0, 0, 0, time.UTC)
	slot1End := slot1Start.Add(15 * time.Minute)
	slot2Start := slot1End
	slot2End := slot2Start.Add(15 * time.Minute)

	s := &EVDispatchState{}
	// Build up some delivered energy in slot 1.
	s.Tick(slot1Start, slot1Start, slot1End, 1000, 0, 1400, 11000, steps,
		30*time.Second, 5*time.Minute)
	s.Tick(slot1Start.Add(60*time.Second), slot1Start, slot1End, 1000, 4000,
		1400, 11000, steps, 30*time.Second, 5*time.Minute)
	if s.deliveredWh <= 0 {
		t.Fatalf("expected non-zero deliveredWh in slot 1, got %.1f", s.deliveredWh)
	}

	// Cross into slot 2.
	s.Tick(slot2Start, slot2Start, slot2End, 500, 4000, 1400, 11000, steps,
		30*time.Second, 5*time.Minute)
	if s.deliveredWh != 0 {
		t.Errorf("slot rollover should reset deliveredWh, got %.1f", s.deliveredWh)
	}
	if !s.slotStart.Equal(slot2Start) {
		t.Errorf("slotStart should track slot 2, got %v", s.slotStart)
	}
}

// TestEVDispatchTickHoldoffSuppressesChange asserts that when the formula
// wants a different command but the holdoff hasn't elapsed, the prior
// value is returned and send=false. This is what kills the 6→11→0→6 flap
// caused by Easee cloud reporting lag.
func TestEVDispatchTickHoldoffSuppressesChange(t *testing.T) {
	steps := []float64{0, 1400, 4100, 7400, 11000}
	slotStart := time.Date(2026, 4, 20, 22, 0, 0, 0, time.UTC)
	slotEnd := slotStart.Add(15 * time.Minute)

	s := &EVDispatchState{}
	holdoff := 30 * time.Second

	// First tick @ t=0: 4000 Wh budget → wants max → command 11kW.
	cmd0, send0 := s.Tick(slotStart, slotStart, slotEnd, 4000, 0,
		1400, 11000, steps, holdoff, 5*time.Minute)
	if !send0 || cmd0 != 11000 {
		t.Fatalf("t=0: want send=true cmd=11000, got send=%v cmd=%.0f", send0, cmd0)
	}

	// 5 s later, plan has shrunk to 50 Wh (re-plan or drift) — formula
	// wants 0 because over-delivered. Holdoff hasn't elapsed, so the
	// command must hold at 11 kW with send=false.
	cmd1, send1 := s.Tick(slotStart.Add(5*time.Second), slotStart, slotEnd,
		50, 11000, 1400, 11000, steps, holdoff, 5*time.Minute)
	if send1 {
		t.Errorf("t=5s within holdoff: send must be false, got cmd=%.0f", cmd1)
	}
	if cmd1 != 11000 {
		t.Errorf("t=5s within holdoff: prior cmd must be repeated, got %.0f", cmd1)
	}

	// 35 s after t=0, holdoff has elapsed. Over-delivered (≥150 Wh
	// accumulated against 50 Wh budget) → wantW = 0 → cmd = 0.
	cmd2, send2 := s.Tick(slotStart.Add(35*time.Second), slotStart, slotEnd,
		50, 11000, 1400, 11000, steps, holdoff, 5*time.Minute)
	if !send2 {
		t.Errorf("t=35s past holdoff: send must be true, got cmd=%.0f", cmd2)
	}
	if cmd2 != 0 {
		t.Errorf("t=35s budget exhausted: cmd should be 0, got %.0f", cmd2)
	}
}

// TestEVDispatchTickHeartbeat asserts that an unchanged command is
// re-sent every `heartbeat` interval so a charger watchdog can't silently
// zero the setpoint.
func TestEVDispatchTickHeartbeat(t *testing.T) {
	steps := []float64{0, 1400, 4100, 7400, 11000}
	slotStart := time.Date(2026, 4, 20, 22, 0, 0, 0, time.UTC)
	slotEnd := slotStart.Add(15 * time.Minute)

	s := &EVDispatchState{}
	heartbeat := 60 * time.Second

	// First tick — sends.
	_, send0 := s.Tick(slotStart, slotStart, slotEnd, 1000, 0,
		1400, 11000, steps, 30*time.Second, heartbeat)
	if !send0 {
		t.Fatal("first tick must send")
	}

	// 30 s later, plan unchanged, command unchanged. Should NOT send
	// (holdoff dwell met but no change AND not yet heartbeat).
	_, send1 := s.Tick(slotStart.Add(30*time.Second), slotStart, slotEnd,
		1000, 4000, 1400, 11000, steps, 30*time.Second, heartbeat)
	if send1 {
		t.Error("30s in, no change, before heartbeat: must not send")
	}

	// 65 s later, heartbeat fires.
	_, send2 := s.Tick(slotStart.Add(65*time.Second), slotStart, slotEnd,
		1000, 4000, 1400, 11000, steps, 30*time.Second, heartbeat)
	if !send2 {
		t.Error("past heartbeat: must re-send")
	}
}
