package api

import (
	"net/http"
	"strconv"
	"time"
)

// GET /api/demand — live billing-demand state for C&I sites: the running
// demand-integration window, the billing-cycle peak so far, NMD, and the
// cycle's recent completed intervals. 404 when no tariff/demand tracking
// is configured (residential sites).
func (s *Server) handleDemand(w http.ResponseWriter, r *http.Request) {
	if s.deps.Demand == nil {
		http.Error(w, "demand tracking not configured", http.StatusNotFound)
		return
	}
	snap := s.deps.Demand.Snapshot(time.Now())

	limit := 48
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 3000 {
			limit = n
		}
	}
	resp := map[string]any{"demand": snap}
	if s.deps.Cfg != nil {
		s.deps.CfgMu.RLock()
		resp["nmd_kva"] = s.deps.Cfg.Site.NMDkVA
		resp["currency"] = s.deps.Cfg.Site.Currency
		if s.deps.Cfg.Tariff != nil {
			resp["demand_charge_ct_kva"] = s.deps.Cfg.Tariff.DemandChargeCtKVA
		}
		s.deps.CfgMu.RUnlock()
	}
	if s.deps.State != nil && limit > 0 {
		if ivs, err := s.deps.State.DemandIntervals(snap.CycleStartMs, limit); err == nil {
			resp["intervals"] = ivs
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
