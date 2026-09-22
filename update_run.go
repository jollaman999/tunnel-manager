package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/api"
	"github.com/jollaman999/tunnel-manager/internal/install"
	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/settings"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// canInstallUpdate reports whether an update may be installed from inside this
// process.
//
// Three things have to hold. The platform has to be one the install registers a
// service on and restarts, which is every one but Windows here: there the
// install works but nothing brings the process back, and an update that ends
// with the service down is worse than no update. The process has to have the
// privilege the install needs, since it writes into the directory programs an
// administrator installed go in. And the executable this process was started
// from has to be the one the service is started from, because what the install
// replaces is that path: run from a build directory, an install would put the
// release over the installed copy while this process goes on running something
// else, which is not what a press on an update screen asked for.
func canInstallUpdate(databaseFile string) bool {
	if runtime.GOOS == "windows" {
		return false
	}

	if install.CheckPrivilege() != nil {
		return false
	}

	running, err := os.Executable()
	if err != nil {
		return false
	}

	running, err = filepath.EvalSymlinks(running)
	if err != nil {
		return false
	}

	planned, err := filepath.EvalSymlinks(install.Defaults().WithDatabase(databaseFile).ExecutablePath)
	if err != nil {
		return false
	}

	return running == planned
}

// startUpdateInstall runs this program again with -install, as a process of its
// own, and comes back as soon as it has started.
//
// It is a separate process and not work done here on purpose. What an install
// does at the end is replace the executable at the path this process was
// started from, and this process cannot then start itself again: the restart
// finds the path it was run from through /proc/self/exe, which reads as the
// file having been deleted once something has been renamed over it, and the
// exec of that name fails. The service would go down and stay down.
//
// So the install is left to the same command an operator types, which puts the
// file in place and has the service manager restart the service. That restart
// ends this process, and the child with it: a child is in the cgroup of this
// unit, and systemd takes down what is in the cgroup of a unit it is stopping.
// Nothing is lost by that. The executable is written to a file beside its
// destination and renamed over, which is a step that either happened or did
// not, so a child that is killed leaves either the release or what was there
// before, and the service manager starts whichever it is.
func startUpdateInstall(logger *zap.Logger, databaseFile string) error {
	running, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to find the file this process was started from: %w", err)
	}

	command := exec.Command(running, "-install", "-db", databaseFile)

	// The child writes its report to nothing. Whoever asked is holding a
	// screen, not this terminal, and the lines that matter are the ones this
	// process logged before handing over.
	command.Stdout = nil
	command.Stderr = nil

	err = command.Start()
	if err != nil {
		return fmt.Errorf("failed to run %s with -install: %w", running, err)
	}

	logger.Info("the install is running as a process of its own. This service is restarted at the "+
		"end of it, which is what ends this process",
		logid.UpdateInstallAsked.Field(),
		zap.Int("pid", command.Process.Pid))

	// The child is not waited for. It outlives the wait by design: what it does
	// last is restart this service, and a wait would be this process sitting on
	// the answer to a request while the thing it is waiting for takes it down.
	go func() {
		_ = command.Wait()
	}()

	return nil
}

// runUpdateChecks looks for a newer release on a timer, and installs one where
// the settings say to.
//
// It reads the settings on every pass rather than holding what they said at
// startup, so that turning the check off on the Settings screen stops it
// without a restart, the way the log level takes hold without one.
//
// The first look is one interval in and not at startup. A machine that has just
// come up is doing everything else it does at startup, and a release that
// appeared while it was down is not a thing that has to be known in the first
// second. It also keeps a restart loop from making a request every time round.
func runUpdateChecks(ctx context.Context, logger *zap.Logger, db *gorm.DB,
	handler *api.UpdateHandler, installable bool) {
	logger.Info("looking for a newer release on a timer",
		logid.UpdateCheckLoopStarted.Field())

	// The timer is armed for the shortest interval the settings take and the
	// pass decides whether it is due. Rearming it from the stored interval
	// instead would leave a setting changed from a day to an hour waiting out
	// the day that was already running.
	ticker := time.NewTicker(updateCheckTick)
	defer ticker.Stop()

	var last time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		set, err := settings.Load(db)
		if err != nil {
			// The settings could not be read, which is a thing the rest of the
			// process reports on its own. Nothing is looked up on a guess.
			continue
		}

		if !set.UpdateCheckEnabled {
			continue
		}

		due := time.Duration(set.UpdateCheckIntervalHours) * time.Hour
		if !last.IsZero() && time.Since(last) < due {
			continue
		}

		last = time.Now()

		result := handler.Look(ctx)

		// Only a release that was read, that compares, and that compares as
		// newer starts an install. A check that failed and a version that could
		// not be compared both fall through here, which is the whole of why
		// Latest carries the two apart.
		if !installable || !set.UpdateAutoInstall || result.Problem != "" ||
			!result.Comparable || !result.Newer {
			continue
		}

		logger.Warn("a newer release is being installed because the settings ask for it. This "+
			"service is restarted at the end of it, which takes every tunnel down",
			logid.UpdateAutoInstallStarting.Field(),
			zap.String("latest", result.Tag))

		err = handler.StartInstall()
		if err != nil {
			logger.Error("failed to start the install",
				logid.UpdateInstallStartFailed.Field(), zap.Error(err))
		}
	}
}

// updateCheckTick is how often the pass above runs. It is not the interval the
// settings name: the pass reads that and decides whether the interval has gone
// by, and this is only how often it asks itself the question.
const updateCheckTick = time.Minute
