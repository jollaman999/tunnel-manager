//go:build windows

package crypto

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"unsafe"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/windows"

	"github.com/jollaman999/tunnel-manager/internal/logid"
)

func TestLoadOrCreateKeyReadsKeyFileWhateverItsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tunnel-manager.key")

	key, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey returned an error: %v", err)
	}

	t.Cleanup(func() {
		_ = os.Chmod(path, 0600)
	})

	for _, mode := range []os.FileMode{0644, 0444} {
		err = os.Chmod(path, mode)
		if err != nil {
			t.Fatalf("failed to set the key file mode to %#o: %v", mode, err)
		}

		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("failed to stat the key file: %v", err)
		}

		if info.Mode().Perm()&0077 == 0 {
			t.Fatalf("os.Stat reported %#o, which no longer looks group or world readable", info.Mode().Perm())
		}

		again, err := LoadOrCreateKey(path)
		if err != nil {
			t.Fatalf("LoadOrCreateKey refused the key file at mode %#o: %v", info.Mode().Perm(), err)
		}

		if string(again) != string(key) {
			t.Fatal("LoadOrCreateKey returned another key")
		}
	}
}

// daclOf reads the DACL of the file with whether it is protected from what the
// directory hands down.
func daclOf(t *testing.T, path string) (*windows.ACL, bool) {
	t.Helper()

	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("failed to read the security descriptor of %s: %v", path, err)
	}

	control, _, err := sd.Control()
	if err != nil {
		t.Fatalf("failed to read the security descriptor control of %s: %v", path, err)
	}

	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("failed to read the DACL of %s: %v", path, err)
	}

	if dacl == nil {
		t.Fatalf("%s has no DACL, so everybody may read it", path)
	}

	return dacl, control&windows.SE_DACL_PROTECTED != 0
}

// requireOwnerSystemAdminsOnly fails unless the DACL of the file is protected,
// inherits nothing and allows the user of this process, SYSTEM and
// Administrators and nobody else.
func requireOwnerSystemAdminsOnly(t *testing.T, path string) {
	t.Helper()

	dacl, protected := daclOf(t, path)
	if !protected {
		t.Error("the DACL of the key file is not protected, so it takes what its directory hands down")
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
			t.Errorf("the DACL of the key file lets %s in", sid)

			continue
		}

		want[sid] = true
	}

	for sid, seen := range want {
		if !seen {
			t.Errorf("the DACL of the key file does not let %s in", sid)
		}
	}
}

func TestLoadOrCreateKeyCreatesKeyFileForOwnerSystemAndAdminsOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tunnel-manager.key")

	_, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey returned an error: %v", err)
	}

	requireOwnerSystemAdminsOnly(t, path)
}

func TestLoadOrCreateKeyNarrowsAWideKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tunnel-manager.key")

	key, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey returned an error: %v", err)
	}

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("failed to read the user of this process: %v", err)
	}

	// What a file under C:\ ends up with: Users may read it, and it takes
	// what its directory hands down.
	wide, err := windows.SecurityDescriptorFromString(
		"D:(A;;FA;;;" + user.User.Sid.String() + ")(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;BU)")
	if err != nil {
		t.Fatalf("failed to build a wide security descriptor: %v", err)
	}

	wideDACL, _, err := wide.DACL()
	if err != nil {
		t.Fatalf("failed to read the wide DACL: %v", err)
	}

	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, wideDACL, nil)
	if err != nil {
		t.Fatalf("failed to widen the key file: %v", err)
	}

	core, logs := observer.New(zap.WarnLevel)

	again, err := LoadOrCreateKeyWithLogger(path, zap.New(core))
	if err != nil {
		t.Fatalf("LoadOrCreateKeyWithLogger refused the wide key file: %v", err)
	}

	if string(again) != string(key) {
		t.Fatal("LoadOrCreateKeyWithLogger returned another key")
	}

	requireOwnerSystemAdminsOnly(t, path)

	narrowed := logs.FilterField(logid.EncryptionKeyFileNarrowed.Field()).All()
	if len(narrowed) != 1 {
		t.Fatalf("got %d lines saying the key file was narrowed, want 1: %v", len(narrowed), logs.All())
	}

	users, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Fatalf("failed to build the SID of Users: %v", err)
	}

	readers, _ := narrowed[0].ContextMap()["readers"].([]interface{})
	if !slices.Contains(readers, interface{}(accountName(users))) {
		t.Errorf("the line names %v as the readers, want %s among them", readers, accountName(users))
	}

	logs.TakeAll()

	_, err = LoadOrCreateKeyWithLogger(path, zap.New(core))
	if err != nil {
		t.Fatalf("LoadOrCreateKeyWithLogger refused the narrowed key file: %v", err)
	}

	if logs.Len() != 0 {
		t.Errorf("loading a narrowed key file wrote %v", logs.All())
	}
}
