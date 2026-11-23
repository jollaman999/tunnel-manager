//go:build windows

package main

import (
	"fmt"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"golang.org/x/sys/windows/svc"
)

// This file has been built for windows and never run on it. What it does with
// the service control manager - when Execute is called, which states the SCM
// accepts and how long it waits for them - is written against the documented
// behaviour of the svc package and nothing else.

// windowsServiceName is the name this program is registered under.
//
// It is the serviceName of internal/install, which is unexported there: the
// name is the one thing an install and the program it installs have to agree
// on, and a second spelling of it would be a service that registers under one
// name and answers under another.
//
// The SCM tells a process which service it is starting, so for a service that
// has a process to itself this name is not what the connection is made under.
// It is passed because svc.Run asks for it, and it is passed correctly because
// an own-process service that named the wrong one would be harder to find than
// it is worth.
const windowsServiceName = "tunnel-manager"

// serviceStopWaitHint is what the SCM is told a stop takes.
//
// It is the three waits the shutdown is made of: draining the API server,
// letting the reconcile pass that is running end, and letting the tunnels that
// were still connecting when they were stopped end their attempt
// (tunnel.StartEndWaitLimit). The SCM kills a service that says nothing for
// longer than its wait hint, and killing this one part way through leaves the
// tunnels it built behind on the hosts.
const serviceStopWaitHint = shutdownTimeout + reconcileStopTimeout + tunnel.StartEndWaitLimit

// runningAsService says whether this process was started by the service control
// manager rather than by somebody at a console.
//
// A check that could not be made is answered as "not a service". The console is
// the case where being wrong is visible and recoverable: a process that is not
// a service and enters the SCM protocol is killed by the SCM for never
// connecting, while one that is a service and runs as a console program is
// stopped by the SCM after its start timeout. Either way the answer to be wrong
// in is the one that still writes to a console an operator is looking at.
func runningAsService() bool {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return false
	}

	return isService
}

// runService runs this program under the service control manager.
//
// run is the whole of what this program does when it is started from a console:
// it is called once, in a goroutine of its own, and it has to return only once
// the shutdown has finished. stop is what asks run to shut down. It is called
// from the loop that answers the SCM, so it has to return straight away and
// leave run to end on its own; closing the channel the shutdown waits on is
// what it is for. What run answers is whether the uninstall on the Settings
// screen ended it.
//
// It returns once run has returned, which is what the SCM takes as the service
// having stopped.
func runService(run func() bool, stop func()) error {
	err := svc.Run(windowsServiceName, serviceHandler{run: run, stop: stop})
	if err != nil {
		return fmt.Errorf("failed to run as a Windows service: %w", err)
	}

	return nil
}

// serviceHandler is the side of this program the SCM talks to. It holds the two
// functions runService was handed and nothing else: what the program does is
// not this file's business, and every state this reports is worked out from
// whether run has returned and what it answered.
type serviceHandler struct {
	run  func() bool
	stop func()
}

// Execute is called by the svc package once the SCM has connected.
//
// The service is reported as running as soon as run has been started, and not
// once it has finished starting. There is no signal for the latter - run is the
// whole of the startup and the serving both - and a service that is slow to
// report running is stopped by the SCM for not reporting it. A startup that
// fails ends the process, which is what the failure actions on the registration
// are there to answer.
func (h serviceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	// Interrogate is always accepted and is not named here. Stop is the service
	// being stopped and Shutdown is the machine going down, and this program
	// does the same thing for both: everything it built is on other hosts and
	// has to be taken down in order either way.
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	status <- svc.Status{State: svc.StartPending}

	done := make(chan struct{})

	// uninstalled is what run answered. It is written before done is closed
	// and read only after, so the close is what hands it over.
	uninstalled := false

	go func() {
		defer close(done)

		uninstalled = h.run()
	}()

	status <- svc.Status{State: svc.Running, Accepts: accepted}

	// stopping tells the two ways out of this loop apart. A run that ended
	// after it was asked to is a service that stopped, and a run that ended
	// while nobody asked is a service that failed.
	stopping := false

	for {
		select {
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				// Answered with the status the SCM already holds. Interrogate
				// asks what the state is, and anything else written here would
				// be this handler telling the SCM about a change that did not
				// happen.
				status <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				if stopping {
					// A second stop while the first is on its way down. The
					// shutdown is already running and asking for it again would
					// only reset the wait hint.
					continue
				}

				stopping = true

				// Reported before the shutdown is asked for, so that the SCM
				// knows how long this takes before it begins taking it.
				status <- svc.Status{
					State:    svc.StopPending,
					WaitHint: uint32(serviceStopWaitHint / time.Millisecond),
				}

				h.stop()
			default:
				// A command that was not accepted above. The SCM does not send
				// these, and one that arrived would be answered by doing
				// nothing rather than by reporting a state nothing changed.
			}
		case <-done:
			if stopping {
				return false, 0
			}

			// The uninstall on the Settings screen removed the data and
			// marked this registration for deletion, which the SCM carries
			// out once the service has stopped (install.RemoveThisService).
			// Reported as a stop with exit code 0, which the failure actions
			// leave alone even with the flag that takes in a stop with an
			// exit code: reported as a failure it would be started again
			// with an empty database a few seconds after the screen said the
			// installation was gone, if the deletion had not gone through.
			// It is told apart from the restart below by what run answered,
			// and not by how it ended, since both end the same way with
			// nobody having asked the SCM for anything.
			if uninstalled {
				return false, 0
			}

			// The program ended without having been asked to: the restart on
			// the Settings screen, which this platform cannot do in place and
			// leaves to the failure actions. Reported as a failure rather
			// than as a stop, because a service that the SCM was told stopped
			// normally is a service the failure actions on the registration
			// leave down. A startup that failed or a server that could not
			// bind never gets here: those end the process from inside run.
			//
			// The SCM reports this as WIN32_EXIT_CODE 1066 with a service
			// exit code of 1 and logs event 7024, and it applies the failure
			// actions to it only where the registration has the flag that
			// takes in a stop with an exit code (failureActionsFlag in
			// internal/install). Watched on Windows with a registration
			// without the flag: the service stayed stopped. That it is
			// started again with the flag set is the documented behaviour
			// and has not been watched happen.
			return true, 1
		}
	}
}
