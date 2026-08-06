// ftw-shadow-export copies settled FTW state into the optional local FTWDB
// sidecar. It never joins the Core write path and never writes state.db.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/srcfl/ftw/go/internal/ftwdbshadow"
	"github.com/srcfl/ftw/go/internal/nova"
	"github.com/srcfl/ftw/go/internal/state"
)

const sourceIDDomain = "ftwdb-shadow-source-v1\x00"

const minimumFastPoll = 10 * time.Second

var Version = "dev"

type streamKind string

const (
	streamFast   streamKind = "fast"
	streamLedger streamKind = "ledger"
	streamAll    streamKind = "all"
)

type commandConfig struct {
	statePath    string
	socketPath   string
	identityPath string
	stream       streamKind
	dumpDir      string
	backfill     time.Duration
	poll         time.Duration
	ledgerPoll   time.Duration
	settleDelay  time.Duration
	queryTimeout time.Duration
	ioTimeout    time.Duration
	retryInitial time.Duration
	retryMax     time.Duration
	maxRows      int
	once         bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ftw-shadow-export:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	config, err := parseFlags(args)
	if err != nil {
		return err
	}
	identity, err := nova.LoadExistingIdentity(config.identityPath)
	if err != nil {
		return fmt.Errorf("load existing FTW identity: %w", err)
	}
	publicKey := identity.PublicKeyHex()
	nodeID := "ftw-" + publicKey[:16]
	clientVersion := "ftw-shadow-export/" + Version
	if len(clientVersion) > 64 {
		return errors.New("FTW shadow exporter version exceeds the protocol limit")
	}

	source, err := state.OpenShadowSource(config.statePath)
	if err != nil {
		return err
	}
	defer source.Close()

	fastID := deriveSourceID(publicKey, streamFast)
	ledgerID := deriveSourceID(publicKey, streamLedger)
	fast := &stateBatchSource{
		source: source,
		mapOptions: ftwdbshadow.MapOptions{
			SourceID: fastID,
			SiteKey:  publicKey,
		},
		queryTimeout: config.queryTimeout,
	}
	ledger := &stateBatchSource{
		source: source,
		mapOptions: ftwdbshadow.MapOptions{
			SourceID: ledgerID,
			SiteKey:  publicKey,
		},
		queryTimeout: config.queryTimeout,
		ledgerOnly:   true,
		ledgerPoll:   config.ledgerPoll,
	}
	base := ftwdbshadow.ExporterConfig{
		SocketPath:    config.socketPath,
		NodeID:        nodeID,
		ClientVersion: clientVersion,
		Backfill:      config.backfill,
		PollInterval:  config.poll,
		SettleDelay:   config.settleDelay,
		IOTimeout:     config.ioTimeout,
		RetryInitial:  config.retryInitial,
		RetryMax:      config.retryMax,
		MaxRows:       config.maxRows,
		Logger:        slog.Default(),
		Once:          true,
	}

	if config.dumpDir != "" {
		return dumpStreams(ctx, config.dumpDir, config.stream, fast, ledger, base, fastID, ledgerID)
	}
	return runStreams(ctx, config, fast, ledger, base, fastID, ledgerID)
}

type stateBatchSource struct {
	source       shadowReader
	mapOptions   ftwdbshadow.MapOptions
	queryTimeout time.Duration
	ledgerOnly   bool
	ledgerPoll   time.Duration
}

type shadowReader interface {
	ReadAfter(context.Context, state.ShadowReadOptions) (state.ShadowExportBatch, error)
}

func (source *stateBatchSource) ReadBatch(
	ctx context.Context,
	request ftwdbshadow.PullRequest,
) (ftwdbshadow.CommitBatchRequest, bool, error) {
	if request.AfterSequence > math.MaxInt64 {
		return ftwdbshadow.CommitBatchRequest{}, false, errors.New("FTWDB shadow cursor exceeds source milliseconds")
	}
	maxRows := request.MaxRows
	for {
		batch, err := source.source.ReadAfter(ctx, state.ShadowReadOptions{
			AfterMS:            int64(request.AfterSequence),
			Now:                request.Now,
			SettleDelay:        request.SettleDelay,
			MaxRows:            maxRows,
			QueryTimeout:       source.queryTimeout,
			EnergyLedgerOnly:   source.ledgerOnly,
			LedgerPollInterval: source.ledgerPoll,
		})
		if err != nil {
			if errors.Is(err, state.ErrShadowBatchTooLarge) &&
				!errors.Is(err, state.ErrShadowMillisecondTooLarge) && maxRows > 1 {
				maxRows = max(1, maxRows/2)
				slog.Warn("FTWDB shadow source batch exceeded memory limit; retrying a smaller read",
					"next_max_rows", maxRows, "error", err)
				continue
			}
			return ftwdbshadow.CommitBatchRequest{}, false, err
		}
		if batch.RowCount() == 0 {
			return ftwdbshadow.CommitBatchRequest{}, false, nil
		}
		mapped, err := ftwdbshadow.MapShadowBatch(source.mapOptions, batch)
		if err == nil {
			return mapped, true, nil
		}
		if !errors.Is(err, ftwdbshadow.ErrMappedBatchTooLarge) || maxRows == 1 {
			return ftwdbshadow.CommitBatchRequest{}, false, err
		}
		maxRows = max(1, maxRows/2)
		slog.Warn("FTWDB shadow mapped batch exceeded wire limits; retrying a smaller read",
			"rows", batch.RowCount(), "next_max_rows", maxRows)
	}
}

func runStreams(
	ctx context.Context,
	config commandConfig,
	fast, ledger ftwdbshadow.BatchSource,
	base ftwdbshadow.ExporterConfig,
	fastID, ledgerID ftwdbshadow.ID128,
) error {
	nextLedger := time.Time{}
	for {
		if config.stream == streamFast || config.stream == streamAll {
			current := base
			current.SourceID = fastID
			if err := ftwdbshadow.RunExporter(ctx, fast, current); err != nil {
				return fmt.Errorf("export fast FTW shadow stream: %w", err)
			}
		}
		now := time.Now()
		if (config.stream == streamLedger || config.stream == streamAll) &&
			(nextLedger.IsZero() || !now.Before(nextLedger)) {
			current := base
			current.SourceID = ledgerID
			current.PollInterval = config.ledgerPoll
			if err := ftwdbshadow.RunExporter(ctx, ledger, current); err != nil {
				return fmt.Errorf("export energy ledger shadow stream: %w", err)
			}
			nextLedger = time.Now().Add(config.ledgerPoll)
		}
		if config.once || ctx.Err() != nil {
			return nil
		}
		delay := config.poll
		if config.stream == streamLedger {
			delay = config.ledgerPoll
		}
		if err := waitContext(ctx, delay); err != nil {
			return nil
		}
	}
}

func dumpStreams(
	ctx context.Context,
	directory string,
	selected streamKind,
	fast, ledger ftwdbshadow.BatchSource,
	base ftwdbshadow.ExporterConfig,
	fastID, ledgerID ftwdbshadow.ID128,
) error {
	if err := secureDumpDirectory(directory); err != nil {
		return err
	}
	dump := func(name string, source ftwdbshadow.BatchSource, sourceID ftwdbshadow.ID128) error {
		current := base
		current.SourceID = sourceID
		current.Once = false
		count, err := ftwdbshadow.DumpExportFrames(ctx, source, current, func(prepared ftwdbshadow.PreparedCommit) error {
			return writeDumpFrame(directory, name, prepared)
		})
		if err != nil {
			return err
		}
		slog.Info("FTWDB shadow frames dumped", "stream", name, "frames", count, "directory", directory)
		return nil
	}
	if selected == streamFast || selected == streamAll {
		if err := dump(string(streamFast), fast, fastID); err != nil {
			return fmt.Errorf("dump fast FTW shadow stream: %w", err)
		}
	}
	if selected == streamLedger || selected == streamAll {
		if err := dump(string(streamLedger), ledger, ledgerID); err != nil {
			return fmt.Errorf("dump energy ledger shadow stream: %w", err)
		}
	}
	return nil
}

func writeDumpFrame(directory, stream string, prepared ftwdbshadow.PreparedCommit) error {
	name := fmt.Sprintf("%s-%020d-%s.hex", stream, prepared.Sequence(), prepared.CommitID())
	path := filepath.Join(directory, name)
	encoded := make([]byte, hex.EncodedLen(len(prepared.Bytes()))+1)
	hex.Encode(encoded, prepared.Bytes())
	encoded[len(encoded)-1] = '\n'
	file, err := os.CreateTemp(directory, ".ftwdb-shadow-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	published := false
	defer func() {
		_ = file.Close()
		if !published {
			_ = os.Remove(temporary)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if err := writeAll(file, encoded); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(temporary, path); errors.Is(err, os.ErrExist) {
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if bytes.Equal(existing, encoded) {
			if removeErr := os.Remove(temporary); removeErr != nil {
				return removeErr
			}
			published = true
			return nil
		}
		return fmt.Errorf("refuse to replace different frame %s", path)
	} else if err != nil {
		return err
	}
	if err := syncDirectory(directory); err != nil {
		return err
	}
	if err := os.Remove(temporary); err != nil {
		return err
	}
	published = true
	return nil
}

func secureDumpDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("dump path %s must not be a symlink", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("dump path %s is not a directory", path)
	}
	if !ownedByCurrentUser(info) {
		return fmt.Errorf("dump directory %s must be owned by the current user", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("dump directory %s must not grant group or other access", path)
	}
	return nil
}

func deriveSourceID(publicKey string, stream streamKind) ftwdbshadow.ID128 {
	digest := sha256.Sum256([]byte(sourceIDDomain + string(stream) + "\x00" + publicKey))
	var id ftwdbshadow.ID128
	copy(id[:], digest[:len(id)])
	return id
}

func parseFlags(args []string) (commandConfig, error) {
	flags := flag.NewFlagSet("ftw-shadow-export", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var config commandConfig
	var selected string
	flags.StringVar(&config.statePath, "state", "state.db", "path to the existing FTW state.db")
	flags.StringVar(&config.socketPath, "socket", "/run/ftwdb-shadow/ftwdb-shadow.sock", "path to the FTWDB Unix socket")
	flags.StringVar(&config.identityPath, "identity-key", "", "path to the existing nova.key (default: beside state.db)")
	flags.StringVar(&selected, "stream", string(streamAll), "stream to export: fast, ledger, or all")
	flags.StringVar(&config.dumpDir, "dump-dir", "", "write exact request frames here without opening the sidecar socket")
	flags.DurationVar(&config.backfill, "backfill", 24*time.Hour, "history window used when the sidecar has no durable cursor")
	flags.DurationVar(&config.poll, "poll", 30*time.Second, "fast stream poll interval")
	flags.DurationVar(&config.ledgerPoll, "ledger-poll", state.MinimumShadowLedgerPollInterval, "energy ledger poll interval")
	flags.DurationVar(&config.settleDelay, "settle-delay", state.DefaultShadowSettleDelay, "age required before a source row can export")
	flags.DurationVar(&config.queryTimeout, "query-timeout", state.DefaultShadowQueryTimeout, "maximum time for one read-only source query")
	flags.DurationVar(&config.ioTimeout, "io-timeout", 5*time.Second, "absolute deadline for one sidecar operation")
	flags.DurationVar(&config.retryInitial, "retry-initial", 250*time.Millisecond, "initial sidecar reconnect delay")
	flags.DurationVar(&config.retryMax, "retry-max", 10*time.Second, "maximum sidecar reconnect delay")
	flags.IntVar(&config.maxRows, "batch", state.DefaultShadowMaxRows, "maximum source rows per commit")
	flags.BoolVar(&config.once, "once", false, "drain one settled view and exit")
	if err := flags.Parse(args); err != nil {
		return commandConfig{}, err
	}
	if flags.NArg() != 0 {
		return commandConfig{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	config.stream = streamKind(selected)
	if config.stream != streamFast && config.stream != streamLedger && config.stream != streamAll {
		return commandConfig{}, fmt.Errorf("invalid stream %q: want fast, ledger, or all", selected)
	}
	if config.statePath == "" {
		return commandConfig{}, errors.New("state path is required")
	}
	absState, err := filepath.Abs(config.statePath)
	if err != nil {
		return commandConfig{}, err
	}
	config.statePath = absState
	if config.identityPath == "" {
		config.identityPath = filepath.Join(filepath.Dir(absState), "nova.key")
	} else {
		config.identityPath, err = filepath.Abs(config.identityPath)
		if err != nil {
			return commandConfig{}, err
		}
	}
	if config.backfill < 0 || config.poll <= 0 || config.settleDelay < 0 || config.queryTimeout <= 0 ||
		config.ioTimeout <= 0 || config.retryInitial <= 0 || config.retryMax < config.retryInitial {
		return commandConfig{}, errors.New("durations must be positive, except settle-delay and backfill may be zero")
	}
	if config.poll < minimumFastPoll {
		return commandConfig{}, fmt.Errorf("poll must be at least %s to cap durable write frequency", minimumFastPoll)
	}
	if config.ledgerPoll < state.MinimumShadowLedgerPollInterval {
		return commandConfig{}, fmt.Errorf("ledger-poll must be at least %s", state.MinimumShadowLedgerPollInterval)
	}
	if config.maxRows < 1 || config.maxRows > state.MaxShadowExportRows {
		return commandConfig{}, fmt.Errorf("batch must be in [1,%d]", state.MaxShadowExportRows)
	}
	if config.dumpDir != "" {
		config.dumpDir, err = filepath.Abs(config.dumpDir)
		if err != nil {
			return commandConfig{}, err
		}
	}
	return config, nil
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func writeAll(writer io.Writer, value []byte) error {
	for len(value) > 0 {
		written, err := writer.Write(value)
		value = value[written:]
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}
