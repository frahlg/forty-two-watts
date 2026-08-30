package modbus

import (
	"errors"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/srcfl/ftw/go/internal/drivers"
)

// Engine owns Modbus TCP sessions keyed by host:port. Drivers, the
// debug probe, fingerprinting and the optional LAN proxy all go through
// it, so several Lua drivers can poll the same inverter (or different
// unit IDs behind one RS-485 gateway) without each opening a socket the
// device will not accept.
type Engine struct {
	mu       sync.Mutex
	sessions map[string]*leased
}

type leased struct {
	cap  *Capability
	refs int
}

// NewEngine returns an empty session pool. One Engine lives for the
// process; Dial stays a private connection for tests and one-shot probes
// that must not join the pool.
func NewEngine() *Engine {
	return &Engine{sessions: make(map[string]*leased)}
}

// Open returns a ModbusCap on the shared session for host:port. unitID is
// applied per request so two handles on the same socket can address
// different slaves. The socket stays up until every handle (drivers and
// the proxy pin) has Closed.
func (e *Engine) Open(host string, port, unitID int, allowUnverifiedLocal bool) (drivers.ModbusCap, error) {
	if e == nil {
		return nil, errors.New("modbus engine is nil")
	}
	if err := validateEndpoint(host, port, unitID); err != nil {
		return nil, err
	}
	key := sessionKey(host, port)

	e.mu.Lock()
	if s := e.sessions[key]; s != nil {
		s.refs++
		refs := s.refs
		cap := s.cap
		e.mu.Unlock()
		if refs == 2 {
			slog.Info("modbus session shared", "addr", key, "refs", refs)
		}
		return newHandle(e, key, unitID, cap), nil
	}
	e.mu.Unlock()

	cap, err := DialWithOptions(host, port, unitID, allowUnverifiedLocal)
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	if s := e.sessions[key]; s != nil {
		s.refs++
		refs := s.refs
		existing := s.cap
		e.mu.Unlock()
		_ = cap.Close()
		if refs == 2 {
			slog.Info("modbus session shared", "addr", key, "refs", refs)
		}
		return newHandle(e, key, unitID, existing), nil
	}
	e.sessions[key] = &leased{cap: cap, refs: 1}
	e.mu.Unlock()
	return newHandle(e, key, unitID, cap), nil
}

func (e *Engine) release(key string) error {
	e.mu.Lock()
	s := e.sessions[key]
	if s == nil {
		e.mu.Unlock()
		return nil
	}
	s.refs--
	if s.refs > 0 {
		e.mu.Unlock()
		return nil
	}
	delete(e.sessions, key)
	cap := s.cap
	e.mu.Unlock()
	if cap == nil {
		return nil
	}
	return cap.Close()
}

func (e *Engine) lookup(key string) *Capability {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.sessions[key]
	if s == nil {
		return nil
	}
	return s.cap
}

func (e *Engine) sessionCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.sessions)
}

func (e *Engine) refCount(key string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.sessions[key]
	if s == nil {
		return 0
	}
	return s.refs
}

func sessionKey(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

type handle struct {
	engine *Engine
	key    string
	unitID int
	cap    *Capability
	closed atomic.Bool
}

func newHandle(e *Engine, key string, unitID int, cap *Capability) *handle {
	return &handle{engine: e, key: key, unitID: unitID, cap: cap}
}

func (h *handle) Read(addr, count uint16, kind int32) ([]uint16, error) {
	if err := h.alive(); err != nil {
		return nil, err
	}
	return h.cap.readAs(h.unitID, addr, count, kind)
}

func (h *handle) WriteSingle(addr, value uint16) error {
	if err := h.alive(); err != nil {
		return err
	}
	return h.cap.writeSingleAs(h.unitID, addr, value)
}

func (h *handle) WriteMulti(addr uint16, values []uint16) error {
	if err := h.alive(); err != nil {
		return err
	}
	return h.cap.writeMultiAs(h.unitID, addr, values)
}

func (h *handle) Close() error {
	if !h.closed.CompareAndSwap(false, true) {
		return nil
	}
	return h.engine.release(h.key)
}

func (h *handle) alive() error {
	if h == nil || h.closed.Load() {
		return errors.New("modbus handle closed")
	}
	return nil
}

var _ drivers.ModbusCap = (*handle)(nil)
