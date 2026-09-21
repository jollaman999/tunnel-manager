package install

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/auth"
	"github.com/jollaman999/tunnel-manager/internal/settings"
)

// installedFile is one file this installation is made of, with what it is
// beside it. The description is there because a list of paths on its own asks
// the operator to recognise a path they have never had to look at: the key and
// the write ahead log are not files anybody typed.
type installedFile struct {
	Path string
	What string
}

// logFileExtension is the extension a log file carries. It is what picks the
// log out of the files the running service has open, since the process holds
// its log and its database open and nothing else of its own.
const logFileExtension = ".log"

// rotatedLogTimeFormat is the timestamp the log rotation puts into the name of
// a file it rotated, and rotatedLogCompressSuffix is what it appends to one it
// compressed afterwards.
//
// They are spelled out because lumberjack keeps them unexported
// (gopkg.in/natefinch/lumberjack.v2@v2.2.1, backupTimeFormat and
// compressSuffix), and they are the same two the running process removes the
// rotated logs by (internal/api/uninstall.go, isRotatedLogName). A name is only
// taken for a rotated log when this timestamp parses out of it: matching on the
// prefix alone would take a file somebody else left in the log directory, and
// it is a delete at the end of this.
const (
	rotatedLogTimeFormat     = "2006-01-02T15-04-05.000"
	rotatedLogCompressSuffix = ".gz"
)

// installationFiles names the files an installation whose database is
// databaseFile is made of, and answers only the ones that are there.
//
// openFiles is what the running service had open, and may be empty. The
// database and the log are read out of it where it has them, because that is
// the process saying which file it actually opened rather than anything here
// working out where it should have been. What is not in it has to be worked out
// all the same: the key is read at startup and closed again, the initial
// password file is usually gone by now, and a rotated log is not open by
// definition.
//
// Only files that exist are answered. This list is shown to an operator before
// anything is removed, and a line naming a file that is not there reads as a
// removal that has lost track of what it is doing.
func installationFiles(databaseFile string, openFiles []string) []installedFile {
	if databaseFile == "" {
		return nil
	}

	dataDir := dataDirOf(databaseFile)

	files := []installedFile{
		{Path: databaseFile, What: "the database"},
		{Path: databaseFile + walSuffix, What: "the write ahead log of the database"},
		{Path: databaseFile + shmSuffix, What: "the shared memory file of the database"},
		{Path: keyFileOf(dataDir), What: "the key the stored SSH passwords are sealed with"},
		{Path: auth.InitialPasswordFile(databaseFile), What: "the initial password file"},
	}

	for _, logFile := range logFilesOf(dataDir, openFiles) {
		for _, rotated := range rotatedLogsBeside(logFile) {
			files = append(files, installedFile{Path: rotated, What: "a rotated log file"})
		}

		files = append(files, installedFile{Path: logFile, What: "the log file"})
	}

	return onlyExisting(files)
}

// keyFileOf is where the key the stored SSH passwords are sealed with is.
//
// It is worked out the way the process works it out at startup: the stored
// security.key_file read against the directory the database is in (main.go,
// resolveInstallPath). What is not done here is reading the stored value, which
// would mean opening the database of a service that is still running, so the
// default is used - and a setting that was changed to name a file outside the
// data directory is a file this does not list. It is listed from inside the
// data directory in every other case, because -purge removes that directory
// whole.
func keyFileOf(dataDir string) string {
	return resolveAgainst(dataDir, settings.Defaults().SecurityKeyFile)
}

// logFilesOf is the log files of this installation.
//
// What the running process had open wins, because it is the file being written
// to: the path is a setting, and one that was changed without a restart names a
// file this process never wrote to. With nothing open to go by - the service was
// already down - the default is read against the data directory, the same way
// the process reads it.
func logFilesOf(dataDir string, openFiles []string) []string {
	var open []string

	for _, file := range openFiles {
		if strings.HasSuffix(file, logFileExtension) {
			open = append(open, file)
		}
	}

	if len(open) > 0 {
		return open
	}

	return []string{resolveAgainst(dataDir, settings.Defaults().LoggingFilePath)}
}

// resolveAgainst reads a stored path against the directory the database is in,
// which is what the process does with it (main.go, resolveInstallPath). An
// absolute path is left alone and wins.
func resolveAgainst(dataDir string, path string) string {
	if path == "" || dataDir == "" || filepath.IsAbs(path) {
		return path
	}

	return filepath.Join(dataDir, path)
}

// rotatedLogsBeside finds what the log rotation left next to logFile.
//
// The directory is read and the names are matched rather than globbed, for the
// reason the running process does the same: a path may hold the characters a
// pattern is made of.
func rotatedLogsBeside(logFile string) []string {
	dir := filepath.Dir(logFile)
	name := filepath.Base(logFile)
	extension := filepath.Ext(name)
	prefix := strings.TrimSuffix(name, extension) + "-"

	entries, err := os.ReadDir(dir)
	if err != nil {
		// A directory that cannot be read costs the rotated files and nothing
		// else: the log itself is named on its own.
		return nil
	}

	var rotated []string

	for _, entry := range entries {
		if entry.IsDir() || !isRotatedLogName(entry.Name(), prefix, extension) {
			continue
		}

		rotated = append(rotated, filepath.Join(dir, entry.Name()))
	}

	return rotated
}

// isRotatedLogName reports whether a name is one the rotation wrote. The
// timestamp is parsed and not merely looked at, which is what keeps a file
// somebody named tunnel-manager-old.log out of the list.
func isRotatedLogName(name string, prefix string, extension string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}

	stamp := strings.TrimPrefix(name, prefix)
	stamp = strings.TrimSuffix(stamp, rotatedLogCompressSuffix)

	if !strings.HasSuffix(stamp, extension) {
		return false
	}

	_, err := time.Parse(rotatedLogTimeFormat, strings.TrimSuffix(stamp, extension))

	return err == nil
}

// onlyExisting keeps the files that are there, each once.
func onlyExisting(files []installedFile) []installedFile {
	seen := make(map[string]bool, len(files))
	kept := make([]installedFile, 0, len(files))

	for _, file := range files {
		if file.Path == "" || seen[file.Path] {
			continue
		}

		seen[file.Path] = true

		_, err := os.Lstat(file.Path)
		if err != nil {
			continue
		}

		kept = append(kept, file)
	}

	return kept
}

// outsideDataDir answers the files that are not under dataDir.
//
// -purge removes the data directory whole, so everything under it goes with it.
// A file that is not under it - a log or a key a setting pointed somewhere else
// - has to be named and removed on its own, or an uninstall would leave the
// very file that holds the sealed passwords behind.
func outsideDataDir(files []installedFile, dataDir string) []installedFile {
	if dataDir == "" {
		return files
	}

	prefix := filepath.Clean(dataDir) + string(filepath.Separator)

	var outside []installedFile

	for _, file := range files {
		if strings.HasPrefix(filepath.Clean(file.Path), prefix) {
			continue
		}

		outside = append(outside, file)
	}

	return outside
}
