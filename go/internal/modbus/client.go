// Package modbus provides a Modbus TCP capability wrapper for drivers.
package modbus

import (
	"fmt"
	"sync"
	"time"

	sv "github.com/simonvetter/modbus"

	"github.com/frahlg/forty-two-watts/go/internal/drivers"
)

// Capability wraps a simonvetter/modbus client.
type Capability struct {
	mu     sync.Mutex
	client *sv.ModbusClient
}

// Dial opens a Modbus TCP connection.
func Dial(host string, port, unitID int) (*Capability, error) {
	cli, err := sv.NewClient(&sv.ClientConfiguration{
		URL:     fmt.Sprintf("tcp://%s:%d", host, port),
		Timeout: 5 * time.Second,
	})
	if err != nil { return nil, err }
	if err := cli.Open(); err != nil { return nil, err }
	// unitID is ignored in TCP (already in PDU) but some devices require a specific value
	if unitID > 0 {
		_ = cli.SetUnitId(uint8(unitID))
	}
	return &Capability{client: cli}, nil
}

// Close the underlying connection.
func (c *Capability) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client.Close()
}

// Read — implements drivers.ModbusCap.
func (c *Capability) Read(addr, count uint16, kind int32) ([]uint16, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var rt sv.RegType
	switch kind {
	case drivers.ModbusInput: rt = sv.INPUT_REGISTER
	case drivers.ModbusHolding: rt = sv.HOLDING_REGISTER
	default: rt = sv.INPUT_REGISTER
	}
	return c.client.ReadRegisters(addr, count, rt)
}

// WriteSingle — implements drivers.ModbusCap.
func (c *Capability) WriteSingle(addr, value uint16) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client.WriteRegister(addr, value)
}

// WriteMulti — implements drivers.ModbusCap.
func (c *Capability) WriteMulti(addr uint16, values []uint16) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client.WriteRegisters(addr, values)
}
