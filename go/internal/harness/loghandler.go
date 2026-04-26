package harness

import (
	"context"
	"log/slog"
)

// driverLogHandler is a slog.Handler that routes every log record into the
// event stream as a "log" event. Driver log lines flow through slog.Default
// (because HostEnv.Logger = slog.With("driver", name) in drivers/host.go),
// so installing this as the default captures host.log() calls from Lua and
// the registry's own lifecycle logs with zero changes to the driver package.
type driverLogHandler struct {
	out   *EventWriter
	attrs []slog.Attr
	group string
}

func newDriverLogHandler(out *EventWriter) *driverLogHandler {
	return &driverLogHandler{out: out}
}

func (h *driverLogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelDebug
}

func (h *driverLogHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]any{}
	for _, a := range h.attrs {
		attrs[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	// "driver" attr is what HostEnv.Logger sets; "name" is what the registry
	// uses for its own add/remove/restart logs. Either identifies the driver
	// this record belongs to.
	var driver string
	if v, ok := attrs["driver"].(string); ok {
		driver = v
		delete(attrs, "driver")
	} else if v, ok := attrs["name"].(string); ok {
		driver = v
		delete(attrs, "name")
	}
	h.out.Log(driver, levelString(r.Level), r.Message, attrs)
	return nil
}

func (h *driverLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &next
}

func (h *driverLogHandler) WithGroup(name string) slog.Handler {
	next := *h
	next.group = name
	return &next
}

func levelString(l slog.Level) string {
	switch {
	case l <= slog.LevelDebug:
		return "debug"
	case l <= slog.LevelInfo:
		return "info"
	case l <= slog.LevelWarn:
		return "warn"
	default:
		return "error"
	}
}
