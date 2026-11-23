//go:build windows

package install

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// None of this file has been run on Windows. It is built for windows from a
// Linux machine and held in place by the tests beside it, which cover the two
// parts that can be wrong without an operating system to say so: the command
// line the registration carries, and the configuration a registration is made
// with. Everything that talks to the service control manager - the calls
// themselves, the states they answer with, the timing of a start - is checked
// against the documented behaviour of those calls and nothing else, and every
// place where that is all there is says so.

// What the service is called where a person reads it. The name it is
// registered under is serviceName, which is what an uninstall looks it up by
// and is not shown anywhere.
const (
	serviceDisplayName = "Tunnel Manager"
	serviceDescription = "Keeps the configured SSH tunnels up and serves the tunnel-manager API and web UI."
)

// How the service is restarted after it fails, which is the design's
// "sc failure restart/5000" line.
//
// Five seconds is long enough that a machine which is still coming up has
// finished doing so before the next attempt, and short enough that a tunnel is
// back before an operator has finished reading the alert.
//
// The count of actions does not limit how often the service is started again.
// The documentation of SERVICE_FAILURE_ACTIONS says the SCM counts the failures
// since the boot, takes element [N-1] of the actions for the Nth failure, and
// "if N is greater than cActions, the service controller repeats the last
// action in the array"; the count goes back to 0 after the reset period passes
// without a failure. So every failure after the third is a restart as well,
// which is what lets the restart on the Settings screen be pressed any number
// of times a day: each press ends the process as a failure (main_windows.go).
// Three actions rather than one is what services.msc shows as its three rows,
// first, second and subsequent failures, all of them a restart.
//
// Not verified on Windows: the repeat of the last action is the documented
// behaviour and has not been watched happen past the third failure.
const (
	serviceRestartDelay       = 5 * time.Second
	serviceRestartActions     = 3
	serviceFailureResetPeriod = 24 * time.Hour
)

// How long a start or a stop is waited for, and how often it is asked about.
//
// The SCM answers a start as soon as it has created the process, and the state
// only becomes Running once the process has reported it, so there is nothing to
// wait on but the state. The limit is a count and not a deadline because the
// wait has to end even if the clock of the machine is moved while it runs: an
// install that never returns leaves an operator with no idea whether the
// service is up.
const (
	servicePollInterval = 250 * time.Millisecond
	servicePollLimit    = 120
)

func init() {
	newService = newSCMService
	startedByServiceManager = svc.IsWindowsService
}

// scmService registers the service with the Windows service control manager.
//
// It holds nothing. Every method opens its own connection to the SCM and closes
// it again, because an install is a handful of calls seconds apart and a handle
// kept open across them would be a handle held while the executable is copied,
// which is the longest step of all and the one most likely to fail.
type scmService struct{}

func newSCMService() (service, error) {
	return scmService{}, nil
}

// openedService is a service handle and the SCM connection it was opened
// through. Both have to be closed, and they have to be closed in this order.
type openedService struct {
	manager *mgr.Mgr
	service *mgr.Service
}

func (o openedService) close() {
	_ = o.service.Close()
	_ = o.manager.Disconnect()
}

// openService opens the service this program installs.
//
// It answers ErrNotInstalled only for the SCM saying the service does not
// exist. Every other failure is answered as itself: a connection that was
// refused for want of privilege, or a service that is there but could not be
// opened, is not "nothing is installed", and an uninstall that took it for that
// would walk away from a service that is still running.
func openService() (openedService, error) {
	manager, err := mgr.Connect()
	if err != nil {
		return openedService{}, fmt.Errorf("failed to connect to the service control manager: %w", err)
	}

	service, err := manager.OpenService(serviceName)
	if err != nil {
		_ = manager.Disconnect()

		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return openedService{}, ErrNotInstalled
		}

		return openedService{}, fmt.Errorf("failed to open the %s service: %w", serviceName, err)
	}

	return openedService{manager: manager, service: service}, nil
}

// Current reads the registration out of the SCM.
func (scmService) Current() (Installed, error) {
	opened, err := openService()
	if err != nil {
		return Installed{}, err
	}

	defer opened.close()

	config, err := opened.service.Config()
	if err != nil {
		return Installed{}, fmt.Errorf("the %s service is registered but reading its configuration failed: %w",
			serviceName, err)
	}

	executable, database, err := parseServiceCommand(config.BinaryPathName)
	if err != nil {
		return Installed{}, err
	}

	// DefinitionPath is left empty on purpose. Windows keeps the registration
	// in the registry under the service name and there is no file to name, and
	// install.go says so where Installed is declared.
	return Installed{ExecutablePath: executable, DatabaseFile: database}, nil
}

// OpenFiles answers that the files this service has open cannot be read here.
//
// Windows has nothing like /proc/<pid>/fd. Enumerating the handles of another
// process means NtQuerySystemInformation with SystemExtendedHandleInformation,
// walking every handle on the machine and turning the object names of those
// that are files back into paths - an undocumented interface and a fair amount
// of code, for a question this platform does not need to ask: the registration
// carries the whole command line, -install always writes -db into it, and
// parseServiceCommand reads it back. So the caller is told the answer is not
// available rather than being handed a guess, and it falls back on what the
// registration says.
func (scmService) OpenFiles() ([]string, error) {
	return nil, fmt.Errorf("%w: Windows keeps no such list for another process. What this "+
		"installation is made of comes from the command line the service is registered with",
		ErrOpenFilesUnknown)
}

// CheckPlan has nothing to refuse on this platform, so it answers nil.
//
// The command line the SCM keeps is quoted with syscall.EscapeArg and read back
// by parseServiceCommand, which handles that quoting: the default install lives
// under Program Files, so a path with a space in it is the ordinary case here
// rather than one that has to be kept out.
func (scmService) CheckPlan(Plan) error {
	return nil
}

// Register writes the registration for plan.
//
// An existing registration is changed rather than deleted and made again.
// A deleted service is only gone once every handle to it is closed, and while
// the SCM still holds one the name cannot be used again, so a delete followed by
// a create is a registration that fails to be made on a machine where anything -
// services.msc among them - happens to have the old one open. Changing the
// configuration has no such moment.
func (scmService) Register(plan Plan, existing *Installed) error {
	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to the service control manager: %w", err)
	}

	defer func() {
		_ = manager.Disconnect()
	}()

	config := serviceConfig(plan)
	arguments := serviceArguments(plan)

	var service *mgr.Service

	if existing == nil {
		// The executable and the arguments go over apart here because
		// CreateService takes them that way and escapes them itself. What it
		// writes is the same string serviceConfig put in BinaryPathName, since
		// both are syscall.EscapeArg over the same parts.
		service, err = manager.CreateService(serviceName, plan.ExecutablePath, config, arguments...)
		if err != nil {
			return fmt.Errorf("failed to create the %s service: %w", serviceName, err)
		}
	} else {
		service, err = manager.OpenService(serviceName)
		if err != nil {
			return fmt.Errorf("failed to open the %s service to write over its registration: %w", serviceName, err)
		}

		err = service.UpdateConfig(config)
		if err != nil {
			_ = service.Close()

			return fmt.Errorf("failed to change the registration of the %s service: %w", serviceName, err)
		}
	}

	defer func() {
		_ = service.Close()
	}()

	// The failure actions are set after the service exists, because they are a
	// second call either way: CreateService does not carry them and neither
	// does UpdateConfig. They are set on every registration, a written over one
	// included, so that a registration an earlier release made without the
	// flag in recoverySettings gets it from the next -install or update.
	err = setFailureActions(service.Handle)
	if err != nil {
		return err
	}

	// The service is left registered and not started. Start is what starts it,
	// and the flow above reads the registration back in between.
	return nil
}

// Unregister deletes the registration.
//
// The Installed it is handed is not used. There is one registration per name on
// this platform and it is in the registry rather than at a path, so what was
// read out of it says nothing about where it has to be removed from.
func (s scmService) Unregister(_ Installed) error {
	// Stopped first. A service deleted while it runs is marked for deletion and
	// stays registered until the process ends, and an operator who ran an
	// uninstall would be left with a service that is still serving.
	err := s.Stop()
	if err != nil {
		return err
	}

	return deleteService()
}

// UnregisterSelf deletes the registration of the service whose process is the
// one asking, and does not stop it.
//
// DeleteService does not take a running service away. Its documentation says
// it "marks a service for deletion from the service control manager database.
// The database entry is not removed until all open handles to the service have
// been closed by calls to the CloseServiceHandle function, and the service is
// not running." So the entry stays while this process answers the uninstall,
// and goes when the process ends and the handler in main_windows.go reports
// the service stopped; this one's own handle is closed before that, here. Until
// then the name is taken, and an install that runs in between is refused by
// CreateService with ERROR_SERVICE_MARKED_FOR_DELETE. (The documented
// behaviour of DeleteService, not run on Windows.)
func (scmService) UnregisterSelf(_ Installed) error {
	return deleteService()
}

// deleteService marks the registration for deletion and closes the handle it
// opened to do so. A registration that is not there, or is already marked, is
// the state that was asked for.
func deleteService() error {
	opened, err := openService()

	switch {
	case errors.Is(err, ErrNotInstalled):
		return nil
	case err != nil:
		return err
	}

	defer opened.close()

	return deletedAnswer(opened.service.Delete())
}

// deletedAnswer is what DeleteService answered, read as Unregister and
// UnregisterSelf answer it, apart so that a test holds it without an SCM.
func deletedAnswer(err error) error {
	if err == nil || errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		return nil
	}

	return fmt.Errorf("failed to delete the %s service: %w", serviceName, err)
}

// Start starts the service and waits for it to report that it is running.
func (scmService) Start() error {
	opened, err := openService()
	if err != nil {
		return err
	}

	defer opened.close()

	err = opened.service.Start()
	if err != nil {
		// Already running is the state that was being asked for. The wait below
		// sees it straight away.
		if !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			return fmt.Errorf("failed to start the %s service: %w", serviceName, err)
		}
	}

	return awaitState(opened.service, svc.Running)
}

// Stop stops the service and waits for it to report that it has stopped.
//
// A service that is not running, and a service that is not registered at all,
// are both the state the caller was asking for.
func (scmService) Stop() error {
	opened, err := openService()

	switch {
	case errors.Is(err, ErrNotInstalled):
		return nil
	case err != nil:
		return err
	}

	defer opened.close()

	status, err := opened.service.Control(svc.Stop)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return nil
		}

		return fmt.Errorf("failed to ask the %s service to stop: %w", serviceName, err)
	}

	if status.State == svc.Stopped {
		return nil
	}

	return awaitState(opened.service, svc.Stopped)
}

// ServicePID answers the process id the SCM reports for the service, which it
// reports as 0 while the service is not running. A service that is not
// registered is not running either.
func (scmService) ServicePID() (int, error) {
	opened, err := openService()

	switch {
	case errors.Is(err, ErrNotInstalled):
		return 0, nil
	case err != nil:
		return 0, err
	}

	defer opened.close()

	status, err := opened.service.Query()
	if err != nil {
		return 0, fmt.Errorf("failed to read the state of the %s service: %w", serviceName, err)
	}

	return int(status.ProcessId), nil
}

// DisableAtBoot sets the start type of the service to disabled and changes
// nothing else about it.
//
// ChangeServiceConfig is called straight rather than through UpdateConfig,
// which takes a whole configuration and writes every field of it back. A
// disabled service that is running goes on running: the start type is read
// when something asks the SCM to start the service, and the process asking is
// the service, which ends a moment later and is reported to the SCM as having
// stopped. Register writes the automatic start back. (The documented behaviour
// of ChangeServiceConfig, not run on Windows.)
func (scmService) DisableAtBoot() error {
	opened, err := openService()
	if err != nil {
		return err
	}

	defer opened.close()

	change := disabledAtBoot()

	err = windows.ChangeServiceConfig(opened.service.Handle, change.serviceType, change.startType,
		change.errorControl, nil, nil, nil, nil, nil, nil, nil)
	if err != nil {
		return fmt.Errorf("failed to set the start type of the %s service to disabled: %w", serviceName, err)
	}

	return nil
}

// startTypeChange is the three numbers ChangeServiceConfig is handed ahead of
// its strings, which are all left out (nil) here.
type startTypeChange struct {
	serviceType  uint32
	startType    uint32
	errorControl uint32
}

// disabledAtBoot is what DisableAtBoot hands ChangeServiceConfig, apart so
// that a test holds it without an SCM. SERVICE_NO_CHANGE is how that call is
// told to leave a number as it is.
func disabledAtBoot() startTypeChange {
	return startTypeChange{
		serviceType:  windows.SERVICE_NO_CHANGE,
		startType:    windows.SERVICE_DISABLED,
		errorControl: windows.SERVICE_NO_CHANGE,
	}
}

// awaitState polls the service until it reports want.
//
// The number of turns is bounded rather than the time, for the reason given
// where servicePollLimit is declared. What is reported when it runs out names
// the state the service was left in, because that is the difference between a
// service that is still starting and one that never moved.
func awaitState(service *mgr.Service, want svc.State) error {
	var status svc.Status

	for try := 0; try < servicePollLimit; try++ {
		if try > 0 {
			time.Sleep(servicePollInterval)
		}

		var err error

		status, err = service.Query()
		if err != nil {
			return fmt.Errorf("failed to read the state of the %s service: %w", serviceName, err)
		}

		if status.State == want {
			return nil
		}

		// A service that is Stopped while it was asked to start did not fail to
		// come up slowly: it came up and ended, and waiting the rest of the
		// limit out would only delay the exit code that says why. The first turn
		// is left out of this, because the SCM sets the state as it creates the
		// process and a query that arrived before it did would read the state
		// the service had beforehand.
		//
		// Not verified on Windows: which of the two the first query sees is a
		// race this has never been run against.
		if want == svc.Running && status.State == svc.Stopped && try > 0 {
			return fmt.Errorf("the %s service was started and stopped again straight away, "+
				"with exit code %d. Its log says why", serviceName, status.Win32ExitCode)
		}
	}

	return fmt.Errorf("the %s service was still in state %d after %s, giving up waiting for state %d",
		serviceName, status.State, time.Duration(servicePollLimit)*servicePollInterval, want)
}

// serviceConfig is what the service is registered with.
//
// ServiceStartName is left empty, which is how the SCM is told LocalSystem: the
// design's account column for Windows. A name would have to come with a
// password, and a password for an account this installs would have to be
// invented here and stored somewhere.
func serviceConfig(plan Plan) mgr.Config {
	return mgr.Config{
		ServiceType:  windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
		// CreateService pays no attention to this field and builds the line
		// itself out of the executable and the arguments it is handed. It is put
		// here all the same, because UpdateConfig takes the finished line, and
		// one place deciding what the registration starts is what keeps a
		// registration that was written over saying the same as a new one.
		BinaryPathName: commandLineFor(plan.ExecutablePath, serviceArguments(plan)),
		DisplayName:    serviceDisplayName,
		Description:    serviceDescription,
	}
}

// serviceArguments is what the registration passes the executable.
//
// The database path is the whole of it. Everything else this process needs is
// read out of the database, and the path of the database is what an uninstall
// reads back out of the registration.
func serviceArguments(plan Plan) []string {
	return []string{databaseFlag, plan.DatabaseFile}
}

// databaseFlag is the flag main takes the database path through, spelled here
// because this is what writes it into the registration and parseServiceCommand
// is what reads it back.
const databaseFlag = "-db"

// commandLineFor puts an executable and its arguments together the way the SCM
// keeps them, which is one string quoted as a Windows command line.
//
// It is the same syscall.EscapeArg that mgr.CreateService uses to build the
// line it writes, so a registration made by creating the service and one made
// by changing an existing one carry the same string. Doing it by hand is only
// necessary because UpdateConfig takes the finished line and CreateService takes
// the parts.
func commandLineFor(executable string, arguments []string) string {
	line := syscall.EscapeArg(executable)

	for _, argument := range arguments {
		line += " " + syscall.EscapeArg(argument)
	}

	return line
}

// parseServiceCommand reads an executable path and a -db argument back out of
// the command line a registration carries.
//
// The path needs the quoting handled and not merely stripped: the default
// install is under Program Files, so the line reads
//
//	"C:\Program Files\tunnel-manager\tunnel-manager.exe" -db C:\ProgramData\...
//
// and splitting that on spaces answers with C:\Program as the executable, which
// is a path an uninstall would then not find anything at.
func parseServiceCommand(line string) (executable string, database string, err error) {
	arguments := splitCommandLine(line)
	if len(arguments) == 0 {
		return "", "", fmt.Errorf("the %s service is registered with an empty command line, "+
			"so there is no way to tell what it starts", serviceName)
	}

	executable = arguments[0]

	for i := 1; i < len(arguments); i++ {
		argument := arguments[i]

		// Both spellings of the flag and both ways of giving it a value are
		// read. What this writes is the two-token form with one dash, but a
		// registration made by hand with sc.exe is just as likely to carry
		// -db=C:\... , and an uninstall that did not recognise it would report
		// no database and leave the file behind.
		for _, flag := range []string{databaseFlag, "-" + databaseFlag} {
			if argument == flag && i+1 < len(arguments) {
				return executable, arguments[i+1], nil
			}

			if value, found := strings.CutPrefix(argument, flag+"="); found {
				return executable, value, nil
			}
		}
	}

	// No -db in the registration. That is a registration which lets the process
	// work out its own default, and Installed says an empty DatabaseFile means
	// exactly that.
	return executable, "", nil
}

// splitCommandLine splits a Windows command line into its arguments.
//
// The rules are the ones CommandLineToArgvW applies and syscall.EscapeArg
// writes for: a quote opens and closes a stretch where spaces are part of the
// argument, a run of backslashes is only special in front of a quote, and there
// 2n of them mean n backslashes with the quote doing its work while 2n+1 mean n
// backslashes and a quote that is part of the argument. Backslashes anywhere
// else are what every Windows path is full of and are left alone.
//
// CommandLineToArgvW reads the first argument by a simpler rule of its own, in
// which backslashes are never escapes. It makes no difference to what this
// reads back: what wrote the line is EscapeArg, which does not produce the
// forms the two rules disagree about for any path a file can be at.
//
// An unquoted path with a space in it cannot be told from a path followed by an
// argument, by this or by Windows itself, and comes back as the several
// arguments it looks like.
func splitCommandLine(line string) []string {
	var (
		arguments   []string
		current     strings.Builder
		started     bool
		quoted      bool
		backslashes int
	)

	// The bytes are walked rather than the runes. Every byte this has to treat
	// specially is ASCII, and a byte of a multi-byte character is never one of
	// them, so a path in any language comes through whole.
	for i := 0; i < len(line); i++ {
		switch character := line[i]; character {
		case '\\':
			backslashes++
			started = true
		case '"':
			current.WriteString(strings.Repeat(`\`, backslashes/2))

			if backslashes%2 == 1 {
				current.WriteByte('"')
			} else {
				quoted = !quoted
			}

			backslashes = 0
			started = true
		case ' ', '\t':
			current.WriteString(strings.Repeat(`\`, backslashes))
			backslashes = 0

			if quoted {
				current.WriteByte(character)

				break
			}

			if started {
				arguments = append(arguments, current.String())
				current.Reset()

				started = false
			}
		default:
			current.WriteString(strings.Repeat(`\`, backslashes))
			backslashes = 0

			current.WriteByte(character)
			started = true
		}
	}

	current.WriteString(strings.Repeat(`\`, backslashes))

	if started {
		arguments = append(arguments, current.String())
	}

	return arguments
}

// failureActions is what the service is told to do when it fails.
//
// The actions are answered alongside the structure that points at them so that
// the caller holds both: the structure carries a pointer into the slice, and a
// caller that kept only the structure would be handing the SCM a pointer to an
// array nothing refers to.
func failureActions() (windows.SERVICE_FAILURE_ACTIONS, []windows.SC_ACTION) {
	actions := make([]windows.SC_ACTION, serviceRestartActions)

	for i := range actions {
		actions[i] = windows.SC_ACTION{
			Type: windows.SC_ACTION_RESTART,
			// The delay of an action is milliseconds, while the reset period
			// below is seconds. They are two fields of one structure with two
			// units, which is why each is spelled out of a Duration rather than
			// written as a number.
			Delay: uint32(serviceRestartDelay / time.Millisecond),
		}
	}

	return windows.SERVICE_FAILURE_ACTIONS{
		ResetPeriod:  uint32(serviceFailureResetPeriod / time.Second),
		ActionsCount: uint32(len(actions)),
		Actions:      &actions[0],
	}, actions
}

// failureActionsFlag is what has the failure actions apply to a service that
// reported it stopped with an exit code other than 0, as well as to one whose
// process ended without reporting anything.
//
// Without it the restart on the Settings screen leaves the service down. That
// restart ends the process with the service reported stopped and a
// service-specific exit code of 1 (main_windows.go), and the documentation of
// SERVICE_FAILURE_ACTIONS_FLAG says that while fFailureActionsOnNonCrashFailures
// is FALSE "the failure actions are queued only if the service process
// terminates without reporting a status of SERVICE_STOPPED". This was watched
// happen on Windows: the service stayed STOPPED with WIN32_EXIT_CODE 1066 and
// SERVICE_EXIT_CODE 1, and the SCM logged event 7024 and started nothing.
//
// With it TRUE the actions are queued also "if it enters the SERVICE_STOPPED
// state but the dwWin32ExitCode member of the SERVICE_STATUS structure is not
// ERROR_SUCCESS (0)". Every stop that is meant to leave the service down
// reports 0, so none of them is restarted: a stop or a shutdown the SCM asked
// for, and the uninstall on the Settings screen, all end in Execute answering
// (false, 0), which the svc package reports as NO_ERROR. A stop that the SCM
// asked for is not a failure in the first place, whatever it ends with.
// (The documented behaviour of that flag, not watched happen with it set.)
func failureActionsFlag() windows.SERVICE_FAILURE_ACTIONS_FLAG {
	return windows.SERVICE_FAILURE_ACTIONS_FLAG{FailureActionsOnNonCrashFailures: 1}
}

// serviceSetting is one ChangeServiceConfig2 call: the information level and
// the structure handed with it.
//
// The structure is carried as itself rather than only as the pointer the call
// takes, so that a test reads the values a registration is made with, and so
// that the memory the pointer points into is held by the same value that
// carries it.
type serviceSetting struct {
	infoLevel uint32
	failure   *windows.SERVICE_FAILURE_ACTIONS
	actions   []windows.SC_ACTION
	flag      *windows.SERVICE_FAILURE_ACTIONS_FLAG
}

// pointer is what ChangeServiceConfig2 is handed for this setting.
func (s serviceSetting) pointer() *byte {
	if s.flag != nil {
		return (*byte)(unsafe.Pointer(s.flag))
	}

	return (*byte)(unsafe.Pointer(s.failure))
}

// recoverySettings is every ChangeServiceConfig2 call a registration makes, in
// the order it makes them: the actions, and then the flag that has them apply
// to the restart on the Settings screen. The flag is set after the actions
// because it says when they are taken, and a flag with no actions configured
// has nothing to queue.
func recoverySettings() []serviceSetting {
	failure, actions := failureActions()
	flag := failureActionsFlag()

	return []serviceSetting{
		{infoLevel: windows.SERVICE_CONFIG_FAILURE_ACTIONS, failure: &failure, actions: actions},
		{infoLevel: windows.SERVICE_CONFIG_FAILURE_ACTIONS_FLAG, flag: &flag},
	}
}

// setFailureActions is the sc failure and the sc failureflag lines of the
// design.
//
// Not verified on Windows: that the SCM took these values is only checked by
// the test beside this, which holds the values themselves.
func setFailureActions(handle windows.Handle) error {
	settings := recoverySettings()

	for _, setting := range settings {
		err := windows.ChangeServiceConfig2(handle, setting.infoLevel, setting.pointer())
		if err != nil {
			return fmt.Errorf("failed to set what the %s service does after it fails (level %d): %w",
				serviceName, setting.infoLevel, err)
		}
	}

	// The settings are held until every call has returned. Each structure
	// points into memory the settings own, and the SCM has read it by the time
	// the call returns.
	runtime.KeepAlive(settings)

	return nil
}

// RestartsAfterFailure answers whether the SCM starts the service again once
// its process has reported it stopped with an exit code other than 0, which is
// how the restart on the Settings screen ends the process on this platform.
//
// It is read out of the registration rather than assumed from what Register
// writes, because a registration an earlier release made carries the actions
// without the flag, and that one leaves the service down after the restart
// until -install or an update writes it again. The screen says beforehand
// whether the service comes back, and it has to say what this registration
// does and not what a new one would.
//
// It is asked by the process that is the service, which runs as LocalSystem
// and may read its own registration.
func RestartsAfterFailure() (bool, error) {
	opened, err := openService()
	if err != nil {
		return false, err
	}

	defer opened.close()

	flag, err := opened.service.RecoveryActionsOnNonCrashFailures()
	if err != nil {
		return false, fmt.Errorf("failed to read when the failure actions of the %s service apply: %w",
			serviceName, err)
	}

	actions, err := opened.service.RecoveryActions()
	if err != nil {
		return false, fmt.Errorf("failed to read the failure actions of the %s service: %w", serviceName, err)
	}

	return restartsAfterFailure(flag, actions), nil
}

// restartsAfterFailure is RestartsAfterFailure once the two settings have been
// read, apart so that a test holds it without an SCM.
//
// Every action has to be a restart, and not only the first. Which action is
// taken depends on how many failures there have been since the count was last
// reset (see serviceRestartActions), and a restart pressed for the third time
// in a day is answered by the third action and every one after by the last.
func restartsAfterFailure(flag bool, actions []mgr.RecoveryAction) bool {
	if !flag || len(actions) == 0 {
		return false
	}

	for _, action := range actions {
		if action.Type != mgr.ServiceRestart {
			return false
		}
	}

	return true
}
