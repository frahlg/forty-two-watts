package api

import (
	"net/http"

	"github.com/srcfl/ftw/go/internal/config"
)

// What this box would add to FTW's daily fleet totals.
//
// The endpoint exists so the Settings tab can render the real message rather
// than a paragraph promising what the message contains. It is built by the
// same call the sender uses, so the two cannot drift: if the payload ever
// grew a field it should not have, this screen would show it.

type fleetPingView struct {
	Enabled  bool   `json:"enabled"`
	Endpoint string `json:"endpoint"`
	// FTWRelay lets Settings state FTW's retention rules only when the running
	// process is using the endpoint those rules govern.
	FTWRelay bool `json:"ftw_relay"`
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

	// The switch is live, but the endpoint is fixed when the pinger starts.
	// Report that active endpoint rather than a newly saved value that needs a
	// restart before it can be used.
	s.deps.CfgMu.RLock()
	enabled := s.deps.Cfg.FleetPing.On()
	s.deps.CfgMu.RUnlock()

	endpoint := s.deps.FleetPing.Endpoint()
	writeJSON(w, http.StatusOK, fleetPingView{
		Enabled:  enabled,
		Endpoint: endpoint,
		FTWRelay: endpoint == config.DefaultFleetPingEndpoint,
		Payload:  s.deps.FleetPing.Payload(),
	})
}
