//go:build darwin

// The launchd backend of the install. This installation is one LaunchDaemon:
// a plist under /Library/LaunchDaemons, which is where launchd takes jobs that
// run as root from, bootstrapped into the system domain.
//
// None of this was run on a Mac. It was written and checked on Linux with the
// package cross compiled, so what is held down instead is the two things that
// can be held down from here: the plist that is written, and the launchctl
// argv of every method. Both are frozen in service_darwin_test.go, so a change
// to either is seen without a Mac. Everything that could only be learned by
// running launchctl - its exit codes, the wording of launchctl print - is
// marked as unchecked where it is relied on, and nothing here is written so
// that a wrong guess about it passes silently: what is not understood is
// reported with the launchctl output beside it.
package install

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The names launchd knows this installation by.
//
// The label is in reverse domain form because that is what launchd expects of
// a job, and it carries the repository this program is released from, so that
// it cannot collide with another tunnel-manager somebody else ships. The plist
// is named after the label because launchctl bootstrap takes the file and the
// label has to agree with it, and a file named anything else is one more thing
// to keep in step.
const (
	launchdLabel     = "io.github.jollaman999." + serviceName
	launchdPlistPath = "/Library/LaunchDaemons/" + launchdLabel + ".plist"
	// launchdDomain is the domain the job is bootstrapped into. It is the
	// system domain and not a user one because this service runs as root and
	// has to be up with no session logged in.
	launchdDomain = "system"
	// launchdTarget is how the job is named once it is in that domain, which
	// is what bootout and print are given.
	launchdTarget = launchdDomain + "/" + launchdLabel
)

// launchctlAbsolutePath is where launchctl is on a Mac.
//
// It is named absolutely so that an install run with no PATH, or with one a
// login shell never built, still finds it. This path was not checked on a Mac,
// which is why launchctlPath below falls back to a PATH lookup rather than
// failing: being wrong about the path would otherwise make every method here
// fail with "no such file", and the fallback costs one stat per command.
const launchctlAbsolutePath = "/bin/launchctl"

// launchctlTimeout bounds every launchctl run.
//
// launchctl talks to launchd over a socket, so a launchd that is wedged would
// otherwise hold an install for as long as the operator is willing to wait.
// Thirty seconds is far more than any of these calls needs - they are a file
// read, a job removal and a status print - and short enough that an operator
// watching an install is told rather than left guessing.
const launchctlTimeout = 30 * time.Second

// startPollAttempts and startPollInterval bound the wait for the job to come
// up after it was bootstrapped, and for it to go after it was booted out.
//
// bootstrap returns once launchd has taken the job, not once the process is
// running, so what is polled is launchctl print. The bound is on the number of
// attempts and not on a deadline alone, because that is the one shape that
// cannot turn into a loop that never ends.
//
// Ten seconds altogether is the wait for a process that only has to open a
// database file before it is up, and a service that needs longer than that is
// one the operator has to be told about rather than waited on.
const (
	startPollAttempts = 40
	startPollInterval = 250 * time.Millisecond
)

// plistMode is what the plist is written as.
//
// launchd refuses to load a job whose plist anybody but its owner may write,
// which is the whole point: a plist a non-root account could edit is a root
// shell for that account. (The refusal is documented behaviour of launchd and
// was not checked here.) Nothing in the file is a secret - it holds two paths
// this program's own -help prints - so it stays readable.
const plistMode fs.FileMode = 0o644

// plistDirMode is what /Library/LaunchDaemons is made as if it is not there.
// It is a directory of the system that every Mac has, so this is for the case
// that it somehow is not, and it is made the way the system keeps it rather
// than closed off, since launchd is not the only thing that reads it.
const plistDirMode fs.FileMode = 0o755

func init() {
	newService = newLaunchdService
}

func newLaunchdService() (service, error) {
	return launchdService{}, nil
}

// launchdService is the backend. It holds nothing: the plist on disk is the
// whole of the state, which is what lets an uninstall in a later release find
// an installation this one wrote.
type launchdService struct{}

// Current reads the registration out of the plist.
//
// launchctl is not asked. The plist is the record - it is what launchd is
// given and what holds the two paths - and a job that is registered but not
// currently loaded still has to be found, which is exactly the state an
// install leaves behind between Stop and Start.
func (launchdService) Current() (Installed, error) {
	return currentFrom(launchdPlistPath)
}

// currentFrom is Current with the plist path handed in, so that the reading
// can be covered from a temporary directory instead of from
// /Library/LaunchDaemons.
func currentFrom(path string) (Installed, error) {
	document, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The one answer that means "nothing is registered". Every other
			// way of failing to read this file is a failure below, because a
			// plist that is there and unreadable is a service that is still
			// running and must not be walked away from.
			return Installed{}, ErrNotInstalled
		}

		return Installed{}, fmt.Errorf("failed to read the LaunchDaemon plist %s: %w", path, err)
	}

	arguments, err := programArguments(document)
	if err != nil {
		return Installed{}, fmt.Errorf("%s is there but the registration in it could not be read: %w", path, err)
	}

	return Installed{
		ExecutablePath: arguments[0],
		DatabaseFile:   databaseArgument(arguments),
		DefinitionPath: path,
	}, nil
}

// Register writes the plist. It does not load it: Start does that, and an
// install that has not yet copied the executable would otherwise have launchd
// starting a file that is half written.
func (launchdService) Register(plan Plan, existing *Installed) error {
	return writePlist(plistPathFor(existing), launchdPlist(plan))
}

// plistPathFor is where the plist goes.
//
// A registration that is already there is written over where it is, the way
// the interface asks. On this platform Current only ever looks in one place,
// so this is the default in every case an install reaches today; it is written
// this way regardless, because the day something else hands an Installed in -
// an operator who moved the file, a later release that looks in more than one
// directory - the alternative is two plists with the same label, and launchd
// would be holding the one this install did not write.
func plistPathFor(existing *Installed) string {
	if existing != nil && existing.DefinitionPath != "" {
		return existing.DefinitionPath
	}

	return launchdPlistPath
}

// writePlist puts document at path.
//
// It writes beside the file and renames over it for the same reason the
// executable is installed that way: launchd reads this file on its own
// schedule, and a file that is half written is a job definition that is
// neither the old one nor the new one.
func writePlist(path string, document []byte) error {
	err := os.MkdirAll(filepath.Dir(path), plistDirMode)
	if err != nil {
		return fmt.Errorf("failed to make the directory the LaunchDaemon plist goes in, %s: %w",
			filepath.Dir(path), err)
	}

	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("failed to make a file beside %s to write the LaunchDaemon plist into: %w", path, err)
	}

	tempPath := temp.Name()

	_, err = temp.Write(document)
	if err != nil {
		_ = temp.Close()
		_ = os.Remove(tempPath)

		return fmt.Errorf("failed to write the LaunchDaemon plist to %s: %w", tempPath, err)
	}

	err = temp.Close()
	if err != nil {
		_ = os.Remove(tempPath)

		return fmt.Errorf("failed to finish writing %s: %w", tempPath, err)
	}

	err = os.Chmod(tempPath, plistMode)
	if err != nil {
		_ = os.Remove(tempPath)

		return fmt.Errorf("failed to set the mode of %s: %w", tempPath, err)
	}

	err = os.Rename(tempPath, path)
	if err != nil {
		_ = os.Remove(tempPath)

		return fmt.Errorf("failed to put the LaunchDaemon plist at %s: %w", path, err)
	}

	return nil
}

// Unregister boots the job out and removes the plist. In that order: a plist
// that went first would leave launchd holding a job with nothing on disk
// naming it, and the label would still be taken.
func (launchdService) Unregister(installed Installed) error {
	err := bootout()
	if err != nil {
		return err
	}

	path := installed.DefinitionPath
	if path == "" {
		path = launchdPlistPath
	}

	err = os.Remove(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("failed to remove the LaunchDaemon plist %s: %w", path, err)
	}

	return nil
}

// Start loads the job and returns once launchd has a process for it.
//
// There is no fallback to launchctl load -w. bootstrap has been in launchctl
// since OS X 10.11 and load is the interface it replaced; the oldest macOS
// this binary can be built for is the oldest Go itself supports, which is
// macOS 11, so a machine that has no bootstrap is a machine this program
// cannot run on anyway. A fallback would be a second path through here that
// nothing would ever take and nobody could test. (Which launchctl subcommands
// a given macOS has was read from Apple's documentation, not checked on a Mac.)
func (s launchdService) Start() error {
	installed, err := s.Current()
	if err != nil {
		return fmt.Errorf("failed to read the LaunchDaemon plist to start it: %w", err)
	}

	arguments := bootstrapArgs(installed.DefinitionPath)

	ctx, cancel := context.WithTimeout(context.Background(), launchctlTimeout)
	output, err := runLaunchctl(ctx, arguments...)

	cancel()

	if err != nil {
		// A bootstrap of a job that is already bootstrapped fails, and that is
		// the state Start was asked for rather than a failure. launchctl's
		// exit codes were not checked on a Mac, so the state itself is asked
		// for instead of the code being read: if launchd holds the job now,
		// the bootstrap failing says nothing this caller has to act on.
		up, _, printErr := loaded()
		if printErr != nil {
			return fmt.Errorf("launchctl %s failed and the state of %s could not be read afterwards: %w\n%s",
				strings.Join(arguments, " "), launchdTarget, printErr, strings.TrimSpace(output))
		}

		if !up {
			return fmt.Errorf("launchctl %s failed: %w\n%s",
				strings.Join(arguments, " "), err, strings.TrimSpace(output))
		}
	}

	return waitRunning()
}

// Stop boots the job out and waits for launchd to let go of it.
//
// A job that is registered and not loaded is not a failure here: it is the
// state the caller wanted, and the install asks for it before it writes over
// the executable, which it may well do twice in a row.
func (launchdService) Stop() error {
	return bootout()
}

// bootstrapArgs, bootoutArgs and printArgs are the launchctl argv of this
// backend, in one place.
//
// They are functions rather than lines inside the methods so that the commands
// this package runs as root can be read off in a test on a machine that has no
// launchctl. That is the only check of them there is from here.
func bootstrapArgs(plistPath string) []string {
	return []string{"bootstrap", launchdDomain, plistPath}
}

func bootoutArgs() []string {
	return []string{"bootout", launchdTarget}
}

func printArgs() []string {
	return []string{"print", launchdTarget}
}

// bootout removes the job from the system domain.
//
// Removing one that is not there is not a failure, and which of the two
// happened is worked out by asking what the state is now rather than by
// reading an exit code, since the codes were not checked on a Mac.
func bootout() error {
	arguments := bootoutArgs()

	ctx, cancel := context.WithTimeout(context.Background(), launchctlTimeout)
	output, err := runLaunchctl(ctx, arguments...)

	cancel()

	if err != nil {
		up, _, printErr := loaded()
		if printErr != nil {
			return fmt.Errorf("launchctl %s failed and the state of %s could not be read afterwards: %w\n%s",
				strings.Join(arguments, " "), launchdTarget, printErr, strings.TrimSpace(output))
		}

		if up {
			return fmt.Errorf("launchctl %s failed and %s is still loaded: %w\n%s",
				strings.Join(arguments, " "), launchdTarget, err, strings.TrimSpace(output))
		}

		return nil
	}

	return waitGone()
}

// waitRunning polls until launchd reports a process for the job.
//
// bootstrap answers once launchd has taken the job, which is not the same as
// the program running: a binary that exits on a bad database file is taken
// just as happily. What the install reports to the operator is that the
// service came up, so that is what is waited for here.
func waitRunning() error {
	var last string

	for attempt := 0; attempt < startPollAttempts; attempt++ {
		up, output, err := loaded()
		if err != nil {
			return err
		}

		last = output

		if up {
			_, running := servicePID(output)
			if running {
				return nil
			}
		}

		time.Sleep(startPollInterval)
	}

	return fmt.Errorf("%s was loaded but no process of it was running after %s. "+
		"The last launchctl print of it was:\n%s",
		launchdTarget, startPollAttempts*startPollInterval, strings.TrimSpace(last))
}

// waitGone polls until launchd no longer holds the job. bootout asks launchd
// to take the job away and the process it is running may take the whole of its
// shutdown to go, so a Stop that returned straight away would hand back a
// service that is still holding its executable open, which is the one thing
// the caller stopped it for.
func waitGone() error {
	var last string

	for attempt := 0; attempt < startPollAttempts; attempt++ {
		up, output, err := loaded()
		if err != nil {
			return err
		}

		if !up {
			return nil
		}

		last = output

		time.Sleep(startPollInterval)
	}

	return fmt.Errorf("%s was booted out but is still loaded after %s. "+
		"The last launchctl print of it was:\n%s",
		launchdTarget, startPollAttempts*startPollInterval, strings.TrimSpace(last))
}

// loaded says whether launchd holds a job under this label, and hands back
// what launchctl print wrote so that a caller can put it in front of an
// operator.
//
// A non zero exit of print is taken as "not loaded". That is what it means for
// a target launchd does not know, and there is no exit code here to tell that
// apart from print failing for another reason - the codes were not checked on
// a Mac, and reading them wrongly is exactly how a backend ends up insisting a
// service is gone when it is not. So the output is carried out with the answer
// and ends up in the message of whichever caller could not make sense of it.
// The error answered here is kept for the case where launchctl could not be
// run at all, which is a different thing entirely from a target that is not
// there.
func loaded() (bool, string, error) {
	arguments := printArgs()

	ctx, cancel := context.WithTimeout(context.Background(), launchctlTimeout)
	defer cancel()

	output, err := runLaunchctl(ctx, arguments...)

	switch {
	case err == nil:
		return true, output, nil
	case ctx.Err() != nil:
		return false, output, fmt.Errorf("launchctl %s did not finish within %s: %w",
			strings.Join(arguments, " "), launchctlTimeout, ctx.Err())
	}

	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return false, output, nil
	}

	return false, output, fmt.Errorf("failed to run launchctl %s: %w", strings.Join(arguments, " "), err)
}

// servicePID reads the process id out of what launchctl print wrote.
//
// The output is a block of "name = value" lines and the job has a pid line
// only while a process of it is running, which is what makes it the answer to
// "did it come up". The shape of that line was taken from launchctl's
// documented output and not from a Mac, so a release of macOS that words it
// differently would make this wait out its attempts and report the output it
// could not read - which is the failure that puts the text in front of
// somebody who can see it, rather than one that claims the service is up.
func servicePID(printOutput string) (int, bool) {
	for _, line := range strings.Split(printOutput, "\n") {
		name, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(name) != "pid" {
			continue
		}

		pid, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || pid <= 0 {
			continue
		}

		return pid, true
	}

	return 0, false
}

// runLaunchctl runs one launchctl command under ctx and hands back everything
// it wrote. Standard error is taken with standard output because launchctl
// puts the reason a call failed on either one depending on the call, and what
// this package does with the text is show it to an operator.
func runLaunchctl(ctx context.Context, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, launchctlPath(), arguments...)

	output, err := command.CombinedOutput()

	return string(output), err
}

// launchctlPath is the launchctl to run. See launchctlAbsolutePath for why
// there are two answers.
func launchctlPath() string {
	_, err := os.Stat(launchctlAbsolutePath)
	if err != nil {
		return "launchctl"
	}

	return launchctlAbsolutePath
}

// launchdPlist is the job definition for plan.
//
// There is no WorkingDirectory in it, on purpose. Nothing this program does
// depends on the directory it is started in: the key file, the log and the
// initial password are read against the directory the database file is in
// (main.go, resolveInstallPath), which is named in ProgramArguments right
// here. Setting one would be a rule this platform followed and the systemd
// unit did not, for no difference in where a single file lands, and a
// WorkingDirectory that is not there is one more way for launchd to fail to
// start the job at all - after a -uninstall -purge removed the data directory,
// for one. (That launchd refuses to spawn over a missing WorkingDirectory is
// from its documentation and was not checked on a Mac; the reason for leaving
// the key out stands either way.)
//
// KeepAlive is the Restart=always of the systemd unit: launchd starts the job
// again whenever it exits, however it exited. RunAtLoad is what makes it come
// up when the machine boots, since the plist is loaded then.
func launchdPlist(plan Plan) []byte {
	var document bytes.Buffer

	document.WriteString(xml.Header)
	document.WriteString(plistDoctype)
	document.WriteString("<plist version=\"1.0\">\n<dict>\n")

	plistKey(&document, "Label")
	plistString(&document, "\t", launchdLabel)

	plistKey(&document, "ProgramArguments")
	document.WriteString("\t<array>\n")

	for _, argument := range programArgumentsOf(plan) {
		plistString(&document, "\t\t", argument)
	}

	document.WriteString("\t</array>\n")

	plistKey(&document, "RunAtLoad")
	document.WriteString("\t<true/>\n")

	plistKey(&document, "KeepAlive")
	document.WriteString("\t<true/>\n")

	document.WriteString("</dict>\n</plist>\n")

	return document.Bytes()
}

// plistDoctype is the line every property list carries. It names a DTD on
// apple.com and nothing fetches it - not launchd and not the reader below -
// but a plist without it is not a plist as far as the tools that read them by
// hand are concerned.
const plistDoctype = "<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" " +
	"\"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n"

// programArgumentsOf is the command line launchd starts, which is also the
// record an uninstall reads the two paths back out of.
//
// -db is passed even when the database sits where the defaults put it. Left
// out, the process works out its own path from the user configuration
// directory, which depends on the HOME of whoever the job runs as - and there
// is nothing here to read an installation back out of.
func programArgumentsOf(plan Plan) []string {
	return []string{plan.ExecutablePath, databaseFlag, plan.DatabaseFile}
}

// databaseFlag is the flag main.go reads the database path from.
const databaseFlag = "-db"

func plistKey(document *bytes.Buffer, key string) {
	document.WriteString("\t<key>" + key + "</key>\n")
}

// plistString writes one <string> element, with the value escaped.
//
// The escaping is why this is not a printf: these values are file paths that
// come from the operator through -bin and -db, and a path holding an ampersand
// or an angle bracket - both of which a filesystem takes happily - would write
// a plist launchd cannot parse, which is an installation that never comes up.
func plistString(document *bytes.Buffer, indent string, value string) {
	document.WriteString(indent + "<string>")
	// Writing to a bytes.Buffer, which does not fail.
	_ = xml.EscapeText(document, []byte(value))
	document.WriteString("</string>\n")
}

// plistFile, plistDict and plistEntry are as much of a property list as
// reading ProgramArguments back needs.
//
// It is encoding/xml and not a plist library because a plist is XML and this
// has to read exactly one array of strings out of the top level dictionary.
// A dependency would have to be picked, checked and followed for a release, to
// read a file this package wrote itself.
//
// The entries of the dictionary are taken in the order they appear, as ",any"
// rather than as named fields, because a property list dictionary is a flat
// list of alternating <key> and value elements: which value a key belongs to
// is the order and nothing else, and named fields would throw that away.
type plistFile struct {
	Dict plistDict `xml:"dict"`
}

type plistDict struct {
	Entries []plistEntry `xml:",any"`
}

type plistEntry struct {
	XMLName xml.Name
	// Text is what a <key> or a <string> holds.
	Text string `xml:",chardata"`
	// Strings are the <string> elements of an <array>.
	Strings []string `xml:"string"`
}

// programArguments reads the ProgramArguments array out of a LaunchDaemon
// plist.
//
// Everything it cannot make sense of is a failure and never an empty answer.
// This is what an uninstall finds the executable and the database by, and an
// empty answer would be read as an installation with no paths - which is an
// uninstall that removes the registration and leaves both files behind.
func programArguments(document []byte) ([]string, error) {
	var file plistFile

	err := xml.Unmarshal(document, &file)
	if err != nil {
		return nil, fmt.Errorf("failed to parse it as XML: %w", err)
	}

	for index, entry := range file.Dict.Entries {
		if entry.XMLName.Local != "key" || strings.TrimSpace(entry.Text) != "ProgramArguments" {
			continue
		}

		if index+1 >= len(file.Dict.Entries) {
			return nil, errors.New("the ProgramArguments key has no value after it")
		}

		value := file.Dict.Entries[index+1]
		if value.XMLName.Local != "array" {
			return nil, fmt.Errorf("ProgramArguments is a <%s> and not an <array>", value.XMLName.Local)
		}

		if len(value.Strings) == 0 {
			return nil, errors.New("the ProgramArguments array is empty, so it names no executable")
		}

		return value.Strings, nil
	}

	return nil, errors.New("it has no ProgramArguments, so it names no executable")
}

// databaseArgument is the -db path out of a command line, and the empty string
// when there is none. An empty answer is a real one here, the way the Installed
// field says: a registration that passes no -db is one where the process works
// out its own path.
//
// The forms other than the one this package writes are read because the plist
// is a text file on the operator's own machine, and one that was edited by hand
// is still the registration this installation has to be removed by.
func databaseArgument(arguments []string) string {
	// From 1: the first argument is the executable, and a program that happens
	// to be installed at a path ending in -db is not a flag.
	for index := 1; index < len(arguments); index++ {
		argument := arguments[index]

		for _, name := range []string{databaseFlag, "-" + databaseFlag} {
			if argument == name {
				if index+1 < len(arguments) {
					return arguments[index+1]
				}

				return ""
			}

			if strings.HasPrefix(argument, name+"=") {
				return strings.TrimPrefix(argument, name+"=")
			}
		}
	}

	return ""
}
