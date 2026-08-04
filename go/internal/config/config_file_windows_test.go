//go:build windows

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func verifyConfigFileOwnerOnly(path string) error {
	path, err := normalizeWindowsConfigPath(path)
	if err != nil {
		return fmt.Errorf("normalize config path: %w", err)
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return fmt.Errorf("open current process token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("read current user SID: %w", err)
	}
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("read config security descriptor: %w", err)
	}
	control, _, err := sd.Control()
	if err != nil {
		return fmt.Errorf("read security descriptor control: %w", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("config DACL is inheritable: control=%#x", control)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("read config owner: %w", err)
	}
	if !owner.Equals(user.User.Sid) {
		return fmt.Errorf("config owner SID = %s, want current user %s", owner.String(), user.User.Sid.String())
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read config DACL: %w", err)
	}
	if dacl.AceCount != 1 {
		return fmt.Errorf("config DACL ACE count = %d, want one owner ACE", dacl.AceCount)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		return fmt.Errorf("read config DACL ACE: %w", err)
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
		return fmt.Errorf("config DACL ACE type = %d, want allow", ace.Header.AceType)
	}
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !aceSID.Equals(user.User.Sid) {
		return fmt.Errorf("config DACL ACE SID = %s, want current user %s", aceSID.String(), user.User.Sid.String())
	}
	required := uint32(windows.FILE_READ_DATA | windows.FILE_WRITE_DATA)
	if uint32(ace.Mask)&required != required {
		return fmt.Errorf("config owner ACE mask = %#x, lacks read/write", ace.Mask)
	}
	return nil
}

func TestCreateConfigTempUsesOwnerOnlyACLBeforeFirstWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml.tmp")
	f, err := createConfigTemp(path, configFileMode)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	if err := verifyConfigFileOwnerOnly(path); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateConfigTempDoesNotFollowReparsePoint(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target-that-must-not-be-created")
	link := filepath.Join(dir, "config.yaml.tmp")
	targetPtr, err := windows.UTF16PtrFromString(target)
	if err != nil {
		t.Fatal(err)
	}
	linkPtr, err := windows.UTF16PtrFromString(link)
	if err != nil {
		t.Fatal(err)
	}
	const symbolicLinkFlagAllowUnprivilegedCreate = 0x2
	if err := windows.CreateSymbolicLink(linkPtr, targetPtr, symbolicLinkFlagAllowUnprivilegedCreate); err != nil {
		t.Fatalf("CreateSymbolicLink with the unprivileged flag failed; hosted Windows must support this test: %v", err)
	}
	defer os.Remove(link)

	f, err := createConfigTemp(link, configFileMode)
	if err == nil {
		_ = f.Close()
		t.Fatal("createConfigTemp opened a reparse point")
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reparse target status = %v, target must not be created or written", err)
	}
	if got, err := os.Readlink(link); err != nil {
		t.Fatalf("reparse candidate was not preserved: %v", err)
	} else if got != target {
		t.Fatalf("reparse target = %q, want %q", got, target)
	}
}

func TestSaveAtomicSupportsLongWindowsPath(t *testing.T) {
	dir := t.TempDir()
	for len(filepath.Join(dir, "config.yaml")) <= 260 {
		dir = filepath.Join(dir, "long-config-path-"+strings.Repeat("x", 20))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "config.yaml")
	if len(path) <= 260 {
		t.Fatalf("test path length = %d, want more than MAX_PATH", len(path))
	}
	normalized, err := normalizeWindowsConfigPath(path + ".tmp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(normalized, `\\?\`) {
		t.Fatalf("normalized long path = %q, want extended prefix", normalized)
	}
	c, err := Parse([]byte(minimalYAML), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveAtomic(path, c); err != nil {
		t.Fatal(err)
	}
	if err := verifyConfigFileOwnerOnly(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(path); err != nil {
		t.Fatalf("saved config at long path cannot be read: %v", err)
	}
}

func TestNormalizeWindowsConfigPathKeepsShortPathsAndPrefixesLongUNC(t *testing.T) {
	short := filepath.Join(t.TempDir(), "config.yaml")
	got, err := normalizeWindowsConfigPath(short)
	if err != nil {
		t.Fatal(err)
	}
	if got != short {
		t.Fatalf("short path changed from %q to %q", short, got)
	}
	relative, err := normalizeWindowsConfigPath("config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if relative != "config.yaml" {
		t.Fatalf("short relative path changed to %q", relative)
	}
	unc := `\\server\share\` + strings.Repeat("x", 240) + `\config.yaml`
	got, err = normalizeWindowsConfigPath(unc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, `\\?\UNC\server\share\`) {
		t.Fatalf("long UNC path = %q, want \\?\\UNC prefix", got)
	}
}
