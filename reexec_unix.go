//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// canReexec says that this process can run this program again in place of
// itself. It is asked before the restart is offered, so that the screen can say
// beforehand whether the service comes back on its own.
func canReexec() bool {
	return true
}

// reexec replaces the image of this process with this program again.
//
// apiPort is the port the API was served on. It is handed over in
// previousAPIPortEnv, so that the new image tries it again when the stored port
// is still taken; 0 hands over none.
//
// It is exec and not a child process on purpose. A child leaves the session but
// not the cgroup, and systemd takes down what is left in the cgroup of a unit
// whose main process ended, while docker ends the container when PID 1 does. So
// a restart that forks and exits works only where the service was started by
// hand and fails in both ways this is deployed. exec keeps the process, the PID
// and the cgroup, and the supervisor sees nothing happen at all.
//
// The caller runs the ordered shutdown first. Nothing here closes a listener,
// and a port that is still held is one the new image cannot bind.
//
// A return from this is a failure. On success there is no code here any more to
// return to: the image was replaced.
func reexec(apiPort int) error {
	// The path comes from the kernel rather than from os.Args[0], which may be
	// relative to a working directory that has changed since, and may not name
	// this program at all when the process was started through a shell that set
	// it to something else.
	path, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to find the file this process was started from: %w", err)
	}

	// The arguments and the environment are handed over as they are, so the new
	// image opens the same database and reads the same settings. A -db that was
	// dropped here would silently start a second, empty installation.
	err = syscall.Exec(path, os.Args, restartEnviron(apiPort))
	if err != nil {
		return fmt.Errorf("failed to run %s again: %w", path, err)
	}

	// exec does not return when it worked, so reaching this means it came back
	// while reporting no failure.
	return fmt.Errorf("running %s again returned without replacing this process", path)
}
