//go:build windows

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// createConfigTemp passes a protected security descriptor to CreateFile. The
// descriptor must be present at creation time: applying an ACL after
// os.OpenFile would leave a race in which another local user could read the
// credentials in the temp file before the ACL was tightened.
func createConfigTemp(path string, _ os.FileMode) (*os.File, error) {
	sd, err := ownerOnlyConfigSecurityDescriptor()
	if err != nil {
		return nil, fmt.Errorf("build private config security descriptor: %w", err)
	}
	openPath, err := normalizeWindowsConfigPath(path)
	if err != nil {
		return nil, fmt.Errorf("normalize config temp path: %w", err)
	}
	pathPtr, err := windows.UTF16PtrFromString(openPath)
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	h, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		sa,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(h), path)
	if f == nil {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("wrap config temp handle")
	}
	return f, nil
}

const windowsLongPathThreshold = 248

// normalizeWindowsConfigPath follows the long-path rule used by the Go
// standard library. Short and relative paths stay unchanged. A long path is
// first made absolute, then gets the extended prefix needed by CreateFile;
// UNC paths use the required \\?\UNC form.
func normalizeWindowsConfigPath(path string) (string, error) {
	if isExtendedWindowsPath(path) {
		return path, nil
	}
	pathLength := len(path)
	if !filepath.IsAbs(path) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		pathLength += len(cwd) + 1
	}
	if pathLength < windowsLongPathThreshold {
		return path, nil
	}
	fullPath, err := fullWindowsPath(path)
	if err != nil {
		return "", err
	}
	if isExtendedWindowsPath(fullPath) || isWindowsDevicePath(fullPath) {
		return fullPath, nil
	}
	if isWindowsUNCPath(fullPath) {
		return `\\?\UNC\` + fullPath[2:], nil
	}
	return `\\?\` + fullPath, nil
}

func fullWindowsPath(path string) (string, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	size := uint32(len(path) + 1)
	if size < 256 {
		size = 256
	}
	for {
		buf := make([]uint16, size)
		n, err := windows.GetFullPathName(pathPtr, size, &buf[0], nil)
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "", fmt.Errorf("GetFullPathName returned an empty path")
		}
		if n < size {
			return windows.UTF16ToString(buf[:n]), nil
		}
		size = n + 1
	}
}

func isExtendedWindowsPath(path string) bool {
	if len(path) < 4 {
		return false
	}
	return path[:4] == `\??\` ||
		(isWindowsSeparator(path[0]) && isWindowsSeparator(path[1]) && path[2] == '?' && isWindowsSeparator(path[3]))
}

func isWindowsUNCPath(path string) bool {
	return len(path) >= 2 && isWindowsSeparator(path[0]) && isWindowsSeparator(path[1])
}

func isWindowsDevicePath(path string) bool {
	return len(path) >= 4 && isWindowsUNCPath(path) && path[2] == '.' && isWindowsSeparator(path[3])
}

func isWindowsSeparator(c byte) bool {
	return c == '\\' || c == '/'
}

func ownerOnlyConfigSecurityDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, fmt.Errorf("open current process token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read current user SID: %w", err)
	}
	ownerSID := user.User.Sid

	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(ownerSID),
		},
	}}, nil)
	if err != nil {
		return nil, fmt.Errorf("build owner ACL: %w", err)
	}
	sd, err := windows.NewSecurityDescriptor()
	if err != nil {
		return nil, fmt.Errorf("initialize security descriptor: %w", err)
	}
	if err := sd.SetOwner(ownerSID, false); err != nil {
		return nil, fmt.Errorf("set security descriptor owner: %w", err)
	}
	if err := sd.SetDACL(acl, true, false); err != nil {
		return nil, fmt.Errorf("set security descriptor DACL: %w", err)
	}
	// Do not let an inheritable ACE from the config directory re-open the
	// file to another local account after CreateFile applies this descriptor.
	if err := sd.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		return nil, fmt.Errorf("protect security descriptor DACL: %w", err)
	}
	return sd.ToSelfRelative()
}

func replaceConfigTemp(tmp, path string) error {
	// os.Rename uses MoveFileEx with REPLACE_EXISTING on Windows. Keep Go's
	// long-path handling and the existing atomic replacement semantics; the
	// temp file's protected DACL becomes the destination file's DACL.
	return os.Rename(tmp, path)
}

// syncDir fsyncs a directory where Windows permits it. Windows filesystems
// may reject directory FlushFileBuffers, so retain #792's best-effort rule.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return nil
	}
	return nil
}
