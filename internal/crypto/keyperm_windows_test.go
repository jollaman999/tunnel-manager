//go:build windows

package crypto

import (
	"errors"
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

// widenPath puts on path what a directory under C:\ hands down: Users may read,
// and every entry is handed on to what is created inside.
func widenPath(t *testing.T, path string) {
	t.Helper()

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("failed to read the user of this process: %v", err)
	}

	wide, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.User.Sid.String() +
		")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FR;;;BU)")
	if err != nil {
		t.Fatalf("failed to build a wide security descriptor: %v", err)
	}

	wideDACL, _, err := wide.DACL()
	if err != nil {
		t.Fatalf("failed to read the wide DACL: %v", err)
	}

	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, wideDACL, nil)
	if err != nil {
		t.Fatalf("failed to widen %s: %v", path, err)
	}
}

// requireNoOtherReaders fails when anybody but the owner, SYSTEM and
// Administrators may read path, whether the entries are its own or handed down.
func requireNoOtherReaders(t *testing.T, path string) {
	t.Helper()

	readers, err := keyFileOtherReaders(path)
	if err != nil {
		t.Fatalf("failed to read who may read %s: %v", path, err)
	}

	if len(readers) != 0 {
		t.Errorf("%s may be read by %v", path, readers)
	}
}

func TestMkdirAllPrivateMakesDirectoriesForOwnerSystemAndAdminsOnly(t *testing.T) {
	parent := t.TempDir()
	widenPath(t, parent)

	dir := filepath.Join(parent, "data", "keys")

	err := MkdirAllPrivate(dir, keyDirMode)
	if err != nil {
		t.Fatalf("MkdirAllPrivate returned an error: %v", err)
	}

	for _, path := range []string{filepath.Join(parent, "data"), dir} {
		_, protected := daclOf(t, path)
		if !protected {
			t.Errorf("the DACL of %s is not protected, so it takes what its directory hands down", path)
		}

		requireNoOtherReaders(t, path)
	}

	// A file something else creates inside takes what the directory hands
	// down, which is what the write-ahead log of SQLite relies on.
	file := filepath.Join(dir, "made-by-another")

	err = os.WriteFile(file, []byte("x"), 0644)
	if err != nil {
		t.Fatalf("failed to create a file inside: %v", err)
	}

	requireNoOtherReaders(t, file)

	err = MkdirAllPrivate(dir, keyDirMode)
	if err != nil {
		t.Errorf("MkdirAllPrivate refused a directory that is there: %v", err)
	}
}

func TestMkdirAllPrivateLeavesADirectoryThatIsThereAlone(t *testing.T) {
	dir := t.TempDir()
	widenPath(t, dir)

	err := MkdirAllPrivate(dir, keyDirMode)
	if err != nil {
		t.Fatalf("MkdirAllPrivate returned an error: %v", err)
	}

	readers, err := keyFileOtherReaders(dir)
	if err != nil {
		t.Fatalf("failed to read who may read %s: %v", dir, err)
	}

	if len(readers) == 0 {
		t.Error("MkdirAllPrivate narrowed a directory that was already there")
	}
}

func TestNarrowPrivateDirNarrowsTheDirectoryAndWhatTookItsDACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")

	err := os.Mkdir(dir, 0755)
	if err != nil {
		t.Fatalf("failed to create the directory: %v", err)
	}

	widenPath(t, dir)

	file := filepath.Join(dir, "tunnel-manager.log")

	err = os.WriteFile(file, []byte("a line\n"), 0644)
	if err != nil {
		t.Fatalf("failed to create a file inside: %v", err)
	}

	readers, err := keyFileOtherReaders(file)
	if err != nil || len(readers) == 0 {
		t.Fatalf("the file inside was not wide to begin with: %v, %v", readers, err)
	}

	readers, err = NarrowPrivateDir(dir)
	if err != nil {
		t.Fatalf("NarrowPrivateDir returned an error: %v", err)
	}

	users, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Fatalf("failed to build the SID of Users: %v", err)
	}

	if !slices.Contains(readers, accountName(users)) {
		t.Errorf("NarrowPrivateDir named %v, want %s among them", readers, accountName(users))
	}

	_, protected := daclOf(t, dir)
	if !protected {
		t.Error("the DACL of the directory is not protected after the narrowing")
	}

	requireNoOtherReaders(t, dir)
	requireNoOtherReaders(t, file)

	readers, err = NarrowPrivateDir(dir)
	if err != nil || len(readers) != 0 {
		t.Errorf("narrowing the narrowed directory again returned %v, %v, want nothing", readers, err)
	}
}

func TestNarrowPrivateFileNarrowsAWideFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tunnel-manager.db")

	err := os.WriteFile(path, []byte("rows"), 0644)
	if err != nil {
		t.Fatalf("failed to create the file: %v", err)
	}

	widenPath(t, path)

	readers, err := NarrowPrivateFile(path)
	if err != nil || len(readers) == 0 {
		t.Fatalf("NarrowPrivateFile returned %v, %v, want the readers of the wide file", readers, err)
	}

	requireOwnerSystemAdminsOnly(t, path)

	_, err = NarrowPrivateFile(filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("NarrowPrivateFile returned %v for a file that is not there, want os.ErrNotExist", err)
	}
}

func TestReservePrivateFileCreatesANarrowFileAndLeavesOneThatIsThere(t *testing.T) {
	dir := t.TempDir()
	widenPath(t, dir)

	path := filepath.Join(dir, "tunnel-manager.log")

	err := ReservePrivateFile(path)
	if err != nil {
		t.Fatalf("ReservePrivateFile returned an error: %v", err)
	}

	requireOwnerSystemAdminsOnly(t, path)

	err = os.WriteFile(path, []byte("a line\n"), 0600)
	if err != nil {
		t.Fatalf("failed to write the reserved file: %v", err)
	}

	requireOwnerSystemAdminsOnly(t, path)

	err = ReservePrivateFile(path)
	if err != nil {
		t.Fatalf("ReservePrivateFile refused a file that is there: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil || string(got) != "a line\n" {
		t.Errorf("the file reads %q, %v after the second reserve, want what was written", got, err)
	}
}
