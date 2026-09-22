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

// startUpdateInstall runs this program again with -install, outside this
// service, and comes back as soon as it has started.
//
// It is a separate process and not work done here on purpose. What an install
// does at the end is replace the executable at the path this process was
// started from, and this process cannot then start itself again: the restart
// finds the path it was run from through /proc/self/exe, which reads as the
// file having been deleted once something has been renamed over it, and the
// exec of that name fails. The service would go down and stay down.
//
// Outside this service, and not merely a child of it. An install stops the
// service before it puts the new executable in place, because a file that is
// open for execution cannot be written to; systemd stops a unit by signalling
// everything in its cgroup, and a child of this process is in that cgroup. Left
// as a plain child, the install was killed by the very stop it had just asked
// for, three lines before the copy it was there to do. What it left behind was
// the release downloaded to a temporary directory, the old executable still in
// place, and a service that stays down: an explicit stop is not a failure, so
// Restart=always does not bring it back.
//
// So on a systemd machine the install is handed to systemd as a transient unit
// of its own. Stopping this service then does not touch it, and it runs the
// same steps in the same order as the install an operator types by hand.
func startUpdateInstall(logger *zap.Logger, databaseFile string) error {
	running, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to find the file this process was started from: %w", err)
	}

	command, apart, err := installCommand(running, databaseFile)
	if err != nil {
		return err
	}

	err = command.Start()
	if err != nil {
		return fmt.Errorf("failed to run %s with -install: %w", running, err)
	}

	logger.Info("the install is running as a process of its own. This service is restarted at the "+
		"end of it, which is what ends this process",
		logid.UpdateInstallAsked.Field(),
		zap.Int("pid", command.Process.Pid),
		zap.Bool("outside_this_service", apart),
		zap.String("writes_its_report_to", installReportPath(databaseFile, apart)))

	// The child is not waited for. It outlives the wait by design: what it does
	// last is restart this service, and a wait would be this process sitting on
	// the answer to a request while the thing it is waiting for takes it down.
	go func() {
		_ = command.Wait()
	}()

	return nil
}

// installCommand is the command that runs the install, and whether it will run
// outside this service.
//
// Where systemd is the service manager and systemd-run is on the machine, the
// install is asked for as a transient unit. --collect has systemd forget the
// unit once it has exited, so a failed install does not leave a unit behind
// that the next one would collide with, and the name carries the process that
// asked so that two of them could never be the same unit.
//
// Everywhere else the install is a plain child, which is what it was before and
// is right on a platform whose service manager does not take down a tree. Its
// report then goes to a file rather than to nothing: an install that fails is
// the one moment those lines are worth having, and the process that would have
// read them from a terminal is not there.
func installCommand(running string, databaseFile string) (*exec.Cmd, bool, error) {
	if runtime.GOOS == "linux" {
		runner, err := exec.LookPath("systemd-run")
		if err == nil {
			unit := fmt.Sprintf("tunnel-manager-install-%d", os.Getpid())

			return exec.Command(runner, "--collect", "--quiet", "--unit", unit,
				running, "-install", "-db", databaseFile), true, nil
		}
	}

	command := exec.Command(running, "-install", "-db", databaseFile)

	report, err := os.OpenFile(installReportPath(databaseFile, false),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, installReportMode)
	if err != nil {
		return nil, false, fmt.Errorf("failed to open the file the install writes its report to: %w", err)
	}

	command.Stdout = report
	command.Stderr = report

	return command, false, nil
}

// installReportMode is what the file the install writes its report to is made
// with. It is read by whoever is looking into an install that did not finish,
// and it sits in the data directory, which is the account the service runs as
// and nobody else.
const installReportMode = 0o600

// installReportPath is where the report of an install can be read.
//
// A transient unit writes to the journal, which is where everything else this
// service says goes, so there is nothing to name. A plain child writes to a
// file beside the log of the service.
func installReportPath(databaseFile string, apart bool) string {
	if apart {
		return "the journal"
	}

	return filepath.Join(filepath.Dir(databaseFile), "logs", "update-install.log")
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

		handler.Look(ctx)

		// Only a release that was read, that compares, and that compares as
		// newer starts an install. Those three are asked as one question, in
		// NewerAvailable, so that this call site cannot drop one of them.
		if !installable || !set.UpdateAutoInstall || !handler.NewerAvailable() {
			continue
		}

		logger.Warn("a newer release is being installed because the settings ask for it. This "+
			"service is restarted at the end of it, which takes every tunnel down",
			logid.UpdateAutoInstallStarting.Field())

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
