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
// finished doing so before the second attempt, and short enough that a tunnel
// is back before an operator has finished reading the alert. Three attempts,
// because a service that failed three times in a row is failing for a reason
// no further restart is going to change, and a reset period of a day, so that
// a machine which failed once a month is started every time rather than having
// used up its three attempts a year ago.
//
// Not verified on Windows: what the SCM does after the third action is exhausted
// is documented to be nothing until the reset period passes, but this has not
// been watched happen.
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
	// does UpdateConfig.
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

	opened, err := openService()

	switch {
	case errors.Is(err, ErrNotInstalled):
		return nil
	case err != nil:
		return err
	}

	defer opened.close()

	err = opened.service.Delete()
	if err != nil {
		// Already marked for deletion is the state that was being asked for.
		if errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			return nil
		}

		return fmt.Errorf("failed to delete the %s service: %w", serviceName, err)
	}

	return nil
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

// setFailureActions is the sc failure line of the design.
//
// The flag that would make these actions apply to a service which exited
// cleanly is deliberately not set. Left alone, the SCM restarts the service
// when it ends without having reported that it stopped, which is a crash, and
// leaves it down when it was stopped on purpose - and an uninstall stops it on
// purpose.
//
// Not verified on Windows: that the SCM took these values is only checked by
// the test beside this, which holds the values themselves.
func setFailureActions(handle windows.Handle) error {
	failure, actions := failureActions()

	err := windows.ChangeServiceConfig2(handle, windows.SERVICE_CONFIG_FAILURE_ACTIONS,
		(*byte)(unsafe.Pointer(&failure)))
	if err != nil {
		return fmt.Errorf("failed to set what the %s service does after it fails: %w", serviceName, err)
	}

	// The slice is held until the call has returned. The structure points into
	// it, and the SCM has read it by the time this returns.
	runtime.KeepAlive(actions)

	return nil
}
