package install

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installedIn puts an installation under a directory of the test - the
// executable, the data directory and a file in it - and answers the
// registration that would name it. Nothing here writes where a real install
// writes.
func installedIn(t *testing.T) (Installed, Plan) {
	t.Helper()

	plan := planIn(t)

	writeFile(t, plan.ExecutablePath, "the installed build")
	writeFile(t, plan.DatabaseFile, "the database")
	writeFile(t, filepath.Join(plan.DataDir, "key"), "the key the passwords are sealed with")

	return Installed{
		ExecutablePath: plan.ExecutablePath,
		DatabaseFile:   plan.DatabaseFile,
		DefinitionPath: "/lib/systemd/system/tunnel-manager.service",
	}, plan
}

// TestUninstallWithNothingRegistered is the case the design names: nothing is
// registered, so nothing is removed and the operator is told that rather than
// left to wonder what an uninstall that printed a report actually did.
//
// It is not a failure. "There is nothing to remove" is the state the command
// was asking for, the way rm -f and systemctl disable answer for something that
// is not there, and an uninstall is run twice often enough - by hand, and by
// whatever script wraps it - that the second run has to end the way the first
// one did. So the answer is nil and the process exits 0; what carries the
// message is the report.
func TestUninstallWithNothingRegistered(t *testing.T) {
	allowPrivilege(t)

	svc := &fakeService{currentErr: ErrNotInstalled}

	var out strings.Builder

	removed, err := uninstall(svc, Removal{AssumeYes: true}, nil, &out)
	if err != nil {
		t.Fatalf("the uninstall failed although there was simply nothing to remove: %v", err)
	}

	if strings.Join(svc.calls, ",") != "current" {
		t.Errorf("the backend was used as %v, want the read alone", svc.calls)
	}

	if removed.Registered || removed.Purged || removed.ExecutableWasThere {
		t.Errorf("the uninstall says it did something: %+v", removed)
	}

	if !removed.NothingToRemove {
		t.Error("the removal does not say there was nothing to remove")
	}

	// The report has to say what to do next, since there is nothing left on
	// the machine that names where an installation was put, and it has to say
	// that nothing happening was the answer and not a failure.
	for _, want := range []string{"-bin", "-db", "nothing to remove", "not a failure"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not hold %q:\n%s", want, out.String())
		}
	}

	t.Logf("\n%s", out.String())
}

// TestUninstallTwiceOverIsTheSameAnswer is why the case above is not a failure.
// The second run of a removal finds nothing registered, and an operator or a
// script that gets a failure out of it goes looking for what went wrong.
func TestUninstallTwiceOverIsTheSameAnswer(t *testing.T) {
	allowPrivilege(t)

	registered, _ := installedIn(t)
	svc := &fakeService{current: registered}

	_, err := uninstall(svc, Removal{AssumeYes: true}, nil, &strings.Builder{})
	if err != nil {
		t.Fatalf("the first uninstall failed: %v", err)
	}

	// What the backend answers once its registration has been taken out.
	svc.current = Installed{}
	svc.currentErr = ErrNotInstalled

	var out strings.Builder

	removed, err := uninstall(svc, Removal{AssumeYes: true}, nil, &out)
	if err != nil {
		t.Fatalf("the second uninstall failed: %v", err)
	}

	if !removed.NothingToRemove {
		t.Errorf("the second run says it had something to remove: %+v", removed)
	}
}

// TestUninstallPurgeWithNothingToRemoveIsStillRefused is the line the case
// above must not cross. -purge asks for data to be destroyed, and with no
// registration and no -db there is nothing saying where that data is. Answering
// "nothing to do" there would leave somebody believing their data was gone.
func TestUninstallPurgeWithNothingToRemoveIsStillRefused(t *testing.T) {
	allowPrivilege(t)

	svc := &fakeService{currentErr: ErrNotInstalled}

	var out strings.Builder

	removed, err := uninstall(svc, Removal{Purge: true, AssumeYes: true}, nil, &out)
	if err == nil {
		t.Fatal("-purge was answered as nothing to do")
	}

	if removed.Purged {
		t.Error("the removal says the data was purged")
	}

	if out.Len() != 0 {
		t.Errorf("something was reported for a command that was refused:\n%s", out.String())
	}

	t.Logf("refused as it should: %v", err)
}

// TestUninstallWithNothingRegisteredButPathsNamed covers the other half of that
// rule: with the paths named by hand there is something to remove, and no
// registration to stop or take out.
func TestUninstallWithNothingRegisteredButPathsNamed(t *testing.T) {
	allowPrivilege(t)

	_, plan := installedIn(t)
	svc := &fakeService{currentErr: ErrNotInstalled}

	var out strings.Builder

	removed, err := uninstall(svc, Removal{ExecutablePath: plan.ExecutablePath, DatabaseFile: plan.DatabaseFile, AssumeYes: true}, nil, &out)
	if err != nil {
		t.Fatalf("the uninstall failed: %v", err)
	}

	if strings.Join(svc.calls, ",") != "current" {
		t.Errorf("the backend was used as %v, want the read alone since nothing was registered", svc.calls)
	}

	if _, err := os.Stat(plan.ExecutablePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the executable named by -bin is still there: %v", err)
	}

	if !removed.ExecutableWasThere {
		t.Error("the removal does not say the executable was there")
	}

	if removed.Registered {
		t.Error("the removal says a registration was taken out, but there was none")
	}

	if removed.DataDir != plan.DataDir {
		t.Errorf("the data directory is %q, want %q", removed.DataDir, plan.DataDir)
	}

	if _, err := os.Stat(plan.DatabaseFile); err != nil {
		t.Errorf("the database was removed without -purge: %v", err)
	}
}

// TestUninstallRemovesAndKeepsTheData is the ordinary run. The order the
// backend is used in is what cannot be seen from the files afterwards: a
// registration taken out before the service was stopped leaves a running
// process with nothing naming it.
func TestUninstallRemovesAndKeepsTheData(t *testing.T) {
	allowPrivilege(t)

	registered, plan := installedIn(t)
	svc := &fakeService{current: registered}

	var out strings.Builder

	removed, err := uninstall(svc, Removal{AssumeYes: true}, nil, &out)
	if err != nil {
		t.Fatalf("the uninstall failed: %v", err)
	}

	want := []string{"current", "stop", "unregister"}
	if strings.Join(svc.calls, ",") != strings.Join(want, ",") {
		t.Errorf("the backend was used as %v, want %v", svc.calls, want)
	}

	if svc.unregistered == nil || *svc.unregistered != registered {
		t.Errorf("the backend was told to unregister %+v, want %+v", svc.unregistered, registered)
	}

	if _, err := os.Stat(plan.ExecutablePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the executable is still at %s: %v", plan.ExecutablePath, err)
	}

	if !removed.Registered || !removed.ExecutableWasThere {
		t.Errorf("the removal does not say what it did: %+v", removed)
	}

	if removed.Purged {
		t.Error("the removal says the data was purged, but -purge was not asked for")
	}

	// The whole point of the default: what is in the data directory is the
	// hosts, the credentials and the key they are sealed with.
	for _, path := range []string{plan.DataDir, plan.DatabaseFile, filepath.Join(plan.DataDir, "key")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was removed without -purge: %v", path, err)
		}
	}

	if !strings.Contains(out.String(), plan.DataDir) || !strings.Contains(out.String(), "left in place") {
		t.Errorf("the report does not say where the data was left:\n%s", out.String())
	}

	t.Logf("\n%s", out.String())
}

// TestUninstallPurge covers the flag that cannot be taken back: the data
// directory goes, and what went is named on the console before it does.
func TestUninstallPurge(t *testing.T) {
	allowPrivilege(t)

	registered, plan := installedIn(t)
	svc := &fakeService{current: registered}

	var out strings.Builder

	removed, err := uninstall(svc, Removal{Purge: true, AssumeYes: true}, nil, &out)
	if err != nil {
		t.Fatalf("the uninstall failed: %v", err)
	}

	if !removed.Purged {
		t.Error("the removal does not say the data was purged")
	}

	if _, err := os.Stat(plan.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the data directory %s is still there: %v", plan.DataDir, err)
	}

	if !strings.Contains(out.String(), "-purge: removing "+plan.DataDir) {
		t.Errorf("the console does not name what -purge removed:\n%s", out.String())
	}

	if !strings.Contains(out.String(), "removed with everything under it") {
		t.Errorf("the report does not say the data went:\n%s", out.String())
	}

	t.Logf("\n%s", out.String())
}

// TestUninstallAsksBeforeItRemovesAnything covers the question, through the
// whole flow and not through askToRemove alone.
//
// What it holds is the order. The list is worked out and the question asked
// while the service is still registered and running, because the database in
// that list comes out of the files the running process has open - a flow that
// stopped the service first would be asking a process that no longer exists.
// So an answer of no has to leave the backend with nothing but the read behind
// it.
func TestUninstallAsksBeforeItRemovesAnything(t *testing.T) {
	allowPrivilege(t)
	atATerminal(t)

	registered, plan := installedIn(t)
	svc := &fakeService{current: registered}

	var out strings.Builder

	removed, err := uninstall(svc, Removal{Purge: true}, strings.NewReader("n\n"), &out)
	if err != nil {
		t.Fatalf("answering no failed: %v", err)
	}

	if !removed.Cancelled {
		t.Errorf("the removal does not say it was cancelled: %+v", removed)
	}

	// Nothing was stopped, nothing was taken out. The read of the registration
	// and the question put to the running service are all that happened, and
	// neither of them changes anything.
	if strings.Join(svc.calls, ",") != "current,openfiles" {
		t.Errorf("the backend was used as %v, want the read and the open files", svc.calls)
	}

	for _, path := range []string{plan.ExecutablePath, plan.DataDir, plan.DatabaseFile} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Errorf("%s was removed although the question was answered no: %v", path, statErr)
		}
	}

	// Asked before it was answered, with the paths in it: the one mistake this
	// catches is a removal pointed at another installation than the operator
	// had in mind, and that is only visible from the paths.
	for _, want := range []string{plan.ExecutablePath, plan.DataDir, plan.DatabaseFile,
		registered.DefinitionPath, "[y/N]"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the question does not name %q:\n%s", want, out.String())
		}
	}

	if !strings.Contains(out.String(), "nothing was removed") {
		t.Errorf("the report does not say nothing was removed:\n%s", out.String())
	}

	t.Logf("\n%s", out.String())
}

// TestUninstallAnsweredYesGoesAhead is the other half of that: the same command
// answered yes removes the installation.
func TestUninstallAnsweredYesGoesAhead(t *testing.T) {
	allowPrivilege(t)
	atATerminal(t)

	registered, plan := installedIn(t)
	svc := &fakeService{current: registered}

	var out strings.Builder

	removed, err := uninstall(svc, Removal{}, strings.NewReader("y\n"), &out)
	if err != nil {
		t.Fatalf("answering yes failed: %v", err)
	}

	if removed.Cancelled || !removed.Registered {
		t.Errorf("the removal did not go ahead: %+v", removed)
	}

	if _, statErr := os.Stat(plan.ExecutablePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the executable is still at %s: %v", plan.ExecutablePath, statErr)
	}

	t.Logf("\n%s", out.String())
}

// TestUninstallWithNobodyToAskIsRefused covers the run from a script: there is
// no terminal, so there is nobody to answer, and without -y it is refused
// before anything is touched.
func TestUninstallWithNobodyToAskIsRefused(t *testing.T) {
	allowPrivilege(t)

	registered, plan := installedIn(t)
	svc := &fakeService{current: registered}

	var out strings.Builder

	// A reader that is not a terminal, which is what the real check answers for
	// anything a script hands over. inputIsTerminal is not replaced here.
	_, err := uninstall(svc, Removal{}, strings.NewReader("y\n"), &out)
	if err == nil {
		t.Fatal("the uninstall went ahead with nobody to ask")
	}

	if strings.Join(svc.calls, ",") != "current" {
		t.Errorf("the backend was used as %v, want the read alone", svc.calls)
	}

	if _, statErr := os.Stat(plan.ExecutablePath); statErr != nil {
		t.Errorf("the executable was removed by a command that was refused: %v", statErr)
	}

	t.Logf("refused as it should: %v", err)
}

// TestUninstallFindsTheDatabaseInTheRunningProcess is what the whole of the
// open files mechanism is for: a registration that passes no -db, and a -purge
// that still removes the right directory.
//
// The registration says nothing about which database the process opened. The
// process has it open, along with the write ahead log beside it, and that pair
// is what says which file it is.
func TestUninstallFindsTheDatabaseInTheRunningProcess(t *testing.T) {
	allowPrivilege(t)

	registered, plan := installedIn(t)
	registered.DatabaseFile = ""

	svc := &fakeService{
		current: registered,
		openFiles: []string{
			plan.DatabaseFile,
			plan.DatabaseFile + walSuffix,
			plan.DatabaseFile + shmSuffix,
			filepath.Join(plan.DataDir, "logs", "tunnel-manager.log"),
		},
	}

	var out strings.Builder

	removed, err := uninstall(svc, Removal{Purge: true, AssumeYes: true}, nil, &out)
	if err != nil {
		t.Fatalf("the uninstall failed: %v", err)
	}

	if removed.DatabaseFile != plan.DatabaseFile {
		t.Errorf("the removal is about the database %q, want %q", removed.DatabaseFile, plan.DatabaseFile)
	}

	if !removed.Purged {
		t.Error("the removal does not say the data was purged")
	}

	if _, statErr := os.Stat(plan.DataDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the data directory %s is still there: %v", plan.DataDir, statErr)
	}

	t.Logf("\n%s", out.String())
}

// TestUninstallPurgeWithoutADatabase covers -purge on a registration that names
// no -db. There is then nothing saying where the data is, and a guess would be
// a RemoveAll of a guessed path.
//
// What this holds is when the refusal comes. It used to come after the service
// had been stopped, its registration taken out and its executable deleted, so
// an operator whose command was refused was left with no service, no binary and
// the data they had asked to have removed - a refusal that had already done the
// three things that cannot be taken back. Now nothing at all is touched, which
// is what the assertions below are: the backend is only read from, and both the
// executable and the data are still where they were.
func TestUninstallPurgeWithoutADatabase(t *testing.T) {
	allowPrivilege(t)

	registered, plan := installedIn(t)
	registered.DatabaseFile = ""

	svc := &fakeService{current: registered}

	// Written to the console as well, so that a strace of this test shows any
	// write the flow makes in the same stream as the syscalls it made. The
	// marker after the call is where the flow ended: everything traced past it
	// is the test framework removing its own temporary directory.
	var out strings.Builder
	console := io.MultiWriter(&out, os.Stdout)

	_, err := uninstall(svc, Removal{Purge: true, AssumeYes: true}, nil, console)

	fmt.Fprintln(os.Stdout, "marker: the uninstall has returned, what follows is this test tidying up")

	if err == nil {
		t.Fatal("-purge went ahead with nothing saying where the data is")
	}

	// Nothing was stopped and nothing was taken out: the read and the question
	// put to the running service, and no more. Asking which files it has open
	// is how a registration that names no database is answered, and it changes
	// nothing on the machine.
	if strings.Join(svc.calls, ",") != "current,openfiles" {
		t.Errorf("the backend was used as %v, want the read and the open files", svc.calls)
	}

	if _, statErr := os.Stat(plan.ExecutablePath); statErr != nil {
		t.Errorf("the executable was removed by a command that was refused: %v", statErr)
	}

	for _, path := range []string{plan.DataDir, plan.DatabaseFile, filepath.Join(plan.DataDir, "key")} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Errorf("%s was removed by a command that was refused: %v", path, statErr)
		}
	}

	// And no report either. A report is the record of what was done, and
	// nothing was done; the refusal itself is the whole of the answer.
	if out.Len() != 0 {
		t.Errorf("something was reported although nothing was done:\n%s", out.String())
	}

	t.Logf("refused as it should: %v", err)
}

// TestUninstallPurgeOfADirectoryThatIsRefusedTouchesNothing is the same rule
// through the other gate: the path is known, and it is one the guard will not
// hand to RemoveAll. The service and the executable have to survive that
// refusal too.
func TestUninstallPurgeOfADirectoryThatIsRefusedTouchesNothing(t *testing.T) {
	allowPrivilege(t)

	registered, plan := installedIn(t)

	// A database directly under the root: the data directory worked out of it
	// is "/", which the guard refuses.
	registered.DatabaseFile = "/tunnel-manager.db"

	svc := &fakeService{current: registered}

	var out strings.Builder

	_, err := uninstall(svc, Removal{Purge: true, AssumeYes: true}, nil, &out)
	if err == nil {
		t.Fatal("-purge went ahead with the root as the data directory")
	}

	// The read and the question put to the running service, and no more.
	// Neither of them changes anything on the machine.
	if strings.Join(svc.calls, ",") != "current,openfiles" {
		t.Errorf("the backend was used as %v, want the read and the open files", svc.calls)
	}

	if _, statErr := os.Stat(plan.ExecutablePath); statErr != nil {
		t.Errorf("the executable was removed by a command that was refused: %v", statErr)
	}

	t.Logf("refused as it should: %v", err)
}

// TestCheckPurgeTarget is the guard on its own, held against the paths that
// must never be handed to RemoveAll.
//
// They are given to the check and not to the flow, deliberately. Driving a
// removal of /var/lib or of a home directory end to end would mean that a
// broken check removes those directories on the machine the tests run on, and
// the round trip of this package is run as root.
func TestCheckPurgeTarget(t *testing.T) {
	cases := []struct {
		name     string
		dataDir  string
		database string
		refuse   bool
	}{
		{
			name:     "the default linux data directory",
			dataDir:  "/var/lib/tunnel-manager",
			database: "/var/lib/tunnel-manager/tunnel-manager.db",
		},
		{
			name:     "a data directory of its own elsewhere",
			dataDir:  "/opt/tm/data",
			database: "/opt/tm/data/tm.db",
		},
		{
			name:     "the default macos data directory",
			dataDir:  "/Library/Application Support/tunnel-manager",
			database: "/Library/Application Support/tunnel-manager/tunnel-manager.db",
		},
		{
			name:     "a directory the database is not in",
			dataDir:  "/var/lib/tunnel-manager",
			database: "/opt/tm/tm.db",
			refuse:   true,
		},
		{
			name:     "the parent of the directory the database is in",
			dataDir:  "/var/lib",
			database: "/var/lib/tunnel-manager/tunnel-manager.db",
			refuse:   true,
		},
		{name: "the root", dataDir: "/", database: "/tunnel-manager.db", refuse: true},
		{name: "a directory under the root", dataDir: "/var", database: "/var/tunnel-manager.db", refuse: true},
		{name: "var lib itself", dataDir: "/var/lib", database: "/var/lib/tunnel-manager.db", refuse: true},
		{name: "usr local", dataDir: "/usr/local", database: "/usr/local/tunnel-manager.db", refuse: true},
		{
			name:     "the macos application support directory itself",
			dataDir:  "/Library/Application Support",
			database: "/Library/Application Support/tunnel-manager.db",
			refuse:   true,
		},
		{name: "a home directory", dataDir: "/home/someone", database: "/home/someone/tm.db", refuse: true},
		{name: "a macos home directory", dataDir: "/Users/someone", database: "/Users/someone/tm.db", refuse: true},
		{
			name:     "the root reached through a dotted path",
			dataDir:  "/var/lib/tunnel-manager/../../..",
			database: "/var/lib/tunnel-manager/../../../tunnel-manager.db",
			refuse:   true,
		},
		{name: "a relative path", dataDir: "data", database: "data/tunnel-manager.db", refuse: true},
		{name: "nothing at all", dataDir: "", database: "", refuse: true},
		{
			name:     "a windows drive",
			dataDir:  `C:\`,
			database: `C:\tunnel-manager.db`,
			refuse:   true,
		},
		{
			name:     "the windows program data directory itself",
			dataDir:  `C:\ProgramData`,
			database: `C:\ProgramData\tunnel-manager.db`,
			refuse:   true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkPurgeTarget(c.dataDir, c.database)

			switch {
			case c.refuse && err == nil:
				t.Errorf("-purge would have removed %q", c.dataDir)
			case !c.refuse && err != nil:
				t.Errorf("-purge was refused %q: %v", c.dataDir, err)
			case c.refuse:
				t.Logf("refused as it should: %v", err)
			}
		})
	}
}

// TestPathElements covers the counting the guard above stands on, including the
// spellings a path arrives in from another platform.
func TestPathElements(t *testing.T) {
	cases := []struct {
		path string
		want []string
	}{
		{path: "/", want: []string{}},
		{path: "/var", want: []string{"var"}},
		{path: "/var/lib//tunnel-manager/", want: []string{"var", "lib", "tunnel-manager"}},
		{path: "/var/lib/tunnel-manager/..", want: []string{"var", "lib"}},
		{path: `C:\`, want: []string{}},
		{path: `C:\ProgramData\tunnel-manager`, want: []string{"ProgramData", "tunnel-manager"}},
		{path: "/Library/Application Support/tunnel-manager", want: []string{"Library", "Application Support", "tunnel-manager"}},
	}

	for _, c := range cases {
		got := pathElements(c.path)

		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("pathElements(%q) is %v, want %v", c.path, got, c.want)
		}
	}
}

// TestWhatToRemove covers which of the two sources each path comes from. The
// registration is what says which files this installation owns, so it is
// believed over the flags wherever it names something.
func TestWhatToRemove(t *testing.T) {
	registered := &Installed{
		ExecutablePath: "/usr/local/bin/tunnel-manager",
		DatabaseFile:   "/var/lib/tunnel-manager/tunnel-manager.db",
		DefinitionPath: "/lib/systemd/system/tunnel-manager.service",
	}

	cases := []struct {
		name       string
		request    Removal
		existing   *Installed
		executable string
		database   string
		dataDir    string
	}{
		{
			name:       "the registration alone",
			existing:   registered,
			executable: registered.ExecutablePath,
			database:   registered.DatabaseFile,
			dataDir:    "/var/lib/tunnel-manager",
		},
		{
			name:       "the flags do not move a removal that is registered",
			request:    Removal{ExecutablePath: "/opt/tm/tunnel-manager", DatabaseFile: "/opt/tm/tm.db"},
			existing:   registered,
			executable: registered.ExecutablePath,
			database:   registered.DatabaseFile,
			dataDir:    "/var/lib/tunnel-manager",
		},
		{
			name:       "a registration that passes no database",
			request:    Removal{DatabaseFile: "/opt/tm/tm.db"},
			existing:   &Installed{ExecutablePath: registered.ExecutablePath},
			executable: registered.ExecutablePath,
			database:   "/opt/tm/tm.db",
			dataDir:    "/opt/tm",
		},
		{
			name:       "nothing registered, the flags say where it is",
			request:    Removal{ExecutablePath: "/opt/tm/tunnel-manager", DatabaseFile: "/opt/tm/tm.db"},
			executable: "/opt/tm/tunnel-manager",
			database:   "/opt/tm/tm.db",
			dataDir:    "/opt/tm",
		},
	}

	// Nothing registered and nothing named is the one answer that is not a set
	// of paths, so it is checked apart from the table above.
	t.Run("nothing registered and nothing named", func(t *testing.T) {
		removed := whatToRemove(Removal{}, nil)

		if !removed.NothingToRemove {
			t.Fatalf("whatToRemove found something to remove: %+v", removed)
		}

		if removed.ExecutablePath != "" || removed.DatabaseFile != "" || removed.DataDir != "" {
			t.Errorf("paths were worked out for a removal with nothing to work on: %+v", removed)
		}
	})

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			removed := whatToRemove(c.request, c.existing)

			if removed.NothingToRemove {
				t.Fatalf("whatToRemove found nothing to remove in %+v", c)
			}

			if removed.ExecutablePath != c.executable {
				t.Errorf("the executable to remove is %q, want %q", removed.ExecutablePath, c.executable)
			}

			if removed.DatabaseFile != c.database {
				t.Errorf("the database is %q, want %q", removed.DatabaseFile, c.database)
			}

			if removed.DataDir != c.dataDir {
				t.Errorf("the data directory is %q, want %q", removed.DataDir, c.dataDir)
			}

			if removed.Source == "" {
				t.Error("the removal does not say where its paths came from")
			}
		})
	}
}

// TestUninstallWithoutPrivilege checks that the refusal comes before the
// backend is reached, the same as it does for an install.
func TestUninstallWithoutPrivilege(t *testing.T) {
	was := privileged
	privileged = func() bool { return false }

	t.Cleanup(func() {
		privileged = was
	})

	svc := &fakeService{currentErr: ErrNotInstalled}

	_, err := uninstall(svc, Removal{Purge: true, AssumeYes: true}, nil, &strings.Builder{})
	if err == nil {
		t.Fatal("the uninstall ran as a process with no privilege")
	}

	if len(svc.calls) != 0 {
		t.Errorf("the backend was used as %v, want nothing", svc.calls)
	}
}

// TestUninstallWithoutABackend covers the platform no backend was written for,
// where the hook is nil and a call on it would panic.
func TestUninstallWithoutABackend(t *testing.T) {
	was := newService
	newService = nil

	t.Cleanup(func() {
		newService = was
	})

	_, err := Uninstall(Removal{}, nil, &strings.Builder{})
	if !errors.Is(err, ErrNoBackend) {
		t.Errorf("the uninstall answered %v, want %v", err, ErrNoBackend)
	}
}

// TestRemovedReport checks that what an operator is left with is in what they
// read: what went, and - the line that is easy to miss - what was kept and
// where.
func TestRemovedReport(t *testing.T) {
	removed := Removed{
		Source:             "the registration of this system",
		Registered:         true,
		ExecutablePath:     "/usr/local/bin/tunnel-manager",
		ExecutableWasThere: true,
		Definition:         "/usr/lib/systemd/system/tunnel-manager.service",
		DataDir:            "/var/lib/tunnel-manager",
		DatabaseFile:       "/var/lib/tunnel-manager/tunnel-manager.db",
	}

	var out strings.Builder

	err := removed.Report(&out)
	if err != nil {
		t.Fatalf("failed to write the report: %v", err)
	}

	for _, want := range []string{
		removed.Source,
		removed.ExecutablePath,
		removed.Definition,
		removed.DataDir,
		removed.DatabaseFile,
		"removed",
		"left in place",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not hold %q:\n%s", want, out.String())
		}
	}

	t.Logf("\n%s", out.String())
}

// TestRemovedReportOfAnExecutableLeftForTheReboot covers the Windows answer.
// An operator told the file was removed, when it is still on disk until the
// machine comes up again, would go looking for what put it back.
func TestRemovedReportOfAnExecutableLeftForTheReboot(t *testing.T) {
	removed := Removed{
		Source:             "the registration of this system",
		Registered:         true,
		ExecutablePath:     `C:\Program Files\tunnel-manager\tunnel-manager.exe`,
		ExecutableWasThere: true,
		ExecutableAtReboot: true,
		DataDir:            `C:\ProgramData\tunnel-manager`,
		DatabaseFile:       `C:\ProgramData\tunnel-manager\tunnel-manager.db`,
	}

	var out strings.Builder

	err := removed.Report(&out)
	if err != nil {
		t.Fatalf("failed to write the report: %v", err)
	}

	if !strings.Contains(out.String(), "removed at the next reboot") {
		t.Errorf("the report does not say the executable is still there until the reboot:\n%s", out.String())
	}

	// Windows keeps no file for the registration, so the line has to name the
	// service instead of being blank.
	if !strings.Contains(out.String(), "the registration of "+serviceName) {
		t.Errorf("the report does not name the registration that went:\n%s", out.String())
	}

	t.Logf("\n%s", out.String())
}

// TestRemovedReportOfAnExecutableThatWasNotThere covers the path that named no
// file. Somebody has already been here, and reading that as "removed" hides it.
func TestRemovedReportOfAnExecutableThatWasNotThere(t *testing.T) {
	var out strings.Builder

	err := Removed{ExecutablePath: "/usr/local/bin/tunnel-manager"}.Report(&out)
	if err != nil {
		t.Fatalf("failed to write the report: %v", err)
	}

	if !strings.Contains(out.String(), "no file was there") {
		t.Errorf("the report does not say the path named no file:\n%s", out.String())
	}

	if !strings.Contains(out.String(), "nothing was registered") {
		t.Errorf("the report does not say there was no registration:\n%s", out.String())
	}
}
