package main

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The .env belongs to the operator. An update records two tags in it and must
// leave everything else exactly as it found it.
func TestMergeEnvFilePreservesEverythingElse(t *testing.T) {
	existing := `# Site settings — do not reformat
COMPOSE_PROJECT_NAME=myhome

# pinned by hand last winter
FTW_IMAGE_TAG=v1.10.0
export FTW_OPTIMIZER_IMAGE_TAG=v1.3.2
SOME_SECRET=a=b=c
`
	got := mergeEnvFile(existing, map[string]string{
		mainTagEnv:    "v1.13.3-beta.1",
		updaterTagEnv: "v1.13.3-beta.1",
	})

	for _, want := range []string{
		"# Site settings — do not reformat",
		"COMPOSE_PROJECT_NAME=myhome",
		"# pinned by hand last winter",
		"export FTW_OPTIMIZER_IMAGE_TAG=v1.3.2",
		"SOME_SECRET=a=b=c",
		"FTW_IMAGE_TAG=v1.13.3-beta.1",
		"FTW_UPDATER_IMAGE_TAG=v1.13.3-beta.1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("merged .env lost or missed %q\ngot:\n%s", want, got)
		}
	}
	if strings.Contains(got, "FTW_IMAGE_TAG=v1.10.0") {
		t.Errorf("stale pin survived:\n%s", got)
	}
	// Rewritten in place, not appended alongside the old line.
	if n := strings.Count(got, "\nFTW_IMAGE_TAG="); n != 1 {
		t.Errorf("FTW_IMAGE_TAG assigned %d times, want exactly 1\n%s", n, got)
	}
	if n := strings.Count(got, "FTW_UPDATER_IMAGE_TAG="); n != 1 {
		t.Errorf("FTW_UPDATER_IMAGE_TAG assigned %d times, want exactly 1\n%s", n, got)
	}
	// The rewritten pin should stay where the operator had it, under its comment.
	if strings.Index(got, "# pinned by hand last winter") > strings.Index(got, "\nFTW_IMAGE_TAG=") {
		t.Errorf("rewrite moved the key away from its comment\n%s", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Error("merged .env must end with a newline")
	}
}

func TestMergeEnvFileCreatesFromNothing(t *testing.T) {
	got := mergeEnvFile("", map[string]string{mainTagEnv: "v1.0.0", updaterTagEnv: "v1.0.0"})
	if got != "FTW_IMAGE_TAG=v1.0.0\nFTW_UPDATER_IMAGE_TAG=v1.0.0\n" {
		t.Fatalf("new .env = %q", got)
	}
	// Order is fixed so repeated updates do not reshuffle the file.
	if got != mergeEnvFile("", map[string]string{updaterTagEnv: "v1.0.0", mainTagEnv: "v1.0.0"}) {
		t.Error("output depends on map iteration order")
	}
}

func TestMergeEnvFileReplacesTheEffectiveLastAssignment(t *testing.T) {
	existing := "FTW_IMAGE_TAG=older\n# the later value wins\nexport FTW_IMAGE_TAG=old\n"
	got := mergeEnvFile(existing, map[string]string{mainTagEnv: "v2.0.0-beta.1"})
	if strings.Count(got, mainTagEnv+"=") != 1 {
		t.Fatalf("merged .env retained duplicate effective assignments:\n%s", got)
	}
	if !strings.Contains(got, "# the later value wins\nFTW_IMAGE_TAG=v2.0.0-beta.1") {
		t.Fatalf("the effective last assignment was not replaced in place:\n%s", got)
	}
}

func TestEnvPinScriptWritesAtomicallyInPlace(t *testing.T) {
	script := envPinScript("/srv/ftw", "FTW_IMAGE_TAG=v1.2.3\n")
	if !strings.Contains(script, "base64 -d") {
		t.Error("payload should be decoded, not interpolated as shell text")
	}
	// Temp file beside the target: a rename across filesystems is not atomic.
	if !strings.Contains(script, "/srv/ftw/.env.ftw-update-tmp") || !strings.Contains(script, "mv") {
		t.Errorf("expected an atomic write next to the target\n%s", script)
	}
	if !strings.Contains(script, "'/srv/ftw/.env'") {
		t.Errorf("target path should be quoted\n%s", script)
	}
	// A failed write must say so rather than pass silently.
	if !strings.Contains(script, ">&2") {
		t.Errorf("failure should reach stderr\n%s", script)
	}

	start := strings.Index(script, "printf %s '") + len("printf %s '")
	end := strings.Index(script[start:], "'") + start
	decoded, err := base64.StdEncoding.DecodeString(script[start:end])
	if err != nil {
		t.Fatalf("payload does not decode: %v", err)
	}
	if string(decoded) != "FTW_IMAGE_TAG=v1.2.3\n" {
		t.Fatalf("payload = %q", decoded)
	}
}

func TestEnvPinScriptPreservesExistingOwnerAndMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("FTW_IMAGE_TAG=old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-c", envPinScript(dir, "FTW_IMAGE_TAG=new\n")).CombinedOutput(); err != nil {
		t.Fatalf("run env pin: %v\n%s", err, out)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("mode = %v, want %v", after.Mode().Perm(), before.Mode().Perm())
	}
	beforeStat, beforeOK := before.Sys().(*syscall.Stat_t)
	afterStat, afterOK := after.Sys().(*syscall.Stat_t)
	if !beforeOK || !afterOK || beforeStat.Uid != afterStat.Uid || beforeStat.Gid != afterStat.Gid {
		t.Fatalf("owner changed: before=%v after=%v", before.Sys(), after.Sys())
	}
}

func TestEnvPinScriptQuotesApostropheInProjectPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Fred's FTW")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "FTW_IMAGE_TAG=v2.0.0-beta.1\n"
	if out, err := exec.Command("sh", "-c", envPinScript(dir, content)).CombinedOutput(); err != nil {
		t.Fatalf("run env pin in apostrophe path: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("persisted .env = %q, want %q", got, content)
	}
}

// A quote or newline in a tag must not be able to escape the helper's sh -c.
func TestEnvPinScriptIsInertToShellMetacharacters(t *testing.T) {
	script := envPinScript("/srv/ftw", "FTW_IMAGE_TAG='; rm -rf / #\n")
	if strings.Contains(script, "rm -rf") {
		t.Fatalf("payload leaked into the script text:\n%s", script)
	}
}

func TestReadEnvFile(t *testing.T) {
	dir := t.TempDir()
	if got, err := readEnvFile(dir); err != nil || got != "" {
		t.Fatalf("missing .env should be empty and fine, got %q, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := readEnvFile(dir); err != nil || got != "A=1\n" {
		t.Fatalf("readEnvFile = %q, %v", got, err)
	}
}

// The constants here must track componentSpec, or the updater would write a
// variable the compose file does not read.
func TestTagEnvConstantsMatchComponentSpecs(t *testing.T) {
	s, _ := newTestServer(t)
	core, err := s.componentSpec("core")
	if err != nil {
		t.Fatal(err)
	}
	if core.tagEnv != mainTagEnv {
		t.Errorf("core tagEnv = %q, mainTagEnv = %q", core.tagEnv, mainTagEnv)
	}
}

func TestHelperPersistsTagsBeforeRecreating(t *testing.T) {
	s, runner := newTestServer(t)
	writeCompose(t, s.composeFile, composeWithUpdater)
	dir := filepath.Dir(s.composeFile)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("COMPOSE_PROJECT_NAME=myhome\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.replaceUpdater(context.Background(), "v1.13.3-beta.1"); err != nil {
		t.Fatalf("replaceUpdater: %v", err)
	}
	run := strings.Join(runner.snapshot()[2], " ")

	if !strings.Contains(run, "-v "+dir+":"+dir+" ") {
		t.Errorf("helper needs the project writable to persist .env\ngot: %s", run)
	}
	if strings.Contains(run, dir+":"+dir+":ro") {
		t.Errorf("project is still mounted read-only\ngot: %s", run)
	}
	// Write the pin first, then recreate: the new sidecar should come up with
	// the file already correct.
	pinAt, upAt := strings.Index(run, "base64 -d"), strings.Index(run, "exec docker 'compose'")
	if pinAt < 0 || upAt < 0 || pinAt > upAt {
		t.Errorf("expected the .env write before the recreate\ngot: %s", run)
	}

	start := strings.Index(run, "printf %s '") + len("printf %s '")
	end := strings.Index(run[start:], "'") + start
	decoded, err := base64.StdEncoding.DecodeString(run[start:end])
	if err != nil {
		t.Fatalf("payload does not decode: %v", err)
	}
	for _, want := range []string{
		"COMPOSE_PROJECT_NAME=myhome",
		"FTW_IMAGE_TAG=v1.13.3-beta.1",
		"FTW_UPDATER_IMAGE_TAG=v1.13.3-beta.1",
	} {
		if !strings.Contains(string(decoded), want) {
			t.Errorf("persisted .env missing %q\ngot:\n%s", want, decoded)
		}
	}
}

func TestLegacyReleaseIdentitySurvivesLaterPlainComposeRecreate(t *testing.T) {
	s, runner := newTestServer(t)
	writeCompose(t, s.composeFile, `services:
  ftw:
    image: ghcr.io/srcfl/ftw:${FTW_IMAGE_TAG:-latest}
    volumes:
      - ./data:/app/data
  ftw-updater:
    image: ghcr.io/srcfl/ftw-updater:${FTW_UPDATER_IMAGE_TAG:-latest}
`)
	target := "v1.13.3-beta.1"
	if err := s.replaceUpdater(context.Background(), target); err != nil {
		t.Fatalf("replaceUpdater: %v", err)
	}

	// Run the same persistence fragment the detached helper runs, without its
	// delay or its final sidecar recreate.
	runArgs := runner.snapshot()[2]
	script := runArgs[len(runArgs)-1]
	script = strings.TrimPrefix(script, "sleep 3; ")
	if end := strings.LastIndex(script, "; exec docker "); end >= 0 {
		script = script[:end]
	} else {
		t.Fatalf("helper script has no final Compose recreate: %s", script)
	}
	cmd := exec.Command("sh", "-c", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run helper persistence: %v\n%s", err, out)
	}

	dir := filepath.Dir(s.composeFile)
	env, err := readEnvFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(env, mainTagEnv+"="+target) {
		t.Fatalf("persisted .env lost exact Core tag:\n%s", env)
	}
	overrides := discoverOverrides(s.composeFile)
	if len(overrides) != 1 || filepath.Base(overrides[0]) != releaseIdentityOverrideName {
		t.Fatalf("plain Compose would not discover the release identity override: %v", overrides)
	}
	mapped, err := serviceEnvironmentUsesVariable(append([]string{s.composeFile}, overrides...), canonicalMainServiceName, mainTagEnv, mainTagEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !mapped {
		t.Fatal("later plain Compose recreate would omit FTW_IMAGE_TAG from Core")
	}
}

func TestLegacyListEnvironmentReleaseIdentitySurvivesLaterPlainComposeRecreate(t *testing.T) {
	s, runner := newTestServer(t)
	writeCompose(t, s.composeFile, `services:
  ftw:
    image: ghcr.io/srcfl/ftw:${FTW_IMAGE_TAG:-latest}
    environment:
      - OPERATOR_SETTING=preserved
      - FTW_IMAGE_TAG=${FTW_IMAGE_TAG:-}
  ftw-updater:
    image: ghcr.io/srcfl/ftw-updater:${FTW_UPDATER_IMAGE_TAG:-latest}
`)
	target := "v1.13.3-beta.1"
	if err := s.replaceUpdater(context.Background(), target); err != nil {
		t.Fatalf("replaceUpdater: %v", err)
	}

	run := strings.Join(runner.snapshot()[2], " ")
	if strings.Contains(run, releaseIdentityOverrideName) {
		t.Fatalf("list-form mapping should not create a redundant override: %s", run)
	}
	if !strings.Contains(run, "base64 -d") {
		t.Fatalf("list-form mapping did not persist the exact tag: %s", run)
	}

	mapped, err := serviceEnvironmentUsesVariable([]string{s.composeFile}, canonicalMainServiceName, mainTagEnv, mainTagEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !mapped {
		t.Fatal("later plain Compose recreate would omit FTW_IMAGE_TAG from list-form environment")
	}
}

func TestComposeEnvironmentListBareVariableFailsClosed(t *testing.T) {
	dir := t.TempDir()
	compose := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(compose, []byte(`services:
  ftw:
    environment:
      - FTW_IMAGE_TAG
`), 0o600); err != nil {
		t.Fatal(err)
	}
	mapped, err := serviceEnvironmentUsesVariable([]string{compose}, canonicalMainServiceName, mainTagEnv, mainTagEnv)
	if err != nil {
		t.Fatal(err)
	}
	if mapped {
		t.Fatal("bare list entry does not prove that FTW_IMAGE_TAG is passed by value")
	}
}

func TestComposeEnvironmentListUsesLastAssignment(t *testing.T) {
	dir := t.TempDir()
	compose := filepath.Join(dir, "compose.yml")
	tests := []struct {
		name  string
		items string
		want  bool
	}{
		{
			name: "later composite overrides exact mapping",
			items: `      - FTW_IMAGE_TAG=${FTW_IMAGE_TAG:-}
      - FTW_IMAGE_TAG=${FTW_IMAGE_TAG:-}${ARCH}
`,
			want: false,
		},
		{
			name: "later exact mapping overrides composite",
			items: `      - FTW_IMAGE_TAG=${FTW_IMAGE_TAG:-}${ARCH}
      - FTW_IMAGE_TAG=${FTW_IMAGE_TAG:-}
`,
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := "services:\n  ftw:\n    environment:\n" + tc.items
			if err := os.WriteFile(compose, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := serviceEnvironmentUsesVariable([]string{compose}, canonicalMainServiceName, mainTagEnv, mainTagEnv)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("serviceEnvironmentUsesVariable() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestComposeValueUsesOnlyOneExactVariableExpansion(t *testing.T) {
	for _, value := range []string{
		"$FTW_IMAGE_TAG",
		"${FTW_IMAGE_TAG}",
		"${FTW_IMAGE_TAG:-}",
		"${FTW_IMAGE_TAG:-latest}",
	} {
		if !composeValueUsesVariable(value, mainTagEnv) {
			t.Errorf("composeValueUsesVariable(%q) = false, want true", value)
		}
	}
	for _, value := range []string{
		"${FTW_IMAGE_TAG:-}${ARCH}",
		"${FTW_IMAGE_TAG:-${DEFAULT_TAG}}",
		"prefix-${FTW_IMAGE_TAG}",
		"${FTW_IMAGE_TAG}-suffix",
	} {
		if composeValueUsesVariable(value, mainTagEnv) {
			t.Errorf("composeValueUsesVariable(%q) = true, want false", value)
		}
	}
}

func TestHardCodedLegacyImageDoesNotPersistFalseReleaseIdentity(t *testing.T) {
	s, _ := newTestServer(t)
	writeCompose(t, s.composeFile, `services:
  ftw:
    image: ghcr.io/srcfl/ftw:latest
`)
	if _, err := s.releaseIdentityPinStep(filepath.Dir(s.composeFile), "v1.13.3-beta.1"); err == nil {
		t.Fatal("hard-coded host image must not receive a different persisted runtime identity")
	}
}

// An unreadable .env must not cost the site its update.
func TestUnreadableEnvStillReplacesTheUpdater(t *testing.T) {
	s, runner := newTestServer(t)
	writeCompose(t, s.composeFile, composeWithUpdater)
	// A directory where the file should be: readable path, unreadable content.
	if err := os.Mkdir(filepath.Join(filepath.Dir(s.composeFile), ".env"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := s.replaceUpdater(context.Background(), "v1.13.3-beta.1"); err != nil {
		t.Fatalf("replaceUpdater must still run: %v", err)
	}
	run := strings.Join(runner.snapshot()[2], " ")
	if strings.Contains(run, "base64 -d") {
		t.Errorf("must not overwrite an .env it could not read\ngot: %s", run)
	}
	if !strings.Contains(run, "'up' '-d' '--no-deps' 'ftw-updater'") {
		t.Errorf("the recreate itself must still happen\ngot: %s", run)
	}
}
