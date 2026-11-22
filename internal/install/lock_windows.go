//go:build windows

package install

import (
	"errors"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code GetExitCodeProcess reports for a process that
// has not exited (STILL_ACTIVE, which x/sys/windows does not name).
const stillActive = 259

// processRunning says whether a process with this id is running.
//
// Opening it is not enough on its own. A process that has exited stays
// openable for as long as anything holds a handle to it, so the exit code is
// read as well. A process this account may not open is still a process that is
// there.
func processRunning(pid int) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}

	defer func() {
		_ = windows.CloseHandle(handle)
	}()

	var code uint32

	err = windows.GetExitCodeProcess(handle, &code)
	if err != nil {
		return true
	}

	return code == stillActive
}
