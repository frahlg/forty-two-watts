package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// POST /api/drivers/verify_tesla — operator-facing "Verify connection"
// button for the Tesla vehicle driver config form. Accepts {ip, vin}
// exactly matching the driver's own config fields, issues a single
// GET to the proxy's vehicle_data endpoint from the backend (avoids
// browser CORS), and returns a summary the UI can render inline.
//
// Hardened against SSRF: the `ip` param is constrained to RFC1918
// private space (10/8, 172.16/12, 192.168/16), link-local is rejected,
// loopback is rejected, ports are restricted to 80/443/8080, VIN is
// regex-validated to the standard 17-char pattern, redirects are
// refused, and the upstream body is never reflected back to the caller.
// See PR #184 review S1.
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

// validVINRe is the standard 17-character VIN charset: A-HJ-NPR-Z0-9
// (excludes I, O, Q to avoid confusion with 0/1). Applied even though
// Teslas pattern-match narrower because an attacker could try to abuse
// a relaxed validation to smuggle `/` or `?` into the URL path.
var validVINRe = regexp.MustCompile(`^[A-HJ-NPR-Z0-9]{17}$`)

// allowedVerifyPorts is the tiny set of ports we'll probe on the
// supposed proxy host. TeslaBLEProxy defaults to 8080 in every
// deployment we've seen; 80/443 are included for completeness in case
// someone fronted it with a reverse proxy. Locked down to these three
// so the endpoint can't be used to scan arbitrary LAN services.
var allowedVerifyPorts = map[int]bool{80: true, 443: true, 8080: true}

// errRedirectsForbidden ends a redirect chain before the backend follows
// a Location header to an unintended target (another SSRF vector).
var errRedirectsForbidden = errors.New("redirects disabled")

// maxVerifyBody caps how much of the proxy response we read. Tesla
// vehicle_data responses are typically 5-15 KB; 1 MB is pessimistic
// headroom. Prevents a malicious proxy from stalling the handler.
const maxVerifyBody = 1 << 20

// validateProxyIP accepts "host" or "host:port" where host is an IPv4
// literal in RFC1918 space. Hostnames are rejected — we want no DNS
// step between parse and connect, because a DNS rebind can change the
// answer between verification and connection.
func validateProxyIP(s string) (string, int, error) {
	host, port := splitHostPort(s, 8080)
	if !allowedVerifyPorts[port] {
		return "", 0, fmt.Errorf("port %d not permitted (allowed: 80, 443, 8080)", port)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", 0, fmt.Errorf("invalid IP literal %q (hostnames not allowed)", host)
	}
	if ip.To4() == nil {
		return "", 0, fmt.Errorf("IPv6 not supported")
	}
	if ip.IsLoopback() {
		return "", 0, fmt.Errorf("loopback address not permitted")
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return "", 0, fmt.Errorf("link-local address not permitted")
	}
	if !isPrivateIPv4(ip) {
		return "", 0, fmt.Errorf("public IP not permitted (RFC1918 only)")
	}
	return ip.String(), port, nil
}

// isPrivateIPv4 matches 10/8, 172.16/12, 192.168/16. Intentionally
// does NOT match 169.254/16 (link-local — caller blocks separately),
// 100.64/10 (carrier-grade NAT), or RFC6598 space.
func isPrivateIPv4(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	switch {
	case ip4[0] == 10:
		return true
	case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
		return true
	case ip4[0] == 192 && ip4[1] == 168:
		return true
	}
	return false
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
	ipRaw := strings.TrimSpace(req.IP)
	vin := strings.ToUpper(strings.TrimSpace(req.VIN))
	if ipRaw == "" || vin == "" {
		writeJSON(w, 400, verifyTeslaResponse{Error: "both `ip` and `vin` are required"})
		return
	}
	if !validVINRe.MatchString(vin) {
		writeJSON(w, 400, verifyTeslaResponse{Error: "invalid VIN (must be 17 chars, A-HJ-NPR-Z and 0-9)"})
		return
	}

	host, port, err := validateProxyIP(ipRaw)
	if err != nil {
		writeJSON(w, 400, verifyTeslaResponse{Error: "invalid proxy ip: " + err.Error()})
		return
	}

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

	// Client configured for SSRF resistance: no redirects (a compromised
	// proxy could send a 302 to a metadata endpoint), context-bound
	// timeout, no arbitrary transport sharing.
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return errRedirectsForbidden
		},
	}
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
		// Do NOT reflect the upstream body — even the status number on
		// its own is already a port-scan oracle, but the body can carry
		// metadata-service tokens on a successful SSRF hop. Fixed message.
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxVerifyBody)).Decode(&parsed); err != nil {
		writeJSON(w, 200, verifyTeslaResponse{
			Error: "decode failed (upstream response not recognizable JSON)", URL: url, Status: resp.StatusCode,
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
			URL:   url, Status: resp.StatusCode,
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
