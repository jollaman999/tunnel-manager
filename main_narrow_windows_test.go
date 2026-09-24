//go:build windows

package main

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
	"github.com/jollaman999/tunnel-manager/internal/settings"
)

// widenDir puts on dir what a directory under C:\ hands down: Users may read,
// and every entry is handed on to what is created inside.
func widenDir(t *testing.T, dir string) {
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

	err = windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, wideDACL, nil)
	if err != nil {
		t.Fatalf("failed to widen %s: %v", dir, err)
	}
}

// otherSIDs returns the SIDs other than the user of this process, SYSTEM and
// Administrators that the DACL of path lets in.
func otherSIDs(t *testing.T, path string) []string {
	t.Helper()

	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("failed to read the security descriptor of %s: %v", path, err)
	}

	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("failed to read the DACL of %s: %v", path, err)
	}

	if dacl == nil {
		return []string{"Everyone (no DACL)"}
	}

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("failed to read the user of this process: %v", err)
	}

	allowed := map[string]bool{"S-1-5-18": true, "S-1-5-32-544": true, user.User.Sid.String(): true}

	var others []string

	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE

		err = windows.GetAce(dacl, i, &ace)
		if err != nil {
			t.Fatalf("failed to read entry %d of the DACL of %s: %v", i, path, err)
		}

		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}

		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if !allowed[sid] {
			others = append(others, sid)
		}
	}

	return others
}

// TestNarrowInstallFilesNarrowsWhatAnEarlierReleaseLeftWide lays down what an
// earlier release left in a data directory the operator made: the database,
// the log directory and the key directory, all taking the DACL of a directory
// Users may read. The files and the directories this program made are
// narrowed with one line for each directory, and the data directory is left
// as it is.
func TestNarrowInstallFilesNarrowsWhatAnEarlierReleaseLeftWide(t *testing.T) {
	installDir := t.TempDir()
	widenDir(t, installDir)

	set := settings.Defaults()

	databaseFile := filepath.Join(installDir, "tunnel-manager.db")
	logFile := resolveInstallPath(installDir, set.LoggingFilePath)
	keyFile := resolveInstallPath(installDir, set.SecurityKeyFile)

	for _, dir := range []string{filepath.Dir(logFile), filepath.Dir(keyFile)} {
		err := os.MkdirAll(dir, 0755)
		if err != nil {
			t.Fatalf("failed to create %s: %v", dir, err)
		}
	}

	for _, path := range []string{databaseFile, databaseFile + "-wal", logFile, keyFile} {
		err := os.WriteFile(path, []byte("x"), 0644)
		if err != nil {
			t.Fatalf("failed to create %s: %v", path, err)
		}

		if len(otherSIDs(t, path)) == 0 {
			t.Fatalf("%s was not wide to begin with", path)
		}
	}

	core, logs := observer.New(zap.WarnLevel)

	narrowInstallFiles(zap.New(core), installDir, databaseFile, &set)

	for _, path := range []string{databaseFile, databaseFile + "-wal", logFile, keyFile,
		filepath.Dir(logFile), filepath.Dir(keyFile)} {
		others := otherSIDs(t, path)
		if len(others) != 0 {
			t.Errorf("%s still lets %v in", path, others)
		}
	}

	if len(otherSIDs(t, installDir)) == 0 {
		t.Error("the data directory was narrowed, and it may be one the operator made")
	}

	narrowed := logs.FilterField(logid.StartupFilesNarrowed.Field()).All()
	if len(narrowed) != 3 || logs.Len() != 3 {
		t.Fatalf("got %d lines saying files were narrowed out of %d, want one for each of the three directories: %v",
			len(narrowed), logs.Len(), logs.All())
	}

	var dirs []string
	for _, entry := range narrowed {
		dir, _ := entry.ContextMap()["dir"].(string)
		dirs = append(dirs, dir)
	}

	for _, want := range []string{installDir, filepath.Dir(logFile), filepath.Dir(keyFile)} {
		if !slices.Contains(dirs, filepath.Clean(want)) {
			t.Errorf("no line names %s, the lines name %v", want, dirs)
		}
	}

	logs.TakeAll()

	narrowInstallFiles(zap.New(core), installDir, databaseFile, &set)

	if logs.Len() != 0 {
		t.Errorf("a second startup wrote %v", logs.All())
	}
}
