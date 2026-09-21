package install

import (
	"path/filepath"
	"strings"
	"testing"
)

// installationOnDisk writes out an installation the way a running one looks:
// the database and the two files SQLite keeps beside it, the key, the initial
// password file, the log and two rotated logs. It answers the data directory
// and the database file.
func installationOnDisk(t *testing.T) (string, string) {
	t.Helper()

	dataDir := t.TempDir()
	databaseFile := filepath.Join(dataDir, databaseFileName)

	for _, path := range []string{
		databaseFile,
		databaseFile + walSuffix,
		databaseFile + shmSuffix,
		filepath.Join(dataDir, "keys", "tunnel-manager.key"),
		filepath.Join(dataDir, "initial-password"),
		filepath.Join(dataDir, "logs", "tunnel-manager.log"),
		filepath.Join(dataDir, "logs", "tunnel-manager-2026-09-21T02-44-10.000.log"),
		filepath.Join(dataDir, "logs", "tunnel-manager-2026-09-20T02-44-10.000.log.gz"),
		// Two files that are not of this installation, in the directory the
		// rotated logs are looked for in. Neither carries a timestamp that
		// parses, which is the whole of what keeps them out.
		filepath.Join(dataDir, "logs", "tunnel-manager-old.log"),
		filepath.Join(dataDir, "logs", "something-else.log"),
	} {
		writeFile(t, path, "")
	}

	return dataDir, databaseFile
}

// TestInstallationFiles holds what a removal says it is about to remove.
//
// The database and the log come from what the running service had open, which
// is the process saying which files it opened. The rest cannot come from there
// and is worked out: the key is read at startup and closed again, the initial
// password file is usually gone, and a rotated log is not open by definition.
func TestInstallationFiles(t *testing.T) {
	dataDir, databaseFile := installationOnDisk(t)
	logFile := filepath.Join(dataDir, "logs", "tunnel-manager.log")

	// What a running tunnel-manager has open, in the shape it was read off this
	// machine.
	openFiles := []string{logFile, databaseFile, databaseFile + shmSuffix, databaseFile + walSuffix}

	want := []string{
		databaseFile,
		databaseFile + walSuffix,
		databaseFile + shmSuffix,
		filepath.Join(dataDir, "keys", "tunnel-manager.key"),
		filepath.Join(dataDir, "initial-password"),
		filepath.Join(dataDir, "logs", "tunnel-manager-2026-09-20T02-44-10.000.log.gz"),
		filepath.Join(dataDir, "logs", "tunnel-manager-2026-09-21T02-44-10.000.log"),
		logFile,
	}

	files := installationFiles(databaseFile, openFiles)

	got := make([]string, 0, len(files))
	for _, file := range files {
		got = append(got, file.Path)
	}

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the files of the installation are\n%s\nwant\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	for _, file := range files {
		t.Logf("%s  %s", file.Path, file.What)
	}
}

// TestInstallationFilesWithoutTheProcess covers the same installation with
// nothing open: the service was already down when the uninstall ran.
//
// The log is then the default read against the data directory, which is what
// the process itself does with the stored path (main.go, resolveInstallPath).
func TestInstallationFilesWithoutTheProcess(t *testing.T) {
	dataDir, databaseFile := installationOnDisk(t)

	files := installationFiles(databaseFile, nil)

	found := false

	for _, file := range files {
		if file.Path == filepath.Join(dataDir, "logs", "tunnel-manager.log") {
			found = true
		}
	}

	if !found {
		t.Errorf("the log file is not among %v", files)
	}
}

// TestInstallationFilesOnlyNamesWhatIsThere covers the other half of the list:
// a path of an installation that has nothing at it is not shown.
//
// The list is put in front of an operator before anything is removed, and a
// line naming a file that is not there reads as a removal that has lost track
// of what it is doing.
func TestInstallationFilesOnlyNamesWhatIsThere(t *testing.T) {
	dataDir := t.TempDir()
	databaseFile := filepath.Join(dataDir, databaseFileName)

	writeFile(t, databaseFile, "the database")

	files := installationFiles(databaseFile, nil)

	if len(files) != 1 || files[0].Path != databaseFile {
		t.Errorf("the files are %v, want the database alone", files)
	}
}

// TestInstallationFilesWithoutADatabase covers the case where nothing says
// where the data is. There is no directory to read anything against, so there
// is nothing to name.
func TestInstallationFilesWithoutADatabase(t *testing.T) {
	if files := installationFiles("", nil); files != nil {
		t.Errorf("the files of an installation whose database is not known are %v, want none", files)
	}
}

// TestOutsideDataDir covers which files -purge has to remove on their own.
//
// -purge removes the data directory whole, so what is under it needs no naming.
// A log or a key whose setting points somewhere else is not under it, and
// leaving the key behind is leaving the one file that opens every stored SSH
// password.
func TestOutsideDataDir(t *testing.T) {
	files := []installedFile{
		{Path: "/var/lib/tunnel-manager/tunnel-manager.db", What: "the database"},
		{Path: "/var/lib/tunnel-manager/keys/tunnel-manager.key", What: "the key"},
		{Path: "/var/log/tunnel-manager/tunnel-manager.log", What: "the log file"},
		// The directory itself is not "under" itself, and is removed by the
		// RemoveAll rather than beside it.
		{Path: "/var/lib/tunnel-manager", What: "the data directory"},
	}

	outside := outsideDataDir(files, "/var/lib/tunnel-manager")

	want := []string{"/var/log/tunnel-manager/tunnel-manager.log", "/var/lib/tunnel-manager"}

	got := make([]string, 0, len(outside))
	for _, file := range outside {
		got = append(got, file.Path)
	}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the files outside the data directory are %v, want %v", got, want)
	}
}
