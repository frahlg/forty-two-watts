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
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the full application config.
type Config struct {
	Site          Site               `yaml:"site" json:"site"`
	Fuse          Fuse               `yaml:"fuse" json:"fuse"`
	Drivers       []Driver           `yaml:"drivers" json:"drivers"`
	API           API                `yaml:"api" json:"api"`
	HomeAssistant *HomeAssistant     `yaml:"homeassistant,omitempty" json:"homeassistant,omitempty"`
	State         *StateConf         `yaml:"state,omitempty" json:"state,omitempty"`
	Price         *Price             `yaml:"price,omitempty" json:"price,omitempty"`
	Weather       *Weather           `yaml:"weather,omitempty" json:"weather,omitempty"`
	Planner       *Planner           `yaml:"planner,omitempty" json:"planner,omitempty"`
	Batteries     map[string]Battery `yaml:"batteries,omitempty" json:"batteries,omitempty"`
	OCPP          *OCPP              `yaml:"ocpp,omitempty" json:"ocpp,omitempty"`
	EVCharger     *EVCharger         `yaml:"ev_charger,omitempty" json:"ev_charger,omitempty"`
}

// OCPP configures the embedded OCPP 1.6J Central System for EV chargers.
// When enabled, EV chargers connect to ws://<bind>:<port>/<chargerId>
// and their power readings flow into telemetry.Store as DerEV samples,
// which the dispatch clamp uses to keep home batteries from feeding
// the car. See go/internal/ocpp.
type OCPP struct {
	Enabled            bool   `yaml:"enabled" json:"enabled"`
	Bind               string `yaml:"bind,omitempty" json:"bind,omitempty"`
	Port               int    `yaml:"port,omitempty" json:"port,omitempty"`
	Path               string `yaml:"path,omitempty" json:"path,omitempty"`
	Username           string `yaml:"username,omitempty" json:"username,omitempty"`
	Password           string `yaml:"password,omitempty" json:"password,omitempty"`
	HeartbeatIntervalS int    `yaml:"heartbeat_interval_s,omitempty" json:"heartbeat_interval_s,omitempty"`
}

// EVCharger is the high-level EV charger config written by the Settings UI.
// The backend auto-generates a Lua driver entry from this on startup so
// users never touch raw driver YAML for their EV charger.
//
// Password is stored in state.db (key "ev_charger_password"), NOT in config.yaml.
// It is populated at runtime by main.go after loading state and by the API
// handler on POST /api/config.
type EVCharger struct {
	Provider string `yaml:"provider" json:"provider"` // "easee" (only option for now)
	Email    string `yaml:"email" json:"email"`
	Password string `yaml:"-" json:"password"` // persisted in state.db, not YAML
	Serial   string `yaml:"serial,omitempty" json:"serial,omitempty"`
}

// Planner configures the MPC scheduler (optional — disabled if omitted).
// Mode: "self_consumption" (default) | "cheap_charge" | "arbitrage".
type Planner struct {
	Enabled             bool    `yaml:"enabled" json:"enabled"`
	Mode                string  `yaml:"mode,omitempty" json:"mode,omitempty"`
	BaseLoadW           float64 `yaml:"base_load_w,omitempty" json:"base_load_w,omitempty"`
	HorizonHours        int     `yaml:"horizon_hours,omitempty" json:"horizon_hours,omitempty"`
	IntervalMin         int     `yaml:"interval_min,omitempty" json:"interval_min,omitempty"`
	SoCMinPct           float64 `yaml:"soc_min_pct,omitempty" json:"soc_min_pct,omitempty"`
	SoCMaxPct           float64 `yaml:"soc_max_pct,omitempty" json:"soc_max_pct,omitempty"`
	ChargeEfficiency    float64 `yaml:"charge_efficiency,omitempty" json:"charge_efficiency,omitempty"`
	DischargeEfficiency float64 `yaml:"discharge_efficiency,omitempty" json:"discharge_efficiency,omitempty"`
	ExportOrePerKWh     float64 `yaml:"export_ore_per_kwh,omitempty" json:"export_ore_per_kwh,omitempty"` // 0 = use mean spot
}

// Site is the top-level control loop config.
type Site struct {
	Name                 string  `yaml:"name" json:"name"`
	ControlIntervalS     int     `yaml:"control_interval_s" json:"control_interval_s"`
	GridTargetW          float64 `yaml:"grid_target_w" json:"grid_target_w"`
	GridToleranceW       float64 `yaml:"grid_tolerance_w" json:"grid_tolerance_w"`
	WatchdogTimeoutS     int     `yaml:"watchdog_timeout_s" json:"watchdog_timeout_s"`
	SmoothingAlpha       float64 `yaml:"smoothing_alpha" json:"smoothing_alpha"`
	Gain                 float64 `yaml:"gain" json:"gain"`
	SlewRateW            float64 `yaml:"slew_rate_w" json:"slew_rate_w"`
	MinDispatchIntervalS int     `yaml:"min_dispatch_interval_s" json:"min_dispatch_interval_s"`
}

// Fuse describes the shared breaker limit used by the fuse guard.
type Fuse struct {
	MaxAmps float64 `yaml:"max_amps" json:"max_amps"`
	Phases  int     `yaml:"phases" json:"phases"`
	Voltage float64 `yaml:"voltage" json:"voltage"`
}

// MaxPowerW returns the total power budget for the fuse guard.
func (f Fuse) MaxPowerW() float64 {
	return f.MaxAmps * f.Voltage * float64(f.Phases)
}

// Driver is one driver entry. Each driver is a Lua script loaded by
// the driver host at startup (or on hot-reload via the file watcher).
type Driver struct {
	Name               string  `yaml:"name" json:"name"`
	Lua                string  `yaml:"lua,omitempty" json:"lua,omitempty"` // path to .lua file
	IsSiteMeter        bool    `yaml:"is_site_meter,omitempty" json:"is_site_meter,omitempty"`
	BatteryCapacityWh  float64 `yaml:"battery_capacity_wh,omitempty" json:"battery_capacity_wh,omitempty"`
	// Disabled skips this driver at startup / reload. Set via the UI when
	// you want to temporarily take a driver out without editing yaml.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
	// HasPassword is a JSON-only signal to the UI that Config["password"]
	// holds a non-empty value on disk. Populated by MaskSecrets after the
	// real password is blanked out so the operator can still tell apart
	// "never entered" from "saved but masked". Never written to yaml.
	HasPassword bool `yaml:"-" json:"has_password,omitempty"`

	// Capabilities: the resources this driver is allowed to use.
	// Unset capabilities are explicitly denied.
	Capabilities Capabilities `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`

	// Driver-specific config: arbitrary key/value map passed to
	// driver_init(config) in Lua. Used for credentials, device addresses,
	// thresholds, etc. that don't fit the generic capabilities model.
	Config map[string]any `yaml:"config,omitempty" json:"config,omitempty"`

	// Legacy protocol fields (equivalent to capabilities, still accepted
	// for backwards compatibility with master-branch configs).
	MQTT   *MQTTConfig   `yaml:"mqtt,omitempty" json:"mqtt,omitempty"`
	Modbus *ModbusConfig `yaml:"modbus,omitempty" json:"modbus,omitempty"`
}

// Capabilities explicitly scope what host resources a driver can access.
type Capabilities struct {
	MQTT   *MQTTConfig   `yaml:"mqtt,omitempty" json:"mqtt,omitempty"`
	Modbus *ModbusConfig `yaml:"modbus,omitempty" json:"modbus,omitempty"`
	HTTP   *HTTPCapability `yaml:"http,omitempty" json:"http,omitempty"`
}

// MQTTConfig grants access to one MQTT broker.
type MQTTConfig struct {
	Host     string `yaml:"host" json:"host"`
	Port     int    `yaml:"port,omitempty" json:"port,omitempty"` // default 1883
	Username string `yaml:"username,omitempty" json:"username,omitempty"`
	Password string `yaml:"password,omitempty" json:"password,omitempty"`
}

// ModbusConfig grants access to one Modbus TCP endpoint.
type ModbusConfig struct {
	Host   string `yaml:"host" json:"host"`
	Port   int    `yaml:"port,omitempty" json:"port,omitempty"`   // default 502
	UnitID int    `yaml:"unit_id,omitempty" json:"unit_id,omitempty"` // default 1
}

// HTTPCapability grants HTTP access to specific hostnames (future).
type HTTPCapability struct {
	AllowedHosts []string `yaml:"allowed_hosts" json:"allowed_hosts"`
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
	Port int `yaml:"port" json:"port"`
}

// HomeAssistant is the MQTT bridge config.
type HomeAssistant struct {
	Enabled          bool   `yaml:"enabled" json:"enabled"`
	Broker           string `yaml:"broker" json:"broker"`
	Port             int    `yaml:"port,omitempty" json:"port,omitempty"`
	Username         string `yaml:"username,omitempty" json:"username,omitempty"`
	Password         string `yaml:"password,omitempty" json:"password,omitempty"`
	PublishIntervalS int    `yaml:"publish_interval_s,omitempty" json:"publish_interval_s,omitempty"`
}

// StateConf is the persistent state DB config.
//
// Path is the SQLite file (default "state.db"). ColdDir is the directory
// where >14d-old time-series data is rolled off as Parquet, partitioned
// YYYY/MM/DD.parquet (default "cold/" alongside Path).
type StateConf struct {
	Path    string `yaml:"path" json:"path"`
	ColdDir string `yaml:"cold_dir" json:"cold_dir"`
}

// Price is the spot-price source config.
type Price struct {
	Provider         string  `yaml:"provider" json:"provider"` // elprisetjustnu | entsoe | none
	Zone             string  `yaml:"zone,omitempty" json:"zone,omitempty"`
	GridTariffOreKwh float64 `yaml:"grid_tariff_ore_kwh,omitempty" json:"grid_tariff_ore_kwh,omitempty"`
	VATPercent       float64 `yaml:"vat_percent,omitempty" json:"vat_percent,omitempty"`
	APIKey           string  `yaml:"api_key,omitempty" json:"api_key,omitempty"`

	// Currency is the ISO code for pricing (default "SEK"). ENTSOE
	// returns EUR/MWh; we convert using ECB daily FX rates.
	Currency string `yaml:"currency,omitempty" json:"currency,omitempty"`

	// ExportBonusOreKwh is a per-kWh bonus on top of spot when exporting.
	// Some retailers pay spot + fixed bonus (e.g. 60 öre in Sweden via
	// "skattereduktion" + electricity-certificate value). Default 0.
	ExportBonusOreKwh float64 `yaml:"export_bonus_ore_kwh,omitempty" json:"export_bonus_ore_kwh,omitempty"`

	// ExportFeeOreKwh is a per-kWh deduction on export (e.g. transmission
	// fees some DSOs charge for feed-in). Reduces effective export price.
	ExportFeeOreKwh float64 `yaml:"export_fee_ore_kwh,omitempty" json:"export_fee_ore_kwh,omitempty"`
}

// Weather is the weather-forecast source config.
type Weather struct {
	Provider  string  `yaml:"provider" json:"provider"` // met_no | openweather | none
	Latitude  float64 `yaml:"latitude" json:"latitude"`
	Longitude float64 `yaml:"longitude" json:"longitude"`
	APIKey    string  `yaml:"api_key,omitempty" json:"api_key,omitempty"`

	// PVRatedW is the system's nameplate PV output (W) — used as the
	// initial twin prior AND the ceiling for naive PV estimates. If 0,
	// we fall back to a heuristic (sum of battery_capacity_wh / 3),
	// which is only roughly right for homes where PV and storage were
	// sized together. Set explicitly for accurate day-1 forecasts.
	PVRatedW float64 `yaml:"pv_rated_w,omitempty" json:"pv_rated_w,omitempty"`

	// HeatingWPerDegC adds load proportional to max(18°C − outdoor_temp, 0).
	// A rough-but-useful way to teach the planner that cold nights cost
	// more than mild ones without running a full ML temperature fit.
	// Typical Swedish single-family values: 200–500 W/°C. 0 disables.
	HeatingWPerDegC float64 `yaml:"heating_w_per_degc,omitempty" json:"heating_w_per_degc,omitempty"`
}

// Battery is per-battery overrides (keyed by driver name in the top-level map).
type Battery struct {
	SoCMin        *float64 `yaml:"soc_min,omitempty" json:"soc_min,omitempty"`
	SoCMax        *float64 `yaml:"soc_max,omitempty" json:"soc_max,omitempty"`
	MaxChargeW    *float64 `yaml:"max_charge_w,omitempty" json:"max_charge_w,omitempty"`
	MaxDischargeW *float64 `yaml:"max_discharge_w,omitempty" json:"max_discharge_w,omitempty"`
	Weight        *float64 `yaml:"weight,omitempty" json:"weight,omitempty"`
}

// MaskSecrets returns a copy of the config with sensitive fields (passwords,
// API keys) replaced by empty strings so they are never exposed via the API.
// The original config is not modified.
func (c Config) MaskSecrets() Config {
	out := c

	if out.EVCharger != nil {
		cp := *out.EVCharger
		cp.Password = ""
		out.EVCharger = &cp
	}
	if out.HomeAssistant != nil {
		cp := *out.HomeAssistant
		cp.Password = ""
		out.HomeAssistant = &cp
	}
	if out.OCPP != nil {
		cp := *out.OCPP
		cp.Password = ""
		out.OCPP = &cp
	}
	if out.Price != nil {
		cp := *out.Price
		cp.APIKey = ""
		out.Price = &cp
	}
	if out.Weather != nil {
		cp := *out.Weather
		cp.APIKey = ""
		out.Weather = &cp
	}

	if len(out.Drivers) > 0 {
		drivers := make([]Driver, len(out.Drivers))
		copy(drivers, out.Drivers)
		for i := range drivers {
			if drivers[i].Config != nil {
				cp := make(map[string]any, len(drivers[i].Config))
				for k, v := range drivers[i].Config {
					cp[k] = v
				}
				if pw, has := cp["password"]; has {
					// Signal "stored" to the UI before we blank it out.
					if s, ok := pw.(string); ok && s != "" {
						drivers[i].HasPassword = true
					}
					cp["password"] = ""
				}
				drivers[i].Config = cp
			}
			if drivers[i].Capabilities.MQTT != nil {
				cp := *drivers[i].Capabilities.MQTT
				cp.Password = ""
				drivers[i].Capabilities.MQTT = &cp
			}
			if drivers[i].MQTT != nil {
				cp := *drivers[i].MQTT
				cp.Password = ""
				drivers[i].MQTT = &cp
			}
		}
		out.Drivers = drivers
	}

	return out
}

// PreserveMaskedSecrets copies real secrets from `existing` into `incoming`
// wherever the incoming value is empty (the UI sends "" for masked fields).
// Call this before saving a config received from the API.
func (incoming *Config) PreserveMaskedSecrets(existing *Config) {
	if incoming.EVCharger != nil && existing.EVCharger != nil && incoming.EVCharger.Password == "" {
		incoming.EVCharger.Password = existing.EVCharger.Password
	}
	if incoming.HomeAssistant != nil && existing.HomeAssistant != nil && incoming.HomeAssistant.Password == "" {
		incoming.HomeAssistant.Password = existing.HomeAssistant.Password
	}
	if incoming.OCPP != nil && existing.OCPP != nil && incoming.OCPP.Password == "" {
		incoming.OCPP.Password = existing.OCPP.Password
	}
	if incoming.Price != nil && existing.Price != nil && incoming.Price.APIKey == "" {
		incoming.Price.APIKey = existing.Price.APIKey
	}
	if incoming.Weather != nil && existing.Weather != nil && incoming.Weather.APIKey == "" {
		incoming.Weather.APIKey = existing.Weather.APIKey
	}
	for i := range incoming.Drivers {
		for _, ed := range existing.Drivers {
			if incoming.Drivers[i].Name != ed.Name {
				continue
			}
			if incoming.Drivers[i].Config != nil && ed.Config != nil {
				if pw, ok := incoming.Drivers[i].Config["password"]; ok {
					if pw == "" || pw == nil {
						incoming.Drivers[i].Config["password"] = ed.Config["password"]
					}
				}
			}
			// Restore MQTT password in capabilities block.
			if incoming.Drivers[i].Capabilities.MQTT != nil && ed.Capabilities.MQTT != nil &&
				incoming.Drivers[i].Capabilities.MQTT.Password == "" {
				incoming.Drivers[i].Capabilities.MQTT.Password = ed.Capabilities.MQTT.Password
			}
			// Restore MQTT password in legacy block.
			if incoming.Drivers[i].MQTT != nil && ed.MQTT != nil &&
				incoming.Drivers[i].MQTT.Password == "" {
				incoming.Drivers[i].MQTT.Password = ed.MQTT.Password
			}
			break
		}
	}
}

// Load parses a config file from disk. Returns a fully-validated Config.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return Parse(data, filepath.Dir(path))
}

// Parse parses config bytes and validates. baseDir resolves driver Lua paths.
func Parse(data []byte, baseDir string) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	applyDefaults(&c)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	c.ResolveDriverPaths(baseDir)
	return &c, nil
}

// ResolveDriverPaths joins relative Lua driver paths with baseDir.
func (c *Config) ResolveDriverPaths(baseDir string) {
	for i := range c.Drivers {
		c.Drivers[i].Lua = stripLeadingDotDot(c.Drivers[i].Lua)
		if c.Drivers[i].Lua != "" && !filepath.IsAbs(c.Drivers[i].Lua) {
			c.Drivers[i].Lua = filepath.Join(baseDir, c.Drivers[i].Lua)
		}
	}
}

func stripLeadingDotDot(p string) string {
	for strings.HasPrefix(p, "../") {
		p = p[3:]
	}
	return p
}

// UnresolveDriverPaths converts resolved driver paths back to config-relative form.
//
// Paths that are outside baseDir (filepath.Rel would yield a ../-prefixed
// result) are left absolute — otherwise the next ResolveDriverPaths would
// strip the leading ../ via stripLeadingDotDot and silently re-anchor the
// driver under baseDir.
func (c *Config) UnresolveDriverPaths(baseDir string) {
	for i := range c.Drivers {
		c.Drivers[i].Lua = relToBaseDir(baseDir, c.Drivers[i].Lua)
	}
}

func relToBaseDir(baseDir, p string) string {
	if p == "" {
		return p
	}
	rel, err := filepath.Rel(baseDir, p)
	if err != nil {
		return p
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return rel
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
		if d.Lua == "" {
			return fmt.Errorf("driver %q: must specify `lua`", d.Name)
		}
		if d.EffectiveMQTT() == nil && d.EffectiveModbus() == nil && d.Capabilities.HTTP == nil {
			return fmt.Errorf("driver %q: must have mqtt, modbus, or http capability", d.Name)
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
	// Driver paths are resolved to absolute-ish paths at Load() time.
	// Convert them back to config-relative before writing so that
	// repeated save cycles don't accumulate extra "../" prefixes.
	baseDir := filepath.Dir(path)
	out := *c
	if len(out.Drivers) > 0 {
		drivers := make([]Driver, len(out.Drivers))
		copy(drivers, out.Drivers)
		for i := range drivers {
			drivers[i].Lua = relDriverPath(baseDir, drivers[i].Lua)
		}
		out.Drivers = drivers
	}
	data, err := yaml.Marshal(&out)
	if err != nil {
		return fmt.Errorf("yaml marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	return os.Rename(tmp, path)
}

func relDriverPath(baseDir, p string) string {
	if p == "" {
		return ""
	}
	rel, err := filepath.Rel(baseDir, p)
	if err != nil {
		return p
	}
	return rel
}
