//go:build unix

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/ftwdbshadow"
	"github.com/srcfl/ftw/go/internal/nova"
	"github.com/srcfl/ftw/go/internal/state"
)

// TestRustSidecarInterop is an opt-in cross-repository gate. It proves that
// the real Go source, mapper, codec, client, and exporter can commit through
// the real Rust sidecar without adding a build-time dependency to FTW.
func TestRustSidecarInterop(t *testing.T) {
	sidecarPath := os.Getenv("FTWDB_SHADOW_BIN")
	if sidecarPath == "" {
		t.Skip("set FTWDB_SHADOW_BIN to the ftwdb-shadow binary")
	}
	var err error
	sidecarPath, err = filepath.Abs(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	if info, statErr := os.Stat(sidecarPath); statErr != nil || !info.Mode().IsRegular() {
		t.Fatalf("FTWDB_SHADOW_BIN is not a regular file: %v", statErr)
	}

	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(directory, "state.db")
	identityPath := filepath.Join(directory, "nova.key")
	storePath := filepath.Join(directory, "shadow-store")
	socketPath := filepath.Join(directory, "run", "shadow.sock")

	core, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	identity, err := nova.LoadOrCreateIdentity(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	rowTime := time.Now().Add(-2 * time.Second).Truncate(time.Millisecond)
	if err := core.RecordHistory(state.HistoryPoint{
		TsMs: rowTime.UnixMilli(), GridW: 800, PVW: -500, BatW: 100,
		LoadW: 1_200, BatSoC: 0.5, JSON: `{"targets":{"battery":100}}`,
	}); err != nil {
		t.Fatal(err)
	}

	var sidecarLog bytes.Buffer
	command := exec.Command(sidecarPath, storePath, socketPath)
	command.Stderr = &sidecarLog
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	waitResult := make(chan error, 1)
	go func() { waitResult <- command.Wait() }()
	stopSidecar := func(signal os.Signal) error {
		if waited {
			return nil
		}
		if signalErr := command.Process.Signal(signal); signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
			return signalErr
		}
		select {
		case waitErr := <-waitResult:
			waited = true
			return waitErr
		case <-time.After(5 * time.Second):
			_ = command.Process.Kill()
			waitErr := <-waitResult
			waited = true
			return fmt.Errorf("sidecar did not stop after signal; forced kill result: %v", waitErr)
		}
	}
	t.Cleanup(func() {
		if !waited {
			_ = stopSidecar(os.Kill)
		}
	})

	startDeadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Lstat(socketPath); err == nil {
			break
		}
		select {
		case waitErr := <-waitResult:
			waited = true
			t.Fatalf("sidecar stopped before its socket appeared: %v: %s", waitErr, sidecarLog.String())
		default:
		}
		if time.Now().After(startDeadline) {
			_ = stopSidecar(syscall.SIGTERM)
			t.Fatalf("sidecar socket did not appear: %s", sidecarLog.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	exportCtx, cancelExport := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelExport()
	if err := run(exportCtx, []string{
		"-state", statePath,
		"-identity-key", identityPath,
		"-socket", socketPath,
		"-stream", "fast",
		"-once",
		"-backfill", "1h",
		"-settle-delay", "1ms",
		"-query-timeout", "2s",
		"-io-timeout", "2s",
	}); err != nil {
		_ = stopSidecar(syscall.SIGTERM)
		t.Fatalf("real exporter failed: %v: %s", err, sidecarLog.String())
	}

	sourceID := deriveSourceID(identity.PublicKeyHex(), streamFast)
	healthCtx, cancelHealth := context.WithTimeout(context.Background(), 3*time.Second)
	client, _, err := ftwdbshadow.Connect(healthCtx, ftwdbshadow.ClientConfig{
		SocketPath: socketPath, SourceID: sourceID, NodeID: "interop-check",
		ClientVersion: "interop-test", IOTimeout: 2 * time.Second,
	})
	if err != nil {
		cancelHealth()
		_ = stopSidecar(syscall.SIGTERM)
		t.Fatalf("connect for durable proof: %v: %s", err, sidecarLog.String())
	}
	health, err := client.Health(healthCtx, 42)
	_ = client.Close()
	cancelHealth()
	if err != nil {
		_ = stopSidecar(syscall.SIGTERM)
		t.Fatalf("read durable proof: %v: %s", err, sidecarLog.String())
	}
	if health.Status != ftwdbshadow.HealthHealthy || health.DurableThroughSequence == nil ||
		*health.DurableThroughSequence < uint64(rowTime.UnixMilli()) {
		_ = stopSidecar(syscall.SIGTERM)
		t.Fatalf("durable health does not cover source row: %#v", health)
	}

	if err := stopSidecar(syscall.SIGTERM); err != nil {
		t.Fatalf("stop sidecar: %v: %s", err, sidecarLog.String())
	}
	if !strings.Contains(sidecarLog.String(), "client_errors=0") {
		t.Fatalf("sidecar did not report a clean session shutdown: %s", sidecarLog.String())
	}
}
