package api

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/auth"
	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// tunnelStopper is what the uninstall needs from the tunnel manager. Nothing
// else of it is reachable from here on purpose: the one thing that has to
// happen before the files go is that no tunnel is left on a remote host.
type tunnelStopper interface {
	StopAllTunnels()
}

// uninstallExitDelay is how long the process stays up after the answer is
// written. The answer is what the browser draws the screen that says the
// installation is gone, and a process that exits while that answer is still on
// the wire leaves the operator with a page that cannot say what happened and
// cannot ask again either, since there is nothing left to ask.
//
// Three seconds is far more than a body of this size needs on a link the UI is
// usable over at all, and it is short enough that an operator watching the
// service does not start wondering whether the process hung.
const uninstallExitDelay = 3 * time.Second

// rotatedLogTimeFormat is the timestamp lumberjack puts into the name of a
// rotated log file. It is spelled out here because that package keeps it
// unexported (gopkg.in/natefinch/lumberjack.v2@v2.2.1, backupTimeFormat), and a
// name is only treated as a rotated log when this parses out of it: matching on
// the prefix alone would take a file somebody else left in the log directory.
const rotatedLogTimeFormat = "2006-01-02T15-04-05.000"

// rotatedLogCompressSuffix is what lumberjack appends to a rotated file it
// compressed (compressSuffix in the same file).
const rotatedLogCompressSuffix = ".gz"

// UninstallPaths names the files this installation is made of.
//
// They are handed in from the startup rather than read out of the settings
// here, so that what is removed is what this process actually opened. A key
// file or a log file that was changed on the Settings screen without a restart
// names a file this process never wrote to, and the running one would be left
// behind while an unrelated file went.
type UninstallPaths struct {
	// DatabaseFile is the SQLite file. The write ahead log and the shared
	// memory file that sit next to it are worked out from this one.
	DatabaseFile string
	// KeyFile holds the key the stored SSH passwords are sealed with. Removing
	// it is the step that cannot be undone even with a backup of the database.
	KeyFile string
	// InitialPasswordFile is written on the first startup and removed once the
	// account is set up, so it is usually not there any more.
	InitialPasswordFile string
	// LogFile is the file the logs are written to. The rotated ones beside it
	// are found by their names.
	LogFile string
}

// UninstallHandler removes this installation and ends the process.
//
// It is apart from the other handlers because what it needs is not rows: it
// stops the loop that keeps the tunnels up, closes the database and deletes
// files, and every one of those is held by the startup. They are handed in
// rather than reached for here, so that a test can watch the order they are
// used in, which is the part of this that cannot be checked after the fact.
type UninstallHandler struct {
	db      *gorm.DB
	logger  *zap.Logger
	manager tunnelStopper
	// stopReconcile ends the reconcile loop and returns once the pass it was in
	// has ended. A loop that is still running reads the desired state and
	// starts again what was just stopped, so this runs before anything is
	// removed.
	stopReconcile func()
	paths         UninstallPaths
	// exit ends the process. It is a function of the startup rather than
	// os.Exit, so that the process goes down the same way a signal takes it
	// down and so that a test is not ended by the code it is testing.
	exit func()
	// exitAfter is the wait between the answer and exit. It is a field and not
	// the constant itself so that a test does not have to sit through it.
	exitAfter time.Duration
}

func NewUninstallHandler(db *gorm.DB, logger *zap.Logger, manager tunnelStopper,
	stopReconcile func(), paths UninstallPaths, exit func()) *UninstallHandler {
	return &UninstallHandler{
		db:            db,
		logger:        logger,
		manager:       manager,
		stopReconcile: stopReconcile,
		paths:         paths,
		exit:          exit,
		exitAfter:     uninstallExitDelay,
	}
}

// uninstallRequest is what the screen sends. The password of the account is
// asked for again because a session that is open on an unattended screen is
// otherwise one click away from removing the installation.
type uninstallRequest struct {
	Password string `json:"password"`
}

// removedFile is one file the uninstall dealt with, named by what it was so
// that the screen does not have to know what a path means.
type removedFile struct {
	Path string `json:"path"`
	What string `json:"what"`
	// Error is filled in only for a file that could not be removed.
	Error string `json:"error,omitempty"`
}

// uninstallResult is what the uninstall answers with. The files that could not
// be removed are carried apart from the ones that went, because they are the
// ones left for the operator to deal with by hand, and after this answer there
// is no server left to ask about them.
type uninstallResult struct {
	Removed []removedFile `json:"removed"`
	Failed  []removedFile `json:"failed"`
	// ExitInSec is how long the process stays up after this answer. The screen
	// says it, so the number comes from the side that waits it out.
	ExitInSec int `json:"exit_in_sec"`
}

// Uninstall removes this installation.
//
// The order is the whole of it. The password is checked before anything is
// touched, the tunnels are taken down before the files go so that no listener
// is left on a remote host, and the loop that would build them again is stopped
// first of all. The answer is written while the process is still up, and the
// process ends a few seconds later.
func (h *UninstallHandler) Uninstall(c echo.Context) error {
	var req uninstallRequest

	err := c.Bind(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestBodyInvalid, errorArgs{"reason": err.Error()})
	}

	userID, ok := c.Get(contextUserIDKey).(uint)
	if !ok {
		// The middleware is what puts it there, so getting here means the route
		// was hung somewhere the middleware does not cover.
		h.logger.Error("the uninstall was reached with no account on the context", logid.UninstallNoAccountOnContext.Field())
		return failure(c, http.StatusInternalServerError, errAccountReadFailed)
	}

	var user models.User

	err = h.db.First(&user, userID).Error
	if err != nil {
		h.logger.Error("failed to read the account", logid.AccountReadFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errAccountReadFailed)
	}

	if !auth.CheckPassword(user.PasswordHash, req.Password) {
		// Nothing has been touched at this point, and the log says so: an
		// operator reading it later has to be able to tell an uninstall that
		// was refused from one that ran.
		h.logger.Warn("the uninstall was asked for with a password that does not open the account, "+
			"so nothing was stopped and nothing was removed",
			logid.UninstallPasswordWrong.Field())

		return failure(c, http.StatusUnauthorized, errUninstallPasswordWrong)
	}

	h.logger.Warn("the uninstall was asked for and the password opens the account. The tunnels are "+
		"stopped, the files of this installation are removed and the process ends",
		logid.UninstallStarting.Field())

	// The loop goes first. It is the only thing that starts tunnels, and it
	// builds again whatever is stopped while it runs, so stopping the tunnels
	// ahead of it would take them down for one pass of the loop and no longer.
	// This is the order the shutdown at the end of main uses for the same reason.
	h.stopReconcile()

	// Every step says it did its part. This is the one call that cannot be
	// taken back, and the log is all that is left of it: the file it was written
	// to is removed a moment later, but the console the service writes to keeps
	// it, and the order in it is what tells a tunnel that was stopped from one
	// that went down with the process.
	h.logger.Info("stopped the reconcile loop, so nothing starts a tunnel again",
		logid.UninstallReconcileLoopStopped.Field())

	// The tunnels are taken down before any file goes, so that no listener is
	// left behind on a remote host. A process that exits closes its connections,
	// but it is the manager that knows what it opened.
	h.manager.StopAllTunnels()

	h.logger.Info("stopped every tunnel", logid.UninstallTunnelsStopped.Field())

	h.closeDatabase()

	result := h.removeFiles()
	result.ExitInSec = int(h.exitAfter / time.Second)

	// The answer is written before the timer starts, so the wait is the time the
	// browser has to receive it in rather than time the writing ate into.
	err = c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    result,
	})
	if err != nil {
		// The files are gone either way. A process left running on an
		// installation that no longer exists serves nothing, so it still ends.
		h.logger.Error("failed to write the answer of the uninstall",
			logid.UninstallAnswerWriteFailed.Field(),
			zap.Error(err))
	}

	time.AfterFunc(h.exitAfter, h.exit)

	return err
}

// closeDatabase closes the handle before the file is removed.
//
// SQLite writes the write ahead log back into the database file and removes the
// -wal and the -shm file as the last connection closes. Removing the file under
// an open connection would leave those two behind instead, and a checkpoint
// that ran afterwards would write a database file back out.
func (h *UninstallHandler) closeDatabase() {
	sqlDB, err := h.db.DB()
	if err != nil {
		h.logger.Error("failed to reach the database handle to close it",
			logid.UninstallDatabaseHandleUnreachable.Field(),
			zap.Error(err))
		return
	}

	err = sqlDB.Close()
	if err != nil {
		// The files are removed anyway. A handle that did not close holds
		// nothing this process needs any more, and it goes with the process.
		h.logger.Error("failed to close the database", logid.UninstallDatabaseCloseFailed.Field(), zap.Error(err))
		return
	}

	h.logger.Info("closed the database", logid.UninstallDatabaseClosed.Field())
}

// removeFiles deletes everything this installation is made of and reports what
// became of each file.
//
// A file that cannot be removed does not stop the rest. On Windows a file that
// is open cannot be deleted, and the log file this process writes to is exactly
// that, so a run that stopped at the first failure would leave the database and
// the key behind over the one file that was never going to go.
func (h *UninstallHandler) removeFiles() uninstallResult {
	result := uninstallResult{
		Removed: []removedFile{},
		Failed:  []removedFile{},
	}

	for _, file := range h.installedFiles() {
		err := os.Remove(file.Path)

		switch {
		case err == nil:
			h.logger.Info("removed a file of the installation",
				logid.UninstallFileRemoved.Field(),
				zap.String("what", file.What),
				zap.String("path", file.Path))

			result.Removed = append(result.Removed, file)
		case errors.Is(err, fs.ErrNotExist):
			// Nothing was there. The initial password file is removed once the
			// account is set up, and SQLite removes the -wal and the -shm file
			// itself as the connection closes, so this is the usual case for
			// several of these and is not worth an entry on the screen.
			h.logger.Debug("a file of the installation was not there",
				logid.UninstallFileNotThere.Field(),
				zap.String("what", file.What),
				zap.String("path", file.Path))
		default:
			h.logger.Error("failed to remove a file of the installation",
				logid.UninstallFileRemoveFailed.Field(),
				zap.String("what", file.What),
				zap.String("path", file.Path),
				zap.Error(err))

			file.Error = err.Error()
			result.Failed = append(result.Failed, file)
		}
	}

	return result
}

// installedFiles is every file that goes, in the order it goes.
//
// The executable is not among them. Windows does not let a running process
// delete its own image, and on Unix it would stay on disk until the process
// ends anyway, which makes it half of a job rather than one done.
//
// The log file is last. Everything above it is reported through the log while
// it is still being written, and on a system where an open file cannot be
// removed it is the one that fails.
func (h *UninstallHandler) installedFiles() []removedFile {
	files := []removedFile{
		{Path: h.paths.DatabaseFile, What: "the database"},
		// The two files SQLite keeps beside the database in WAL mode. The
		// database is in that mode (internal/database/database.go, sqliteDSN),
		// so removing the file alone would leave these behind, and a -wal still
		// holds the rows that were written last.
		{Path: databaseSidecar(h.paths.DatabaseFile, "-wal"), What: "the write ahead log of the database"},
		{Path: databaseSidecar(h.paths.DatabaseFile, "-shm"), What: "the shared memory file of the database"},
		{Path: h.paths.KeyFile, What: "the encryption key"},
		{Path: h.paths.InitialPasswordFile, What: "the initial password file"},
	}

	for _, rotated := range h.rotatedLogFiles() {
		files = append(files, removedFile{Path: rotated, What: "a rotated log file"})
	}

	files = append(files, removedFile{Path: h.paths.LogFile, What: "the log file"})

	// A path that was never configured names nothing, and os.Remove("") reports
	// a failure the screen would show as a file that could not be removed.
	kept := make([]removedFile, 0, len(files))

	for _, file := range files {
		if file.Path == "" {
			continue
		}

		file.Path = absolutePath(file.Path)
		kept = append(kept, file)
	}

	return kept
}

// databaseSidecar names one of the two files SQLite keeps next to the database.
// They carry the name of the database file with the suffix appended, extension
// and all, so they are built from the path rather than from its stem.
func databaseSidecar(databaseFile string, suffix string) string {
	if databaseFile == "" {
		return ""
	}

	return databaseFile + suffix
}

// rotatedLogFiles finds what the log rotation left beside the log file.
//
// lumberjack writes a rotated file as the name of the log file with a timestamp
// put in front of the extension, and appends .gz to one it compressed
// afterwards. The directory is read and the names are matched rather than
// globbed, because a path may hold the characters a pattern is made of, and it
// is a delete that is at the end of this.
func (h *UninstallHandler) rotatedLogFiles() []string {
	if h.paths.LogFile == "" {
		return nil
	}

	dir := filepath.Dir(h.paths.LogFile)
	name := filepath.Base(h.paths.LogFile)
	ext := filepath.Ext(name)
	prefix := strings.TrimSuffix(name, ext) + "-"

	entries, err := os.ReadDir(dir)
	if err != nil {
		// The log file itself is removed on its own below, so a directory that
		// cannot be read costs the rotated files and nothing else.
		h.logger.Error("failed to read the log directory, so the rotated log files are left behind",
			logid.UninstallLogDirectoryReadFailed.Field(),
			zap.String("directory", dir),
			zap.Error(err))

		return nil
	}

	var rotated []string

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		if !isRotatedLogName(entry.Name(), prefix, ext) {
			continue
		}

		rotated = append(rotated, filepath.Join(dir, entry.Name()))
	}

	return rotated
}

// isRotatedLogName reports whether a name is one the rotation wrote. The
// timestamp is parsed and not merely looked at, which is what keeps a file
// somebody named tunnel-manager-old.log out of the list.
func isRotatedLogName(name string, prefix string, ext string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}

	stamp := strings.TrimPrefix(name, prefix)
	stamp = strings.TrimSuffix(stamp, rotatedLogCompressSuffix)

	if !strings.HasSuffix(stamp, ext) {
		return false
	}

	stamp = strings.TrimSuffix(stamp, ext)

	_, err := time.Parse(rotatedLogTimeFormat, stamp)

	return err == nil
}

// absolutePath is how a path is reported. The configured value may be relative,
// and a relative path is read against the working directory, which differs
// between running from the repository, from the container and from systemd. A
// path that could not be resolved is reported as it was written, since a
// failure here says nothing about the file.
func absolutePath(path string) string {
	resolved, err := filepath.Abs(path)
	if err != nil {
		return path
	}

	return resolved
}
