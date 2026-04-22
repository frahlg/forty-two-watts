package api

// Loadpoint / EV target + policy + settings endpoints.
//
// Kept separate from api.go to prevent that file from growing
// unwieldy (see go/internal/api/CLAUDE.md). Pattern mirrors
// api_selfupdate.go.

import (
	"net/http"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/loadpoint"
)

// lpView extends the plain loadpoint.State with the per-loadpoint
// settings and last-used policy the UI needs to pre-fill its
// controls. The dashboard fetches /api/loadpoints every few seconds
// and uses this bundle to render the EV popup; keeping everything in
// one response avoids a second round-trip on popup open.
type lpView struct {
	loadpoint.State
	Settings   loadpoint.Settings     `json:"settings"`
	LastPolicy loadpoint.TargetPolicy `json:"last_policy"`
}

// GET /api/loadpoints returns the configured EV loadpoints with their
// current observable state, persisted settings, and last-used policy.
func (s *Server) handleLoadpoints(w http.ResponseWriter, r *http.Request) {
	if s.deps.Loadpoints == nil {
		writeJSON(w, 200, map[string]any{"enabled": false, "loadpoints": []any{}})
		return
	}
	states := s.deps.Loadpoints.States()
	views := make([]lpView, 0, len(states))
	for _, st := range states {
		views = append(views, lpView{
			State:      st,
			Settings:   s.deps.Loadpoints.Settings(st.ID),
			LastPolicy: s.deps.Loadpoints.LastPolicy(st.ID),
		})
	}
	writeJSON(w, 200, map[string]any{
		"enabled":    true,
		"loadpoints": views,
	})
}

// POST /api/loadpoints/{id}/target sets user intent for a loadpoint.
//
// Body:
//
//	{"soc_pct": 80, "target_time_ms": 1745000000000,
//	 "origin": "manual", "policy": {"allow_grid": false,
//	 "allow_battery_support": false}}
//
// Missing origin defaults to "manual". Missing policy defaults to
// {false,false} = surplus-only. target_time_ms == 0 → no deadline.
// Triggers an MPC replan so the new target lands in the schedule
// within one control cycle.
func (s *Server) handleLoadpointTarget(w http.ResponseWriter, r *http.Request) {
	if s.deps.Loadpoints == nil {
		writeJSON(w, 404, map[string]string{"error": "loadpoints not configured"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "id required"})
		return
	}
	var req struct {
		SoCPct       float64                `json:"soc_pct"`
		TargetTimeMs int64                  `json:"target_time_ms"`
		Origin       string                 `json:"origin"`
		Policy       loadpoint.TargetPolicy `json:"policy"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	var deadline time.Time
	if req.TargetTimeMs > 0 {
		deadline = time.UnixMilli(req.TargetTimeMs).UTC()
	}
	origin := req.Origin
	if origin == "" {
		origin = "manual"
	}
	if !s.deps.Loadpoints.SetTarget(id, req.SoCPct, deadline, origin, req.Policy) {
		writeJSON(w, 404, map[string]string{"error": "loadpoint not found"})
		return
	}
	// Remember this policy so the next Start press pre-fills it.
	// Only do this for a "real" arm (socPct > 0); a zero-SoCPct call
	// is a clear, not a user intent expression.
	if req.SoCPct > 0 {
		s.deps.Loadpoints.SaveLastPolicy(id, req.Policy)
	}
	if s.deps.MPC != nil {
		go s.deps.MPC.Replan(r.Context())
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// POST /api/loadpoints/{id}/soc re-anchors the inferred vehicle SoC
// for the current session. Easee and most chargers don't expose the
// car's BMS — the manager estimates from `plugin_soc_pct +
// delivered_wh/capacity`. When that drifts, the operator reads the
// true value off the car and posts it here; future observations
// accumulate from the new anchor.
//
// Body: {"soc_pct": 60}
//
// Returns 409 if the loadpoint is unplugged.
func (s *Server) handleLoadpointSoC(w http.ResponseWriter, r *http.Request) {
	if s.deps.Loadpoints == nil {
		writeJSON(w, 404, map[string]string{"error": "loadpoints not configured"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "id required"})
		return
	}
	var req struct {
		SoCPct float64 `json:"soc_pct"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if _, ok := s.deps.Loadpoints.State(id); !ok {
		writeJSON(w, 404, map[string]string{"error": "loadpoint not found"})
		return
	}
	if !s.deps.Loadpoints.SetCurrentSoC(id, req.SoCPct) {
		writeJSON(w, 409, map[string]string{
			"error": "loadpoint not plugged in — SoC can only be set during an active session",
		})
		return
	}
	if s.deps.MPC != nil {
		go s.deps.MPC.Replan(r.Context())
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// POST /api/loadpoints/{id}/settings updates persistent per-loadpoint
// settings. Body fields all optional; zero/missing fields are ignored
// (so partial updates work). Validation errors return 400.
//
//	{"charge_duration_h": 8, "surplus_hysteresis_w": 500,
//	 "surplus_hysteresis_s": 300, "surplus_starvation_s": 1800}
func (s *Server) handleLoadpointSettings(w http.ResponseWriter, r *http.Request) {
	if s.deps.Loadpoints == nil {
		writeJSON(w, 404, map[string]string{"error": "loadpoints not configured"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "id required"})
		return
	}
	if _, ok := s.deps.Loadpoints.State(id); !ok {
		writeJSON(w, 404, map[string]string{"error": "loadpoint not found"})
		return
	}
	var req loadpoint.Settings
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if err := s.deps.Loadpoints.UpdateSettings(id, req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
