package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/ftwdbshadow"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestDeriveSourceIDIsStableAndSeparatesStreams(t *testing.T) {
	key := strings.Repeat("01", 64)
	fast := deriveSourceID(key, streamFast)
	if fast.IsZero() {
		t.Fatal("fast source ID is zero")
	}
	if got := deriveSourceID(key, streamFast); got != fast {
		t.Fatalf("fast source ID changed: %s != %s", got, fast)
	}
	if ledger := deriveSourceID(key, streamLedger); ledger == fast || ledger.IsZero() {
		t.Fatalf("ledger source ID %s is not distinct from %s", ledger, fast)
	}
}

func TestParseFlagsUsesWearBoundedDefaults(t *testing.T) {
	config, err := parseFlags([]string{"-state", filepath.Join(t.TempDir(), "state.db")})
	if err != nil {
		t.Fatal(err)
	}
	if config.poll < minimumFastPoll {
		t.Fatalf("poll = %s, below %s", config.poll, minimumFastPoll)
	}
	if config.ledgerPoll < minimumFastPoll {
		t.Fatalf("ledger poll = %s", config.ledgerPoll)
	}
	if config.stream != streamAll {
		t.Fatalf("stream = %q, want all", config.stream)
	}
}

func TestParseFlagsRejectsWriteHeavyPoll(t *testing.T) {
	_, err := parseFlags([]string{"-poll", "9s"})
	if err == nil || !strings.Contains(err.Error(), "cap durable write frequency") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunMissingIdentityDoesNotCreateFiles(t *testing.T) {
	directory := t.TempDir()
	before, err := directoryNames(directory)
	if err != nil {
		t.Fatal(err)
	}
	err = run(context.Background(), []string{
		"-state", filepath.Join(directory, "state.db"),
		"-identity-key", filepath.Join(directory, "missing.key"),
		"-once",
	})
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want missing identity", err)
	}
	after, err := directoryNames(directory)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(before, after) {
		t.Fatalf("directory changed: before=%v after=%v", before, after)
	}
}

func TestWriteDumpFrameIsPrivateAndIdempotent(t *testing.T) {
	directory := t.TempDir()
	prepared, err := ftwdbshadow.PrepareExportCommit(testBatch())
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDumpFrame(directory, "fast", prepared); err != nil {
		t.Fatal(err)
	}
	if err := writeDumpFrame(directory, "fast", prepared); err != nil {
		t.Fatalf("same frame was not idempotent: %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("dump contains %d files, want 1", len(entries))
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("frame mode = %#o, want 0600", info.Mode().Perm())
	}
	encoded, err := os.ReadFile(filepath.Join(directory, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if encoded[len(encoded)-1] != '\n' {
		t.Fatal("dump does not end with a newline")
	}
	frame, err := hex.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	message, err := ftwdbshadow.Decode(frame)
	if err != nil {
		t.Fatal(err)
	}
	batch, ok := message.(ftwdbshadow.CommitBatchRequest)
	if !ok || batch.Sequence != prepared.Sequence() || batch.CommitID != prepared.CommitID() {
		t.Fatalf("decoded dump = %#v", message)
	}
}

func TestWriteDumpFrameDoesNotReplaceExistingData(t *testing.T) {
	directory := t.TempDir()
	prepared, err := ftwdbshadow.PrepareExportCommit(testBatch())
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("fast-%020d-%s.hex", prepared.Sequence(), prepared.CommitID())
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte("different\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeDumpFrame(directory, "fast", prepared); err == nil {
		t.Fatal("different existing frame was replaced")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "different\n" {
		t.Fatalf("existing frame changed to %q", contents)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("dump contains %d files after conflict, want 1", len(entries))
	}
}

func TestSecureDumpDirectoryRejectsSharedMode(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := secureDumpDirectory(directory); err == nil {
		t.Fatal("shared dump directory was accepted")
	}
}

func TestSecureDumpDirectoryRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := secureDumpDirectory(link); err == nil {
		t.Fatal("symlink dump directory was accepted")
	}
}

func TestStateBatchSourceShrinksMappedBatchBeforeSocketIO(t *testing.T) {
	var limits []int
	reader := shadowReaderFunc(func(_ context.Context, options state.ShadowReadOptions) (state.ShadowExportBatch, error) {
		limits = append(limits, options.MaxRows)
		rows := make([]state.ShadowHistoryRow, options.MaxRows)
		for index := range rows {
			value := float64(index)
			targets := make(map[string]float64, 20)
			for driver := 0; driver < 20; driver++ {
				targets[fmt.Sprintf("driver-%02d", driver)] = float64(driver)
			}
			rows[index] = state.ShadowHistoryRow{
				TsMS:       10_001 + int64(index),
				GridW:      &value,
				PVW:        &value,
				BatteryW:   &value,
				LoadW:      &value,
				BatterySoC: &value,
				Targets:    targets,
			}
		}
		return state.ShadowExportBatch{
			AfterMS:  10_000,
			CutoffMS: 10_000 + int64(len(rows)),
			History:  rows,
		}, nil
	})
	var sourceID ftwdbshadow.ID128
	sourceID[15] = 1
	source := &stateBatchSource{
		source:       reader,
		mapOptions:   ftwdbshadow.MapOptions{SourceID: sourceID, SiteKey: "stable-site"},
		queryTimeout: time.Second,
	}
	batch, ok, err := source.ReadBatch(context.Background(), ftwdbshadow.PullRequest{
		AfterSequence: 10_000,
		Now:           time.UnixMilli(20_000),
		SettleDelay:   time.Second,
		MaxRows:       1_024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || batch.Sequence != 10_512 {
		t.Fatalf("batch ok=%v sequence=%d", ok, batch.Sequence)
	}
	if !slices.Equal(limits, []int{1_024, 512}) {
		t.Fatalf("read limits = %v, want [1024 512]", limits)
	}
}

func TestStateBatchSourceShrinksSourceMemoryBound(t *testing.T) {
	var limits []int
	reader := shadowReaderFunc(func(_ context.Context, options state.ShadowReadOptions) (state.ShadowExportBatch, error) {
		limits = append(limits, options.MaxRows)
		if options.MaxRows > 512 {
			return state.ShadowExportBatch{}, fmt.Errorf("test bytes: %w", state.ErrShadowBatchTooLarge)
		}
		value := 1.0
		return state.ShadowExportBatch{
			AfterMS:  10_000,
			CutoffMS: 10_001,
			History:  []state.ShadowHistoryRow{{TsMS: 10_001, GridW: &value}},
		}, nil
	})
	var sourceID ftwdbshadow.ID128
	sourceID[15] = 1
	source := &stateBatchSource{
		source:       reader,
		mapOptions:   ftwdbshadow.MapOptions{SourceID: sourceID, SiteKey: "stable-site"},
		queryTimeout: time.Second,
	}
	_, ok, err := source.ReadBatch(context.Background(), ftwdbshadow.PullRequest{
		AfterSequence: 10_000,
		Now:           time.UnixMilli(20_000),
		SettleDelay:   time.Second,
		MaxRows:       1_024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !slices.Equal(limits, []int{1_024, 512}) {
		t.Fatalf("ok=%v read limits=%v, want true and [1024 512]", ok, limits)
	}
}

func TestStateBatchSourceDoesNotShrinkOneMillisecondGroup(t *testing.T) {
	reads := 0
	reader := shadowReaderFunc(func(context.Context, state.ShadowReadOptions) (state.ShadowExportBatch, error) {
		reads++
		return state.ShadowExportBatch{}, state.ErrShadowMillisecondTooLarge
	})
	var sourceID ftwdbshadow.ID128
	sourceID[15] = 1
	source := &stateBatchSource{
		source:       reader,
		mapOptions:   ftwdbshadow.MapOptions{SourceID: sourceID, SiteKey: "stable-site"},
		queryTimeout: time.Second,
	}
	_, _, err := source.ReadBatch(context.Background(), ftwdbshadow.PullRequest{
		AfterSequence: 10_000,
		Now:           time.UnixMilli(20_000),
		SettleDelay:   time.Second,
		MaxRows:       1_024,
	})
	if !errors.Is(err, state.ErrShadowMillisecondTooLarge) {
		t.Fatalf("error = %v", err)
	}
	if reads != 1 {
		t.Fatalf("source read %d times, want 1", reads)
	}
}

func directoryNames(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	return names, nil
}

type shadowReaderFunc func(context.Context, state.ShadowReadOptions) (state.ShadowExportBatch, error)

func (fn shadowReaderFunc) ReadAfter(
	ctx context.Context,
	options state.ShadowReadOptions,
) (state.ShadowExportBatch, error) {
	return fn(ctx, options)
}

func testBatch() ftwdbshadow.CommitBatchRequest {
	var sourceID, entityID ftwdbshadow.ID128
	sourceID[15] = 1
	entityID[15] = 2
	return ftwdbshadow.CommitBatchRequest{
		SourceID: sourceID,
		Sequence: 42,
		Entities: []ftwdbshadow.Entity{{
			ID:        entityID,
			Kind:      "site",
			Name:      "test",
			ValidFrom: 1,
		}},
		Series: []ftwdbshadow.SeriesDefinition{{
			ID:               1,
			OwnerEntity:      &entityID,
			Name:             "grid_w",
			PhysicalQuantity: "power",
			CanonicalUnit:    "W",
			Semantics:        ftwdbshadow.SeriesGauge,
		}},
		Points: []ftwdbshadow.Point{{
			SeriesID:      1,
			ValidTime:     1,
			ValidTimeEnd:  1,
			KnowledgeTime: 1,
			ChangeTime:    1,
			Value:         12,
		}},
	}
}
