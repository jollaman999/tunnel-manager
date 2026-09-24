//go:build windows

package auth

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// requireOwnerSystemAdminsOnly fails unless the DACL of the file is protected,
// inherits nothing and allows the user of this process, SYSTEM and
// Administrators and nobody else.
func requireOwnerSystemAdminsOnly(t *testing.T, path string) {
	t.Helper()

	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("failed to read the security descriptor of %s: %v", path, err)
	}

	control, _, err := sd.Control()
	if err != nil {
		t.Fatalf("failed to read the security descriptor control of %s: %v", path, err)
	}

	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Error("the DACL of the initial password file is not protected, so it takes what its directory hands down")
	}

	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("failed to read the DACL of %s: %v", path, err)
	}

	if dacl == nil {
		t.Fatalf("%s has no DACL, so everybody may read it", path)
	}

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("failed to read the user of this process: %v", err)
	}

	want := map[string]bool{"S-1-5-18": false, "S-1-5-32-544": false, user.User.Sid.String(): false}

	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE

		err = windows.GetAce(dacl, i, &ace)
		if err != nil {
			t.Fatalf("failed to read entry %d of the DACL: %v", i, err)
		}

		if ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
			t.Errorf("entry %d of the DACL is inherited", i)
		}

		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Errorf("entry %d of the DACL is of type %d, want an allow entry", i, ace.Header.AceType)

			continue
		}

		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()

		_, ok := want[sid]
		if !ok {
			t.Errorf("the DACL of the initial password file lets %s in", sid)

			continue
		}

		want[sid] = true
	}

	for sid, seen := range want {
		if !seen {
			t.Errorf("the DACL of the initial password file does not let %s in", sid)
		}
	}
}

func TestWriteInitialPasswordFileIsForOwnerSystemAndAdminsOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "initial-password")

	err := writeInitialPasswordFile(path, "test-password")
	if err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	requireOwnerSystemAdminsOnly(t, path)
}

// TestWriteInitialPasswordFileReplacesAWideFileWithANarrowOne covers the
// leftover that is removed and created again: the new file does not keep the
// DACL of the one it replaced.
func TestWriteInitialPasswordFileReplacesAWideFileWithANarrowOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "initial-password")

	err := os.WriteFile(path, []byte("test-password-2"), 0644)
	if err != nil {
		t.Fatalf("failed to prepare the file: %v", err)
	}

	err = writeInitialPasswordFile(path, "test-password")
	if err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	requireOwnerSystemAdminsOnly(t, path)
}
