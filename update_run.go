package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/api"
	"github.com/jollaman999/tunnel-manager/internal/crypto"
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
//
// ended is called once the install is over without having ended this process,
// which is what lets the Update screen start another one, with why the install
// failed where it says (installProblem). An install that holds the lock of this
// installation already, one typed at a terminal, is refused here with
// install.ErrInstallRunning rather than started to be refused by that lock out
// of sight of the press that asked for it.
func startUpdateInstall(logger *zap.Logger, databaseFile string, ended func(problem string)) error {
	dataDir := install.Defaults().WithDatabase(databaseFile).DataDir
	if install.InstallRunning(dataDir) {
		return fmt.Errorf("%w: %s is held", install.ErrInstallRunning, install.InstallLockFile(dataDir))
	}

	// A reason left by an install before this one would be read as this one's
	// once it is over, so it goes before this one starts.
	err := os.Remove(installProblemPath(dataDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("failed to remove %s, which an install before this one left: %w",
			installProblemPath(dataDir), err)
	}

	running, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to find the file this process was started from: %w", err)
	}

	command, apart, release, err := installCommand(running, databaseFile)
	if err != nil {
		return err
	}

	err = startInstall(command, release)
	if err != nil {
		return fmt.Errorf("failed to run %s with -install: %w", running, err)
	}

	logger.Info("the install is running as a process of its own. This service is restarted at the "+
		"end of it, which is what ends this process",
		logid.UpdateInstallAsked.Field(),
		zap.Int("pid", command.Process.Pid),
		zap.Bool("outside_this_service", apart),
		zap.String("writes_its_report_to", installReportPath(databaseFile, apart)))

	// The child is not waited for here. It outlives the wait by design: what it
	// does last is restart this service, and a wait would be this process
	// sitting on the answer to a request while the thing it is waiting for
	// takes it down. It is waited for on the side, only to say when it ended.
	go func() {
		err := command.Wait()

		// A plain child is the install, so its end is the install's. On
		// systemd the child is systemd-run, which ends once the unit has
		// started, and the install is then over when the unit lets the lock
		// go. A systemd-run that failed started no unit at all.
		if apart && err == nil {
			over := waitForInstallEnd(dataDir, installLockAppearWait, installLockHeldWait, installLockPoll)
			if !over {
				logger.Warn("the lock of this installation has been held for longer than an install takes. "+
					"The install is taken as over so that another one can be started; if one is still "+
					"running, the lock refuses the next",
					logid.UpdateInstallWaitGaveUp.Field(),
					zap.String("lock", install.InstallLockFile(dataDir)),
					zap.Duration("waited", installLockHeldWait))
			}
		}

		if apart && err != nil {
			logger.Error("systemd-run did not start the install",
				logid.UpdateInstallStartFailed.Field(), zap.Error(err))

			ended("systemd-run did not start the install: " + err.Error())

			return
		}

		ended(installProblem(dataDir))
	}()

	return nil
}

// installLockAppearWait is how long an install handed to systemd is given to
// take the lock of the installation, and installLockPoll is how often the lock
// is looked at meanwhile and afterwards.
//
// The lock is the first thing -install takes once it has checked its
// privilege, so it takes the time a process needs to start. A unit that is not
// seen holding it in this long has ended, refused or failed before it took the
// lock or so soon after that no look fell in between, which is an install that
// is over either way.
//
// installLockHeldWait is how long the lock is waited on once it has been
// taken. An install puts a bound on every step it takes: two minutes for the
// whole download (fetchTimeout), two for the stop and two for the start of the
// service (jobTimeout), and thirty seconds for each of the few other calls to
// systemctl (commandTimeout), which adds up to under ten minutes. Thirty is
// three times that. What it is there for is a lock that never reads as let go:
// one that holds something other than a process id, or names a process id the
// system has handed to another process since. Waited on without an end, that
// lock would keep the Update screen refusing every install until the service
// restarted. Giving up here does not let two installs run at once: the lock is
// still what an install has to take, and a second one is refused by it.
var (
	installLockAppearWait = time.Minute
	installLockHeldWait   = 30 * time.Minute
	installLockPoll       = 2 * time.Second
)

// waitForInstallEnd waits for an install of the installation whose data
// directory is dataDir to take the lock and let it go again, or for appear to
// pass without it having taken the lock. It answers false where it gave up on
// a lock that was still held after held.
//
// Both halves are needed because the wait starts before the install has run.
// Read only as "is the lock held", the moment between systemd-run ending and
// the install taking the lock reads as an install that is over, and a press
// in that moment would start a second one.
//
// A reason left in installProblemPath ends the first half as well. It is
// written by an install that has failed, before it lets the lock go, and one
// that took the lock and let it go between two looks would otherwise be waited
// on for the whole of appear while the Update screen waited on it.
func waitForInstallEnd(dataDir string, appear time.Duration, held time.Duration, poll time.Duration) bool {
	deadline := time.Now().Add(appear)

	for !install.InstallRunning(dataDir) {
		if time.Now().After(deadline) || installProblemLeft(dataDir) {
			return true
		}

		time.Sleep(poll)
	}

	deadline = time.Now().Add(held)

	for install.InstallRunning(dataDir) {
		if time.Now().After(deadline) {
			return false
		}

		time.Sleep(poll)
	}

	return true
}

// startInstall starts the command installCommand made and then lets go of what
// this process opened for it, whether the start worked or not.
//
// The child is handed a copy of the report file and writes through that copy,
// so the one this process holds is of no further use. Kept open, it is a handle
// leaked on every press, and on Windows a file with a handle open on it cannot
// be deleted or renamed, which is a report nobody can clear away while this
// service runs.
func startInstall(command *exec.Cmd, release func()) error {
	defer release()

	return command.Start()
}

// installCommand is the command that runs the install, whether it will run
// outside this service, and what lets go of what was opened for it. The last
// is called once the command has been started, or once starting it failed.
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
//
// Either way -install carries fromUpdateFlag, which keeps it from putting the
// executable that is running back in place when the release cannot be used.
func installCommand(running string, databaseFile string) (*exec.Cmd, bool, func(), error) {
	if runtime.GOOS == "linux" {
		runner, err := exec.LookPath("systemd-run")
		if err == nil {
			unit := fmt.Sprintf("tunnel-manager-install-%d", os.Getpid())

			return exec.Command(runner, "--collect", "--quiet", "--unit", unit,
				running, "-install", "-"+fromUpdateFlag, "-db", databaseFile), true, func() {}, nil
		}
	}

	command := exec.Command(running, "-install", "-"+fromUpdateFlag, "-db", databaseFile)

	// On Windows the report is made with the DACL of the owner, SYSTEM and
	// Administrators first, as the log beside it is. On Unix this does nothing.
	err := crypto.ReservePrivateFile(installReportPath(databaseFile, false))
	if err != nil {
		return nil, false, nil, fmt.Errorf("failed to open the file the install writes its report to: %w", err)
	}

	report, err := os.OpenFile(installReportPath(databaseFile, false),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, installReportMode)
	if err != nil {
		return nil, false, nil, fmt.Errorf("failed to open the file the install writes its report to: %w", err)
	}

	command.Stdout = report
	command.Stderr = report

	return command, false, func() {
		_ = report.Close()
	}, nil
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

// installProblemPath is where an install this service started leaves why it
// failed (leaveInstallProblem), for the service to read once it is over. It
// sits in the data directory beside the lock, which the install and the service
// both work out from the same -db.
func installProblemPath(dataDir string) string {
	return filepath.Join(dataDir, "update-install.problem")
}

// installProblemLeft says whether an install has left a reason.
func installProblemLeft(dataDir string) bool {
	_, err := os.Lstat(installProblemPath(dataDir))

	return err == nil
}

// installProblem is the reason an install left, empty where it left none. An
// install that went through leaves none, and neither does one that never ran.
//
// A reason that is there and cannot be read is said as that. The file is only
// there because the install failed, and an empty answer would be the Update
// screen saying nothing about an install that did not go through.
func installProblem(dataDir string) string {
	path := installProblemPath(dataDir)

	said, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}

	if err != nil {
		return fmt.Sprintf("the install failed, and why could not be read from %s: %v", path, err)
	}

	return strings.TrimSpace(string(said))
}

// answerInstallProblemLeft hands left the reason an install left in dataDir
// before this process started, and nothing where it left none.
//
// It is there for an install this service started that failed after it had
// stopped the service. The install starts the service again on its way out and
// writes the reason after that, so the process that started the install, which
// would have read it once the install was over, is gone, and this one is what
// the Update screen talks to now.
//
// The install writes the reason before it lets the lock go, and this process
// may be up while it still holds it. So a lock that is held is waited on, on
// the side and for as long as the wait in startUpdateInstall gives an install,
// and the reason is read once it is let go. A lock that is not held is an
// install that is over, and what it left is read at once.
func answerInstallProblemLeft(logger *zap.Logger, dataDir string, left func(problem string)) {
	answer := func() {
		problem := installProblem(dataDir)
		if problem != "" {
			left(problem)
		}
	}

	if !install.InstallRunning(dataDir) {
		answer()

		return
	}

	go func() {
		over := waitForInstallEnd(dataDir, 0, installLockHeldWait, installLockPoll)
		if !over {
			logger.Warn("the lock of this installation has been held for longer than an install takes. "+
				"The install is taken as over, and the reason it left for the Update screen, if any, "+
				"is read now",
				logid.UpdateInstallWaitGaveUp.Field(),
				zap.String("lock", install.InstallLockFile(dataDir)),
				zap.Duration("waited", installLockHeldWait))
		}

		answer()
	}()
}

// runUpdateChecks looks for a newer release on a timer, and installs one where
// the settings say to.
//
// It reads the settings on every pass rather than holding what they said at
// startup, so that turning the check off on the Settings screen stops it
// without a restart, the way the log level takes hold without one.
//
// The first look is one interval after the last one, which a restart does not
// forget: the time of every look is kept in dataDir (keptUpdateChecks), and
// where none is kept the start stands in for it. A machine that has just come
// up is doing everything else it does at startup, and a release that appeared
// while it was down is not a thing that has to be known in the first second.
// It also keeps a restart loop from making a request every time round. And a
// machine that is stopped every night, with a day between looks, still looks
// once a day: counted from every start, it would never look at all.
func runUpdateChecks(ctx context.Context, logger *zap.Logger, db *gorm.DB,
	handler *api.UpdateHandler, installable bool, dataDir string) {
	logger.Info("looking for a newer release on a timer",
		logid.UpdateCheckLoopStarted.Field())

	// The timer is armed for the shortest interval the settings take and the
	// pass decides whether it is due. Rearming it from the stored interval
	// instead would leave a setting changed from a day to an hour waiting out
	// the day that was already running.
	ticker := time.NewTicker(updateCheckTick)
	defer ticker.Stop()

	// The interval is read on every pass and measured from the last look, so a
	// day shortened to an hour after startup is due an hour after that look.
	last := lastUpdateCheck(logger, dataDir, updateCheckNow())

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
		if updateCheckNow().Sub(last) < due {
			continue
		}

		last = updateCheckNow()

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
var updateCheckTick = time.Minute

// updateCheckNow is the clock the pass above measures the interval with. It is
// time.Now outside tests; a test that waited out an interval of an hour could
// not be run.
var updateCheckNow = time.Now

// updateCheckPath is where the time of the last look for a newer release is
// kept, in the data directory beside the lock.
func updateCheckPath(dataDir string) string {
	return filepath.Join(dataDir, "update-check.last")
}

// keptUpdateChecks is check with the time of every call written into
// updateCheckPath before the request goes out, for the loop to measure its
// interval from after a restart (lastUpdateCheck).
//
// It is wrapped round the check the Update handler is handed rather than put
// in the loop, so that a look the Update screen asks for is kept as well: the
// handler is where the screen and the loop both land.
//
// Every request counts, whether it read a release or failed. What the time is
// kept for is a service that restarts within the interval, and one going round
// a restart loop with the network down or the API refusing it is one of those:
// counting only the looks that worked would have it make a request every time
// round, which is what measuring from the last look is there to stop. A look
// that failed is not tried again before the interval without a restart either.
func keptUpdateChecks(logger *zap.Logger, dataDir string,
	check func(context.Context, string) (install.Latest, error)) func(context.Context, string) (install.Latest, error) {
	return func(ctx context.Context, version string) (install.Latest, error) {
		keepUpdateCheck(logger, dataDir, updateCheckNow())

		return check(ctx, version)
	}
}

// keepUpdateCheck writes at into updateCheckPath.
//
// A time that cannot be kept is said in the log and the look goes ahead. The
// look is what was asked for; what is lost is only that a restart before the
// next one counts the interval from its start.
func keepUpdateCheck(logger *zap.Logger, dataDir string, at time.Time) {
	path := updateCheckPath(dataDir)

	// On Windows the file is made with the DACL of the owner, SYSTEM and
	// Administrators first, as the reason an install leaves beside it is. On
	// Unix this does nothing.
	err := crypto.ReservePrivateFile(path)
	if err == nil {
		err = os.WriteFile(path, []byte(at.UTC().Format(time.RFC3339Nano)), installReportMode)
	}

	if err != nil {
		logger.Warn("the time of this look for a newer release could not be kept. A restart before "+
			"the interval has gone by counts the interval from its start",
			logid.UpdateCheckTimeNotKept.Field(),
			zap.String("path", path),
			zap.Error(err))
	}
}

// lastUpdateCheck is the time keepUpdateCheck kept in dataDir, or start where
// there is none to go by.
//
// No file is a service that has never looked, and it starts from start
// without a word. A file that cannot be read, or that does not hold a time,
// is said in the log and starts from start as well. So is a time after start:
// the clock has been put back since it was written, and measured from it the
// next look would wait out the interval and however far the clock went back.
func lastUpdateCheck(logger *zap.Logger, dataDir string, start time.Time) time.Time {
	path := updateCheckPath(dataDir)

	said, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return start
	}

	var last time.Time

	if err == nil {
		last, err = time.Parse(time.RFC3339Nano, strings.TrimSpace(string(said)))
	}

	if err == nil && last.After(start) {
		err = fmt.Errorf("it says %s, which is after this start at %s",
			last.Format(time.RFC3339), start.UTC().Format(time.RFC3339))
	}

	if err != nil {
		logger.Warn("the time of the last look for a newer release could not be used. The interval "+
			"is counted from this start",
			logid.UpdateCheckTimeNotRead.Field(),
			zap.String("path", path),
			zap.Error(err))

		return start
	}

	return last
}
