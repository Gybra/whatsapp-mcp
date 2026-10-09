//go:build windows

package main

import (
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// assertPrivate checks for a protected DACL with a single entry for the current user.
func assertPrivate(t *testing.T, path string, dir bool) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || info.IsDir() != dir {
		t.Fatalf("%s: %v (dir %v)", path, err, dir)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	user, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	sddl := sd.String()
	if !strings.Contains(sddl, "D:P") || strings.Count(sddl, "(A;") != 1 || !strings.Contains(sddl, user.String()) {
		t.Fatalf("%s: DACL %s is not private to %s", path, sddl, user)
	}
}
