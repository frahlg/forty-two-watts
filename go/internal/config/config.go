// Package config parses and validates the top-level YAML config.
//
// This is the single source of truth that the file-watcher re-parses on
// every change and that the settings UI writes back. All fields are
// hot-reloadable unless noted otherwise. See docs/configuration.md.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the full application config.
type Config struct {
	Site          Site               `yaml:"site"`
	Fuse          Fuse               `yaml:"fuse"`
	Drivers       []Driver           `yaml:"drivers"`
	API           API                `yaml:"api"`
	HomeAssistant *HomeAssistant     `yaml:"homeassistant,omitempty"`
	State         *StateConf         `yaml:"state,omitempty"`
	Price         *Price             `yaml:"price,omitempty"`
	Weather       *Weather           `yaml:"weather,omitempty"`
	Planner       *Planner           `yaml:"planner,omitempty"`
	Batteries     map[string]Battery `yaml:"batteries,omitempty"`
}

// Planner configures the MPC scheduler (optional — disabled if omitted).
// Mode: "self_consumption" (default) | "cheap_charge" | "arbitrage".
type Planner struct {
	Enabled             bool    `yaml:"enabled"`
	Mode                string  `yaml:"mode,omitempty"`
	BaseLoadW           float64 `yaml:"base_load_w,omitempty"`
	HorizonHours        int     `yaml:"horizon_hours,omitempty"`
	IntervalMin         int     `yaml:"interval_min,omitempty"`
	SoCMinPct           float64 `yaml:"soc_min_pct,omitempty"`
	SoCMaxPct           float64 `yaml:"soc_max_pct,omitempty"`
	ChargeEfficiency    float64 `yaml:"charge_efficiency,omitempty"`
	DischargeEfficiency float64 `yaml:"discharge_efficiency,omitempty"`
	ExportOrePerKWh     float64 `yaml:"export_ore_per_kwh,omitempty"` // 0 = use mean spot
}

// Site is the top-level control loop config.
type Site struct {
	Name                 string  `yaml:"name"`
	ControlIntervalS     int     `yaml:"control_interval_s"`
	GridTargetW          float64 `yaml:"grid_target_w"`
	GridToleranceW       float64 `yaml:"grid_tolerance_w"`
	WatchdogTimeoutS     int     `yaml:"watchdog_timeout_s"`
	SmoothingAlpha       float64 `yaml:"smoothing_alpha"`
	Gain                 float64 `yaml:"gain"`
	SlewRateW            float64 `yaml:"slew_rate_w"`
	MinDispatchIntervalS int     `yaml:"min_dispatch_interval_s"`
}

// Fuse describes the shared breaker limit used by the fuse guard.
type Fuse struct {
	MaxAmps float64 `yaml:"max_amps"`
	Phases  int     `yaml:"phases"`
	Voltage float64 `yaml:"voltage"`
}

// MaxPowerW returns the total power budget for the fuse guard.
func (f Fuse) MaxPowerW() float64 {
	return f.MaxAmps * f.Voltage * float64(f.Phases)
}

// Driver is one driver entry. In the Go/WASM port, WASM is the primary format.
// Lua is kept as a legacy fallback (not implemented in go-port initially).
type Driver struct {
	Name               string  `yaml:"name"`
	WASM               string  `yaml:"wasm,omitempty"` // path to .wasm file
	Lua                string  `yaml:"lua,omitempty"`  // legacy, path to .lua file
	IsSiteMeter        bool    `yaml:"is_site_meter,omitempty"`
	BatteryCapacityWh  float64 `yaml:"battery_capacity_wh,omitempty"`

	// Capabilities: the resources this driver is allowed to use.
	// Unset capabilities are explicitly denied.
	Capabilities Capabilities `yaml:"capabilities,omitempty"`

	// Legacy protocol fields (equivalent to capabilities, still accepted
	// for backwards compatibility with master-branch configs).
	MQTT   *MQTTConfig   `yaml:"mqtt,omitempty"`
	Modbus *ModbusConfig `yaml:"modbus,omitempty"`
}

// Capabilities explicitly scope what host resources a driver can access.
type Capabilities struct {
	MQTT   *MQTTConfig   `yaml:"mqtt,omitempty"`
	Modbus *ModbusConfig `yaml:"modbus,omitempty"`
	HTTP   *HTTPCapability `yaml:"http,omitempty"`
}

// MQTTConfig grants access to one MQTT broker.
type MQTTConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port,omitempty"` // default 1883
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
}

// ModbusConfig grants access to one Modbus TCP endpoint.
type ModbusConfig struct {
	Host   string `yaml:"host"`
	Port   int    `yaml:"port,omitempty"`   // default 502
	UnitID int    `yaml:"unit_id,omitempty"` // default 1
}

// HTTPCapability grants HTTP access to specific hostnames (future).
type HTTPCapability struct {
	AllowedHosts []string `yaml:"allowed_hosts"`
}

// EffectiveMQTT returns the driver's MQTT config, preferring capabilities over legacy.
func (d Driver) EffectiveMQTT() *MQTTConfig {
	if d.Capabilities.MQTT != nil {
		return d.Capabilities.MQTT
	}
	return d.MQTT
}

// EffectiveModbus returns the driver's Modbus config, preferring capabilities.
func (d Driver) EffectiveModbus() *ModbusConfig {
	if d.Capabilities.Modbus != nil {
		return d.Capabilities.Modbus
	}
	return d.Modbus
}

// API is the HTTP server config.
type API struct {
	Port int `yaml:"port"`
}

// HomeAssistant is the MQTT bridge config.
type HomeAssistant struct {
	Enabled          bool   `yaml:"enabled"`
	Broker           string `yaml:"broker"`
	Port             int    `yaml:"port,omitempty"`
	Username         string `yaml:"username,omitempty"`
	Password         string `yaml:"password,omitempty"`
	PublishIntervalS int    `yaml:"publish_interval_s,omitempty"`
}

// StateConf is the persistent state DB config.
type StateConf struct {
	Path string `yaml:"path"`
}

// Price is the spot-price source config.
type Price struct {
	Provider         string  `yaml:"provider"` // elprisetjustnu | entsoe | none
	Zone             string  `yaml:"zone,omitempty"`
	GridTariffOreKwh float64 `yaml:"grid_tariff_ore_kwh,omitempty"`
	VATPercent       float64 `yaml:"vat_percent,omitempty"`
	APIKey           string  `yaml:"api_key,omitempty"`
}

// Weather is the weather-forecast source config.
type Weather struct {
	Provider  string  `yaml:"provider"` // met_no | openweather | none
	Latitude  float64 `yaml:"latitude"`
	Longitude float64 `yaml:"longitude"`
	APIKey    string  `yaml:"api_key,omitempty"`
}

// Battery is per-battery overrides (keyed by driver name in the top-level map).
type Battery struct {
	SoCMin        *float64 `yaml:"soc_min,omitempty"`
	SoCMax        *float64 `yaml:"soc_max,omitempty"`
	MaxChargeW    *float64 `yaml:"max_charge_w,omitempty"`
	MaxDischargeW *float64 `yaml:"max_discharge_w,omitempty"`
	Weight        *float64 `yaml:"weight,omitempty"`
}

// Load parses a config file from disk. Returns a fully-validated Config.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return Parse(data, filepath.Dir(path))
}

// Parse parses config bytes and validates. baseDir resolves driver WASM paths.
func Parse(data []byte, baseDir string) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	applyDefaults(&c)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	// Resolve relative driver paths
	for i := range c.Drivers {
		if c.Drivers[i].WASM != "" && !filepath.IsAbs(c.Drivers[i].WASM) {
			c.Drivers[i].WASM = filepath.Join(baseDir, c.Drivers[i].WASM)
		}
		if c.Drivers[i].Lua != "" && !filepath.IsAbs(c.Drivers[i].Lua) {
			c.Drivers[i].Lua = filepath.Join(baseDir, c.Drivers[i].Lua)
		}
	}
	return &c, nil
}

// applyDefaults fills in sensible zero-value defaults.
func applyDefaults(c *Config) {
	if c.Site.ControlIntervalS == 0 {
		c.Site.ControlIntervalS = 5
	}
	if c.Site.GridToleranceW == 0 {
		c.Site.GridToleranceW = 42 // The Answer
	}
	if c.Site.WatchdogTimeoutS == 0 {
		c.Site.WatchdogTimeoutS = 60
	}
	if c.Site.SmoothingAlpha == 0 {
		c.Site.SmoothingAlpha = 0.3
	}
	if c.Site.Gain == 0 {
		c.Site.Gain = 0.5
	}
	if c.Site.SlewRateW == 0 {
		c.Site.SlewRateW = 500
	}
	if c.Site.MinDispatchIntervalS == 0 {
		c.Site.MinDispatchIntervalS = 5
	}
	if c.Fuse.Phases == 0 {
		c.Fuse.Phases = 3
	}
	if c.Fuse.Voltage == 0 {
		c.Fuse.Voltage = 230
	}
	if c.API.Port == 0 {
		c.API.Port = 8080
	}
	// Driver connection defaults
	for i := range c.Drivers {
		d := &c.Drivers[i]
		if cap := d.Capabilities.MQTT; cap != nil && cap.Port == 0 {
			cap.Port = 1883
		}
		if cap := d.Capabilities.Modbus; cap != nil {
			if cap.Port == 0 { cap.Port = 502 }
			if cap.UnitID == 0 { cap.UnitID = 1 }
		}
		if cap := d.MQTT; cap != nil && cap.Port == 0 {
			cap.Port = 1883
		}
		if cap := d.Modbus; cap != nil {
			if cap.Port == 0 { cap.Port = 502 }
			if cap.UnitID == 0 { cap.UnitID = 1 }
		}
	}
	if c.HomeAssistant != nil {
		if c.HomeAssistant.Port == 0 {
			c.HomeAssistant.Port = 1883
		}
		if c.HomeAssistant.PublishIntervalS == 0 {
			c.HomeAssistant.PublishIntervalS = 5
		}
	}
}

// Validate ensures the config is internally consistent and safe to run with.
func (c *Config) Validate() error {
	if len(c.Drivers) == 0 {
		return errors.New("at least one driver must be configured")
	}
	siteMeters := 0
	names := make(map[string]bool, len(c.Drivers))
	for _, d := range c.Drivers {
		if d.Name == "" {
			return errors.New("driver: name is required")
		}
		if names[d.Name] {
			return fmt.Errorf("driver %q: duplicate name", d.Name)
		}
		names[d.Name] = true

		if d.IsSiteMeter {
			siteMeters++
		}
		if d.WASM == "" && d.Lua == "" {
			return fmt.Errorf("driver %q: must specify `wasm` or `lua`", d.Name)
		}
		if d.EffectiveMQTT() == nil && d.EffectiveModbus() == nil {
			return fmt.Errorf("driver %q: must have mqtt or modbus capability", d.Name)
		}
	}
	if siteMeters == 0 {
		return errors.New("at least one driver must be is_site_meter: true")
	}

	if c.Site.SmoothingAlpha <= 0 || c.Site.SmoothingAlpha > 1 {
		return errors.New("site.smoothing_alpha must be in (0, 1]")
	}
	if c.Fuse.MaxAmps <= 0 {
		return errors.New("fuse.max_amps must be > 0")
	}
	return nil
}

// SiteMeterDriver returns the name of the driver marked is_site_meter.
func (c *Config) SiteMeterDriver() string {
	for _, d := range c.Drivers {
		if d.IsSiteMeter {
			return d.Name
		}
	}
	return ""
}

// SaveAtomic writes config to disk via tmp-file + rename. Safe from partial writes.
func SaveAtomic(path string, c *Config) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("yaml marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	return os.Rename(tmp, path)
}
