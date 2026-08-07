package api

import "net/http"

// What this box would tell Sourceful about itself.
//
// The endpoint exists so the Settings tab can render the real message rather
// than a paragraph promising what the message contains. It is built by the
// same call the sender uses, so the two cannot drift: if the payload ever
// grew a field it should not have, this screen would show it.

type fleetPingView struct {
	Enabled  bool   `json:"enabled"`
	Endpoint string `json:"endpoint"`
	// Payload is the whole message, exactly as it would be posted.
	Payload any `json:"payload"`
}

func (s *Server) handleFleetPing(w http.ResponseWriter, r *http.Request) {
	if s.deps.FleetPing == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "the fleet ping is not running on this box",
		})
		return
	}

	// Resolved the same way the sender resolves them, so a config that never
	// carried the section reads here as what the box is actually doing.
	s.deps.CfgMu.RLock()
	enabled := s.deps.Cfg.FleetPing.On()
	endpoint := s.deps.Cfg.FleetPing.Resolved()
	s.deps.CfgMu.RUnlock()

	writeJSON(w, http.StatusOK, fleetPingView{
		Enabled:  enabled,
		Endpoint: endpoint,
		Payload:  s.deps.FleetPing.Payload(),
	})
}
