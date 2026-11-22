//go:build windows

package main

import (
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

// executeWait bounds how long a test waits on Execute. The run it is handed
// returns at once or on the stop the test sends, so this is only reached by a
// handler that never ends.
const executeWait = 5 * time.Second

// executeAnswer is what Execute returned.
type executeAnswer struct {
	specific bool
	code     uint32
}

// runExecute calls Execute with run and stop, sends it the requests given and
// answers what it returned. The status channel is buffered past what one run
// reports, since nothing in a test reads it the way the SCM does.
func runExecute(t *testing.T, run func() bool, stop func(), send ...svc.Cmd) executeAnswer {
	t.Helper()

	requests := make(chan svc.ChangeRequest)
	status := make(chan svc.Status, 16)
	answered := make(chan executeAnswer, 1)

	go func() {
		specific, code := serviceHandler{run: run, stop: stop}.Execute(nil, requests, status)
		answered <- executeAnswer{specific: specific, code: code}
	}()

	for _, cmd := range send {
		select {
		case requests <- svc.ChangeRequest{Cmd: cmd}:
		case <-time.After(executeWait):
			t.Fatalf("Execute did not take the %v request", cmd)
		}
	}

	select {
	case answer := <-answered:
		return answer
	case <-time.After(executeWait):
		t.Fatalf("Execute did not return within %s", executeWait)
	}

	return executeAnswer{}
}

// TestExecuteReportsAnUninstallAsAStop is the case the failure actions must
// not see as a failure: the uninstall on the Settings screen removed the data
// and left the registration, and a failure is what they restart.
func TestExecuteReportsAnUninstallAsAStop(t *testing.T) {
	answer := runExecute(t, func() bool { return true }, func() {})

	if answer.specific || answer.code != 0 {
		t.Errorf("Execute after an uninstall returned (%v, %d), want (false, 0)", answer.specific, answer.code)
	}
}

// TestExecuteReportsARestartAsAFailure is the case beside it that has to stay
// as it was: a restart on this platform ends the run with nobody having asked
// the SCM, and coming back is left to the failure actions.
func TestExecuteReportsARestartAsAFailure(t *testing.T) {
	answer := runExecute(t, func() bool { return false }, func() {})

	if !answer.specific || answer.code != 1 {
		t.Errorf("Execute after a restart returned (%v, %d), want (true, 1)", answer.specific, answer.code)
	}
}

// TestExecuteReportsAStopFromTheSCMAsAStop covers the stop the SCM asked for,
// which is a stop whatever the run answers.
func TestExecuteReportsAStopFromTheSCMAsAStop(t *testing.T) {
	stopped := make(chan struct{})

	run := func() bool {
		<-stopped

		return false
	}

	answer := runExecute(t, run, func() { close(stopped) }, svc.Stop)

	if answer.specific || answer.code != 0 {
		t.Errorf("Execute after a stop returned (%v, %d), want (false, 0)", answer.specific, answer.code)
	}
}
