// Package install registers this program with the service manager of the
// platform it runs on, so that a release is installed with the binary that was
// downloaded and nothing else has to be fetched alongside it.
//
// What differs between the platforms is held behind one interface, and what
// they share - where the files go, whether this process may write there, what
// an install is allowed to overwrite and what is reported afterwards - is here.
// The design this follows is docs/design/2026-09-21-installer.md.
package install

import (
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// The names this installation is made of, on every platform.
//
// They are here rather than in each backend so that the three cannot drift
// apart. The name of the service is what an uninstall looks a registration up
// by, and a backend that spelled it differently would leave behind the very
// registration it wrote.
const (
	serviceName           = "tunnel-manager"
	executableName        = serviceName
	windowsExecutableName = executableName + ".exe"
	// databaseFileName is the same name main uses for the database it makes
	// when -db names nothing, so that an installed service and a run by hand
	// look for the file under the same name.
	databaseFileName = serviceName + ".db"
)

// Where each platform keeps the two things this installation owns, as the
// design settled them. Everything else - the key file, the log, the initial
// password - is read against the directory the database is in, so naming these
// two names all of it.
const (
	// unixExecutableDir is /usr/local rather than /usr, because what is
	// installed here was not put there by the package manager of the
	// distribution and has no business in the tree that one owns.
	unixExecutableDir = "/usr/local/bin"
	linuxDataDir      = "/var/lib/" + serviceName
	darwinDataDir     = "/Library/Application Support/" + serviceName
	windowsProgramDir = `C:\Program Files\` + serviceName
	windowsDataDir    = `C:\ProgramData\` + serviceName
)

// executableMode and dataDirMode are what is created.
//
// The data directory is closed to everybody but the account the service runs
// as, which is root or LocalSystem. The key that the stored SSH passwords are
// sealed with lives in it, and so does the initial password file, and a mode
// that let the rest of the machine read them would hand over every host this
// installation can reach.
const (
	executableMode fs.FileMode = 0o755
	dataDirMode    fs.FileMode = 0o700
)

// ErrNotInstalled is what a backend reports when this platform has no
// registration for this service. It is a value and not a failure to read,
// because "nothing is registered" is an answer an uninstall acts on rather than
// an error it gives up over: there is nothing to remove and nothing was left
// half removed.
var ErrNotInstalled = errors.New("no " + serviceName + " service is registered on this system")

// ErrNoBackend is what an install on a platform no backend was written for
// reports. See the newService hook below for why this is a run time answer and
// not a build failure.
var ErrNoBackend = errors.New("installing as a service is not implemented for " + runtime.GOOS)

// Plan is where an install puts things. It is worked out before anything is
// touched, so that the paths an install is refused over are the same ones it
// would have written to.
type Plan struct {
	// ExecutablePath is the file the service is started from. The service
	// definition names this path, which is how an uninstall later finds the
	// executable without any state file of ours.
	ExecutablePath string
	// DataDir holds the database and everything the process keeps beside it.
	DataDir string
	// DatabaseFile is what the service definition passes to -db. It is under
	// DataDir in every default, but it is carried apart because -db may name a
	// file somewhere else entirely, and then it is the definition that has to
	// say where it went.
	DatabaseFile string
}

// WithDatabase answers the plan with the database file the operator named, and
// the data directory that goes with it.
//
// The directory follows the file because it is the file that says where this
// installation is. Everything else the process keeps - the key the stored
// passwords are sealed with, the log, the initial password file - is read
// against the directory the database is in, and an uninstall works the data
// directory out of the registered -db the same way. Left at the default while
// -db pointed somewhere else, an install would make and hand out one directory
// while the process wrote into another, and -purge would then remove the empty
// one and leave the keys where they actually are.
func (p Plan) WithDatabase(databaseFile string) Plan {
	p.DatabaseFile = databaseFile
	p.DataDir = dataDirOf(databaseFile)

	return p
}

// Installed is what was read back out of the registration that exists now.
//
// There is no state file of ours anywhere: the service definition already holds
// the executable path and the -db path, so it is the one record, and it cannot
// go stale against itself. The three backends read it from three different
// places (systemctl show, the plist, the SCM config) and answer with this, so
// the removal flow above them is written once.
//
// A caller cannot build one of these for a system with no registration, and
// does not have to guess: a backend answers ErrNotInstalled instead.
type Installed struct {
	// ExecutablePath is the file the registration starts.
	ExecutablePath string
	// DatabaseFile is the -db argument the registration passes, empty when the
	// registration passes none and the process would work out its own default.
	DatabaseFile string
	// DefinitionPath is the file the registration itself lives in: the systemd
	// unit, the LaunchDaemon plist. Windows keeps its registration in the
	// registry rather than in a file, so there it is empty, and nothing above
	// may take an empty value to mean the service is not registered.
	DefinitionPath string
}

// service is what each platform provides. The backends are service_linux.go,
// service_darwin.go and service_windows.go.
//
// Every method deals with the one service this program installs, named by the
// serviceName constant above, so none of them takes a name: a name that could
// be passed in is a name an uninstall could be pointed at by mistake.
type service interface {
	// Current reads the registration that exists now.
	//
	// It reports ErrNotInstalled, wrapped or not, when there is none. That is
	// the answer an install takes as "register fresh" and an uninstall takes as
	// "nothing to do", so a backend must not answer it for a lookup that merely
	// failed - a registration that exists but could not be read has to come
	// back as a failure, or an uninstall would walk away from a service that is
	// still running.
	Current() (Installed, error)

	// CheckPlan reports whether this backend can register the service at the
	// paths in plan, and answers nil when it has nothing to refuse.
	//
	// It is asked before anything is written. The paths have to go into a
	// service definition and be read back out of one by a later uninstall, and
	// what survives that trip is the backend's own business: systemd splits a
	// command line on spaces, so a path with one in it cannot be read back at
	// all. A backend that only refused inside Register would refuse after the
	// executable had been copied into place, leaving a machine holding a new
	// binary that nothing on it starts.
	CheckPlan(plan Plan) error

	// Register writes the service definition for plan, leaving the service
	// registered and not started. Start is what starts it.
	//
	// existing is what Current answered, and is nil when Current answered
	// ErrNotInstalled. It is passed rather than looked up again because
	// registering over something that is already there is not the same call on
	// every platform: the Windows backend changes the configuration of the
	// service that exists instead of deleting it and making it again, since a
	// deleted service keeps its name until every handle to it is closed. The
	// Unix backends write their file at the one path they use and answer the
	// same either way.
	Register(plan Plan, existing *Installed) error

	// OpenFiles answers the files the running service holds open, as absolute
	// paths, with what is not a file of this installation - sockets, pipes, the
	// terminal, the kernel's own trees - already left out.
	//
	// This is how a removal finds the database. What a service was registered
	// with does not have to name one: a unit or a plist that passes no -db
	// starts a process which works out a path of its own, and the registration
	// then says nothing about the file that is about to be removed. What the
	// process has open is the file itself.
	//
	// It reports ErrOpenFilesUnknown, wrapped, for every answer that is not a
	// list: a service that is not running, a platform where the question cannot
	// be asked (Windows). That is not a failure of the removal - what the caller
	// does about it depends on whether it was going to remove data with it.
	OpenFiles() ([]string, error)

	// Unregister removes the registration named by installed, and nothing else.
	// The executable and the data are removed by the caller, which is what
	// knows whether the operator asked for the data to go too.
	//
	// Removing a registration that is not there is not a failure: it is the
	// state the caller was asking for.
	Unregister(installed Installed) error

	// Start starts the service and returns once it is up or once it has failed
	// to come up. It must not return nil for a service that was only asked to
	// start, because that answer is what the install reports to the operator as
	// the service having come up.
	Start() error

	// Stop stops the service and returns once it has stopped. A service that is
	// registered and not running is not a failure: the install stops the
	// service before overwriting its executable, and one that was already down
	// is exactly the state that was wanted.
	Stop() error
}

// newService builds the backend for this platform. Each backend file sets it
// from its init.
//
// It is a variable rather than a function declared once per platform so that
// this package, and everything that imports it, builds on every platform from
// the start. A platform with no backend then says so when an install is asked
// for, which is the one moment it matters, instead of breaking the build of the
// whole program.
var newService func() (service, error)

// privileged is what the flow asks, rather than the platform check straight, so
// that the flow can be run by a test. The test is not root and cannot become
// root, and the part of an install worth covering is what it does to the files
// and to the registration, which starts after this answer.
var privileged = hasPrivilege

// Defaults is where an install puts things on this platform when the operator
// names nothing.
func Defaults() Plan {
	return plannedFor(runtime.GOOS)
}

// plannedFor is Defaults with the platform handed in.
//
// It is apart from Defaults because the paths of all three platforms have to be
// checked from whichever one the tests run on. Building them is the part that
// can be wrong, and a test that could only ever see the platform it runs on
// would leave the other two to be found by an operator.
func plannedFor(goos string) Plan {
	switch goos {
	case "windows":
		return Plan{
			ExecutablePath: joinFor(goos, windowsProgramDir, windowsExecutableName),
			DataDir:        windowsDataDir,
			DatabaseFile:   joinFor(goos, windowsDataDir, databaseFileName),
		}
	case "darwin":
		return Plan{
			ExecutablePath: joinFor(goos, unixExecutableDir, executableName),
			DataDir:        darwinDataDir,
			DatabaseFile:   joinFor(goos, darwinDataDir, databaseFileName),
		}
	default:
		// Linux, and any other Unix this happens to be built for. The layout of
		// the one that is deployed is the sensible answer there too, and a
		// platform that wants another one is a backend that does not exist yet
		// and would refuse the install before these paths were used.
		return Plan{
			ExecutablePath: joinFor(goos, unixExecutableDir, executableName),
			DataDir:        linuxDataDir,
			DatabaseFile:   joinFor(goos, linuxDataDir, databaseFileName),
		}
	}
}

// joinFor puts a path together for goos and not for the platform this process
// runs on. filepath.Join is compiled with one separator, so on a Linux machine
// it answers C:/ProgramData/tunnel-manager, which is not a path Windows keeps
// anything at.
func joinFor(goos string, dir string, name string) string {
	if goos == "windows" {
		return dir + `\` + name
	}

	return dir + "/" + name
}

// Outcome is what an install did, in the terms it has to be checked by.
//
// Overwriting an executable and restarting a service cannot be taken back, so
// what the operator is left with is this: which file is in place now, whether
// it is the file that was there before, where it came from, and whether the
// service came back up.
type Outcome struct {
	Plan Plan
	// Source names the file the installed executable was copied from, so that a
	// download that failed and fell back to this running process is told apart
	// from a release that was fetched.
	Source string
	// DigestBefore is the md5 of the executable that was in place before, empty
	// when there was no file there.
	DigestBefore string
	// DigestAfter is the md5 of the executable that is in place now.
	DigestAfter string
	// Definition is the service definition that is registered now, read back
	// after registering rather than assumed, because on Linux it is the
	// existing unit's own place that is written to. Empty where the platform
	// keeps no file for it.
	Definition string
	// Replaced says a registration was already there and was written over.
	Replaced bool
	// Started says the service was started and came up.
	Started bool
}

// Report writes the outcome for a person to read.
//
// It is built whole and written once so that a failure part way through leaves
// no half line on the console: this is the record of a step that cannot be
// taken back, and half of it is worse than none.
func (o Outcome) Report(w io.Writer) error {
	var b strings.Builder

	b.WriteString(serviceName + " install\n")
	reportLine(&b, "executable", o.Plan.ExecutablePath)
	reportLine(&b, "taken from", o.Source)
	reportLine(&b, "md5 before", digestText(o.DigestBefore))
	reportLine(&b, "md5 after", digestText(o.DigestAfter))
	reportLine(&b, "data", o.Plan.DataDir)
	reportLine(&b, "database", o.Plan.DatabaseFile)
	reportLine(&b, "service", definitionText(o.Definition))

	if o.Replaced {
		reportLine(&b, "registration", "written over the one that was already there")
	} else {
		reportLine(&b, "registration", "new, nothing was registered before")
	}

	if o.Started {
		reportLine(&b, "state", "started")
	} else {
		reportLine(&b, "state", "not started")
	}

	_, err := io.WriteString(w, b.String())

	return err
}

// reportLineWidth lines the values up under each other. A report of an install
// is read by looking down the values, not by reading the labels.
const reportLineWidth = 13

func reportLine(b *strings.Builder, label string, value string) {
	fmt.Fprintf(b, "  %-*s %s\n", reportLineWidth, label, value)
}

func digestText(digest string) string {
	if digest == "" {
		return "no file was there"
	}

	return digest
}

func definitionText(definition string) string {
	if definition == "" {
		// Windows, where the registration is in the registry under the service
		// name and there is no file to name.
		return "registered as " + serviceName
	}

	return definition
}

// Install puts executable in place and registers the service.
//
// executable is the file to install, which the caller has either downloaded
// from the release or picked as this running process. Choosing between those
// two is not done here: what is chosen only has to be named in the report, so
// that an operator can tell which of the two they ended up with.
func Install(plan Plan, executable string, source string, out io.Writer) (Outcome, error) {
	if newService == nil {
		return Outcome{}, ErrNoBackend
	}

	svc, err := newService()
	if err != nil {
		return Outcome{}, err
	}

	return install(svc, plan, executable, source, out)
}

// install is Install with the backend handed in, which is what the tests drive.
func install(svc service, plan Plan, executable string, source string, out io.Writer) (Outcome, error) {
	err := CheckPrivilege()
	if err != nil {
		return Outcome{}, err
	}

	existing, err := currentOrNone(svc)
	if err != nil {
		return Outcome{}, err
	}

	// Which database the registration is using is asked of the process that is
	// running it, because the registration does not have to name one. It is
	// asked here, before anything is stopped: the answer comes out of the open
	// files of that process, and a process that has been stopped has none.
	unknownDatabase := fillDatabase(svc, existing)

	// Refused before anything is touched, so that the paths in the message are
	// the ones that are still on disk untouched.
	err = refuseElsewhere(plan, existing, unknownDatabase)
	if err != nil {
		return Outcome{}, err
	}

	// Asked here rather than left to Register, which runs after the executable
	// has been copied. A path this platform cannot carry is the same refusal
	// either way, but here the machine is still as it was, and there is no new
	// binary sitting at a path no registration names.
	err = svc.CheckPlan(plan)
	if err != nil {
		return Outcome{}, err
	}

	outcome := Outcome{Plan: plan, Source: source, Replaced: existing != nil}

	outcome.DigestBefore, err = fileDigest(plan.ExecutablePath)
	if err != nil {
		return Outcome{}, err
	}

	// What a failure from here on has to undo. The service is about to be
	// stopped, and every way out between that and the start at the end leaves
	// it down: an explicit stop is not a failure to a service manager, so
	// nothing brings it back on its own. It is put back up on the way out and
	// the failure that is reported is the one that happened, not what putting
	// it back said.
	stopped := false

	failed := func(err error) (Outcome, error) {
		if stopped {
			_ = svc.Start()
		}

		return Outcome{}, err
	}

	if existing != nil {
		// The running service holds its own executable open, and on Linux a
		// file that is open for execution cannot be written to at all. It is
		// also the only moment in this flow where the service is down, which is
		// why the design says the operator is told about it beforehand.
		err = svc.Stop()
		if err != nil {
			return Outcome{}, fmt.Errorf("failed to stop the service before replacing its executable: %w", err)
		}

		stopped = true
	}

	err = os.MkdirAll(plan.DataDir, dataDirMode)
	if err != nil {
		return failed(fmt.Errorf("failed to make the data directory %s: %w", plan.DataDir, err))
	}

	err = os.MkdirAll(filepath.Dir(plan.ExecutablePath), executableMode)
	if err != nil {
		return failed(fmt.Errorf("failed to make the directory the executable goes in, %s: %w",
			filepath.Dir(plan.ExecutablePath), err))
	}

	err = copyExecutable(executable, plan.ExecutablePath)
	if err != nil {
		return failed(err)
	}

	outcome.DigestAfter, err = fileDigest(plan.ExecutablePath)
	if err != nil {
		return failed(err)
	}

	err = svc.Register(plan, existing)
	if err != nil {
		return failed(fmt.Errorf("failed to register the service: %w", err))
	}

	// Read back rather than assumed. The registration is the only record of
	// where this installation is, so an install that could not read its own
	// registration afterwards has left behind something no uninstall can find.
	registered, err := svc.Current()
	if err != nil {
		return failed(fmt.Errorf("the service was registered but reading the registration back failed: %w", err))
	}

	outcome.Definition = registered.DefinitionPath

	err = svc.Start()
	if err != nil {
		// The report is written all the same. The executable is in place and
		// the service is registered, and an operator who is not told that much
		// has no idea what state the machine was left in.
		_ = outcome.Report(out)

		return outcome, fmt.Errorf("failed to start the service: %w", err)
	}

	outcome.Started = true

	err = outcome.Report(out)
	if err != nil {
		return outcome, fmt.Errorf("the install finished but reporting it failed: %w", err)
	}

	return outcome, nil
}

// currentOrNone reads the registration and turns "there is none" into a nil
// rather than an error, because every caller here has something to do in both
// cases and only a read that failed for another reason stops them.
func currentOrNone(svc service) (*Installed, error) {
	current, err := svc.Current()

	switch {
	case errors.Is(err, ErrNotInstalled):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("failed to read the service registration of this system: %w", err)
	}

	return &current, nil
}

// fillDatabase works out which database the registration in existing is using,
// when the registration itself does not name one, and answers why it could not
// be worked out.
//
// The running process is asked: the files it has open are the files it opened,
// which is the question being asked, while the command line it was started with
// is only what it was asked to open and may name nothing at all. A registration
// that already names a database is believed and the process is not asked - that
// is what the Windows backend answers, where -install always writes the path
// into the command line and there is no way to read a process's open files.
//
// A failure is answered and not returned as an error: not knowing is an answer
// the callers act on differently. An install refuses with it in the message, a
// removal that keeps the data goes ahead without it, and only -purge stops.
func fillDatabase(svc service, existing *Installed) error {
	if existing == nil || existing.DatabaseFile != "" {
		return nil
	}

	files, err := svc.OpenFiles()
	if err != nil {
		return err
	}

	database, err := databaseAmong(files)
	if err != nil {
		return err
	}

	existing.DatabaseFile = database

	return nil
}

// refuseElsewhere stops an install that would leave an older one behind.
//
// An installation that is registered at other paths owns an executable and a
// database this install would not touch: the new registration would point
// somewhere else, and the old files would sit there with nothing naming them,
// which is a state no uninstall can clear up afterwards. So it is the operator
// who removes the old one, and this says which one it is.
func refuseElsewhere(plan Plan, existing *Installed, unknownDatabase error) error {
	if existing == nil {
		return nil
	}

	sameExecutable := samePath(plan.ExecutablePath, existing.ExecutablePath)

	if sameExecutable && samePath(plan.DatabaseFile, existing.DatabaseFile) {
		return nil
	}

	// A registration that passes no -db is refused as well, and it is told
	// apart here because the reason is a different one: the executable is the
	// very path this install is putting the file at, and what stops it is that
	// nothing says which database that registration is using. The process it
	// starts works one out for itself, under the user data directory of
	// whoever the service runs as, and an install that took the empty value
	// for "no database" would register another one over it and leave that file
	// with nothing naming it. Guessing that the two are the same is the
	// dangerous reading, so this refuses and says what to do instead.
	if sameExecutable && existing.DatabaseFile == "" {
		return fmt.Errorf("%s is already registered as a service at %s, and which database it uses is not "+
			"known: the registration passes no -db, and %s. Nothing here can say whether it is %s.\n"+
			"Run -uninstall first. Installing over it would leave that database behind with nothing "+
			"naming it",
			serviceName, existing.ExecutablePath, unknownDatabaseText(unknownDatabase), plan.DatabaseFile)
	}

	return fmt.Errorf("%s is already registered as a service at another place:\n"+
		"  registered  executable %s, database %s\n"+
		"  asked for   executable %s, database %s\n"+
		"Run -uninstall first. Installing over it would leave the registered executable and "+
		"database behind with nothing naming them",
		serviceName,
		existing.ExecutablePath, registeredDatabaseText(existing.DatabaseFile),
		plan.ExecutablePath, plan.DatabaseFile)
}

// unknownDatabaseText says why the running process could not be asked which
// database it has open. The reason is the whole of what the operator can act
// on - a service that is not running is started again and asked, a platform
// that cannot be asked is told the path with -db - so it is carried into the
// message rather than summed up as "not known".
func unknownDatabaseText(err error) string {
	if err == nil {
		return "the service it is registered from was not asked which files it has open"
	}

	return err.Error()
}

// registeredDatabaseText names the database of the registration for the line
// above. A registration that passes no -db would print as nothing at all,
// beside a path on the line under it, and an operator reading that cannot tell
// an unknown database from one this program failed to print.
func registeredDatabaseText(databaseFile string) string {
	if databaseFile == "" {
		return "not known, the registration passes no -db"
	}

	return databaseFile
}

// samePath compares two paths as the platform reads them. They come from two
// places that spell them differently: one from the operator or from the
// defaults, the other out of a service definition that may carry a trailing
// separator or a doubled one.
func samePath(a string, b string) bool {
	if a == "" || b == "" {
		return a == b
	}

	a = filepath.Clean(a)
	b = filepath.Clean(b)

	if runtime.GOOS == "windows" {
		// Windows paths are not case sensitive, and the SCM hands back the
		// registration spelled the way it was written, which is not necessarily
		// the way the defaults spell Program Files.
		return strings.EqualFold(a, b)
	}

	return a == b
}

// copyExecutable puts src at dst.
//
// It writes a file beside dst and renames it over, rather than opening dst and
// writing into it. Writing in place would leave dst holding half of one build
// and half of another if anything went wrong part way, and a rename is the one
// step that either happened or did not. The temporary file is beside dst and
// not in the temporary directory because a rename across filesystems is not a
// rename at all.
func copyExecutable(src string, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open the executable to install, %s: %w", src, err)
	}

	defer func() {
		_ = source.Close()
	}()

	temp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*")
	if err != nil {
		return fmt.Errorf("failed to make a file beside %s to write the new executable into: %w", dst, err)
	}

	tempPath := temp.Name()

	_, err = io.Copy(temp, source)
	if err != nil {
		_ = temp.Close()
		_ = os.Remove(tempPath)

		return fmt.Errorf("failed to write the new executable to %s: %w", tempPath, err)
	}

	// Closed before the mode is set and before the rename, so that what is
	// renamed into place is a file with everything written to it. A rename of a
	// file that still has bytes in a buffer would put a short executable at the
	// path the service is started from.
	err = temp.Close()
	if err != nil {
		_ = os.Remove(tempPath)

		return fmt.Errorf("failed to finish writing %s: %w", tempPath, err)
	}

	// CreateTemp makes the file readable by its owner alone, which for a binary
	// that a service manager starts is not enough on its own, and says nothing
	// about being executable.
	err = os.Chmod(tempPath, executableMode)
	if err != nil {
		_ = os.Remove(tempPath)

		return fmt.Errorf("failed to make %s executable: %w", tempPath, err)
	}

	err = os.Rename(tempPath, dst)
	if err != nil {
		_ = os.Remove(tempPath)

		return fmt.Errorf("failed to put the new executable at %s: %w", dst, err)
	}

	return nil
}

// fileDigest is the md5 of the file at path, and the empty string when there is
// no file there.
//
// md5 is here to tell one build of the binary from another, not to withstand
// anybody: what the operator has to see is whether the file in place now is the
// file that was there before. What the downloaded release is checked against is
// the SHA256SUMS of the release, which is a different question with a different
// answer.
func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}

		return "", fmt.Errorf("failed to open %s to work out its md5: %w", path, err)
	}

	defer func() {
		_ = file.Close()
	}()

	digest := md5.New()

	_, err = io.Copy(digest, file)
	if err != nil {
		return "", fmt.Errorf("failed to read %s to work out its md5: %w", path, err)
	}

	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}

// CheckPrivilege reports whether this process may do an install at all.
//
// It is asked before anything is written, because the steps of an install are
// not one step: a process that could copy the executable but could not register
// the service would leave a machine with a new binary and the old service still
// pointing at nothing of it.
func CheckPrivilege() error {
	if privileged() {
		return nil
	}

	return errors.New("installing and removing the service needs more than this process has. " + privilegeHint)
}

// Removal is what an uninstall was asked for, beside what it reads out of the
// registration itself.
//
// The two paths are what the design leaves the operator for a machine whose
// registration is gone: there is no state file of ours anywhere, so once the
// service manager holds nothing, nothing on the machine says where this
// installation was put, and then it is the operator who has to name it.
type Removal struct {
	// ExecutablePath is the -bin of the command line, empty when none was
	// named.
	ExecutablePath string
	// DatabaseFile is the -db of the command line, empty when none was named.
	// The directory it is in is the data directory this removal reports on, and
	// the one -purge removes.
	DatabaseFile string
	// Purge asks for the data directory to be removed as well. It cannot be
	// taken back, which is why what it removed is named on the console while it
	// happens and again in the report.
	Purge bool
	// AssumeYes skips the question that is otherwise asked before anything is
	// removed. It is what a script says instead of answering, and it is the
	// only way an uninstall runs where the standard input is not a terminal:
	// see askToRemove.
	AssumeYes bool
}

// Removed is what an uninstall did, in the terms it has to be checked by.
//
// Stopping a service, taking a registration out and removing an executable
// cannot be taken back either, so what the operator is left with is this: what
// was found, what went, and - the part that is easy to miss - what was kept and
// where it is.
type Removed struct {
	// Source names where the paths below came from, so that a removal driven by
	// the registration is told apart from one the operator pointed by hand.
	Source string
	// Registered says a registration was there and was stopped and taken out.
	Registered bool
	// ExecutablePath is the file that was removed, empty when neither the
	// registration nor the operator named one.
	ExecutablePath string
	// ExecutableWasThere says there was a file at that path to begin with. A
	// path that named nothing is not a failure - the executable may have been
	// removed by hand before - but it is not the same answer as having removed
	// it, and an operator reading the report has to be able to tell.
	ExecutableWasThere bool
	// ExecutableAtReboot says the file could not be removed now and was handed
	// to the next reboot instead. See removeExecutable on Windows.
	ExecutableAtReboot bool
	// Definition is the registration that was taken out. Empty where the
	// platform keeps no file for it.
	Definition string
	// DataDir is the directory the database is in. It is reported whether or
	// not it was removed, because a removal that keeps the data has to say
	// where the data it kept is.
	DataDir string
	// DatabaseFile is the database of the installation that was removed.
	DatabaseFile string
	// Purged says the data directory was removed.
	Purged bool
	// NothingToRemove says there was no registration on this machine and no
	// -bin or -db either, so the uninstall had nothing to work on and touched
	// nothing. It is an answer and not a failure, which is why it is a field
	// here rather than an error: see whatToRemove.
	NothingToRemove bool
	// Cancelled says the question asked before anything was removed was
	// answered no, so nothing was stopped and nothing was removed. It is an
	// answer and not a failure for the same reason NothingToRemove is one: the
	// operator was asked and said no, and the machine is in the state they
	// asked for.
	Cancelled bool
}

// nothingToRemoveText is the whole of the report for that case.
//
// It is wrapped by hand because it is a paragraph and not a value on a labelled
// line, and it is indented to the width the report's values start at so that it
// reads as part of the same block. What it has to carry is both halves: that
// nothing happening was not a failure, and how to point an uninstall at an
// installation whose registration is already gone.
var nothingToRemoveText = "  nothing to remove: " + ErrNotInstalled.Error() + ",\n" +
	"  and neither -bin nor -db named anything. Nothing was stopped and nothing\n" +
	"  was removed, which is the state that was asked for and not a failure.\n" +
	"  If this program was installed on this machine and the registration is\n" +
	"  already gone, name what is left with -bin and -db and run this again.\n"

// cancelledText is the whole of the report for a removal that was answered no.
//
// It says what state the machine is in and not merely that the answer was no,
// because the question is asked before anything is stopped: the service is
// still running, which is the part an operator has to be able to count on
// without going and looking.
const cancelledText = "  nothing was removed: the question was answered no.\n" +
	"  The service is still registered and running and every file of the\n" +
	"  installation is where it was.\n"

// keyOutsideDataDirText is the last paragraph of the report of a -purge.
//
// What -purge removes is the data directory, and the key the stored SSH
// passwords are sealed with is in there only while security.key_file is left
// at its default. The stored setting is not read - that would mean opening the
// database of a service that is still running, see keyFileOf - so a removal
// cannot tell a key that was moved out of the data directory from one that was
// never made, and there is nothing to make this line conditional on. It is
// written every time -purge runs for that reason: the operator is the one who
// knows which of the two this installation was, and the file it is about opens
// every stored SSH password in a backup of the database.
const keyOutsideDataDirText = "\n" +
	"  The key the stored SSH passwords are sealed with was in the data\n" +
	"  directory that went, unless security.key_file was set to a path outside\n" +
	"  it. That setting was not read here. If it named such a path, that file is\n" +
	"  still on disk and has to be removed by hand: it opens every stored SSH\n" +
	"  password in a backup of the database.\n"

// Report writes what the uninstall did for a person to read.
//
// It is built whole and written once, the same way the install report is and
// for the same reason: this is the record of steps that cannot be taken back.
func (r Removed) Report(w io.Writer) error {
	var b strings.Builder

	b.WriteString(serviceName + " uninstall\n")

	if r.Cancelled {
		b.WriteString(cancelledText)

		_, err := io.WriteString(w, b.String())

		return err
	}

	if r.NothingToRemove {
		// The lines below would every one of them read "not known", which
		// looks like a removal that lost track of what it was doing rather
		// than one that had nothing to do.
		b.WriteString(nothingToRemoveText)

		_, err := io.WriteString(w, b.String())

		return err
	}

	reportLine(&b, "taken from", r.Source)
	reportLine(&b, "executable", r.executableText())
	reportLine(&b, "data", r.dataText())
	reportLine(&b, "database", knownText(r.DatabaseFile))

	if r.Registered {
		reportLine(&b, "service", removedDefinitionText(r.Definition)+", removed")
		reportLine(&b, "state", "stopped and taken out of the service manager")
	} else {
		reportLine(&b, "service", "nothing was registered")
		reportLine(&b, "state", "there was nothing registered to stop")
	}

	if r.Purged {
		b.WriteString(keyOutsideDataDirText)
	}

	_, err := io.WriteString(w, b.String())

	return err
}

func (r Removed) executableText() string {
	switch {
	case r.ExecutablePath == "":
		return "not known: nothing was registered and -bin named none"
	case r.ExecutableAtReboot:
		return r.ExecutablePath + ", removed at the next reboot of this machine"
	case !r.ExecutableWasThere:
		return r.ExecutablePath + ", no file was there"
	}

	return r.ExecutablePath + ", removed"
}

func (r Removed) dataText() string {
	switch {
	case r.DataDir == "":
		return "not known: no database file was named"
	case r.Purged:
		return r.DataDir + ", removed with everything under it"
	}

	return r.DataDir + ", left in place"
}

func knownText(path string) string {
	if path == "" {
		return "not known"
	}

	return path
}

// removedDefinitionText names the registration that went. It is apart from
// definitionText because that one reads as a registration that exists now,
// which after an uninstall it does not.
func removedDefinitionText(definition string) string {
	if definition == "" {
		// Windows, where the registration is in the registry under the service
		// name and there is no file to name.
		return "the registration of " + serviceName
	}

	return definition
}

// Uninstall stops the service, takes its registration out and removes the
// executable that registration named.
//
// The data is kept unless request asks for it to go: what an operator loses by
// keeping it is disk, and what they lose by removing it is every host, every
// credential and the key the passwords are sealed with, so the one that cannot
// be undone is the one that has to be asked for.
func Uninstall(request Removal, in io.Reader, out io.Writer) (Removed, error) {
	if newService == nil {
		return Removed{}, ErrNoBackend
	}

	svc, err := newService()
	if err != nil {
		return Removed{}, err
	}

	return uninstall(svc, request, in, out)
}

// uninstall is Uninstall with the backend handed in, which is what the tests
// drive.
//
// The order of the first half is what it is for one reason: the files the
// running service has open are read while it is still running. Reading comes
// first, then the question, and only then the stop and the removals. A flow
// that stopped the service first would be asking a process that no longer
// exists which files it had open.
func uninstall(svc service, request Removal, in io.Reader, out io.Writer) (Removed, error) {
	err := CheckPrivilege()
	if err != nil {
		return Removed{}, err
	}

	existing, err := currentOrNone(svc)
	if err != nil {
		return Removed{}, err
	}

	registeredDatabase := ""
	if existing != nil {
		registeredDatabase = existing.DatabaseFile
	}

	openFiles, unknownDatabase := askTheRunningService(svc, existing, request)

	// Worked out before anything is touched, so that a removal which has
	// nothing to work on stops while the machine is still as it was.
	removed := whatToRemove(request, existing)

	if existing != nil && registeredDatabase == "" && existing.DatabaseFile != "" {
		// Said in the report, because where the paths came from is what an
		// operator checks them against: a database the registration named is a
		// different kind of answer from one read off the descriptors of a
		// process that was running a moment ago.
		removed.Source = "the registration of this system, and the running service for the database it had open"
	}

	// The rest of what this installation is made of: the key, the initial
	// password file and the logs the rotation left. They are not open files -
	// the key is read at startup and closed - so they are worked out from the
	// database, which is the path everything else of this installation is read
	// against.
	files := installationFiles(removed.DatabaseFile, openFiles)

	if request.Purge {
		// Checked here and not only where the directory is removed, which is
		// after the service has been stopped, its registration taken out and
		// its executable deleted. A -purge that is refused has to leave the
		// installation it was refused over standing: every step below this
		// line is one that cannot be taken back, and an operator who is told
		// their data is still there wants the rest of it still there too.
		err = checkPurgeTarget(removed.DataDir, removed.DatabaseFile)
		if err != nil {
			return Removed{}, purgeRefusalWithReason(err, removed.DatabaseFile, unknownDatabase)
		}
	}

	if removed.NothingToRemove {
		err = removed.Report(out)
		if err != nil {
			return removed, fmt.Errorf("the uninstall had nothing to remove but reporting it failed: %w", err)
		}

		return removed, nil
	}

	// Asked while everything is still standing. What is shown is the list above,
	// paths and all, because the one mistake this catches is a removal pointed
	// at another installation than the operator had in mind, and that is only
	// visible from the paths.
	goAhead, err := askToRemove(removalPlanText(removed, files, existing != nil, request.Purge),
		request.AssumeYes, in, out)
	if err != nil {
		return Removed{}, err
	}

	if !goAhead {
		removed.Cancelled = true

		err = removed.Report(out)
		if err != nil {
			return removed, fmt.Errorf("the uninstall was answered no but reporting it failed: %w", err)
		}

		return removed, nil
	}

	if existing != nil {
		// Stopped before the registration goes. The other way round leaves a
		// running process with nothing registered naming it: the service
		// manager would no longer stop it, and an operator looking for what is
		// holding the port has nothing to look the process up by.
		err = svc.Stop()
		if err != nil {
			return removed, fmt.Errorf("failed to stop the service: %w", err)
		}

		err = svc.Unregister(*existing)
		if err != nil {
			return removed, fmt.Errorf("failed to take the service registration out: %w", err)
		}

		removed.Registered = true
	}

	err = removeInstalledExecutable(&removed)
	if err != nil {
		return removed, err
	}

	if request.Purge {
		err = purgeData(removed.DataDir, removed.DatabaseFile, files, out)
		if err != nil {
			// The report is written all the same. What the directory is was
			// settled at the top of this flow, so a failure here is the
			// removal itself giving up part way, and the service and the
			// executable are already gone: the operator has to be told that
			// much and that the data directory is in whatever state the
			// failure left it.
			_ = removed.Report(out)

			return removed, err
		}

		removed.Purged = true
	}

	err = removed.Report(out)
	if err != nil {
		return removed, fmt.Errorf("the uninstall finished but reporting it failed: %w", err)
	}

	return removed, nil
}

// askTheRunningService reads the files the service has open and works the
// database out of them, and answers why it could not where it could not.
//
// It is skipped when the operator named -db: they have said which database this
// is, and asking a process to check up on that would only raise the question of
// which of the two to believe. It is skipped when nothing is registered too,
// since there is no service to ask.
//
// The list itself is answered as well as the database, because the log file is
// in it. Which file the logs go to is a stored setting, and one that was changed
// without a restart names a file this process never wrote to; the descriptor is
// the file that is actually being written.
func askTheRunningService(svc service, existing *Installed, request Removal) ([]string, error) {
	if existing == nil || request.DatabaseFile != "" {
		return nil, nil
	}

	// A registration that names the database has answered the question this is
	// here for. It is still asked when -purge was given, because then the logs
	// are removed as well and the descriptor is the log that is being written
	// to, whatever the stored setting says it should be.
	if existing.DatabaseFile != "" && !request.Purge {
		return nil, nil
	}

	files, err := svc.OpenFiles()
	if err != nil {
		return nil, err
	}

	if existing.DatabaseFile != "" {
		// The registration named one, which is the Windows answer: there the
		// command line always carries -db and the open files cannot be read.
		return files, nil
	}

	database, err := databaseAmong(files)
	if err != nil {
		return files, err
	}

	existing.DatabaseFile = database

	return files, nil
}

// purgeRefusalWithReason puts the reason the database is not known into the
// refusal.
//
// checkPurgeTarget knows that nothing says where the data is; it does not know
// why, and why is the whole of what the operator can do something about - a
// service that is not running is started and asked again, a platform that
// cannot be asked is told the path with -db. The refusals for a directory that
// is known and refused for what it is are left as they are.
func purgeRefusalWithReason(refusal error, databaseFile string, unknownDatabase error) error {
	if databaseFile != "" || unknownDatabase == nil {
		return refusal
	}

	return fmt.Errorf("%w: %s. Give -db the database file of the installation to remove", refusal, unknownDatabase)
}

// whatToRemove settles which paths this removal is about.
//
// The registration is believed over -bin and -db wherever it names something,
// and the two flags fill in what it does not. They are not allowed to point the
// removal somewhere else while a registration exists: the registration is what
// says which files this installation owns, and a removal that took the operator
// word for it would remove a path nothing on this machine claims while leaving
// the registered one behind.
func whatToRemove(request Removal, existing *Installed) Removed {
	if existing == nil && request.ExecutablePath == "" && request.DatabaseFile == "" {
		// Not a failure. Nothing is registered and nothing was named, so what
		// was asked for - this machine without the service on it - is already
		// the state it is in, the same way removing a file that is not there
		// or disabling a unit that was never enabled is done rather than
		// refused. A removal is also a thing that gets run twice, by a person
		// who is not sure it took the first time and by whatever script wraps
		// it, and the second run has to end the way the first one did.
		return Removed{NothingToRemove: true}
	}

	removed := Removed{}
	named := false

	if existing != nil {
		removed.ExecutablePath = existing.ExecutablePath
		removed.DatabaseFile = existing.DatabaseFile
		removed.Definition = existing.DefinitionPath
	}

	if removed.ExecutablePath == "" && request.ExecutablePath != "" {
		removed.ExecutablePath = request.ExecutablePath
		named = true
	}

	if removed.DatabaseFile == "" && request.DatabaseFile != "" {
		removed.DatabaseFile = request.DatabaseFile
		named = true
	}

	removed.DataDir = dataDirOf(removed.DatabaseFile)
	removed.Source = removalSource(existing != nil, named)

	return removed
}

func removalSource(registered bool, named bool) string {
	switch {
	case registered && named:
		return "the registration of this system, and -bin or -db for what it does not name"
	case registered:
		return "the registration of this system"
	}

	return "-bin and -db, since nothing is registered on this system"
}

// dataDirOf is the directory the database file is in, which is the directory
// this installation keeps everything in: the key the stored passwords are
// sealed with, the log and the initial password file are all read against it.
func dataDirOf(databaseFile string) string {
	if databaseFile == "" {
		return ""
	}

	return filepath.Dir(filepath.Clean(databaseFile))
}

// removeInstalledExecutable removes the file the registration was started from
// and records what happened to it.
//
// Whether there was a file is asked before rather than taken from the removal,
// because "there was nothing there" and "it was removed" are different things
// to tell an operator, and only the first of them means somebody has already
// been here.
func removeInstalledExecutable(removed *Removed) error {
	if removed.ExecutablePath == "" {
		return nil
	}

	_, err := os.Stat(removed.ExecutablePath)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("failed to look at the executable %s: %w", removed.ExecutablePath, err)
	}

	removed.ExecutableWasThere = true

	removed.ExecutableAtReboot, err = removeExecutable(removed.ExecutablePath)
	if err != nil {
		return err
	}

	return nil
}

// purgeData removes the data directory, once it is sure that is what the
// directory is.
//
// The flow above has already asked the same question before it stopped the
// service, and this asks it again. It is the last statement before a RemoveAll
// of a path, so what it costs is one function call and what it buys is that no
// future caller can reach that RemoveAll without the check having run.
func purgeData(dataDir string, databaseFile string, files []installedFile, out io.Writer) error {
	err := checkPurgeTarget(dataDir, databaseFile)
	if err != nil {
		return err
	}

	// Named on the console before it goes and not only in the report
	// afterwards. This is the one step of an uninstall that cannot be undone by
	// installing again, and an operator who sees the path while it happens can
	// tell at once that -purge was pointed at the wrong installation.
	_, err = fmt.Fprintf(out, "-purge: removing %s and everything under it. This cannot be taken back\n", dataDir)
	if err != nil {
		return fmt.Errorf("failed to report what -purge was about to remove: %w", err)
	}

	err = os.RemoveAll(dataDir)
	if err != nil {
		return fmt.Errorf("failed to remove the data directory %s: %w", dataDir, err)
	}

	// Whatever of this installation is not under that directory. A log or a key
	// whose setting names a path of its own is outside it, and leaving the key
	// behind is leaving the one file that opens every stored SSH password.
	for _, file := range outsideDataDir(files, dataDir) {
		_, err = fmt.Fprintf(out, "-purge: removing %s, %s\n", file.Path, file.What)
		if err != nil {
			return fmt.Errorf("failed to report what -purge was about to remove: %w", err)
		}

		err = os.Remove(file.Path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("failed to remove %s, %s: %w", file.Path, file.What, err)
		}
	}

	return nil
}

// purgeMinimumDepth is how deep the data directory has to be before -purge may
// remove it whole.
//
// /var/lib/tunnel-manager is three deep and C:\ProgramData\tunnel-manager is
// two counting from the drive, while one is /var, /opt or C:\ProgramData - the
// directories a machine keeps everything else in - and zero is the root itself.
const purgeMinimumDepth = 2

// purgeRefused are the directories that pass the depth above and still hold
// more than one installation. Removing any of them takes out software nobody
// asked this program about.
var purgeRefused = map[string]bool{
	"var/lib":                     true,
	"var/log":                     true,
	"var/tmp":                     true,
	"var/cache":                   true,
	"usr/bin":                     true,
	"usr/lib":                     true,
	"usr/local":                   true,
	"usr/share":                   true,
	"etc/systemd":                 true,
	"library/application support": true,
	"library/launchdaemons":       true,
	"windows/system32":            true,
	"program files/common files":  true,
}

// homeRoots are the directories user home directories sit directly under. A
// home directory is two deep and would pass the rules above, and a -db that
// named a file in one is not a reason to remove everything that person has.
var homeRoots = []string{"home", "Users"}

// checkPurgeTarget refuses a directory that is not the data directory of this
// installation.
//
// -purge hands a path to RemoveAll, and the path is worked out from a -db which
// was either typed by hand or read out of a registration this program did not
// necessarily write. Every rule here guards the same mistake: removing a
// directory that holds more than this installation. Being wrong here removes
// somebody else's data, and no part of that can be given back.
func checkPurgeTarget(dataDir string, databaseFile string) error {
	if dataDir == "" || databaseFile == "" {
		return errors.New("-purge was asked for, but nothing says where the data of this installation is: " +
			"no database file is named by the registration and none was given with -db")
	}

	if !filepath.IsAbs(dataDir) {
		return fmt.Errorf("-purge will not remove %q: it is not an absolute path, so what it means depends "+
			"on where this was run from", dataDir)
	}

	// The directory is worked out from the database file by the caller, so this
	// holds for every path that gets here today. It is checked all the same,
	// because it is the one rule that says the directory belongs to this
	// installation at all - the rest only say it is not the machine.
	if !samePath(filepath.Dir(databaseFile), dataDir) {
		return fmt.Errorf("-purge will not remove %s: the database of this installation is %s, which is "+
			"not in that directory", dataDir, databaseFile)
	}

	elements := pathElements(dataDir)

	if len(elements) < purgeMinimumDepth {
		return fmt.Errorf("-purge will not remove %s: it is the root of the filesystem or a directory "+
			"directly under it, which holds far more than this installation", dataDir)
	}

	if refusedPurgeTarget(elements) {
		return fmt.Errorf("-purge will not remove %s: it is a directory the system keeps other things in, "+
			"not a directory of this installation alone", dataDir)
	}

	return nil
}

// refusedPurgeTarget says whether the directory is one of the named ones.
func refusedPurgeTarget(elements []string) bool {
	if purgeRefused[strings.ToLower(strings.Join(elements, "/"))] {
		return true
	}

	if len(elements) == purgeMinimumDepth {
		for _, root := range homeRoots {
			if strings.EqualFold(elements[0], root) {
				return true
			}
		}
	}

	return false
}

// pathElements is the directories a path is made of, without the drive letter
// of a Windows path.
//
// Both separators are read rather than the one this program was built with. A
// path comes here from a registration or from a command line, and one spelled
// the other way round has to be counted as the directories it is rather than
// read as one long name - a name that would pass the depth rule and be removed
// whole.
func pathElements(path string) []string {
	// Cleaned first, or a path with .. in it counts as deeper than the
	// directory it actually names: /var/lib/x/.. is /var/lib.
	cleaned := filepath.Clean(strings.ReplaceAll(path, `\`, "/"))

	elements := strings.FieldsFunc(cleaned, func(r rune) bool {
		return r == '/' || r == '\\'
	})

	// A drive is not a directory under the root, it is the root.
	if len(elements) > 0 && strings.HasSuffix(elements[0], ":") {
		elements = elements[1:]
	}

	return elements
}
