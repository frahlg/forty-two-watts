// Package ha is the Home Assistant MQTT bridge: MQTT autodiscovery +
// periodic state publish + command subscriber for mode/target/peak/ev.
//
// Uses the same site sign convention as the rest of the app. HA users see
// grid_w as + import / − export, PV as negative (generation), battery as
// + charge / − discharge. That matches everyone else's conventions so
// HA charts can be dropped in without sign fiddling.
package ha

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/control"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// CommandCallbacks is how the bridge hands received commands back to the
// control loop. Caller provides these at construction time.
type CommandCallbacks struct {
	SetMode             func(string) error
	SetGridTarget       func(float64) error
	SetPeakLimit        func(float64) error
	SetEVCharging       func(float64, bool) error
	SetBatteryCoversEV  func(bool) error
}

// Bridge is an instance of the HA MQTT bridge.
type Bridge struct {
	cfg         *config.HomeAssistant
	client      paho.Client
	tel         *telemetry.Store
	ctrl        *control.State
	ctrlMu      *sync.Mutex
	cb          CommandCallbacks
	driverNames []string

	stop chan struct{}
	done chan struct{}

	topicPrefix string // e.g. "forty-two-watts"
	discoPrefix string // e.g. "homeassistant"
	deviceID    string

	mu               sync.Mutex
	lastPublishMs    int64
	sensorsAnnounced int
}

// IsConnected returns true if the Paho MQTT client currently has an
// active connection to the broker.
func (b *Bridge) IsConnected() bool {
	if b == nil || b.client == nil {
		return false
	}
	return b.client.IsConnected()
}

// BrokerAddr returns the configured "host:port" string for diagnostics.
func (b *Bridge) BrokerAddr() string {
	if b == nil || b.cfg == nil {
		return ""
	}
	return fmt.Sprintf("%s:%d", b.cfg.Broker, b.cfg.Port)
}

// LastPublishMs is the Unix milliseconds when the last state publish
// went out. 0 if nothing has been published yet.
func (b *Bridge) LastPublishMs() int64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastPublishMs
}

// SensorsAnnounced is the count of HA-discovery sensors we registered
// on connect. Non-zero means the auto-discovery worked.
func (b *Bridge) SensorsAnnounced() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sensorsAnnounced
}

// Start connects to the HA broker, publishes autodiscovery, and begins
// periodic state publishes. Returns immediately; the goroutine runs until
// Stop() is called.
func Start(
	cfg *config.HomeAssistant,
	tel *telemetry.Store,
	ctrl *control.State, ctrlMu *sync.Mutex,
	driverNames []string,
	cb CommandCallbacks,
) (*Bridge, error) {
	b := &Bridge{
		cfg:         cfg,
		tel:         tel,
		ctrl:        ctrl,
		ctrlMu:      ctrlMu,
		cb:          cb,
		driverNames: driverNames,
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		topicPrefix: "forty-two-watts",
		discoPrefix: "homeassistant",
		deviceID:    "forty_two_watts",
	}
	opts := paho.NewClientOptions().
		AddBroker(fmt.Sprintf("tcp://%s:%d", cfg.Broker, cfg.Port)).
		SetClientID("forty-two-watts-ha").
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(10 * time.Second).
		SetOnConnectHandler(func(_ paho.Client) {
			slog.Info("HA MQTT connected", "broker", cfg.Broker)
			b.publishDiscovery()
			b.subscribeCommands()
		})
	if cfg.Username != "" { opts.SetUsername(cfg.Username) }
	if cfg.Password != "" { opts.SetPassword(cfg.Password) }
	b.client = paho.NewClient(opts)
	if tok := b.client.Connect(); tok.WaitTimeout(10*time.Second) && tok.Error() != nil {
		return nil, tok.Error()
	}

	go b.publishLoop()
	return b, nil
}

// Stop disconnects and waits for the publish loop to exit.
func (b *Bridge) Stop() {
	close(b.stop)
	<-b.done
	b.client.Disconnect(500)
}

// ---- Autodiscovery ----

// discoveryDevice is the device block embedded in every discovery message
// so HA groups all the sensors under one device page.
func (b *Bridge) discoveryDevice() map[string]any {
	return map[string]any{
		"identifiers":  []string{b.deviceID},
		"name":         "forty-two-watts",
		"manufacturer": "Sourceful",
		"model":        "Home EMS",
	}
}

// publishDiscovery registers all sensors and controls with HA. Called on
// every reconnect — HA de-dupes by unique_id so it's safe to re-publish.
func (b *Bridge) publishDiscovery() {
	dev := b.discoveryDevice()

	// ---- Sensors (site level) ----
	sensors := []struct {
		id, name, unit, devClass string
		state                    string // the topic to read from
	}{
		{"grid_power", "Grid Power", "W", "power", b.stateTopic("grid_w")},
		{"pv_power", "PV Power", "W", "power", b.stateTopic("pv_w")},
		{"battery_power", "Battery Power", "W", "power", b.stateTopic("bat_w")},
		{"load_power", "Load Power", "W", "power", b.stateTopic("load_w")},
		{"battery_soc", "Battery SoC", "%", "battery", b.stateTopic("bat_soc_pct")},
		{"grid_target", "Grid Target", "W", "power", b.stateTopic("grid_target_w")},
	}
	for _, s := range sensors {
		msg := map[string]any{
			"name":                s.name,
			"unique_id":           b.deviceID + "_" + s.id,
			"state_topic":         s.state,
			"unit_of_measurement": s.unit,
			"device_class":        s.devClass,
			"device":              dev,
		}
		data, _ := json.Marshal(msg)
		topic := fmt.Sprintf("%s/sensor/%s/%s/config", b.discoPrefix, b.deviceID, s.id)
		b.publish(topic, data, true) // retained
	}

	// ---- Mode as HA select ----
	modeMsg := map[string]any{
		"name":             "Mode",
		"unique_id":        b.deviceID + "_mode",
		"state_topic":      b.stateTopic("mode"),
		"command_topic":    b.cmdTopic("mode"),
		"options":          []string{"idle", "self_consumption", "peak_shaving", "charge", "priority", "weighted"},
		"device":           dev,
	}
	data, _ := json.Marshal(modeMsg)
	b.publish(fmt.Sprintf("%s/select/%s/mode/config", b.discoPrefix, b.deviceID), data, true)

	// ---- Grid target as HA number ----
	targetMsg := map[string]any{
		"name":                "Grid Target",
		"unique_id":           b.deviceID + "_grid_target_cmd",
		"state_topic":         b.stateTopic("grid_target_w"),
		"command_topic":       b.cmdTopic("grid_target_w"),
		"min":                 -10000,
		"max":                 10000,
		"step":                50,
		"unit_of_measurement": "W",
		"device":              dev,
	}
	data, _ = json.Marshal(targetMsg)
	b.publish(fmt.Sprintf("%s/number/%s/grid_target/config", b.discoPrefix, b.deviceID), data, true)

	// ---- Battery-covers-EV as HA switch ----
	// Operator-facing override: when ON, the control loop lets the battery
	// discharge into the EV (price-arbitrage scenarios). Default OFF.
	bceMsg := map[string]any{
		"name":          "Battery Covers EV",
		"unique_id":     b.deviceID + "_battery_covers_ev_cmd",
		"state_topic":   b.stateTopic("battery_covers_ev"),
		"command_topic": b.cmdTopic("battery_covers_ev"),
		"payload_on":    "ON",
		"payload_off":   "OFF",
		"state_on":      "ON",
		"state_off":     "OFF",
		"icon":          "mdi:car-battery",
		"device":        dev,
	}
	data, _ = json.Marshal(bceMsg)
	b.publish(fmt.Sprintf("%s/switch/%s/battery_covers_ev/config", b.discoPrefix, b.deviceID), data, true)

	// ---- Per-driver sensors ----
	for _, name := range b.driverNames {
		for _, s := range []struct{ id, label, unit, class string }{
			{"_meter_w", " Meter Power", "W", "power"},
			{"_pv_w", " PV Power", "W", "power"},
			{"_bat_w", " Battery Power", "W", "power"},
			{"_bat_soc_pct", " Battery SoC", "%", "battery"},
		} {
			msg := map[string]any{
				"name":                name + s.label,
				"unique_id":           b.deviceID + "_" + name + s.id,
				"state_topic":         b.driverTopic(name, s.id[1:]),
				"unit_of_measurement": s.unit,
				"device_class":        s.class,
				"device":              dev,
			}
			data, _ := json.Marshal(msg)
			topic := fmt.Sprintf("%s/sensor/%s/%s%s/config", b.discoPrefix, b.deviceID, name, s.id)
			b.publish(topic, data, true)
		}
	}
	// Count total sensors announced (site + per-driver).
	b.mu.Lock()
	b.sensorsAnnounced = len(sensors) + len(b.driverNames)*5 // 5 per driver
	b.mu.Unlock()
}

// ---- Command subscriber ----

func (b *Bridge) subscribeCommands() {
	b.client.Subscribe(b.cmdTopic("mode"), 0, func(_ paho.Client, m paho.Message) {
		mode := string(m.Payload())
		if b.cb.SetMode != nil {
			if err := b.cb.SetMode(mode); err != nil {
				slog.Warn("HA set mode failed", "err", err)
			}
		}
	})
	b.client.Subscribe(b.cmdTopic("grid_target_w"), 0, func(_ paho.Client, m paho.Message) {
		f, err := strconv.ParseFloat(string(m.Payload()), 64)
		if err != nil { return }
		if b.cb.SetGridTarget != nil {
			_ = b.cb.SetGridTarget(f)
		}
	})
	b.client.Subscribe(b.cmdTopic("peak_limit_w"), 0, func(_ paho.Client, m paho.Message) {
		f, err := strconv.ParseFloat(string(m.Payload()), 64)
		if err != nil { return }
		if b.cb.SetPeakLimit != nil {
			_ = b.cb.SetPeakLimit(f)
		}
	})
	b.client.Subscribe(b.cmdTopic("ev_charging_w"), 0, func(_ paho.Client, m paho.Message) {
		f, err := strconv.ParseFloat(string(m.Payload()), 64)
		if err != nil { return }
		if b.cb.SetEVCharging != nil {
			_ = b.cb.SetEVCharging(f, f > 0)
		}
	})
	b.client.Subscribe(b.cmdTopic("battery_covers_ev"), 0, func(_ paho.Client, m paho.Message) {
		on := string(m.Payload()) == "ON"
		if b.cb.SetBatteryCoversEV != nil {
			_ = b.cb.SetBatteryCoversEV(on)
		}
	})
}

// ---- State publish loop ----

func (b *Bridge) publishLoop() {
	defer close(b.done)
	interval := time.Duration(b.cfg.PublishIntervalS) * time.Second
	if interval <= 0 { interval = 5 * time.Second }
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-b.stop:
			return
		case <-t.C:
			b.publishState()
		}
	}
}

func (b *Bridge) publishState() {
	// Record the publish tick so /api/ha/status can show liveness.
	b.mu.Lock()
	b.lastPublishMs = time.Now().UnixMilli()
	b.mu.Unlock()
	// Site-level aggregates
	b.ctrlMu.Lock()
	siteMeter := b.ctrl.SiteMeterDriver
	mode := string(b.ctrl.Mode)
	gridTarget := b.ctrl.GridTargetW
	batteryCoversEV := b.ctrl.BatteryCoversEV
	b.ctrlMu.Unlock()

	gridW := 0.0
	if r := b.tel.Get(siteMeter, telemetry.DerMeter); r != nil {
		gridW = r.SmoothedW
	}
	var pvW, batW, sumSoC float64
	var socCount int
	for _, r := range b.tel.ReadingsByType(telemetry.DerPV) { pvW += r.SmoothedW }
	for _, r := range b.tel.ReadingsByType(telemetry.DerBattery) {
		batW += r.SmoothedW
		if r.SoC != nil {
			sumSoC += *r.SoC
			socCount++
		}
	}
	avgSoC := 0.0
	if socCount > 0 { avgSoC = sumSoC / float64(socCount) }
	loadW := gridW - batW - pvW
	if loadW < 0 { loadW = 0 }

	b.publishValue("grid_w", gridW)
	b.publishValue("pv_w", pvW)
	b.publishValue("bat_w", batW)
	b.publishValue("load_w", loadW)
	b.publishValue("bat_soc_pct", avgSoC*100)
	b.publishValue("grid_target_w", gridTarget)
	b.publishString("mode", mode)
	bceState := "OFF"
	if batteryCoversEV {
		bceState = "ON"
	}
	b.publishString("battery_covers_ev", bceState)

	// Per-driver
	for _, name := range b.driverNames {
		if r := b.tel.Get(name, telemetry.DerMeter); r != nil {
			b.publishDriver(name, "meter_w", r.SmoothedW)
		}
		if r := b.tel.Get(name, telemetry.DerPV); r != nil {
			b.publishDriver(name, "pv_w", r.SmoothedW)
		}
		if r := b.tel.Get(name, telemetry.DerBattery); r != nil {
			b.publishDriver(name, "bat_w", r.SmoothedW)
			if r.SoC != nil {
				b.publishDriver(name, "bat_soc_pct", *r.SoC*100)
			}
		}
	}
}

// ---- Helpers ----

func (b *Bridge) stateTopic(name string) string { return b.topicPrefix + "/state/" + name }
func (b *Bridge) cmdTopic(name string) string   { return b.topicPrefix + "/cmd/" + name }
func (b *Bridge) driverTopic(driver, field string) string {
	return fmt.Sprintf("%s/driver/%s/%s", b.topicPrefix, driver, field)
}

func (b *Bridge) publishValue(name string, v float64) {
	b.publish(b.stateTopic(name), []byte(strconv.FormatFloat(v, 'f', 2, 64)), false)
}
func (b *Bridge) publishString(name string, s string) {
	b.publish(b.stateTopic(name), []byte(s), false)
}
func (b *Bridge) publishDriver(driver, field string, v float64) {
	b.publish(b.driverTopic(driver, field), []byte(strconv.FormatFloat(v, 'f', 2, 64)), false)
}
func (b *Bridge) publish(topic string, payload []byte, retained bool) {
	tok := b.client.Publish(topic, 0, retained, payload)
	tok.WaitTimeout(3 * time.Second)
}

// unused import suppressors
var _ = context.Background
