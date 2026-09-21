//go:build windows

package install

import (
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

// These tests are what stands in for running this on Windows, which has not
// been done. They cover what is decided in this package and can therefore be
// wrong here: the command line a registration carries, reading it back, and the
// values a service is registered with. What the service control manager does
// with any of it is not covered by anything and has to be checked on a Windows
// machine.

// TestParseServiceCommand holds the reading of BinaryPathName to the shapes it
// actually arrives in.
//
// The first case is the default install and is the one that matters most: the
// executable is under Program Files, so the path has a space in it and the SCM
// hands the line back with the quotes it was written with. A reader that merely
// split on spaces would answer C:\Program, and an uninstall would then find
// nothing to remove and say the installation was not there.
func TestParseServiceCommand(t *testing.T) {
	cases := []struct {
		name       string
		line       string
		executable string
		database   string
	}{
		{
			name:       "the default install",
			line:       `"C:\Program Files\tunnel-manager\tunnel-manager.exe" -db C:\ProgramData\tunnel-manager\tunnel-manager.db`,
			executable: `C:\Program Files\tunnel-manager\tunnel-manager.exe`,
			database:   `C:\ProgramData\tunnel-manager\tunnel-manager.db`,
		},
		{
			name:       "a database path with a space in it",
			line:       `"C:\Program Files\tunnel-manager\tunnel-manager.exe" -db "D:\Tunnel Data\tunnel-manager.db"`,
			executable: `C:\Program Files\tunnel-manager\tunnel-manager.exe`,
			database:   `D:\Tunnel Data\tunnel-manager.db`,
		},
		{
			name:       "nothing quoted",
			line:       `C:\tm\tunnel-manager.exe -db C:\tm\tm.db`,
			executable: `C:\tm\tunnel-manager.exe`,
			database:   `C:\tm\tm.db`,
		},
		{
			name:       "no -db at all, which is the process working out its own default",
			line:       `"C:\Program Files\tunnel-manager\tunnel-manager.exe"`,
			executable: `C:\Program Files\tunnel-manager\tunnel-manager.exe`,
			database:   "",
		},
		{
			name:       "registered by hand with sc.exe, one dash and an equals sign",
			line:       `"C:\Program Files\tunnel-manager\tunnel-manager.exe" -db=C:\tm\tm.db`,
			executable: `C:\Program Files\tunnel-manager\tunnel-manager.exe`,
			database:   `C:\tm\tm.db`,
		},
		{
			name:       "registered by hand with two dashes",
			line:       `C:\tm\tunnel-manager.exe --db C:\tm\tm.db`,
			executable: `C:\tm\tunnel-manager.exe`,
			database:   `C:\tm\tm.db`,
		},
		{
			name:       "runs of spaces between the arguments",
			line:       `  "C:\Program Files\tunnel-manager\tunnel-manager.exe"   -db    "D:\Tunnel Data\tm.db"  `,
			executable: `C:\Program Files\tunnel-manager\tunnel-manager.exe`,
			database:   `D:\Tunnel Data\tm.db`,
		},
		{
			name:       "a share rather than a drive, where the path opens with two backslashes",
			line:       `\\build\release\tunnel-manager.exe -db C:\tm\tm.db`,
			executable: `\\build\release\tunnel-manager.exe`,
			database:   `C:\tm\tm.db`,
		},
		{
			name:       "another flag in front of -db",
			line:       `"C:\Program Files\tunnel-manager\tunnel-manager.exe" -reset-settings -db C:\tm\tm.db`,
			executable: `C:\Program Files\tunnel-manager\tunnel-manager.exe`,
			database:   `C:\tm\tm.db`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			executable, database, err := parseServiceCommand(c.line)
			if err != nil {
				t.Fatalf("reading %q failed: %v", c.line, err)
			}

			if executable != c.executable {
				t.Errorf("executable of %q is %q, want %q", c.line, executable, c.executable)
			}

			if database != c.database {
				t.Errorf("database of %q is %q, want %q", c.line, database, c.database)
			}

			t.Logf("line       %s\nexecutable %s\ndatabase   %s", c.line, executable, database)
		})
	}
}

// TestParseServiceCommandWithoutACommand checks that a registration with
// nothing in it is a failure and not an empty answer. An empty executable path
// handed back as though it had been read would be a path the uninstall goes on
// to act on.
func TestParseServiceCommandWithoutACommand(t *testing.T) {
	_, _, err := parseServiceCommand("   ")
	if err == nil {
		t.Fatal("reading an empty command line answered without an error")
	}

	t.Log(err)
}

// TestCommandLineForTheDefaultInstall holds the string that is written into the
// registration.
//
// It is written out rather than built from the plan, for the reason the default
// paths are in install_test.go: this is the one record of where an installation
// is, and a change to how it is spelled is a change an uninstall reads back.
func TestCommandLineForTheDefaultInstall(t *testing.T) {
	plan := plannedFor("windows")

	const want = `"C:\Program Files\tunnel-manager\tunnel-manager.exe" -db C:\ProgramData\tunnel-manager\tunnel-manager.db`

	line := commandLineFor(plan.ExecutablePath, serviceArguments(plan))
	if line != want {
		t.Errorf("the registration carries %q, want %q", line, want)
	}

	t.Log(line)
}

// TestCommandLineRoundTrip is the pair of the two above: whatever paths an
// operator names, what is written has to read back as the paths that were
// written. It is what stops the writer and the reader drifting apart over a
// path neither of the tables above happens to hold.
func TestCommandLineRoundTrip(t *testing.T) {
	plans := []Plan{
		plannedFor("windows"),
		{
			ExecutablePath: `D:\Tunnel Manager\bin\tunnel-manager.exe`,
			DatabaseFile:   `D:\Tunnel Manager\data\tunnel-manager.db`,
		},
		{
			ExecutablePath: `C:\tm\tunnel-manager.exe`,
			DatabaseFile:   `C:\tm\tm.db`,
		},
		{
			// A trailing backslash is the one shape where the rules for the
			// first argument and the rules for the rest differ, and EscapeArg
			// writes it as a quoted run of doubled backslashes.
			ExecutablePath: `C:\Program Files\tunnel-manager\tunnel-manager.exe`,
			DatabaseFile:   `D:\Tunnel Data\`,
		},
	}

	for _, plan := range plans {
		t.Run(plan.ExecutablePath+" "+plan.DatabaseFile, func(t *testing.T) {
			line := commandLineFor(plan.ExecutablePath, serviceArguments(plan))

			executable, database, err := parseServiceCommand(line)
			if err != nil {
				t.Fatalf("reading back %q failed: %v", line, err)
			}

			if executable != plan.ExecutablePath {
				t.Errorf("%q read back as executable %q, want %q", line, executable, plan.ExecutablePath)
			}

			if database != plan.DatabaseFile {
				t.Errorf("%q read back as database %q, want %q", line, database, plan.DatabaseFile)
			}

			t.Log(line)
		})
	}
}

// TestServiceConfig holds every value the service is registered with.
//
// These are the SCM half of the design's table for Windows - automatic start,
// the LocalSystem account - and none of them can be checked on this machine by
// registering anything, so they are checked as the values that would be handed
// over.
func TestServiceConfig(t *testing.T) {
	config := serviceConfig(plannedFor("windows"))

	if config.ServiceType != windows.SERVICE_WIN32_OWN_PROCESS {
		t.Errorf("service type is %d, want SERVICE_WIN32_OWN_PROCESS (%d)",
			config.ServiceType, windows.SERVICE_WIN32_OWN_PROCESS)
	}

	if config.StartType != mgr.StartAutomatic {
		t.Errorf("start type is %d, want mgr.StartAutomatic (%d)", config.StartType, mgr.StartAutomatic)
	}

	if config.ErrorControl != mgr.ErrorNormal {
		t.Errorf("error control is %d, want mgr.ErrorNormal (%d)", config.ErrorControl, mgr.ErrorNormal)
	}

	// The empty account is the whole of how LocalSystem is asked for, so it is
	// checked as such. A name here would be a service running as somebody else
	// with a password this program would have had to invent.
	if config.ServiceStartName != "" {
		t.Errorf("the account is %q, want it empty, which is LocalSystem", config.ServiceStartName)
	}

	if config.Password != "" {
		t.Errorf("a password is set on the registration, %q", config.Password)
	}

	if config.DisplayName != serviceDisplayName {
		t.Errorf("display name is %q, want %q", config.DisplayName, serviceDisplayName)
	}

	if config.Description == "" {
		t.Error("the registration carries no description, so services.msc shows an empty column for it")
	}

	if config.DelayedAutoStart {
		t.Error("the service is registered as a delayed automatic start, which the design does not ask for")
	}

	const wantPath = `"C:\Program Files\tunnel-manager\tunnel-manager.exe" -db C:\ProgramData\tunnel-manager\tunnel-manager.db`

	if config.BinaryPathName != wantPath {
		t.Errorf("binary path is %q, want %q", config.BinaryPathName, wantPath)
	}

	t.Logf("service type   %d\nstart type     %d\nerror control  %d\naccount        %q (empty is LocalSystem)\n"+
		"display name   %s\ndescription    %s\nbinary path    %s",
		config.ServiceType, config.StartType, config.ErrorControl, config.ServiceStartName,
		config.DisplayName, config.Description, config.BinaryPathName)
}

// TestFailureActions holds what the service does after it fails, which is the
// design's sc failure restart/5000 line.
//
// The two fields of the structure are in two different units - the delay of an
// action is milliseconds and the reset period is seconds - so the numbers are
// written out here as the numbers the SCM is handed, which is the mistake this
// is here to catch.
func TestFailureActions(t *testing.T) {
	failure, actions := failureActions()

	const wantReset = 24 * 60 * 60

	if failure.ResetPeriod != wantReset {
		t.Errorf("reset period is %d, want %d seconds, which is a day", failure.ResetPeriod, wantReset)
	}

	if failure.ActionsCount != uint32(len(actions)) {
		t.Errorf("the count says %d actions and there are %d", failure.ActionsCount, len(actions))
	}

	if len(actions) != 3 {
		t.Fatalf("there are %d actions, want 3 restarts", len(actions))
	}

	if failure.Actions != &actions[0] {
		t.Error("the structure does not point at the actions it was answered with")
	}

	// Nothing else is set. A command would be run as LocalSystem and a reboot
	// would take the machine down over one service failing to start.
	if failure.Command != nil || failure.RebootMsg != nil {
		t.Error("the failure actions carry a command or a reboot message, and neither was asked for")
	}

	const wantDelay = 5000

	for i, action := range actions {
		if action.Type != windows.SC_ACTION_RESTART {
			t.Errorf("action %d is type %d, want SC_ACTION_RESTART (%d)", i, action.Type, windows.SC_ACTION_RESTART)
		}

		if action.Delay != wantDelay {
			t.Errorf("action %d waits %d, want %d milliseconds", i, action.Delay, wantDelay)
		}
	}

	t.Logf("reset period %d seconds\nactions      %d x restart after %d ms",
		failure.ResetPeriod, len(actions), actions[0].Delay)
}

// TestPollLimitIsBounded checks that the wait for a start or a stop ends.
//
// A service that never reaches the state it was asked for is the case this
// covers: the wait is a count of turns, and a count of zero or a limit of hours
// would leave an install sitting there with nothing on the console.
func TestPollLimitIsBounded(t *testing.T) {
	if servicePollLimit <= 0 {
		t.Fatalf("the poll limit is %d, so nothing would ever be waited for", servicePollLimit)
	}

	longest := time.Duration(servicePollLimit) * servicePollInterval
	if longest > time.Minute {
		t.Errorf("a start or a stop is waited for up to %s, which is longer than an operator waits "+
			"in front of a console with nothing written on it", longest)
	}

	t.Logf("%d turns of %s, at most %s", servicePollLimit, servicePollInterval, longest)
}
