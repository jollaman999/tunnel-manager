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

// TestParseExecStart holds the reading of a registration to what systemctl on
// this machine actually answers.
//
// The first case is the output of the deployed service, copied as it was
// printed. It is the whole of how an uninstall finds what to remove: read it
// wrong and the removal either misses the installation or takes a path that was
// never installed.
func TestParseExecStart(t *testing.T) {
	cases := []struct {
		name       string
		value      string
		executable string
		database   string
		fails      bool
	}{
		{
			name: "the deployed service",
			value: "{ path=/usr/local/bin/tunnel-manager ; argv[]=/usr/local/bin/tunnel-manager " +
				"-db /var/lib/tunnel-manager/tunnel-manager.db ; ignore_errors=no ; " +
				"start_time=[Mon 2026-09-21 02:44:10 KST] ; stop_time=[n/a] ; pid=219825 ; " +
				"code=(null) ; status=0/0 }",
			executable: "/usr/local/bin/tunnel-manager",
			database:   "/var/lib/tunnel-manager/tunnel-manager.db",
		},
		{
			name:       "no -db at all",
			value:      "{ path=/usr/local/bin/tunnel-manager ; argv[]=/usr/local/bin/tunnel-manager ; ignore_errors=no }",
			executable: "/usr/local/bin/tunnel-manager",
			database:   "",
		},
		{
			name: "-db written with an equals sign",
			value: "{ path=/opt/tm/tunnel-manager ; argv[]=/opt/tm/tunnel-manager -db=/opt/tm/tm.db ; " +
				"ignore_errors=no }",
			executable: "/opt/tm/tunnel-manager",
			database:   "/opt/tm/tm.db",
		},
		{
			name: "other flags around the database",
			value: "{ path=/usr/local/bin/tunnel-manager ; argv[]=/usr/local/bin/tunnel-manager -port 8080 " +
				"--db /srv/tm.db -log /var/log/tm.log ; ignore_errors=no }",
			executable: "/usr/local/bin/tunnel-manager",
			database:   "/srv/tm.db",
		},
		{
			name: "several ExecStart lines, the first is the one",
			value: "{ path=/usr/local/bin/first ; argv[]=/usr/local/bin/first -db /first.db ; ignore_errors=no }\n" +
				"{ path=/usr/local/bin/second ; argv[]=/usr/local/bin/second -db /second.db ; ignore_errors=no }",
			executable: "/usr/local/bin/first",
			database:   "/first.db",
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
			executable, database, err := parseExecStart(c.value)

			if c.fails {
				if err == nil {
					t.Fatalf("parseExecStart(%q) answered %q and %q, want a failure", c.value, executable, database)
				}

				t.Logf("refused as it should: %v", err)

				return
			}

			if err != nil {
				t.Fatalf("parseExecStart(%q) failed: %v", c.value, err)
			}

			if executable != c.executable {
				t.Errorf("executable is %q, want %q", executable, c.executable)
			}

			if database != c.database {
				t.Errorf("database is %q, want %q", database, c.database)
			}
		})
	}
}

// TestUnitFile writes out the two units this can produce, whole.
//
// They are written out rather than built from the pieces, for the reason the
// default paths are in install_test.go: a unit built from the same code that
// builds the unit passes whatever that code does. What has to be caught is a
// directive that quietly went missing - Restart=always or TimeoutStopSec - and
// that only shows up against a file somebody read and agreed with.
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
	unitPath := filepath.Join(unitDir, roundTripUnit)

	// Registered even before anything was written, so that a run which fails
	// halfway leaves nothing behind either.
	t.Cleanup(func() {
		_, _ = svc.run(jobTimeout, "disable", "--now", svc.unit)
		_ = os.Remove(unitPath)
		_, _ = svc.run(commandTimeout, "daemon-reload")
	})

	dir := t.TempDir()
	plan := Plan{
		ExecutablePath: filepath.Join(dir, serviceName+"-installtest"),
		DataDir:        filepath.Join(dir, "data"),
		DatabaseFile:   filepath.Join(dir, "data", databaseFileName),
	}

	// A program that stays up and ignores the arguments it is handed. What is
	// being checked is the registration and not this program, and the real
	// binary would want a database and a port, which is a second thing that
	// could fail and say nothing about the backend.
	err := os.WriteFile(plan.ExecutablePath, []byte("#!/bin/sh\nexec sleep 600\n"), executableMode)
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

	if current.DefinitionPath != unitPath {
		t.Errorf("the registration is at %q, want %q", current.DefinitionPath, unitPath)
	}

	if current.ExecutablePath != plan.ExecutablePath {
		t.Errorf("the registration starts %q, want %q", current.ExecutablePath, plan.ExecutablePath)
	}

	if current.DatabaseFile != plan.DatabaseFile {
		t.Errorf("the registration passes -db %q, want %q", current.DatabaseFile, plan.DatabaseFile)
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

	err = svc.Stop()
	if err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	if state := activeState(t, svc.unit); state == "active" {
		t.Errorf("after Stop the service is still %q", state)
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

// packagedUnitDir is where the distribution's own units live, and where the
// unit of this machine's deployment actually sits. It is only ever written to
// by the test below, which puts one file there and removes it again.
const packagedUnitDir = "/usr/lib/systemd/system"

// fragmentTestUnit is the unit that test plants there. It is a name of its own
// for the reason every other unit here has one, and doubly so: this is the one
// test that writes outside /etc.
const fragmentTestUnit = serviceName + "-fragmenttest" + unitSuffix

// TestInstallWritesOverTheUnitWhereItAlreadyIs installs over a registration
// that lives in /usr/lib/systemd/system.
//
// This is the case the deployment on this machine is in: its unit is in
// /usr/lib and not in /etc. A Register that wrote to /etc anyway would not
// replace that unit but shadow it, leaving two files where systemd takes the
// /etc one and the old one sits there still looking like the installation.
// Nothing about that is visible from the unit that was written; it takes
// systemd being asked which file it loaded.
func TestInstallWritesOverTheUnitWhereItAlreadyIs(t *testing.T) {
	roundTripGate(t, fragmentTestUnit)
	allowPrivilege(t)

	svc := systemd{unit: fragmentTestUnit}
	packagedPath := filepath.Join(packagedUnitDir, fragmentTestUnit)
	etcPath := filepath.Join(unitDir, fragmentTestUnit)

	// Registered before the file is planted, so that a run which fails at any
	// step still takes the unit out of /usr/lib. That directory belongs to the
	// package manager and a file of this test's left behind there is the one
	// piece of this run that would not be cleaned up by a reboot or a reinstall.
	t.Cleanup(func() {
		_, _ = svc.run(jobTimeout, "disable", "--now", svc.unit)
		_ = os.Remove(packagedPath)
		_ = os.Remove(etcPath)
		_, _ = svc.run(commandTimeout, "daemon-reload")
	})

	dir := t.TempDir()
	plan := Plan{
		ExecutablePath: filepath.Join(dir, serviceName+"-fragmenttest"),
		DataDir:        filepath.Join(dir, "data"),
		DatabaseFile:   filepath.Join(dir, "data", databaseFileName),
	}

	// Written by hand rather than by unitFile, because this is the state the
	// machine is put into before the test starts and not something under test.
	// The marker is what says afterwards whether this file was written over or
	// merely left alone. The paths are the ones the install will ask for, so
	// that it is the placing of the unit being tested and not the refusal of an
	// installation registered somewhere else.
	planted := "# planted by the test, standing in for the unit of a distribution package\n" +
		"[Unit]\n" +
		"Description=Tunnel Manager Service, planted by the test\n" +
		"\n" +
		"[Service]\n" +
		"Type=simple\n" +
		"ExecStart=" + plan.ExecutablePath + " -db " + plan.DatabaseFile + "\n" +
		"\n" +
		"[Install]\n" +
		"WantedBy=multi-user.target\n"

	err := os.WriteFile(packagedPath, []byte(planted), unitFileMode)
	if err != nil {
		t.Fatalf("failed to plant a unit at %s: %v", packagedPath, err)
	}

	_, err = svc.run(commandTimeout, "daemon-reload")
	if err != nil {
		t.Fatalf("daemon-reload after planting the unit failed: %v", err)
	}

	if _, err := os.Stat(etcPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s is there before the test installed anything: %v", etcPath, err)
	}

	before, err := svc.Current()
	if err != nil {
		t.Fatalf("the planted unit could not be read back: %v", err)
	}

	if before.DefinitionPath != packagedPath {
		t.Fatalf("systemd loaded the planted unit from %q, want %q", before.DefinitionPath, packagedPath)
	}

	source := writeFile(t, filepath.Join(t.TempDir(), "downloaded"), "#!/bin/sh\nexec sleep 600\n")

	var out strings.Builder

	outcome, err := install(svc, plan, source, "the test", &out)
	if err != nil {
		t.Fatalf("the install over the planted unit failed: %v", err)
	}

	t.Logf("\n%s", out.String())

	if !outcome.Replaced {
		t.Error("the install says nothing was registered before")
	}

	if outcome.Definition != packagedPath {
		t.Errorf("the install reports the registration at %q, want %q", outcome.Definition, packagedPath)
	}

	// The one that decides it: systemd itself saying which file it loaded. A
	// second unit in /etc would win, and this would name that one.
	fragment := systemctlShow(t, svc.unit, "FragmentPath")
	if strings.TrimSpace(fragment) != "FragmentPath="+packagedPath {
		t.Errorf("after the install systemctl answers %q, want FragmentPath=%s", fragment, packagedPath)
	}

	t.Logf("systemctl show %s -p FragmentPath -p ExecStart:\n%s",
		svc.unit, systemctlShow(t, svc.unit, "FragmentPath", "ExecStart"))

	if _, err := os.Stat(etcPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the install put a second unit at %s: %v", etcPath, err)
	}

	// The planted file is gone from under the marker: written over where it
	// was, rather than left as it was beside a new one.
	written, err := os.ReadFile(packagedPath)
	if err != nil {
		t.Fatalf("after the install the unit at %s could not be read: %v", packagedPath, err)
	}

	if strings.Contains(string(written), "planted by the test") {
		t.Errorf("the unit at %s is still the planted one:\n%s", packagedPath, written)
	}

	if !strings.Contains(string(written), "ExecStart="+plan.ExecutablePath) {
		t.Errorf("the unit at %s does not start %s:\n%s", packagedPath, plan.ExecutablePath, written)
	}

	if state := activeState(t, svc.unit); state != "active" {
		t.Errorf("after the install the service is %q, want \"active\"", state)
	} else {
		t.Logf("after the install, systemctl is-active %s says %q", svc.unit, state)
	}
}
