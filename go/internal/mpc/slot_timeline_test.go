package mpc

import (
	"math"
	"testing"
)

func TestValidateSlotChronology(t *testing.T) {
	const (
		start    = int64(1_800_000_000_000)
		minuteMs = int64(60 * 1000)
	)
	tests := []struct {
		name  string
		slots []Slot
		ok    bool
	}{
		{name: "contiguous", slots: []Slot{{StartMs: start, LenMin: 15}, {StartMs: start + 15*minuteMs, LenMin: 15}}, ok: true},
		{name: "gap", slots: []Slot{{StartMs: start, LenMin: 15}, {StartMs: start + 30*minuteMs, LenMin: 15}}, ok: true},
		{name: "duplicate", slots: []Slot{{StartMs: start, LenMin: 15}, {StartMs: start, LenMin: 15}}},
		{name: "overlap", slots: []Slot{{StartMs: start, LenMin: 60}, {StartMs: start + 15*minuteMs, LenMin: 15}}},
		{name: "out of order", slots: []Slot{{StartMs: start + 15*minuteMs, LenMin: 15}, {StartMs: start, LenMin: 15}}},
		{name: "zero start", slots: []Slot{{LenMin: 15}}},
		{name: "negative start", slots: []Slot{{StartMs: -1, LenMin: 15}}},
		{name: "zero length", slots: []Slot{{StartMs: start}}},
		{name: "negative length", slots: []Slot{{StartMs: start, LenMin: -1}}},
		{name: "end overflow", slots: []Slot{{StartMs: math.MaxInt64 - minuteMs, LenMin: 2}}},
		{name: "empty"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSlotChronology(tc.slots)
			if tc.ok && err != nil {
				t.Fatalf("validateSlotChronology rejected valid slots: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("validateSlotChronology accepted invalid slots")
			}
		})
	}
}

func TestValidateActionChronologyMatchesSlotContract(t *testing.T) {
	const start = int64(1_800_000_000_000)
	actions := []Action{
		{SlotStartMs: start, SlotLenMin: 15},
		{SlotStartMs: start + 10*60*1000, SlotLenMin: 15},
	}
	if err := validateActionChronology(actions); err == nil {
		t.Fatal("validateActionChronology accepted overlapping actions")
	}
}

func TestValidatePlanSlotAlignment(t *testing.T) {
	const start = int64(1_800_000_000_000)
	slots := []Slot{{StartMs: start, LenMin: 15}}
	tests := []struct {
		name    string
		actions []Action
		ok      bool
	}{
		{name: "exact", actions: []Action{{SlotStartMs: start, SlotLenMin: 15}}, ok: true},
		{name: "missing action"},
		{name: "shifted start", actions: []Action{{SlotStartMs: start + 60*1000, SlotLenMin: 15}}},
		{name: "different length", actions: []Action{{SlotStartMs: start, SlotLenMin: 30}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePlanSlotAlignment(slots, tc.actions)
			if tc.ok && err != nil {
				t.Fatalf("validatePlanSlotAlignment rejected an exact plan: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("validatePlanSlotAlignment accepted a mismatched plan")
			}
		})
	}
}
