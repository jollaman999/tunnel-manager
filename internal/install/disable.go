package install

import (
	"errors"
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

// RemovedPart is what became of one part of the service RemoveThisService
// takes away: the registration, the executable or the data directory.
type RemovedPart struct {
	// Path names the part. For the registration it is the unit or the plist,
	// and on Windows, where the registration is in the registry and there is
	// no file to name, it is the name of the service.
	Path string
	// Removed says the part went. AtReboot says it goes at the next restart of
	// the system instead, which only the executable on Windows answers: see
	// removeExecutable there.
	Removed  bool
	AtReboot bool
	// Kept says the part was left in place on purpose, and Err then says why.
	Kept bool
	// Err is what went wrong removing the part, or the reason it was kept.
	Err error
}

// ServiceRemoval is what RemoveThisService did to the service this process
// runs as.
type ServiceRemoval struct {
	// IsService says a service manager runs this process as the service of
	// this installation. Where it is false the parts below are left as they
	// were and say nothing.
	IsService bool
	// DisableErr is what went wrong turning the start at boot off, or telling
	// whether this process is the service at all.
	DisableErr error
	// The three parts, in the order they go.
	Registration RemovedPart
	Executable   RemovedPart
	DataDir      RemovedPart
}

// RemoveThisService takes the service this process runs as off the system:
// its start at boot, its registration, its executable and the directory its
// data was in. databaseFile is the database this process opened, which names
// that directory. It is what the uninstall on the Settings screen ends with,
// once the files it names are gone, and what it leaves is what -uninstall
// -purge leaves.
//
// The service is not stopped, and that is the difference from -uninstall,
// whose pieces this is otherwise made of. The service is this process, and
// stopping it from inside would end the process before the answer to the
// uninstall was written. So the registration is taken away from under a
// process that is still running, the way each backend's UnregisterSelf
// describes, and the exit that follows the answer is what takes it down
// (UninstalledExitCode).
//
// The start at boot is turned off first even though the registration goes a
// moment later. On systemd the links that start the unit at boot can only be
// worked out from the unit file, which is gone after, and a registration that
// could not be taken out is then at least one that does not start an empty
// installation on the next boot, whose account is set up by whoever reaches
// the page first.
//
// A process that is not the service - started by hand, in a container, or a
// second copy beside a registered one - has nothing of this to remove, and
// that is answered with IsService false and no error. Nothing registers it, so
// its executable and its data directory are not known to be an installation:
// a run by hand may be a build in a working copy with its database in the
// directory it was run from. Only the files the uninstall names go for it.
func RemoveThisService(databaseFile string) ServiceRemoval {
	if newService == nil || startedByServiceManager == nil {
		return ServiceRemoval{}
	}

	svc, err := newService()
	if err != nil {
		return ServiceRemoval{DisableErr: err}
	}

	return removeThisService(svc, startedByServiceManager, os.Getpid(), databaseFile)
}

// removeThisService is RemoveThisService with the backend, the check and the
// process id handed in, which is what the tests drive.
//
// A part that cannot go does not stop the ones after it, the same way a file
// that cannot be removed does not stop the uninstall on the Settings screen:
// the data is gone already, and what is left is reported part by part. The one
// exception is the executable, which stays while the registration does.
func removeThisService(svc service, started func() (bool, error), self int, databaseFile string) ServiceRemoval {
	isService, err := disableThisService(svc, started, self)

	removal := ServiceRemoval{IsService: isService, DisableErr: err}

	if !isService {
		return removal
	}

	// Read before anything is taken out, since the registration is the record
	// of where the executable is and is gone afterwards.
	installed, err := svc.Current()
	if err != nil {
		removal.Registration = RemovedPart{
			Err: fmt.Errorf("failed to read the registration of the %s service: %w", serviceName, err),
		}
	} else {
		removal.Registration = unregisterSelf(svc, installed)
	}

	removal.Executable = removeOwnExecutable(installed.ExecutablePath, removal.Registration)
	removal.DataDir = removeDataDir(databaseFile)

	return removal
}

// disableThisService turns off the start at boot of the service this process
// runs as, and answers whether this process is the service.
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

// unregisterSelf takes the registration out without stopping the service.
func unregisterSelf(svc service, installed Installed) RemovedPart {
	part := RemovedPart{Path: installed.DefinitionPath}
	if part.Path == "" {
		part.Path = serviceName
	}

	err := svc.UnregisterSelf(installed)
	if err != nil {
		part.Err = fmt.Errorf("failed to take the service registration out: %w", err)

		return part
	}

	part.Removed = true

	return part
}

// errExecutableKept is why the executable stays when the registration does.
// It is the program -uninstall is run with, and -uninstall is what takes out a
// registration this could not, so removing it would leave the operator a
// registration and nothing on the system to remove it with.
var errExecutableKept = errors.New("kept because the service registration is still there: " +
	"this is the program that runs -uninstall, which removes both")

// removeOwnExecutable removes the executable the registration starts, which
// is the one this process is running from. Unix removes the name and leaves
// the file to the process that has it open, and Windows, which refuses to
// delete an image that is running, is handed the file to delete at its next
// restart: removeExecutable is the call -uninstall makes for both.
func removeOwnExecutable(path string, registration RemovedPart) RemovedPart {
	part := RemovedPart{Path: path}

	switch {
	case !registration.Removed:
		part.Kept = true
		part.Err = errExecutableKept

		return part
	case path == "":
		part.Kept = true
		part.Err = errors.New("the service registration names no executable")

		return part
	}

	atReboot, err := removeExecutable(path)
	if err != nil {
		part.Err = err

		return part
	}

	part.Removed = true
	part.AtReboot = atReboot

	return part
}

// removeDataDir removes the directory the database was in, with what is left
// under it. It is the directory -uninstall -purge removes, and it is checked
// the way -purge checks it first (checkPurgeTarget): a directory that holds
// more than this installation is kept, and the refusal is the reason.
func removeDataDir(databaseFile string) RemovedPart {
	dataDir := dataDirOf(databaseFile)
	part := RemovedPart{Path: dataDir}

	err := checkPurgeTarget(dataDir, databaseFile)
	if err != nil {
		part.Kept = true
		part.Err = err

		return part
	}

	err = os.RemoveAll(dataDir)
	if err != nil {
		part.Err = fmt.Errorf("failed to remove the data directory %s: %w", dataDir, err)

		return part
	}

	part.Removed = true

	return part
}
