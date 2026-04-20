package notifications

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/events"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// ---- test helpers ----

type fakePub struct {
	mu    sync.Mutex
	msgs  []Message
	errOn int // fail after this many (0 means never)
	count int
	fail  bool
}

func (f *fakePub) Publish(_ context.Context, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count++
	if f.fail {
		return fmt.Errorf("boom")
	}
	f.msgs = append(f.msgs, m)
	return nil
}

func (f *fakePub) Messages() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Message, len(f.msgs))
	copy(out, f.msgs)
	return out
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newSvc(cfg *config.Notifications, pub Publisher) (*Service, *clock) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	s := New(cfg, pub, nil)
	s.now = clk.now
	return s, clk
}

func healthOk(lastSuccess time.Time) map[string]telemetry.DriverHealth {
	return map[string]telemetry.DriverHealth{
		"ferroamp": {Name: "ferroamp", LastSuccess: &lastSuccess, TickCount: 10},
	}
}

func healthStale(lastSuccess time.Time) map[string]telemetry.DriverHealth {
	return map[string]telemetry.DriverHealth{
		"ferroamp": {Name: "ferroamp", LastSuccess: &lastSuccess, TickCount: 10, Status: telemetry.StatusOffline},
	}
}

func baseCfg() *config.Notifications {
	return &config.Notifications{
		Enabled:         true,
		Provider:        "ntfy",
		DefaultPriority: 3,
		Ntfy:            &config.NtfyConfig{Server: "https://example", Topic: "test"},
		Events: []config.NotificationRule{
			{Type: EventDriverOffline, Enabled: true, ThresholdS: 600, Priority: 4, CooldownS: 3600},
			{Type: EventDriverRecovered, Enabled: true, Priority: 3},
		},
	}
}

// ---- tests ----

func TestOfflineFiresAfterThreshold(t *testing.T) {
	pub := &fakePub{}
	svc, clk := newSvc(baseCfg(), pub)
	last := clk.now()
	// Below threshold — no fire.
	clk.advance(400 * time.Second)
	svc.Observe(healthOk(last))
	if n := len(pub.Messages()); n != 0 {
		t.Fatalf("below threshold: got %d msgs", n)
	}
	// Cross threshold — one fire.
	clk.advance(400 * time.Second)
	svc.Observe(healthStale(last))
	if n := len(pub.Messages()); n != 1 {
		t.Fatalf("above threshold: got %d msgs", n)
	}
}

func TestOfflineFiresOncePerOutage(t *testing.T) {
	pub := &fakePub{}
	svc, clk := newSvc(baseCfg(), pub)
	last := clk.now()
	clk.advance(800 * time.Second)
	for i := 0; i < 20; i++ {
		svc.Observe(healthStale(last))
		clk.advance(5 * time.Second)
	}
	if n := len(pub.Messages()); n != 1 {
		t.Fatalf("expected 1 fire per outage, got %d", n)
	}
}

func TestCooldownBlocksBackToBack(t *testing.T) {
	pub := &fakePub{}
	svc, clk := newSvc(baseCfg(), pub)
	last := clk.now()
	clk.advance(700 * time.Second)
	svc.Observe(healthStale(last)) // fires
	// Recover
	last2 := clk.now()
	clk.advance(5 * time.Second)
	svc.Observe(healthOk(last2)) // recovered fires
	// New outage inside cooldown
	clk.advance(100 * time.Second)
	svc.Observe(healthStale(last2)) // threshold not yet met
	clk.advance(700 * time.Second)
	svc.Observe(healthStale(last2)) // threshold met but cooldown blocks
	msgs := pub.Messages()
	// Expect: offline-1 + recovered-1, no second offline due to cooldown.
	offlineCount := 0
	for _, m := range msgs {
		if strings.Contains(m.Title, "offline") {
			offlineCount++
		}
	}
	if offlineCount != 1 {
		t.Fatalf("expected 1 offline fire, got %d (%+v)", offlineCount, msgs)
	}
}

func TestDisabledServiceNoops(t *testing.T) {
	cfg := baseCfg()
	cfg.Enabled = false
	pub := &fakePub{}
	svc, clk := newSvc(cfg, pub)
	last := clk.now()
	clk.advance(1000 * time.Second)
	svc.Observe(healthStale(last))
	if n := len(pub.Messages()); n != 0 {
		t.Fatalf("disabled should not publish, got %d", n)
	}
	if err := svc.SendTest(); err == nil {
		t.Fatalf("SendTest should error when disabled")
	}
}

func TestSendTestProducesOneTestMsg(t *testing.T) {
	pub := &fakePub{}
	svc, _ := newSvc(baseCfg(), pub)
	if err := svc.SendTest(); err != nil {
		t.Fatalf("SendTest: %v", err)
	}
	msgs := pub.Messages()
	if len(msgs) != 1 {
		t.Fatalf("got %d msgs", len(msgs))
	}
	if !strings.Contains(strings.ToLower(msgs[0].Title), "test") {
		t.Fatalf("title missing 'test': %q", msgs[0].Title)
	}
}

func TestTemplateInterpolation(t *testing.T) {
	cfg := baseCfg()
	cfg.Events[0].TitleTemplate = "offline: {{.Device}}"
	cfg.Events[0].BodyTemplate = "dur: {{.Duration}} ({{.DurationS}}s)"
	pub := &fakePub{}
	svc, clk := newSvc(cfg, pub)
	last := clk.now()
	clk.advance(700 * time.Second)
	svc.Observe(healthStale(last))
	msgs := pub.Messages()
	if len(msgs) != 1 {
		t.Fatalf("got %d", len(msgs))
	}
	if msgs[0].Title != "offline: ferroamp" {
		t.Fatalf("title: %q", msgs[0].Title)
	}
	if !strings.Contains(msgs[0].Body, "11m") || !strings.Contains(msgs[0].Body, "700s") {
		t.Fatalf("body: %q", msgs[0].Body)
	}
}

func TestDefaultTemplatesWhenBlank(t *testing.T) {
	pub := &fakePub{}
	svc, clk := newSvc(baseCfg(), pub)
	last := clk.now()
	clk.advance(700 * time.Second)
	svc.Observe(healthStale(last))
	msgs := pub.Messages()
	if len(msgs) != 1 {
		t.Fatalf("got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Title, "ferroamp offline") {
		t.Fatalf("default title missing: %q", msgs[0].Title)
	}
	if !strings.Contains(msgs[0].Body, "has not reported telemetry") {
		t.Fatalf("default body missing: %q", msgs[0].Body)
	}
}

func TestNilServiceSafe(t *testing.T) {
	var s *Service
	s.Observe(nil)
	s.Reload(nil)
	s.Subscribe(nil)
	_ = s.Enabled()
	_ = s.Status()
	if err := s.SendTest(); err == nil {
		t.Error("nil SendTest must error")
	}
}

func TestRecoveredOnlyAfterOfflineFired(t *testing.T) {
	pub := &fakePub{}
	svc, clk := newSvc(baseCfg(), pub)
	// Healthy → healthy: zero msgs.
	last := clk.now()
	for i := 0; i < 5; i++ {
		last = clk.now()
		svc.Observe(healthOk(last))
		clk.advance(5 * time.Second)
	}
	if n := len(pub.Messages()); n != 0 {
		t.Fatalf("healthy→healthy: got %d", n)
	}
	// healthy → offline → healthy: expect 2 msgs.
	lockedLast := last
	clk.advance(700 * time.Second)
	svc.Observe(healthStale(lockedLast)) // offline
	newLast := clk.now()
	clk.advance(5 * time.Second)
	svc.Observe(healthOk(newLast)) // recovered
	if n := len(pub.Messages()); n != 2 {
		t.Fatalf("lifecycle: got %d msgs (%+v)", n, pub.Messages())
	}
}

func TestDisabledRuleSkipped(t *testing.T) {
	cfg := baseCfg()
	cfg.Events[0].Enabled = false
	pub := &fakePub{}
	svc, clk := newSvc(cfg, pub)
	last := clk.now()
	clk.advance(700 * time.Second)
	svc.Observe(healthStale(last))
	if n := len(pub.Messages()); n != 0 {
		t.Fatalf("disabled rule: got %d", n)
	}
}

func TestPublisherErrorBumpsFailed(t *testing.T) {
	pub := &fakePub{fail: true}
	svc, clk := newSvc(baseCfg(), pub)
	last := clk.now()
	clk.advance(700 * time.Second)
	svc.Observe(healthStale(last))
	st := svc.Status()
	if st.Failed != 1 {
		t.Fatalf("expected Failed=1, got %+v", st)
	}
}

// ---- ntfy provider ----

func TestNtfyHeadersAndBearer(t *testing.T) {
	var got *http.Request
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	p := newNtfyProvider(&config.Notifications{
		Provider: "ntfy",
		Ntfy:     &config.NtfyConfig{Server: srv.URL, Topic: "my topic", AccessToken: "tk"},
	})
	err := p.Publish(context.Background(), Message{Title: "T", Body: "B", Priority: 4, Tags: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestURI != "/my%20topic" {
		t.Errorf("request URI: %q", got.RequestURI)
	}
	if got.Header.Get("Title") != "T" {
		t.Errorf("Title header: %q", got.Header.Get("Title"))
	}
	if got.Header.Get("Priority") != "4" {
		t.Errorf("Priority header: %q", got.Header.Get("Priority"))
	}
	if got.Header.Get("Tags") != "a,b" {
		t.Errorf("Tags header: %q", got.Header.Get("Tags"))
	}
	if got.Header.Get("Authorization") != "Bearer tk" {
		t.Errorf("Authorization: %q", got.Header.Get("Authorization"))
	}
	if gotBody != "B" {
		t.Errorf("body: %q", gotBody)
	}
}

func TestNtfyBasicAuthFallback(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.WriteHeader(200)
	}))
	defer srv.Close()
	p := newNtfyProvider(&config.Notifications{
		Provider: "ntfy",
		Ntfy:     &config.NtfyConfig{Server: srv.URL, Topic: "t", Username: "u", Password: "p"},
	})
	if err := p.Publish(context.Background(), Message{Body: "x"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Header.Get("Authorization"), "Basic ") {
		t.Errorf("basic auth: %q", got.Header.Get("Authorization"))
	}
}

func TestNtfyNon2xxSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte("topic reserved"))
	}))
	defer srv.Close()
	p := newNtfyProvider(&config.Notifications{
		Provider: "ntfy",
		Ntfy:     &config.NtfyConfig{Server: srv.URL, Topic: "t"},
	})
	err := p.Publish(context.Background(), Message{Body: "x"})
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "topic reserved") {
		t.Fatalf("expected 403+body in error, got %v", err)
	}
}

func TestNtfyErrorsWhenUnset(t *testing.T) {
	p := newNtfyProvider(&config.Notifications{Provider: "ntfy", Ntfy: &config.NtfyConfig{}})
	if err := p.Publish(context.Background(), Message{Body: "x"}); err == nil {
		t.Error("expected error for empty server/topic")
	}
	p = newNtfyProvider(&config.Notifications{Provider: "ntfy", Ntfy: &config.NtfyConfig{Server: "http://x", Topic: ""}})
	if err := p.Publish(context.Background(), Message{Body: "x"}); err == nil {
		t.Error("expected error for empty topic")
	}
}

func TestHumanDurationBoundaries(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m"},
		{59 * time.Minute, "59m"},
		{time.Hour, "1h"},
		{time.Hour + 30*time.Minute, "1h30m"},
		{3 * time.Hour, "3h"},
	}
	for _, c := range cases {
		if got := humanDuration(c.d); got != c.want {
			t.Errorf("humanDuration(%v) = %q; want %q", c.d, got, c.want)
		}
	}
}

func TestStrategyRegistry(t *testing.T) {
	p := NewProvider(&config.Notifications{Provider: "ntfy", Ntfy: &config.NtfyConfig{Server: "http://x", Topic: "t"}})
	if p == nil || p.Name() != "ntfy" {
		t.Fatalf("ntfy not registered; got %+v", p)
	}
	if NewProvider(&config.Notifications{Provider: "nonesuch"}) != nil {
		t.Fatal("unknown provider must return nil")
	}
}

func TestEventBusSubscribe(t *testing.T) {
	pub := &fakePub{}
	svc, clk := newSvc(baseCfg(), pub)
	bus := events.NewBus()
	svc.Subscribe(bus)
	last := clk.now()
	clk.advance(700 * time.Second)
	bus.Publish(events.HealthTick{Health: healthStale(last), Now: clk.now()})
	if n := len(pub.Messages()); n != 1 {
		t.Fatalf("bus did not wire tick: got %d msgs", n)
	}
	// Test event round-trips via Reply chan.
	reply := make(chan error, 1)
	bus.Publish(events.NotificationTest{Reply: reply})
	select {
	case err := <-reply:
		if err != nil {
			t.Fatalf("test publish err: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("no reply from test event")
	}
}
