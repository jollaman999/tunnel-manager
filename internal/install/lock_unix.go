//go:build !windows

package install

import (
	"errors"
	"syscall"
)

// processRunning says whether a process with this id is running.
//
// Signal 0 is checked for and not sent. A process that runs as another account
// refuses it with EPERM, which is still a process that is there.
func processRunning(pid int) bool {
	err := syscall.Kill(pid, 0)

	return err == nil || errors.Is(err, syscall.EPERM)
}
