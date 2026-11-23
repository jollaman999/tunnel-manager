package install

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// thisProcess stands in for os.Getpid in these tests. The number is not one a
// fake would answer by accident.
const thisProcess = 4242

func startedBy(managed bool, err error) func() (bool, error) {
	return func() (bool, error) {
		return managed, err
	}
}

// TestDisableThisServiceTurnsOffTheStartAtBootOfTheServiceItIs is the case the
// uninstall on the Settings screen is for: the service manager runs this very
// process, and what is asked of it is the start at boot and nothing more. A
// stop or an unregister here would end the process before its answer.
func TestDisableThisServiceTurnsOffTheStartAtBootOfTheServiceItIs(t *testing.T) {
	svc := &fakeService{servicePID: thisProcess}

	disabled, err := disableThisService(svc, startedBy(true, nil), thisProcess)
	if err != nil {
		t.Fatalf("disableThisService: %v", err)
	}

	if !disabled {
		t.Fatalf("the process the service manager runs was answered as not being the service")
	}

	if got := strings.Join(svc.calls, ","); got != "servicepid,disable" {
		t.Fatalf("the calls were %s, want servicepid,disable", got)
	}
}

// TestDisableThisServiceLeavesAServiceItIsNotAlone covers the copies of this
// program that are not the service: one started by hand beside a registered
// service, and one on a machine where nothing is registered or nothing is
// running. Neither has a start at boot of its own to turn off.
func TestDisableThisServiceLeavesAServiceItIsNotAlone(t *testing.T) {
	cases := []struct {
		name string
		pid  int
	}{
		{name: "another process is the service", pid: thisProcess + 1},
		{name: "no process of the service is running", pid: 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := &fakeService{servicePID: c.pid}

			disabled, err := disableThisService(svc, startedBy(true, nil), thisProcess)
			if err != nil {
				t.Fatalf("disableThisService: %v", err)
			}

			if disabled {
				t.Fatalf("a process that is not the service was answered as the service")
			}

			for _, call := range svc.calls {
				if call == "disable" {
					t.Fatalf("the start at boot of a service this process is not was turned off")
				}
			}
		})
	}
}

// TestDisableThisServiceDoesNotAskWhereNoServiceManagerCouldHaveStartedIt is a
// run in a container or by hand on a system with no service manager. The
// service manager is not asked at all, because asking one that is not there is
// a failure the screen would show for nothing.
func TestDisableThisServiceDoesNotAskWhereNoServiceManagerCouldHaveStartedIt(t *testing.T) {
	svc := &fakeService{servicePID: thisProcess}

	disabled, err := disableThisService(svc, startedBy(false, nil), thisProcess)
	if err != nil {
		t.Fatalf("disableThisService: %v", err)
	}

	if disabled || len(svc.calls) != 0 {
		t.Fatalf("answered %v after the calls %v, want false and no call", disabled, svc.calls)
	}
}

// TestDisableThisServiceReportsWhatWentWrong holds the three failures apart.
// Only the one that came from turning the start at boot off is answered as the
// service: that is the one where this process is known to be it, and the screen
// says the service is still enabled.
func TestDisableThisServiceReportsWhatWentWrong(t *testing.T) {
	refused := errors.New("refused")

	cases := []struct {
		name         string
		svc          *fakeService
		started      func() (bool, error)
		wantDisabled bool
	}{
		{
			name:    "whether a service manager started this could not be told",
			svc:     &fakeService{servicePID: thisProcess},
			started: startedBy(false, refused),
		},
		{
			name:    "the service manager could not be asked which process the service is",
			svc:     &fakeService{servicePIDErr: refused},
			started: startedBy(true, nil),
		},
		{
			name:         "turning the start at boot off failed",
			svc:          &fakeService{servicePID: thisProcess, disableErr: refused},
			started:      startedBy(true, nil),
			wantDisabled: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isService, err := disableThisService(c.svc, c.started, thisProcess)

			if !errors.Is(err, refused) {
				t.Fatalf("the error is %v, want it to carry %v", err, refused)
			}

			if isService != c.wantDisabled {
				t.Fatalf("answered %v for being the service, want %v", isService, c.wantDisabled)
			}
		})
	}
}

// selfRemovalFixture is an installation in a temporary directory, laid out the
// way a service one is: an executable, a definition and a data directory with
// the database in it and a directory of logs under it.
type selfRemovalFixture struct {
	executable   string
	definition   string
	dataDir      string
	databaseFile string
}

func newSelfRemovalFixture(t *testing.T) selfRemovalFixture {
	t.Helper()

	dir := t.TempDir()
	fixture := selfRemovalFixture{
		executable:   writeFile(t, filepath.Join(dir, "bin", serviceName), "the program"),
		definition:   writeFile(t, filepath.Join(dir, "units", serviceName+".service"), "the unit"),
		dataDir:      filepath.Join(dir, "data"),
		databaseFile: filepath.Join(dir, "data", databaseFileName),
	}

	// What the uninstall on the Settings screen leaves in the data directory
	// once it has removed the files it names: the directory of the logs, and
	// whatever else was put there.
	writeFile(t, filepath.Join(fixture.dataDir, "logs", "kept-by-hand.txt"), "")

	return fixture
}

func (f selfRemovalFixture) service(pid int) *fakeService {
	return &fakeService{
		servicePID: pid,
		current: Installed{
			ExecutablePath: f.executable,
			DefinitionPath: f.definition,
		},
	}
}

func there(t *testing.T, path string) bool {
	t.Helper()

	_, err := os.Lstat(path)
	if err == nil {
		return true
	}

	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed to look at %s: %v", path, err)
	}

	return false
}

// TestRemoveThisServiceTakesItAwayWithoutStoppingIt is the uninstall on the
// Settings screen of a service: the start at boot goes, then the registration,
// then the executable and the data directory, and the service is never
// stopped and never unregistered the way -uninstall does it, which stops it
// first. The process asking is the service, and a stop would end it before the
// answer it is in the middle of was written.
func TestRemoveThisServiceTakesItAwayWithoutStoppingIt(t *testing.T) {
	fixture := newSelfRemovalFixture(t)
	svc := fixture.service(thisProcess)

	removal := removeThisService(svc, startedBy(true, nil), thisProcess, fixture.databaseFile)

	if got := strings.Join(svc.calls, ","); got != "servicepid,disable,current,unregisterself" {
		t.Fatalf("the calls were %s, want servicepid,disable,current,unregisterself", got)
	}

	if !removal.IsService || removal.DisableErr != nil {
		t.Fatalf("answered IsService %v and %v, want the service with no error",
			removal.IsService, removal.DisableErr)
	}

	if svc.unregisteredSelf == nil || svc.unregisteredSelf.DefinitionPath != fixture.definition {
		t.Fatalf("the registration taken out was %+v, want the one at %s", svc.unregisteredSelf, fixture.definition)
	}

	parts := []struct {
		name string
		part RemovedPart
		path string
	}{
		{name: "registration", part: removal.Registration, path: fixture.definition},
		{name: "executable", part: removal.Executable, path: fixture.executable},
		{name: "data directory", part: removal.DataDir, path: fixture.dataDir},
	}

	for _, p := range parts {
		if !p.part.Removed || p.part.Kept || p.part.Err != nil || p.part.Path != p.path {
			t.Errorf("the %s is reported as %+v, want %s removed", p.name, p.part, p.path)
		}
	}

	for _, path := range []string{fixture.executable, fixture.dataDir} {
		if there(t, path) {
			t.Errorf("%s is still there", path)
		}
	}
}

// TestRemoveThisServiceLeavesWhatIsNotTheService covers the copies of this
// program that no service manager runs: one started by hand, one in a
// container, one started beside the service. Nothing registers them, so there
// is no registration of theirs to remove, and the executable and the directory
// the database is in are not theirs to remove either: a run by hand may well be
// the build in a working copy and a database in the directory it was run from.
func TestRemoveThisServiceLeavesWhatIsNotTheService(t *testing.T) {
	cases := []struct {
		name    string
		started func() (bool, error)
		pid     int
	}{
		{name: "no service manager could have started it", started: startedBy(false, nil), pid: thisProcess},
		{name: "another process is the service", started: startedBy(true, nil), pid: thisProcess + 1},
		{name: "whether a service manager started it could not be told",
			started: startedBy(false, errors.New("refused")), pid: thisProcess},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fixture := newSelfRemovalFixture(t)
			svc := fixture.service(c.pid)

			removal := removeThisService(svc, c.started, thisProcess, fixture.databaseFile)

			if removal.IsService {
				t.Fatalf("a process that is not the service was answered as the service")
			}

			if removal.Registration != (RemovedPart{}) || removal.Executable != (RemovedPart{}) ||
				removal.DataDir != (RemovedPart{}) {
				t.Fatalf("parts were reported for a process that is not the service: %+v", removal)
			}

			for _, call := range svc.calls {
				if call == "unregisterself" || call == "unregister" || call == "stop" {
					t.Fatalf("the calls were %v, want no registration touched", svc.calls)
				}
			}

			for _, path := range []string{fixture.executable, fixture.definition, fixture.databaseFile} {
				if path == fixture.databaseFile {
					path = fixture.dataDir
				}

				if !there(t, path) {
					t.Errorf("%s was removed", path)
				}
			}
		})
	}
}

// TestRemoveThisServiceSaysWhatWasLeft holds what each failure leaves behind.
// A part that could not go does not stop the parts after it, with one
// exception: the executable stays while the registration does, because it is
// the program that runs -uninstall, which is what removes a registration that
// is left over. A start at boot that could not be turned off does not keep the
// registration either, since removing the registration is what takes the start
// at boot away for good.
func TestRemoveThisServiceSaysWhatWasLeft(t *testing.T) {
	refused := errors.New("refused")

	t.Run("the start at boot could not be turned off", func(t *testing.T) {
		fixture := newSelfRemovalFixture(t)
		svc := fixture.service(thisProcess)
		svc.disableErr = refused

		removal := removeThisService(svc, startedBy(true, nil), thisProcess, fixture.databaseFile)

		if !removal.IsService || !errors.Is(removal.DisableErr, refused) {
			t.Fatalf("answered IsService %v and %v, want the service and the refusal",
				removal.IsService, removal.DisableErr)
		}

		if !removal.Registration.Removed || !removal.Executable.Removed || !removal.DataDir.Removed {
			t.Fatalf("the parts are %+v, want every one removed", removal)
		}
	})

	t.Run("the registration could not be taken out", func(t *testing.T) {
		fixture := newSelfRemovalFixture(t)
		svc := fixture.service(thisProcess)
		svc.unregisterSelfErr = refused

		removal := removeThisService(svc, startedBy(true, nil), thisProcess, fixture.databaseFile)

		if removal.Registration.Removed || !errors.Is(removal.Registration.Err, refused) ||
			removal.Registration.Path != fixture.definition {
			t.Fatalf("the registration is reported as %+v, want %s failed with the refusal",
				removal.Registration, fixture.definition)
		}

		if removal.Executable.Removed || !removal.Executable.Kept || removal.Executable.Err == nil ||
			removal.Executable.Path != fixture.executable {
			t.Fatalf("the executable is reported as %+v, want %s kept with a reason",
				removal.Executable, fixture.executable)
		}

		if !there(t, fixture.executable) {
			t.Fatalf("the executable went while the registration that names it stayed")
		}

		if !removal.DataDir.Removed || there(t, fixture.dataDir) {
			t.Fatalf("the data directory is reported as %+v, want it removed", removal.DataDir)
		}
	})

	t.Run("the registration could not be read", func(t *testing.T) {
		fixture := newSelfRemovalFixture(t)
		svc := fixture.service(thisProcess)
		svc.currentErr = refused

		removal := removeThisService(svc, startedBy(true, nil), thisProcess, fixture.databaseFile)

		if removal.Registration.Removed || !errors.Is(removal.Registration.Err, refused) {
			t.Fatalf("the registration is reported as %+v, want it failed with the refusal", removal.Registration)
		}

		if removal.Executable.Removed || !removal.Executable.Kept || !there(t, fixture.executable) {
			t.Fatalf("the executable is reported as %+v, want it kept", removal.Executable)
		}

		for _, call := range svc.calls {
			if call == "unregisterself" {
				t.Fatalf("a registration that could not be read was taken out")
			}
		}
	})

	t.Run("the data directory is not one this installation alone has", func(t *testing.T) {
		fixture := newSelfRemovalFixture(t)
		svc := fixture.service(thisProcess)

		// Relative, which -purge refuses for the same reason.
		removal := removeThisService(svc, startedBy(true, nil), thisProcess, databaseFileName)

		if removal.DataDir.Removed || !removal.DataDir.Kept || removal.DataDir.Err == nil {
			t.Fatalf("the data directory is reported as %+v, want it kept with the refusal", removal.DataDir)
		}

		if !removal.Registration.Removed || !removal.Executable.Removed {
			t.Fatalf("the parts are %+v, want the registration and the executable removed", removal)
		}
	})
}

// TestAtRebootOrderPutsEveryDirectoryAfterWhatIsInIt holds the order a tree is
// handed to the next boot in. The session manager deletes a directory only
// once it is empty and carries the deletions out in the order they were handed
// over, so a directory ahead of anything in it would stay.
func TestAtRebootOrderPutsEveryDirectoryAfterWhatIsInIt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")

	writeFile(t, filepath.Join(root, "logs", "tunnel-manager.log"), "")
	writeFile(t, filepath.Join(root, "logs", "old", "tunnel-manager-1.log"), "")
	writeFile(t, filepath.Join(root, "keys", "tunnel-manager.key"), "")

	order, err := atRebootOrder(root)
	if err != nil {
		t.Fatalf("reading the tree failed: %v", err)
	}

	want := map[string]bool{
		root:                               true,
		filepath.Join(root, "logs"):        true,
		filepath.Join(root, "logs", "old"): true,
		filepath.Join(root, "keys"):        true,
		filepath.Join(root, "logs", "tunnel-manager.log"):          true,
		filepath.Join(root, "logs", "old", "tunnel-manager-1.log"): true,
		filepath.Join(root, "keys", "tunnel-manager.key"):          true,
	}

	if len(order) != len(want) {
		t.Fatalf("the order holds %d paths, want %d: %v", len(order), len(want), order)
	}

	position := map[string]int{}

	for i, path := range order {
		if !want[path] {
			t.Fatalf("the order holds %s, which is not in the tree", path)
		}

		position[path] = i
	}

	for path, i := range position {
		for other, j := range position {
			if other != path && strings.HasPrefix(other, path+string(filepath.Separator)) && j > i {
				t.Errorf("%s is handed over before %s, which is in it", path, other)
			}
		}
	}

	if order[len(order)-1] != root {
		t.Errorf("the last path is %s, want the directory itself", order[len(order)-1])
	}

	t.Logf("order: %v", order)
}

// stubRemoveAtReboot puts a recorder in place of the deletion at the next boot
// for one test, and answers the paths it was handed.
func stubRemoveAtReboot(t *testing.T, failOn string) *[]string {
	t.Helper()

	var handed []string

	before := removeAtReboot
	removeAtReboot = func(path string) error {
		if path == failOn {
			return errors.New("refused")
		}

		handed = append(handed, path)

		return nil
	}

	t.Cleanup(func() {
		removeAtReboot = before
	})

	return &handed
}

// heldDataDir is a data directory RemoveAll cannot take all of: one file in it
// cannot be removed, and everything else goes. On Windows the file is held
// open, which is the log file the uninstall could not delete there. Elsewhere
// an open file is removed all the same, and a directory without the write bit
// stands in for it.
func heldDataDir(t *testing.T) (selfRemovalFixture, string) {
	t.Helper()

	fixture := newSelfRemovalFixture(t)
	held := writeFile(t, filepath.Join(fixture.dataDir, "logs", "held", "tunnel-manager.log"), "")

	if runtime.GOOS == "windows" {
		file, err := os.Open(held)
		if err != nil {
			t.Fatalf("failed to open %s: %v", held, err)
		}

		t.Cleanup(func() {
			_ = file.Close()
		})

		return fixture, held
	}

	if os.Geteuid() == 0 {
		t.Skip("root removes a file from a directory without the write bit, so nothing is held")
	}

	err := os.Chmod(filepath.Dir(held), 0o500)
	if err != nil {
		t.Fatalf("failed to take the write bit off %s: %v", filepath.Dir(held), err)
	}

	t.Cleanup(func() {
		_ = os.Chmod(filepath.Dir(held), 0o700)
	})

	return fixture, held
}

// TestRemoveDataDirHandsWhatIsLeftToTheNextBoot is the uninstall on Windows
// with a file some other handle keeps open: what RemoveAll could not take goes
// at the next boot, every directory after what is in it, and the answer says
// so instead of reporting the data directory as left behind.
func TestRemoveDataDirHandsWhatIsLeftToTheNextBoot(t *testing.T) {
	fixture, held := heldDataDir(t)
	handed := stubRemoveAtReboot(t, "")

	part := removeDataDir(fixture.databaseFile)

	if !part.Removed || !part.AtReboot || part.Err != nil || part.Path != fixture.dataDir {
		t.Fatalf("the data directory is reported as %+v, want %s removed at the next reboot",
			part, fixture.dataDir)
	}

	want := []string{
		held,
		filepath.Dir(held),
		filepath.Join(fixture.dataDir, "logs"),
		fixture.dataDir,
	}

	if strings.Join(*handed, "\n") != strings.Join(want, "\n") {
		t.Fatalf("handed to the next boot:\n%s\nwant:\n%s", strings.Join(*handed, "\n"), strings.Join(want, "\n"))
	}

	if there(t, filepath.Join(fixture.dataDir, "logs", "kept-by-hand.txt")) {
		t.Errorf("a file that could be removed was left for the next boot")
	}
}

// TestRemoveDataDirSaysWhatCouldNotBeHandedOver covers a deletion at the next
// boot that was refused as well: the data directory is reported as left, with
// both reasons.
func TestRemoveDataDirSaysWhatCouldNotBeHandedOver(t *testing.T) {
	fixture, held := heldDataDir(t)
	stubRemoveAtReboot(t, held)

	part := removeDataDir(fixture.databaseFile)

	if part.Removed || part.AtReboot || part.Err == nil {
		t.Fatalf("the data directory is reported as %+v, want it failed", part)
	}

	if !strings.Contains(part.Err.Error(), "next reboot") {
		t.Errorf("the reason %q does not say the deletion at the next reboot was refused", part.Err)
	}
}

// TestRemoveDataDirWithoutADeletionAtReboot is every platform but Windows,
// where a file in use is removed all the same and nothing is handed to a next
// boot: a data directory that could not go is reported as left.
func TestRemoveDataDirWithoutADeletionAtReboot(t *testing.T) {
	fixture, _ := heldDataDir(t)

	before := removeAtReboot
	removeAtReboot = nil

	t.Cleanup(func() {
		removeAtReboot = before
	})

	part := removeDataDir(fixture.databaseFile)

	if part.Removed || part.AtReboot || part.Err == nil {
		t.Fatalf("the data directory is reported as %+v, want it failed", part)
	}
}
