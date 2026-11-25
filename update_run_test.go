package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"

	"github.com/jollaman999/tunnel-manager/internal/api"
	"github.com/jollaman999/tunnel-manager/internal/install"
	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/settings"
	"github.com/labstack/echo/v5"
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

	for _, want := range []string{"--collect", "--unit", "-install", "-" + fromUpdateFlag,
		"-db /var/lib/tunnel-manager/tunnel-manager.db"} {
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

// updateClock is a clock that stands still until a test moves it on, for the
// update loop to measure its interval with. The loop goes on running on its
// short tick against it, so a test sees every pass an interval of hours would
// take without waiting the hours out.
type updateClock struct {
	now   atomic.Int64
	reads atomic.Int32
}

// stopUpdateClock puts an updateClock in place of the clock of the update loop
// until the test ends.
func stopUpdateClock(t *testing.T) *updateClock {
	t.Helper()

	stopped := &updateClock{}
	stopped.now.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())

	clock := updateCheckNow
	updateCheckNow = func() time.Time {
		stopped.reads.Add(1)

		return time.Unix(0, stopped.now.Load())
	}

	t.Cleanup(func() {
		updateCheckNow = clock
	})

	return stopped
}

func (c *updateClock) advance(by time.Duration) {
	c.now.Add(int64(by))
}

// started runs start, which starts a loop, and waits for that loop to have read
// the clock. The start of a loop is what its first interval is measured from,
// so a clock moved on before then would move the start with it.
func (c *updateClock) started(t *testing.T, start func()) {
	t.Helper()

	before := c.reads.Load()

	start()

	deadline := time.Now().Add(5 * time.Second)
	for c.reads.Load() == before {
		if time.Now().After(deadline) {
			t.Fatal("the loop did not read the clock it measures its interval with")
		}

		time.Sleep(time.Millisecond)
	}
}

// storeUpdateSettings stores whether the check and the install are on and how
// many hours apart the check is, over what is stored already.
func storeUpdateSettings(t *testing.T, db *gorm.DB, check, autoInstall bool, hours int) {
	t.Helper()

	// The row is created first and switched off after. A first insert leaves
	// out a false that has a default of true, and the check would be on.
	loaded, err := settings.Load(db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	stored := *loaded
	stored.UpdateCheckEnabled = check
	stored.UpdateAutoInstall = autoInstall
	stored.UpdateCheckIntervalHours = hours

	err = settings.Save(db, &stored, nil)
	if err != nil {
		t.Fatalf("failed to store the settings: %v", err)
	}
}

// runUpdateLoop runs the update loop on a tick short enough for a test, until
// the test ends, with a data directory that holds no time of a last look.
func runUpdateLoop(t *testing.T, clock *updateClock, db *gorm.DB, handler *api.UpdateHandler) {
	t.Helper()

	runUpdateLoopIn(t, clock, db, handler, t.TempDir())
}

// runUpdateLoopIn is runUpdateLoop with the data directory dataDir.
func runUpdateLoopIn(t *testing.T, clock *updateClock, db *gorm.DB, handler *api.UpdateHandler,
	dataDir string) {
	t.Helper()

	tick := updateCheckTick
	updateCheckTick = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	t.Cleanup(func() {
		cancel()
		<-done
		updateCheckTick = tick
	})

	clock.started(t, func() {
		go func() {
			defer close(done)
			runUpdateChecks(ctx, zap.NewNop(), db, handler, true, dataDir)
		}()
	})
}

// waitForLooks waits for the loop to have looked as many times as want, and
// then for long enough that a look past that would be seen.
func waitForLooks(looks *atomic.Int32, want int32) {
	deadline := time.Now().Add(5 * time.Second)
	for looks.Load() < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	time.Sleep(50 * time.Millisecond)
}

// TestTheUpdateLoopTakesUpAChangedSettingWithoutARestart stores the check and
// the install switched off, starts the loop, and turns both on underneath it.
// The Settings screen reports the two as in place on the strength of the loop
// reading them on every pass, so a loop that held what it started with would
// make that report untrue.
func TestTheUpdateLoopTakesUpAChangedSettingWithoutARestart(t *testing.T) {
	db := newSettingsDB(t)
	storeUpdateSettings(t, db, false, false, 24)

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

	clock := stopUpdateClock(t)
	runUpdateLoop(t, clock, db, handler)

	// The interval has gone by while the check is off, so that it is the
	// switch and not the interval that keeps the loop from looking.
	clock.advance(25 * time.Hour)
	time.Sleep(100 * time.Millisecond)

	if looks.Load() != 0 || installs.Load() != 0 {
		t.Fatalf("looks = %d, installs = %d while the settings say neither, want none",
			looks.Load(), installs.Load())
	}

	storeUpdateSettings(t, db, true, true, 24)

	deadline := time.Now().Add(5 * time.Second)
	for installs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if looks.Load() != 1 || installs.Load() != 1 {
		t.Fatalf("looks = %d, installs = %d after both were turned on, want one of each",
			looks.Load(), installs.Load())
	}
}

// TestTheFirstLookIsAnIntervalAfterStart is a process that has just started.
// The loop passes many times before the interval has gone by, and none of them
// is a request to the release API; the pass after it is one.
func TestTheFirstLookIsAnIntervalAfterStart(t *testing.T) {
	db := newSettingsDB(t)
	storeUpdateSettings(t, db, true, false, 24)

	var looks atomic.Int32

	check := func(context.Context, string) (install.Latest, error) {
		looks.Add(1)

		return install.Latest{Tag: "v99.0.0", Newer: true, Comparable: true}, nil
	}

	handler := api.NewUpdateHandler(zap.NewNop(), db, "3.0.0", true, check, func() error { return nil })

	clock := stopUpdateClock(t)
	runUpdateLoop(t, clock, db, handler)

	time.Sleep(100 * time.Millisecond)

	if looks.Load() != 0 {
		t.Fatalf("the loop looked %d times right after it started, want none before the interval",
			looks.Load())
	}

	clock.advance(24*time.Hour - time.Minute)
	time.Sleep(100 * time.Millisecond)

	if looks.Load() != 0 {
		t.Fatalf("the loop looked %d times a minute short of the interval, want none", looks.Load())
	}

	clock.advance(time.Minute)
	waitForLooks(&looks, 1)

	if looks.Load() != 1 {
		t.Fatalf("the loop looked %d times once the interval had gone by, want one", looks.Load())
	}
}

// TestAnIntervalShortenedAfterStartIsMeasuredFromTheStart changes a day to an
// hour half an hour after the start. The look is due an hour after the start,
// not a day after it and not at once.
func TestAnIntervalShortenedAfterStartIsMeasuredFromTheStart(t *testing.T) {
	db := newSettingsDB(t)
	storeUpdateSettings(t, db, true, false, 24)

	var looks atomic.Int32

	check := func(context.Context, string) (install.Latest, error) {
		looks.Add(1)

		return install.Latest{Tag: "v99.0.0", Newer: true, Comparable: true}, nil
	}

	handler := api.NewUpdateHandler(zap.NewNop(), db, "3.0.0", true, check, func() error { return nil })

	clock := stopUpdateClock(t)
	runUpdateLoop(t, clock, db, handler)

	clock.advance(30 * time.Minute)
	storeUpdateSettings(t, db, true, false, 1)
	time.Sleep(100 * time.Millisecond)

	if looks.Load() != 0 {
		t.Fatalf("the loop looked %d times half an hour after it started, want none", looks.Load())
	}

	clock.advance(30 * time.Minute)
	waitForLooks(&looks, 1)

	if looks.Load() != 1 {
		t.Fatalf("the loop looked %d times an hour after it started, want one: the interval "+
			"changed to an hour did not take hold without a restart", looks.Load())
	}
}

// TestARestartDoesNotLookAgainBeforeAnInterval is an install that ends in the
// restart of the service, and a release that still reads as newer after it, as
// one whose executable gives a version that is not its tag does. The service
// is stood in for by a loop and a handler, and a restart by a new pair.
//
// A loop that looked on its first pass started the install again a pass after
// every restart, so that the service went round install and restart for as
// long as the release stood.
func TestARestartDoesNotLookAgainBeforeAnInterval(t *testing.T) {
	db := newSettingsDB(t)
	storeUpdateSettings(t, db, true, true, 24)

	var looks, starts atomic.Int32

	// Every life of the service keeps the time of its looks where the next
	// one reads it, as the data directory of an installation does.
	dataDir := t.TempDir()

	check := keptUpdateChecks(zap.NewNop(), dataDir, func(context.Context, string) (install.Latest, error) {
		looks.Add(1)

		return install.Latest{Tag: "v99.0.0", Newer: true, Comparable: true}, nil
	})

	installed := make(chan struct{}, 100)

	tick := updateCheckTick
	updateCheckTick = 5 * time.Millisecond

	t.Cleanup(func() {
		updateCheckTick = tick
	})

	clock := stopUpdateClock(t)

	serve := func() (context.CancelFunc, chan struct{}) {
		handler := api.NewUpdateHandler(zap.NewNop(), db, "3.0.0", true, check, func() error {
			starts.Add(1)
			installed <- struct{}{}

			return nil
		})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})

		clock.started(t, func() {
			go func() {
				defer close(done)
				runUpdateChecks(ctx, zap.NewNop(), db, handler, true, dataDir)
			}()
		})

		return cancel, done
	}

	cancel, done := serve()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	clock.advance(24 * time.Hour)

	restarts := 0
	deadline := time.After(300 * time.Millisecond)

wait:
	for {
		select {
		case <-installed:
			cancel()
			<-done
			cancel, done = serve()
			restarts++
		case <-deadline:
			break wait
		}
	}

	t.Logf("tick %v: looks = %d, installs started = %d, restarts = %d",
		updateCheckTick, looks.Load(), starts.Load(), restarts)

	if looks.Load() != 1 || starts.Load() != 1 {
		t.Fatalf("looks = %d, installs = %d across %d restarts within the interval, want one of each: "+
			"a restart looked again at once", looks.Load(), starts.Load(), restarts)
	}

	clock.advance(24 * time.Hour)
	waitForLooks(&looks, 2)

	if looks.Load() != 2 {
		t.Errorf("the restarted service looked %d times in all an interval after it came up, want two",
			looks.Load())
	}
}

// lookingTimer is a handler whose look counts into looks and finds a newer
// release, kept the way main keeps it, in dataDir.
func lookingTimer(db *gorm.DB, dataDir string, looks *atomic.Int32) *api.UpdateHandler {
	check := keptUpdateChecks(zap.NewNop(), dataDir, func(context.Context, string) (install.Latest, error) {
		looks.Add(1)

		return install.Latest{Tag: "v99.0.0", Newer: true, Comparable: true}, nil
	})

	return api.NewUpdateHandler(zap.NewNop(), db, "3.0.0", true, check, func() error { return nil })
}

// TestAStartMeasuresTheIntervalFromTheLastLook is a machine stopped every
// night: it looked 23 hours before this start and the interval is a day. The
// look is due an hour in. Measured from every start instead, a machine that is
// never up for a whole interval never looked at all.
func TestAStartMeasuresTheIntervalFromTheLastLook(t *testing.T) {
	db := newSettingsDB(t)
	storeUpdateSettings(t, db, true, false, 24)

	var looks atomic.Int32

	dataDir := t.TempDir()
	handler := lookingTimer(db, dataDir, &looks)

	clock := stopUpdateClock(t)
	start := time.Unix(0, clock.now.Load())
	keepUpdateCheck(zap.NewNop(), dataDir, start.Add(-23*time.Hour))

	runUpdateLoopIn(t, clock, db, handler, dataDir)

	clock.advance(time.Hour - time.Minute)
	time.Sleep(100 * time.Millisecond)

	if looks.Load() != 0 {
		t.Fatalf("the loop looked %d times a minute short of a day after the last look, want none",
			looks.Load())
	}

	clock.advance(time.Minute)
	waitForLooks(&looks, 1)

	if looks.Load() != 1 {
		t.Fatalf("the loop looked %d times a day after the last look and an hour after the start, "+
			"want one", looks.Load())
	}

	kept := lastUpdateCheck(zap.NewNop(), dataDir, start.Add(48*time.Hour))
	if !kept.Equal(start.Add(time.Hour)) {
		t.Errorf("the time kept after the look is %v, want the time of the look, %v", kept, start.Add(time.Hour))
	}
}

// TestALastLookAheadOfTheClockIsMeasuredFromTheStart is a clock put back after
// the last look was kept, here to an hour before it. The look is due a day
// after the start, not a day after a time the clock has not reached.
func TestALastLookAheadOfTheClockIsMeasuredFromTheStart(t *testing.T) {
	db := newSettingsDB(t)
	storeUpdateSettings(t, db, true, false, 24)

	var looks atomic.Int32

	dataDir := t.TempDir()
	handler := lookingTimer(db, dataDir, &looks)

	clock := stopUpdateClock(t)
	keepUpdateCheck(zap.NewNop(), dataDir, time.Unix(0, clock.now.Load()).Add(time.Hour))

	runUpdateLoopIn(t, clock, db, handler, dataDir)

	clock.advance(24*time.Hour - time.Minute)
	time.Sleep(100 * time.Millisecond)

	if looks.Load() != 0 {
		t.Fatalf("the loop looked %d times a minute short of a day after the start, want none", looks.Load())
	}

	clock.advance(time.Minute)
	waitForLooks(&looks, 1)

	if looks.Load() != 1 {
		t.Fatalf("the loop looked %d times a day after the start, want one: the time ahead of the "+
			"clock was measured from", looks.Load())
	}
}

// TestLastUpdateCheckFallsBackToTheStart is every file the loop can find in
// place of the time of the last look, and what it measures from for each.
func TestLastUpdateCheckFallsBackToTheStart(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)

	cases := []struct {
		name  string
		write func(t *testing.T, dataDir string)
		want  time.Time
		said  bool
	}{
		{
			name:  "no file",
			write: func(*testing.T, string) {},
			want:  start,
		},
		{
			name: "a time before the start",
			write: func(_ *testing.T, dataDir string) {
				keepUpdateCheck(zap.NewNop(), dataDir, start.Add(-5*time.Hour))
			},
			want: start.Add(-5 * time.Hour),
		},
		{
			name: "the start itself",
			write: func(_ *testing.T, dataDir string) {
				keepUpdateCheck(zap.NewNop(), dataDir, start)
			},
			want: start,
		},
		{
			name: "a time after the start",
			write: func(_ *testing.T, dataDir string) {
				keepUpdateCheck(zap.NewNop(), dataDir, start.Add(time.Second))
			},
			want: start,
			said: true,
		},
		{
			name: "not a time",
			write: func(t *testing.T, dataDir string) {
				err := os.WriteFile(updateCheckPath(dataDir), []byte("yesterday"), 0o600)
				if err != nil {
					t.Fatalf("failed to write the file: %v", err)
				}
			},
			want: start,
			said: true,
		},
		{
			name: "empty",
			write: func(t *testing.T, dataDir string) {
				err := os.WriteFile(updateCheckPath(dataDir), nil, 0o600)
				if err != nil {
					t.Fatalf("failed to write the file: %v", err)
				}
			},
			want: start,
			said: true,
		},
		{
			name: "a directory in its place",
			write: func(t *testing.T, dataDir string) {
				err := os.Mkdir(updateCheckPath(dataDir), 0o700)
				if err != nil {
					t.Fatalf("failed to make the directory: %v", err)
				}
			},
			want: start,
			said: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dataDir := t.TempDir()
			c.write(t, dataDir)

			core, logs := observer.New(zap.WarnLevel)

			got := lastUpdateCheck(zap.New(core), dataDir, start)
			if !got.Equal(c.want) {
				t.Errorf("lastUpdateCheck = %v, want %v", got, c.want)
			}

			said := logs.FilterField(logid.UpdateCheckTimeNotRead.Field()).Len() == 1
			if said != c.said {
				t.Errorf("a warning was said: %v, want %v (%v)", said, c.said, logs.All())
			}
		})
	}
}

// TestEveryLookIsKeptWhetherItWorkedOrNot is a look that failed, which counts
// the same as one that read a release. A restart loop with the network down
// would otherwise make a request every time round.
func TestEveryLookIsKeptWhetherItWorkedOrNot(t *testing.T) {
	dataDir := t.TempDir()
	clock := stopUpdateClock(t)

	failing := keptUpdateChecks(zap.NewNop(), dataDir, func(context.Context, string) (install.Latest, error) {
		return install.Latest{}, errors.New("the network is down")
	})

	handler := api.NewUpdateHandler(zap.NewNop(), newSettingsDB(t), "3.0.0", true, failing, func() error { return nil })

	looked := time.Unix(0, clock.now.Load())

	result := handler.Look(context.Background())
	if result.Problem == "" {
		t.Fatal("the look worked, want one that failed")
	}

	kept := lastUpdateCheck(zap.NewNop(), dataDir, looked.Add(time.Hour))
	if !kept.Equal(looked) {
		t.Errorf("the time kept after a look that failed is %v, want %v", kept, looked)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(updateCheckPath(dataDir))
		if err != nil {
			t.Fatalf("failed to read the file the time is kept in: %v", err)
		}

		if info.Mode().Perm() != 0o600 {
			t.Errorf("the file the time is kept in is %v, want -rw-------", info.Mode().Perm())
		}
	}

	clock.advance(3 * time.Hour)

	working := keptUpdateChecks(zap.NewNop(), dataDir, func(context.Context, string) (install.Latest, error) {
		return install.Latest{Tag: "v3.0.0", Comparable: true}, nil
	})

	_, err := working(context.Background(), "3.0.0")
	if err != nil {
		t.Fatalf("the look failed: %v", err)
	}

	kept = lastUpdateCheck(zap.NewNop(), dataDir, looked.Add(4*time.Hour))
	if !kept.Equal(looked.Add(3 * time.Hour)) {
		t.Errorf("the time kept after a look that worked is %v, want %v", kept, looked.Add(3*time.Hour))
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

	err := startUpdateInstall(zap.NewNop(), filepath.Join(dir, "tunnel-manager.db"), func(string) { ended = true })
	if !errors.Is(err, install.ErrInstallRunning) {
		t.Fatalf("the start answered %v, want install.ErrInstallRunning", err)
	}

	if ended {
		t.Error("an install that was never started was reported as ended")
	}
}

// TestWhyAnUpdateFailedReachesTheUpdateScreen follows the reason an install
// leaves from the data directory to GET /api/update, on the start handed to
// systemd, which is the one where this process does not see the install end
// but only the lock.
//
// The stand-in for systemd-run does what a failed update does: it leaves the
// reason and ends without ever being seen holding the lock, which is an install
// that took it and let it go between two looks. The reason is what ends the
// wait then, and the screen is not kept waiting the whole minute an install has
// to take the lock.
func TestWhyAnUpdateFailedReachesTheUpdateScreen(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd-run is the linux path")
	}

	runner := filepath.Join(t.TempDir(), "systemd-run")

	stand := func(script string) {
		t.Helper()

		err := os.WriteFile(runner, []byte("#!/bin/sh\nfor last in \"$@\"; do :; done\n"+script+"exit 0\n"), 0o755)
		if err != nil {
			t.Fatalf("failed to write the stand-in for systemd-run: %v", err)
		}
	}

	t.Setenv("PATH", filepath.Dir(runner))

	appear, held, poll := installLockAppearWait, installLockHeldWait, installLockPoll
	installLockAppearWait, installLockHeldWait, installLockPoll = 30*time.Second, time.Second, 5*time.Millisecond

	t.Cleanup(func() {
		installLockAppearWait, installLockHeldWait, installLockPoll = appear, held, poll
	})

	dir := t.TempDir()
	database := filepath.Join(dir, "tunnel-manager.db")

	err := os.WriteFile(installProblemPath(dir), []byte("an install before this one failed"), 0o600)
	if err != nil {
		t.Fatalf("failed to leave the reason of an install before: %v", err)
	}

	ended := make(chan struct{}, 1)

	var handler *api.UpdateHandler

	handler = api.NewUpdateHandler(zap.NewNop(), nil, "3.0.0", true,
		func(context.Context, string) (install.Latest, error) {
			return install.Latest{}, nil
		},
		func() error {
			return startUpdateInstall(zap.NewNop(), database, func(problem string) {
				handler.InstallEnded(problem)
				ended <- struct{}{}
			})
		})

	read := func() string {
		t.Helper()

		rec := httptest.NewRecorder()
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/api/update", nil), rec)

		err := handler.GetUpdate(c)
		if err != nil {
			t.Fatalf("GetUpdate returned an error: %v", err)
		}

		var resp struct {
			Data map[string]any `json:"data"`
		}

		err = json.Unmarshal(rec.Body.Bytes(), &resp)
		if err != nil {
			t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
		}

		problem, _ := resp.Data["install_problem"].(string)

		return problem
	}

	waitEnded := func(within time.Duration) time.Duration {
		t.Helper()

		start := time.Now()

		select {
		case <-ended:
		case <-time.After(within):
			t.Fatalf("the install was not reported as over within %v", within)
		}

		return time.Since(start)
	}

	why := "the update was not installed and nothing was stopped or replaced: the release was not used " +
		"(release v99.0.0 carries no SHA256SUMS.sig), and an update installs a release or nothing"

	stand("printf '%s' '" + why + "' > \"${last%/*}/update-install.problem\"\n")

	err = handler.StartInstall()
	if err != nil {
		t.Fatalf("the start failed: %v", err)
	}

	took := waitEnded(10 * time.Second)

	t.Logf("the failed install was reported as over %v after the start", took)

	if got := read(); got != why {
		t.Errorf("install_problem = %q, want %q", got, why)
	}

	// The next install leaves nothing. Neither the reason of the one before
	// nor the one left before that may be read as its own.
	stand("")

	installLockAppearWait = 100 * time.Millisecond

	err = handler.StartInstall()
	if err != nil {
		t.Fatalf("the second start failed: %v", err)
	}

	if got := read(); got != "" {
		t.Errorf("install_problem = %q while the next install runs, want none", got)
	}

	waitEnded(10 * time.Second)

	if got := read(); got != "" {
		t.Errorf("install_problem = %q after an install that left nothing, want none", got)
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

// startedUpdateHandler is the update handler as main makes it, with the reason
// an install left in dir answered the way main answers it at startup, and what
// reads install_problem off GET /api/update.
func startedUpdateHandler(t *testing.T, dir string) func() (string, string) {
	t.Helper()

	handler := api.NewUpdateHandler(zap.NewNop(), nil, "3.0.0", true,
		func(context.Context, string) (install.Latest, error) {
			return install.Latest{}, nil
		},
		func() error {
			return nil
		})

	answerInstallProblemLeft(zap.NewNop(), dir, handler.InstallEndedBefore)

	return func() (string, string) {
		t.Helper()

		rec := httptest.NewRecorder()
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/api/update", nil), rec)

		err := handler.GetUpdate(c)
		if err != nil {
			t.Fatalf("GetUpdate returned an error: %v", err)
		}

		var resp struct {
			Data map[string]any `json:"data"`
		}

		err = json.Unmarshal(rec.Body.Bytes(), &resp)
		if err != nil {
			t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
		}

		problem, _ := resp.Data["install_problem"].(string)

		return problem, rec.Body.String()
	}
}

// TestWhyAnUpdateFailedAfterTheStopIsAnsweredByTheProcessItStarted is an
// install that failed after it had stopped the service. It starts the service
// again on its way out, so the process that started it is gone, and the reason
// it left was a file nobody read: the Update tab said nothing.
func TestWhyAnUpdateFailedAfterTheStopIsAnsweredByTheProcessItStarted(t *testing.T) {
	why := "failed to register the service: systemctl daemon-reload: exit status 1"

	t.Run("the reason is there at startup", func(t *testing.T) {
		dir := t.TempDir()

		err := os.WriteFile(installProblemPath(dir), []byte(why+"\n"), 0o600)
		if err != nil {
			t.Fatalf("failed to leave the reason: %v", err)
		}

		read := startedUpdateHandler(t, dir)

		if got, _ := read(); got != why {
			t.Errorf("install_problem = %q, want %q", got, why)
		}

		_, err = os.Stat(installProblemPath(dir))
		if err != nil {
			t.Errorf("the reason was taken away once read: %v. It goes when the next install starts", err)
		}
	})

	// The install starts the service before it writes the reason, and writes it
	// before it lets the lock go. A process that came up in between and read
	// the directory once would find nothing and say nothing for good.
	t.Run("the install still holds the lock at startup", func(t *testing.T) {
		poll := installLockPoll
		installLockPoll = 5 * time.Millisecond

		t.Cleanup(func() {
			installLockPoll = poll
		})

		dir := t.TempDir()
		holdInstallLockFor(t, dir)

		read := startedUpdateHandler(t, dir)

		if got, _ := read(); got != "" {
			t.Fatalf("install_problem = %q while the install holds the lock and has left nothing, want none", got)
		}

		time.Sleep(50 * time.Millisecond)

		err := os.WriteFile(installProblemPath(dir), []byte(why), 0o600)
		if err != nil {
			t.Fatalf("failed to leave the reason: %v", err)
		}

		err = os.Remove(install.InstallLockFile(dir))
		if err != nil {
			t.Fatalf("failed to let the lock go: %v", err)
		}

		deadline := time.Now().Add(5 * time.Second)

		got, _ := read()
		for got != why && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)

			got, _ = read()
		}

		if got != why {
			t.Errorf("install_problem = %q once the install let the lock go, want %q", got, why)
		}
	})

	t.Run("no reason was left", func(t *testing.T) {
		read := startedUpdateHandler(t, t.TempDir())

		if got, body := read(); got != "" || strings.Contains(body, "install_problem") {
			t.Errorf("install_problem = %q where no install left a reason, want the field left out: %s", got, body)
		}
	})
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

	err = startUpdateInstall(zap.New(core), filepath.Join(t.TempDir(), "tunnel-manager.db"), func(string) { close(ended) })
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

// TestAnUpdateWhoseReleaseCannotBeCheckedDoesNotRestartTheService is the
// install the loop starts on its own, followed past the end of this process.
//
// The release is newer and carries nothing its download could be checked
// against, which is what a release made before the checksums were signed looks
// like, so Fetch answers with the executable that is running. The service is
// stood in for by a loop and a handler; an install that reaches put is a
// restart of it, and the next one starts with a loop that has never looked.
func TestAnUpdateWhoseReleaseCannotBeCheckedDoesNotRestartTheService(t *testing.T) {
	db := newSettingsDB(t)

	loaded, err := settings.Load(db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	stored := *loaded
	stored.UpdateCheckEnabled = true
	stored.UpdateAutoInstall = true

	err = settings.Save(db, &stored, nil)
	if err != nil {
		t.Fatalf("failed to store the settings: %v", err)
	}

	if stored.UpdateCheckIntervalHours != 24 {
		t.Fatalf("the interval is %d hours, want the default of 24", stored.UpdateCheckIntervalHours)
	}

	tick := updateCheckTick
	updateCheckTick = 5 * time.Millisecond

	t.Cleanup(func() {
		updateCheckTick = tick
	})

	clock := stopUpdateClock(t)

	plan := install.Defaults().WithDatabase(filepath.Join(t.TempDir(), "data", "tunnel-manager.db"))

	// The install is run with what the command this service starts it with
	// says, so that a command that left the flag out is the install that
	// restarts the service.
	command, _, release, err := installCommand("/usr/local/bin/tunnel-manager", reportPlace(t))
	if err != nil {
		t.Fatalf("building the command failed: %v", err)
	}

	release()

	fromUpdate := slices.Contains(command.Args, "-"+fromUpdateFlag)

	type ended struct {
		err       error
		restarted bool
	}

	over := make(chan ended, 1)

	var looks, starts, restarts atomic.Int32

	steps := installSteps{
		fetch: func(context.Context, string, string) (*install.Fetched, error) {
			return &install.Fetched{Path: "/usr/local/bin/tunnel-manager", Source: install.SourceRunning,
				Why: "release v99.0.0 carries no SHA256SUMS.sig"}, nil
		},
		put: func(install.Plan, string, string, io.Writer) (install.Outcome, error) {
			restarts.Add(1)

			return install.Outcome{}, nil
		},
	}

	check := func(context.Context, string) (install.Latest, error) {
		looks.Add(1)

		return install.Latest{Tag: "v99.0.0", Newer: true, Comparable: true}, nil
	}

	var handler *api.UpdateHandler

	// serve is one life of the service: a loop that has never looked and a
	// handler that has never installed, as a process that has just started has.
	serve := func() (context.CancelFunc, chan struct{}) {
		handler = api.NewUpdateHandler(zap.NewNop(), db, "3.0.0", true, check, func() error {
			starts.Add(1)

			go func() {
				before := restarts.Load()
				err := installRelease(plan, steps, fromUpdate, io.Discard)
				over <- ended{err: err, restarted: restarts.Load() != before}
			}()

			return nil
		})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})

		clock.started(t, func() {
			go func() {
				defer close(done)
				runUpdateChecks(ctx, zap.NewNop(), db, handler, true, t.TempDir())
			}()
		})

		return cancel, done
	}

	cancel, done := serve()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	clock.advance(24 * time.Hour)

	var failures []error

	deadline := time.After(2 * time.Second)

wait:
	for restarts.Load() < 5 {
		select {
		case end := <-over:
			if end.restarted {
				cancel()
				<-done
				cancel, done = serve()

				continue
			}

			failures = append(failures, end.err)
			handler.InstallEnded("")
		case <-deadline:
			break wait
		}
	}

	t.Logf("tick %v: looks = %d, installs started = %d, restarts = %d, installs that failed = %v",
		updateCheckTick, looks.Load(), starts.Load(), restarts.Load(), failures)

	if restarts.Load() != 0 {
		t.Errorf("the service was restarted %d times on the executable it was already running, want none",
			restarts.Load())
	}

	if len(failures) != 1 || failures[0] == nil || !strings.Contains(failures[0].Error(), "SHA256SUMS.sig") {
		t.Errorf("the installs that failed are %v, want one that names why the release was not used", failures)
	}

	if starts.Load() != 1 {
		t.Errorf("%d installs were started, want one: the next is an interval away", starts.Load())
	}
}
