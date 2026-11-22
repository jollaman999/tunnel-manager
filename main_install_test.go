package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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

			err := installRelease(plan, steps, io.Discard)
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

		if installRelease(plan, steps, io.Discard) == nil {
			t.Fatal("the install reported that it went through")
		}

		assertEmptyDirectory(t, dataDir)
	})

	t.Run("made and installed", func(t *testing.T) {
		dataDir := filepath.Join(t.TempDir(), "opt", "tm")
		plan := install.Defaults().WithDatabase(filepath.Join(dataDir, "tm.db"))
		steps, reached := installStepsThat(t, nil, nil)

		err := installRelease(plan, steps, io.Discard)
		if err != nil {
			t.Fatalf("the install failed: %v", err)
		}

		if !*reached {
			t.Fatal("the install was never reached")
		}

		assertEmptyDirectory(t, dataDir)
	})
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
