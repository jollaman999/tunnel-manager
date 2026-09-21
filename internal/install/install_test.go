package install

import (
	"errors"
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
	registerErr   error
	startErr      error

	calls          []string
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
		afterRegister: Installed{ExecutablePath: plan.ExecutablePath, DatabaseFile: plan.DatabaseFile, DefinitionPath: "/etc/systemd/system/tunnel-manager.service"},
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

	want := []string{"current", "stop", "register", "current", "start"}
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
		DefinitionPath: "/etc/systemd/system/tunnel-manager.service",
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
			err := refuseElsewhere(plan, c.existing)

			if c.refuse && err == nil {
				t.Error("the install was not refused")
			}

			if !c.refuse && err != nil {
				t.Errorf("the install was refused: %v", err)
			}
		})
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
