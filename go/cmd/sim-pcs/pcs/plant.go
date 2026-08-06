package pcs

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/simonvetter/modbus"
)

// Plant is N racks addressed as Modbus unit IDs 1..N on one TCP server.
type Plant struct {
	racks []*Rack // index 0 = unit ID 1
}

func NewPlant(n int, cfg RackConfig) *Plant {
	if n < 1 {
		n = 1
	}
	p := &Plant{}
	for i := 0; i < n; i++ {
		rc := cfg
		// Slight per-rack SoC spread so allocation/balancing logic has
		// something real to balance from the first tick.
		rc.InitialSoC = clamp01(cfg.InitialSoC + float64(i-(n-1)/2)*0.02)
		p.racks = append(p.racks, NewRack(rc))
	}
	return p
}

func (p *Plant) Racks() []*Rack { return p.racks }

func (p *Plant) rack(unitID uint8) (*Rack, bool) {
	i := int(unitID) - 1
	if i < 0 || i >= len(p.racks) {
		return nil, false
	}
	return p.racks[i], true
}

// Tick advances every rack.
func (p *Plant) Tick(dt time.Duration) {
	for _, r := range p.racks {
		r.Tick(dt)
	}
}

// HandleCoils implements modbus.RequestHandler.
func (p *Plant) HandleCoils(*modbus.CoilsRequest) ([]bool, error) {
	return nil, modbus.ErrIllegalFunction
}

func (p *Plant) HandleDiscreteInputs(*modbus.DiscreteInputsRequest) ([]bool, error) {
	return nil, modbus.ErrIllegalFunction
}

func (p *Plant) HandleHoldingRegisters(req *modbus.HoldingRegistersRequest) ([]uint16, error) {
	r, ok := p.rack(req.UnitId)
	if !ok {
		return nil, modbus.ErrIllegalFunction
	}
	if r.Snapshot().CommsLost {
		// Simulate a dead unit: no reply until the client times out.
		time.Sleep(6 * time.Second)
		return nil, modbus.ErrServerDeviceFailure
	}
	if req.IsWrite {
		// Only the setpoint is writable.
		if req.Addr == RegSetpoint && len(req.Args) == 2 {
			r.SetSetpoint(float64(DecodeI32(req.Args[0], req.Args[1])))
			return nil, nil
		}
		return nil, modbus.ErrIllegalDataAddress
	}
	regs := r.Registers()
	start := int(req.Addr) - RegionStart
	end := start + int(req.Quantity)
	if start < 0 || end > len(regs) {
		return nil, modbus.ErrIllegalDataAddress
	}
	return regs[start:end], nil
}

func (p *Plant) HandleInputRegisters(req *modbus.InputRegistersRequest) ([]uint16, error) {
	hr := &modbus.HoldingRegistersRequest{
		UnitId: req.UnitId, Addr: req.Addr, Quantity: req.Quantity,
	}
	return p.HandleHoldingRegisters(hr)
}

// ControlMux serves the fault-injection HTTP API used by tests/e2e:
//
//	GET  /state                     → JSON of every rack
//	POST /rack/{n}/fault?on=1|0     → toggle a rack fault
//	POST /rack/{n}/comms?on=1|0     → toggle comms loss
//	POST /rack/{n}/soc?v=0.42       → pin SoC
func (p *Plant) ControlMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, _ *http.Request) {
		out := make([]RackState, 0, len(p.racks))
		for _, r := range p.racks {
			out = append(out, r.Snapshot())
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	rackFrom := func(r *http.Request) (*Rack, error) {
		n, err := strconv.Atoi(r.PathValue("n"))
		if err != nil {
			return nil, fmt.Errorf("bad rack %q", r.PathValue("n"))
		}
		rack, ok := p.rack(uint8(n))
		if !ok {
			return nil, fmt.Errorf("no rack %d", n)
		}
		return rack, nil
	}
	onOff := func(r *http.Request) bool { return r.URL.Query().Get("on") == "1" }
	mux.HandleFunc("POST /rack/{n}/fault", func(w http.ResponseWriter, r *http.Request) {
		rack, err := rackFrom(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rack.SetFault(onOff(r))
	})
	mux.HandleFunc("POST /rack/{n}/comms", func(w http.ResponseWriter, r *http.Request) {
		rack, err := rackFrom(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rack.SetCommsLost(onOff(r))
	})
	mux.HandleFunc("POST /rack/{n}/soc", func(w http.ResponseWriter, r *http.Request) {
		rack, err := rackFrom(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		v, err := strconv.ParseFloat(r.URL.Query().Get("v"), 64)
		if err != nil || v < 0 || v > 1 {
			http.Error(w, "v must be 0..1", http.StatusBadRequest)
			return
		}
		rack.SetSoC(v)
	})
	return mux
}
