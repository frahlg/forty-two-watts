package ftwdbshadow

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

type sourceFunc func(context.Context, PullRequest) (CommitBatchRequest, bool, error)

func (fn sourceFunc) ReadBatch(ctx context.Context, request PullRequest) (CommitBatchRequest, bool, error) {
	return fn(ctx, request)
}

type fakeExportSession struct {
	health func(context.Context, uint64) (HealthResponse, error)
	commit func(context.Context, PreparedCommit) (DurableCommitResult, error)
	flush  func(context.Context, uint64) (Ack, error)
	closed int
}

func (session *fakeExportSession) Health(ctx context.Context, nonce uint64) (HealthResponse, error) {
	return session.health(ctx, nonce)
}

func (session *fakeExportSession) CommitDurable(
	ctx context.Context,
	prepared PreparedCommit,
) (DurableCommitResult, error) {
	return session.commit(ctx, prepared)
}

func (session *fakeExportSession) Flush(ctx context.Context, sequence uint64) (Ack, error) {
	if session.flush == nil {
		return Ack{}, errors.New("unexpected flush")
	}
	return session.flush(ctx, sequence)
}

func (session *fakeExportSession) Close() error {
	session.closed++
	return nil
}

func TestPrepareExportCommitIsContentDerived(t *testing.T) {
	first := validExportBatch(testID(1), 42)
	first.Entities[0].Properties = map[string]PropertyValue{
		"z": IntegerProperty(2),
		"a": TextProperty("same"),
	}
	second := validExportBatch(testID(1), 42)
	second.Entities[0].Properties = map[string]PropertyValue{
		"a": TextProperty("same"),
		"z": IntegerProperty(2),
	}
	first.CommitID = testID(90)
	second.CommitID = testID(91)

	preparedFirst, err := PrepareExportCommit(first)
	if err != nil {
		t.Fatal(err)
	}
	preparedSecond, err := PrepareExportCommit(second)
	if err != nil {
		t.Fatal(err)
	}
	if preparedFirst.CommitID().IsZero() {
		t.Fatal("content-derived commit ID is zero")
	}
	if preparedFirst.CommitID() != preparedSecond.CommitID() {
		t.Fatalf("commit IDs differ: %s != %s", preparedFirst.CommitID(), preparedSecond.CommitID())
	}
	if !bytes.Equal(preparedFirst.Bytes(), preparedSecond.Bytes()) {
		t.Fatal("canonical batches produced different retry frames")
	}
}

func TestExporterAbsentSidecarNeverReadsSource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads := 0
	connects := 0
	deps := fixedExporterDependencies(time.UnixMilli(50_000))
	deps.connect = func(context.Context, ClientConfig) (exportSession, error) {
		connects++
		return nil, retryableTransportError(fs.ErrNotExist)
	}
	deps.wait = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	source := sourceFunc(func(context.Context, PullRequest) (CommitBatchRequest, bool, error) {
		reads++
		return CommitBatchRequest{}, false, nil
	})

	if err := runExporter(ctx, source, validExporterConfig(), deps); err != nil {
		t.Fatal(err)
	}
	if connects != 1 || reads != 0 {
		t.Fatalf("connects=%d reads=%d, want 1 and 0", connects, reads)
	}
}

func TestExporterStalledHealthNeverReadsSource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads := 0
	session := &fakeExportSession{
		health: func(context.Context, uint64) (HealthResponse, error) {
			return HealthResponse{}, retryableTransportError(context.DeadlineExceeded)
		},
		commit: unexpectedCommit(t),
	}
	deps := fixedExporterDependencies(time.UnixMilli(50_000))
	deps.connect = oneSessionConnector(t, session)
	deps.wait = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	source := sourceFunc(func(context.Context, PullRequest) (CommitBatchRequest, bool, error) {
		reads++
		return CommitBatchRequest{}, false, nil
	})

	if err := runExporter(ctx, source, validExporterConfig(), deps); err != nil {
		t.Fatal(err)
	}
	if reads != 0 {
		t.Fatalf("source read %d times while health stalled", reads)
	}
	if session.closed != 1 {
		t.Fatalf("session closed %d times, want 1", session.closed)
	}
}

func TestExporterDegradedHealthPausesBeforeSourceRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads := 0
	config := validExporterConfig()
	session := &fakeExportSession{
		health: func(_ context.Context, nonce uint64) (HealthResponse, error) {
			return HealthResponse{
				Nonce:        nonce,
				SourceID:     config.SourceID,
				Status:       HealthDegraded,
				QueueEntries: 12,
			}, nil
		},
		commit: unexpectedCommit(t),
	}
	deps := fixedExporterDependencies(time.UnixMilli(50_000))
	deps.connect = oneSessionConnector(t, session)
	deps.wait = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	source := sourceFunc(func(context.Context, PullRequest) (CommitBatchRequest, bool, error) {
		reads++
		return CommitBatchRequest{}, false, nil
	})

	if err := runExporter(ctx, source, config, deps); err != nil {
		t.Fatal(err)
	}
	if reads != 0 {
		t.Fatalf("source read %d times while sidecar was degraded", reads)
	}
}

func TestExporterCancellationDuringSourceReadStopsCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	config := validExporterConfig()
	source := sourceFunc(func(context.Context, PullRequest) (CommitBatchRequest, bool, error) {
		cancel()
		return CommitBatchRequest{}, false, context.Canceled
	})
	session := &fakeExportSession{
		health: healthWithDurable(config.SourceID, nil),
		commit: unexpectedCommit(t),
	}
	deps := fixedExporterDependencies(time.UnixMilli(50_000))
	deps.connect = oneSessionConnector(t, session)

	if err := runExporter(ctx, source, config, deps); err != nil {
		t.Fatalf("cancelled exporter returned %v", err)
	}
	if session.closed != 1 {
		t.Fatalf("session closed %d times, want 1", session.closed)
	}
}

func TestExporterRetriesExactFrameAfterLostAckWithoutAdvancingCursor(t *testing.T) {
	now := time.UnixMilli(50_000)
	config := validExporterConfig()
	config.Once = true
	baseline := backfillCursor(now, config.Backfill)
	sequence := baseline + 10
	var cursors []uint64
	source := sourceFunc(func(_ context.Context, request PullRequest) (CommitBatchRequest, bool, error) {
		cursors = append(cursors, request.AfterSequence)
		if request.AfterSequence == baseline {
			return validExportBatch(config.SourceID, sequence), true, nil
		}
		return CommitBatchRequest{}, false, nil
	})
	var frames [][]byte
	first := &fakeExportSession{
		health: healthWithDurable(config.SourceID, nil),
		commit: func(_ context.Context, prepared PreparedCommit) (DurableCommitResult, error) {
			frames = append(frames, prepared.Bytes())
			return DurableCommitResult{}, retryableTransportError(io.ErrUnexpectedEOF)
		},
	}
	durable := sequence
	second := &fakeExportSession{
		health: healthWithWatermarks(config.SourceID, &sequence, nil),
		commit: func(_ context.Context, prepared PreparedCommit) (DurableCommitResult, error) {
			frames = append(frames, prepared.Bytes())
			return DurableCommitResult{DurableThrough: durable}, nil
		},
	}
	deps := fixedExporterDependencies(now)
	deps.connect = sessionSequenceConnector(t, first, second)

	if err := runExporter(context.Background(), source, config, deps); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || !bytes.Equal(frames[0], frames[1]) {
		t.Fatalf("exact retry frames differ or count=%d", len(frames))
	}
	if !slices.Equal(cursors, []uint64{baseline, sequence}) {
		t.Fatalf("source cursors = %v, want [%d %d]", cursors, baseline, sequence)
	}
}

func TestExporterUsesDurableHealthAfterLostAck(t *testing.T) {
	now := time.UnixMilli(50_000)
	config := validExporterConfig()
	config.Once = true
	baseline := backfillCursor(now, config.Backfill)
	sequence := baseline + 10
	var cursors []uint64
	source := sourceFunc(func(_ context.Context, request PullRequest) (CommitBatchRequest, bool, error) {
		cursors = append(cursors, request.AfterSequence)
		if request.AfterSequence == baseline {
			return validExportBatch(config.SourceID, sequence), true, nil
		}
		return CommitBatchRequest{}, false, nil
	})
	commits := 0
	first := &fakeExportSession{
		health: healthWithDurable(config.SourceID, nil),
		commit: func(context.Context, PreparedCommit) (DurableCommitResult, error) {
			commits++
			return DurableCommitResult{}, retryableTransportError(io.ErrUnexpectedEOF)
		},
	}
	second := &fakeExportSession{
		health: healthWithDurable(config.SourceID, &sequence),
		commit: func(context.Context, PreparedCommit) (DurableCommitResult, error) {
			commits++
			return DurableCommitResult{}, errors.New("pending frame was sent after durable health proof")
		},
	}
	deps := fixedExporterDependencies(now)
	deps.connect = sessionSequenceConnector(t, first, second)

	if err := runExporter(context.Background(), source, config, deps); err != nil {
		t.Fatal(err)
	}
	if commits != 1 {
		t.Fatalf("commit called %d times, want 1", commits)
	}
	if !slices.Equal(cursors, []uint64{baseline, sequence}) {
		t.Fatalf("source cursors = %v, want [%d %d]", cursors, baseline, sequence)
	}
}

func TestExporterCommitStallDoesNotAdvanceSourceCursor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.UnixMilli(50_000)
	config := validExporterConfig()
	baseline := backfillCursor(now, config.Backfill)
	sequence := baseline + 10
	var cursors []uint64
	source := sourceFunc(func(_ context.Context, request PullRequest) (CommitBatchRequest, bool, error) {
		cursors = append(cursors, request.AfterSequence)
		return validExportBatch(config.SourceID, sequence), true, nil
	})
	session := &fakeExportSession{
		health: healthWithDurable(config.SourceID, nil),
		commit: func(context.Context, PreparedCommit) (DurableCommitResult, error) {
			return DurableCommitResult{}, retryableTransportError(context.DeadlineExceeded)
		},
	}
	deps := fixedExporterDependencies(now)
	deps.connect = oneSessionConnector(t, session)
	deps.wait = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}

	if err := runExporter(ctx, source, config, deps); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cursors, []uint64{baseline}) {
		t.Fatalf("source cursors = %v, want only %d", cursors, baseline)
	}
}

func TestExporterFlushesAcceptedBatchWithoutReadingSource(t *testing.T) {
	now := time.UnixMilli(50_000)
	config := validExporterConfig()
	config.Once = true
	durable := uint64(20_000)
	accepted := uint64(20_010)
	reads := 0
	source := sourceFunc(func(context.Context, PullRequest) (CommitBatchRequest, bool, error) {
		reads++
		return CommitBatchRequest{}, false, nil
	})
	flushes := 0
	session := &fakeExportSession{
		health: healthWithWatermarks(config.SourceID, &accepted, &durable),
		commit: unexpectedCommit(t),
		flush: func(_ context.Context, through uint64) (Ack, error) {
			flushes++
			if through != accepted {
				t.Fatalf("flush through %d, want %d", through, accepted)
			}
			return durableFlushAck(config.SourceID, accepted), nil
		},
	}
	deps := fixedExporterDependencies(now)
	deps.connect = oneSessionConnector(t, session)

	if err := runExporter(context.Background(), source, config, deps); err != nil {
		t.Fatal(err)
	}
	if flushes != 1 {
		t.Fatalf("flushes = %d, want 1", flushes)
	}
	if reads != 1 {
		t.Fatalf("source reads = %d, want only the post-flush empty read", reads)
	}
}

func TestExporterFlushesFirstAcceptedBatchWithoutDurableLowerCursor(t *testing.T) {
	config := validExporterConfig()
	config.Once = true
	accepted := uint64(20_010)
	reads := 0
	source := sourceFunc(func(context.Context, PullRequest) (CommitBatchRequest, bool, error) {
		reads++
		return CommitBatchRequest{}, false, nil
	})
	session := &fakeExportSession{
		health: healthWithWatermarks(config.SourceID, &accepted, nil),
		commit: unexpectedCommit(t),
		flush: func(context.Context, uint64) (Ack, error) {
			return durableFlushAck(config.SourceID, accepted), nil
		},
	}
	deps := fixedExporterDependencies(time.UnixMilli(50_000))
	deps.connect = oneSessionConnector(t, session)

	if err := runExporter(context.Background(), source, config, deps); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("source reads = %d, want only the post-flush empty read", reads)
	}
}

func TestExporterRejectsAcceptedCursorBelowDurableCursor(t *testing.T) {
	config := validExporterConfig()
	accepted := uint64(20_000)
	durable := uint64(20_010)
	reads := 0
	source := sourceFunc(func(context.Context, PullRequest) (CommitBatchRequest, bool, error) {
		reads++
		return CommitBatchRequest{}, false, nil
	})
	session := &fakeExportSession{
		health: healthWithWatermarks(config.SourceID, &accepted, &durable),
		commit: unexpectedCommit(t),
	}
	deps := fixedExporterDependencies(time.UnixMilli(50_000))
	deps.connect = oneSessionConnector(t, session)

	err := runExporter(context.Background(), source, config, deps)
	if err == nil || !strings.Contains(err.Error(), "accepted cursor 20000 is below durable cursor 20010") {
		t.Fatalf("error = %v", err)
	}
	if reads != 0 {
		t.Fatalf("source read %d times", reads)
	}
}

func TestExporterStopsWhenAcceptedFlushDoesNotReachDurableCursor(t *testing.T) {
	config := validExporterConfig()
	config.Once = true
	durable := uint64(20_000)
	accepted := uint64(20_010)
	reads := 0
	source := sourceFunc(func(context.Context, PullRequest) (CommitBatchRequest, bool, error) {
		reads++
		return CommitBatchRequest{}, false, nil
	})
	session := &fakeExportSession{
		health: healthWithWatermarks(config.SourceID, &accepted, &durable),
		commit: unexpectedCommit(t),
		flush: func(context.Context, uint64) (Ack, error) {
			return durableFlushAck(config.SourceID, durable), nil
		},
	}
	deps := fixedExporterDependencies(time.UnixMilli(50_000))
	deps.connect = oneSessionConnector(t, session)

	err := runExporter(context.Background(), source, config, deps)
	if err == nil || !strings.Contains(err.Error(), "did not make accepted sequence") {
		t.Fatalf("error = %v", err)
	}
	if reads != 0 {
		t.Fatalf("source read %d times before durable proof", reads)
	}
}

func TestDumpExportFramesNeverNeedsSocket(t *testing.T) {
	config := validExporterConfig()
	config.SocketPath = ""
	config.NodeID = ""
	config.ClientVersion = ""
	config.Once = true
	sequence := uint64(time.Now().Add(time.Hour).UnixMilli())
	source := sourceFunc(func(context.Context, PullRequest) (CommitBatchRequest, bool, error) {
		return validExportBatch(config.SourceID, sequence), true, nil
	})
	var frame []byte
	count, err := DumpExportFrames(context.Background(), source, config, func(prepared PreparedCommit) error {
		frame = prepared.Bytes()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("dumped %d frames, want 1", count)
	}
	message, err := Decode(frame)
	if err != nil {
		t.Fatal(err)
	}
	batch, ok := message.(CommitBatchRequest)
	if !ok || batch.Sequence != sequence || batch.CommitID.IsZero() {
		t.Fatalf("dump frame = %#v", message)
	}
}

func TestCappedDoubleStopsAtMaximum(t *testing.T) {
	if got := cappedDouble(4*time.Second, 5*time.Second); got != 5*time.Second {
		t.Fatalf("cappedDouble = %s, want 5s", got)
	}
	if got := cappedDouble(5*time.Second, 5*time.Second); got != 5*time.Second {
		t.Fatalf("cappedDouble at cap = %s, want 5s", got)
	}
}

func validExporterConfig() ExporterConfig {
	return ExporterConfig{
		SocketPath:    "/run/ftwdb-shadow/ftwdb-shadow.sock",
		SourceID:      testID(1),
		NodeID:        "ftw-test",
		ClientVersion: "test",
		Backfill:      10 * time.Second,
		PollInterval:  time.Second,
		SettleDelay:   2 * time.Second,
		IOTimeout:     time.Second,
		RetryInitial:  time.Millisecond,
		RetryMax:      time.Second,
		MaxRows:       32,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func fixedExporterDependencies(now time.Time) exporterDependencies {
	return exporterDependencies{
		now: func() time.Time { return now },
		wait: func(context.Context, time.Duration) error {
			return nil
		},
	}
}

func validExportBatch(sourceID ID128, sequence uint64) CommitBatchRequest {
	owner := testID(2)
	return CommitBatchRequest{
		SourceID: sourceID,
		Sequence: sequence,
		Entities: []Entity{{ID: owner, Kind: "site", Name: "test", ValidFrom: 1}},
		Series: []SeriesDefinition{{
			ID:               1,
			OwnerEntity:      &owner,
			Name:             "grid_w",
			PhysicalQuantity: "power",
			CanonicalUnit:    "W",
			Semantics:        SeriesGauge,
		}},
		Points: []Point{{SeriesID: 1, ValidTime: 1, ValidTimeEnd: 1, KnowledgeTime: 1, ChangeTime: 1, Value: 12}},
	}
}

func testID(value byte) ID128 {
	var id ID128
	id[len(id)-1] = value
	return id
}

func retryableTransportError(err error) error {
	return &ClientError{Operation: "test", Kind: FailureTransport, CanRetry: true, Err: err}
}

func healthWithDurable(sourceID ID128, durable *uint64) func(context.Context, uint64) (HealthResponse, error) {
	return healthWithWatermarks(sourceID, durable, durable)
}

func healthWithWatermarks(
	sourceID ID128,
	accepted, durable *uint64,
) func(context.Context, uint64) (HealthResponse, error) {
	return func(_ context.Context, nonce uint64) (HealthResponse, error) {
		return HealthResponse{
			Nonce:                   nonce,
			SourceID:                sourceID,
			Status:                  HealthHealthy,
			AcceptedThroughSequence: accepted,
			DurableThroughSequence:  durable,
		}, nil
	}
}

func durableFlushAck(sourceID ID128, sequence uint64) Ack {
	return Ack{
		Kind: AckFlush, SourceID: sourceID, Sequence: sequence,
		AcceptedThroughSequence: &sequence, DurableThroughSequence: &sequence, Durable: true,
	}
}

func unexpectedCommit(t *testing.T) func(context.Context, PreparedCommit) (DurableCommitResult, error) {
	t.Helper()
	return func(context.Context, PreparedCommit) (DurableCommitResult, error) {
		t.Fatal("unexpected commit")
		return DurableCommitResult{}, nil
	}
}

func oneSessionConnector(t *testing.T, session exportSession) func(context.Context, ClientConfig) (exportSession, error) {
	t.Helper()
	used := false
	return func(context.Context, ClientConfig) (exportSession, error) {
		if used {
			t.Fatal("unexpected reconnect")
		}
		used = true
		return session, nil
	}
}

func sessionSequenceConnector(
	t *testing.T,
	sessions ...exportSession,
) func(context.Context, ClientConfig) (exportSession, error) {
	t.Helper()
	next := 0
	return func(context.Context, ClientConfig) (exportSession, error) {
		if next >= len(sessions) {
			t.Fatal("unexpected extra connection")
		}
		session := sessions[next]
		next++
		return session, nil
	}
}
