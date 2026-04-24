package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// POST /api/drivers/verify_tesla — operator-facing "Verify connection"
// button for the Tesla vehicle driver config form. Accepts {ip, vin}
// exactly matching the driver's own config fields, issues a single
// GET to the proxy's vehicle_data endpoint from the backend (avoids
// browser CORS), and returns a summary the UI can render inline.
//
// Kept in its own file per the api/CLAUDE.md split convention. Stays
// off the auth-requiring paths because (a) no auth layer exists in
// this app and (b) the endpoint only reads vehicle data the operator
// already has full access to — nothing to steal by poking it.
type verifyTeslaRequest struct {
	IP  string `json:"ip"`
	VIN string `json:"vin"`
}

type verifyTeslaResponse struct {
	OK             bool    `json:"ok"`
	Error          string  `json:"error,omitempty"`
	URL            string  `json:"url,omitempty"`
	Status         int     `json:"status,omitempty"`
	SoCPct         float64 `json:"soc_pct,omitempty"`
	ChargeLimitPct float64 `json:"charge_limit_pct,omitempty"`
	ChargingState  string  `json:"charging_state,omitempty"`
}

// handleVerifyTesla runs a one-off vehicle_data fetch against the
// configured TeslaBLEProxy. Mirrors exactly what the driver does on
// each poll, minus the emit. Errors are surfaced verbatim so the
// operator can distinguish "proxy unreachable" from "vehicle asleep"
// from "VIN not paired".
func (s *Server) handleVerifyTesla(w http.ResponseWriter, r *http.Request) {
	var req verifyTeslaRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, verifyTeslaResponse{Error: "invalid body: " + err.Error()})
		return
	}
	ip := strings.TrimSpace(req.IP)
	vin := strings.TrimSpace(req.VIN)
	if ip == "" || vin == "" {
		writeJSON(w, 400, verifyTeslaResponse{Error: "both `ip` and `vin` are required"})
		return
	}

	// Accept bare host or host:port. Mirrors drivers/tesla_vehicle.lua.
	host, port := splitHostPort(ip, 8080)
	url := fmt.Sprintf("http://%s:%d/api/1/vehicles/%s/vehicle_data?endpoints=charge_state&wakeup=true",
		host, port, vin)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		writeJSON(w, 500, verifyTeslaResponse{Error: "build request: " + err.Error(), URL: url})
		return
	}
	httpReq.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		writeJSON(w, 200, verifyTeslaResponse{Error: "request failed: " + err.Error(), URL: url})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusRequestTimeout { // 408: car asleep
		writeJSON(w, 200, verifyTeslaResponse{
			Error:  "vehicle asleep (408) — the proxy reached your car but it did not wake in time. Try again in a few seconds.",
			URL:    url,
			Status: resp.StatusCode,
		})
		return
	}
	if resp.StatusCode >= 400 {
		writeJSON(w, 200, verifyTeslaResponse{
			Error:  fmt.Sprintf("HTTP %d from proxy", resp.StatusCode),
			URL:    url,
			Status: resp.StatusCode,
		})
		return
	}

	// Three response shapes in the wild:
	//
	//   1. TeslaBLEProxy: { response: { response: { charge_state: {…} } } }
	//      — the proxy wraps the upstream body once more. THIS is
	//      what the real-world deployment emits (tested locally).
	//   2. Tesla Owner API:  { response: { charge_state: {…} } }
	//   3. Bare:             { charge_state: {…} }
	//
	// We try all three and pick whichever yielded non-zero fields.
	type chargeState struct {
		BatteryLevel        float64 `json:"battery_level"`
		ChargeLimitSoC      float64 `json:"charge_limit_soc"`
		ChargingState       string  `json:"charging_state"`
		MinutesToFullCharge float64 `json:"minutes_to_full_charge"`
		TimeToFullCharge    float64 `json:"time_to_full_charge"`
	}
	var parsed struct {
		Response struct {
			Response struct {
				ChargeState chargeState `json:"charge_state"`
			} `json:"response"`
			ChargeState chargeState `json:"charge_state"`
		} `json:"response"`
		ChargeState chargeState `json:"charge_state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		writeJSON(w, 200, verifyTeslaResponse{
			Error: "decode failed: " + err.Error(), URL: url, Status: resp.StatusCode,
		})
		return
	}
	cs := parsed.Response.Response.ChargeState
	if cs.BatteryLevel == 0 && cs.ChargeLimitSoC == 0 && cs.ChargingState == "" {
		cs = parsed.Response.ChargeState
	}
	if cs.BatteryLevel == 0 && cs.ChargeLimitSoC == 0 && cs.ChargingState == "" {
		cs = parsed.ChargeState
	}
	if cs.BatteryLevel == 0 && cs.ChargeLimitSoC == 0 && cs.ChargingState == "" {
		writeJSON(w, 200, verifyTeslaResponse{
			Error: "proxy returned 200 but no charge_state fields — VIN may not be paired",
			URL: url, Status: resp.StatusCode,
		})
		return
	}
	writeJSON(w, 200, verifyTeslaResponse{
		OK:             true,
		URL:            url,
		Status:         resp.StatusCode,
		SoCPct:         cs.BatteryLevel,
		ChargeLimitPct: cs.ChargeLimitSoC,
		ChargingState:  cs.ChargingState,
	})
}

// splitHostPort parses "host" or "host:port" and returns the port
// defaulting to `def` when absent. Copes with IPv4 and bracketed
// IPv6 (though brackets aren't expected in the typical config).
func splitHostPort(s string, def int) (string, int) {
	// Only split on the LAST colon that's followed by digits — avoids
	// eating a port out of an IPv6 without brackets. Good enough for
	// the LAN-only case this endpoint serves.
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return s, def
	}
	host := s[:i]
	portStr := s[i+1:]
	var port int
	for _, r := range portStr {
		if r < '0' || r > '9' {
			return s, def
		}
		port = port*10 + int(r-'0')
	}
	if port == 0 {
		return s, def
	}
	return host, port
}
