//go:build !windows

package main

// runningAsService says that this process was not started by a service manager
// that expects a protocol of it.
//
// systemd and launchd start a service the way a console starts a program: the
// process runs, and it is told to stop with a signal, which the run already
// waits for. There is nothing here to answer, so the answer is no on every
// platform but Windows, and main has one shape rather than one per platform.
func runningAsService() bool {
	return false
}

// runService runs the program, since there is no service protocol to enter on
// this platform.
//
// The stop is dropped rather than held: what would call it is the loop that
// answers the service control manager, and there is none here. A stop on this
// platform arrives as a signal, and the run waits for that itself.
func runService(run func(), _ func()) error {
	run()

	return nil
}
