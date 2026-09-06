package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const optimizerTagEnv = "FTW_OPTIMIZER_IMAGE_TAG"

func (s *server) validateOptimizerPinLayout() error {
	image, ok, err := serviceImageFromComposeFiles(s.hostComposeFiles(), optimizerServiceName)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("service %s has no host Compose image", optimizerServiceName)
	}
	if _, ok := composeImageRepositoryForTag(image, optimizerTagEnv); !ok {
		return fmt.Errorf("service %s must use ${%s} in its image to preserve an update; change the host Compose image before updating", optimizerServiceName, optimizerTagEnv)
	}
	_, err = readEnvFile(filepath.Dir(s.composeFile))
	return err
}

// persistOptimizerPin runs only after optimizer health succeeds. The updater's
// project mount is read-only, so a short helper uses the current updater image
// to write the pin, preserving the operator's other keys, owner and mode.
// Unlike best-effort updater replacement, this runs synchronously and verifies
// readback before the component operation may report done.
func (s *server) persistOptimizerPin(target string) error {
	if err := s.validateOptimizerPinLayout(); err != nil {
		return err
	}
	projectDir := filepath.Dir(s.composeFile)
	existing, err := readEnvFile(projectDir)
	if err != nil {
		return err
	}
	content := mergeEnvFile(existing, map[string]string{optimizerTagEnv: target})
	service, err := s.updaterServiceName()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if s.imageID == nil {
		return fmt.Errorf("cannot identify updater image for persisting optimizer pin")
	}
	helperImage, err := s.imageID(ctx, service)
	if err != nil {
		return err
	}
	envPath := filepath.Join(projectDir, ".env")
	tmpPath := envPath + ".ftw-optimizer-pin-tmp"
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(existing)))
	// Reject a file changed since read/merge; never overwrite a new operator
	// setting with an old copy. Payload and paths remain shell-quoted.
	check := fmt.Sprintf("if [ -e %s ]; then test \"$(sha256sum %s | cut -d ' ' -f 1)\" = %s; else test %s = %s; fi", shellQuote(envPath), shellQuote(envPath), shellQuote(sum), shellQuote(sum), shellQuote(fmt.Sprintf("%x", sha256.Sum256(nil))))
	script := "set -eu; " + check + "; test ! -L " + shellQuote(tmpPath) + "; " + envTempWriteScript(envPath, tmpPath, content) + " && { " + check + "; } && mv " + shellQuote(tmpPath) + " " + shellQuote(envPath)
	args := []string{"run", "--rm", "--network", "none", "--user", "0:0", "-v", projectDir + ":" + projectDir, "--entrypoint", "sh", helperImage, "-c", script}
	if err := s.runner(ctx, nil, args...); err != nil {
		return fmt.Errorf("write optimizer image pin: %w", err)
	}
	got, err := os.ReadFile(envPath)
	if err != nil {
		return fmt.Errorf("read optimizer image pin: %w", err)
	}
	if string(got) != content {
		return fmt.Errorf("optimizer image pin did not persist; retry after repairing %s", envPath)
	}
	return nil
}

func (s *server) saveOptimizerPin(target string) error {
	if s.optimizerPin == nil {
		return fmt.Errorf("optimizer image pin persistence is unavailable")
	}
	if target == "" || strings.ContainsAny(target, "\r\n") {
		return fmt.Errorf("invalid optimizer image pin")
	}
	return s.optimizerPin(target)
}
