package mpc

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestPlanSnapshotFailedRequestDoesNotMakeOldPlanCurrent(t *testing.T) {
	svc := newCancellationTestService(t, nil)
	now := time.Now()
	svc.InstallPlan(Plan{GeneratedAtMs: now.UnixMilli(), Actions: []Action{{
		SlotStartMs: now.UnixMilli(), SlotLenMin: 60, LoadpointW: 6000,
	}}}, svc.Defaults, "garage")
	before := svc.PlanSnapshot()
	if windows, wh := before.LoadpointPlanWindows("garage", now, 0); len(windows) != 1 || wh != 6000 {
		t.Fatalf("initial windows=%v wh=%v", windows, wh)
	}
	// Missing prices is a normal early return that retains the old plan for
	// dispatch. It must not claim that plan includes the newly saved setting.
	svc.Zone = "no-prices"
	svc.RequestReplan("saved-goal")
	waitForRequestedReplans(t, svc)
	failed := svc.PlanSnapshot()
	if failed.Pending || !failed.Outdated || failed.Plan != before.Plan || !failed.ReplanAt.Equal(before.ReplanAt) || failed.Reason != before.Reason {
		t.Fatalf("failed request freshness=%+v", failed)
	}
	if windows, wh := failed.LoadpointPlanWindows("garage", now, 0); len(windows) != 0 || wh != 0 {
		t.Fatalf("failed new request exposes old windows=%v wh=%v", windows, wh)
	}

	svc.Zone = "SE3"
	svc.RequestReplan("retry-goal")
	waitForRequestedReplans(t, svc)
	after := svc.PlanSnapshot()
	if after.Pending || after.Outdated || after.Plan == nil || after.Plan == before.Plan || after.Reason != "retry-goal" {
		t.Fatalf("successful retry freshness=%+v", after)
	}
	// A caller that already captured the old snapshot keeps its own matching
	// plan and legacy loadpoint ID even after the service publishes another.
	if windows, wh := before.LoadpointPlanWindows("garage", now, 0); len(windows) != 1 || wh != 6000 {
		t.Fatalf("captured snapshot changed with the service: windows=%v wh=%v", windows, wh)
	}
}

func TestPlanSnapshotIsCurrentBeforeDiagnosticWriteFinishes(t *testing.T) {
	svc := newCancellationTestService(t, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() { unblock(); waitForRequestedReplans(t, svc) })
	svc.SaveDiag = func(*Diagnostic, string) error {
		close(entered)
		<-release
		return nil
	}
	svc.RequestReplan("saved-soc")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not publish its plan")
	}
	snapshot := svc.PlanSnapshot()
	if snapshot.Plan == nil || snapshot.ReplanAt.IsZero() || snapshot.Reason != "saved-soc" || snapshot.Pending || snapshot.Outdated {
		t.Fatalf("published snapshot=%+v", snapshot)
	}
	if !svc.IsReplanning() {
		t.Fatal("work waiter lost the outstanding diagnostic write")
	}
}

func TestRequestReplanCoalescesEditsWhileOldSolveIsBlocked(t *testing.T) {
	optimizer := &blockingFirstOptimizer{
		firstMode: make(chan Mode, 1), secondMode: make(chan Mode, 1), releaseFirst: make(chan struct{}),
	}
	svc := newCancellationTestService(t, optimizer)
	var once sync.Once
	unblock := func() { once.Do(func() { close(optimizer.releaseFirst) }) }
	t.Cleanup(func() { unblock(); waitForRequestedReplans(t, svc) })
	svc.RequestReplan("first")
	select {
	case <-optimizer.firstMode:
	case <-time.After(time.Second):
		t.Fatal("first request did not reach optimizer")
	}
	for i := 0; i < 100; i++ {
		svc.RequestReplan(fmt.Sprintf("edit-%d", i))
	}
	if calls := optimizer.calls.Load(); calls != 1 {
		t.Fatalf("edits started %d parallel solves while the old one was blocked", calls)
	}
	if snapshot := svc.PlanSnapshot(); !snapshot.Pending || !snapshot.Outdated || snapshot.Plan != nil {
		t.Fatalf("pending edits freshness=%+v", snapshot)
	}
	unblock()
	waitForRequestedReplans(t, svc)
	if calls := optimizer.calls.Load(); calls != 2 {
		t.Fatalf("optimizer calls=%d, want old request and latest edit only", calls)
	}
	snapshot := svc.PlanSnapshot()
	if snapshot.Plan == nil || snapshot.Pending || snapshot.Outdated || snapshot.Reason != "edit-99" {
		t.Fatalf("latest edit not published: %+v", snapshot)
	}
}

func TestStopWaitsForRequestedReplanAndDiscardsQueuedEdits(t *testing.T) {
	optimizer := &stopWaitOptimizer{
		secondStarted: make(chan struct{}), secondCanceled: make(chan struct{}),
		releaseSecond: make(chan struct{}), closed: make(chan struct{}),
	}
	svc := newCancellationTestService(t, optimizer)
	var once sync.Once
	unblock := func() { once.Do(func() { close(optimizer.releaseSecond) }) }
	svc.Start(context.Background())
	t.Cleanup(func() { unblock(); svc.Stop() })
	deadline := time.Now().Add(time.Second)
	for svc.Latest() == nil || svc.IsReplanning() {
		if time.Now().After(deadline) {
			t.Fatal("initial scheduled plan did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	svc.RequestReplan("blocked-edit")
	select {
	case <-optimizer.secondStarted:
	case <-time.After(time.Second):
		t.Fatal("requested plan did not reach optimizer")
	}
	for i := 0; i < 10; i++ {
		svc.RequestReplan("queued-edit")
	}
	stopped := make(chan struct{})
	go func() { svc.Stop(); close(stopped) }()
	deadline = time.Now().Add(time.Second)
	for {
		svc.mu.RLock()
		stopping := svc.stopping
		svc.mu.RUnlock()
		if stopping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Stop did not begin")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-stopped:
		t.Fatal("Stop returned while the optimizer still ran")
	default:
	}
	unblock()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not drain the requested replan queue")
	}
	svc.RequestReplan("after-stop")
	if optimizer.closedWhileActive.Load() || optimizer.calls.Load() != 2 {
		t.Fatalf("unsafe stop: closed while active=%v, calls=%d", optimizer.closedWhileActive.Load(), optimizer.calls.Load())
	}
	if snapshot := svc.PlanSnapshot(); snapshot.Pending || !snapshot.Outdated {
		t.Fatalf("stopped request freshness=%+v", snapshot)
	}
}

func waitForRequestedReplans(t *testing.T, svc *Service) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		svc.mu.RLock()
		idle := !svc.requestedReplanRunning && svc.queuedReplan == nil
		svc.mu.RUnlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("requested replans did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}
