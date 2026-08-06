package ftwdbshadow

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

const exportCommitDomain = "ftwdb-shadow-export-commit-v1\x00"

// PullRequest asks for one fully materialized source batch. AfterSequence is
// exclusive. ReadBatch must close every database transaction before it
// returns; the exporter may perform socket I/O as soon as the call completes.
type PullRequest struct {
	AfterSequence uint64
	Now           time.Time
	SettleDelay   time.Duration
	MaxRows       int
}

// BatchSource reads authoritative FTW state without changing it. A false
// return means no settled rows exist after the requested cursor.
type BatchSource interface {
	ReadBatch(context.Context, PullRequest) (CommitBatchRequest, bool, error)
}

// ExporterConfig controls one source-bound exporter. The exporter has no disk
// cursor: it resumes from the sidecar's durable watermark and keeps at most one
// exact encoded commit frame in memory while an acknowledgement is uncertain.
type ExporterConfig struct {
	SocketPath    string
	SourceID      ID128
	NodeID        string
	ClientVersion string
	Capabilities  uint64

	Backfill     time.Duration
	PollInterval time.Duration
	SettleDelay  time.Duration
	IOTimeout    time.Duration
	RetryInitial time.Duration
	RetryMax     time.Duration
	MaxRows      int
	Once         bool
	Logger       *slog.Logger
}

// PrepareExportCommit assigns a content-derived commit ID and encodes the
// batch once. Repeating it for the same canonical batch returns the same frame.
func PrepareExportCommit(batch CommitBatchRequest) (PreparedCommit, error) {
	batch.CommitID = ID128{}
	canonical, err := Encode(batch)
	if err != nil {
		return PreparedCommit{}, fmt.Errorf("encode FTWDB shadow batch for commit ID: %w", err)
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(exportCommitDomain))
	_, _ = digest.Write(canonical)
	sum := digest.Sum(nil)
	copy(batch.CommitID[:], sum[:len(batch.CommitID)])
	return PrepareCommit(batch)
}

// RunExporter copies settled state until ctx is cancelled. Cancellation is a
// clean stop. Source, contract, and non-retryable sidecar errors stop the run.
func RunExporter(ctx context.Context, source BatchSource, config ExporterConfig) error {
	return runExporter(ctx, source, config, defaultExporterDependencies())
}

// DumpExportFrames reads one fixed settled view and sends each exact prepared
// frame to sink. It never opens the sidecar socket or writes to the source.
func DumpExportFrames(
	ctx context.Context,
	source BatchSource,
	config ExporterConfig,
	sink func(PreparedCommit) error,
) (int, error) {
	if source == nil {
		return 0, errors.New("FTWDB shadow source is required")
	}
	if sink == nil {
		return 0, errors.New("FTWDB shadow frame sink is required")
	}
	if err := validateExporterConfig(config, false); err != nil {
		return 0, err
	}
	now := time.Now()
	cursor := backfillCursor(now, config.Backfill)
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return count, nil
		}
		batch, ok, err := source.ReadBatch(ctx, PullRequest{
			AfterSequence: cursor,
			Now:           now,
			SettleDelay:   config.SettleDelay,
			MaxRows:       config.MaxRows,
		})
		if err != nil {
			if ctx.Err() != nil {
				return count, nil
			}
			return count, fmt.Errorf("read FTWDB shadow source after %d: %w", cursor, err)
		}
		if !ok {
			return count, nil
		}
		prepared, err := preparePulledBatch(batch, config.SourceID, cursor)
		if err != nil {
			return count, err
		}
		if err := sink(prepared); err != nil {
			return count, fmt.Errorf("write FTWDB shadow frame %d: %w", prepared.Sequence(), err)
		}
		cursor = prepared.Sequence()
		count++
		if config.Once {
			return count, nil
		}
	}
}

type exportSession interface {
	Health(context.Context, uint64) (HealthResponse, error)
	CommitDurable(context.Context, PreparedCommit) (DurableCommitResult, error)
	Flush(context.Context, uint64) (Ack, error)
	Close() error
}

type exporterDependencies struct {
	connect func(context.Context, ClientConfig) (exportSession, error)
	now     func() time.Time
	wait    func(context.Context, time.Duration) error
}

func defaultExporterDependencies() exporterDependencies {
	return exporterDependencies{
		connect: func(ctx context.Context, config ClientConfig) (exportSession, error) {
			client, _, err := Connect(ctx, config)
			return client, err
		},
		now: time.Now,
		wait: func(ctx context.Context, delay time.Duration) error {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
}

func runExporter(
	ctx context.Context,
	source BatchSource,
	config ExporterConfig,
	deps exporterDependencies,
) error {
	if source == nil {
		return errors.New("FTWDB shadow source is required")
	}
	if err := validateExporterConfig(config, true); err != nil {
		return err
	}
	if deps.connect == nil || deps.now == nil || deps.wait == nil {
		return errors.New("FTWDB shadow exporter dependencies are incomplete")
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clientConfig := ClientConfig{
		SocketPath:    config.SocketPath,
		SourceID:      config.SourceID,
		NodeID:        config.NodeID,
		ClientVersion: config.ClientVersion,
		Capabilities:  config.Capabilities,
		IOTimeout:     config.IOTimeout,
	}

	var pending *PreparedCommit
	var provenDurable *uint64
	retryDelay := config.RetryInitial
	initialCursor := backfillCursor(deps.now(), config.Backfill)
	var fixedNow time.Time
	if config.Once {
		fixedNow = deps.now()
	}

	for {
		if ctx.Err() != nil {
			return nil
		}
		session, err := deps.connect(ctx, clientConfig)
		if err != nil {
			if retryErr := handleRetry(ctx, deps, logger, "connect", err, &retryDelay, config.RetryMax); retryErr != nil {
				return retryErr
			}
			continue
		}

		resume, err := resumeCursor(ctx, session, deps.now(), initialCursor, pending, provenDurable)
		if err != nil {
			_ = session.Close()
			if retryErr := handleRetry(ctx, deps, logger, "health", err, &retryDelay, config.RetryMax); retryErr != nil {
				return retryErr
			}
			continue
		}
		cursor := resume.cursor
		if pending != nil && cursor >= pending.Sequence() {
			pending = nil
		}
		flushAccepted := resume.flushAccepted

		for {
			if ctx.Err() != nil {
				_ = session.Close()
				return nil
			}
			if pending != nil {
				result, commitErr := session.CommitDurable(ctx, *pending)
				if commitErr != nil {
					_ = session.Close()
					if retryErr := handleRetry(ctx, deps, logger, "commit", commitErr, &retryDelay, config.RetryMax); retryErr != nil {
						return retryErr
					}
					break
				}
				if result.DurableThrough < pending.Sequence() {
					_ = session.Close()
					return fmt.Errorf(
						"FTWDB shadow durable watermark %d does not cover commit %d",
						result.DurableThrough,
						pending.Sequence(),
					)
				}
				cursor = result.DurableThrough
				value := cursor
				provenDurable = &value
				logger.Debug("FTWDB shadow batch durable", "sequence", pending.Sequence())
				pending = nil
				retryDelay = config.RetryInitial
			}
			if pending == nil && flushAccepted != nil {
				accepted := *flushAccepted
				ack, flushErr := session.Flush(ctx, accepted)
				if flushErr != nil {
					_ = session.Close()
					if retryErr := handleRetry(ctx, deps, logger, "flush accepted", flushErr,
						&retryDelay, config.RetryMax); retryErr != nil {
						return retryErr
					}
					break
				}
				if ack.DurableThroughSequence == nil || *ack.DurableThroughSequence < accepted {
					_ = session.Close()
					return fmt.Errorf(
						"FTWDB shadow flush did not make accepted sequence %d durable",
						accepted,
					)
				}
				cursor = *ack.DurableThroughSequence
				value := cursor
				provenDurable = &value
				logger.Warn("made a previously accepted FTWDB shadow batch durable",
					"accepted_through", accepted, "durable_through", cursor)
				flushAccepted = nil
				retryDelay = config.RetryInitial
				continue
			}

			now := fixedNow
			if !config.Once {
				now = deps.now()
			}
			batch, ok, readErr := source.ReadBatch(ctx, PullRequest{
				AfterSequence: cursor,
				Now:           now,
				SettleDelay:   config.SettleDelay,
				MaxRows:       config.MaxRows,
			})
			if readErr != nil {
				_ = session.Close()
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("read FTWDB shadow source after %d: %w", cursor, readErr)
			}
			if ok {
				prepared, prepareErr := preparePulledBatch(batch, config.SourceID, cursor)
				if prepareErr != nil {
					_ = session.Close()
					return prepareErr
				}
				pending = &prepared
				continue
			}
			if config.Once {
				_ = session.Close()
				return nil
			}
			retryDelay = config.RetryInitial
			if err := deps.wait(ctx, config.PollInterval); err != nil {
				_ = session.Close()
				return nil
			}
		}
	}
}

type exporterResume struct {
	cursor        uint64
	flushAccepted *uint64
}

func resumeCursor(
	ctx context.Context,
	session exportSession,
	now time.Time,
	initialCursor uint64,
	pending *PreparedCommit,
	provenDurable *uint64,
) (exporterResume, error) {
	health, err := session.Health(ctx, uint64(now.UnixNano()))
	if err != nil {
		return exporterResume{}, err
	}
	if health.Status != HealthHealthy {
		return exporterResume{}, &ClientError{
			Operation: "health",
			Kind:      FailureTransport,
			CanRetry:  true,
			Err: fmt.Errorf(
				"sidecar status %d with %d queued entries",
				health.Status,
				health.QueueEntries,
			),
		}
	}
	if health.AcceptedThroughSequence != nil && health.DurableThroughSequence != nil &&
		*health.AcceptedThroughSequence < *health.DurableThroughSequence {
		return exporterResume{}, fmt.Errorf(
			"FTWDB shadow accepted cursor %d is below durable cursor %d",
			*health.AcceptedThroughSequence,
			*health.DurableThroughSequence,
		)
	}
	if provenDurable != nil {
		if health.DurableThroughSequence == nil || *health.DurableThroughSequence < *provenDurable {
			return exporterResume{}, fmt.Errorf(
				"FTWDB shadow durable watermark regressed below %d",
				*provenDurable,
			)
		}
	}
	if health.DurableThroughSequence != nil && health.AcceptedThroughSequence == nil {
		return exporterResume{}, errors.New("FTWDB shadow health has a durable cursor without an accepted cursor")
	}
	cursor := initialCursor
	if health.DurableThroughSequence != nil {
		cursor = *health.DurableThroughSequence
	}
	if health.AcceptedThroughSequence == nil ||
		(health.DurableThroughSequence != nil && *health.AcceptedThroughSequence == *health.DurableThroughSequence) {
		return exporterResume{cursor: cursor}, nil
	}
	if pending != nil {
		return exporterResume{cursor: cursor}, nil
	}
	accepted := *health.AcceptedThroughSequence
	return exporterResume{cursor: cursor, flushAccepted: &accepted}, nil
}

func preparePulledBatch(batch CommitBatchRequest, sourceID ID128, after uint64) (PreparedCommit, error) {
	if batch.SourceID.IsZero() {
		batch.SourceID = sourceID
	}
	if batch.SourceID != sourceID {
		return PreparedCommit{}, fmt.Errorf(
			"FTWDB shadow batch source %s does not match exporter source %s",
			batch.SourceID,
			sourceID,
		)
	}
	if batch.Sequence <= after {
		return PreparedCommit{}, fmt.Errorf(
			"FTWDB shadow batch sequence %d does not advance cursor %d",
			batch.Sequence,
			after,
		)
	}
	prepared, err := PrepareExportCommit(batch)
	if err != nil {
		return PreparedCommit{}, fmt.Errorf("prepare FTWDB shadow batch %d: %w", batch.Sequence, err)
	}
	return prepared, nil
}

func handleRetry(
	ctx context.Context,
	deps exporterDependencies,
	logger *slog.Logger,
	operation string,
	err error,
	delay *time.Duration,
	maximum time.Duration,
) error {
	if ctx.Err() != nil {
		return nil
	}
	if !ShouldRetry(err) {
		return fmt.Errorf("FTWDB shadow %s failed: %w", operation, err)
	}
	logger.Warn("FTWDB shadow sidecar unavailable", "operation", operation, "retry_in", *delay, "error", err)
	if waitErr := deps.wait(ctx, *delay); waitErr != nil {
		return nil
	}
	*delay = cappedDouble(*delay, maximum)
	return nil
}

func cappedDouble(value, maximum time.Duration) time.Duration {
	if value >= maximum || value > maximum/2 {
		return maximum
	}
	return value * 2
}

func backfillCursor(now time.Time, backfill time.Duration) uint64 {
	milliseconds := now.Add(-backfill).UnixMilli()
	if milliseconds <= 0 {
		return 0
	}
	return uint64(milliseconds)
}

func validateExporterConfig(config ExporterConfig, requireSocket bool) error {
	switch {
	case requireSocket && config.SocketPath == "":
		return errors.New("FTWDB shadow socket path is required")
	case config.SourceID.IsZero():
		return errors.New("FTWDB shadow source ID is required")
	case requireSocket && config.NodeID == "":
		return errors.New("FTWDB shadow node ID is required")
	case requireSocket && config.ClientVersion == "":
		return errors.New("FTWDB shadow client version is required")
	case config.Backfill < 0:
		return errors.New("FTWDB shadow backfill must not be negative")
	case config.PollInterval <= 0:
		return errors.New("FTWDB shadow poll interval must be positive")
	case config.SettleDelay < 0:
		return errors.New("FTWDB shadow settle delay must not be negative")
	case requireSocket && config.IOTimeout <= 0:
		return errors.New("FTWDB shadow I/O timeout must be positive")
	case requireSocket && config.RetryInitial <= 0:
		return errors.New("FTWDB shadow initial retry delay must be positive")
	case requireSocket && config.RetryMax < config.RetryInitial:
		return errors.New("FTWDB shadow maximum retry delay must cover the initial delay")
	case config.MaxRows <= 0 || config.MaxRows > MaxBatchPoints:
		return fmt.Errorf("FTWDB shadow max rows must be between 1 and %d", MaxBatchPoints)
	default:
		return nil
	}
}
