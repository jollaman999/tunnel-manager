package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/install"
)

// installStepsThat stands in for the download and the install. fetchErr fails
// the download, and putErr is what the install answers with once it is reached.
func installStepsThat(t *testing.T, fetchErr error, putErr error) (installSteps, *bool) {
	t.Helper()

	reached := false

	executable := filepath.Join(t.TempDir(), "tunnel-manager")

	err := os.WriteFile(executable, []byte("the new build"), 0o755)
	if err != nil {
		t.Fatalf("failed to write the stand-in for the release: %v", err)
	}

	return installSteps{
		fetch: func(context.Context, string, string) (*install.Fetched, error) {
			if fetchErr != nil {
				return nil, fetchErr
			}

			return &install.Fetched{Path: executable, Source: install.SourceRunning}, nil
		},
		put: func(install.Plan, string, string, io.Writer) (install.Outcome, error) {
			reached = true

			return install.Outcome{}, putErr
		},
	}, &reached
}

// TestARefusedInstallLeavesNoDataDirectory follows -install past the privilege
// in the order runInstall takes it. The lock is taken in the data directory
// before the release is downloaded, and on a machine with no installation yet
// that directory is not there: making it for the lock and leaving it behind is
// what an install that refuses, or one whose download failed, must not do,
// since the report of a refusal says nothing was touched.
func TestARefusedInstallLeavesNoDataDirectory(t *testing.T) {
	cases := []struct {
		name     string
		fetchErr error
		putErr   error
	}{
		{"the install refused", nil, errors.New("another installation is registered at /opt/tm")},
		{"the download failed", errors.New("the release could not be read"), nil},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			// Two levels that are not there, the way -db can name a
			// directory under one that does not exist yet either.
			top := filepath.Join(t.TempDir(), "opt")
			plan := install.Defaults().WithDatabase(filepath.Join(top, "tm", "tm.db"))

			steps, _ := installStepsThat(t, tt.fetchErr, tt.putErr)

			err := installRelease(plan, steps, false, io.Discard)
			if err == nil {
				t.Fatal("the install reported that it went through")
			}

			_, statErr := os.Stat(top)
			if !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("%s is there after the install failed: %v", top, statErr)
			}
		})
	}
}

// TestAnInstallKeepsTheDataDirectoryItWasGiven is the two cases a directory
// stays: one that was there before the install, which is the installation
// being upgraded, and one an install that went through made.
func TestAnInstallKeepsTheDataDirectoryItWasGiven(t *testing.T) {
	t.Run("there before and refused", func(t *testing.T) {
		dataDir := filepath.Join(t.TempDir(), "data")

		err := os.Mkdir(dataDir, 0o700)
		if err != nil {
			t.Fatalf("failed to make the data directory: %v", err)
		}

		plan := install.Defaults().WithDatabase(filepath.Join(dataDir, "tm.db"))
		steps, _ := installStepsThat(t, nil, errors.New("refused"))

		if installRelease(plan, steps, false, io.Discard) == nil {
			t.Fatal("the install reported that it went through")
		}

		assertEmptyDirectory(t, dataDir)
	})

	t.Run("made and installed", func(t *testing.T) {
		dataDir := filepath.Join(t.TempDir(), "opt", "tm")
		plan := install.Defaults().WithDatabase(filepath.Join(dataDir, "tm.db"))
		steps, reached := installStepsThat(t, nil, nil)

		err := installRelease(plan, steps, false, io.Discard)
		if err != nil {
			t.Fatalf("the install failed: %v", err)
		}

		if !*reached {
			t.Fatal("the install was never reached")
		}

		assertEmptyDirectory(t, dataDir)
	})
}

// TestAnUpdateInstallsTheReleaseOrNothing is the one place an install the
// service started and one somebody typed part ways. Where the release cannot be
// used, the typed one puts the running executable in place, and the update
// stops before put, which is the step that stops the service, with the reason.
func TestAnUpdateInstallsTheReleaseOrNothing(t *testing.T) {
	why := "release v3.7.5 carries no SHA256SUMS.sig"

	cases := []struct {
		name       string
		source     install.Source
		fromUpdate bool
		wantPut    bool
	}{
		{"typed, the release cannot be used", install.SourceRunning, false, true},
		{"an update, the release cannot be used", install.SourceRunning, true, false},
		{"an update, the release was checked", install.SourceRelease, true, true},
		{"typed, the release was checked", install.SourceRelease, false, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := install.Defaults().WithDatabase(filepath.Join(t.TempDir(), "data", "tm.db"))

			reached := false

			steps := installSteps{
				fetch: func(context.Context, string, string) (*install.Fetched, error) {
					return &install.Fetched{Path: "/usr/local/bin/tunnel-manager", Source: c.source,
						Tag: "v3.7.5", Verified: c.source == install.SourceRelease, Why: why}, nil
				},
				put: func(install.Plan, string, string, io.Writer) (install.Outcome, error) {
					reached = true

					return install.Outcome{}, nil
				},
			}

			err := installRelease(plan, steps, c.fromUpdate, io.Discard)

			if reached != c.wantPut {
				t.Errorf("put reached = %v, want %v", reached, c.wantPut)
			}

			if c.wantPut && err != nil {
				t.Errorf("the install failed: %v", err)
			}

			if !c.wantPut && (err == nil || !strings.Contains(err.Error(), why)) {
				t.Errorf("the install answered %v, want an error that carries %q", err, why)
			}
		})
	}
}

// TestAnInstallHandsTheFetchTheRunningVersion is what the download is held to.
// A release before the version of this process is not taken (install.Fetch),
// and the only place that version comes from is the constant this binary was
// built with: anything else handed in would let an older release through, or
// turn away the one that is the same.
func TestAnInstallHandsTheFetchTheRunningVersion(t *testing.T) {
	plan := install.Defaults().WithDatabase(filepath.Join(t.TempDir(), "data", "tm.db"))

	asked := "nothing was asked"

	steps := installSteps{
		fetch: func(_ context.Context, _ string, running string) (*install.Fetched, error) {
			asked = running

			return &install.Fetched{Path: "/usr/local/bin/tunnel-manager", Source: install.SourceRunning,
				Why: "release v0.0.1 is older than the running version " + running}, nil
		},
		put: func(install.Plan, string, string, io.Writer) (install.Outcome, error) {
			return install.Outcome{}, nil
		},
	}

	err := installRelease(plan, steps, true, io.Discard)

	if asked != version {
		t.Errorf("the fetch was handed %q as the running version, want %q", asked, version)
	}

	// An update that finds the release older than what runs is one that finds
	// nothing to install, and it stops before the service is touched.
	if err == nil || !strings.Contains(err.Error(), "older than the running version") {
		t.Errorf("the update answered %v, want it refused with the reason", err)
	}
}

// TestAnUpdateThatFailedLeavesWhyForTheUpdateScreen is the reason an install
// the service started ends with, left in the data directory for the service to
// read. An install somebody typed has a terminal to say it on and leaves
// nothing.
func TestAnUpdateThatFailedLeavesWhyForTheUpdateScreen(t *testing.T) {
	cases := []struct {
		name       string
		source     install.Source
		fetchErr   error
		fromUpdate bool
		wantLeft   bool
	}{
		{"an update, the release cannot be used", install.SourceRunning, nil, true, true},
		{"an update, the download failed", install.SourceRelease, errors.New("the release could not be read"), true, true},
		{"an update that went through", install.SourceRelease, nil, true, false},
		{"typed, the download failed", install.SourceRelease, errors.New("the release could not be read"), false, false},
		{"typed, the release cannot be used", install.SourceRunning, nil, false, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := install.Defaults().WithDatabase(filepath.Join(t.TempDir(), "tm.db"))

			steps := installSteps{
				fetch: func(context.Context, string, string) (*install.Fetched, error) {
					if c.fetchErr != nil {
						return nil, c.fetchErr
					}

					return &install.Fetched{Path: "/usr/local/bin/tunnel-manager", Source: c.source,
						Tag: "v3.7.5", Verified: true, Why: "release v3.7.5 carries no SHA256SUMS.sig"}, nil
				},
				put: func(install.Plan, string, string, io.Writer) (install.Outcome, error) {
					return install.Outcome{}, nil
				},
			}

			err := installRelease(plan, steps, c.fromUpdate, io.Discard)

			left := installProblemPath(plan.DataDir)

			said, readErr := os.ReadFile(left)

			if !c.wantLeft {
				if !errors.Is(readErr, fs.ErrNotExist) {
					t.Errorf("the install left %q in %s (read: %v), want nothing there", said, left, readErr)
				}

				return
			}

			if err == nil {
				t.Fatal("the install went through, want it to fail")
			}

			if readErr != nil {
				t.Fatalf("the install left no reason in %s: %v", left, readErr)
			}

			if string(said) != err.Error() {
				t.Errorf("the reason left is %q, want the error the install ended with, %q", said, err.Error())
			}

			requireReportKeptToTheOwner(t, left)

			if install.InstallRunning(plan.DataDir) {
				t.Error("the lock was still held once the install had returned")
			}
		})
	}
}

// TestAnInstallTakesAwayTheReasonAnUpdateBeforeItLeft is a reason an update
// left and an install after it, typed or not. The service that install starts
// answers whatever reason it finds at startup, and the one of the update before
// would be shown as the install's.
func TestAnInstallTakesAwayTheReasonAnUpdateBeforeItLeft(t *testing.T) {
	for _, fromUpdate := range []bool{false, true} {
		plan := install.Defaults().WithDatabase(filepath.Join(t.TempDir(), "tm.db"))

		err := os.WriteFile(installProblemPath(plan.DataDir), []byte("the update before failed"), 0o600)
		if err != nil {
			t.Fatalf("failed to leave the reason of the update before: %v", err)
		}

		steps := installSteps{
			fetch: func(context.Context, string, string) (*install.Fetched, error) {
				return &install.Fetched{Path: "/usr/local/bin/tunnel-manager", Source: install.SourceRelease,
					Tag: "v3.7.5", Verified: true}, nil
			},
			put: func(install.Plan, string, string, io.Writer) (install.Outcome, error) {
				return install.Outcome{}, nil
			},
		}

		err = installRelease(plan, steps, fromUpdate, io.Discard)
		if err != nil {
			t.Fatalf("the install failed: %v", err)
		}

		said, readErr := os.ReadFile(installProblemPath(plan.DataDir))
		if !errors.Is(readErr, fs.ErrNotExist) {
			t.Errorf("from the update = %v: the install went through and %q (read: %v) is still there",
				fromUpdate, said, readErr)
		}
	}
}

// TestAnUpdateRefusedItsPrivilegeLeavesWhy is the one failure that comes
// before installRelease. It is followed only where this test cannot install,
// since where it can the same call would put a service on the machine.
func TestAnUpdateRefusedItsPrivilegeLeavesWhy(t *testing.T) {
	if install.CheckPrivilege() == nil {
		t.Skip("this test runs with the privilege an install needs, and would install")
	}

	for _, fromUpdate := range []bool{true, false} {
		dir := t.TempDir()
		asked := installation{install: true, database: filepath.Join(dir, "tm.db"), databaseNamed: true,
			fromUpdate: fromUpdate}

		err := asked.runInstall(io.Discard)
		if err == nil {
			t.Fatal("an install without the privilege went through")
		}

		said, readErr := os.ReadFile(installProblemPath(dir))

		if fromUpdate && string(said) != err.Error() {
			t.Errorf("the update left %q (read: %v), want %q", said, readErr, err.Error())
		}

		if !fromUpdate && !errors.Is(readErr, fs.ErrNotExist) {
			t.Errorf("a typed install left %q (read: %v), want nothing", said, readErr)
		}
	}
}

// TestFromUpdateGoesWithInstall is the hidden flag given on its own, or with
// -uninstall, which it has nothing to say about.
func TestFromUpdateGoesWithInstall(t *testing.T) {
	cases := []struct {
		name   string
		asked  installation
		refuse bool
	}{
		{"with -install", installation{install: true, fromUpdate: true}, false},
		{"on its own", installation{fromUpdate: true}, true},
		{"with -uninstall", installation{uninstall: true, fromUpdate: true}, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !c.asked.asked() {
				t.Fatal("the command line was not taken for an installation command")
			}

			err := c.asked.check()
			if (err != nil) != c.refuse {
				t.Errorf("check answered %v, want refused = %v", err, c.refuse)
			}
		})
	}
}

// assertEmptyDirectory holds dir to being there with nothing in it: the lock
// the install held is gone with the install.
func assertEmptyDirectory(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("the data directory %s is not there: %v", dir, err)
	}

	if len(entries) != 0 {
		t.Errorf("the data directory still holds %v", entries)
	}
}

// TestInstallSourceSaysWhatWasChecked is the line of the report that says where
// the executable came from. A release the development build took without its
// signature must not read like one that was held to it: the report is all an
// operator or the journal keeps of an install.
func TestInstallSourceSaysWhatWasChecked(t *testing.T) {
	cases := []struct {
		name    string
		fetched install.Fetched
		want    string
	}{
		{"a release whose signature and checksum matched",
			install.Fetched{Source: install.SourceRelease, Tag: "v3.7.5", Verified: true},
			"the v3.7.5 release, signature and checksum verified"},
		{"a release the development build took on its checksum",
			install.Fetched{Source: install.SourceRelease, Tag: "v3.7.5"},
			"the v3.7.5 release, checksum verified, signature not checked (a development build)"},
		{"the running executable",
			install.Fetched{Source: install.SourceRunning, Path: "/usr/local/bin/tunnel-manager", Why: "release v3.7.5 carries no SHA256SUMS.sig"},
			"this running process, /usr/local/bin/tunnel-manager (the release was not used: release v3.7.5 carries no SHA256SUMS.sig)"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := installSource(&c.fetched)
			if got != c.want {
				t.Errorf("installSource = %q, want %q", got, c.want)
			}
		})
	}
}
