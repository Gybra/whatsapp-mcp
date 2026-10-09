//go:build !windows

package main

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// restrictUmask makes every file the bridge creates, including SQLite sidecars and media, owner-only.
func restrictUmask() {
	syscall.Umask(0o077)
}

// securePath accepts only paths owned by the current user and removes group and other access.
func securePath(path string, info fs.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("%w: %s", errUnsafePath, path)
	}
	want := fs.FileMode(0o600)
	if info.IsDir() {
		want = 0o700
	}
	if info.Mode().Perm() != want {
		return os.Chmod(path, want)
	}
	return nil
}
