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

	// Register writes the service definition for plan, leaving the service
	// registered and not started. Start is what starts it.
	//
	// existing is what Current answered, and is nil when Current answered
	// ErrNotInstalled. It is passed rather than looked up again because a
	// registration that is already there has a place of its own that has to be
	// written over: on this machine the unit sits in /usr/lib/systemd/system,
	// and a backend that wrote a new one into /etc/systemd/system would leave
	// two units with /etc winning, so the old file would stay and still look
	// like the installation.
	Register(plan Plan, existing *Installed) error

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

	// Refused before anything is touched, so that the paths in the message are
	// the ones that are still on disk untouched.
	err = refuseElsewhere(plan, existing)
	if err != nil {
		return Outcome{}, err
	}

	outcome := Outcome{Plan: plan, Source: source, Replaced: existing != nil}

	outcome.DigestBefore, err = fileDigest(plan.ExecutablePath)
	if err != nil {
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
	}

	err = os.MkdirAll(plan.DataDir, dataDirMode)
	if err != nil {
		return Outcome{}, fmt.Errorf("failed to make the data directory %s: %w", plan.DataDir, err)
	}

	err = os.MkdirAll(filepath.Dir(plan.ExecutablePath), executableMode)
	if err != nil {
		return Outcome{}, fmt.Errorf("failed to make the directory the executable goes in, %s: %w",
			filepath.Dir(plan.ExecutablePath), err)
	}

	err = copyExecutable(executable, plan.ExecutablePath)
	if err != nil {
		return Outcome{}, err
	}

	outcome.DigestAfter, err = fileDigest(plan.ExecutablePath)
	if err != nil {
		return Outcome{}, err
	}

	err = svc.Register(plan, existing)
	if err != nil {
		return Outcome{}, fmt.Errorf("failed to register the service: %w", err)
	}

	// Read back rather than assumed. The registration is the only record of
	// where this installation is, so an install that could not read its own
	// registration afterwards has left behind something no uninstall can find.
	registered, err := svc.Current()
	if err != nil {
		return Outcome{}, fmt.Errorf("the service was registered but reading the registration back failed: %w", err)
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

// refuseElsewhere stops an install that would leave an older one behind.
//
// An installation that is registered at other paths owns an executable and a
// database this install would not touch: the new registration would point
// somewhere else, and the old files would sit there with nothing naming them,
// which is a state no uninstall can clear up afterwards. So it is the operator
// who removes the old one, and this says which one it is.
func refuseElsewhere(plan Plan, existing *Installed) error {
	if existing == nil {
		return nil
	}

	if samePath(plan.ExecutablePath, existing.ExecutablePath) && samePath(plan.DatabaseFile, existing.DatabaseFile) {
		return nil
	}

	return fmt.Errorf("%s is already registered as a service at another place:\n"+
		"  registered  executable %s, database %s\n"+
		"  asked for   executable %s, database %s\n"+
		"Run -uninstall first. Installing over it would leave the registered executable and "+
		"database behind with nothing naming them",
		serviceName,
		existing.ExecutablePath, existing.DatabaseFile,
		plan.ExecutablePath, plan.DatabaseFile)
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
