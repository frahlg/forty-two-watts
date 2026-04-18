package drivers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// ErrNoCapability is returned by host functions the driver wasn't granted.
var ErrNoCapability = errors.New("capability not granted")

// MQTTCap is the interface the host implements to give a driver MQTT access.
// Each driver gets its own instance bound to its configured broker.
type MQTTCap interface {
	Subscribe(topic string) error
	Publish(topic string, payload []byte) error
	// PopMessages returns and clears any buffered messages received since
	// the last call.
	PopMessages() []MQTTMessage
	// Close disconnects the underlying client. Called by Registry.Remove
	// so a driver restart doesn't leak a paho session under the same
	// clientID. Safe to call on an already-closed cap.
	Close() error
}

// MQTTMessage is one inbound MQTT message.
type MQTTMessage struct {
	Topic   string `json:"topic"`
	Payload string `json:"payload"` // raw bytes as UTF-8 string
}

// ModbusCap is the interface for Modbus TCP access.
type ModbusCap interface {
	Read(addr uint16, count uint16, kind int32) ([]uint16, error)
	WriteSingle(addr uint16, value uint16) error
	WriteMulti(addr uint16, values []uint16) error
	// Close tears down the TCP connection. Called on driver remove.
	Close() error
}

// HostEnv is the per-driver runtime context. Captures capabilities (potentially
// nil if not granted), the shared telemetry store, and identifying info.
type HostEnv struct {
	DriverName string
	Logger     *slog.Logger
	Telemetry  *telemetry.Store
	MQTT       MQTTCap    // nil → mqtt_* calls return ErrNoCapability
	Modbus     ModbusCap  // nil → modbus_* calls return ErrNoCapability
	HTTP       bool       // false → http_* calls return ErrNoCapability
	Start      time.Time  // monotonic start; host.millis() computed from here

	mu sync.Mutex
	// Desired poll interval — driver can set via host.set_poll_interval OR
	// return it from driver_poll. We persist the last hint here.
	PollIntervalMS int32
	// Identity set by driver / capability layer.
	// Make + SN are reported via host.set_make / host.set_sn.
	// Endpoint is the protocol+host+port string set by the registry when
	// it wires the capability (see WithEndpoint).
	Make     string
	SN       string
	MAC      string // resolved by ARP after first connection (best-effort)
	Endpoint string // e.g. "modbus://192.168.1.1:502" or "mqtt://broker:1883"
}

// NewHostEnv creates a fresh host environment for a driver.
func NewHostEnv(name string, tel *telemetry.Store) *HostEnv {
	return &HostEnv{
		DriverName:     name,
		Logger:         slog.With("driver", name),
		Telemetry:      tel,
		Start:          time.Now(),
		PollIntervalMS: 5000,
	}
}

// WithMQTT binds an MQTT capability to this host.
func (h *HostEnv) WithMQTT(m MQTTCap) *HostEnv { h.MQTT = m; return h }

// WithModbus binds a Modbus capability.
func (h *HostEnv) WithModbus(m ModbusCap) *HostEnv { h.Modbus = m; return h }

// WithHTTP enables the HTTP capability.
func (h *HostEnv) WithHTTP() *HostEnv { h.HTTP = true; return h }

// millis returns monotonic milliseconds since host startup.
func (h *HostEnv) millis() int64 {
	return time.Since(h.Start).Milliseconds()
}

const (
	logDebug int32 = 0
	logInfo  int32 = 1
	logWarn  int32 = 2
	logError int32 = 3
)

const (
	ModbusCoil     int32 = 0
	ModbusDiscrete int32 = 1
	ModbusHolding  int32 = 2
	ModbusInput    int32 = 3
)

func (h *HostEnv) log(level int32, msg string) {
	switch level {
	case logDebug:
		h.Logger.Debug(msg)
	case logInfo:
		h.Logger.Info(msg)
	case logWarn:
		h.Logger.Warn(msg)
	case logError:
		h.Logger.Error(msg)
	default:
		h.Logger.Info(msg)
	}
}

// setPollInterval records the driver's requested poll interval.
func (h *HostEnv) setPollInterval(ms int32) {
	h.mu.Lock()
	h.PollIntervalMS = ms
	h.mu.Unlock()
}

// emitTelemetry accepts a JSON telemetry blob from the driver and routes it
// into the telemetry store. Expected shape:
//
//	{"type": "meter"|"pv"|"battery", "w": 123.4, "soc": 0.5 (optional), ...}
//
// Extra fields are preserved in the reading's Data payload so the UI/API can
// surface them verbatim.
func (h *HostEnv) emitTelemetry(rawJSON []byte) error {
	var env struct {
		Type string   `json:"type"`
		W    float64  `json:"w"`
		SoC  *float64 `json:"soc,omitempty"`
	}
	if err := json.Unmarshal(rawJSON, &env); err != nil {
		return fmt.Errorf("emit_telemetry: invalid json: %w", err)
	}
	t, err := telemetry.ParseDerType(env.Type)
	if err != nil {
		return err
	}
	if h.Telemetry != nil {
		h.Telemetry.Update(h.DriverName, t, env.W, env.SoC, rawJSON)
	}
	// Successful emit counts as a tick for health
	if h.Telemetry != nil {
		h.Telemetry.DriverHealthMut(h.DriverName).RecordSuccess()
	}
	return nil
}

// emitMetric buffers a scalar diagnostic metric for the long-format TS DB.
// Driver authors call this for anything beyond the standard pv/battery/meter
// shape — temperatures, voltages, frequencies, MPPT currents, etc.
func (h *HostEnv) emitMetric(name string, value float64) {
	if h.Telemetry == nil { return }
	h.Telemetry.EmitMetric(h.DriverName, name, value)
}

// setSN records the device serial number.
func (h *HostEnv) setSN(sn string) {
	h.mu.Lock(); h.SN = sn; h.mu.Unlock()
}

// setMake records the device manufacturer.
func (h *HostEnv) setMake(m string) {
	h.mu.Lock(); h.Make = m; h.mu.Unlock()
}

// PollInterval returns the driver's current requested poll cadence.
func (h *HostEnv) PollInterval() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.PollIntervalMS <= 0 {
		return 5 * time.Second
	}
	return time.Duration(h.PollIntervalMS) * time.Millisecond
}

// Identity returns (make, serial) set by the driver.
func (h *HostEnv) Identity() (make, sn string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.Make, h.SN
}

// FullIdentity returns every identity bit known to the host so callers
// (the registry) can compute a stable device_id.
func (h *HostEnv) FullIdentity() (make, sn, mac, endpoint string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.Make, h.SN, h.MAC, h.Endpoint
}

// SetEndpoint records the protocol-specific connection string for this
// driver so it can participate in device_id resolution. Called by main
// when wiring the MQTT/Modbus capability.
func (h *HostEnv) SetEndpoint(ep string) {
	h.mu.Lock(); h.Endpoint = ep; h.mu.Unlock()
}

// SetMAC records the L2 hardware address discovered via ARP.
func (h *HostEnv) SetMAC(mac string) {
	h.mu.Lock(); h.MAC = mac; h.mu.Unlock()
}

// ---- MQTT proxy ----

func (h *HostEnv) mqttSubscribe(ctx context.Context, topic string) error {
	if h.MQTT == nil { return ErrNoCapability }
	return h.MQTT.Subscribe(topic)
}

func (h *HostEnv) mqttPublish(ctx context.Context, topic string, payload []byte) error {
	if h.MQTT == nil { return ErrNoCapability }
	return h.MQTT.Publish(topic, payload)
}

func (h *HostEnv) mqttPollMessages() ([]MQTTMessage, error) {
	if h.MQTT == nil { return nil, ErrNoCapability }
	return h.MQTT.PopMessages(), nil
}

// ---- Modbus proxy ----

func (h *HostEnv) modbusRead(addr, count uint16, kind int32) ([]uint16, error) {
	if h.Modbus == nil { return nil, ErrNoCapability }
	return h.Modbus.Read(addr, count, kind)
}

func (h *HostEnv) modbusWriteSingle(addr, value uint16) error {
	if h.Modbus == nil { return ErrNoCapability }
	return h.Modbus.WriteSingle(addr, value)
}

func (h *HostEnv) modbusWriteMulti(addr uint16, values []uint16) error {
	if h.Modbus == nil { return ErrNoCapability }
	return h.Modbus.WriteMulti(addr, values)
}
