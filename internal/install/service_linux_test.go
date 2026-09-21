//go:build linux

package install

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestExecStartExecutable holds the reading of a registration to what systemctl
// on this machine actually answers.
//
// The first case is the output of the deployed service, copied as it was
// printed. What is read out of it is the executable and nothing else: the -db
// of the command line used to be read here too and is not any more, because a
// unit does not have to carry one and the file the process actually opened is
// asked of the process itself (see execStartExecutable and OpenFiles). The
// cases that were here for the spellings of that flag went with it.
func TestExecStartExecutable(t *testing.T) {
	cases := []struct {
		name       string
		value      string
		executable string
		fails      bool
	}{
		{
			name: "the deployed service",
			value: "{ path=/usr/local/bin/tunnel-manager ; argv[]=/usr/local/bin/tunnel-manager " +
				"-db /var/lib/tunnel-manager/tunnel-manager.db ; ignore_errors=no ; " +
				"start_time=[Mon 2026-09-21 02:44:10 KST] ; stop_time=[n/a] ; pid=219825 ; " +
				"code=(null) ; status=0/0 }",
			executable: "/usr/local/bin/tunnel-manager",
		},
		{
			name:       "no -db at all",
			value:      "{ path=/usr/local/bin/tunnel-manager ; argv[]=/usr/local/bin/tunnel-manager ; ignore_errors=no }",
			executable: "/usr/local/bin/tunnel-manager",
		},
		{
			name: "several ExecStart lines, the first is the one",
			value: "{ path=/usr/local/bin/first ; argv[]=/usr/local/bin/first -db /first.db ; ignore_errors=no }\n" +
				"{ path=/usr/local/bin/second ; argv[]=/usr/local/bin/second -db /second.db ; ignore_errors=no }",
			executable: "/usr/local/bin/first",
		},
		{
			name:  "nothing that looks like a command line",
			value: "{ path=/usr/local/bin/tunnel-manager ; ignore_errors=no }",
			fails: true,
		},
		{
			name:  "a command line with no words in it",
			value: "{ path=/usr/local/bin/tunnel-manager ; argv[]= ; ignore_errors=no }",
			fails: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			executable, err := execStartExecutable(c.value)

			if c.fails {
				if err == nil {
					t.Fatalf("execStartExecutable(%q) answered %q, want a failure", c.value, executable)
				}

				t.Logf("refused as it should: %v", err)

				return
			}

			if err != nil {
				t.Fatalf("execStartExecutable(%q) failed: %v", c.value, err)
			}

			if executable != c.executable {
				t.Errorf("executable is %q, want %q", executable, c.executable)
			}
		})
	}
}

// TestUnitFile writes out the two units this can produce, whole.
//
// They are written out rather than built from the pieces, for the reason the
// default paths are in install_test.go: a unit built from the same code that
// builds the unit passes whatever that code does. What has to be caught is a
// directive that quietly went missing - Restart=always, TimeoutStopSec, or the
// LimitNOFILE that is the only way this service gets more descriptors than the
// machine hands out by default - and that only shows up against a file somebody
// read and agreed with.
func TestUnitFile(t *testing.T) {
	defaults := `# Written by tunnel-manager -install. Anything changed here is written over by
# the next install.
[Unit]
Description=Tunnel Manager Service
After=network.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/tunnel-manager -db /var/lib/tunnel-manager/tunnel-manager.db
StateDirectory=tunnel-manager

Restart=always
RestartSec=5
TimeoutStopSec=90

# The descriptor limit of this service alone. The process cannot raise its
# own hard limit, so the one it runs with is the one set here.
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
`

	// StateDirectory= is gone here and nowhere else has changed. systemd
	// resolves that name under /var/lib and refuses anything else, so a unit
	// that kept the directive for /opt would not load at all.
	elsewhere := `# Written by tunnel-manager -install. Anything changed here is written over by
# the next install.
[Unit]
Description=Tunnel Manager Service
After=network.target

[Service]
Type=simple
User=root
ExecStart=/opt/tm/tunnel-manager -db /opt/tm/tm.db

Restart=always
RestartSec=5
TimeoutStopSec=90

# The descriptor limit of this service alone. The process cannot raise its
# own hard limit, so the one it runs with is the one set here.
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
`

	cases := []struct {
		name string
		plan Plan
		want string
	}{
		{
			name: "the default paths",
			plan: plannedFor("linux"),
			want: defaults,
		},
		{
			name: "a data directory outside /var/lib",
			plan: Plan{
				ExecutablePath: "/opt/tm/tunnel-manager",
				DataDir:        "/opt/tm",
				DatabaseFile:   "/opt/tm/tm.db",
			},
			want: elsewhere,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := unitFile(c.plan)

			if got != c.want {
				t.Errorf("the unit for %+v is\n%s\nwant\n%s", c.plan, got, c.want)
			}

			t.Logf("\n%s", got)
		})
	}
}

// TestStateDirectoryName covers the answers the unit above is built on, for the
// paths that are on the edge of the rule rather than plainly one side of it.
func TestStateDirectoryName(t *testing.T) {
	cases := []struct {
		dataDir string
		want    string
	}{
		{dataDir: "/var/lib/tunnel-manager", want: "tunnel-manager"},
		{dataDir: "/var/lib/tunnel-manager/", want: "tunnel-manager"},
		{dataDir: "/var/lib//tunnel-manager", want: "tunnel-manager"},
		// A directory deeper than one level is not a name StateDirectory could
		// be given, since the name it takes is resolved directly under /var/lib.
		{dataDir: "/var/lib/tm/data", want: ""},
		{dataDir: "/var/lib", want: ""},
		{dataDir: "/opt/tm", want: ""},
		{dataDir: "/Library/Application Support/tunnel-manager", want: ""},
	}

	for _, c := range cases {
		got := stateDirectoryName(c.dataDir)

		if got != c.want {
			t.Errorf("stateDirectoryName(%q) is %q, want %q", c.dataDir, got, c.want)
		}
	}
}

// TestCheckUnitPaths covers what Register refuses before it writes anything.
func TestCheckUnitPaths(t *testing.T) {
	cases := []struct {
		name   string
		plan   Plan
		refuse bool
	}{
		{name: "the default paths", plan: plannedFor("linux")},
		{
			name:   "a relative executable",
			plan:   Plan{ExecutablePath: "tunnel-manager", DatabaseFile: "/var/lib/tm.db"},
			refuse: true,
		},
		{
			name:   "a space in the executable path",
			plan:   Plan{ExecutablePath: "/opt/tunnel manager/tunnel-manager", DatabaseFile: "/opt/tm.db"},
			refuse: true,
		},
		{
			name:   "a space in the database path",
			plan:   Plan{ExecutablePath: "/usr/local/bin/tunnel-manager", DatabaseFile: "/opt/tm data/tm.db"},
			refuse: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkUnitPaths(c.plan)

			switch {
			case c.refuse && err == nil:
				t.Errorf("checkUnitPaths(%+v) allowed it, want a refusal", c.plan)
			case !c.refuse && err != nil:
				t.Errorf("checkUnitPaths(%+v) refused it: %v", c.plan, err)
			case c.refuse:
				t.Logf("refused as it should: %v", err)
			}
		})
	}
}

// pathCheckTestUnit is the unit name the refusal below asks systemd about. It
// is a name of its own like every other unit in this file, so that nothing here
// can read - let alone write - the registration of a service that is deployed
// on the machine the tests run on.
const pathCheckTestUnit = serviceName + "-pathcheck" + unitSuffix

// TestInstallRefusesAUnitPathBeforeTheExecutableIsCopied drives the real
// systemd backend with a database path a unit cannot carry, and holds the state
// the refused install leaves the machine in.
//
// Nothing here is registered or started: the refusal comes before any of that,
// and the only systemctl this runs is the read of a unit name nothing owns. The
// refusal itself is TestCheckUnitPaths' to cover. What is covered here is when
// it happens - before the executable is copied. It used to happen inside
// Register, which is after the copy, so an operator whose path was refused was
// left with a new binary at a path no registration names and no way to tell it
// from an install that worked.
func TestInstallRefusesAUnitPathBeforeTheExecutableIsCopied(t *testing.T) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skipf("the install reads the registration of this system first, and systemctl is not here: %v", err)
	}

	allowPrivilege(t)

	dir := t.TempDir()

	// A space in the directory the database is in, which is what a unit cannot
	// carry in a way an uninstall could read back.
	plan := Plan{
		ExecutablePath: filepath.Join(dir, "bin", executableName),
		DataDir:        filepath.Join(dir, "tm data"),
		DatabaseFile:   filepath.Join(dir, "tm data", databaseFileName),
	}

	source := writeFile(t, filepath.Join(dir, "downloaded"), "the new build")

	_, err := install(systemd{unit: pathCheckTestUnit}, plan, source, "the test", &strings.Builder{})
	if err == nil {
		t.Fatal("the install went ahead with a database path a unit cannot carry")
	}

	if _, statErr := os.Stat(plan.ExecutablePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the executable was copied to %s before the path was refused: %v", plan.ExecutablePath, statErr)
	}

	if _, statErr := os.Stat(plan.DataDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the data directory %s was made before the path was refused: %v", plan.DataDir, statErr)
	}

	t.Logf("refused as it should: %v", err)
}

// roundTripEnv is what has to be set for the round trip below to run.
//
// Being root is not enough on its own to ask for it: it registers a unit with
// systemd and starts it, and somebody running the test suite as root for
// another reason has not agreed to that happening on their machine.
const roundTripEnv = "TM_INSTALL_SYSTEMD_ROUNDTRIP"

// roundTripUnit is the unit the round trip uses. It is deliberately not the
// name this program installs under: the machine this is developed on runs that
// service, and a test that registered, started and removed it would take the
// deployment down and leave it removed.
const roundTripUnit = serviceName + "-installtest" + unitSuffix

// TestSystemdRoundTrip registers, reads back, starts, stops and removes a unit
// through the backend itself.
//
// This is the part of a service backend that cannot be checked by holding
// strings against each other. Whether systemd takes the unit, whether
// FragmentPath comes back pointing at the file that was written, whether the
// service is up when Start answers - none of that is visible from the file, and
// all of it is what an install is judged by.
func TestSystemdRoundTrip(t *testing.T) {
	if os.Getenv(roundTripEnv) != "1" {
		t.Skipf("set %s=1 to register a %s with systemd", roundTripEnv, roundTripUnit)
	}

	if os.Geteuid() != 0 {
		t.Skipf("registering a unit needs root, this is uid %d", os.Geteuid())
	}

	svc := systemd{unit: roundTripUnit}

	// Written out rather than built from unitDir, for the reason the default
	// paths in install_test.go are written out: built from the constant, this
	// would pass for whatever the constant happens to be, and where the unit
	// goes is exactly what has to be caught if it changes. The link is where
	// enabling puts it, which is the other half of being registered - a unit
	// with no link from multi-user.target does not come back after a reboot.
	unitPath := "/lib/systemd/system/" + roundTripUnit
	wantsLink := "/etc/systemd/system/multi-user.target.wants/" + roundTripUnit

	// Registered even before anything was written, so that a run which fails
	// halfway leaves nothing behind either.
	t.Cleanup(func() {
		_, _ = svc.run(jobTimeout, "disable", "--now", svc.unit)
		_ = os.Remove(wantsLink)
		_ = os.Remove(unitPath)
		_, _ = svc.run(commandTimeout, "daemon-reload")
	})

	dir := t.TempDir()
	plan := Plan{
		ExecutablePath: filepath.Join(dir, serviceName+"-installtest"),
		DataDir:        filepath.Join(dir, "data"),
		DatabaseFile:   filepath.Join(dir, "data", databaseFileName),
	}

	err := os.WriteFile(plan.ExecutablePath, []byte(databaseHoldingProgram(t, plan, "the test unit")), executableMode)
	if err != nil {
		t.Fatalf("failed to write the program the test unit starts: %v", err)
	}

	_, err = svc.Current()
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("before registering, Current answered %v, want ErrNotInstalled. Is %s already on this machine?",
			err, roundTripUnit)
	}

	err = svc.Register(plan, nil)
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	t.Logf("systemctl show %s -p FragmentPath -p ExecStart:\n%s",
		svc.unit, systemctlShow(t, svc.unit, "FragmentPath", "ExecStart"))

	current, err := svc.Current()
	if err != nil {
		t.Fatalf("after registering, Current failed: %v", err)
	}

	if !sameFile(t, current.DefinitionPath, unitPath) {
		t.Errorf("the registration is at %q, want %q", current.DefinitionPath, unitPath)
	} else {
		t.Logf("the registration is at %q, which is %q", current.DefinitionPath, unitPath)
	}

	if current.ExecutablePath != plan.ExecutablePath {
		t.Errorf("the registration starts %q, want %q", current.ExecutablePath, plan.ExecutablePath)
	}

	// The unit is where this install writes units, and nowhere else. Asked of
	// the file system and not of systemd, because FragmentPath above is what
	// systemd loaded and this is what is on the disk.
	if _, statErr := os.Stat(unitPath); statErr != nil {
		t.Errorf("the unit is not at %s: %v", unitPath, statErr)
	}

	// And enabling made the link under /etc, which is what makes the service
	// come back after a reboot. It is read with Lstat and Readlink: what is
	// there is a symbolic link to the unit above, and a copy of the file would
	// pass a plain Stat.
	link, linkErr := os.Readlink(wantsLink)
	if linkErr != nil {
		t.Errorf("enabling did not leave a link at %s: %v", wantsLink, linkErr)
	} else if !sameFile(t, link, unitPath) {
		t.Errorf("the link at %s points at %q, want %q", wantsLink, link, unitPath)
	} else {
		t.Logf("%s -> %s", wantsLink, link)
	}

	// What the registration says about the database: nothing. A unit carries a
	// -db and this does not read it any more, because a unit need not carry one
	// at all. It is the running process that is asked, further down.
	if current.DatabaseFile != "" {
		t.Errorf("the registration answered the database %q, want it empty and asked of the process",
			current.DatabaseFile)
	}

	// Register leaves the service registered and not started, which is what the
	// install flow counts on: it copies the executable and starts the service
	// itself, in that order.
	if state := activeState(t, svc.unit); state == "active" {
		t.Errorf("the service is %q straight after Register, want it not started", state)
	}

	err = svc.Start()
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if state := activeState(t, svc.unit); state != "active" {
		t.Errorf("after Start the service is %q, want \"active\"", state)
	} else {
		t.Logf("after Start, systemctl is-active %s says %q", svc.unit, state)
	}

	// The part an uninstall depends on and no fake can show: the main process
	// of a unit systemd is running, read out of systemd, and the files that
	// process actually has open, read out of /proc. This is where the database
	// of an installation comes from.
	database := databaseOfRunningService(t, svc)

	if database != plan.DatabaseFile {
		t.Errorf("the database read out of the running service is %q, want %q", database, plan.DatabaseFile)
	}

	// A service that is not running has no descriptors to read, and that has to
	// be an answer and not a list.
	err = svc.Stop()
	if err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	if state := activeState(t, svc.unit); state == "active" {
		t.Errorf("after Stop the service is still %q", state)
	}

	_, err = svc.OpenFiles()
	if !errors.Is(err, ErrOpenFilesUnknown) {
		t.Errorf("the open files of a service that is stopped answered %v, want %v", err, ErrOpenFilesUnknown)
	} else {
		t.Logf("with the service stopped: %v", err)
	}

	err = svc.Unregister(current)
	if err != nil {
		t.Fatalf("Unregister failed: %v", err)
	}

	_, err = os.Stat(unitPath)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("after Unregister, %s is still there: %v", unitPath, err)
	}

	if shown := systemctlShow(t, svc.unit, "FragmentPath"); strings.TrimSpace(shown) != "FragmentPath=" {
		t.Errorf("after Unregister, systemctl still answers %q", shown)
	}

	// The link under /etc goes with the unit. It is disable that removes it,
	// and a link left pointing at a unit file that is gone is what systemd
	// reports as a broken enablement on the next boot.
	if _, statErr := os.Lstat(wantsLink); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("after Unregister, %s is still there: %v", wantsLink, statErr)
	}

	_, err = svc.Current()
	if !errors.Is(err, ErrNotInstalled) {
		t.Errorf("after Unregister, Current answered %v, want ErrNotInstalled", err)
	}

	// Removing a registration that is not there is the state the caller asked
	// for and not a failure, which is what the uninstall flow relies on.
	err = svc.Unregister(current)
	if err != nil {
		t.Errorf("Unregister of a unit that is already gone failed: %v", err)
	}
}

// openFilesPollAttempts bounds the wait below. Twenty five attempts at the
// interval the backend polls a start with is about five seconds, which is
// nothing against a shell opening three files and everything against a test
// that would otherwise hang.
const openFilesPollAttempts = 25

// databaseOfRunningService reads the database out of the files the running
// service has open, waiting for the program to have opened them.
//
// The wait is the test's and not the backend's. systemctl start answers once
// systemd has forked the process, and is-active says active from that moment,
// which is before that process has opened anything: the stand-in is a shell
// that opens three files and then execs, and reading its descriptors in the
// first instant answers an empty list. This has been seen happen, roughly one
// run in five. A service an uninstall is pointed at has been running since the
// machine came up, so that instant is not a state it is ever read in, and a
// backend that waited would be waiting on behalf of nobody.
func databaseOfRunningService(t *testing.T, svc systemd) string {
	t.Helper()

	var last error

	for attempt := 0; attempt < openFilesPollAttempts; attempt++ {
		openFiles, err := svc.OpenFiles()
		if err != nil {
			t.Fatalf("reading the open files of the started service failed: %v", err)
		}

		database, err := databaseAmong(openFiles)
		if err == nil {
			t.Logf("systemctl show %s -p MainPID:\n%s\nopen files:\n%s",
				svc.unit, systemctlShow(t, svc.unit, "MainPID"), strings.Join(openFiles, "\n"))

			return database
		}

		last = err

		time.Sleep(startPollInterval)
	}

	t.Fatalf("the database could not be read out of the open files of the service after %s: %v",
		openFilesPollAttempts*startPollInterval, last)

	return ""
}

// databaseHoldingProgram is the stand-in for the real binary in the round trips
// here: a program that stays up holding the database and the two files SQLite
// keeps beside it open, and writes the three files first so that it can.
//
// It has to hold them rather than merely be started with -db, because that is
// where an install and an uninstall now read the database of an installation
// from: the descriptors of the running process, and not the command line it was
// registered with. The descriptors a shell opens are not closed on exec, so
// they are still open on the sleep that replaces it.
//
// The real binary is not used for the reason it never was: it would want a port
// to itself and a database it could actually open, which is a second thing that
// can fail and says nothing about the backend. What the comment is for is
// telling two of these apart by their md5.
func databaseHoldingProgram(t *testing.T, plan Plan, comment string) string {
	t.Helper()

	err := os.MkdirAll(plan.DataDir, dataDirMode)
	if err != nil {
		t.Fatalf("failed to make the data directory %s: %v", plan.DataDir, err)
	}

	for _, path := range []string{plan.DatabaseFile, plan.DatabaseFile + walSuffix, plan.DatabaseFile + shmSuffix} {
		err = os.WriteFile(path, []byte("held open by the program the test unit starts"), 0o600)
		if err != nil {
			t.Fatalf("failed to write %s: %v", path, err)
		}
	}

	return "#!/bin/sh\n" +
		"# " + comment + "\n" +
		"exec 3< " + plan.DatabaseFile +
		" 4< " + plan.DatabaseFile + walSuffix +
		" 5< " + plan.DatabaseFile + shmSuffix + "\n" +
		"exec sleep 600\n"
}

// sameFile says whether two paths name the same file on this machine.
//
// /lib is a symbolic link to usr/lib on every system that merged /usr, and
// systemd answers with the path it resolved: the unit this install writes to
// /lib/systemd/system comes back from systemctl as /usr/lib/systemd/system.
// They are one file, and a test that compared the spellings would be failing
// over the name of a symbolic link rather than over where the unit went.
func sameFile(t *testing.T, got string, want string) bool {
	t.Helper()

	if got == want {
		return true
	}

	gotResolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Logf("%s could not be resolved: %v", got, err)

		return false
	}

	wantResolved, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Logf("%s could not be resolved: %v", want, err)

		return false
	}

	return gotResolved == wantResolved
}

// systemctlShow is the test asking systemd directly, rather than through the
// backend. What the backend reads is the thing under test, so the output the
// run is judged by has to come from somewhere else.
func systemctlShow(t *testing.T, unit string, properties ...string) string {
	t.Helper()

	args := []string{"show", unit}
	for _, property := range properties {
		args = append(args, "-p", property)
	}

	return strings.TrimSpace(systemctl(t, args...))
}

// activeState is what systemctl is-active says. Its exit status is not read:
// it is non-zero for every state that is not active, and the state is the
// answer being asked for.
func activeState(t *testing.T, unit string) string {
	t.Helper()

	// A stop is finished when systemctl stop answers, but a service that was
	// just started may still be in activating for a moment, and this is also
	// what the check after Start reads.
	deadline := time.Now().Add(5 * time.Second)

	state := ""

	for {
		state = strings.TrimSpace(systemctl(t, "is-active", unit))
		if state != "activating" || time.Now().After(deadline) {
			return state
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// systemctl runs the command and answers its output, whatever its exit status
// was. It is given the same kind of deadline the backend gives its own calls,
// so that a systemd which is not answering ends the test instead of holding it
// until the whole run is killed.
func systemctl(t *testing.T, args ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "systemctl", args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		// Not fatal on its own: is-active exits non-zero for a service that is
		// not up, which is an answer this test expects to get.
		t.Logf("systemctl %s exited with %v", strings.Join(args, " "), err)
	}

	return string(out)
}
