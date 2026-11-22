package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/jollaman999/tunnel-manager/internal/api"
	"github.com/jollaman999/tunnel-manager/internal/install"
	"github.com/jollaman999/tunnel-manager/internal/logid"
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

// holdInstallLockFor leaves the lock of the installation in dir as an install
// that is running holds it: under the id of a process that is there and is not
// this one, which the parent of the test binary is.
func holdInstallLockFor(t *testing.T, dir string) {
	t.Helper()

	err := os.WriteFile(install.InstallLockFile(dir), []byte(strconv.Itoa(os.Getppid())), 0o600)
	if err != nil {
		t.Fatalf("failed to write the lock: %v", err)
	}
}

// TestAnInstallRunningIsNotStartedAgain is an install somebody typed at a
// terminal while the Update screen is pressed. The second one would only be
// refused by the lock once it ran, out of sight of the press, which would have
// been told that it started.
func TestAnInstallRunningIsNotStartedAgain(t *testing.T) {
	dir := t.TempDir()
	holdInstallLockFor(t, dir)

	ended := false

	err := startUpdateInstall(zap.NewNop(), filepath.Join(dir, "tunnel-manager.db"), func() { ended = true })
	if !errors.Is(err, install.ErrInstallRunning) {
		t.Fatalf("the start answered %v, want install.ErrInstallRunning", err)
	}

	if ended {
		t.Error("an install that was never started was reported as ended")
	}
}

// TestTheWaitForAnInstallCoversTheMomentBeforeItTakesTheLock is the install
// handed to systemd: systemd-run ends as soon as the unit has started, which is
// before the install has taken the lock. Read as "the lock is not held", that
// moment would be an install that is over.
func TestTheWaitForAnInstallCoversTheMomentBeforeItTakesTheLock(t *testing.T) {
	dir := t.TempDir()

	done := make(chan struct{})

	go func() {
		waitForInstallEnd(dir, 10*time.Second, 10*time.Second, 5*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("the wait ended before the install had taken the lock")
	case <-time.After(100 * time.Millisecond):
	}

	holdInstallLockFor(t, dir)

	select {
	case <-done:
		t.Fatal("the wait ended while the install held the lock")
	case <-time.After(100 * time.Millisecond):
	}

	err := os.Remove(install.InstallLockFile(dir))
	if err != nil {
		t.Fatalf("failed to let the lock go: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not end once the install let the lock go")
	}
}

// TestTheWaitForAnInstallThatNeverTookTheLockEnds is a unit that ended before
// it took the lock, refused or failed. The press is let go once the time it
// had to take it is up, rather than held until the service restarts.
func TestTheWaitForAnInstallThatNeverTookTheLockEnds(t *testing.T) {
	start := time.Now()

	waitForInstallEnd(t.TempDir(), 50*time.Millisecond, 10*time.Second, 5*time.Millisecond)

	if waited := time.Since(start); waited < 50*time.Millisecond || waited > 5*time.Second {
		t.Errorf("the wait took %v, want about the 50ms it was given", waited)
	}
}

// TestTheWaitForAnInstallGivesUpOnALockThatIsNeverLetGo is a lock that reads
// as held for good: it holds something other than a process id, or names an id
// the system has handed to another process since. Waited on without an end,
// it kept the Update screen refusing every install until the service
// restarted.
func TestTheWaitForAnInstallGivesUpOnALockThatIsNeverLetGo(t *testing.T) {
	dir := t.TempDir()

	err := os.WriteFile(install.InstallLockFile(dir), []byte("not a process id"), 0o600)
	if err != nil {
		t.Fatalf("failed to write the lock: %v", err)
	}

	done := make(chan bool, 1)

	go func() {
		done <- waitForInstallEnd(dir, 10*time.Second, 100*time.Millisecond, 5*time.Millisecond)
	}()

	select {
	case over := <-done:
		if over {
			t.Error("a lock still held when the wait gave up was reported as let go")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not give up on a lock that is never let go")
	}
}

// TestAnInstallWhoseLockIsNeverLetGoIsTakenAsOver follows the start handed to
// systemd as far as the Update screen is concerned: systemd-run ends, the unit
// takes the lock, and the lock never reads as let go. The screen is told the
// install is over once the wait gives up, and the log says why.
func TestAnInstallWhoseLockIsNeverLetGoIsTakenAsOver(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd-run is the linux path")
	}

	// The stand-in for systemd-run does what the unit would: it takes the lock,
	// here with something in it that names no process, and it ends. PATH holds
	// nothing but this file, so only what the shell has built in is used.
	runner := filepath.Join(t.TempDir(), "systemd-run")

	script := "#!/bin/sh\nfor last in \"$@\"; do :; done\n" +
		"printf 'not a process id' > \"${last%/*}/install.lock\"\nexit 0\n"

	err := os.WriteFile(runner, []byte(script), 0o755)
	if err != nil {
		t.Fatalf("failed to write the stand-in for systemd-run: %v", err)
	}

	t.Setenv("PATH", filepath.Dir(runner))

	appear, held, poll := installLockAppearWait, installLockHeldWait, installLockPoll
	installLockAppearWait, installLockHeldWait, installLockPoll = 10*time.Second, 100*time.Millisecond, 5*time.Millisecond

	t.Cleanup(func() {
		installLockAppearWait, installLockHeldWait, installLockPoll = appear, held, poll
	})

	core, logs := observer.New(zap.InfoLevel)
	ended := make(chan struct{})

	err = startUpdateInstall(zap.New(core), filepath.Join(t.TempDir(), "tunnel-manager.db"), func() { close(ended) })
	if err != nil {
		t.Fatalf("the start failed: %v", err)
	}

	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the install was never reported as over")
	}

	if logs.FilterField(logid.UpdateInstallWaitGaveUp.Field()).Len() != 1 {
		t.Errorf("no line says the wait gave up. Lines: %v", logs.All())
	}
}
