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
	case !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION):
		// Not a file that is being held. A permission that is missing or a
		// path that names something else is a failure and is reported as one,
		// rather than quietly turned into a removal at some later reboot the
		// operator would have to remember.
		return false, fmt.Errorf("failed to remove the executable %s: %w", path, err)
	}

	held := err

	from, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, fmt.Errorf("failed to remove the executable %s (%w), and its path could not be "+
			"handed to Windows to remove at the next reboot: %w", path, held, err)
	}

	// No destination, which is how MoveFileEx is asked to delete rather than to
	// move. It is written into the registry for the session manager to carry
	// out at the start of the next boot, before anything has the file open, and
	// so needs the privilege an install already needs.
	err = windows.MoveFileEx(from, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
	if err != nil {
		return false, fmt.Errorf("the executable %s is in use (%w) and could not be set to be removed at "+
			"the next reboot either: %w", path, held, err)
	}

	return true, nil
}
