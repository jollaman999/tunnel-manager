//go:build windows

package main

import (
	"errors"

	"github.com/jollaman999/tunnel-manager/internal/install"
)

// canReexec says that this process cannot run this program again in place of
// itself. Windows has no exec: a process image is not replaced, it is started
// as a new process, and a new process started from a dying one is exactly the
// arrangement the Unix side avoids. So the restart ends the process here and
// starting it again is left to whatever supervises it.
func canReexec() bool {
	return false
}

// serviceStartsAgain says whether the service control manager starts this
// program again once the restart has ended the process.
//
// The restart ends a service as a failure (main_windows.go), and the failure
// actions of the registration are what start it again. They do so only where
// the registration carries the flag that has them apply to a service that
// reported it stopped, which -install writes and a registration an earlier
// release made does not (install.RestartsAfterFailure). The screen asks before
// it offers the restart, so that an operator is told beforehand whether the
// service comes back.
//
// A process that is not the service, one started from a console, has nothing
// that starts it again. A registration that could not be read is answered the
// same way: the screen then says the service may stay down, which is the
// answer that is only wrong in the direction an operator can recover from.
func serviceStartsAgain() bool {
	if !runningAsService() {
		return false
	}

	starts, err := install.RestartsAfterFailure()
	if err != nil {
		return false
	}

	return starts
}

// reexec reports that this platform has no way to do it. The caller ends the
// process instead of running this program again, and apiPort is handed to no
// one: whatever starts the program again does so with its own environment.
func reexec(apiPort int) error {
	return errors.New("this platform cannot replace the image of a running process, " +
		"so starting this program again is left to whatever supervises it")
}
