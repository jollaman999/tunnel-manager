//go:build windows

package main

import "errors"

// canReexec says that this process cannot run this program again in place of
// itself. Windows has no exec: a process image is not replaced, it is started
// as a new process, and a new process started from a dying one is exactly the
// arrangement the Unix side avoids. So the restart ends the process here and
// starting it again is left to whatever supervises it.
//
// The screen asks this before it offers the restart, so that an operator on
// this platform is told beforehand that the service comes back only if
// something starts it again.
func canReexec() bool {
	return false
}

// reexec reports that this platform has no way to do it. The caller ends the
// process instead of running this program again, and apiPort is handed to no
// one: whatever starts the program again does so with its own environment.
func reexec(apiPort int) error {
	return errors.New("this platform cannot replace the image of a running process, " +
		"so starting this program again is left to whatever supervises it")
}
