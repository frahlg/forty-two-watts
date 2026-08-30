package api

import (
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

// Planner-visibility decoration for GET /api/loadpoints. Fills the
// fields that answer the operator's first question at plug-in — "when
// will it charge, and if not, why not?":
//
//   - the next window in which the active MPC plan allocates charge
//     energy to the loadpoint (so the UI can say "charging planned
//     02:15–06:30" instead of sitting silent until the cheap slots
//     arrive, which teaches operators to press Start and lose the
//     plan);
//   - whether grid-funded planning is deferred because the deadline
//     lies past the published price horizon (which otherwise behaves
//     exactly like a PV-only mode nobody chose).
//
// Per the api/CLAUDE.md split convention, this lives in its own file
// and is called from handleLoadpoints in api.go.

// decorateLoadpointsWithPlan mutates states in place.
func (s *Server) decorateLoadpointsWithPlan(states []loadpoint.State) {
	now := time.Now()
	for i := range states {
		if s.deps.LoadpointCtrl != nil {
			states[i].GridDeferred = s.deps.LoadpointCtrl.GridDeferred(states[i].ID)
		}
		if s.deps.MPC == nil {
			continue
		}
		windows, totalWh := s.deps.MPC.LoadpointPlanWindows(states[i].ID, now, 1)
		if len(windows) == 0 {
			continue
		}
		states[i].PlanNextStartMs = windows[0].Start.UnixMilli()
		states[i].PlanNextEndMs = windows[0].End.UnixMilli()
		states[i].PlanNextWh = windows[0].EnergyWh
		states[i].PlanTotalWh = totalWh
	}
}
