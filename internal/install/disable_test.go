package install

import (
	"errors"
	"strings"
	"testing"
)

// thisProcess stands in for os.Getpid in these tests. The number is not one a
// fake would answer by accident.
const thisProcess = 4242

func startedBy(managed bool, err error) func() (bool, error) {
	return func() (bool, error) {
		return managed, err
	}
}

// TestDisableThisServiceTurnsOffTheStartAtBootOfTheServiceItIs is the case the
// uninstall on the Settings screen is for: the service manager runs this very
// process, and what is asked of it is the start at boot and nothing more. A
// stop or an unregister here would end the process before its answer.
func TestDisableThisServiceTurnsOffTheStartAtBootOfTheServiceItIs(t *testing.T) {
	svc := &fakeService{servicePID: thisProcess}

	disabled, err := disableThisService(svc, startedBy(true, nil), thisProcess)
	if err != nil {
		t.Fatalf("disableThisService: %v", err)
	}

	if !disabled {
		t.Fatalf("the process the service manager runs was answered as not being the service")
	}

	if got := strings.Join(svc.calls, ","); got != "servicepid,disable" {
		t.Fatalf("the calls were %s, want servicepid,disable", got)
	}
}

// TestDisableThisServiceLeavesAServiceItIsNotAlone covers the copies of this
// program that are not the service: one started by hand beside a registered
// service, and one on a machine where nothing is registered or nothing is
// running. Neither has a start at boot of its own to turn off.
func TestDisableThisServiceLeavesAServiceItIsNotAlone(t *testing.T) {
	cases := []struct {
		name string
		pid  int
	}{
		{name: "another process is the service", pid: thisProcess + 1},
		{name: "no process of the service is running", pid: 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := &fakeService{servicePID: c.pid}

			disabled, err := disableThisService(svc, startedBy(true, nil), thisProcess)
			if err != nil {
				t.Fatalf("disableThisService: %v", err)
			}

			if disabled {
				t.Fatalf("a process that is not the service was answered as the service")
			}

			for _, call := range svc.calls {
				if call == "disable" {
					t.Fatalf("the start at boot of a service this process is not was turned off")
				}
			}
		})
	}
}

// TestDisableThisServiceDoesNotAskWhereNoServiceManagerCouldHaveStartedIt is a
// run in a container or by hand on a system with no service manager. The
// service manager is not asked at all, because asking one that is not there is
// a failure the screen would show for nothing.
func TestDisableThisServiceDoesNotAskWhereNoServiceManagerCouldHaveStartedIt(t *testing.T) {
	svc := &fakeService{servicePID: thisProcess}

	disabled, err := disableThisService(svc, startedBy(false, nil), thisProcess)
	if err != nil {
		t.Fatalf("disableThisService: %v", err)
	}

	if disabled || len(svc.calls) != 0 {
		t.Fatalf("answered %v after the calls %v, want false and no call", disabled, svc.calls)
	}
}

// TestDisableThisServiceReportsWhatWentWrong holds the three failures apart.
// Only the one that came from turning the start at boot off is answered as the
// service: that is the one where this process is known to be it, and the screen
// says the service is still enabled.
func TestDisableThisServiceReportsWhatWentWrong(t *testing.T) {
	refused := errors.New("refused")

	cases := []struct {
		name         string
		svc          *fakeService
		started      func() (bool, error)
		wantDisabled bool
	}{
		{
			name:    "whether a service manager started this could not be told",
			svc:     &fakeService{servicePID: thisProcess},
			started: startedBy(false, refused),
		},
		{
			name:    "the service manager could not be asked which process the service is",
			svc:     &fakeService{servicePIDErr: refused},
			started: startedBy(true, nil),
		},
		{
			name:         "turning the start at boot off failed",
			svc:          &fakeService{servicePID: thisProcess, disableErr: refused},
			started:      startedBy(true, nil),
			wantDisabled: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isService, err := disableThisService(c.svc, c.started, thisProcess)

			if !errors.Is(err, refused) {
				t.Fatalf("the error is %v, want it to carry %v", err, refused)
			}

			if isService != c.wantDisabled {
				t.Fatalf("answered %v for being the service, want %v", isService, c.wantDisabled)
			}
		})
	}
}
