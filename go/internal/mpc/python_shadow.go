package mpc

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"time"
)

var errEmptyShadowPlan = errors.New("shadow optimizer returned a plan with no actions")

// The Python/HiGHS optimizer stopped being the champion in #1020: on 12
// validation-site snapshots, replayed with the terminal-SoC credit netted out,
// the Go DP at 201×401 landed within 12.7 öre per plan of the MILP (arbitrage
// rows −3…−10 öre), and it cannot reach the relaxation failure modes the
// external stack needed guard rails for. It stays wired behind Core as a field
// shadow so that claim keeps being measured on live sites instead of on a
// downloaded snapshot directory: same slots, same params, one number per
// replan.
//
// Everything here is subordinate to the champion. The shadow runs after the
// plan is published, cannot delay it, cannot fail it, and its output reaches
// only the log and the Diagnostic.

const (
	// pythonShadowTimeout bounds one challenger solve. The worker enforces its
	// own per-request timeout; this is the outer stop so a wedged transport
	// cannot hold a goroutine — or shutdown — open indefinitely.
	pythonShadowTimeout = 90 * time.Second
	// pythonShadowErrQuiet is how long one distinct error string stays quiet
	// after it has been reported once. A worker that is simply absent must not
	// write a warning every replan for weeks.
	pythonShadowErrQuiet = time.Hour
	// shadowErrWindowLimit bounds the suppression map. Errors carrying a
	// request id or timestamp would otherwise make every message distinct.
	shadowErrWindowLimit = 32
	// championEvalDriftOre is how far Core's valuation of its own plan may
	// sit from the cost that plan reported before it counts as a bug rather
	// than float residue. A 192-slot horizon accumulates far less than this.
	championEvalDriftOre = 0.5
)

// shadowRefusalReason names which side Core declined to cost, and why.
func shadowRefusalReason(championErr, shadowErr error) string {
	switch {
	case championErr != nil && shadowErr != nil:
		return "core plan: " + championErr.Error() + "; python plan: " + shadowErr.Error()
	case championErr != nil:
		return "core plan: " + championErr.Error()
	default:
		return "python plan: " + shadowErr.Error()
	}
}

type shadowErrWindow struct {
	openedAt   time.Time
	suppressed int
}

// startPythonShadow queues one challenger solve behind a published Core plan.
// Skipped — never queued — while a previous shadow is still solving or the
// service is stopping.
func (s *Service) startPythonShadow(champion Plan, slots []Slot, p Params,
	reason string, replanAtMs int64) {
	if s == nil || s.ShadowOptimizer == nil || len(champion.Actions) == 0 || len(slots) == 0 {
		return
	}
	s.mu.Lock()
	if s.stopping || s.shadowBusy {
		busy := s.shadowBusy && !s.stopping
		s.mu.Unlock()
		if busy {
			slog.Info("mpc: python shadow still solving; skipping this replan",
				"decision_id", champion.DecisionID)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pythonShadowTimeout)
	s.shadowBusy = true
	s.shadowCancel = cancel
	s.shadowWG.Add(1)
	s.mu.Unlock()
	go s.runPythonShadow(ctx, cancel, champion, slots, p, reason, replanAtMs)
}

func (s *Service) runPythonShadow(ctx context.Context, cancel context.CancelFunc,
	champion Plan, slots []Slot, p Params, reason string, replanAtMs int64) {
	defer s.shadowWG.Done()
	defer cancel()
	defer func() {
		// A challenger that panics must not take the planner with it.
		if r := recover(); r != nil {
			slog.Error("mpc: python shadow panicked",
				"panic", r, "decision_id", champion.DecisionID)
		}
		s.mu.Lock()
		s.shadowBusy = false
		s.shadowCancel = nil
		s.mu.Unlock()
	}()

	start := time.Now()
	shadow, err := s.ShadowOptimizer.Optimize(ctx, slots, p)
	solveMs := msSince(start)
	if err != nil {
		s.logPythonShadowError(err, time.Now())
		return
	}
	if len(shadow.Actions) == 0 {
		s.logPythonShadowError(errEmptyShadowPlan, time.Now())
		return
	}

	block := compareDPShadow(champion, shadow)
	block.ForecastBasis = "same downside input, python challenger"
	block.Solver = shadow.Solver
	block.SelfReportedOre = shadow.TotalCostOre
	if shadow.Solver != nil {
		block.SelfReportedObjectiveOre = shadow.Solver.ObjectiveOre
	}
	if block.FirstAction != nil {
		mode, _, _ := actionToSlot(*block.FirstAction, p.Mode)
		block.FirstAction.EMSMode = mode
	}
	if block.Solver != nil && block.Solver.SolveMs == 0 {
		block.Solver.SolveMs = solveMs
	}

	// Both plans are costed by Core, not by whoever produced them. Doing it
	// for the champion too is not ceremony: it keeps the subtraction
	// symmetric, so the verdict cannot drift if the DP's own bookkeeping
	// ever changes, and it is the only way the cross-check below exists.
	championEval, championErr := evaluatePlan(champion, slots, p)
	shadowEval, shadowErr := evaluatePlan(shadow, slots, p)

	base := []any{
		"decision_id", champion.DecisionID,
		"reason", reason,
		"python_self_reported_ore", shadow.TotalCostOre,
		"python_objective_ore", block.SelfReportedObjectiveOre,
		"python_solve_ms", solveMs,
		"mean_abs_battery_delta_w", block.MeanAbsBatteryDeltaW,
		"direction_disagreements", block.DirectionDisagreements,
		"compared_slots", block.ComparedSlots,
	}
	if championErr != nil || shadowErr != nil {
		block.EvaluationRefusedReason = shadowRefusalReason(championErr, shadowErr)
		slog.Warn("mpc: core champion vs python shadow not scored",
			append(base, "evaluation_refused", block.EvaluationRefusedReason)...)
		s.recordPythonShadow(champion, slots, p, reason, replanAtMs, block)
		return
	}

	block.ActiveEvaluationDriftOre = championEval.CostOre - champion.TotalCostOre
	if math.Abs(block.ActiveEvaluationDriftOre) > championEvalDriftOre {
		slog.Warn("mpc: core's own plan does not cost what core reported",
			"decision_id", champion.DecisionID,
			"reported_ore", champion.TotalCostOre,
			"evaluated_ore", championEval.CostOre,
			"drift_ore", block.ActiveEvaluationDriftOre)
	}
	block.TotalCostOre = shadowEval.CostOre
	block.ActiveMinusShadowOre = championEval.CostOre - shadowEval.CostOre
	block.ActivePVCurtailmentSlots = championEval.CurtailedSlots
	block.PVCurtailmentSlots = shadowEval.CurtailedSlots

	// Raw totals do not compare: a plan that ends the horizon fuller looks
	// expensive while it is merely storing value. Correct both sides before
	// the difference is written anywhere a human will read it.
	championOre := terminalCorrectedOre(championEval.CostOre, championEval.EndSoC, p)
	shadowOre := terminalCorrectedOre(shadowEval.CostOre, shadowEval.EndSoC, p)
	block.ActiveTerminalCorrectedOre = championOre
	block.TerminalCorrectedOre = shadowOre
	block.ActiveMinusShadowTerminalCorrectedOre = championOre - shadowOre

	slog.Info("mpc: core champion vs python shadow",
		append(base,
			"core_cost_ore", championEval.CostOre,
			"python_cost_ore", shadowEval.CostOre,
			"core_cost_ore_terminal_corrected", championOre,
			"python_cost_ore_terminal_corrected", shadowOre,
			"python_minus_core_ore_terminal_corrected", shadowOre-championOre,
			"core_evaluation_drift_ore", block.ActiveEvaluationDriftOre,
			"pv_curtailment_slots_core", championEval.CurtailedSlots,
			"pv_curtailment_slots_python", shadowEval.CurtailedSlots)...)

	s.recordPythonShadow(champion, slots, p, reason, replanAtMs, block)
}

// recordPythonShadow attaches the comparison to the decision it was solved
// against. A newer plan having taken over means this measurement has no home:
// the newer plan gets its own shadow, and the persisted snapshot keeps the
// diagnostic the replan already wrote.
func (s *Service) recordPythonShadow(champion Plan, slots []Slot, p Params,
	reason string, replanAtMs int64, block *ShadowPlan) {
	s.mu.Lock()
	current := s.last != nil && s.last.DecisionID == champion.DecisionID
	if current {
		s.lastPythonShadow = block
		s.lastPythonShadowFor = champion.DecisionID
	}
	saveDiag := s.SaveDiag
	zone := s.Zone
	s.mu.Unlock()
	if !current || saveDiag == nil {
		return
	}
	d := buildDiagnostic(&champion, slots, p, zone, replanAtMs, reason)
	if d == nil {
		return
	}
	d.PythonShadow = block
	// SaveDiagnostic upserts on the plan's generation time, so this rewrites
	// the row the replan already wrote rather than adding a second snapshot of
	// the same decision.
	if err := saveDiag(d, reason); err != nil {
		slog.Warn("mpc: persist python shadow diagnostic failed",
			"decision_id", champion.DecisionID, "err", err)
	}
}

// logPythonShadowError reports one distinct failure per hour and counts the
// rest. A missing worker is a normal state under planner.engine: core; it is
// worth saying once, not every replan.
func (s *Service) logPythonShadowError(err error, now time.Time) {
	msg := err.Error()
	s.mu.Lock()
	if s.shadowErrWindows == nil || len(s.shadowErrWindows) > shadowErrWindowLimit {
		s.shadowErrWindows = make(map[string]shadowErrWindow, 4)
	}
	window, seen := s.shadowErrWindows[msg]
	report := !seen || now.Sub(window.openedAt) >= pythonShadowErrQuiet
	suppressed := window.suppressed
	if report {
		s.shadowErrWindows[msg] = shadowErrWindow{openedAt: now}
	} else {
		window.suppressed++
		s.shadowErrWindows[msg] = window
	}
	s.mu.Unlock()
	if report {
		slog.Warn("mpc: python shadow failed; champion plan is unaffected",
			"err", msg, "suppressed_since_last", suppressed)
	}
}
