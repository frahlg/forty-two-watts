package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOptimizerPinPreservesEnvMetadataAndOtherTags(t *testing.T) {
	s, _ := newTestServer(t)
	writeCompose(t, s.composeFile, composeWithUpdater)
	file := filepath.Join(filepath.Dir(s.composeFile), ".env")
	original := "# keep this\nFTW_IMAGE_TAG=v2.14.0-beta.1\nFTW_UPDATER_IMAGE_TAG=v2.14.0-beta.1\nFTW_OPTIMIZER_IMAGE_TAG=old\nSECRET=a=b=c\nexport FTW_OPTIMIZER_IMAGE_TAG=v1.4.0-beta.3\n"
	if err := os.WriteFile(file, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(file)
	calls := 0
	s.runner = func(ctx context.Context, _ []string, args ...string) error {
		calls++
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "run --rm --network none --user 0:0") || strings.Contains(joined, "docker.sock") {
			t.Fatalf("pin helper has unnecessary privileges: %v", args)
		}
		if args[len(args)-2] != "-c" {
			t.Fatal("missing shell script")
		}
		out, err := exec.CommandContext(ctx, "sh", "-c", args[len(args)-1]).CombinedOutput()
		if err != nil {
			t.Fatalf("pin script: %v %s", err, out)
		}
		return nil
	}
	if err := s.persistOptimizerPin("v1.4.0-beta.4"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(file)
	want := mergeEnvFile(original, map[string]string{optimizerTagEnv: "v1.4.0-beta.4"})
	if string(got) != want || strings.Count(string(got), optimizerTagEnv+"=") != 1 {
		t.Fatalf("merged pin = %q", got)
	}
	after, _ := os.Stat(file)
	b, a := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
	if before.Mode() != after.Mode() || b.Uid != a.Uid || b.Gid != a.Gid {
		t.Fatal("pin changed owner or mode")
	}
	if calls != 1 {
		t.Fatalf("helper calls=%d", calls)
	}
}

func TestOptimizerPinCreatesNewPrivateEnvAndChecksReadback(t *testing.T) {
	for _, write := range []bool{true, false} {
		t.Run(map[bool]string{true: "new file", false: "helper did not write"}[write], func(t *testing.T) {
			s, _ := newTestServer(t)
			writeCompose(t, s.composeFile, composeWithUpdater)
			s.runner = func(ctx context.Context, _ []string, args ...string) error {
				if !write {
					return nil
				}
				return exec.CommandContext(ctx, "sh", "-c", args[len(args)-1]).Run()
			}
			err := s.persistOptimizerPin("v1.4.0-beta.4")
			if !write {
				if err == nil {
					t.Fatal("missing readback accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			st, err := os.Stat(filepath.Join(filepath.Dir(s.composeFile), ".env"))
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode().Perm() != 0o600 {
				t.Fatalf("new env mode=%o", st.Mode().Perm())
			}
		})
	}
}

func TestOptimizerPinDoesNotOverwriteConcurrentOperatorEdit(t *testing.T) {
	s, _ := newTestServer(t)
	writeCompose(t, s.composeFile, composeWithUpdater)
	file := filepath.Join(filepath.Dir(s.composeFile), ".env")
	if err := os.WriteFile(file, []byte("SITE=before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.runner = func(ctx context.Context, _ []string, args ...string) error {
		if err := os.WriteFile(file, []byte("SITE=operator-edit\n"), 0o600); err != nil {
			return err
		}
		return exec.CommandContext(ctx, "sh", "-c", args[len(args)-1]).Run()
	}
	if err := s.persistOptimizerPin("v1.4.0-beta.4"); err == nil {
		t.Fatal("concurrent edit overwritten")
	}
	got, _ := os.ReadFile(file)
	if string(got) != "SITE=operator-edit\n" {
		t.Fatalf("operator edit lost: %q", got)
	}
}

func TestOptimizerPinDoesNotReplaceEnvWhenMetadataCopyFails(t *testing.T) {
	s, _ := newTestServer(t)
	writeCompose(t, s.composeFile, composeWithUpdater)
	file := filepath.Join(filepath.Dir(s.composeFile), ".env")
	if err := os.WriteFile(file, []byte("SITE=current\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file+".ftw-optimizer-pin-tmp", []byte("SITE=stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.runner = func(ctx context.Context, _ []string, args ...string) error {
		// A failed metadata copy must not rename a leftover temporary file.
		return exec.CommandContext(ctx, "sh", "-c", "cp() { return 1; }; "+args[len(args)-1]).Run()
	}
	if err := s.persistOptimizerPin("v1.4.0-beta.4"); err == nil {
		t.Fatal("failed copy accepted")
	}
	got, _ := os.ReadFile(file)
	if string(got) != "SITE=current\n" {
		t.Fatalf("original env replaced after failed copy: %q", got)
	}
}

func TestOptimizerUpdateAndRollbackPersistBeforeDone(t *testing.T) {
	for _, action := range []string{"update", "rollback"} {
		for _, fail := range []bool{false, true} {
			t.Run(action+map[bool]string{false: " success", true: " pin failure"}[fail], func(t *testing.T) {
				s, _ := newTestServer(t)
				healthy := false
				s.healthCheck = func(context.Context, string) error { healthy = true; return nil }
				var saved string
				s.optimizerPin = func(target string) error {
					if !healthy || s.readState().State == "done" {
						t.Fatal("pin must follow health and precede done")
					}
					saved = target
					if fail {
						return errors.New("read-only project")
					}
					return nil
				}
				if action == "update" {
					s.runComponentJob("update", "v1.4.0-beta.4", "optimizer", time.Now())
				} else {
					s.writeState(State{State: "done", Component: "optimizer", PreviousImageID: "sha256:previous"})
					s.runComponentRollback("optimizer", time.Now())
				}
				st := s.readState()
				if fail {
					if st.State != "failed" || !strings.Contains(st.Message, "image pin was not saved") {
						t.Fatalf("pin failure hidden: %+v", st)
					}
				} else if st.State != "done" {
					t.Fatalf("state=%+v", st)
				}
				if action == "update" && saved != "v1.4.0-beta.4" {
					t.Fatalf("saved=%q", saved)
				}
				if action == "rollback" && !strings.HasPrefix(saved, "ftw-rollback-") {
					t.Fatalf("rollback pin=%q", saved)
				}
			})
		}
	}
}

func TestDockerLogHidesEnvShellPayloadWithoutChangingCommand(t *testing.T) {
	args := []string{"run", "--entrypoint", "sh", "image", "-c", "echo SECRET=base64-payload"}
	logged := loggedDockerArgs(args)
	if strings.Contains(strings.Join(logged, " "), "SECRET") {
		t.Fatal("shell secret logged")
	}
	if args[5] != "echo SECRET=base64-payload" {
		t.Fatal("log redaction changed executed arguments")
	}
}

func TestOptimizerUpdateRejectsNonPersistentLayoutBeforeDocker(t *testing.T) {
	s, runner := newTestServer(t)
	writeCompose(t, s.composeFile, "services:\n  ftw-optimizer:\n    image: ghcr.io/srcfl/ftw-optimizer:latest\n")
	s.runComponentJob("update", "v1.4.0-beta.4", "optimizer", time.Now())
	st := s.readState()
	if st.State != "failed" || !strings.Contains(st.Message, "must use ${FTW_OPTIMIZER_IMAGE_TAG}") {
		t.Fatalf("unusable pin accepted: %+v", st)
	}
	if len(runner.snapshot()) != 0 {
		t.Fatal("blocked update called Docker")
	}
}

func TestOptimizerFailedHealthRestoresPreviousPersistentTag(t *testing.T) {
	s, _ := newTestServer(t)
	s.imageRef = func(context.Context, string) (string, error) {
		return canonicalOptimizerImage + ":v1.4.0-beta.3", nil
	}
	checks := 0
	s.healthCheck = func(context.Context, string) error {
		checks++
		if checks == 1 {
			return errors.New("new optimizer unhealthy")
		}
		return nil
	}
	var saved string
	s.optimizerPin = func(target string) error {
		if checks != 2 {
			t.Fatal("previous tag saved before recovery health")
		}
		saved = target
		return nil
	}
	s.runComponentJob("update", "v1.4.0-beta.4", "optimizer", time.Now())
	st := s.readState()
	if st.State != "failed" || !strings.Contains(st.Message, "previous image restored") || saved != "v1.4.0-beta.3" {
		t.Fatalf("recovery did not persist previous image: %+v saved=%q", st, saved)
	}
}
