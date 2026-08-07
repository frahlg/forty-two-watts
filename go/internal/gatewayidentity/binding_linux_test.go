//go:build linux

package gatewayidentity

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLinuxBindingMarkerTransitionKeepsTransactionLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := PathsForKey(filepath.Join(dir, "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := openBindingStorage(paths, defaultBindingFileOps())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InstallNoReplace("state", []byte("pending\n")); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Lock(); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.ReplaceExact("state", []byte("pending\n"), []byte("active\n")); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}

	contenderStorage, err := openBindingStorage(paths, defaultBindingFileOps())
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	contender := contenderStorage.(*linuxBindingStorage)
	if err := syscall.Flock(
		int(contender.dir.Fd()), syscall.LOCK_EX|syscall.LOCK_NB,
	); !errors.Is(err, syscall.EWOULDBLOCK) {
		_ = contender.Close()
		_ = store.Close()
		t.Fatalf("transaction lock was released by ReplaceExact: %v", err)
	}
	if err := store.Close(); err != nil {
		_ = contender.Close()
		t.Fatal(err)
	}
	if err := lockLinuxBindingDirectory(contender.dir); err != nil {
		_ = contender.Close()
		t.Fatalf("transaction lock remained after Close: %v", err)
	}
	if err := contender.Close(); err != nil {
		t.Fatal(err)
	}
}
