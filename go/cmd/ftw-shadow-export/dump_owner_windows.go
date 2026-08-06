//go:build windows

package main

import "os"

func ownedByCurrentUser(os.FileInfo) bool {
	// The v1 sidecar is Unix-only, and this build cannot prove a Unix owner.
	// Refuse secure dumps instead of weakening the ownership contract.
	return false
}

func syncDirectory(string) error {
	return os.ErrPermission
}
