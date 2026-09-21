//go:build linux

package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// procRoot is the process tree the open files are read from.
//
// It is a variable so that a test can point it at a directory of its own: what
// is under /proc/<pid>/fd is symbolic links, and a directory of symbolic links
// made by a test is read by exactly the same code, down to the readlink. The
// real path is the only one anything but a test ever sets.
var procRoot = "/proc"

// openFilesOfPID answers the files process pid holds open, as the paths they
// name.
//
// This is what an uninstall asks instead of reading the command line the
// service was registered with. A registration that passes no -db says nothing
// about which database the process opened, while the descriptors say what it
// actually has open, which is the file that is about to be removed. On this
// machine a running tunnel-manager holds its log, its database and the two
// files SQLite keeps beside it.
func openFilesOfPID(pid int) ([]string, error) {
	dir := filepath.Join(procRoot, strconv.Itoa(pid), "fd")

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %s could not be read: %w", ErrOpenFilesUnknown, dir, err)
	}

	paths := make([]string, 0, len(entries))

	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(dir, entry.Name()))
		if err != nil {
			// A descriptor that was closed between the listing and the read.
			// The process is running and going about its work while this looks
			// at it, so descriptors come and go; one that is gone is one the
			// process does not have open, which is the answer being collected.
			continue
		}

		path, ok := openFilePath(target)
		if !ok {
			continue
		}

		paths = append(paths, path)
	}

	return sortedUnique(paths), nil
}

// OpenFiles answers the files the running service has open.
//
// The process id comes from systemd rather than from a search of the process
// table, so that the files read are the ones of the service this uninstall is
// about and not of another copy of the program somebody started by hand.
func (s systemd) OpenFiles() ([]string, error) {
	pid, err := s.mainPID()
	if err != nil {
		return nil, err
	}

	return openFilesOfPID(pid)
}

// mainPID is the process id systemd has for this unit.
//
// It is 0 for a unit that is not running, which systemctl answers without
// failing, so a service that is down is told apart from one that could not be
// asked about. Neither is a failure of the uninstall: both mean the question
// has no answer, and what a caller does about that depends on whether it was
// going to remove anything with it.
func (s systemd) mainPID() (int, error) {
	value, err := s.property("MainPID")
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrOpenFilesUnknown, err)
	}

	pid, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%w: systemctl answered %q for the main process of %s, which is not a number",
			ErrOpenFilesUnknown, value, s.unit)
	}

	if pid <= 0 {
		return 0, fmt.Errorf("%w: %s is registered but no process of it is running, so there is nothing "+
			"to ask which files it has open", ErrOpenFilesUnknown, s.unit)
	}

	return pid, nil
}
