// Package selfupdate probes the GitHub Releases API for newer versions of
// forty-two-watts and triggers pull+restart via the ftw-updater sidecar
// over a Unix socket.
//
// The check is probe-only — nothing mutates the host until the user
// explicitly POSTs /api/version/update or /api/version/restart and the
// sidecar receives the signal on the shared update-ipc volume. See
// docs/self-update.md for the full architecture.
package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Store is the subset of state.Store methods this package needs. Declared as
// an interface so tests don't need a real SQLite DB.
type Store interface {
	SaveConfig(key, value string) error
	LoadConfig(key string) (string, bool)
}

const (
	skippedKey           = "update.skipped_version"
	defaultCheckInterval = 6 * time.Hour
	defaultHTTPTimeout   = 10 * time.Second
	// staleThreshold flags an in-flight update as failed when the sidecar
	// state file hasn't been refreshed within this window. Catches the
	// sidecar crashing mid-pull so the UI overlay can unblock.
	staleThreshold = 5 * time.Minute
)

// Config configures the Checker.
type Config struct {
	// Repo is the GitHub "owner/name" slug. Defaults to frahlg/forty-two-watts.
	Repo string
	// CurrentVersion is the running binary's version (from main.Version).
	CurrentVersion string
	// CheckInterval is the probe cadence. 0 = 6 h.
	CheckInterval time.Duration
	// SocketPath is where the sidecar listens. Empty disables Trigger.
	SocketPath string
	// StatusPath is the sidecar's state.json. Empty disables Status.
	StatusPath string

	// Overrides for tests.
	HTTPClient  *http.Client
	ReleasesURL string
	Now         func() time.Time
}

// Info is the cached view returned to the UI.
type Info struct {
	Current         string    `json:"current"`
	Latest          string    `json:"latest,omitempty"`
	PublishedAt     time.Time `json:"published_at,omitempty"`
	ReleaseNotesURL string    `json:"release_notes_url,omitempty"`
	CheckedAt       time.Time `json:"checked_at,omitempty"`
	UpdateAvailable bool      `json:"update_available"`
	Skipped         bool      `json:"skipped"`
	SkippedVersion  string    `json:"skipped_version,omitempty"`
	Err             string    `json:"err,omitempty"`
}

// UpdateStatus mirrors the sidecar's state.json so handlers can pass it
// through unchanged.
type UpdateStatus struct {
	State     string    `json:"state"` // idle, pulling, restarting, done, failed
	Action    string    `json:"action,omitempty"`
	Target    string    `json:"target,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	Message   string    `json:"message,omitempty"`
}

// Checker is the background version-check service.
type Checker struct {
	cfg   Config
	store Store

	mu   sync.RWMutex
	info Info
}

// New constructs a Checker but does not start the background loop.
// Call Start(ctx) once wiring is complete.
func New(cfg Config, store Store) *Checker {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.CheckInterval == 0 {
		cfg.CheckInterval = defaultCheckInterval
	}
	if cfg.Repo == "" {
		cfg.Repo = "frahlg/forty-two-watts"
	}
	if cfg.ReleasesURL == "" {
		cfg.ReleasesURL = "https://api.github.com/repos/" + cfg.Repo + "/releases/latest"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	c := &Checker{cfg: cfg, store: store}
	c.info.Current = cfg.CurrentVersion
	c.mu.Lock()
	c.reloadSkipLocked()
	c.mu.Unlock()
	return c
}

// Start launches a goroutine that probes at CheckInterval until ctx is
// cancelled. The first probe runs after a 5–30 s random delay so restart
// bursts don't all hit GitHub at the same instant.
func (c *Checker) Start(ctx context.Context) {
	go c.loop(ctx)
}

func (c *Checker) loop(ctx context.Context) {
	// Jitter the boot probe so many instances upgrading at once don't
	// synchronize. The jitter is coarse (seconds), not security-sensitive.
	delay := time.Duration(5+time.Now().Unix()%25) * time.Second
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return
	}
	if _, err := c.Check(ctx, false); err != nil {
		slog.Warn("selfupdate: initial check failed", "err", err)
	}
	t := time.NewTicker(c.cfg.CheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := c.Check(ctx, false); err != nil {
				slog.Warn("selfupdate: periodic check failed", "err", err)
			}
		}
	}
}

// Check contacts GitHub and refreshes cached Info. A non-force call that
// finds the cache younger than half the check interval returns early and
// never hits the network.
func (c *Checker) Check(ctx context.Context, force bool) (Info, error) {
	c.mu.RLock()
	cached := c.info
	c.mu.RUnlock()
	if !force && !cached.CheckedAt.IsZero() && c.cfg.Now().Sub(cached.CheckedAt) < c.cfg.CheckInterval/2 {
		return cached, nil
	}

	req, err := http.NewRequestWithContext(ctx, "GET", c.cfg.ReleasesURL, nil)
	if err != nil {
		return cached, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "forty-two-watts-selfupdate/"+cached.Current)

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return c.recordErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return c.recordErr(fmt.Errorf("github releases %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
	}

	var rel struct {
		TagName     string    `json:"tag_name"`
		HtmlURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
		Prerelease  bool      `json:"prerelease"`
		Draft       bool      `json:"draft"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return c.recordErr(err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// /releases/latest excludes drafts + prereleases by default, so the
	// skip is defensive against an endpoint swap down the line.
	if !rel.Draft && !rel.Prerelease && rel.TagName != "" {
		c.info.Latest = rel.TagName
		c.info.PublishedAt = rel.PublishedAt
		c.info.ReleaseNotesURL = rel.HtmlURL
		c.info.UpdateAvailable = isNewer(rel.TagName, c.info.Current)
	}
	c.info.CheckedAt = c.cfg.Now()
	c.info.Err = ""
	c.reloadSkipLocked()
	return c.info, nil
}

func (c *Checker) recordErr(err error) (Info, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.info.Err = err.Error()
	return c.info, err
}

// Info returns the cached view. Skip state is re-read from the store on each
// call so a Skip/Unskip from another request is reflected immediately without
// broadcasting.
func (c *Checker) Info() Info {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reloadSkipLocked()
	return c.info
}

func (c *Checker) reloadSkipLocked() {
	if c.store == nil {
		return
	}
	v, ok := c.store.LoadConfig(skippedKey)
	if !ok {
		v = ""
	}
	c.info.SkippedVersion = v
	// Only "skipped" when the persisted version matches the *current* latest.
	// A newer release resurfaces automatically because SkippedVersion !=
	// Latest, so we never silently hide a version the user didn't ask to hide.
	c.info.Skipped = v != "" && v == c.info.Latest
}

// Skip persists the skipped version. An empty string is rejected — use Unskip.
func (c *Checker) Skip(version string) error {
	if c.store == nil {
		return errors.New("selfupdate: no store configured")
	}
	if version == "" {
		return errors.New("selfupdate: empty version")
	}
	if err := c.store.SaveConfig(skippedKey, version); err != nil {
		return err
	}
	c.mu.Lock()
	c.info.SkippedVersion = version
	c.info.Skipped = version == c.info.Latest
	c.mu.Unlock()
	return nil
}

// Unskip clears the persisted skip, so the next check surfaces the
// currently-latest release regardless of what was previously hidden.
func (c *Checker) Unskip() error {
	if c.store == nil {
		return errors.New("selfupdate: no store configured")
	}
	if err := c.store.SaveConfig(skippedKey, ""); err != nil {
		return err
	}
	c.mu.Lock()
	c.info.SkippedVersion = ""
	c.info.Skipped = false
	c.mu.Unlock()
	return nil
}

// Trigger dispatches an update or restart to the sidecar via its Unix
// socket. Returns as soon as the sidecar accepts the request — the actual
// pull + compose up runs async; observe progress via Status().
func (c *Checker) Trigger(ctx context.Context, action, target string) error {
	if c.cfg.SocketPath == "" {
		return errors.New("selfupdate: sidecar socket not configured")
	}
	if action != "update" && action != "restart" {
		return fmt.Errorf("selfupdate: invalid action %q", action)
	}
	body, _ := json.Marshal(map[string]string{"action": action, "target": target})
	cli := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", c.cfg.SocketPath)
			},
		},
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://unix/update", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("sidecar %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// Status reads the sidecar's state.json. Missing or unreadable returns
// {state: idle}. A pulling/restarting state whose last heartbeat is older
// than staleThreshold is surfaced as failed so the UI overlay unblocks.
func (c *Checker) Status() UpdateStatus {
	if c.cfg.StatusPath == "" {
		return UpdateStatus{State: "idle"}
	}
	f, err := os.Open(c.cfg.StatusPath)
	if err != nil {
		return UpdateStatus{State: "idle"}
	}
	defer f.Close()
	var st UpdateStatus
	if err := json.NewDecoder(f).Decode(&st); err != nil || st.State == "" {
		return UpdateStatus{State: "idle"}
	}
	if (st.State == "pulling" || st.State == "restarting") && !st.UpdatedAt.IsZero() {
		if c.cfg.Now().Sub(st.UpdatedAt) > staleThreshold {
			st.State = "failed"
			if st.Message == "" {
				st.Message = "no heartbeat from updater in 5 min"
			}
		}
	}
	return st
}

// isNewer returns true if latest is strictly greater than current in
// x.y.z order. Non-semver current ("dev", commit SHAs) is treated as older
// than any proper release so development builds see the upgrade banner.
func isNewer(latest, current string) bool {
	if latest == "" || latest == current {
		return false
	}
	l := parseSemver(latest)
	cc := parseSemver(current)
	if l == nil {
		return false
	}
	if cc == nil {
		return true
	}
	for i := 0; i < 3; i++ {
		if l[i] > cc[i] {
			return true
		}
		if l[i] < cc[i] {
			return false
		}
	}
	return false
}

func parseSemver(s string) *[3]int {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	if i := strings.IndexAny(s, "-+"); i > 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return nil
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out[i] = n
	}
	return &out
}
