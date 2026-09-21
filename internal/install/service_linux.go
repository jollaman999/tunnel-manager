//go:build linux

package install

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// init hands this package the systemd backend.
//
// Building one cannot fail: it holds nothing that has to be opened, and whether
// systemctl is on this machine at all is found out by the method that needs it,
// which is where it can be reported against the command that was being run. A
// backend that checked here would have to answer for a machine where systemd is
// fine and turn that into "installing is not implemented".
func init() {
	newService = func() (service, error) {
		return systemd{unit: serviceName + unitSuffix}, nil
	}
}

// unitSuffix is what systemd calls a service unit. It is spelled out because
// systemctl takes a bare name too and fills this in itself, while the file on
// disk has to carry it, and the two have to be the same name.
const unitSuffix = ".service"

// unitDir is where a unit written by this install goes when nothing is
// registered yet.
//
// It is /etc and not /usr/lib because /usr/lib belongs to the package manager
// of the distribution, the same reason the executable goes to /usr/local/bin.
// A unit that is already registered is written over where it already is, not
// here: see Register.
const unitDir = "/etc/systemd/system"

// stateDirectoryRoot is the one directory StateDirectory= can make under.
// systemd resolves the name against /var/lib and refuses a path, so a data
// directory anywhere else has to be made by this process instead.
const stateDirectoryRoot = "/var/lib"

// unitFileMode is what the unit is written as. It is read by systemd as root
// and by anybody looking at how the service is configured, and it holds no
// secret: the database it names is what is closed off, by dataDirMode.
const unitFileMode fs.FileMode = 0o644

// unitDirMode is for the directory the unit goes in, on the machine where it
// does not exist yet.
const unitDirMode fs.FileMode = 0o755

// How long the systemctl calls are given.
//
// Everything here runs with a deadline, because an install that hangs is worse
// than one that fails: the executable has already been written over by the time
// the service is registered, and an operator watching a command that never ends
// has no idea whether the machine is halfway through or done.
const (
	// commandTimeout covers the calls that only read or that rewrite systemd's
	// own state - show, daemon-reload, enable, is-active. None of them waits on
	// this service doing anything.
	commandTimeout = 30 * time.Second
	// jobTimeout covers start, stop and disable --now, which wait for a systemd
	// job to finish. It is above the TimeoutStopSec=90 of the unit on purpose:
	// a stop is allowed to take those ninety seconds before systemd kills the
	// process itself, and a deadline under that would kill the systemctl that
	// is waiting for a stop which is still going the way it was told to.
	jobTimeout = 2 * time.Minute
)

// How long Start waits for the service to actually be up.
//
// Start must not answer for a service it only asked to start, so it polls
// is-active. The poll is bounded by a count and not by a clock alone, so that
// it ends even where the sleep is the part that is not behaving.
const (
	startPollInterval = 200 * time.Millisecond
	startPollLimit    = 50
)

// systemd registers this program as a systemd service.
//
// The unit name is a field and not the serviceName constant read straight, so
// that the round trip test can drive all of this against a unit of its own.
// Nothing outside this package can set it: an uninstall is built through
// newService above and only ever gets the one name.
type systemd struct {
	unit string
}

// Current reads the registration out of systemd itself.
//
// There is no state file: the unit holds the path it is started from and the
// -db it passes, which is what the design settled on, and systemctl show hands
// both back.
func (s systemd) Current() (Installed, error) {
	// FragmentPath is empty for a unit systemd has never been given a file for,
	// and systemctl answers that without failing, so this tells a machine with
	// no registration apart from one that could not be asked. A failure below
	// is reported as a failure and never as ErrNotInstalled: an uninstall takes
	// that value as "nothing to remove" and would walk away from a service that
	// is still running.
	fragment, err := s.property("FragmentPath")
	if err != nil {
		return Installed{}, err
	}

	if fragment == "" {
		return Installed{}, ErrNotInstalled
	}

	execStart, err := s.property("ExecStart")
	if err != nil {
		return Installed{}, err
	}

	executable, database, err := parseExecStart(execStart)
	if err != nil {
		return Installed{}, fmt.Errorf("the %s unit at %s is registered but what it starts could not be read: %w",
			s.unit, fragment, err)
	}

	return Installed{
		ExecutablePath: executable,
		DatabaseFile:   database,
		DefinitionPath: fragment,
	}, nil
}

// Register writes the unit for plan and leaves the service registered, enabled
// and not started.
//
// It is enabled here because being registered is what this step is for, and on
// systemd a unit that is never enabled has no link from multi-user.target: it
// would be a service the operator installed that does not come back after a
// reboot. Enabling does not start anything, which is Start's to do.
func (s systemd) Register(plan Plan, existing *Installed) error {
	err := checkUnitPaths(plan)
	if err != nil {
		return err
	}

	// An existing registration is written over where it already lives. The unit
	// on this machine is in /usr/lib/systemd/system, and a new one dropped into
	// /etc/systemd/system would not replace it but shadow it: two units, /etc
	// winning, and the old file left behind still looking like the install.
	path := filepath.Join(unitDir, s.unit)
	if existing != nil && existing.DefinitionPath != "" {
		path = existing.DefinitionPath
	}

	if stateDirectoryName(plan.DataDir) == "" {
		// StateDirectory= cannot name this one, so nothing will make it when
		// the service starts and it is made here instead.
		err = os.MkdirAll(plan.DataDir, dataDirMode)
		if err != nil {
			return fmt.Errorf("failed to make the data directory %s: %w", plan.DataDir, err)
		}
	}

	err = os.MkdirAll(filepath.Dir(path), unitDirMode)
	if err != nil {
		return fmt.Errorf("failed to make the directory the unit goes in, %s: %w", filepath.Dir(path), err)
	}

	err = writeUnit(path, unitFile(plan))
	if err != nil {
		return err
	}

	// systemd reads units at startup and on this, so until it is run the file
	// above is not the registration of anything.
	_, err = s.run(commandTimeout, "daemon-reload")
	if err != nil {
		return err
	}

	_, err = s.run(commandTimeout, "enable", s.unit)
	if err != nil {
		return err
	}

	return nil
}

// Unregister takes the registration out of systemd.
//
// disable --now comes before the file goes, because disabling is what removes
// the links other targets hold to this unit, and systemd cannot work out what
// those are from a unit file that is not there any more. Then the file, then
// another reload, so that what systemd knows matches the disk again.
func (s systemd) Unregister(installed Installed) error {
	// Asked rather than assumed: disable fails on a unit systemd has no file
	// for, and a registration that is already gone is the state the caller
	// wanted, not a failure. Skipping the call is better than running it and
	// ignoring what it says, which would also swallow a stop that did not work.
	loaded, err := s.property("LoadState")
	if err != nil {
		return err
	}

	if loaded != "not-found" {
		_, err = s.run(jobTimeout, "disable", "--now", s.unit)
		if err != nil {
			return err
		}
	}

	if installed.DefinitionPath != "" {
		err = os.Remove(installed.DefinitionPath)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("failed to remove the unit %s: %w", installed.DefinitionPath, err)
		}
	}

	_, err = s.run(commandTimeout, "daemon-reload")
	if err != nil {
		return err
	}

	return nil
}

// Start starts the service and waits until it is up.
//
// systemctl start answers once the job it made has finished, and for a
// Type=simple unit that is the moment the process was forked, not the moment it
// is running. A process that dies on its own configuration - a database it
// cannot open, a port already taken - is forked just the same, so the state is
// asked for afterwards.
func (s systemd) Start() error {
	_, err := s.run(jobTimeout, "start", s.unit)
	if err != nil {
		return err
	}

	return s.waitActive()
}

// Stop stops the service and returns once it has stopped. systemctl stop waits
// for the job, and a unit that is already down is not an error to it either.
func (s systemd) Stop() error {
	_, err := s.run(jobTimeout, "stop", s.unit)
	if err != nil {
		return err
	}

	return nil
}

// waitActive polls until the service is up, it has clearly failed, or the poll
// has been made startPollLimit times.
func (s systemd) waitActive() error {
	state := ""

	for attempt := 0; attempt < startPollLimit; attempt++ {
		// is-active exits non-zero for every state that is not active, so the
		// state it prints is read and the exit status is not. A run that could
		// not be started at all leaves the state empty, and that is the error
		// to report, not the state.
		answer, err := s.run(commandTimeout, "is-active", s.unit)
		if answer == "" && err != nil {
			return err
		}

		state = answer

		switch state {
		case "active":
			return nil
		case "failed":
			// The unit gave up. Restart=always means a unit that is merely
			// crashing sits in activating instead, so reaching failed is the
			// end of the road and there is nothing to wait for.
			return s.notUpError(state)
		}

		time.Sleep(startPollInterval)
	}

	return s.notUpError(state)
}

// notUpError says the service did not come up, and where the reason for it is.
// The reason is in the journal and not in anything systemctl told this process,
// so the message points at it rather than pretending to know.
func (s systemd) notUpError(state string) error {
	return fmt.Errorf("%s was started but is %q. What went wrong is in: systemctl status %s and journalctl -u %s -e",
		s.unit, state, s.unit, s.unit)
}

// property reads one systemctl show property. --value is used so what comes
// back is the value alone, which is what the design document checked against
// the machine this runs on.
func (s systemd) property(name string) (string, error) {
	return s.run(commandTimeout, "show", s.unit, "-p", name, "--value")
}

// run calls systemctl and answers what it wrote to its standard output.
//
// The output is answered along with any error, because is-active says what it
// has to say on standard output and exits non-zero at the same time.
func (s systemd) run(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "systemctl", args...)

	stdout := &strings.Builder{}
	stderr := &strings.Builder{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	out := strings.TrimSpace(stdout.String())

	if err != nil {
		command := "systemctl " + strings.Join(args, " ")

		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return out, fmt.Errorf("%s did not finish within %s and was killed", command, timeout)
		}

		return out, fmt.Errorf("%s failed: %w%s", command, err, stderrSuffix(stderr.String()))
	}

	return out, nil
}

// stderrSuffix puts what the command complained about into the error. Without
// it the operator gets an exit status and has to go and run the command by hand
// to find out that, say, the unit file has a line systemd would not take.
func stderrSuffix(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}

	return ": " + strings.ReplaceAll(stderr, "\n", "; ")
}

// execStartArgv marks the command line inside what systemctl show -p ExecStart
// answers. The whole of that answer is a record with several fields, and this
// is the one that holds the executable and the arguments as they will be run:
//
//	{ path=/usr/local/bin/tunnel-manager ; argv[]=/usr/local/bin/tunnel-manager -db /var/lib/tunnel-manager/tunnel-manager.db ; ignore_errors=no ; ... }
const execStartArgv = "argv[]="

// execStartSeparator is what systemd puts between those fields, and so what
// ends the command line.
const execStartSeparator = " ; "

// parseExecStart pulls the executable path and the -db argument out of that
// record.
//
// This is the whole of how an uninstall knows what to remove, which is why it
// is a function of its own with the real output of the machine held against it
// in the test rather than something done inside Current.
func parseExecStart(value string) (string, string, error) {
	// Only the first line. A unit may carry several ExecStart lines and systemd
	// answers with one record each; the first is the one this install writes,
	// and guessing among the rest would be guessing.
	line := value
	if end := strings.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}

	start := strings.Index(line, execStartArgv)
	if start < 0 {
		return "", "", fmt.Errorf("systemctl answered with no %s in %q", execStartArgv, value)
	}

	argv := line[start+len(execStartArgv):]
	if end := strings.Index(argv, execStartSeparator); end >= 0 {
		argv = argv[:end]
	}

	fields := strings.Fields(argv)
	if len(fields) == 0 {
		return "", "", fmt.Errorf("systemctl answered with an empty command line in %q", value)
	}

	// An empty database is an answer and not a failure: a unit may start this
	// program without -db, and then the process works out its own default. What
	// an uninstall does with that is its own business.
	return fields[0], databaseArgument(fields[1:]), nil
}

// databaseArgument is the value of -db among args, empty when it is not there.
//
// Both spellings the flag package takes are read, -db value and -db=value, and
// the double dash form of each. What is written by Register is only ever the
// first, but what is read here is whatever unit is on the machine, including
// one an operator wrote by hand.
func databaseArgument(args []string) string {
	for i, arg := range args {
		name, value, assigned := strings.Cut(arg, "=")

		if name != "-db" && name != "--db" {
			continue
		}

		if assigned {
			return value
		}

		if i+1 < len(args) {
			return args[i+1]
		}

		// -db as the last word, with nothing after it. The unit would not start
		// at all, and there is no path in it to report.
		return ""
	}

	return ""
}

// checkUnitPaths refuses paths that would not survive the round trip through
// the unit.
//
// systemd splits ExecStart into words and has quoting rules of its own, and
// systemctl show hands the command line back as one string with the words
// separated by spaces. So a path with a space in it can be written into a unit
// that starts, but it cannot be read back out of one, and the uninstall that
// reads it would remove a path that is not the one installed. Refusing before
// anything is written is the honest answer; the alternative is an installation
// that cannot be removed.
func checkUnitPaths(plan Plan) error {
	if !filepath.IsAbs(plan.ExecutablePath) {
		return fmt.Errorf("the executable has to be named by an absolute path for a systemd unit, not %q",
			plan.ExecutablePath)
	}

	for _, path := range []struct {
		what  string
		value string
	}{
		{what: "executable", value: plan.ExecutablePath},
		{what: "database file", value: plan.DatabaseFile},
	} {
		if strings.ContainsAny(path.value, " \t\"'\\") {
			return fmt.Errorf("the %s path %q holds a space or a quote, which a systemd unit cannot carry "+
				"in a way an uninstall could read back. Choose a path without one",
				path.what, path.value)
		}
	}

	return nil
}

// stateDirectoryName is what StateDirectory= is set to for dataDir, and the
// empty string when systemd cannot be asked to make it.
//
// StateDirectory takes a name and resolves it under /var/lib itself; it refuses
// a path, and a directory anywhere else is simply not something it can make.
// Having systemd make it is worth keeping for the default, because then the
// directory exists and belongs to the service the moment the unit starts, with
// nothing having had to be made beforehand.
func stateDirectoryName(dataDir string) string {
	cleaned := filepath.Clean(dataDir)

	if filepath.Dir(cleaned) != stateDirectoryRoot {
		return ""
	}

	return filepath.Base(cleaned)
}

// unitFile is the unit written for plan.
//
// It is the same unit as _scripts/systemd/tunnel-manager.service, which is the
// one running on the deployed machines, with the paths coming from plan: an
// install that wrote a different unit than the one the deployment was tested
// against would be a second configuration nobody is watching.
func unitFile(plan Plan) string {
	b := &strings.Builder{}

	b.WriteString("# Written by " + serviceName + " -install. Anything changed here is written over by\n")
	b.WriteString("# the next install.\n")
	b.WriteString("[Unit]\n")
	b.WriteString("Description=Tunnel Manager Service\n")
	b.WriteString("After=network.target\n")
	b.WriteString("\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString("User=root\n")
	b.WriteString("ExecStart=" + execStartLine(plan) + "\n")

	// Named absolutely above rather than left to the default, which is worked
	// out from the user configuration directory and so depends on the HOME of
	// whoever the unit runs as. Everything else this installation owns - the
	// key, the log, the initial password - is read against the directory the
	// database is in, so this one path names all of it.
	if name := stateDirectoryName(plan.DataDir); name != "" {
		b.WriteString("StateDirectory=" + name + "\n")
	}

	b.WriteString("\n")
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=5\n")
	b.WriteString("TimeoutStopSec=90\n")
	b.WriteString("\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")

	return b.String()
}

// execStartLine is the command the unit starts. -db is left out when the plan
// has no database file, rather than written empty: the flag package would take
// the next word as its value, and a flag with nothing after it stops the
// process before it has started.
func execStartLine(plan Plan) string {
	if plan.DatabaseFile == "" {
		return plan.ExecutablePath
	}

	return plan.ExecutablePath + " -db " + plan.DatabaseFile
}

// writeUnit puts content at path.
//
// It writes beside the file and renames over it, the same way the executable is
// put in place, because the file it writes over may be the unit of a service
// that is running: a write that stopped halfway would leave systemd a unit it
// cannot parse, and the service could then not be started again by anybody. A
// rename either happened or did not.
func writeUnit(path string, content string) error {
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("failed to make a file beside %s to write the unit into: %w", path, err)
	}

	tempPath := temp.Name()

	_, err = temp.WriteString(content)
	if err != nil {
		_ = temp.Close()
		_ = os.Remove(tempPath)

		return fmt.Errorf("failed to write the unit to %s: %w", tempPath, err)
	}

	err = temp.Close()
	if err != nil {
		_ = os.Remove(tempPath)

		return fmt.Errorf("failed to finish writing %s: %w", tempPath, err)
	}

	// CreateTemp makes a file its owner alone can read, and a unit has to be
	// readable the way every other unit on the machine is.
	err = os.Chmod(tempPath, unitFileMode)
	if err != nil {
		_ = os.Remove(tempPath)

		return fmt.Errorf("failed to set the mode of %s: %w", tempPath, err)
	}

	err = os.Rename(tempPath, path)
	if err != nil {
		_ = os.Remove(tempPath)

		return fmt.Errorf("failed to put the unit at %s: %w", path, err)
	}

	return nil
}
