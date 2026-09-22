package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPlannedFor holds the default paths of all three platforms to the table in
// docs/design/2026-09-21-installer.md.
//
// The paths are written out here rather than built from the constants. Built
// from them, this would pass for any set of constants at all, and what it has
// to catch is exactly a constant that was changed: these are the places an
// installation is found at afterwards, and an operator upgrading from a release
// where they were different ends up with two of them.
func TestPlannedFor(t *testing.T) {
	cases := []struct {
		goos       string
		executable string
		dataDir    string
		database   string
	}{
		{
			goos:       "linux",
			executable: "/usr/local/bin/tunnel-manager",
			dataDir:    "/var/lib/tunnel-manager",
			database:   "/var/lib/tunnel-manager/tunnel-manager.db",
		},
		{
			goos:       "darwin",
			executable: "/usr/local/bin/tunnel-manager",
			dataDir:    "/Library/Application Support/tunnel-manager",
			database:   "/Library/Application Support/tunnel-manager/tunnel-manager.db",
		},
		{
			goos:       "windows",
			executable: `C:\Program Files\tunnel-manager\tunnel-manager.exe`,
			dataDir:    `C:\ProgramData\tunnel-manager`,
			database:   `C:\ProgramData\tunnel-manager\tunnel-manager.db`,
		},
	}

	for _, c := range cases {
		t.Run(c.goos, func(t *testing.T) {
			plan := plannedFor(c.goos)

			if plan.ExecutablePath != c.executable {
				t.Errorf("executable of %s is %q, want %q", c.goos, plan.ExecutablePath, c.executable)
			}

			if plan.DataDir != c.dataDir {
				t.Errorf("data directory of %s is %q, want %q", c.goos, plan.DataDir, c.dataDir)
			}

			if plan.DatabaseFile != c.database {
				t.Errorf("database file of %s is %q, want %q", c.goos, plan.DatabaseFile, c.database)
			}

			// Logged so that go test -v shows the table itself, which is what
			// the design document and the README have to be held against.
			t.Logf("executable %s\ndata       %s\ndatabase   %s",
				plan.ExecutablePath, plan.DataDir, plan.DatabaseFile)
		})
	}
}

// TestDefaultsIsThisPlatform checks the one thing the table above cannot: that
// Defaults answers with the row of the platform it is asked on.
func TestDefaultsIsThisPlatform(t *testing.T) {
	if Defaults() != plannedFor(runtime.GOOS) {
		t.Errorf("Defaults() is %+v, want the plan of %s, %+v", Defaults(), runtime.GOOS, plannedFor(runtime.GOOS))
	}
}

// fakeService stands in for a platform backend. It records the order it was
// used in, which is the part of the flow that cannot be seen from the files
// afterwards.
type fakeService struct {
	current    Installed
	currentErr error
	// afterRegister is what Current answers once Register has run, the way a
	// real backend reads back the registration it just wrote.
	afterRegister Installed
	checkPlanErr  error
	registerErr   error
	startErr      error
	// openFiles is what the running service is said to have open, and
	// openFilesErr what asking for them answers instead. A backend that cannot
	// be asked - Windows - answers ErrOpenFilesUnknown, and so does one whose
	// service is not running.
	openFiles    []string
	openFilesErr error

	calls          []string
	checkedPlan    Plan
	registeredPlan Plan
	registeredOver *Installed
	unregistered   *Installed
}

func (f *fakeService) Current() (Installed, error) {
	f.calls = append(f.calls, "current")

	for _, call := range f.calls {
		if call == "register" {
			return f.afterRegister, nil
		}
	}

	return f.current, f.currentErr
}

func (f *fakeService) OpenFiles() ([]string, error) {
	f.calls = append(f.calls, "openfiles")

	if f.openFilesErr != nil {
		return nil, f.openFilesErr
	}

	return f.openFiles, nil
}

func (f *fakeService) CheckPlan(plan Plan) error {
	f.calls = append(f.calls, "checkplan")
	f.checkedPlan = plan

	return f.checkPlanErr
}

func (f *fakeService) Register(plan Plan, existing *Installed) error {
	f.calls = append(f.calls, "register")
	f.registeredPlan = plan
	f.registeredOver = existing

	return f.registerErr
}

func (f *fakeService) Unregister(installed Installed) error {
	f.calls = append(f.calls, "unregister")
	f.unregistered = &installed

	return nil
}

func (f *fakeService) Start() error {
	f.calls = append(f.calls, "start")

	return f.startErr
}

func (f *fakeService) Stop() error {
	f.calls = append(f.calls, "stop")

	return nil
}

// allowPrivilege runs the flow as if this process were root. The check itself
// is the platform's and is not what these cover.
func allowPrivilege(t *testing.T) {
	t.Helper()

	was := privileged
	privileged = func() bool { return true }

	t.Cleanup(func() {
		privileged = was
	})
}

// planIn builds a plan under a directory of the test, so that nothing here
// writes to the places a real install writes to.
func planIn(t *testing.T) Plan {
	t.Helper()

	root := t.TempDir()

	return Plan{
		ExecutablePath: filepath.Join(root, "bin", executableName),
		DataDir:        filepath.Join(root, "data"),
		DatabaseFile:   filepath.Join(root, "data", databaseFileName),
	}
}

func writeFile(t *testing.T, path string, content string) string {
	t.Helper()

	err := os.MkdirAll(filepath.Dir(path), 0o755)
	if err != nil {
		t.Fatalf("failed to make the directory of %s: %v", path, err)
	}

	err = os.WriteFile(path, []byte(content), 0o755)
	if err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
	}

	return path
}

// TestInstallFresh covers a machine with nothing registered: the executable is
// put in place, the data directory is made, the registration is written and
// read back, and the service is started.
func TestInstallFresh(t *testing.T) {
	allowPrivilege(t)

	plan := planIn(t)
	source := writeFile(t, filepath.Join(t.TempDir(), "downloaded"), "the new build")

	svc := &fakeService{
		currentErr:    ErrNotInstalled,
		afterRegister: Installed{ExecutablePath: plan.ExecutablePath, DatabaseFile: plan.DatabaseFile, DefinitionPath: "/lib/systemd/system/tunnel-manager.service"},
	}

	var out strings.Builder

	outcome, err := install(svc, plan, source, "the release", &out)
	if err != nil {
		t.Fatalf("the install failed: %v", err)
	}

	installed, err := os.ReadFile(plan.ExecutablePath)
	if err != nil {
		t.Fatalf("failed to read what was installed: %v", err)
	}

	if string(installed) != "the new build" {
		t.Errorf("the installed executable holds %q, want %q", installed, "the new build")
	}

	info, err := os.Stat(plan.ExecutablePath)
	if err != nil {
		t.Fatalf("failed to stat what was installed: %v", err)
	}

	if info.Mode().Perm() != executableMode {
		t.Errorf("the installed executable is %v, want %v", info.Mode().Perm(), executableMode)
	}

	dir, err := os.Stat(plan.DataDir)
	if err != nil {
		t.Fatalf("the data directory was not made: %v", err)
	}

	if !dir.IsDir() {
		t.Errorf("%s is not a directory", plan.DataDir)
	}

	if outcome.DigestBefore != "" {
		t.Errorf("the md5 before is %q, want it empty since no file was there", outcome.DigestBefore)
	}

	if outcome.DigestAfter == "" {
		t.Error("the md5 after is empty")
	}

	if outcome.Replaced {
		t.Error("the outcome says a registration was written over, but there was none")
	}

	if !outcome.Started {
		t.Error("the outcome says the service was not started")
	}

	if outcome.Definition != svc.afterRegister.DefinitionPath {
		t.Errorf("the outcome names %q as the registration, want the one read back, %q",
			outcome.Definition, svc.afterRegister.DefinitionPath)
	}

	if svc.registeredOver != nil {
		t.Errorf("the backend was told to write over %+v, want nil since nothing was registered", svc.registeredOver)
	}

	if svc.registeredPlan != plan {
		t.Errorf("the backend was given %+v, want %+v", svc.registeredPlan, plan)
	}

	if out.Len() == 0 {
		t.Error("nothing was reported")
	}
}

// TestInstallPutsTheServiceBackWhenItFailsAfterStoppingIt covers the way out of
// an upgrade that fails between the stop and the start.
//
// A service manager does not bring back a service that was stopped on purpose,
// so every failure in that stretch used to leave the machine with the service
// down and nothing about to start it. What is reported is still the failure
// that happened.
func TestInstallPutsTheServiceBackWhenItFailsAfterStoppingIt(t *testing.T) {
	allowPrivilege(t)

	plan := planIn(t)
	writeFile(t, plan.ExecutablePath, "the old build")
	source := writeFile(t, filepath.Join(t.TempDir(), "downloaded"), "the new build")

	registered := Installed{
		ExecutablePath: plan.ExecutablePath,
		DatabaseFile:   plan.DatabaseFile,
		DefinitionPath: "/usr/lib/systemd/system/tunnel-manager.service",
	}

	svc := &fakeService{
		current:       registered,
		afterRegister: registered,
		registerErr:   errors.New("the definition could not be written"),
	}

	var out strings.Builder

	_, err := install(svc, plan, source, "the release", &out)
	if err == nil {
		t.Fatal("the install reported no failure, want the one the registration raised")
	}

	if !strings.Contains(err.Error(), "the definition could not be written") {
		t.Errorf("the failure reported is %v, want the one the registration raised", err)
	}

	want := []string{"current", "checkplan", "stop", "register", "start"}
	if strings.Join(svc.calls, ",") != strings.Join(want, ",") {
		t.Errorf("the backend was used as %v, want %v: the service has to be started again", svc.calls, want)
	}
}

// TestInstallDoesNotStartAServiceItNeverStopped covers a first install that
// fails. There was nothing running, so there is nothing to put back, and a
// start here would leave a service up that the operator never had.
func TestInstallDoesNotStartAServiceItNeverStopped(t *testing.T) {
	allowPrivilege(t)

	plan := planIn(t)
	source := writeFile(t, filepath.Join(t.TempDir(), "downloaded"), "the new build")

	svc := &fakeService{
		currentErr:  ErrNotInstalled,
		registerErr: errors.New("the definition could not be written"),
	}

	var out strings.Builder

	_, err := install(svc, plan, source, "the release", &out)
	if err == nil {
		t.Fatal("the install reported no failure, want the one the registration raised")
	}

	for _, call := range svc.calls {
		if call == "start" {
			t.Fatalf("the backend was used as %v, want no start: nothing was stopped", svc.calls)
		}
	}
}

// TestInstallOverTheSamePaths covers an upgrade. The service has to be stopped
// before its executable is written, since a service that is up holds that file.
func TestInstallOverTheSamePaths(t *testing.T) {
	allowPrivilege(t)

	plan := planIn(t)
	writeFile(t, plan.ExecutablePath, "the old build")
	source := writeFile(t, filepath.Join(t.TempDir(), "downloaded"), "the new build")

	registered := Installed{
		ExecutablePath: plan.ExecutablePath,
		DatabaseFile:   plan.DatabaseFile,
		DefinitionPath: "/usr/lib/systemd/system/tunnel-manager.service",
	}

	svc := &fakeService{current: registered, afterRegister: registered}

	var out strings.Builder

	outcome, err := install(svc, plan, source, "the release", &out)
	if err != nil {
		t.Fatalf("the install failed: %v", err)
	}

	want := []string{"current", "checkplan", "stop", "register", "current", "start"}
	if strings.Join(svc.calls, ",") != strings.Join(want, ",") {
		t.Errorf("the backend was used as %v, want %v", svc.calls, want)
	}

	if !outcome.Replaced {
		t.Error("the outcome does not say a registration was written over")
	}

	if outcome.DigestBefore == "" || outcome.DigestBefore == outcome.DigestAfter {
		t.Errorf("the md5 before is %q and after is %q, want two different non-empty values",
			outcome.DigestBefore, outcome.DigestAfter)
	}

	if svc.registeredOver == nil || svc.registeredOver.DefinitionPath != registered.DefinitionPath {
		t.Errorf("the backend was told to write over %+v, want the registration that was there, %+v",
			svc.registeredOver, registered)
	}
}

// TestInstallRefusesAnotherPlace is the case the design stops: an installation
// registered somewhere else would be left behind with nothing naming it. What
// matters as much as the refusal is that nothing was touched before it.
func TestInstallRefusesAnotherPlace(t *testing.T) {
	allowPrivilege(t)

	plan := planIn(t)
	source := writeFile(t, filepath.Join(t.TempDir(), "downloaded"), "the new build")

	svc := &fakeService{current: Installed{
		ExecutablePath: "/opt/tm/tunnel-manager",
		DatabaseFile:   "/opt/tm/tm.db",
		DefinitionPath: "/lib/systemd/system/tunnel-manager.service",
	}}

	var out strings.Builder

	_, err := install(svc, plan, source, "the release", &out)
	if err == nil {
		t.Fatal("the install went ahead over an installation registered somewhere else")
	}

	if !strings.Contains(err.Error(), "/opt/tm/tunnel-manager") {
		t.Errorf("the refusal does not name the registered executable: %v", err)
	}

	_, err = os.Stat(plan.ExecutablePath)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("something was written to %s before the install was refused", plan.ExecutablePath)
	}

	if strings.Join(svc.calls, ",") != "current" {
		t.Errorf("the backend was used as %v, want the read alone", svc.calls)
	}
}

// TestInstallRefusedByTheBackendBeforeAnythingIsWritten covers a plan this
// platform cannot register - on systemd a path with a space in it, which a unit
// cannot carry in a way an uninstall could read back.
//
// The refusal itself belongs to the backend. What is held here is when it is
// asked: before the executable is copied. Asked from inside Register, as it
// once was, the refusal arrives with the new binary already at a path no
// registration names.
func TestInstallRefusedByTheBackendBeforeAnythingIsWritten(t *testing.T) {
	allowPrivilege(t)

	plan := planIn(t)
	source := writeFile(t, filepath.Join(t.TempDir(), "downloaded"), "the new build")

	svc := &fakeService{
		currentErr:   ErrNotInstalled,
		checkPlanErr: errors.New("that path cannot go in a unit"),
	}

	var out strings.Builder

	_, err := install(svc, plan, source, "the release", &out)
	if err == nil {
		t.Fatal("the install went ahead with a plan the backend refused")
	}

	if _, statErr := os.Stat(plan.ExecutablePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the executable was written to %s before the refusal: %v", plan.ExecutablePath, statErr)
	}

	if _, statErr := os.Stat(plan.DataDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the data directory %s was made before the refusal: %v", plan.DataDir, statErr)
	}

	// The read and the question, and nothing that changes the machine: no
	// stop of a running service either.
	want := []string{"current", "checkplan"}
	if strings.Join(svc.calls, ",") != strings.Join(want, ",") {
		t.Errorf("the backend was used as %v, want %v", svc.calls, want)
	}

	if svc.checkedPlan != plan {
		t.Errorf("the backend was asked about %+v, want the plan the install would use, %+v",
			svc.checkedPlan, plan)
	}

	t.Logf("refused as it should: %v", err)
}

// TestCheckUnitPathsIsWhatTheLinuxBackendAnswers ties the case above to the
// real refusal, on the platform that has one. A path with a space in it is
// written into a systemd unit fine and cannot be read back out of one, so an
// uninstall that read it would remove a path that is not the one installed.
func TestCheckUnitPathsIsWhatTheLinuxBackendAnswers(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the systemd backend is only built on linux")
	}

	svc, err := newService()
	if err != nil {
		t.Fatalf("failed to build the backend: %v", err)
	}

	good := plannedFor("linux")
	if err := svc.CheckPlan(good); err != nil {
		t.Errorf("the default plan was refused: %v", err)
	}

	spaced := good
	spaced.DatabaseFile = "/var/lib/tunnel manager/tunnel-manager.db"

	err = svc.CheckPlan(spaced)
	if err == nil {
		t.Fatal("a database path with a space in it was accepted")
	}

	t.Logf("refused as it should: %v", err)
}

// TestInstallWithoutPrivilege checks that the refusal comes before the backend
// is reached at all.
func TestInstallWithoutPrivilege(t *testing.T) {
	was := privileged
	privileged = func() bool { return false }

	t.Cleanup(func() {
		privileged = was
	})

	plan := planIn(t)
	svc := &fakeService{currentErr: ErrNotInstalled}

	var out strings.Builder

	_, err := install(svc, plan, "", "the release", &out)
	if err == nil {
		t.Fatal("the install ran as a process with no privilege")
	}

	if len(svc.calls) != 0 {
		t.Errorf("the backend was used as %v, want nothing", svc.calls)
	}
}

// TestInstallReportsAStartThatFailed covers the report being written for an
// install that got the executable in place and then could not bring the service
// up. That is the state an operator has to be told about most.
func TestInstallReportsAStartThatFailed(t *testing.T) {
	allowPrivilege(t)

	plan := planIn(t)
	source := writeFile(t, filepath.Join(t.TempDir(), "downloaded"), "the new build")

	svc := &fakeService{
		currentErr:    ErrNotInstalled,
		afterRegister: Installed{ExecutablePath: plan.ExecutablePath, DatabaseFile: plan.DatabaseFile},
		startErr:      errors.New("the unit failed"),
	}

	var out strings.Builder

	outcome, err := install(svc, plan, source, "the release", &out)
	if err == nil {
		t.Fatal("the install reported no failure although the service did not start")
	}

	if outcome.Started {
		t.Error("the outcome says the service started")
	}

	if !strings.Contains(out.String(), "not started") {
		t.Errorf("the report does not say the service is down:\n%s", out.String())
	}
}

// TestRefuseElsewhere covers the comparison on its own, including the paths
// that are the same written two ways.
func TestRefuseElsewhere(t *testing.T) {
	plan := Plan{ExecutablePath: "/usr/local/bin/tunnel-manager", DatabaseFile: "/var/lib/tunnel-manager/tunnel-manager.db"}

	cases := []struct {
		name     string
		existing *Installed
		refuse   bool
	}{
		{name: "nothing registered", existing: nil},
		{
			name:     "the same paths",
			existing: &Installed{ExecutablePath: plan.ExecutablePath, DatabaseFile: plan.DatabaseFile},
		},
		{
			name:     "the same paths spelled another way",
			existing: &Installed{ExecutablePath: "/usr/local/bin/./tunnel-manager", DatabaseFile: "/var/lib//tunnel-manager/tunnel-manager.db"},
		},
		{
			name:     "another executable",
			existing: &Installed{ExecutablePath: "/opt/tm/tunnel-manager", DatabaseFile: plan.DatabaseFile},
			refuse:   true,
		},
		{
			name:     "another database",
			existing: &Installed{ExecutablePath: plan.ExecutablePath, DatabaseFile: "/opt/tm/tm.db"},
			refuse:   true,
		},
		{
			name:     "a registration that passes no database",
			existing: &Installed{ExecutablePath: plan.ExecutablePath},
			refuse:   true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := refuseElsewhere(plan, c.existing, nil)

			if c.refuse && err == nil {
				t.Error("the install was not refused")
			}

			if !c.refuse && err != nil {
				t.Errorf("the install was refused: %v", err)
			}

			if c.refuse && err != nil {
				t.Logf("refused as it should: %v", err)
			}
		})
	}
}

// TestRefuseElsewhereWithoutARegisteredDatabase is the message of the case
// above that reads as nothing at all.
//
// The registration passes no -db, so this install cannot tell whether the
// database it is about to register is the one that registration is already
// using. It is refused either way - taking the unknown for "the same" is the
// reading that loses a database - and what has to be right is what the operator
// is told, which before was a line with an empty path on it.
func TestRefuseElsewhereWithoutARegisteredDatabase(t *testing.T) {
	plan := Plan{
		ExecutablePath: "/usr/local/bin/tunnel-manager",
		DataDir:        "/var/lib/tunnel-manager",
		DatabaseFile:   "/var/lib/tunnel-manager/tunnel-manager.db",
	}

	// The reason the running process could not be asked either, which is what
	// the operator is left to act on.
	unknown := fmt.Errorf("%w: the service is registered but no process of it is running", ErrOpenFilesUnknown)

	err := refuseElsewhere(plan, &Installed{
		ExecutablePath: plan.ExecutablePath,
		DefinitionPath: "/lib/systemd/system/tunnel-manager.service",
	}, unknown)
	if err == nil {
		t.Fatal("the install went ahead over a registration whose database is not known")
	}

	message := err.Error()

	// What it has to say: that the database is unknown, and what the operator
	// can do about it.
	for _, want := range []string{"passes no -db", "not known", "-uninstall", "-db",
		"no process of it is running"} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal does not hold %q: %v", want, err)
		}
	}

	// And what it must not read as: a database whose path is the empty string,
	// which is what an operator was shown before.
	if strings.Contains(message, "database \n") || strings.Contains(message, "database ,") {
		t.Errorf("the refusal prints the unknown database as an empty path: %v", err)
	}

	t.Logf("refused as it should: %v", err)
}

// TestRefuseElsewhereNamesAnUnknownDatabaseInTheTable covers the same empty
// value on the other message, the one for a registration that is at another
// place altogether.
func TestRefuseElsewhereNamesAnUnknownDatabaseInTheTable(t *testing.T) {
	plan := Plan{
		ExecutablePath: "/usr/local/bin/tunnel-manager",
		DatabaseFile:   "/var/lib/tunnel-manager/tunnel-manager.db",
	}

	err := refuseElsewhere(plan, &Installed{ExecutablePath: "/opt/tm/tunnel-manager"}, nil)
	if err == nil {
		t.Fatal("the install went ahead over a registration somewhere else")
	}

	if !strings.Contains(err.Error(), "not known, the registration passes no -db") {
		t.Errorf("the refusal does not say the registered database is unknown: %v", err)
	}

	t.Logf("refused as it should: %v", err)
}

// TestPlanWithDatabase holds the rule that the data directory follows -db.
//
// The install makes the data directory and the registration hands it out, while
// an uninstall works it out of the registered -db. Left at the default while
// -db named a file somewhere else, those are two different directories: the one
// the install made and nothing wrote to, and the one holding the key, the log
// and the initial password that -purge would then not touch.
func TestPlanWithDatabase(t *testing.T) {
	cases := []struct {
		name     string
		plan     Plan
		database string
		dataDir  string
	}{
		{
			name:     "a database outside the default data directory",
			plan:     plannedFor("linux"),
			database: "/opt/tm/tm.db",
			dataDir:  "/opt/tm",
		},
		{
			name:     "the default database of this platform",
			plan:     plannedFor("linux"),
			database: "/var/lib/tunnel-manager/tunnel-manager.db",
			dataDir:  "/var/lib/tunnel-manager",
		},
		{
			name:     "a path spelled with a doubled separator",
			plan:     plannedFor("linux"),
			database: "/opt/tm//data/tm.db",
			dataDir:  "/opt/tm/data",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.plan.WithDatabase(c.database)

			if got.DatabaseFile != c.database {
				t.Errorf("the database is %q, want %q", got.DatabaseFile, c.database)
			}

			if got.DataDir != c.dataDir {
				t.Errorf("the data directory is %q, want %q", got.DataDir, c.dataDir)
			}

			// The directory an uninstall would work out of the very same path,
			// which is the pair that has to match.
			if got.DataDir != dataDirOf(got.DatabaseFile) {
				t.Errorf("the install would use %q while an uninstall reads %q out of the registration",
					got.DataDir, dataDirOf(got.DatabaseFile))
			}

			if got.ExecutablePath != c.plan.ExecutablePath {
				t.Errorf("the executable moved to %q, want %q", got.ExecutablePath, c.plan.ExecutablePath)
			}
		})
	}
}

// TestInstallMakesTheDataDirectoryTheDatabaseIsIn is the same rule end to end:
// what the install makes on disk is the directory the database was pointed at,
// not the default one.
func TestInstallMakesTheDataDirectoryTheDatabaseIsIn(t *testing.T) {
	allowPrivilege(t)

	root := t.TempDir()
	elsewhere := filepath.Join(root, "elsewhere", "tm.db")

	plan := planIn(t).WithDatabase(elsewhere)
	source := writeFile(t, filepath.Join(t.TempDir(), "downloaded"), "the new build")

	svc := &fakeService{
		currentErr:    ErrNotInstalled,
		afterRegister: Installed{ExecutablePath: plan.ExecutablePath, DatabaseFile: plan.DatabaseFile},
	}

	_, err := install(svc, plan, source, "the release", &strings.Builder{})
	if err != nil {
		t.Fatalf("the install failed: %v", err)
	}

	if _, err := os.Stat(filepath.Dir(elsewhere)); err != nil {
		t.Errorf("the directory the database was pointed at was not made: %v", err)
	}

	if svc.registeredPlan.DataDir != filepath.Dir(elsewhere) {
		t.Errorf("the registration was given the data directory %q, want %q",
			svc.registeredPlan.DataDir, filepath.Dir(elsewhere))
	}
}

// TestFileDigest pins the digest to a value worked out elsewhere, so that a
// change of the hash is caught rather than agreed with.
func TestFileDigest(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "file"), "hello")

	digest, err := fileDigest(path)
	if err != nil {
		t.Fatalf("failed to work out the digest: %v", err)
	}

	const wantHello = "5d41402abc4b2a76b9719d911017c592"

	if digest != wantHello {
		t.Errorf("the digest of hello is %q, want %q", digest, wantHello)
	}

	missing, err := fileDigest(filepath.Join(t.TempDir(), "not there"))
	if err != nil {
		t.Fatalf("a file that is not there was reported as a failure: %v", err)
	}

	if missing != "" {
		t.Errorf("the digest of a file that is not there is %q, want it empty", missing)
	}
}

// TestOutcomeReport checks that what cannot be taken back is in what the
// operator reads: both digests, where the executable came from, and whether the
// service is up.
func TestOutcomeReport(t *testing.T) {
	outcome := Outcome{
		Plan:         plannedFor("linux"),
		Source:       "the running executable, /home/someone/tunnel-manager",
		DigestBefore: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DigestAfter:  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Definition:   "/usr/lib/systemd/system/tunnel-manager.service",
		Replaced:     true,
		Started:      true,
	}

	var out strings.Builder

	err := outcome.Report(&out)
	if err != nil {
		t.Fatalf("failed to write the report: %v", err)
	}

	for _, want := range []string{
		outcome.Plan.ExecutablePath,
		outcome.Plan.DataDir,
		outcome.Plan.DatabaseFile,
		outcome.Source,
		outcome.DigestBefore,
		outcome.DigestAfter,
		outcome.Definition,
		"started",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not hold %q:\n%s", want, out.String())
		}
	}

	// Logged so that go test -v shows what an operator is left with after a
	// step that cannot be taken back.
	t.Logf("\n%s", out.String())
}

// TestOutcomeReportWithoutAFileBefore checks that a first install says so
// rather than leaving the line empty, which reads as a digest that could not be
// worked out.
func TestOutcomeReportWithoutAFileBefore(t *testing.T) {
	var out strings.Builder

	err := Outcome{Plan: plannedFor("windows")}.Report(&out)
	if err != nil {
		t.Fatalf("failed to write the report: %v", err)
	}

	if !strings.Contains(out.String(), "no file was there") {
		t.Errorf("the report does not say there was no file before:\n%s", out.String())
	}

	// Windows keeps no file for the registration, so the line has to name the
	// service instead of being blank.
	if !strings.Contains(out.String(), "registered as "+serviceName) {
		t.Errorf("the report does not name the registration:\n%s", out.String())
	}
}

// TestInstallWithoutABackend covers the platform no backend was written for.
// The hook is nil there, and the answer has to be a refusal rather than a panic
// on a nil call.
func TestInstallWithoutABackend(t *testing.T) {
	was := newService
	newService = nil

	t.Cleanup(func() {
		newService = was
	})

	_, err := Install(plannedFor(runtime.GOOS), "", "the release", &strings.Builder{})
	if !errors.Is(err, ErrNoBackend) {
		t.Errorf("the install answered %v, want %v", err, ErrNoBackend)
	}
}
