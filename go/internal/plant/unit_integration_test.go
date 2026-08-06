package plant

import (
	"fmt"
	"math"
	"net"
	"testing"
	"time"

	mbserver "github.com/simonvetter/modbus"

	"github.com/srcfl/ftw/go/cmd/sim-pcs/pcs"
)

// Spins the real sim-pcs Modbus server and drives it through a real
// Unit — the register contract is exercised on both ends.
func TestUnitAgainstSimPCS(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	plant := pcs.NewPlant(2, pcs.DefaultRack())
	srv, err := mbserver.NewServer(&mbserver.ServerConfiguration{
		URL: fmt.Sprintf("tcp://127.0.0.1:%d", port), Timeout: 5 * time.Second, MaxClients: 4,
	}, plant)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	u := NewUnit(UnitConfig{ID: "r1", Host: "127.0.0.1", Port: port, UnitID: 1})
	if err := u.Poll(); err != nil {
		t.Fatalf("poll: %v", err)
	}
	st, _, _ := u.State()
	if !st.Online || st.CapacityWh != 50000 || st.MaxChargeW != 25000 {
		t.Fatalf("state: %+v", st)
	}
	if math.Abs(st.SoC-0.5) > 0.05 {
		t.Fatalf("soc %v, want ≈0.5 (per-rack spread)", st.SoC)
	}

	// Write a setpoint, tick the sim, and watch actual power move.
	if err := u.WriteSetpoint(-8000); err != nil {
		t.Fatalf("write: %v", err)
	}
	for i := 0; i < 100; i++ {
		plant.Tick(100 * time.Millisecond)
	}
	if err := u.Poll(); err != nil {
		t.Fatal(err)
	}
	st, _, _ = u.State()
	if st.PowerW > -7000 {
		t.Fatalf("discharge did not engage: %v W", st.PowerW)
	}

	// Fault the rack: next poll must mark it offline.
	plant.Racks()[0].SetFault(true)
	plant.Tick(100 * time.Millisecond)
	if err := u.Poll(); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := u.State(); st.Online {
		t.Fatal("faulted rack still online")
	}
}
