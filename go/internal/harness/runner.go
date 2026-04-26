package harness

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/drivers"
	"github.com/frahlg/forty-two-watts/go/internal/modbus"
	"github.com/frahlg/forty-two-watts/go/internal/mqtt"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// Spec collapses CLI flags / GUI form fields into one struct the runner
// translates into a config.Driver. Empty capability groups mean the driver
// does not get that capability (so host.mqtt_* / host.modbus_* / host.http_*
// calls return ErrNoCapability — exactly as in production).
type Spec struct {
	Name string
	Lua  string

	MQTTHost string
	MQTTPort int
	MQTTUser string
	MQTTPass string

	ModbusHost string
	ModbusPort int
	ModbusUnit int

	HTTP bool

	// ConfigFile is an optional YAML file whose top-level map is passed
	// to driver_init(config) in Lua — mirrors the `config:` block in
	// config.yaml.
	ConfigFile string

	// ConfigMap is an alternative to ConfigFile for in-process callers
	// (the forthcoming serve mode). Merged on top of ConfigFile if both
	// are set.
	ConfigMap map[string]any

	// Duration bounds the run. Zero = until ctx cancelled.
	Duration time.Duration
}

// Runner wires a Spec into a live driver and streams events for the
// duration of the run. Meant to be used once per driver lifetime; not
// thread-safe.
type Runner struct {
	spec Spec
	out  *EventWriter
	hlog *slog.Logger
}

func NewRunner(spec Spec, out *EventWriter, hlog *slog.Logger) *Runner {
	return &Runner{spec: spec, out: out, hlog: hlog}
}

func (r *Runner) Run(ctx context.Context) error {
	// Install the event-stream log handler as the slog default so driver
	// log calls (which go through the HostEnv.Logger = slog.With("driver", …)
	// chain) land in the event stream. Harness-own logs keep using r.hlog,
	// which is wired to stderr with its own handler by main.
	slog.SetDefault(slog.New(newDriverLogHandler(r.out)))

	cfg, err := r.buildDriverConfig()
	if err != nil {
		return fmt.Errorf("build driver config: %w", err)
	}

	tel := telemetry.NewStore()
	reg := drivers.NewRegistry(tel)
	reg.MQTTFactory = func(name string, c *config.MQTTConfig) (drivers.MQTTCap, error) {
		return mqtt.Dial(c.Host, c.Port, c.Username, c.Password, "ftw-harness-"+name)
	}
	reg.ModbusFactory = func(name string, c *config.ModbusConfig) (drivers.ModbusCap, error) {
		return modbus.Dial(c.Host, c.Port, c.UnitID)
	}
	// ARPLookup intentionally nil: harness may run on a different L2 than
	// the target device (laptop vs Pi) so MAC resolution is meaningless.
	// device_id will fall back to the endpoint hash, which is fine for
	// test output — identity is surfaced raw in the event stream anyway.

	if err := reg.Add(ctx, cfg); err != nil {
		return fmt.Errorf("add driver: %w", err)
	}
	defer reg.ShutdownAll()

	r.out.Ready(cfg.Name)
	r.hlog.Info("driver started", "name", cfg.Name, "lua", cfg.Lua)

	runCtx := ctx
	if r.spec.Duration > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, r.spec.Duration)
		defer cancel()
	}

	env := reg.Env(cfg.Name)
	newStreamer(r.out, tel, env, cfg.Name).run(runCtx)
	r.out.Stopped(cfg.Name)
	return nil
}

func (r *Runner) buildDriverConfig() (config.Driver, error) {
	d := config.Driver{Name: r.spec.Name, Lua: r.spec.Lua}
	if r.spec.MQTTHost != "" {
		d.Capabilities.MQTT = &config.MQTTConfig{
			Host:     r.spec.MQTTHost,
			Port:     r.spec.MQTTPort,
			Username: r.spec.MQTTUser,
			Password: r.spec.MQTTPass,
		}
	}
	if r.spec.ModbusHost != "" {
		d.Capabilities.Modbus = &config.ModbusConfig{
			Host:   r.spec.ModbusHost,
			Port:   r.spec.ModbusPort,
			UnitID: r.spec.ModbusUnit,
		}
	}
	if r.spec.HTTP {
		d.Capabilities.HTTP = &config.HTTPCapability{}
	}
	cfgMap := map[string]any{}
	if r.spec.ConfigFile != "" {
		data, err := os.ReadFile(r.spec.ConfigFile)
		if err != nil {
			return d, fmt.Errorf("read config file: %w", err)
		}
		if err := yaml.Unmarshal(data, &cfgMap); err != nil {
			return d, fmt.Errorf("parse config file: %w", err)
		}
	}
	for k, v := range r.spec.ConfigMap {
		cfgMap[k] = v
	}
	if len(cfgMap) > 0 {
		d.Config = cfgMap
	}
	return d, nil
}
