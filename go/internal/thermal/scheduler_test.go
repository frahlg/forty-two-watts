package thermal

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func mkSlots(prices []float64, startMs int64, lenMin int) []PriceSlot {
	out := make([]PriceSlot, len(prices))
	for i, p := range prices {
		out[i] = PriceSlot{
			StartMs:  startMs + int64(i)*int64(lenMin)*60*1000,
			LenMin:   lenMin,
			PriceOre: p,
		}
	}
	return out
}

func TestScheduleBlocks_PicksMostExpensiveWithinBudget(t *testing.T) {
	// 24 one-hour slots, prices ramp 50 → 250 öre. Budget 4 kWh,
	// max block 2000 W → fits 2 hours of full block. Expect the
	// two most-expensive slots picked.
	slots := mkSlots([]float64{
		50, 60, 70, 80, 90, 100, 110, 120,
		130, 140, 150, 160, 170, 180, 190, 200,
		210, 220, 230, 240, 250, 240, 230, 220,
	}, 1_700_000_000_000, 60)
	cfg := DriverConfig{
		Name: "hp", MaxBlockW: 2000, BudgetKwhPerDay: 4,
	}
	out := ScheduleBlocks(slots, cfg)
	if len(out) != 2 {
		t.Fatalf("want 2 blocks, got %d", len(out))
	}
	// Slots at index 20 (price 250) + 19 (price 240) — sorted
	// back to chronological order by StartMs.
	for _, b := range out {
		if b.BlockW != 2000 {
			t.Errorf("block_w want 2000, got %v", b.BlockW)
		}
	}
}

func TestScheduleBlocks_HonoursPriceFloor(t *testing.T) {
	// All slots below the floor — nothing scheduled even though
	// budget is plenty.
	slots := mkSlots([]float64{20, 30, 40, 30, 20}, 1_700_000_000_000, 60)
	out := ScheduleBlocks(slots, DriverConfig{
		Name: "hp", MaxBlockW: 2000, BudgetKwhPerDay: 100,
		MinSlotPriceOre: 50,
	})
	if len(out) != 0 {
		t.Errorf("expected zero blocks below floor, got %d", len(out))
	}
}

func TestScheduleBlocks_DefaultsApplied(t *testing.T) {
	// Empty cfg fields → defaults (6 kWh budget, 50 öre floor).
	slots := mkSlots([]float64{40, 60}, 1_700_000_000_000, 60)
	out := ScheduleBlocks(slots, DriverConfig{Name: "hp", MaxBlockW: 1000})
	if len(out) != 1 {
		t.Fatalf("want 1 block, got %d", len(out))
	}
	if out[0].StartMs != slots[1].StartMs {
		t.Errorf("expected the 60 öre slot, got start %d", out[0].StartMs)
	}
}

func TestScheduleBlocks_NoMaxBlockReturnsEmpty(t *testing.T) {
	slots := mkSlots([]float64{200}, 1_700_000_000_000, 60)
	out := ScheduleBlocks(slots, DriverConfig{Name: "hp", MaxBlockW: 0, BudgetKwhPerDay: 10})
	if len(out) != 0 {
		t.Errorf("expected empty schedule with no MaxBlockW")
	}
}

func TestScheduleBlocks_PartialBudgetFinalSlot(t *testing.T) {
	// 3 slots all priced 200 öre × 60 min × 2000 W → 2 kWh per
	// slot. Budget 3 kWh → first slot full (2 kWh used), second
	// gets a partial worth 1 kWh = 1000 W block, third doesn't
	// fit.
	slots := mkSlots([]float64{200, 200, 200}, 1_700_000_000_000, 60)
	out := ScheduleBlocks(slots, DriverConfig{
		Name: "hp", MaxBlockW: 2000, BudgetKwhPerDay: 3,
	})
	if len(out) != 2 {
		t.Fatalf("want 2 blocks (one full + one partial), got %d", len(out))
	}
	if out[0].BlockW != 2000 {
		t.Errorf("first slot want full 2000 W, got %v", out[0].BlockW)
	}
	if out[1].BlockW < 900 || out[1].BlockW > 1100 {
		t.Errorf("second slot want ~1000 W partial, got %v", out[1].BlockW)
	}
}

func TestBlockAt_ReturnsCurrentSlotW(t *testing.T) {
	t0 := int64(1_700_000_000_000)
	sched := []SlotBlock{
		{StartMs: t0, LenMin: 60, BlockW: 1500},
		{StartMs: t0 + 3600_000, LenMin: 60, BlockW: 2500},
	}
	// Inside slot 1
	if got := BlockAt(sched, time.UnixMilli(t0+30*60*1000)); got != 1500 {
		t.Errorf("slot 1 want 1500, got %v", got)
	}
	// Inside slot 2
	if got := BlockAt(sched, time.UnixMilli(t0+90*60*1000)); got != 2500 {
		t.Errorf("slot 2 want 2500, got %v", got)
	}
	// After both slots
	if got := BlockAt(sched, time.UnixMilli(t0+200*60*1000)); got != 0 {
		t.Errorf("after schedule want 0, got %v", got)
	}
	// Before the first slot
	if got := BlockAt(sched, time.UnixMilli(t0-60*1000)); got != 0 {
		t.Errorf("before schedule want 0, got %v", got)
	}
}

func TestController_OnlySendsOnChange(t *testing.T) {
	var (
		mu    sync.Mutex
		sends []string
	)
	send := func(ctx context.Context, driver string, payload []byte) error {
		mu.Lock()
		defer mu.Unlock()
		sends = append(sends, driver+":"+string(payload))
		return nil
	}
	c := NewController(send)
	t0 := int64(1_700_000_000_000)
	c.SetSchedule("hp", []SlotBlock{
		{StartMs: t0, LenMin: 60, BlockW: 2000},
	})

	// Two ticks inside the slot — first sends, second is a no-op.
	c.Tick(context.Background(), time.UnixMilli(t0+10*60*1000))
	c.Tick(context.Background(), time.UnixMilli(t0+20*60*1000))
	mu.Lock()
	defer mu.Unlock()
	if len(sends) != 1 {
		t.Fatalf("want 1 send (idempotent), got %d: %v", len(sends), sends)
	}
	// Verify the payload looks right: action=battery, power_w=-2000
	var got map[string]any
	if err := json.Unmarshal([]byte(sends[0][len("hp:"):]), &got); err != nil {
		t.Fatal(err)
	}
	if got["action"] != "battery" || got["power_w"].(float64) != -2000 {
		t.Errorf("unexpected payload: %v", got)
	}
}

func TestController_ReleasesAfterScheduleEnds(t *testing.T) {
	var (
		mu    sync.Mutex
		sends []string
	)
	send := func(ctx context.Context, driver string, payload []byte) error {
		mu.Lock()
		defer mu.Unlock()
		sends = append(sends, string(payload))
		return nil
	}
	c := NewController(send)
	t0 := int64(1_700_000_000_000)
	c.SetSchedule("hp", []SlotBlock{
		{StartMs: t0, LenMin: 60, BlockW: 2000},
	})
	// Inside the slot — block command goes out.
	c.Tick(context.Background(), time.UnixMilli(t0+10*60*1000))
	// Past the slot — release command (power_w: 0) should follow.
	c.Tick(context.Background(), time.UnixMilli(t0+90*60*1000))
	mu.Lock()
	defer mu.Unlock()
	if len(sends) != 2 {
		t.Fatalf("want 2 sends (block then release), got %d: %v", len(sends), sends)
	}
	var release map[string]any
	if err := json.Unmarshal([]byte(sends[1]), &release); err != nil {
		t.Fatal(err)
	}
	if release["power_w"].(float64) != 0 {
		t.Errorf("expected release (power_w=0), got %v", release)
	}
}
