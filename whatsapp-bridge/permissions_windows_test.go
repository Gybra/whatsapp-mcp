//go:build windows

package main

import (
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// assertPrivate checks for a protected DACL whose entries all allow only the current user.
// Directories get two entries for that user: one for the directory and one inherited by its contents.
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
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("%s: DACL %s inherits from its parent (%v)", path, sd, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		t.Fatalf("%s: no DACL entries in %s (%v)", path, sd, err)
	}
	for i := range dacl.AceCount {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !(*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(user) {
			t.Fatalf("%s: DACL %s is not private to %s", path, sd, user)
		}
	}
}
