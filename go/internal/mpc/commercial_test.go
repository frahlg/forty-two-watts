package mpc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func commercialSlots(n int) []Slot {
	slots := make([]Slot, n)
	for i := range slots {
		slots[i] = Slot{
			StartMs: int64(i) * 3_600_000, LenMin: 60,
			PriceOre: 100, SpotOre: 50, Confidence: 1,
		}
	}
	return slots
}

func TestCommercialRequestBlockRendering(t *testing.T) {
	slots := commercialSlots(3)

	if got := commercialRequestBlock(slots, Params{}); got != nil {
		t.Fatalf("nil spec must render nil, got %+v", got)
	}
	if got := commercialRequestBlock(slots, Params{Commercial: &CommercialSpec{}}); got != nil {
		t.Fatalf("empty spec must render nil, got %+v", got)
	}

	p := Params{Commercial: &CommercialSpec{
		DemandRateOrePerKW:      368, // 350 c/kVA ÷ 0.95 PF
		BillingPeakSoFarW:       120_000,
		DemandActive:            []bool{true, false, true},
		BackupMinUsableEnergyWh: 40_000,
	}}
	block := commercialRequestBlock(slots, p)
	if block == nil || block.Version != "srcful-commercial-v1" {
		t.Fatalf("block: %+v", block)
	}
	if len(block.BackupMinUsableEnergyWh) != 3 || block.BackupMinUsableEnergyWh[1] != 40_000 {
		t.Fatalf("backup vector: %v", block.BackupMinUsableEnergyWh)
	}
	if block.DemandCharge == nil || block.DemandCharge.RateOrePerKW != 368 ||
		block.DemandCharge.BillingPeakSoFarW != 120_000 || !block.DemandCharge.ActiveWindow[2] {
		t.Fatalf("demand charge: %+v", block.DemandCharge)
	}

	// A slot-count mismatch on DemandActive drops the demand block
	// rather than sending a vector the worker will reject.
	p.Commercial.DemandActive = []bool{true}
	if block := commercialRequestBlock(slots, p); block.DemandCharge != nil {
		t.Fatalf("mismatched DemandActive must drop demand charge, got %+v", block.DemandCharge)
	}
}

// captureTransport records the payload and reports configurable features.
type captureTransport struct {
	features []string
	payload  []byte
}

func (c *captureTransport) RoundTrip(_ context.Context, payload []byte) ([]byte, error) {
	c.payload = append([]byte(nil), payload...)
	return nil, errors.New("capture only")
}
func (c *captureTransport) Health(context.Context) (OptimizerRuntimeInfo, error) {
	return OptimizerRuntimeInfo{ProtocolVersion: 1, Features: c.features}, nil
}
func (c *captureTransport) Close() error { return nil }

func commercialParams() Params {
	return Params{
		Mode: ModeArbitrage, CapacityWh: 10_000, InitialSoCPct: 50,
		SoCMinPct: 5, SoCMaxPct: 95, MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 1, DischargeEfficiency: 1,
		Commercial: &CommercialSpec{
			DemandRateOrePerKW: 368, DemandActive: []bool{true, true, true},
			BackupMinUsableEnergyWh: 4000,
		},
	}
}

func TestOptimizeSendsCommercialOnlyWithFeature(t *testing.T) {
	slots := commercialSlots(3)

	for _, tc := range []struct {
		features []string
		want     bool
	}{
		{[]string{"champion", "commercial_constraints_v1"}, true},
		{[]string{"champion"}, false},
	} {
		tr := &captureTransport{features: tc.features}
		o := &ExternalOptimizer{cfg: ExternalOptimizerConfig{Timeout: time.Second, Solver: "HIGHS"}, transport: tr}
		_, _ = o.Optimize(context.Background(), slots, commercialParams())
		if tr.payload == nil {
			t.Fatal("no payload captured")
		}
		has := strings.Contains(string(tr.payload), "commercial_constraints")
		if has != tc.want {
			t.Errorf("features %v: commercial in payload = %v, want %v", tc.features, has, tc.want)
		}
		if tc.want {
			var req map[string]json.RawMessage
			if err := json.Unmarshal(tr.payload, &req); err != nil {
				t.Fatalf("payload not JSON: %v", err)
			}
			var block struct {
				Version string `json:"version"`
			}
			if err := json.Unmarshal(req["commercial_constraints"], &block); err != nil || block.Version != "srcful-commercial-v1" {
				t.Fatalf("commercial block malformed: %s", req["commercial_constraints"])
			}
		}
	}
}

func TestValidatePlanEnforcesBackupFloor(t *testing.T) {
	slots := commercialSlots(2)
	p := commercialParams()
	p.Commercial.DemandActive = []bool{true, true}

	action := func(i int, batteryW, socPct float64) Action {
		// Slots carry no PV/load, so grid balance is just the battery,
		// and cost replays as exported energy × SpotOre (50).
		return Action{
			SlotStartMs: slots[i].StartMs, SlotLenMin: 60,
			BatteryW: batteryW, SoCPct: socPct, GridW: batteryW,
			CostOre: batteryW / 1000 * 50,
		}
	}

	// 50% of 10 kWh = 5000 Wh. Floor 4000 Wh. Draining 2 kW for an hour
	// lands at 3000 Wh — below the floor.
	bad := &Plan{TotalCostOre: -100, Actions: []Action{action(0, -2000, 30), action(1, 0, 30)}}
	if err := ValidatePlan(slots, p, bad); err == nil || !strings.Contains(err.Error(), "backup reserve") {
		t.Fatalf("expected backup-reserve rejection, got %v", err)
	}

	// Draining to exactly the floor is allowed.
	good := &Plan{TotalCostOre: -50, Actions: []Action{action(0, -500, 45), action(1, -500, 40)}}
	if err := ValidatePlan(slots, p, good); err != nil {
		t.Fatalf("plan at the floor should validate: %v", err)
	}

	// Without the commercial spec the same drain is fine.
	p.Commercial = nil
	if err := ValidatePlan(slots, p, bad); err != nil {
		t.Fatalf("residential path must be unaffected: %v", err)
	}
}
