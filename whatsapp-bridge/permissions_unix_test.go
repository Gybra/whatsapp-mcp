//go:build !windows

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func assertPrivate(t *testing.T, path string, dir bool) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	want := os.FileMode(0o600)
	if dir {
		want = 0o700
	}
	if info.Mode().Perm() != want || info.IsDir() != dir {
		t.Fatalf("%s: mode %v, want %v (dir %v)", path, info.Mode(), want, dir)
	}
}

func TestPermissionTightensOwnedPaths(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	os.Mkdir(dir, 0o755)
	os.Chmod(dir, 0o755)
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	assertPrivate(t, dir, true)
	file := filepath.Join(dir, "messages.db")
	os.WriteFile(file, []byte("x"), 0o644)
	os.Chmod(file, 0o644)
	if err := EnsurePrivateFile(file); err != nil {
		t.Fatal(err)
	}
	assertPrivate(t, file, false)
}

func TestPermissionRejectsForeignOwner(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root")
	}
	if err := EnsurePrivateFile("/etc/hosts"); err == nil {
		t.Error("root-owned file accepted")
	}
	if err := EnsurePrivateDir("/usr"); err == nil {
		t.Error("root-owned directory accepted")
	}
}

func TestPermissionRejectsSpecialFiles(t *testing.T) {
	if err := EnsurePrivateFile("/dev/null"); err == nil {
		t.Error("device accepted as a private file")
	}
}

type foreignFileInfo struct{ fs.FileInfo }

func (foreignFileInfo) Sys() any { return &syscall.Stat_t{Uid: uint32(os.Getuid() + 1)} }

func TestPermissionRejectsForeignOwnerWithPrivateMode(t *testing.T) {
	file := filepath.Join(t.TempDir(), "already-private")
	os.WriteFile(file, nil, 0o600)
	info, _ := os.Lstat(file)
	// Already 0600, so only the ownership check can reject it.
	if err := securePath(file, foreignFileInfo{info}); err == nil {
		t.Fatal("file owned by another user accepted")
	}
}
