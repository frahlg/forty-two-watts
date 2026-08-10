package appproto

import (
	"errors"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

// fakeLoadpoints stands in for the loadpoint controller. Tests mutate it
// between the write and the read-back, which is how the honest-readback rules
// get proved rather than asserted.
type fakeLoadpoints struct {
	ids map[string]bool

	hold       loadpoint.ManualHold
	holdID     string
	holdSet    bool
	holdCalls  int
	clearCalls int
	// onHold runs inside Hold, so a test can move the world between the
	// write and the read.
	onHold func()

	boostErr    error
	boostLease  loadpoint.BatteryBoostLease
	boostID     string
	boostActive bool
	boostCalls  int
	cancelCalls int
	onBoost     func()
}

func (f *fakeLoadpoints) Exists(id string) bool { return f.ids[id] }

func (f *fakeLoadpoints) Hold(id string, h loadpoint.ManualHold) {
	f.holdCalls++
	f.holdID = id
	f.hold = h
	f.holdSet = true
	if f.onHold != nil {
		f.onHold()
	}
}

func (f *fakeLoadpoints) ClearHold(id string) {
	f.clearCalls++
	if f.holdID == id {
		f.holdSet = false
	}
}

func (f *fakeLoadpoints) ObservedHold(id string, _ time.Time) (loadpoint.ManualHold, bool) {
	if !f.holdSet || f.holdID != id {
		return loadpoint.ManualHold{}, false
	}
	return f.hold, true
}

func (f *fakeLoadpoints) Boost(id string, lease loadpoint.BatteryBoostLease, _ time.Time) error {
	f.boostCalls++
	if f.boostErr != nil {
		return f.boostErr
	}
	f.boostID = id
	f.boostLease = lease
	f.boostActive = true
	if f.onBoost != nil {
		f.onBoost()
	}
	return nil
}

func (f *fakeLoadpoints) CancelBoost(id string, _ time.Time) {
	f.cancelCalls++
	if f.boostID == id {
		f.boostActive = false
	}
}

func (f *fakeLoadpoints) ObservedBoost(id string, _ time.Time) loadpoint.BatteryBoostStatus {
	if f.boostActive && f.boostID == id {
		return loadpoint.BatteryBoostStatus{State: "active", Active: true}
	}
	return loadpoint.BatteryBoostStatus{State: "inactive"}
}

// newEVRig is newRig with one loadpoint, "lp1", behind the port.
func newEVRig(t *testing.T) (*Handler, *fakeLoadpoints, *fakeBox, *recorder, *fakeClock) {
	t.Helper()
	h, box, rec, clock := newRig(t)
	lp := &fakeLoadpoints{ids: map[string]bool{"lp1": true}}
	h.cfg.Loadpoints = lp
	return h, lp, box, rec, clock
}

func cmdLoadpoint(op, cmdID string, args map[string]any, rev uint64, notValidAfterMs int64) Cmd {
	return Cmd{
		CmdID:           cmdID,
		Op:              op,
		Args:            args,
		NotValidAfterMs: notValidAfterMs,
		Expect:          Expect{Rev: rev},
	}
}

func holdArgs() map[string]any {
	return map[string]any{"id": "lp1", "power_w": 7360, "hold_s": 0, "phase_mode": "3p"}
}

func boostArgs() map[string]any {
	return map[string]any{
		"id": "lp1", "duration_s": 3600,
		"min_battery_soc_pct": 30, "ev_target_soc_pct": 80,
	}
}

// The ack says the dispatcher took the intent; the result reports the hold
// the box now carries. The same two-step site.mode.set has, because the echo
// of a request is never confirmation.
func TestLoadpointHoldAcksThenConfirmsFromAReadBack(t *testing.T) {
	h, lp, _, rec, clock := newEVRig(t)
	subscribe(t, h, rec)

	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpLoadpointHold, "cmd-hold-1", holdArgs(), 7, 200_000))

	ack := body[CmdAck](t, rec.only(t, MsgCmdAck))
	if ack.LeaseID == "" {
		t.Fatal("ack carried no lease")
	}
	if ack.ExpiresAtMs != clock.uptimeMs+LeaseMs {
		t.Fatalf("lease expires at %d, want %d", ack.ExpiresAtMs, clock.uptimeMs+LeaseMs)
	}

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdApplied {
		t.Fatalf("state = %q (%+v), want applied", res.State, res.Error)
	}
	if res.Observed == nil || res.Observed.Src != ObservedSrcCore {
		t.Fatalf("observed = %+v; applied without a core read-back", res.Observed)
	}
	if res.Observed.Value != 7360 {
		t.Fatalf("observed value = %v, want the held power", res.Observed.Value)
	}

	if lp.holdCalls != 1 || lp.holdID != "lp1" {
		t.Fatalf("hold reached the controller %d times for %q", lp.holdCalls, lp.holdID)
	}
	if lp.hold.PowerW != 7360 || lp.hold.PhaseMode != "3p" {
		t.Fatalf("installed hold = %+v", lp.hold)
	}
	if !lp.hold.Persistent || !lp.hold.ExpiresAt.IsZero() {
		t.Fatalf("hold_s 0 must install the persistent operator hold, got %+v", lp.hold)
	}
}

// hold_s above zero is the bounded diagnostic hold, expiring on the wall
// clock the box counts from now.
func TestATimedHoldCarriesItsExpiry(t *testing.T) {
	h, lp, _, rec, clock := newEVRig(t)
	subscribe(t, h, rec)

	args := holdArgs()
	args["hold_s"] = 600
	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpLoadpointHold, "cmd-hold-2", args, 7, 200_000))

	if res := body[CmdResult](t, rec.only(t, MsgCmdResult)); res.State != CmdApplied {
		t.Fatalf("state = %q (%+v)", res.State, res.Error)
	}
	if lp.hold.Persistent {
		t.Fatal("a timed hold was installed as persistent")
	}
	want := clock.now.Add(600 * time.Second)
	if !lp.hold.ExpiresAt.Equal(want) {
		t.Fatalf("hold expires %v, want %v", lp.hold.ExpiresAt, want)
	}
}

// Clearing is its own signal, not a zero setpoint — 0 W is a valid hold that
// pauses the charger. The release goes through ClearManualHold, the same call
// the HTTP DELETE makes.
func TestClearReleasesTheHoldThroughTheSameDoor(t *testing.T) {
	h, lp, _, rec, _ := newEVRig(t)
	subscribe(t, h, rec)

	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpLoadpointHold, "cmd-hold-3", holdArgs(), 7, 200_000))
	rec.reset()

	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpLoadpointHold, "cmd-clear-1",
		map[string]any{"id": "lp1", "clear": true}, 7, 200_000))

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdApplied {
		t.Fatalf("state = %q (%+v), want applied", res.State, res.Error)
	}
	if res.Observed == nil || res.Observed.Value != 0 {
		t.Fatalf("observed = %+v, want 0 W after the release", res.Observed)
	}
	if lp.clearCalls != 1 {
		t.Fatalf("ClearHold ran %d times", lp.clearCalls)
	}
	if lp.holdSet {
		t.Fatal("the hold survived its clear")
	}
}

func TestLoadpointBoostAcksThenConfirmsFromAReadBack(t *testing.T) {
	h, lp, _, rec, clock := newEVRig(t)
	subscribe(t, h, rec)

	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpLoadpointBoost, "cmd-boost-1", boostArgs(), 7, 200_000))

	ack := body[CmdAck](t, rec.only(t, MsgCmdAck))
	if ack.LeaseID == "" {
		t.Fatal("ack carried no lease")
	}

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdApplied {
		t.Fatalf("state = %q (%+v), want applied", res.State, res.Error)
	}
	if res.Observed == nil || res.Observed.Src != ObservedSrcCore || res.Observed.Value != 1 {
		t.Fatalf("observed = %+v, want an active boost read back from core", res.Observed)
	}

	if lp.boostCalls != 1 || lp.boostID != "lp1" {
		t.Fatalf("boost reached the controller %d times for %q", lp.boostCalls, lp.boostID)
	}
	if !lp.boostLease.StartedAt.Equal(clock.now) {
		t.Fatalf("lease starts %v, want the box's now", lp.boostLease.StartedAt)
	}
	if want := clock.now.Add(time.Hour); !lp.boostLease.ExpiresAt.Equal(want) {
		t.Fatalf("lease expires %v, want %v", lp.boostLease.ExpiresAt, want)
	}
	if lp.boostLease.MinBatterySoCPct != 30 || lp.boostLease.EVTargetSoCPct != 80 {
		t.Fatalf("lease = %+v", lp.boostLease)
	}
}

// Cancelling follows the boost's own convention — the idempotent withdrawal
// the HTTP DELETE performs — and the result reports the status read back.
func TestCancelWithdrawsTheBoost(t *testing.T) {
	h, lp, _, rec, _ := newEVRig(t)
	subscribe(t, h, rec)

	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpLoadpointBoost, "cmd-boost-2", boostArgs(), 7, 200_000))
	rec.reset()

	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpLoadpointBoost, "cmd-cancel-1",
		map[string]any{"id": "lp1", "cancel": true}, 7, 200_000))

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdApplied {
		t.Fatalf("state = %q (%+v), want applied", res.State, res.Error)
	}
	if res.Observed == nil || res.Observed.Value != 0 {
		t.Fatalf("observed = %+v, want 0 after the withdrawal", res.Observed)
	}
	if lp.cancelCalls != 1 {
		t.Fatalf("CancelBoost ran %d times", lp.cancelCalls)
	}
	if lp.boostActive {
		t.Fatal("the boost survived its cancel")
	}
}

// A command queued while the phone was in a tunnel must never move a charger
// as though the user had just pressed the button.
func TestAnExpiredLoadpointCommandNeverReachesTheCharger(t *testing.T) {
	for _, c := range []struct {
		op   string
		args map[string]any
	}{
		{OpLoadpointHold, holdArgs()},
		{OpLoadpointBoost, boostArgs()},
	} {
		t.Run(c.op, func(t *testing.T) {
			h, lp, _, rec, clock := newEVRig(t)
			subscribe(t, h, rec)

			deliver(t, h, MsgCmd, nil, cmdLoadpoint(c.op, "cmd-late", c.args, 7, clock.uptimeMs-1))

			res := body[CmdResult](t, rec.only(t, MsgCmdResult))
			if res.State != CmdExpired || res.Error == nil || res.Error.Code != ErrCmdExpired {
				t.Fatalf("result = %+v, want expired/%s", res, ErrCmdExpired)
			}
			if lp.holdCalls != 0 || lp.boostCalls != 0 {
				t.Fatal("an expired command reached the controller")
			}
			if rec.has(MsgCmdAck) {
				t.Fatal("an expired command was acked")
			}
		})
	}
}

// A loadpoint the box does not have gets a rejection naming the argument, in
// machine form, and nothing is touched.
func TestAnUnknownLoadpointIdIsRefused(t *testing.T) {
	for _, c := range []struct {
		op   string
		args map[string]any
	}{
		{OpLoadpointHold, map[string]any{"id": "lp9", "power_w": 7360, "hold_s": 0}},
		{OpLoadpointBoost, map[string]any{"id": "lp9", "duration_s": 3600, "min_battery_soc_pct": 30}},
	} {
		t.Run(c.op, func(t *testing.T) {
			h, lp, _, rec, _ := newEVRig(t)
			subscribe(t, h, rec)

			deliver(t, h, MsgCmd, nil, cmdLoadpoint(c.op, "cmd-lost", c.args, 7, 200_000))

			res := body[CmdResult](t, rec.only(t, MsgCmdResult))
			if res.State != CmdRejected || res.Error == nil || res.Error.Code != ErrUnknownOp {
				t.Fatalf("result = %+v", res)
			}
			if res.Error.Args["arg"] != "id" {
				t.Fatalf("refusal args = %v, want the argument named", res.Error.Args)
			}
			if lp.holdCalls != 0 || lp.boostCalls != 0 {
				t.Fatal("an unknown loadpoint reached the controller")
			}
			if rec.has(MsgCmdAck) {
				t.Fatal("a refused command was acked")
			}
		})
	}
}

// A grant without ftw.dispatch.write moves no energy. Asserted on the
// controller and not only on the error, for the same reason the mode's
// viewer test is: a test that only reads the refusal passes just as happily
// with the check deleted.
func TestAViewersLoadpointCommandNeverReachesTheCharger(t *testing.T) {
	for _, c := range []struct {
		op   string
		args map[string]any
	}{
		{OpLoadpointHold, holdArgs()},
		{OpLoadpointBoost, boostArgs()},
	} {
		t.Run(c.op, func(t *testing.T) {
			h, lp, _, rec, _ := newEVRig(t)
			h.cfg.Caller = viewerCaller()
			h.cfg.Grants = newViewerGrants()
			subscribe(t, h, rec)

			deliver(t, h, MsgCmd, nil, cmdLoadpoint(c.op, "cmd-viewer", c.args, 7, 200_000))

			res := body[CmdResult](t, rec.only(t, MsgCmdResult))
			if res.State != CmdRejected || res.Error == nil || res.Error.Code != ErrScopeDenied {
				t.Fatalf("result = %+v, want rejected/%s", res, ErrScopeDenied)
			}
			if res.Error.Args["needScope"] != ScopeDispatchWrite {
				t.Fatalf("refusal args = %v, want the scope it needs", res.Error.Args)
			}
			if lp.holdCalls != 0 || lp.clearCalls != 0 || lp.boostCalls != 0 || lp.cancelCalls != 0 {
				t.Fatal("a viewer reached the loadpoint controller")
			}
			if rec.has(MsgCmdAck) {
				t.Fatal("a refused command was acked")
			}
		})
	}
}

// The box's own invariant: stale meter data stops dispatch. Both loadpoint
// ops move energy, so neither gets around it through this door — the rule
// the generic dispatch-write test pins with a synthetic op, held here by the
// two real ones.
func TestLoadpointCommandsAreRefusedWhileDispatchIsBlocked(t *testing.T) {
	for _, c := range []struct {
		op   string
		args map[string]any
	}{
		{OpLoadpointHold, holdArgs()},
		{OpLoadpointBoost, boostArgs()},
	} {
		t.Run(c.op, func(t *testing.T) {
			h, lp, box, rec, _ := newEVRig(t)
			subscribe(t, h, rec)
			box.snap.DispatchBlockedBy = []string{"meter.p1"}

			deliver(t, h, MsgCmd, nil, cmdLoadpoint(c.op, "cmd-blocked", c.args, 7, 200_000))

			res := body[CmdResult](t, rec.only(t, MsgCmdResult))
			if res.State != CmdRejected || res.Error == nil || res.Error.Code != ErrUnavailable {
				t.Fatalf("result = %+v, want rejected/%s", res, ErrUnavailable)
			}
			if res.Error.Args["dispatchBlockedBy"] == nil {
				t.Fatal("the refusal did not say which source blocked it")
			}
			if lp.holdCalls != 0 || lp.boostCalls != 0 {
				t.Fatal("a blocked dispatch reached the controller")
			}
		})
	}
}

// On a box with no loadpoint controller the op is still known — it is in
// this build's table — and the honest answer is that the subsystem is
// missing, the session's word for the 503 the HTTP routes give. Distinct
// from battery.hold, which no box implements and which stays E_UNKNOWN_OP.
func TestABoxWithoutLoadpointsSaysUnavailableNotUnknown(t *testing.T) {
	h, _, rec, _ := newRig(t)
	subscribe(t, h, rec)

	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpLoadpointHold, "cmd-noev", holdArgs(), 7, 200_000))

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdRejected || res.Error == nil || res.Error.Code != ErrUnavailable {
		t.Fatalf("result = %+v, want rejected/%s", res, ErrUnavailable)
	}
	if res.Error.Args["subsystem"] != "loadpoints" {
		t.Fatalf("refusal args = %v, want the subsystem named", res.Error.Args)
	}
}

// A retry replays the outcome and the charger is commanded once. The same
// idempotency the mode has, proved against this op's own handler.
func TestARetriedHoldReplaysTheOutcomeAndDoesNotActTwice(t *testing.T) {
	h, lp, _, rec, _ := newEVRig(t)
	subscribe(t, h, rec)

	cmd := cmdLoadpoint(OpLoadpointHold, "cmd-retry", holdArgs(), 7, 200_000)
	deliver(t, h, MsgCmd, nil, cmd)
	firstAck := body[CmdAck](t, rec.only(t, MsgCmdAck))
	rec.reset()

	deliver(t, h, MsgCmd, nil, cmd)

	if lp.holdCalls != 1 {
		t.Fatalf("the controller ran %d times for one command id", lp.holdCalls)
	}
	replayAck := body[CmdAck](t, rec.only(t, MsgCmdAck))
	if replayAck.LeaseID != firstAck.LeaseID {
		t.Fatalf("retry issued a new lease %q, original %q", replayAck.LeaseID, firstAck.LeaseID)
	}
	if res := body[CmdResult](t, rec.only(t, MsgCmdResult)); res.State != CmdApplied {
		t.Fatalf("retry replayed state %q", res.State)
	}
}

// The controller refusing a lease — vehicle unplugged, battery not ready —
// is the HTTP route's 409, reported after the ack rather than swallowed.
func TestControlRefusingTheBoostIsReportedNotSwallowed(t *testing.T) {
	h, lp, _, rec, _ := newEVRig(t)
	subscribe(t, h, rec)
	lp.boostErr = errors.New("vehicle is not plugged in")

	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpLoadpointBoost, "cmd-refused", boostArgs(), 7, 200_000))

	if !rec.has(MsgCmdAck) {
		t.Fatal("the dispatcher took the intent but never acked it")
	}
	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdRejected || res.Error == nil || res.Error.Code != ErrUnavailable {
		t.Fatalf("result = %+v", res)
	}
}

// Arguments are validated to the same rules the HTTP bodies enforce, and a
// refusal names the argument so the app can say which one.
func TestAMalformedLoadpointCommandIsRefused(t *testing.T) {
	cases := []struct {
		name string
		op   string
		args map[string]any
		arg  string
	}{
		{"negative power", OpLoadpointHold,
			map[string]any{"id": "lp1", "power_w": -100, "hold_s": 0}, "power_w"},
		{"hold beyond the cap", OpLoadpointHold,
			map[string]any{"id": "lp1", "power_w": 7360, "hold_s": 1801}, "hold_s"},
		{"fractional hold seconds", OpLoadpointHold,
			map[string]any{"id": "lp1", "power_w": 7360, "hold_s": 1.5}, "hold_s"},
		{"phase mode the box does not know", OpLoadpointHold,
			map[string]any{"id": "lp1", "power_w": 7360, "hold_s": 0, "phase_mode": "2p"}, "phase_mode"},
		{"both duration and expiry", OpLoadpointBoost,
			map[string]any{"id": "lp1", "duration_s": 3600, "expires_at_ms": 1_760_003_600_000,
				"min_battery_soc_pct": 30}, "duration_s"},
		{"neither duration nor expiry", OpLoadpointBoost,
			map[string]any{"id": "lp1", "min_battery_soc_pct": 30}, "duration_s"},
		{"a lease the shared validator refuses", OpLoadpointBoost,
			map[string]any{"id": "lp1", "duration_s": 3600, "min_battery_soc_pct": 0}, "lease"},
		{"a lease longer than the boost allows", OpLoadpointBoost,
			map[string]any{"id": "lp1", "duration_s": 5 * 3600, "min_battery_soc_pct": 30}, "lease"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, lp, _, rec, _ := newEVRig(t)
			subscribe(t, h, rec)

			deliver(t, h, MsgCmd, nil, cmdLoadpoint(c.op, "cmd-bad", c.args, 7, 200_000))

			res := body[CmdResult](t, rec.only(t, MsgCmdResult))
			if res.State != CmdRejected || res.Error == nil || res.Error.Code != ErrUnknownOp {
				t.Fatalf("result = %+v", res)
			}
			if res.Error.Args["arg"] != c.arg {
				t.Fatalf("refusal named %v, want %q", res.Error.Args["arg"], c.arg)
			}
			if lp.holdCalls != 0 || lp.boostCalls != 0 {
				t.Fatal("a malformed command reached the controller")
			}
			if rec.has(MsgCmdAck) {
				t.Fatal("a malformed command was acked")
			}
		})
	}
}

// Something else moved the hold between the write and the read. The result
// reports what the box holds now, not what was asked for.
func TestAHoldReadBackThatDisagreesIsSupersededNotApplied(t *testing.T) {
	h, lp, _, rec, _ := newEVRig(t)
	subscribe(t, h, rec)
	lp.onHold = func() { lp.hold.PowerW = 3680 }

	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpLoadpointHold, "cmd-moved", holdArgs(), 7, 200_000))

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdSuperseded {
		t.Fatalf("state = %q, want superseded", res.State)
	}
	if res.Observed == nil || res.Observed.Value != 3680 {
		t.Fatalf("observed = %+v, want the power actually held", res.Observed)
	}
}

// A boost stopped between the write and the read — unplug, safety — is
// reported from the status, never claimed applied on the echo.
func TestABoostStoppedBeforeTheReadBackIsSuperseded(t *testing.T) {
	h, lp, _, rec, _ := newEVRig(t)
	subscribe(t, h, rec)
	lp.onBoost = func() { lp.boostActive = false }

	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpLoadpointBoost, "cmd-stopped", boostArgs(), 7, 200_000))

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdSuperseded {
		t.Fatalf("state = %q, want superseded", res.State)
	}
	if res.Observed == nil || res.Observed.Value != 0 {
		t.Fatalf("observed = %+v, want the boost read back as stopped", res.Observed)
	}
}

// A hold and a boost change what the box means to do next, so the plan is
// remade and pushed unasked — the same rule an applied mode change has.
func TestAppliedLoadpointCommandsPushAFreshPlan(t *testing.T) {
	for _, c := range []struct {
		op   string
		args map[string]any
	}{
		{OpLoadpointHold, holdArgs()},
		{OpLoadpointBoost, boostArgs()},
	} {
		t.Run(c.op, func(t *testing.T) {
			h, _, _, rec, _ := newEVRig(t)
			subscribe(t, h, rec)

			deliver(t, h, MsgCmd, nil, cmdLoadpoint(c.op, "cmd-plan", c.args, 7, 200_000))

			if res := body[CmdResult](t, rec.only(t, MsgCmdResult)); res.State != CmdApplied {
				t.Fatalf("state = %q (%+v)", res.State, res.Error)
			}
			if !rec.has(MsgPlan) {
				t.Fatalf("no plan followed the command; frames were %s", rec.types())
			}
		})
	}
}
