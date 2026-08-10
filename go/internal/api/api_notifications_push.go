package api

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/configreload"
	"github.com/srcfl/ftw/go/internal/notifications"
)

// The web-push routes. The provider itself lives in internal/notifications;
// these handlers own only the HTTP shapes: the public key, the subscription
// rows, and the narrow rules write that lets the app enable an event without
// carrying the whole configuration document over a relay.

// GET /api/notifications/vapid — the applicationServerKey the app hands to
// PushManager.subscribe. The public key and nothing else: the private half
// lives in a file beside state.db and no route returns it.
func (s *Server) handleNotificationsVAPID(w http.ResponseWriter, r *http.Request) {
	if s.deps.WebPush == nil {
		writeJSON(w, 503, map[string]string{"error": "web push unavailable"})
		return
	}
	key, err := s.deps.WebPush.PublicKeyB64()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"public_key": key})
}

// POST /api/notifications/subscriptions — store one browser subscription.
// The body is what PushManager.subscribe returned; the device it belongs to
// is whoever the session authenticated, never a field the body could lie in.
func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil {
		writeJSON(w, 503, map[string]string{"error": "no state store"})
		return
	}
	var req struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	endpoint, err := url.Parse(strings.TrimSpace(req.Endpoint))
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" {
		// Push services live on https origins, and this box will POST
		// encrypted payloads at whatever is stored here — so nothing
		// else may be.
		writeJSON(w, 400, map[string]string{"error": "endpoint must be an https URL"})
		return
	}
	if err := notifications.ValidateSubscriptionKeys(req.Keys.P256dh, req.Keys.Auth); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	id, err := s.deps.State.SavePushSubscription(
		endpoint.String(), req.Keys.P256dh, req.Keys.Auth, callerDeviceID(r))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.pokePushResync()
	writeJSON(w, 200, map[string]string{"id": id})
}

// DELETE /api/notifications/subscriptions/{id} — forget one subscription.
// Idempotent, because the caller wanted it gone and it is.
func (s *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil {
		writeJSON(w, 503, map[string]string{"error": "no state store"})
		return
	}
	if err := s.deps.State.DeletePushSubscription(r.PathValue("id")); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.pokePushResync()
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// PUT /api/notifications/rules — the narrow rules write.
//
// POST /api/config is refused over the passthrough because its body replaces
// the whole document, and a phone's idea of the document may be a year old.
// This is the surgical alternative: each submitted entry upserts the rule of
// its own type, types not mentioned keep whatever they had, and the optional
// top-level enabled flips the master switch. Nothing here can wipe a setting
// its sender never knew about.
func (s *Server) handleNotificationsRulesPut(w http.ResponseWriter, r *http.Request) {
	if s.deps.Cfg == nil || s.deps.CfgMu == nil || s.deps.SaveConfig == nil {
		writeJSON(w, 503, map[string]string{"error": "configuration is not writable here"})
		return
	}
	var req struct {
		Enabled *bool                     `json:"enabled,omitempty"`
		Events  []config.NotificationRule `json:"events"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	known := map[string]bool{}
	for _, t := range notifications.KnownRuleTypes() {
		known[t] = true
	}
	for _, e := range req.Events {
		if !known[e.Type] {
			writeJSON(w, 400, map[string]string{"error": "unknown event type: " + e.Type})
			return
		}
	}

	// Copy the live config, deep enough that the rules slice is ours.
	s.deps.CfgMu.RLock()
	newCfg := *s.deps.Cfg
	notif := config.Notifications{}
	if s.deps.Cfg.Notifications != nil {
		notif = *s.deps.Cfg.Notifications
		notif.Events = append([]config.NotificationRule(nil), notif.Events...)
	} else {
		// First touch: seed the full disabled rule set so the operator's
		// later visit to Settings sees every event, not just this one.
		notif.Events = notifications.DefaultRules()
	}
	s.deps.CfgMu.RUnlock()

	for _, e := range req.Events {
		replaced := false
		for i := range notif.Events {
			if notif.Events[i].Type == e.Type {
				notif.Events[i] = e
				replaced = true
				break
			}
		}
		if !replaced {
			notif.Events = append(notif.Events, e)
		}
	}
	if req.Enabled != nil {
		notif.Enabled = *req.Enabled
	}
	newCfg.Notifications = &notif

	if err := newCfg.Validate(); err != nil {
		writeJSON(w, 400, map[string]string{"error": "validation: " + err.Error()})
		return
	}
	if err := s.deps.SaveConfig(s.deps.ConfigPath, &newCfg); err != nil {
		writeJSON(w, 500, map[string]string{"error": "save failed: " + err.Error()})
		return
	}
	// One apply path, shared with the file watcher and POST /api/config —
	// see #760 for what hand-applying a subset cost last time.
	configreload.Apply(s.deps.CfgMu, s.deps.Cfg, s.deps.CtrlMu, s.deps.Ctrl,
		&newCfg, s.deps.ConfigApplier)
	slog.Info("notification rules updated via API", "events", len(req.Events))
	writeJSON(w, 200, map[string]any{
		"enabled": notif.Enabled,
		"events":  notif.Events,
	})
}

// callerDeviceID names the enrolled device behind a request, empty when the
// request came in off the LAN with no caller at all. Rows without a device
// are simply never swept by a revocation.
func callerDeviceID(r *http.Request) string {
	caller, ok := apiauth.FromRequest(r)
	if !ok || caller.Kind != apiauth.KindApp {
		return ""
	}
	return strings.TrimPrefix(caller.Subject, "app:")
}

func (s *Server) pokePushResync() {
	if s.deps.PushResync != nil {
		// Off this goroutine: the resync talks HTTP to the relay, and the
		// subscription route should answer at memory speed.
		go s.deps.PushResync()
	}
}
