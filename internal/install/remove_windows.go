//go:build windows

package install

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// None of this file has been run on Windows, the same as service_windows.go.
// What it does is built for windows from a Linux machine, and the behaviour it
// is written against - that an image which is being executed cannot be deleted,
// and that MoveFileEx with no destination and MOVEFILE_DELAY_UNTIL_REBOOT has
// the session manager delete it while the next boot comes up - is the
// documented behaviour of those calls and has not been watched happen.

func init() {
	removeAtReboot = removeAtNextBoot
}

// removeExecutable removes the installed executable, and says whether it had to
// be left for the next reboot.
//
// Windows holds an image open for as long as a process is running from it, so
// the file cannot be deleted then. That is the ordinary case for an uninstall
// and not an odd one: the operator runs the very executable that was installed,
// and -uninstall is asked to remove it while it is running.
//
// Which files are refused is decided by the error and not by comparing the path
// against this process. A path compares badly on Windows - short names, a
// mapped drive, a junction, a symbolic link all spell the same file
// differently - and the file may be held by something other than this process
// anyway, such as a service that has not finished shutting down or a virus
// scanner reading it. The error says what the path can only guess at.
func removeExecutable(path string) (bool, error) {
	err := os.Remove(path)

	switch {
	case err == nil, errors.Is(err, fs.ErrNotExist):
		return false, nil
	case !inUse(err):
		// Not a file that is being held. A permission that is missing or a
		// path that names something else is a failure and is reported as one,
		// rather than quietly turned into a removal at some later reboot the
		// operator would have to remember.
		return false, fmt.Errorf("failed to remove the executable %s: %w", path, err)
	}

	held := err

	err = removeAtNextBoot(path)
	if err != nil {
		return false, fmt.Errorf("the executable %s is in use (%w) and could not be set to be removed at "+
			"the next reboot either: %w", path, held, err)
	}

	return true, nil
}

// RemoveFile removes a file of the installation, and says whether it had to be
// left for the next reboot.
//
// It is removeExecutable for the files the uninstall on the Settings screen
// removes. Windows refuses to delete a file another handle holds open without
// sharing the deletion, and the file then goes at the next boot instead. The
// log file is the one this is there for: it is closed before it is removed, but
// a handle somebody else holds on it - a viewer following it, a virus scanner -
// still keeps it there. A file that is not there is answered with the error of
// os.Remove as it is, which the caller tells apart.
func RemoveFile(path string) (bool, error) {
	err := os.Remove(path)
	if err == nil || !inUse(err) {
		return false, err
	}

	held := err

	err = removeAtNextBoot(path)
	if err != nil {
		return false, fmt.Errorf("%w, and it could not be set to be removed at the next reboot either: %w",
			held, err)
	}

	return true, nil
}

// inUse says whether a removal was refused because the file is held.
//
// A running image is refused with ERROR_ACCESS_DENIED and a file another handle
// holds open with ERROR_SHARING_VIOLATION, which is what the log file of a
// running service answered with on Windows ("The process cannot access the file
// because it is being used by another process").
func inUse(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

// removeAtNextBoot hands path to the session manager to delete at the start of
// the next boot.
//
// No destination is how MoveFileEx is asked to delete rather than to move. It
// is written into the registry (PendingFileRenameOperations) for the session
// manager to carry out before anything has the file open, and so needs the
// privilege an install already needs. A directory is deleted there only once
// it is empty, which is why removeTreeAtReboot hands over what is in one before
// the directory itself.
func removeAtNextBoot(path string) error {
	from, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("the path %s could not be handed to Windows: %w", path, err)
	}

	return windows.MoveFileEx(from, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
}
