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

	"github.com/frahlg/forty-two-watts/go/internal/events"
)

// Store is the subset of state.Store methods this package needs. Declared as
// an interface so tests don't need a real SQLite DB.
type Store interface {
	SaveConfig(key, value string) error
	LoadConfig(key string) (string, bool)
}

const (
	skippedKey           = "update.skipped_version"
	defaultCheckInterval = 1 * time.Hour
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
	// CheckInterval is the probe cadence. 0 = 1 h.
	CheckInterval time.Duration
	// SocketPath is where the sidecar listens. Empty disables Trigger.
	SocketPath string
	// StatusPath is the sidecar's state.json. Empty disables Status.
	StatusPath string
	// Bus receives an events.UpdateAvailable event whenever Check
	// discovers a new, non-skipped release tag. Nil disables emission.
	Bus *events.Bus

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
	// ReleaseBody is the markdown body of the GitHub release —
	// typically the auto-generated changelog section (Features, Bug
	// Fixes). The UI renders this inline in the update modal so
	// operators can read what's about to be applied without opening
	// a new tab. Capped at MaxReleaseBodyBytes to keep a pathological
	// release note from ballooning the Info payload.
	ReleaseBody     string    `json:"release_body,omitempty"`
	CheckedAt       time.Time `json:"checked_at,omitempty"`
	UpdateAvailable bool      `json:"update_available"`
	Skipped         bool      `json:"skipped"`
	SkippedVersion  string    `json:"skipped_version,omitempty"`
	Err             string    `json:"err,omitempty"`
	// SidecarReady is true only when the ftw-updater sidecar's Unix socket
	// is present at SocketPath — i.e. the full pull+restart flow is wired
	// up, which in practice means a docker-compose deploy. Native / WSL
	// dev runs with FTW_SELFUPDATE_ENABLED=1 still report update_available
	// honestly, but the UI uses this flag to decide whether to offer an
	// actionable Update button vs just a notify-only indicator.
	SidecarReady bool `json:"sidecar_ready"`
}

// MaxReleaseBodyBytes caps the persisted release body. 16 KiB covers a
// few dozen bullets from semantic-release comfortably; anything larger
// is truncated with a trailing marker and the operator keeps the
// ReleaseNotesURL link for the full thing.
const MaxReleaseBodyBytes = 16 * 1024

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

	mu                 sync.RWMutex
	info               Info
	lastAnnouncedTag   string // dedupe: last tag we emitted UpdateAvailable for
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
		Body        string    `json:"body"`
		PublishedAt time.Time `json:"published_at"`
		Prerelease  bool      `json:"prerelease"`
		Draft       bool      `json:"draft"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return c.recordErr(err)
	}

	c.mu.Lock()
	if !rel.Draft && !rel.Prerelease && rel.TagName != "" {
		c.info.Latest = rel.TagName
		c.info.PublishedAt = rel.PublishedAt
		c.info.ReleaseNotesURL = rel.HtmlURL
		c.info.ReleaseBody = truncateBody(rel.Body)
		c.info.UpdateAvailable = isNewer(rel.TagName, c.info.Current)
	}
	c.info.CheckedAt = c.cfg.Now()
	c.info.Err = ""
	c.reloadSkipLocked()
	// Decide whether to emit under the lock, then publish outside it.
	var announce *events.UpdateAvailable
	if c.cfg.Bus != nil && c.info.UpdateAvailable && !c.info.Skipped &&
		c.info.Latest != "" && c.info.Latest != c.lastAnnouncedTag {
		c.lastAnnouncedTag = c.info.Latest
		announce = &events.UpdateAvailable{
			Version:         c.info.Latest,
			PreviousVersion: c.info.Current,
			ReleaseNotesURL: c.info.ReleaseNotesURL,
			PublishedAt:     c.info.PublishedAt,
			At:              c.cfg.Now(),
		}
	}
	info := c.info
	c.mu.Unlock()
	if announce != nil {
		c.cfg.Bus.Publish(*announce)
	}
	return info, nil
}

func (c *Checker) recordErr(err error) (Info, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.info.Err = err.Error()
	return c.info, err
}

// Info returns the cached view. Skip state is re-read from the store on each
// call so a Skip/Unskip from another request is reflected immediately without
// broadcasting. SidecarReady is re-probed on every call so a sidecar that
// came up (or crashed) after boot is reflected without waiting for the next
// periodic Check.
func (c *Checker) Info() Info {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reloadSkipLocked()
	info := c.info
	info.SidecarReady = c.sidecarReadyLocked()
	return info
}

// sidecarReadyLocked reports whether the updater socket is present as an
// actual Unix socket. An empty SocketPath means the feature was never
// configured for this deploy — docker-compose sets it, native installs
// typically don't.
func (c *Checker) sidecarReadyLocked() bool {
	if c.cfg.SocketPath == "" {
		return false
	}
	fi, err := os.Stat(c.cfg.SocketPath)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSocket != 0
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
	return c.postSidecar(ctx, body)
}

// TriggerRollback asks the sidecar to restore a snapshot over the main
// service's data volume (soft rollback: state.db + config.yaml only;
// image stays). The main container will be stopped, the files copied
// in via `docker cp`, and the service brought back up. Observe
// progress via Status() — new states are "restoring" and "restarting".
// Issue #152.
func (c *Checker) TriggerRollback(ctx context.Context, snapshotID string, files []string) error {
	if c.cfg.SocketPath == "" {
		return errors.New("selfupdate: sidecar socket not configured")
	}
	if snapshotID == "" {
		return errors.New("selfupdate: rollback requires snapshot id")
	}
	body, _ := json.Marshal(map[string]any{
		"action":   "rollback",
		"snapshot": snapshotID,
		"files":    files,
	})
	return c.postSidecar(ctx, body)
}

// postSidecar wraps the Unix-socket POST to the sidecar's /update
// endpoint. Shared by Trigger and TriggerRollback so the HTTP client
// config (socket dialer + timeout) only lives in one place.
func (c *Checker) postSidecar(ctx context.Context, body []byte) error {
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

// truncateBody caps release-body markdown to MaxReleaseBodyBytes so a
// runaway release note (auto-generated from hundreds of commits on a
// long-lived branch) can't inflate /api/version/check payloads. When we
// cut, we leave a clear marker so the UI can point the operator at
// ReleaseNotesURL for the rest.
func truncateBody(b string) string {
	if len(b) <= MaxReleaseBodyBytes {
		return b
	}
	return b[:MaxReleaseBodyBytes] + "\n\n…(truncated — see release notes for full changelog)"
}
