package plant

import (
	"encoding/json"
	"net/http"
	"time"
)

// NewServeMux exposes the plant's versioned HTTP contract, consumed by
// the ftw_plant Lua driver over the loopback:
//
//	GET  /v1/status              → Status (schema_version, features,
//	                               aggregate, per-unit states, lease)
//	POST /v1/setpoint            → {"power_w": W, "ttl_ms": N}
//	                               applies an aggregate target under a
//	                               lease; responds with the expiry.
//
// Contract evolution follows the repo's module rule: grow by adding
// features to /v1/status's `features` array, never by bumping the
// version for additive change.
func NewServeMux(c *Controller) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(c.Status(time.Now()))
	})
	mux.HandleFunc("POST /v1/setpoint", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			PowerW *float64 `json:"power_w"`
			TTLMs  int64    `json:"ttl_ms"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.PowerW == nil {
			http.Error(w, "power_w is required", http.StatusBadRequest)
			return
		}
		expires := c.SetTarget(*body.PowerW, time.Duration(body.TTLMs)*time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted":      true,
			"lease_expires": expires,
		})
	})
	return mux
}
