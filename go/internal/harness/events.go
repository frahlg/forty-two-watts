package harness

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// EventWriter emits newline-delimited JSON events to an io.Writer. Every
// message is one JSON object followed by '\n' so downstream consumers
// (Tauri GUI, CLI piping to jq, test scripts) can line-split the stream.
//
// The harness routes five classes of event: log, emit, metric, identity,
// health, plus control events ready / stopped / error. All events share a
// common envelope {event, ts_ms, driver?}; extra fields vary by kind.
type EventWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func NewEventWriter(w io.Writer) *EventWriter {
	return &EventWriter{w: w}
}

func (e *EventWriter) write(obj map[string]any) {
	if _, ok := obj["ts_ms"]; !ok {
		obj["ts_ms"] = time.Now().UnixMilli()
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, _ = e.w.Write(data)
	_, _ = e.w.Write([]byte("\n"))
}

// Ready signals that a driver is initialised and the poll loop has started.
func (e *EventWriter) Ready(driver string) {
	e.write(map[string]any{"event": "ready", "driver": driver})
}

// Stopped signals the driver was torn down.
func (e *EventWriter) Stopped(driver string) {
	e.write(map[string]any{"event": "stopped", "driver": driver})
}

// Error reports an unrecoverable error to the consumer.
func (e *EventWriter) Error(msg string) {
	e.write(map[string]any{"event": "error", "msg": msg})
}

// Log emits a log line, from the driver (host.log) or the harness itself.
func (e *EventWriter) Log(driver, level, msg string, attrs map[string]any) {
	obj := map[string]any{"event": "log", "level": level, "msg": msg}
	if driver != "" {
		obj["driver"] = driver
	}
	if len(attrs) > 0 {
		obj["attrs"] = attrs
	}
	e.write(obj)
}

// Emit corresponds to a structured host.emit() call (meter/pv/battery/ev/vehicle).
// `data` is the raw JSON payload the driver passed in — preserved verbatim.
func (e *EventWriter) Emit(driver, derType string, rawW float64, soc *float64, data json.RawMessage, ts time.Time) {
	obj := map[string]any{
		"event":  "emit",
		"driver": driver,
		"type":   derType,
		"w":      rawW,
		"ts_ms":  ts.UnixMilli(),
	}
	if soc != nil {
		obj["soc"] = *soc
	}
	if len(data) > 0 {
		obj["data"] = data
	}
	e.write(obj)
}

// Metric corresponds to a host.emit_metric() call — scalar diagnostic.
func (e *EventWriter) Metric(driver, name string, value float64, tsMs int64) {
	e.write(map[string]any{
		"event":  "metric",
		"driver": driver,
		"name":   name,
		"value":  value,
		"ts_ms":  tsMs,
	})
}

// Identity snapshots the (make, sn, mac, endpoint) tuple the registry uses
// to compute a device_id. The harness emits it whenever any field changes.
func (e *EventWriter) Identity(driver, make, sn, mac, endpoint string) {
	e.write(map[string]any{
		"event":    "identity",
		"driver":   driver,
		"make":     make,
		"sn":       sn,
		"mac":      mac,
		"endpoint": endpoint,
	})
}

// Health mirrors telemetry.DriverHealth so the UI can show live status.
func (e *EventWriter) Health(driver, status string, consecutiveErrors int, tickCount uint64, lastError string) {
	e.write(map[string]any{
		"event":              "health",
		"driver":             driver,
		"status":             status,
		"consecutive_errors": consecutiveErrors,
		"tick_count":         tickCount,
		"last_error":         lastError,
	})
}
