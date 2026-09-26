package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/jollaman999/tunnel-manager/internal/api"
	"github.com/jollaman999/tunnel-manager/internal/install"
	"github.com/jollaman999/tunnel-manager/internal/settings"
)

// TestInstallCommandRunsOutsideThisServiceOnSystemd covers the one thing that
// kept an update from ever finishing.
//
// An install stops this service before it replaces the executable, and systemd
// stops a unit by signalling everything in its cgroup. Run as a plain child,
// the install was killed by the stop it had just asked for, before the copy it
// was there to do. Handed to systemd as a unit of its own, it is not in that
// cgroup and the stop does not reach it.
func TestInstallCommandRunsOutsideThisServiceOnSystemd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd-run is the linux path")
	}

	runner := filepath.Join(t.TempDir(), "systemd-run")

	err := os.WriteFile(runner, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	if err != nil {
		t.Fatalf("failed to write the stand-in for systemd-run: %v", err)
	}

	t.Setenv("PATH", filepath.Dir(runner))

	command, apart, release, err := installCommand("/usr/local/bin/tunnel-manager", "/var/lib/tunnel-manager/tunnel-manager.db")
	if err != nil {
		t.Fatalf("building the command failed: %v", err)
	}

	t.Cleanup(release)

	if !apart {
		t.Fatal("the install is a plain child of this service, want it handed to systemd as a unit of its own")
	}

	if command.Path != runner {
		t.Errorf("the install runs %q, want %q", command.Path, runner)
	}

	line := strings.Join(command.Args, " ")

	for _, want := range []string{"--collect", "--unit", "-install", "-db /var/lib/tunnel-manager/tunnel-manager.db"} {
		if !strings.Contains(line, want) {
			t.Errorf("the command is %q, want it to carry %q", line, want)
		}
	}
}

// TestInstallCommandWritesItsReportSomewhereReadable covers the fallback, where
// there is no systemd-run to hand the install to.
//
// It is a plain child there, which is right on a platform whose service manager
// does not take down a tree. What it must not do is write its report to
// nothing: an install that fails is the one moment those lines are worth
// having, and nobody is holding a terminal to read them from.
func TestInstallCommandWritesItsReportSomewhereReadable(t *testing.T) {
	dir := t.TempDir()

	err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700)
	if err != nil {
		t.Fatalf("failed to make the directory the report goes in: %v", err)
	}

	t.Setenv("PATH", filepath.Join(dir, "nothing-here"))

	database := filepath.Join(dir, "tunnel-manager.db")

	command, apart, release, err := installCommand("/usr/local/bin/tunnel-manager", database)
	if err != nil {
		t.Fatalf("building the command failed: %v", err)
	}

	t.Cleanup(release)

	if apart {
		t.Fatal("the install was handed to systemd, want a plain child where there is no systemd-run")
	}

	if command.Stdout == nil || command.Stderr == nil {
		t.Fatal("the install writes its report to nothing")
	}

	report := installReportPath(database, false)

	_, err = os.Stat(report)
	if err != nil {
		t.Fatalf("the report file %s was not made: %v", report, err)
	}

	requireReportKeptToTheOwner(t, report)
}

// reportPlace makes the directory the report of an install goes in, keeps
// systemd-run out of reach so that the install is a plain child, and returns
// the database file the report sits beside.
func reportPlace(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700)
	if err != nil {
		t.Fatalf("failed to make the directory the report goes in: %v", err)
	}

	t.Setenv("PATH", filepath.Join(dir, "nothing-here"))

	return filepath.Join(dir, "tunnel-manager.db")
}

// requireReportLetGo fails unless this process no longer holds the report file
// it opened for the install, and the file can be deleted, which on Windows it
// cannot while a handle is open on it.
func requireReportLetGo(t *testing.T, command *exec.Cmd, report string) {
	t.Helper()

	opened, ok := command.Stdout.(*os.File)
	if !ok {
		t.Fatalf("the install writes its report to %T, want a file", command.Stdout)
	}

	err := opened.Close()
	if !errors.Is(err, os.ErrClosed) {
		t.Errorf("the report file was still open in this process after the start: closing it gave %v", err)
	}

	err = os.Remove(report)
	if err != nil {
		t.Errorf("the report file could not be deleted after the start: %v", err)
	}
}

// TestStartInstallLetsGoOfTheReportWhenTheStartFails covers a start that does
// not happen. Nothing is going to write to the report then, and the file this
// process opened for it has to be let go of all the same.
func TestStartInstallLetsGoOfTheReportWhenTheStartFails(t *testing.T) {
	database := reportPlace(t)

	missing := filepath.Join(filepath.Dir(database), "no-such-program")

	command, _, release, err := installCommand(missing, database)
	if err != nil {
		t.Fatalf("building the command failed: %v", err)
	}

	err = startInstall(command, release)
	if err == nil {
		_ = command.Wait()
		t.Fatalf("starting %s worked, want it to fail", missing)
	}

	requireReportLetGo(t, command, installReportPath(database, false))
}

// TestStartInstallLetsGoOfTheReportTheInstallGoesOnWritingTo covers a start
// that happens. This process lets go of the report as soon as the child is
// running, and the child still writes its report through the copy it was
// handed.
//
// The child is this test binary, which has no -install flag and says so on its
// standard error, after the start has returned.
func TestStartInstallLetsGoOfTheReportTheInstallGoesOnWritingTo(t *testing.T) {
	database := reportPlace(t)

	running, err := os.Executable()
	if err != nil {
		t.Fatalf("failed to find the test binary: %v", err)
	}

	command, apart, release, err := installCommand(running, database)
	if err != nil {
		t.Fatalf("building the command failed: %v", err)
	}

	if apart {
		t.Fatal("the install was handed to systemd, want a plain child where there is no systemd-run")
	}

	err = startInstall(command, release)
	if err != nil {
		t.Fatalf("starting %s failed: %v", running, err)
	}

	opened, ok := command.Stdout.(*os.File)
	if !ok {
		t.Fatalf("the install writes its report to %T, want a file", command.Stdout)
	}

	err = opened.Close()
	if !errors.Is(err, os.ErrClosed) {
		t.Errorf("the report file was still open in this process after the start: closing it gave %v", err)
	}

	_ = command.Wait()

	report := installReportPath(database, false)

	written, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("failed to read the report file %s: %v", report, err)
	}

	if !strings.Contains(string(written), "-install") {
		t.Errorf("the report is %q, want what the install said after the start", written)
	}

	err = os.Remove(report)
	if err != nil {
		t.Errorf("the report file could not be deleted once the install had ended: %v", err)
	}
}

// TestTheUpdateLoopTakesUpAChangedSettingWithoutARestart stores the check and
// the install switched off, starts the loop, and turns both on underneath it.
// The Settings screen reports the two as in place on the strength of the loop
// reading them on every pass, so a loop that held what it started with would
// make that report untrue.
func TestTheUpdateLoopTakesUpAChangedSettingWithoutARestart(t *testing.T) {
	db := newSettingsDB(t)

	// The row is created first and switched off after. A first insert leaves
	// out a false that has a default of true, and the check would be on.
	loaded, err := settings.Load(db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	stored := *loaded
	stored.UpdateCheckEnabled = false
	stored.UpdateAutoInstall = false

	err = settings.Save(db, &stored, nil)
	if err != nil {
		t.Fatalf("failed to store the settings: %v", err)
	}

	var looks, installs atomic.Int32

	check := func(context.Context, string) (install.Latest, error) {
		looks.Add(1)

		return install.Latest{Tag: "v99.0.0", Newer: true, Comparable: true}, nil
	}
	start := func() error {
		installs.Add(1)

		return nil
	}

	handler := api.NewUpdateHandler(zap.NewNop(), db, "3.0.0", true, check, start)

	tick := updateCheckTick
	updateCheckTick = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	t.Cleanup(func() {
		cancel()
		<-done
		updateCheckTick = tick
	})

	go func() {
		defer close(done)
		runUpdateChecks(ctx, zap.NewNop(), db, handler, true)
	}()

	time.Sleep(100 * time.Millisecond)

	if looks.Load() != 0 || installs.Load() != 0 {
		t.Fatalf("looks = %d, installs = %d while the settings say neither, want none",
			looks.Load(), installs.Load())
	}

	stored.UpdateCheckEnabled = true
	stored.UpdateAutoInstall = true

	err = settings.Save(db, &stored, nil)
	if err != nil {
		t.Fatalf("failed to store the settings: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for installs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if looks.Load() != 1 || installs.Load() != 1 {
		t.Fatalf("looks = %d, installs = %d after both were turned on, want one of each",
			looks.Load(), installs.Load())
	}
}
