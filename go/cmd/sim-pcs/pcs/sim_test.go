package pcs

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/simonvetter/modbus"
)

func settle(r *Rack, d time.Duration) {
	for t := time.Duration(0); t < d; t += 100 * time.Millisecond {
		r.Tick(100 * time.Millisecond)
	}
}

func TestRackApproachesSetpoint(t *testing.T) {
	r := NewRack(DefaultRack())
	r.SetSetpoint(10_000)
	settle(r, 10*time.Second) // 5 τ
	if s := r.Snapshot(); s.PowerW < 9_500 {
		t.Fatalf("power %v after 5τ, want ≈10000", s.PowerW)
	}
}

func TestRackSoCIntegratesAndCutsOff(t *testing.T) {
	cfg := DefaultRack()
	cfg.CapacityWh = 1000 // small so an hour of charge fills it
	cfg.InitialSoC = 0.95
	r := NewRack(cfg)
	r.SetSetpoint(25_000)
	settle(r, 60*time.Second)
	// ~25 kW into 1 kWh for a minute would be ~0.4 SoC — but the full
	// cutoff must stop at 1.0 and power must collapse.
	s := r.Snapshot()
	if s.SoC < 0.999 {
		t.Fatalf("soc %v, want full", s.SoC)
	}
	settle(r, 5*time.Second)
	if s := r.Snapshot(); s.PowerW > 500 {
		t.Fatalf("full rack still charging at %v W", s.PowerW)
	}
}

func TestRackFaultCollapsesOutput(t *testing.T) {
	r := NewRack(DefaultRack())
	r.SetSetpoint(10_000)
	settle(r, 5*time.Second)
	r.SetFault(true)
	r.Tick(100 * time.Millisecond)
	s := r.Snapshot()
	if s.PowerW != 0 || s.Status != StatusFault {
		t.Fatalf("faulted rack: power=%v status=%v", s.PowerW, s.Status)
	}
}

func TestRegisterRoundTrip(t *testing.T) {
	cfg := DefaultRack()
	cfg.InitialSoC = 0.42
	r := NewRack(cfg)
	r.SetSetpoint(-12_345)
	regs := r.Registers()
	if got := regs[RegSoC-RegionStart]; got != 4200 {
		t.Errorf("soc reg %d, want 4200", got)
	}
	if got := DecodeI32(regs[RegSetpoint-RegionStart], regs[RegSetpoint-RegionStart+1]); got != -12_345 {
		t.Errorf("setpoint decodes to %d", got)
	}
	if got := DecodeI32(regs[RegCapacity-RegionStart], regs[RegCapacity-RegionStart+1]); got != 50_000 {
		t.Errorf("capacity decodes to %d", got)
	}
}

func TestPlantRoutesUnitIDsAndWrites(t *testing.T) {
	p := NewPlant(3, DefaultRack())

	// Write a setpoint to unit 2 only.
	w := &modbus.HoldingRegistersRequest{
		UnitId: 2, Addr: RegSetpoint, IsWrite: true,
		Args: []uint16{uint16(uint32(5000) >> 16), uint16(uint32(5000))},
	}
	if _, err := p.HandleHoldingRegisters(w); err != nil {
		t.Fatal(err)
	}
	if got := p.Racks()[1].Snapshot().SetpointW; got != 5000 {
		t.Fatalf("unit 2 setpoint %v", got)
	}
	if got := p.Racks()[0].Snapshot().SetpointW; got != 0 {
		t.Fatalf("unit 1 setpoint leaked: %v", got)
	}

	// Read back unit 2's region.
	rd := &modbus.HoldingRegistersRequest{UnitId: 2, Addr: RegionStart, Quantity: RegionLen}
	regs, err := p.HandleHoldingRegisters(rd)
	if err != nil {
		t.Fatal(err)
	}
	if got := DecodeI32(regs[RegSetpoint-RegionStart], regs[RegSetpoint-RegionStart+1]); got != 5000 {
		t.Fatalf("read-back setpoint %d", got)
	}

	// Unknown unit and out-of-range reads error.
	if _, err := p.HandleHoldingRegisters(&modbus.HoldingRegistersRequest{UnitId: 9, Addr: RegionStart, Quantity: 1}); err == nil {
		t.Fatal("unit 9 should not exist")
	}
	if _, err := p.HandleHoldingRegisters(&modbus.HoldingRegistersRequest{UnitId: 1, Addr: 50, Quantity: 1}); err == nil {
		t.Fatal("address 50 should be illegal")
	}
	// Writes anywhere but the setpoint are rejected.
	bad := &modbus.HoldingRegistersRequest{UnitId: 1, Addr: RegSoC, IsWrite: true, Args: []uint16{0}}
	if _, err := p.HandleHoldingRegisters(bad); err == nil {
		t.Fatal("soc must not be writable")
	}
}

func TestPlantEvenRackSpreadIsCentered(t *testing.T) {
	p := NewPlant(4, DefaultRack())
	want := []float64{0.47, 0.49, 0.51, 0.53}
	for i, rack := range p.Racks() {
		if got := rack.Snapshot().SoC; got != want[i] {
			t.Fatalf("rack %d SoC %v, want %v", i+1, got, want[i])
		}
	}
}

func TestPlantCapsRackCountAtUnitIDLimit(t *testing.T) {
	if got := len(NewPlant(MaxRacks+1, DefaultRack()).Racks()); got != MaxRacks {
		t.Fatalf("rack count %d, want %d", got, MaxRacks)
	}
}

func TestControlMuxRejectsRackIDOverflow(t *testing.T) {
	p := NewPlant(3, DefaultRack())
	req := httptest.NewRequest(http.MethodPost, "/rack/257/fault?on=1", nil)
	res := httptest.NewRecorder()
	p.ControlMux().ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want %d", res.Code, http.StatusBadRequest)
	}
	if p.Racks()[0].Snapshot().Faulted {
		t.Fatal("overflowing rack ID changed rack 1")
	}
}
