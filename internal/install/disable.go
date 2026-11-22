package install

import (
	"fmt"
	"os"
)

// startedByServiceManager answers whether this process could have been started
// by the service manager a backend registers with at all. Each backend sets it
// from its init, next to newService.
//
// It is asked before the service manager is, so that a process nobody
// registered - one run by hand, one in a container - is not answered with a
// failure to reach a service manager it never had. Where it is nil, which is a
// platform with no backend, nothing could have registered this process.
var startedByServiceManager func() (bool, error)

// DisableThisService keeps the service manager from starting this
// installation again when the system boots, when this process is the one the
// service manager runs for it. It answers whether it was, and what went wrong
// turning the start at boot off.
//
// It is what the uninstall on the Settings screen ends with. That uninstall
// removes the data and leaves the registration and the executable, which
// -uninstall removes, and a registration that is still enabled starts an empty
// installation with a new initial password on the next boot, whose account is
// set up by whoever reaches the page first. So the start at boot is turned off
// and nothing else is touched: the service is not stopped, because it is this
// process, and stopping it from inside would end the process before the answer
// to the uninstall was written. The exit that follows is what takes it down
// (UninstalledExitCode).
//
// A process that is not the service - started by hand, in a container, or a
// second copy beside a registered one - has nothing to turn off, and that is
// answered as false with no error: the registration, if there is one, belongs
// to another process.
func DisableThisService() (bool, error) {
	if newService == nil || startedByServiceManager == nil {
		return false, nil
	}

	svc, err := newService()
	if err != nil {
		return false, err
	}

	return disableThisService(svc, startedByServiceManager, os.Getpid())
}

// disableThisService is DisableThisService with the backend, the check and the
// process id handed in, which is what the tests drive.
func disableThisService(svc service, started func() (bool, error), self int) (bool, error) {
	managed, err := started()
	if err != nil {
		return false, fmt.Errorf("failed to tell whether a service manager started this process: %w", err)
	}

	if !managed {
		return false, nil
	}

	// The process id is compared rather than the registration merely looked
	// for. A registration being there says that some process is the service,
	// and this one may be a copy somebody started by hand beside it, whose
	// uninstall must not turn off a service it does not run as.
	pid, err := svc.ServicePID()
	if err != nil {
		return false, fmt.Errorf("failed to ask the service manager which process the %s service is: %w",
			serviceName, err)
	}

	if pid != self {
		return false, nil
	}

	err = svc.DisableAtBoot()
	if err != nil {
		return true, err
	}

	return true, nil
}
