package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func reportTestServer(t *testing.T) (*Server, *control.State, *telemetry.Store) {
	t.Helper()
	st := control.NewState(0, 50, "meter")
	tel := telemetry.NewStore()
	tel.DriverHealthMut("meter").RecordSuccess()
	tel.Update("meter", telemetry.DerMeter, 500, nil, nil)
	srv := New(&Deps{
		Ctrl:    st,
		CtrlMu:  &sync.Mutex{},
		Tel:     tel,
		Version: "test-version",
	})
	return srv, st, tel
}

func TestSupportReportServesMarkdownAttachment(t *testing.T) {
	srv, _, _ := reportTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/support/report", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/support/report = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("Content-Type = %q, want text/markdown", ct)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "ftw-help-") {
		t.Errorf("Content-Disposition = %q, want an ftw-help attachment", cd)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# FTW help report",
		"## Findings",
		"## Right now",
		"## Plan",
		"## Forecast quality",
		"## Devices",
		"## Versions",
		"test-version",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("report is missing %q", want)
		}
	}
}

// The report must survive a host where every optional service is nil —
// that is exactly the state a confused user is most likely to be in.
func TestSupportReportWithNoDependencies(t *testing.T) {
	srv := New(&Deps{Ctrl: control.NewState(0, 50, ""), CtrlMu: &sync.Mutex{}, Tel: telemetry.NewStore()})
	req := httptest.NewRequest(http.MethodGet, "/api/support/report", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/support/report = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No plan exists") {
		t.Error("expected the report to say no plan exists")
	}
	if !strings.Contains(body, "No site meter reading") {
		t.Error("expected a finding about the missing site meter")
	}
}

// The load-forecast check is the reason this report exists: a plan built
// against 383 W while the house draws 7.9 kW must be called out in
// Findings, not left for someone to spot in a table.
func TestSupportReportFlagsLoadForecastMiss(t *testing.T) {
	srv, ctrl, _ := reportTestServer(t)
	now := time.Now()
	snap := liveSnapshot{
		HaveGrid:    true,
		LoadW:       7900,
		PredictedLd: 383,
	}
	findings := srv.collectFindings(*ctrl, snap, nil, nil, nil, nil, now)

	var got *finding
	for i := range findings {
		if strings.Contains(findings[i].Title, "load forecast") {
			got = &findings[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no load-forecast finding; got %+v", findings)
	}
	if got.Severity != sevProblem {
		t.Errorf("severity = %q, want %q", got.Severity, sevProblem)
	}
	if !strings.Contains(got.Detail, "383 W") || !strings.Contains(got.Detail, "7.90 kW") {
		t.Errorf("detail should quote both numbers, got %q", got.Detail)
	}
}

func TestForecastMiss(t *testing.T) {
	cases := []struct {
		name              string
		predicted, actual float64
		want              bool
	}{
		{"the 383 W case", 383, 7900, true},
		{"close enough", 2000, 2200, false},
		{"exact", 1000, 1000, false},
		// A quiet house: 200 W of absolute error must not fire just
		// because the ratio looks bad against a small denominator.
		{"small absolute error on a quiet house", 100, 300, false},
		{"forecast far too high", 8000, 400, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := forecastMiss(tc.predicted, tc.actual); got != tc.want {
				t.Errorf("forecastMiss(%v, %v) = %v, want %v",
					tc.predicted, tc.actual, got, tc.want)
			}
		})
	}
}

// "Now" must be answered before "next". The dashboard's plan card leads
// with the next action, and that ambiguity is what made a support thread
// run for six hours.
func TestSupportReportMarksTheActiveSlot(t *testing.T) {
	_, ctrl, _ := reportTestServer(t)
	now := time.Now()
	slotStart := now.Add(-5 * time.Minute).UnixMilli()
	plan := &mpc.Plan{
		GeneratedAtMs: now.Add(-time.Minute).UnixMilli(),
		Mode:          mpc.ModeArbitrage,
		Actions: []mpc.Action{
			{SlotStartMs: slotStart, SlotLenMin: 15, BatteryW: -8400, Reason: "discharge — export at peak"},
			{SlotStartMs: slotStart + 15*60_000, SlotLenMin: 15, BatteryW: 2900, Reason: "absorb PV surplus"},
		},
		Solver: &mpc.SolverInfo{Engine: "highspy", Backend: "highs", Status: "optimal"},
	}

	var b strings.Builder
	writePlanSection(&b, plan, now.Add(-time.Minute), "reactive-load", now)
	out := b.String()

	lines := strings.Split(out, "\n")
	var marked string
	for _, ln := range lines {
		if strings.HasPrefix(ln, "| → |") {
			marked = ln
		}
	}
	if marked == "" {
		t.Fatalf("no slot marked as active:\n%s", out)
	}
	if !strings.Contains(marked, "export at peak") {
		t.Errorf("the wrong slot is marked active: %q", marked)
	}
	if !strings.Contains(out, "highspy / highs") {
		t.Error("solver identity should be in the plan section")
	}
	if !strings.Contains(out, "reactive-load") {
		t.Error("the replan reason should be in the plan section")
	}

	// And the live section should state the active slot's intent in prose.
	var live strings.Builder
	writeRightNow(&live, *ctrl, liveSnapshot{HaveGrid: true, BatW: -7500}, &plan.Actions[0], nil, now)
	if !strings.Contains(live.String(), "-8.40 kW") {
		t.Errorf("active-slot intent missing from Right now:\n%s", live.String())
	}
}

func TestSupportReportFlagsFallbackSolver(t *testing.T) {
	srv, ctrl, _ := reportTestServer(t)
	plan := &mpc.Plan{
		Solver: &mpc.SolverInfo{
			Engine:         "go-dp",
			Backend:        "bellman",
			Fallback:       true,
			FallbackReason: "optimizer handshake failed",
		},
	}
	findings := srv.collectFindings(*ctrl,
		liveSnapshot{HaveGrid: true, LoadW: 1000, PredictedLd: 1000},
		plan, nil, nil, nil, time.Now())

	found := false
	for _, f := range findings {
		if strings.Contains(f.Title, "fallback") {
			found = true
			if !strings.Contains(f.Detail, "optimizer handshake failed") {
				t.Errorf("detail should carry the reason, got %q", f.Detail)
			}
		}
	}
	if !found {
		t.Errorf("no fallback finding; got %+v", findings)
	}
}

func TestSupportReportFlagsOfflineAndFaultedDevices(t *testing.T) {
	srv, ctrl, _ := reportTestServer(t)
	health := map[string]telemetry.DriverHealth{
		"healthy":  {Name: "healthy", LastSuccess: ptrTime(time.Now())},
		"gone":     {Name: "gone", Status: telemetry.StatusOffline},
		"faulting": {Name: "faulting", DeviceFault: true, DeviceFaultReason: "Fault mode"},
	}
	findings := srv.collectFindings(*ctrl,
		liveSnapshot{HaveGrid: true, LoadW: 1000, PredictedLd: 1000},
		nil, nil, nil, health, time.Now())

	var sawOffline, sawFault bool
	for _, f := range findings {
		if strings.Contains(f.Detail, "gone") {
			sawOffline = true
		}
		if strings.Contains(f.Detail, "faulting") {
			sawFault = true
		}
	}
	if !sawOffline {
		t.Error("offline driver not reported")
	}
	if !sawFault {
		t.Error("faulted driver not reported")
	}
}

// Findings are sorted worst-first so a reader hits the real problem before
// the notes.
func TestFindingsAreSortedBySeverity(t *testing.T) {
	var b strings.Builder
	writeFindings(&b, []finding{
		{sevNote, "a note", "detail"},
		{sevProblem, "a problem", "detail"},
		{sevWarning, "a warning", "detail"},
	})
	out := b.String()
	pi := strings.Index(out, "a problem")
	wi := strings.Index(out, "a warning")
	ni := strings.Index(out, "a note")
	if !(pi < wi && wi < ni) {
		t.Errorf("findings out of order:\n%s", out)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
