package main

import (
	"context"
	"errors"
	"io"
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
		fetch: func(context.Context, string) (*install.Fetched, error) {
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
				fetch: func(context.Context, string) (*install.Fetched, error) {
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
