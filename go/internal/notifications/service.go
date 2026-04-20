// Package notifications delivers operator-facing push notifications via
// a pluggable provider (ntfy today; future providers register via
// RegisterProvider).
//
// The rule engine is driven by telemetry.DriverHealth snapshots passed in
// via Service.Observe. It supports two built-in event types today:
//
//   - driver_offline   — fires once per outage after a per-rule threshold
//   - driver_recovered — fires when a driver that previously tripped
//     driver_offline reports successful telemetry again
//
// The user-visible threshold is independent of site.watchdog_timeout_s:
// the watchdog is a safety shortcut that drops stale drivers to autonomous
// mode immediately, while notifications are here to tell the human after
// a longer-than-noise outage.
package notifications

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/events"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// Event types.
const (
	EventDriverOffline   = "driver_offline"
	EventDriverRecovered = "driver_recovered"
)

// Defaults for a freshly-seeded rule.
const (
	DefaultThresholdS = 600
	DefaultCooldownS  = 3600
)

// DefaultRules returns the built-in event types, disabled by default so
// the operator opts in per event via the UI.
func DefaultRules() []config.NotificationRule {
	return []config.NotificationRule{
		{Type: EventDriverOffline, Enabled: false, ThresholdS: DefaultThresholdS, Priority: 4, CooldownS: DefaultCooldownS},
		{Type: EventDriverRecovered, Enabled: false, Priority: 3, CooldownS: 0},
	}
}

// DeviceLookup resolves a driver name to its hardware-stable identity.
// Returns ok=false when no device has been registered yet (cold start).
type DeviceLookup = func(name string) (deviceID, makeStr, serial string, ok bool)

// Message is a rendered notification payload.
type Message struct {
	Title    string
	Body     string
	Priority int
	Tags     []string
}

// Publisher dispatches a rendered Message to its transport.
type Publisher interface {
	Publish(ctx context.Context, m Message) error
}

// Provider is a hot-reloadable Publisher. Implementations must be safe to
// call Publish from multiple goroutines and must honor SetConfig without
// dropping in-flight requests.
type Provider interface {
	Publisher
	Name() string
	SetConfig(cfg *config.Notifications)
}

// ProviderFactory builds a Provider from the top-level Notifications cfg.
type ProviderFactory func(cfg *config.Notifications) Provider

var (
	providersMu sync.RWMutex
	providers   = map[string]ProviderFactory{}
)

// RegisterProvider wires a new transport into the registry. Called from
// provider init() functions. Builtin "ntfy" is registered below.
func RegisterProvider(name string, factory ProviderFactory) {
	providersMu.Lock()
	defer providersMu.Unlock()
	providers[name] = factory
}

// NewProvider constructs the provider named by cfg.Provider (defaulting to
// "ntfy") using the registry. Returns nil if the provider is unknown — the
// Service treats that as disabled.
func NewProvider(cfg *config.Notifications) Provider {
	if cfg == nil {
		return nil
	}
	name := cfg.Provider
	if name == "" {
		name = "ntfy"
	}
	providersMu.RLock()
	f := providers[name]
	providersMu.RUnlock()
	if f == nil {
		return nil
	}
	return f(cfg)
}

// Status is a snapshot of the service state for the UI.
type Status struct {
	Enabled     bool   `json:"enabled"`
	Provider    string `json:"provider,omitempty"`
	Server      string `json:"server,omitempty"`
	Topic       string `json:"topic,omitempty"`
	Sent        uint64 `json:"sent"`
	Failed      uint64 `json:"failed"`
	RuleCount   int    `json:"rule_count"`
	ActiveAlert int    `json:"active_alerts"`
}

// Service is the rule engine + dispatcher.
//
// All public pointer methods are safe on a nil receiver — main.go always
// constructs a Service, but tests sometimes pass nil Deps so handlers
// must remain nil-safe.
type Service struct {
	mu           sync.Mutex
	cfg          *config.Notifications
	pub          Publisher
	lookup       DeviceLookup
	lastFired    map[string]time.Time
	alreadyFired map[string]bool
	activeAlert  map[string]bool
	sent         uint64
	failed       uint64
	now          func() time.Time
}

// New constructs a Service. cfg may be nil (no-op until Reload is called).
func New(cfg *config.Notifications, pub Publisher, lookup DeviceLookup) *Service {
	return &Service{
		cfg:          cfg,
		pub:          pub,
		lookup:       lookup,
		lastFired:    map[string]time.Time{},
		alreadyFired: map[string]bool{},
		activeAlert:  map[string]bool{},
		now:          time.Now,
	}
}

// SetPublisher swaps the transport. Used by main.go's reload applier to
// install a provider built from fresh config when the notifications
// section was absent at startup (or when the provider type changed).
func (s *Service) SetPublisher(pub Publisher) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.pub = pub
	s.mu.Unlock()
}

// Reload swaps the config. Preserves lastFired (cooldown must survive a
// settings toggle) but clears per-outage state so freshly-enabled rules
// don't fire retroactively. If the current publisher implements Provider
// it also gets the fresh config (hot-reload without reconstruction).
func (s *Service) Reload(cfg *config.Notifications) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cfg = cfg
	pub := s.pub
	s.alreadyFired = map[string]bool{}
	s.activeAlert = map[string]bool{}
	s.mu.Unlock()
	if p, ok := pub.(Provider); ok {
		p.SetConfig(cfg)
	}
}

// Subscribe wires this service onto a shared event bus. The core control
// loop publishes HealthTick each tick; the API's /test endpoint publishes
// NotificationTest. Neither producer knows about notifications internals.
func (s *Service) Subscribe(bus *events.Bus) {
	if s == nil || bus == nil {
		return
	}
	bus.Subscribe(events.KindHealthTick, func(e events.Event) {
		ev, ok := e.(events.HealthTick)
		if !ok {
			return
		}
		s.observeAt(ev.Health, ev.Now)
	})
	bus.Subscribe(events.KindNotificationTest, func(e events.Event) {
		ev, ok := e.(events.NotificationTest)
		if !ok {
			return
		}
		err := s.SendTest()
		if ev.Reply != nil {
			select {
			case ev.Reply <- err:
			default:
			}
		}
	})
}

// Enabled reports whether the top-level toggle is on.
func (s *Service) Enabled() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg != nil && s.cfg.Enabled
}

// Status returns a read-only snapshot for the UI.
func (s *Service) Status() Status {
	if s == nil {
		return Status{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Status{Sent: s.sent, Failed: s.failed}
	if s.cfg != nil {
		out.Enabled = s.cfg.Enabled
		out.Provider = s.cfg.Provider
		if s.cfg.Ntfy != nil {
			out.Server = s.cfg.Ntfy.Server
			out.Topic = s.cfg.Ntfy.Topic
		}
		out.RuleCount = len(s.cfg.Events)
	}
	for _, v := range s.activeAlert {
		if v {
			out.ActiveAlert++
		}
	}
	return out
}

// Observe is a compatibility wrapper; prefer the event-bus route.
func (s *Service) Observe(health map[string]telemetry.DriverHealth) {
	if s == nil {
		return
	}
	s.observeAt(health, s.now())
}

func (s *Service) observeAt(health map[string]telemetry.DriverHealth, now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.cfg == nil || !s.cfg.Enabled {
		s.mu.Unlock()
		return
	}
	rules := s.cfg.Events
	cfg := s.cfg
	type dispatch struct {
		rule config.NotificationRule
		data templateData
	}
	var pending []dispatch

	for _, rule := range rules {
		if !rule.Enabled || rule.Type == "" {
			continue
		}
		for driver, h := range health {
			var since time.Duration
			if h.LastSuccess != nil {
				since = now.Sub(*h.LastSuccess)
			}
			key := rule.Type + "|" + driver

			switch rule.Type {
			case EventDriverOffline:
				threshold := time.Duration(rule.ThresholdS) * time.Second
				if threshold == 0 {
					threshold = time.Duration(DefaultThresholdS) * time.Second
				}
				// Cold start: never-started driver shouldn't alarm.
				if h.LastSuccess == nil && h.TickCount == 0 {
					continue
				}
				// Fresh — clear the per-outage latch so the NEXT outage
				// can fire. Don't touch activeAlert (that belongs to the
				// recovered rule's state machine).
				if since < threshold {
					delete(s.alreadyFired, key)
					continue
				}
				if s.alreadyFired[key] {
					continue
				}
				if rule.CooldownS > 0 {
					if last, ok := s.lastFired[key]; ok && now.Sub(last) < time.Duration(rule.CooldownS)*time.Second {
						continue
					}
				}
				s.alreadyFired[key] = true
				s.activeAlert[driver] = true
				s.lastFired[key] = now
				pending = append(pending, dispatch{rule, s.buildData(driver, rule.Type, since, now)})
			case EventDriverRecovered:
				if !s.activeAlert[driver] {
					continue
				}
				stillStale := h.LastSuccess == nil || since > 30*time.Second
				if stillStale {
					continue
				}
				if rule.CooldownS > 0 {
					if last, ok := s.lastFired[key]; ok && now.Sub(last) < time.Duration(rule.CooldownS)*time.Second {
						delete(s.activeAlert, driver)
						continue
					}
				}
				delete(s.activeAlert, driver)
				s.lastFired[key] = now
				pending = append(pending, dispatch{rule, s.buildData(driver, rule.Type, since, now)})
			}
		}
	}
	s.mu.Unlock()

	for _, d := range pending {
		s.dispatch(cfg, d.rule, d.data)
	}
}

// SendTest renders and publishes a synthetic notification. Errors when disabled.
func (s *Service) SendTest() error {
	if s == nil {
		return fmt.Errorf("notifications not configured")
	}
	s.mu.Lock()
	cfg := s.cfg
	pub := s.pub
	s.mu.Unlock()
	if cfg == nil || !cfg.Enabled {
		return fmt.Errorf("notifications disabled")
	}
	if pub == nil {
		return fmt.Errorf("notifications: no publisher")
	}
	prio := cfg.DefaultPriority
	if prio <= 0 {
		prio = 3
	}
	msg := Message{
		Title:    "forty-two-watts: test notification",
		Body:     fmt.Sprintf("Test notification sent at %s.", time.Now().UTC().Format(time.RFC3339)),
		Priority: prio,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := pub.Publish(ctx, msg)
	s.mu.Lock()
	if err != nil {
		s.failed++
	} else {
		s.sent++
	}
	s.mu.Unlock()
	return err
}

// templateData is the {{.}} for rule templates.
type templateData struct {
	Device    string
	DeviceID  string
	Make      string
	Serial    string
	EventType string
	DurationS int
	Duration  string
	Timestamp string
}

func (s *Service) buildData(driver, eventType string, since time.Duration, now time.Time) templateData {
	td := templateData{
		Device:    driver,
		EventType: eventType,
		DurationS: int(since / time.Second),
		Duration:  humanDuration(since),
		Timestamp: now.UTC().Format(time.RFC3339),
	}
	if s.lookup != nil {
		if id, mk, sn, ok := s.lookup(driver); ok {
			td.DeviceID = id
			td.Make = mk
			td.Serial = sn
		}
	}
	return td
}

func (s *Service) dispatch(cfg *config.Notifications, rule config.NotificationRule, data templateData) {
	titleTpl := rule.TitleTemplate
	if strings.TrimSpace(titleTpl) == "" {
		titleTpl = defaultTitleFor(rule.Type)
	}
	bodyTpl := rule.BodyTemplate
	if strings.TrimSpace(bodyTpl) == "" {
		bodyTpl = defaultBodyFor(rule.Type)
	}
	title, err := renderTemplate("title", titleTpl, data)
	if err != nil {
		slog.Warn("notifications: title render failed", "event", rule.Type, "err", err)
		s.bumpFailed()
		return
	}
	body, err := renderTemplate("body", bodyTpl, data)
	if err != nil {
		slog.Warn("notifications: body render failed", "event", rule.Type, "err", err)
		s.bumpFailed()
		return
	}
	prio := rule.Priority
	if prio == 0 {
		prio = cfg.DefaultPriority
	}
	msg := Message{
		Title:    strings.TrimSpace(title),
		Body:     strings.TrimSpace(body),
		Priority: prio,
		Tags:     splitTags(rule.Tags),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.pub.Publish(ctx, msg); err != nil {
		slog.Warn("notifications: publish failed", "event", rule.Type, "driver", data.Device, "err", err)
		s.bumpFailed()
		return
	}
	slog.Info("notifications: sent", "event", rule.Type, "driver", data.Device)
	s.bumpSent()
}

func (s *Service) bumpSent() {
	s.mu.Lock()
	s.sent++
	s.mu.Unlock()
}

func (s *Service) bumpFailed() {
	s.mu.Lock()
	s.failed++
	s.mu.Unlock()
}

// EventDefaults returns the built-in title/body template for every known
// event type. Exposed via the API so the UI can pre-fill form inputs
// with exactly what the backend will render when the operator leaves the
// custom template blank.
func EventDefaults() map[string]struct {
	Title string `json:"title"`
	Body  string `json:"body"`
} {
	types := []string{EventDriverOffline, EventDriverRecovered}
	out := make(map[string]struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}, len(types))
	for _, t := range types {
		out[t] = struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		}{Title: defaultTitleFor(t), Body: defaultBodyFor(t)}
	}
	return out
}

func defaultTitleFor(eventType string) string {
	switch eventType {
	case EventDriverOffline:
		return "forty-two-watts: {{.Device}} offline"
	case EventDriverRecovered:
		return "forty-two-watts: {{.Device}} recovered"
	}
	return "forty-two-watts: {{.EventType}}"
}

func defaultBodyFor(eventType string) string {
	switch eventType {
	case EventDriverOffline:
		return "{{.Device}} has not reported telemetry for {{.Duration}}."
	case EventDriverRecovered:
		return "{{.Device}} is reporting telemetry again."
	}
	return "{{.EventType}} for {{.Device}}"
}

func renderTemplate(name, tpl string, data any) (string, error) {
	t, err := template.New(name).Parse(tpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func splitTags(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// humanDuration formats a duration in the short style used in defaults.
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}
