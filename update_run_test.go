package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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

	command, apart, err := installCommand("/usr/local/bin/tunnel-manager", "/var/lib/tunnel-manager/tunnel-manager.db")
	if err != nil {
		t.Fatalf("building the command failed: %v", err)
	}

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

	command, apart, err := installCommand("/usr/local/bin/tunnel-manager", database)
	if err != nil {
		t.Fatalf("building the command failed: %v", err)
	}

	t.Cleanup(func() {
		opened, ok := command.Stdout.(*os.File)
		if ok {
			_ = opened.Close()
		}
	})

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
