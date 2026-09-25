package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/go-playground/validator/v10"
	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/settings"
	"github.com/jollaman999/tunnel-manager/internal/web"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gopkg.in/natefinch/lumberjack.v2"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// newLoggingSettings returns settings whose logging values are filled in and
// whose log file is the given path. Everything else is left at zero, because
// the startup steps exercised here read nothing else.
func newLoggingSettings(path string) *settings.Settings {
	return &settings.Settings{
		LoggingLevel:          "info",
		LoggingFormat:         "json",
		LoggingFilePath:       path,
		LoggingFileMaxSize:    1,
		LoggingFileMaxBackups: 1,
		LoggingFileMaxAge:     1,
	}
}

// newLogger builds the logger initLogger describes, which is what main does
// with the core it returns.
func newLogger(t *testing.T, s *settings.Settings) (*zap.Logger, error) {
	t.Helper()

	core, _, _, err := initLoggerForTest(t, s, "")
	if err != nil {
		return nil, err
	}

	return zap.New(core), nil
}

// initLoggerForTest is initLogger with the log file closed when the test ends,
// ahead of the removal of the temporary directory it is in: Windows does not
// remove a file that is still open. Emptying the log is what closes the writer
// that holds it, and nothing reads the file once the test is over.
func initLoggerForTest(t *testing.T, s *settings.Settings, installDir string) (zapcore.Core, zap.AtomicLevel,
	func() error, error) {
	t.Helper()

	core, level, empty, err := initLogger(s, installDir)
	if empty != nil {
		t.Cleanup(func() {
			_ = empty()
		})
	}

	return core, level, empty, err
}

// openPathsOfThisProcess returns the file every descriptor of this process
// points at, so a descriptor that was left behind can be told from one that
// was closed.
func openPathsOfThisProcess(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("the open descriptors of this process cannot be read: %v", err)
	}

	var paths []string

	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err != nil {
			// A descriptor closed between the listing and the readlink, which
			// says nothing about the one under test.
			continue
		}
		paths = append(paths, target)
	}

	return paths
}

// TestDefaultDatabasePathIsUnderTheUserConfigDir covers the case the binary is
// downloaded and started with no -db at all: the file lands where the platform
// keeps user data, not in whatever directory the process was started from.
func TestDefaultDatabasePathIsUnderTheUserConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	path, err := defaultDatabasePath()
	if err != nil {
		t.Fatalf("defaultDatabasePath: %v", err)
	}

	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir: %v", err)
	}

	want := filepath.Join(dir, "tunnel-manager", databaseFileName)
	if path != want {
		t.Errorf("the default database path is %q, want %q", path, want)
	}
}

// TestDefaultDatabasePathRefusesToGuessWithoutAHome is the other half of the
// default. With neither XDG_CONFIG_HOME nor HOME there is no place the platform
// calls its own, and inventing one would let one startup build a database in
// one directory and the next one build another somewhere else, so the Hosts
// that were registered would look gone.
func TestDefaultDatabasePathRefusesToGuessWithoutAHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("AppData", "")

	path, err := defaultDatabasePath()
	if err == nil {
		t.Fatalf("defaultDatabasePath made up a path: %q", path)
	}
	if !strings.Contains(err.Error(), "-db") {
		t.Errorf("error = %v, want it to name the -db flag", err)
	}
	if !strings.Contains(err.Error(), "absolute path") {
		t.Errorf("error = %v, want it to ask for an absolute path", err)
	}
}

func TestPrepareLogFileCreatesTheDirectoryAndTheFile(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "logs", "tunnel-manager.log")

	err := prepareLogFile(newLoggingSettings(logFile), "")
	if err != nil {
		t.Fatalf("failed to prepare a log file under a directory that can be created: %v", err)
	}

	info, err := os.Stat(logFile)
	if err != nil {
		t.Fatalf("the log file was not created: %v", err)
	}
	if info.IsDir() {
		t.Fatalf("the log path was created as a directory")
	}
}

// TestPrepareLogFileLeavesNoDescriptorOpenOnTheLogFile records what the
// function does today: the descriptor it opens to see that the file can be
// written to is closed again before it returns. The logs themselves go through
// lumberjack, which opens the file on its own, so a descriptor kept here would
// be held for the life of the process for nothing.
func TestPrepareLogFileLeavesNoDescriptorOpenOnTheLogFile(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "logs", "tunnel-manager.log")

	before := len(openPathsOfThisProcess(t))

	err := prepareLogFile(newLoggingSettings(logFile), "")
	if err != nil {
		t.Fatalf("failed to prepare the log file: %v", err)
	}

	for _, path := range openPathsOfThisProcess(t) {
		if path == logFile {
			t.Fatalf("a descriptor on %s was left open after the log file was prepared", logFile)
		}
	}

	after := len(openPathsOfThisProcess(t))
	if after > before {
		t.Fatalf("%d descriptors were open before the log file was prepared and %d after", before, after)
	}
}

func TestPrepareLogFileReportsADirectoryThatCannotBeCreated(t *testing.T) {
	// The parent of the log directory is a regular file, so the directory
	// cannot be created whatever the privileges of the process are.
	blocking := filepath.Join(t.TempDir(), "not-a-directory")

	err := os.WriteFile(blocking, []byte("x"), 0644)
	if err != nil {
		t.Fatalf("failed to write the blocking file: %v", err)
	}

	err = prepareLogFile(newLoggingSettings(filepath.Join(blocking, "logs", "tunnel-manager.log")), "")
	if err == nil {
		t.Fatal("a log directory that cannot be created was reported as prepared")
	}
	if !strings.Contains(err.Error(), "failed to create log directory") {
		t.Fatalf("the failure does not name the log directory: %v", err)
	}
}

func TestPrepareLogFileReportsALogFileThatCannotBeOpened(t *testing.T) {
	// The log path itself is a directory, so opening it for writing fails
	// whatever the privileges of the process are.
	logFile := filepath.Join(t.TempDir(), "tunnel-manager.log")

	err := os.Mkdir(logFile, 0755)
	if err != nil {
		t.Fatalf("failed to create the directory that takes the place of the log file: %v", err)
	}

	err = prepareLogFile(newLoggingSettings(logFile), "")
	if err == nil {
		t.Fatal("a log file that cannot be opened was reported as prepared")
	}
	if !strings.Contains(err.Error(), "failed to create log file") {
		t.Fatalf("the failure does not name the log file: %v", err)
	}
}

// platformAbsolutePath spells the Unix path p as an absolute path of the
// platform the test runs on. filepath decides what is absolute by the rules of
// that platform, and on Windows a path without a drive is not.
func platformAbsolutePath(p string) string {
	if runtime.GOOS == "windows" {
		return `C:` + filepath.FromSlash(p)
	}

	return p
}

// skipWithoutFileModes leaves a test that reads permission bits where there are
// none to read. Windows carries no Unix mode, and os.Chmod there turns the
// read-only attribute on and off rather than writing the bits this asks about.
func skipWithoutFileModes(t *testing.T) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("this platform carries no Unix permission bits")
	}
}

// requireMode fails unless the path is at exactly that permission.
func requireMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to read the permission of %s: %v", path, err)
	}

	if info.Mode().Perm() != want {
		t.Fatalf("%s has permission %#o, want %#o", path, info.Mode().Perm(), want)
	}
}

// TestANewLogFileIsClosedToTheRestOfTheMachine covers the first startup. The
// log says which hosts this installation reaches and under which account
// names, and a file the whole machine can read hands a local user that map.
func TestANewLogFileIsClosedToTheRestOfTheMachine(t *testing.T) {
	skipWithoutFileModes(t)

	logFile := filepath.Join(t.TempDir(), "logs", "tunnel-manager.log")

	err := prepareLogFile(newLoggingSettings(logFile), "")
	if err != nil {
		t.Fatalf("failed to prepare the log file: %v", err)
	}

	requireMode(t, logFile, logFileMode)
	requireMode(t, filepath.Dir(logFile), logDirMode)
}

// TestALogFromAnEarlierReleaseIsNarrowedAtStartup is the half that matters to
// a deployment that is already running. Creating new files narrowly does
// nothing for the logs that are already lying there at 0644, so the startup
// sets the mode of the file it finds as well as of one it makes, and it does
// that without touching what the file holds. The directory is found rather
// than made on this startup, so it is left as it was, for the reason
// TestALogDirectoryThatWasAlreadyThereKeepsItsMode gives.
func TestALogFromAnEarlierReleaseIsNarrowedAtStartup(t *testing.T) {
	skipWithoutFileModes(t)

	logDir := filepath.Join(t.TempDir(), "logs")

	err := os.Mkdir(logDir, 0755)
	if err != nil {
		t.Fatalf("failed to make the log directory of an earlier release: %v", err)
	}

	logFile := filepath.Join(logDir, "tunnel-manager.log")

	err = os.WriteFile(logFile, []byte("a line an earlier release wrote\n"), 0644)
	if err != nil {
		t.Fatalf("failed to write the log of an earlier release: %v", err)
	}

	// The directory is set after the file, because writing the file into it
	// needed it open.
	err = os.Chmod(logDir, 0755)
	if err != nil {
		t.Fatalf("failed to put the log directory at the mode of an earlier release: %v", err)
	}

	err = prepareLogFile(newLoggingSettings(logFile), "")
	if err != nil {
		t.Fatalf("failed to prepare the log file: %v", err)
	}

	requireMode(t, logFile, logFileMode)
	requireMode(t, logDir, 0755)

	body, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read the log back: %v", err)
	}
	if !strings.Contains(string(body), "a line an earlier release wrote") {
		t.Fatalf("the lines that were already in the log are gone, it holds %q", string(body))
	}
}

// TestALogDirectoryThatWasAlreadyThereKeepsItsMode covers a log path the
// operator pointed into a directory of their own. The directory is shared with
// whatever else lives in it, so narrowing it would take it away from those as
// well: a log file under /tmp run as root would turn /tmp into 0700 and drop
// the sticky bit that keeps one user from removing the files of another. What
// the log holds is in the file, and that is narrowed either way.
func TestALogDirectoryThatWasAlreadyThereKeepsItsMode(t *testing.T) {
	skipWithoutFileModes(t)

	for _, tc := range []struct {
		name string
		mode os.FileMode
	}{
		{"0755", 0755},
		{"01777", 0777 | os.ModeSticky},
	} {
		mode := tc.mode

		t.Run(tc.name, func(t *testing.T) {
			logDir := filepath.Join(t.TempDir(), "shared")

			err := os.Mkdir(logDir, 0700)
			if err != nil {
				t.Fatalf("failed to make the directory the operator made: %v", err)
			}

			// Mkdir is masked by umask and takes no sticky bit, so the mode
			// is written after it.
			err = os.Chmod(logDir, mode)
			if err != nil {
				t.Fatalf("failed to put the directory at %v: %v", mode, err)
			}

			logFile := filepath.Join(logDir, "tunnel-manager.log")

			err = prepareLogFile(newLoggingSettings(logFile), "")
			if err != nil {
				t.Fatalf("failed to prepare the log file: %v", err)
			}

			info, err := os.Stat(logDir)
			if err != nil {
				t.Fatalf("failed to read the mode of %s: %v", logDir, err)
			}

			got := info.Mode() & (os.ModePerm | os.ModeSticky | os.ModeSetuid | os.ModeSetgid)
			if got != mode {
				t.Fatalf("%s is at %v after the startup, want %v as it was", logDir, got, mode)
			}

			requireMode(t, logFile, logFileMode)
		})
	}
}

// TestTheRotatedLogsAreClosedAsWell reads the mode of every file the logging
// leaves in the directory rather than of the current one alone. lumberjack
// opens the files it rotates to and the compressed copies it writes itself,
// and it takes their mode off the file it is rotating, so the whole directory
// follows from the current log being at 0600. Nothing in this application
// passes it a mode, which is why what it does is pinned here.
func TestTheRotatedLogsAreClosedAsWell(t *testing.T) {
	skipWithoutFileModes(t)

	logDir := filepath.Join(t.TempDir(), "logs")
	logFile := filepath.Join(logDir, "tunnel-manager.log")

	set := newLoggingSettings(logFile)
	set.LoggingFileMaxBackups = 3
	set.LoggingFileCompress = true

	// The logger writes to the console as well as to the file, so the lines
	// this has to write to reach a rotation are kept out of the test output.
	withStdoutCaptured(t, func() {
		core, _, _, err := initLoggerForTest(t, set, "")
		if err != nil {
			t.Errorf("failed to build the logger: %v", err)

			return
		}

		logger := zap.New(core)

		// MaxSize is in megabytes and the smallest lumberjack takes is 1, so
		// this is what it costs to see a rotation at all.
		line := strings.Repeat("x", 1024)
		for i := 0; i < 1200; i++ {
			logger.Info(line)
		}
	})

	// The compression runs in a goroutine of its own, so the file it writes
	// appears some time after the rotation that started it. The wait is
	// bounded: what is asked is only that the mode of whatever was left in the
	// directory is right, and the rotated file that is not compressed yet is
	// already one of them.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		matches, err := filepath.Glob(filepath.Join(logDir, "*.gz"))
		if err != nil {
			t.Fatalf("failed to look for a compressed log: %v", err)
		}
		if len(matches) > 0 {
			break
		}

		time.Sleep(50 * time.Millisecond)
	}

	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatalf("failed to read the log directory: %v", err)
	}

	if len(entries) < 2 {
		t.Fatalf("the log did not rotate, the directory holds %d file(s)", len(entries))
	}

	compressed := false

	for _, entry := range entries {
		requireMode(t, filepath.Join(logDir, entry.Name()), logFileMode)

		if strings.HasSuffix(entry.Name(), ".gz") {
			compressed = true
		}
	}

	if !compressed {
		t.Logf("no compressed log appeared within the wait, so only the rotated ones were read")
	}

	requireMode(t, logDir, logDirMode)
}

// withStdoutCaptured runs the function with os.Stdout replaced by a pipe and
// returns what was written to it. The logger takes hold of os.Stdout when it is
// built, so the replacement has to be in place before initLogger is called.
func withStdoutCaptured(t *testing.T, run func()) string {
	t.Helper()

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create the pipe that stands in for stdout: %v", err)
	}

	saved := os.Stdout
	os.Stdout = write

	collected := make(chan string, 1)

	go func() {
		body, _ := io.ReadAll(read)
		collected <- string(body)
	}()

	func() {
		defer func() {
			os.Stdout = saved
			_ = write.Close()
		}()
		run()
	}()

	output := <-collected
	_ = read.Close()

	return output
}

// TestInitLoggerFallsBackToStdoutWhenTheLogFileCannotBeOpened pins the fallback
// that keeps the logs somewhere when the log file is unusable. Without it the
// startup either has no logger at all or writes nowhere, and an operator
// looking into why the process misbehaves finds nothing.
func TestInitLoggerFallsBackToStdoutWhenTheLogFileCannotBeOpened(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "tunnel-manager.log")

	err := os.Mkdir(logFile, 0755)
	if err != nil {
		t.Fatalf("failed to create the directory that takes the place of the log file: %v", err)
	}

	set := newLoggingSettings(logFile)

	var logger *zap.Logger

	output := withStdoutCaptured(t, func() {
		logger, err = newLogger(t, set)
		if err != nil || logger == nil {
			return
		}
		logger.Info("a line that has nowhere else to go")
	})

	if err != nil {
		t.Fatalf("a log file that cannot be opened ended the startup: %v", err)
	}
	if logger == nil {
		t.Fatal("no logger was returned although no failure was reported")
	}

	if !strings.Contains(output, "a line that has nowhere else to go") {
		t.Fatalf("the entry did not reach stdout while the log file was unusable, stdout: %q", output)
	}
	if !strings.Contains(output, "logging to file is disabled") {
		t.Fatalf("stdout does not say that logging to file is disabled, stdout: %q", output)
	}
}

func TestInitLoggerWritesToTheLogFileWhenItCanBeOpened(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "logs", "tunnel-manager.log")

	logger, err := newLogger(t, newLoggingSettings(logFile))
	if err != nil {
		t.Fatalf("failed to build the logger: %v", err)
	}

	logger.Info("a line that belongs in the log file")

	body, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read the log file: %v", err)
	}
	if !strings.Contains(string(body), "a line that belongs in the log file") {
		t.Fatalf("the entry did not reach the log file, its content is %q", string(body))
	}

	// The format is the configured one, so the file can be read by whatever
	// collects it.
	var entry map[string]interface{}

	firstLine := strings.SplitN(strings.TrimSpace(string(body)), "\n", 2)[0]
	err = json.Unmarshal([]byte(firstLine), &entry)
	if err != nil {
		t.Fatalf("the log file is not written in the configured json format: %v, line: %q", err, firstLine)
	}
}

func TestInitLoggerWritesNothingBelowTheConfiguredLevel(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "logs", "tunnel-manager.log")

	set := newLoggingSettings(logFile)
	set.LoggingLevel = "warn"

	logger, err := newLogger(t, set)
	if err != nil {
		t.Fatalf("failed to build the logger: %v", err)
	}

	logger.Info("an entry below the configured level")
	logger.Warn("an entry at the configured level")

	body, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read the log file: %v", err)
	}

	if strings.Contains(string(body), "an entry below the configured level") {
		t.Fatalf("an entry below the configured level was written, content: %q", string(body))
	}
	if !strings.Contains(string(body), "an entry at the configured level") {
		t.Fatalf("an entry at the configured level was not written, content: %q", string(body))
	}
}

func TestInitLoggerRefusesALevelItCannotRead(t *testing.T) {
	set := newLoggingSettings(filepath.Join(t.TempDir(), "logs", "tunnel-manager.log"))
	set.LoggingLevel = "chatty"

	core, _, _, err := initLoggerForTest(t, set, "")
	if err == nil {
		t.Fatal("a log level that cannot be read was accepted")
	}
	if core != nil {
		t.Fatal("a core was returned for a log level that cannot be read")
	}
	if !strings.Contains(err.Error(), "failed to parse log level") {
		t.Fatalf("the failure does not name the log level: %v", err)
	}
}

// TestTheBootstrapLoggerWritesToTheConsole covers the logger the startup runs
// on before it has read anything. Where the logs belong is a setting, and the
// settings are in the database, so this logger is what reports a database that
// cannot be opened.
func TestTheBootstrapLoggerWritesToTheConsole(t *testing.T) {
	output := withStdoutCaptured(t, func() {
		logger, _ := newBootstrapLogger()
		logger.Info("a line from before the settings were read")
	})

	if !strings.Contains(output, "a line from before the settings were read") {
		t.Fatalf("the bootstrap logger wrote nothing to the console, stdout: %q", output)
	}
}

// TestTheLoggersHandedOutBeforeTheSwapFollowIt is what the switch exists for.
// The database handle is built with the bootstrap logger, so a second logger
// made once the settings are read would leave gorm writing to the console for
// the life of the process, and gorm is what reports a query that fails.
func TestTheLoggersHandedOutBeforeTheSwapFollowIt(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "logs", "tunnel-manager.log")

	var buildErr error

	withStdoutCaptured(t, func() {
		logger, swap := newBootstrapLogger()

		// A child made before the swap, which is what NewDatabase is handed.
		child := logger.Named("gorm").With(zap.String("component", "gorm"))

		logger.Info("a line from before the swap")

		core, _, _, err := initLoggerForTest(t, newLoggingSettings(logFile), "")
		if err != nil {
			buildErr = err
			return
		}

		swap.set(core)

		logger.Info("a line from after the swap")
		child.Info("a line from the child that was handed out before the swap")
	})

	if buildErr != nil {
		t.Fatalf("failed to build the core the settings describe: %v", buildErr)
	}

	body, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read the log file: %v", err)
	}

	if strings.Contains(string(body), "a line from before the swap") {
		t.Fatalf("a line written before the swap reached the log file, content: %q", string(body))
	}
	if !strings.Contains(string(body), "a line from after the swap") {
		t.Fatalf("the logger did not follow the swap into the log file, content: %q", string(body))
	}
	if !strings.Contains(string(body), "a line from the child that was handed out before the swap") {
		t.Fatalf("a child logger made before the swap did not follow it, content: %q", string(body))
	}
	if !strings.Contains(string(body), "\"component\":\"gorm\"") {
		t.Fatalf("the fields of the child logger were lost in the swap, content: %q", string(body))
	}
}

// TestTheLevelHandleChangesWhatIsWrittenWithoutARestart pins the handle the
// Settings screen changes the log level through. The level is held in an
// AtomicLevel rather than fixed into the core, which is what lets that one
// setting take hold while the process runs.
func TestTheLevelHandleChangesWhatIsWrittenWithoutARestart(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "logs", "tunnel-manager.log")

	core, level, _, err := initLoggerForTest(t, newLoggingSettings(logFile), "")
	if err != nil {
		t.Fatalf("failed to build the core: %v", err)
	}

	logger := zap.New(core)

	logger.Debug("a debug line written while the level is info")

	level.SetLevel(zapcore.DebugLevel)

	logger.Debug("a debug line written after the level was lowered")

	body, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read the log file: %v", err)
	}

	if strings.Contains(string(body), "a debug line written while the level is info") {
		t.Fatalf("a debug line was written while the level was info, content: %q", string(body))
	}
	if !strings.Contains(string(body), "a debug line written after the level was lowered") {
		t.Fatalf("the level handle did not take hold, content: %q", string(body))
	}
}

// The encryption key check reads the Hosts through gorm, which is served here
// by a fake driver that answers every query with the stored passwords. Only the
// id and the password are handed back, because that is all the check reads.

type hostRows struct {
	hosts []models.Host
	next  int
}

func (r *hostRows) Columns() []string { return []string{"id", "password"} }

func (r *hostRows) Close() error { return nil }

func (r *hostRows) Next(dest []driver.Value) error {
	if r.next >= len(r.hosts) {
		return io.EOF
	}

	host := r.hosts[r.next]
	r.next++

	dest[0] = int64(host.ID)
	dest[1] = host.Password

	return nil
}

type hostConn struct {
	hosts   []models.Host
	readErr error
}

func (c *hostConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("the fake driver does not prepare statements")
}

func (c *hostConn) Close() error { return nil }

func (c *hostConn) Begin() (driver.Tx, error) { return fakeTx{}, nil }

func (c *hostConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return fakeResult{}, nil
}

func (c *hostConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if query == versionQuery {
		return &fakeRows{columns: []string{"version"}, values: []driver.Value{sqliteVersion}}, nil
	}
	if c.readErr != nil {
		return nil, c.readErr
	}
	return &hostRows{hosts: c.hosts}, nil
}

type hostConnector struct {
	hosts   []models.Host
	readErr error
}

func (c *hostConnector) Connect(context.Context) (driver.Conn, error) {
	return &hostConn{hosts: c.hosts, readErr: c.readErr}, nil
}

func (c *hostConnector) Driver() driver.Driver { return fakeDriver{} }

// newHostDB returns a gorm handle whose Host table holds the given rows, or
// fails to be read when readErr is set.
func newHostDB(t *testing.T, hosts []models.Host, readErr error) *gorm.DB {
	t.Helper()

	sqlDB := sql.OpenDB(&hostConnector{hosts: hosts, readErr: readErr})

	t.Cleanup(func() {
		_ = sqlDB.Close()
	})

	db, err := gorm.Open(sqlite.Dialector{Conn: sqlDB}, &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("failed to open the database handle: %v", err)
	}

	return db
}

// runCheckEncryptionKey runs the check and reports whether it stopped the
// startup, along with everything it logged. logger.Fatal ends the process, so
// the logger is built with a hook that panics instead, which is what the
// recover below catches.
func runCheckEncryptionKey(t *testing.T, db *gorm.DB, cipher *crypto.Cipher, keyFile string) (bool, *observer.ObservedLogs) {
	t.Helper()

	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core, zap.WithFatalHook(zapcore.WriteThenPanic))

	stopped := false

	func() {
		defer func() {
			if recover() != nil {
				stopped = true
			}
		}()
		checkEncryptionKey(db, cipher, logger, keyFile)
	}()

	return stopped, logs
}

// loggedAtLeast reports whether an entry of that level carries the text.
func loggedAtLeast(logs *observer.ObservedLogs, level zapcore.Level, text string) bool {
	for _, entry := range logs.All() {
		if entry.Level == level && strings.Contains(entry.Message, text) {
			return true
		}
	}
	return false
}

// TestCheckEncryptionKeyStopsTheStartupWhenAMarkedPasswordDoesNotOpen pins the
// case the check exists for. A process holds one key, so a key that opens none
// of the sealed passwords builds no tunnel at all, and serving an API that
// looks healthy over it hides the reason.
func TestCheckEncryptionKeyStopsTheStartupWhenAMarkedPasswordDoesNotOpen(t *testing.T) {
	stored := newTestCipher(t)
	inUse := newTestCipher(t)

	hosts := []models.Host{
		{ID: 1, Password: encryptedWith(t, stored, "test-password")},
		{ID: 2, Password: encryptedWith(t, stored, "test-password-2")},
	}

	stopped, logs := runCheckEncryptionKey(t, newHostDB(t, hosts, nil), inUse, "/keys/tunnel-manager.key")

	if !stopped {
		t.Fatal("the startup went on although the key opens none of the sealed passwords")
	}
	if !loggedAtLeast(logs, zapcore.FatalLevel, "opens none of the stored passwords") {
		t.Fatalf("the reason the startup stopped was not logged, logged: %v", logs.AllUntimed())
	}
}

// TestAPlaintextPasswordThatLooksEncodedDoesNotBlockTheStartup guards the
// regression eaafaf9 fixed. A password stored before passwords were encrypted
// carries no marker, and one that happens to be spelled in base64 characters
// and is long enough is reported as one that does not open. Treating that as
// proof of a wrong key kept a deployment that holds nothing but plaintext from
// starting, with no API left to set the passwords again.
func TestAPlaintextPasswordThatLooksEncodedDoesNotBlockTheStartup(t *testing.T) {
	inUse := newTestCipher(t)

	hosts := []models.Host{
		{ID: 1, Password: "test-password"},                            // hook:allow
		{ID: 2, Password: "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0"}, // hook:allow
		{ID: 3, Password: "test-password-3"},                          // hook:allow
	}

	stopped, logs := runCheckEncryptionKey(t, newHostDB(t, hosts, nil), inUse, "/keys/tunnel-manager.key")

	if stopped {
		t.Fatalf("the startup was stopped although no stored password carries the marker, logged: %v",
			logs.AllUntimed())
	}

	// The row is still reported, because no tunnel is built for it until its
	// password is set again.
	if !loggedAtLeast(logs, zapcore.WarnLevel, "does not open with the encryption key in use") {
		t.Fatalf("the Host whose password does not open was not reported, logged: %v", logs.AllUntimed())
	}
}

// TestCheckEncryptionKeyGoesOnWhenOneStoredPasswordOpens pins that a single row
// that does not open is a fault of that row. The other Hosts are served, and
// the one that is left behind is named so that its password can be set again.
func TestCheckEncryptionKeyGoesOnWhenOneStoredPasswordOpens(t *testing.T) {
	inUse := newTestCipher(t)
	other := newTestCipher(t)

	hosts := []models.Host{
		{ID: 1, Password: encryptedWith(t, inUse, "test-password")},
		{ID: 2, Password: encryptedWith(t, other, "test-password-2")},
	}

	stopped, logs := runCheckEncryptionKey(t, newHostDB(t, hosts, nil), inUse, "/keys/tunnel-manager.key")

	if stopped {
		t.Fatalf("the startup was stopped although a stored password opens, logged: %v", logs.AllUntimed())
	}
	if !loggedAtLeast(logs, zapcore.WarnLevel, "does not open with the encryption key in use") {
		t.Fatalf("the Host whose password does not open was not reported, logged: %v", logs.AllUntimed())
	}
	if !loggedAtLeast(logs, zapcore.InfoLevel, "the encryption key opens the stored passwords") {
		t.Fatalf("the key that opens a stored password was not reported as such, logged: %v", logs.AllUntimed())
	}

	for _, entry := range logs.All() {
		for key, value := range entry.ContextMap() {
			if key == "host_ids_that_do_not_open" && fmt.Sprintf("%v", value) != "[2]" {
				t.Fatalf("the Hosts that do not open were logged as %v, want [2]", value)
			}
		}
	}
}

func TestCheckEncryptionKeyHasNothingToCheckWhenNoPasswordIsEncrypted(t *testing.T) {
	inUse := newTestCipher(t)

	hosts := []models.Host{
		{ID: 1, Password: "test-password"},   // hook:allow
		{ID: 2, Password: "test-password-2"}, // hook:allow
	}

	stopped, logs := runCheckEncryptionKey(t, newHostDB(t, hosts, nil), inUse, "/keys/tunnel-manager.key")

	if stopped {
		t.Fatalf("the startup was stopped although no stored password is encrypted, logged: %v", logs.AllUntimed())
	}
	if !loggedAtLeast(logs, zapcore.InfoLevel, "no stored password is encrypted") {
		t.Fatalf("a deployment that holds no encrypted password was not reported as such, logged: %v",
			logs.AllUntimed())
	}
}

// TestCheckEncryptionKeyGoesOnWhenTheHostsCannotBeRead pins that a read which
// fails is not taken as a wrong key. The read says nothing about the key, and
// stopping here would turn a database hiccup into a startup that never comes up.
func TestCheckEncryptionKeyGoesOnWhenTheHostsCannotBeRead(t *testing.T) {
	inUse := newTestCipher(t)
	db := newHostDB(t, nil, fmt.Errorf("the Host table cannot be read"))

	stopped, logs := runCheckEncryptionKey(t, db, inUse, "/keys/tunnel-manager.key")

	if stopped {
		t.Fatalf("the startup was stopped over a read that failed, logged: %v", logs.AllUntimed())
	}
	if !loggedAtLeast(logs, zapcore.WarnLevel, "failed to read the Hosts") {
		t.Fatalf("the read that failed was not reported, logged: %v", logs.AllUntimed())
	}
}

func TestTheValidatorAcceptsARequestThatIsFilledIn(t *testing.T) {
	cv := &CustomValidator{validator: validator.New()}

	err := cv.Validate(&models.CreateHostRequest{
		Address:  "192.0.2.10",
		Port:     22,
		User:     "tester",
		Password: "test-password", // hook:allow
	})
	if err != nil {
		t.Fatalf("a request that is filled in was refused: %v", err)
	}
}

// TestTheValidatorTakesAHostNameOrAnAddress holds every field that names where
// to connect to the one rule: a host name or an address, either family, and
// nothing else. A name is resolved when it is dialled, so the rule is about
// its shape only.
func TestTheValidatorTakesAHostNameOrAnAddress(t *testing.T) {
	cv := &CustomValidator{validator: validator.New()}

	requests := map[string]func(value string) interface{}{
		"address of a new Host": func(value string) interface{} {
			return &models.CreateHostRequest{Address: value, Port: 22, User: "tester"}
		},
		"service_address": func(value string) interface{} {
			return &models.CreateServicePortRequest{ServiceAddress: value, ServicePort: 80, LocalPort: 8080}
		},
		"target_address": func(value string) interface{} {
			return &models.LocalForwardRequest{LocalPort: 15432, TargetAddress: value, TargetPort: 5432}
		},
		"address of a changed Host": func(value string) interface{} {
			return &models.UpdateHostRequest{Address: value}
		},
	}

	for name, request := range requests {
		for _, value := range []string{"db.example.com", "localhost", "192.0.2.1", "::1", "2001:db8::10"} {
			err := cv.Validate(request(value))
			if err != nil {
				t.Errorf("%s %q was refused: %v", name, value, err)
			}
		}

		for _, value := range []string{"bad host!", "-leading.example", "db_example.com", "a b"} {
			err := cv.Validate(request(value))
			if err == nil {
				t.Errorf("%s %q was accepted", name, value)
			}
		}
	}

	for name, request := range requests {
		err := cv.Validate(request(""))
		if name == "address of a changed Host" {
			if err != nil {
				t.Errorf("a change that leaves the address out was refused: %v", err)
			}

			continue
		}

		if err == nil {
			t.Errorf("%s left empty was accepted", name)
		}
	}
}

// TestTheValidatorNamesTheFieldAsTheApiSpellsIt pins both halves of the
// validator: a request that breaks the rules is refused, and the field is named
// by its json name, which is the only name the caller ever sent.
func TestTheValidatorNamesTheFieldAsTheApiSpellsIt(t *testing.T) {
	cv := &CustomValidator{validator: validator.New()}

	err := cv.Validate(&models.CreateServicePortRequest{
		ServiceAddress: "not an address",
		ServicePort:    22,
		LocalPort:      70000,
	})
	if err == nil {
		t.Fatal("a request with an address that is not one and a port out of range was accepted")
	}

	if !strings.Contains(err.Error(), "service_address") {
		t.Fatalf("the field is not named as the api spells it: %v", err)
	}
	if strings.Contains(err.Error(), "ServiceAddress") {
		t.Fatalf("the field is named by its Go name, which no caller ever sent: %v", err)
	}
	if !strings.Contains(err.Error(), "local_port") {
		t.Fatalf("the port out of range was not reported: %v", err)
	}
}

// TestTheValidatorFallsBackToTheGoNameWhenThereIsNoJsonName pins that a field
// which is kept out of the json body is still named in the failure. A field
// reported as an empty name would leave the caller with a rule broken and no
// field to look at.
func TestTheValidatorFallsBackToTheGoNameWhenThereIsNoJsonName(t *testing.T) {
	cv := &CustomValidator{validator: validator.New()}

	request := struct {
		Hidden  string `json:"-" validate:"required"`
		Unnamed string `validate:"required"`
	}{}

	err := cv.Validate(&request)
	if err == nil {
		t.Fatal("a request with two empty required fields was accepted")
	}
	if !strings.Contains(err.Error(), "Hidden") {
		t.Fatalf("the field that carries no json name was not named: %v", err)
	}
	if !strings.Contains(err.Error(), "Unnamed") {
		t.Fatalf("the field that carries no json tag at all was not named: %v", err)
	}
}

// TestARelativePathIsReadAgainstTheDatabaseDirectory pins where a stored
// relative path lands. The working directory is not the same twice: systemd
// leaves it at /, the container image sets it to /, and a person running the
// binary is wherever they happened to be. Reading these against it is what put
// the encryption key in the root of the filesystem in an earlier release.
func TestARelativePathIsReadAgainstTheDatabaseDirectory(t *testing.T) {
	installDir := t.TempDir()

	cases := []struct {
		name  string
		given string
		want  string
	}{
		{"the default log path", "logs/tunnel-manager.log", filepath.Join(installDir, "logs", "tunnel-manager.log")},
		{"the default key path", "keys/tunnel-manager.key", filepath.Join(installDir, "keys", "tunnel-manager.key")},
		{"a bare file name", "x.log", filepath.Join(installDir, "x.log")},
		{"an absolute path is left alone", platformAbsolutePath("/var/log/tunnel-manager/x.log"),
			platformAbsolutePath("/var/log/tunnel-manager/x.log")},
		{"an empty path stays empty", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveInstallPath(installDir, tc.given)
			if got != tc.want {
				t.Errorf("resolveInstallPath(%q, %q) = %q, want %q", installDir, tc.given, got, tc.want)
			}
		})
	}
}

// TestTheLogFileIsWrittenBesideTheDatabase runs the path through the logger
// rather than through the helper alone, so that a caller that forgot to resolve
// is caught as well.
func TestTheLogFileIsWrittenBesideTheDatabase(t *testing.T) {
	installDir := t.TempDir()

	set := newLoggingSettings("logs/tunnel-manager.log")

	core, _, _, err := initLoggerForTest(t, set, installDir)
	if err != nil {
		t.Fatalf("failed to build the logger: %v", err)
	}

	zap.New(core).Info("a line that belongs beside the database")

	written := filepath.Join(installDir, "logs", "tunnel-manager.log")

	body, err := os.ReadFile(written)
	if err != nil {
		t.Fatalf("the log file was not written beside the database: %v", err)
	}
	if !strings.Contains(string(body), "a line that belongs beside the database") {
		t.Fatalf("the entry did not reach %s, its content is %q", written, string(body))
	}
}

// newSettingsDB returns a handle on an empty database file that holds the
// settings table. A real file is used rather than the fake driver the tests
// above build, because what the repair is about is the row that comes back out
// of the database after it was written.
func newSettingsDB(t *testing.T) *gorm.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "tunnel-manager.db")

	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	err = db.AutoMigrate(&settings.Settings{})
	if err != nil {
		t.Fatalf("failed to migrate the database: %v", err)
	}

	return db
}

// TestTheStartupPutsAStoredPathOutsideTheInstallationBackToItsDefault is the
// installation that is upgraded. It stored an absolute log file while that was
// accepted, and the startup now reads the settings under a rule that refuses
// it. It comes up on the default and says so, rather than stopping at a
// setting whose screen is served by the server that would not be running.
func TestTheStartupPutsAStoredPathOutsideTheInstallationBackToItsDefault(t *testing.T) {
	db := newSettingsDB(t)

	// The row a first startup writes, which is then made into the row the
	// older version left behind.
	_, err := settings.Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	err = db.Model(&settings.Settings{}).Where("id = ?", 1).
		Updates(map[string]interface{}{"logging_file_path": platformAbsolutePath("/etc/cron.d/x")}).Error
	if err != nil {
		t.Fatalf("storing an absolute log path by hand: %v", err)
	}

	core, logs := observer.New(zapcore.DebugLevel)

	repairStoredPaths(db, zap.New(core))

	if !loggedAtLeast(logs, zapcore.WarnLevel, "was put back to its default") {
		t.Errorf("the repair was not reported as a warning, what was logged is %v", logs.All())
	}

	replaced := false

	for _, entry := range logs.All() {
		fields := entry.ContextMap()
		if fields["setting"] != "logging.file.path" {
			continue
		}
		replaced = true

		if fields["from"] != platformAbsolutePath("/etc/cron.d/x") {
			t.Errorf("the line says the path came from %v, want %s", fields["from"], platformAbsolutePath("/etc/cron.d/x"))
		}
		if fields["to"] != settings.Defaults().LoggingFilePath {
			t.Errorf("the line says the path went to %v, want %q",
				fields["to"], settings.Defaults().LoggingFilePath)
		}
	}

	if !replaced {
		t.Errorf("no line named the log file, what was logged is %v", logs.All())
	}

	// The read the startup makes next is the one that would have stopped it.
	set, err := settings.Load(db)
	if err != nil {
		t.Fatalf("Load after the repair: %v", err)
	}
	if set.LoggingFilePath != settings.Defaults().LoggingFilePath {
		t.Fatalf("the startup runs on the log path %q, want the default %q",
			set.LoggingFilePath, settings.Defaults().LoggingFilePath)
	}
}

// TestTheStartupSaysNothingAboutPathsItDoesNotRepair covers every installation
// but the upgraded one. Nothing is refused, so nothing is written and the log
// of a normal startup carries no line about it.
func TestTheStartupSaysNothingAboutPathsItDoesNotRepair(t *testing.T) {
	db := newSettingsDB(t)

	_, err := settings.Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	core, logs := observer.New(zapcore.DebugLevel)

	repairStoredPaths(db, zap.New(core))

	if logs.Len() != 0 {
		t.Fatalf("a startup with nothing to repair logged %v", logs.All())
	}
}

// proxyTrustRecorder stands in for the authentication handler and remembers
// what it was told, since the handler itself keeps the answer to itself.
type proxyTrustRecorder struct {
	calls []bool
}

func (r *proxyTrustRecorder) TrustProxyHeaders(trust bool) {
	r.calls = append(r.calls, trust)
}

// TestTheProxyHeadersAreTrustedOnlyWhenTheFlagIsGiven pins both halves of the
// flag. The header it turns on is one any client can send, so a deployment
// that has nothing in front of it has to be left exactly as it was: the
// handler is not called at all, and off stays the state it was built in.
func TestTheProxyHeadersAreTrustedOnlyWhenTheFlagIsGiven(t *testing.T) {
	cases := []struct {
		name string
		flag bool
		want []bool
	}{
		{"the flag was left out", false, nil},
		{"the flag was given", true, []bool{true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &proxyTrustRecorder{}

			applyProxyTrust(recorder, tc.flag)

			if len(recorder.calls) != len(tc.want) {
				t.Fatalf("the handler was told %v, want %v", recorder.calls, tc.want)
			}
			for i, call := range recorder.calls {
				if call != tc.want[i] {
					t.Errorf("call %d was %v, want %v", i, call, tc.want[i])
				}
			}
		})
	}
}

// readBackHandler is what the routes of the hardened server below answer with:
// the whole body, read to its end, counted. Nothing here is the real handler,
// because what is under test is the middleware in front of it, and the count is
// how a body that arrived whole is told from one the limit cut short.
func readBackHandler(c echo.Context) error {
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, map[string]int{"read": len(body)})
}

// newHardenedServer builds the server main builds, with the routes the tests
// below need registered the way main registers them: the two imports with the
// body limit of their own, everything else on the general one.
//
// harden is called rather than copied, so a test that passes is a test of the
// wiring that is served.
func newHardenedServer(servedCertificate func() *tls.Certificate) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	harden(e, servedCertificate)

	g := e.Group(apiPrefix)

	g.POST("/login", readBackHandler)
	g.GET("/status", readBackHandler)
	g.PUT("/certificate", readBackHandler)
	g.POST(importTunnelsPath, readBackHandler, importBodyLimitMiddleware())
	g.POST(importSettingsPath, readBackHandler, importBodyLimitMiddleware())

	// The UI is registered as main registers it, because the policy the
	// headers carry is written for the page these routes serve and the page
	// has to be served through them to be covered by it.
	web.RegisterRoutes(e, "test")

	return e
}

// noCertificate is what the holder answers with while HTTPS is off.
func noCertificate() *tls.Certificate { return nil }

// postJSON sends a body of the given size to a path and hands back what came
// out. The body is valid JSON, so nothing is refused for its shape, and the
// padding sits in a string field the way the file of an import does.
func postJSON(e *echo.Echo, method string, path string, size int) *httptest.ResponseRecorder {
	body := `{"file":"` + strings.Repeat("a", size) + `"}`

	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	return recorder
}

// TestABodyOverTheGeneralLimitIsRefused pins the limit on the route anybody who
// can reach the port reaches: POST /api/login takes no session, and without a
// limit what it costs this process is decided by whoever is sending.
func TestABodyOverTheGeneralLimitIsRefused(t *testing.T) {
	e := newHardenedServer(noCertificate)

	recorder := postJSON(e, http.MethodPost, apiPrefix+"/login", 2*1024*1024)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a 2MB body to /api/login was answered %d, want %d",
			recorder.Code, http.StatusRequestEntityTooLarge)
	}
}

// TestABodyOverTheGeneralLimitIsRefusedWithoutAContentLength covers the other
// half of the check. A client that says how much it is sending is refused on
// the number it gave; one that sends without saying, which is what a chunked
// request is, has to be refused on what it actually sent.
func TestABodyOverTheGeneralLimitIsRefusedWithoutAContentLength(t *testing.T) {
	e := newHardenedServer(noCertificate)

	body := `{"file":"` + strings.Repeat("a", 2*1024*1024) + `"}`

	// io.NopCloser hides the reader httptest.NewRequest would otherwise take a
	// length from, which is what leaves ContentLength at -1.
	request := httptest.NewRequest(http.MethodPost, apiPrefix+"/login", io.NopCloser(strings.NewReader(body)))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	if request.ContentLength >= 0 {
		t.Fatalf("the request carries a content length of %d, so it is not the case this test is for",
			request.ContentLength)
	}

	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a 2MB body sent without a content length was answered %d, want %d",
			recorder.Code, http.StatusRequestEntityTooLarge)
	}
}

// TestARequestUnderTheGeneralLimitIsTakenWhole says the limit is a limit and
// not a truncation: the handler is reached and what it reads is everything that
// was sent.
func TestARequestUnderTheGeneralLimitIsTakenWhole(t *testing.T) {
	const padding = 512 * 1024

	e := newHardenedServer(noCertificate)

	recorder := postJSON(e, http.MethodPost, apiPrefix+"/login", padding)

	if recorder.Code != http.StatusOK {
		t.Fatalf("a body under the limit was answered %d, want %d: %s",
			recorder.Code, http.StatusOK, recorder.Body.String())
	}

	want := len(`{"file":"`) + padding + len(`"}`)
	if got := readCount(t, recorder); got != want {
		t.Fatalf("the handler read %d bytes of a %d byte body", got, want)
	}
}

// TestAnImportCarriesABodyTheGeneralLimitWouldRefuse is the point of the two
// limits. An exported configuration is as large as the installation that wrote
// it, so the imports are measured against importBodyLimit while everything else
// stays on the general one.
func TestAnImportCarriesABodyTheGeneralLimitWouldRefuse(t *testing.T) {
	const padding = 8 * 1024 * 1024

	e := newHardenedServer(noCertificate)

	for _, path := range []string{apiPrefix + importTunnelsPath, apiPrefix + importSettingsPath} {
		t.Run(path, func(t *testing.T) {
			recorder := postJSON(e, http.MethodPost, path, padding)

			if recorder.Code != http.StatusOK {
				t.Fatalf("an 8MB import was answered %d, want %d", recorder.Code, http.StatusOK)
			}

			want := len(`{"file":"`) + padding + len(`"}`)
			if got := readCount(t, recorder); got != want {
				t.Fatalf("the import read %d bytes of an %d byte body", got, want)
			}
		})
	}
}

// TestTheSameBodyIsRefusedOnARouteThatIsNotAnImport holds the pair up against
// each other, so that a skipper which stopped matching shows here rather than
// as a login that is suddenly allowed to send eight megabytes.
func TestTheSameBodyIsRefusedOnARouteThatIsNotAnImport(t *testing.T) {
	e := newHardenedServer(noCertificate)

	recorder := postJSON(e, http.MethodPost, apiPrefix+"/login", 8*1024*1024)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an 8MB body to /api/login was answered %d, want %d",
			recorder.Code, http.StatusRequestEntityTooLarge)
	}
}

// TestACertificateRegistrationStillFitsTheGeneralLimit sends what the Settings
// screen sends: a certificate and the PEM private key that goes with it. It is
// the largest body any route but the imports carries, and it is nowhere near
// the limit.
func TestACertificateRegistrationStillFitsTheGeneralLimit(t *testing.T) {
	e := newHardenedServer(noCertificate)

	_, certPEM, keyPEM := issuedCertificate(t)

	body, err := json.Marshal(map[string]string{"cert_pem": certPEM, "key_pem": keyPEM})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	request := httptest.NewRequest(http.MethodPut, apiPrefix+"/certificate", strings.NewReader(string(body)))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("a certificate registration was answered %d, want %d: %s",
			recorder.Code, http.StatusOK, recorder.Body.String())
	}

	if got := readCount(t, recorder); got != len(body) {
		t.Fatalf("the handler read %d bytes of a %d byte registration", got, len(body))
	}
}

// readCount is the number readBackHandler answered with.
func readCount(t *testing.T, recorder *httptest.ResponseRecorder) int {
	t.Helper()

	var answer struct {
		Read int `json:"read"`
	}

	err := json.Unmarshal(recorder.Body.Bytes(), &answer)
	if err != nil {
		t.Fatalf("the answer %q is not what the handler writes: %v", recorder.Body.String(), err)
	}

	return answer.Read
}

// TestBothServersCarryTheDeadlines pins the timeouts onto the two http.Servers
// echo holds. Which of them is started is decided by api.https_enabled several
// hundred lines into main, so a value put on one of them only is a deployment
// that is covered and another that is not.
func TestBothServersCarryTheDeadlines(t *testing.T) {
	e := echo.New()

	harden(e, noCertificate)

	servers := map[string]*http.Server{
		"the plaintext server": e.Server,
		"the TLS server":       e.TLSServer,
	}

	for name, server := range servers {
		t.Run(name, func(t *testing.T) {
			if server.ReadHeaderTimeout != apiReadHeaderTimeout {
				t.Errorf("ReadHeaderTimeout = %v, want %v", server.ReadHeaderTimeout, apiReadHeaderTimeout)
			}
			if server.ReadTimeout != apiReadTimeout {
				t.Errorf("ReadTimeout = %v, want %v", server.ReadTimeout, apiReadTimeout)
			}
			if server.IdleTimeout != apiIdleTimeout {
				t.Errorf("IdleTimeout = %v, want %v", server.IdleTimeout, apiIdleTimeout)
			}

			// No WriteTimeout, and it is checked here so that one added
			// without reading why there is none fails as a test rather than
			// as an import that is cut off half way: see the note under the
			// timeouts in main.go.
			if server.WriteTimeout != 0 {
				t.Errorf("WriteTimeout = %v, want none", server.WriteTimeout)
			}
		})
	}
}

// TestEveryAnswerCarriesTheSecurityHeaders covers the answers a handler wrote
// and the one it never reached alike. The 413 is written by the body limit and
// leaves through the same response, so it has to carry them too: a refusal that
// a browser renders without them is still a page on this origin.
func TestEveryAnswerCarriesTheSecurityHeaders(t *testing.T) {
	e := newHardenedServer(noCertificate)

	cases := []struct {
		name string
		run  func(t *testing.T) *httptest.ResponseRecorder
	}{
		{"an answer a handler wrote", func(*testing.T) *httptest.ResponseRecorder {
			return postJSON(e, http.MethodPost, apiPrefix+"/login", 16)
		}},
		{"a body that was refused", func(*testing.T) *httptest.ResponseRecorder {
			return postJSON(e, http.MethodPost, apiPrefix+"/login", 2*1024*1024)
		}},
		{"a path that is not there", func(*testing.T) *httptest.ResponseRecorder {
			recorder := httptest.NewRecorder()
			e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/nothing-here", nil))

			return recorder
		}},
		{"the page itself", func(*testing.T) *httptest.ResponseRecorder {
			recorder := httptest.NewRecorder()
			e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ui/", nil))

			return recorder
		}},
		{"a page the browser already holds", func(t *testing.T) *httptest.ResponseRecorder {
			first := httptest.NewRecorder()
			e.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/ui/app.js", nil))

			request := httptest.NewRequest(http.MethodGet, "/ui/app.js", nil)
			request.Header.Set("If-None-Match", first.Header().Get("ETag"))

			recorder := httptest.NewRecorder()
			e.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusNotModified {
				t.Fatalf("the second request was answered %d, want %d",
					recorder.Code, http.StatusNotModified)
			}

			return recorder
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := tc.run(t)

			if got := recorder.Header().Get(echo.HeaderXFrameOptions); got != "DENY" {
				t.Errorf("X-Frame-Options = %q, want %q", got, "DENY")
			}
			if got := recorder.Header().Get(echo.HeaderXContentTypeOptions); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
			}
			if got := recorder.Header().Get(echo.HeaderContentSecurityPolicy); got == "" {
				t.Error("no Content-Security-Policy")
			}
		})
	}
}

// TestTheContentSecurityPolicyIsTheOneThePageNeeds reads the page itself and
// works out what the policy has to say for it, so that a policy and a page
// which drifted apart fail here rather than as a screen that comes up blank.
//
// The scripts are pulled out with a regular expression rather than with
// inlineScriptHashes, which is what the policy is built with: two readings of
// the same file that agree are worth something, one function checked against
// itself is worth nothing.
func TestTheContentSecurityPolicyIsTheOneThePageNeeds(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("internal", "web", "static", "index.html"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	policy := contentSecurityPolicy(page)

	scriptTag := regexp.MustCompile(`(?s)<script([^>]*)>(.*?)</script>`)

	inline := 0

	for _, match := range scriptTag.FindAllStringSubmatch(string(page), -1) {
		if strings.Contains(match[1], "src") {
			continue
		}

		inline++

		sum := sha256.Sum256([]byte(match[2]))
		want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"

		if !strings.Contains(policy, want) {
			t.Errorf("the policy does not name the inline script whose hash is %s:\n%s", want, policy)
		}
	}

	if inline == 0 {
		t.Fatal("no inline script was found in the page, so this test checked nothing")
	}

	if got := strings.Count(policy, "'sha256-"); got != inline {
		t.Errorf("the policy names %d hashes for %d inline scripts:\n%s", got, inline, policy)
	}

	// What the page loads besides its own scripts, read off the files it is
	// made of. Anything the UI starts doing that is not here is refused by the
	// browser, which is the point of default-src 'none'.
	for _, directive := range []string{
		"default-src 'none'",
		"script-src 'self'",
		"style-src 'self'",
		"img-src 'self'",
		"connect-src 'self'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	} {
		if !strings.Contains(policy, directive) {
			t.Errorf("the policy is missing %q:\n%s", directive, policy)
		}
	}
}

// selfSignedCertificate is what a fresh installation serves: a certificate that
// signed for itself.
func selfSignedCertificate(t *testing.T) *tls.Certificate {
	t.Helper()

	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "tunnel-manager"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private, Leaf: leaf}
}

// issuedCertificate is what an operator registers: a certificate somebody else
// signed. The PEM of the certificate and of its private key come back with it,
// because that is what the Settings screen sends.
func issuedCertificate(t *testing.T) (*tls.Certificate, string, string) {
	t.Helper()

	issuerPublic, issuerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	issuerTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "a certificate authority"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	issuerDER, err := x509.CreateCertificate(rand.Reader, issuerTemplate, issuerTemplate, issuerPublic, issuerPrivate)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}

	issuer, err := x509.ParseCertificate(issuerDER)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "tunnel-manager"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"tunnel-manager.example"},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, issuer, public, issuerPrivate)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}

	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))

	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private, Leaf: leaf}, certPEM, keyPEM
}

// TestHSTSIsSentOnlyForACertificateSomebodyElseSigned is the whole of the
// caution around this header. A browser that was told to stay on HTTPS refuses
// to let anybody past the warning a self-signed certificate raises, and the
// screens are then unreachable with nothing on them to undo it.
func TestHSTSIsSentOnlyForACertificateSomebodyElseSigned(t *testing.T) {
	issued, _, _ := issuedCertificate(t)

	cases := []struct {
		name        string
		tls         bool
		certificate *tls.Certificate
		want        bool
	}{
		{"HTTPS is turned off", false, nil, false},
		{"a self-signed certificate", true, selfSignedCertificate(t), false},
		{"a certificate somebody else signed", true, issued, true},
		{"a certificate over a plaintext request", false, issued, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newHardenedServer(func() *tls.Certificate { return tc.certificate })

			request := httptest.NewRequest(http.MethodGet, apiPrefix+"/status", nil)
			if tc.tls {
				request.TLS = &tls.ConnectionState{}
			}

			recorder := httptest.NewRecorder()
			e.ServeHTTP(recorder, request)

			got := recorder.Header().Get(echo.HeaderStrictTransportSecurity)

			if tc.want && got != fmt.Sprintf("max-age=%d", hstsMaxAgeSeconds) {
				t.Fatalf("Strict-Transport-Security = %q, want max-age=%d", got, hstsMaxAgeSeconds)
			}

			if !tc.want && got != "" {
				t.Fatalf("Strict-Transport-Security = %q, want nothing", got)
			}
		})
	}
}

// TestTheHeaderFollowsACertificateThatIsReplaced pins that the decision is made
// per request. The certificate is replaced while the process runs, from the
// Settings screen, and a header worked out once at startup would keep saying
// what was true then.
func TestTheHeaderFollowsACertificateThatIsReplaced(t *testing.T) {
	issued, _, _ := issuedCertificate(t)
	serving := selfSignedCertificate(t)

	e := newHardenedServer(func() *tls.Certificate { return serving })

	ask := func() string {
		request := httptest.NewRequest(http.MethodGet, apiPrefix+"/status", nil)
		request.TLS = &tls.ConnectionState{}

		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, request)

		return recorder.Header().Get(echo.HeaderStrictTransportSecurity)
	}

	if got := ask(); got != "" {
		t.Fatalf("a self-signed certificate was answered with %q", got)
	}

	serving = issued

	if got := ask(); got == "" {
		t.Fatal("a registered certificate was answered with no Strict-Transport-Security")
	}
}

// TestACertificateThatWasNotParsedIsNotTakenAsIssued covers what is answered
// when the question cannot be. The header decides whether a browser refuses
// plaintext for the next month, so anything unreadable has to end as no header
// rather than as one sent on a guess.
func TestACertificateThatWasNotParsedIsNotTakenAsIssued(t *testing.T) {
	cases := []struct {
		name string
		pair *tls.Certificate
	}{
		{"nothing is being served", nil},
		{"a pair that holds no certificate", &tls.Certificate{}},
		{"a pair whose certificate does not parse", &tls.Certificate{Certificate: [][]byte{{1, 2, 3}}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if caIssuedCertificate(tc.pair) {
				t.Fatal("was taken for a certificate somebody else signed")
			}
		})
	}
}

// TestALeafThatWasNotParsedYetIsStillRead is the other half: tlsserve fills
// Leaf on everything it hands out, and a pair built some other way is parsed
// here rather than treated as unreadable.
func TestALeafThatWasNotParsedYetIsStillRead(t *testing.T) {
	issued, _, _ := issuedCertificate(t)
	selfSigned := selfSignedCertificate(t)

	issued.Leaf = nil
	selfSigned.Leaf = nil

	if !caIssuedCertificate(issued) {
		t.Error("a certificate somebody else signed was not read out of the pair")
	}

	if caIssuedCertificate(selfSigned) {
		t.Error("a self-signed certificate was read as one somebody else signed")
	}
}

// TestEmptyingTheLogLeavesTheWriterWritingToTheFile is the whole reason the
// emptying goes through the writer rather than at the file.
//
// The writer holds the file open across writes and carries the size it last
// wrote at. A file cut under it is still appended to correctly, because the
// handle is opened O_APPEND, but the size it is carrying is the size the file
// used to be: the next line of any length would be taken for one that fills the
// file and would rotate it on the spot. What the file holds after this is one
// line, and the directory holds one file.
func TestEmptyingTheLogLeavesTheWriterWritingToTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tunnel-manager.log")

	writer := &lumberjack.Logger{
		Filename: path,
		// MaxSize is in megabytes and the smallest lumberjack takes is 1. The
		// lines below are a few dozen bytes, so nothing here rotates by size
		// unless the size the writer is carrying is wrong.
		MaxSize:    1,
		MaxBackups: 3,
	}

	t.Cleanup(func() {
		_ = writer.Close()
	})

	_, err := writer.Write([]byte("a line from before the emptying\n"))
	if err != nil {
		t.Fatalf("failed to write the first line: %v", err)
	}

	err = emptyLogFile(writer, path)
	if err != nil {
		t.Fatalf("failed to empty the log: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to stat the emptied log: %v", err)
	}

	if info.Size() != 0 {
		t.Fatalf("the emptied log is %d bytes, want 0", info.Size())
	}

	_, err = writer.Write([]byte("a line from after the emptying\n"))
	if err != nil {
		t.Fatalf("failed to write after the emptying: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read the log back: %v", err)
	}

	// A hole of NUL bytes is what a writer that kept its offset would leave in
	// front of the line, and the line arriving after nothing at all is what
	// says the append went to the beginning of the file.
	if string(body) != "a line from after the emptying\n" {
		t.Errorf("the log reads %q, want only the line written after the emptying", string(body))
	}

	// One rotated file here would be the stale size rotating the file on the
	// first line after the emptying.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("failed to read the directory: %v", err)
	}

	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}

		t.Errorf("the directory holds %v, want the log file alone", names)
	}
}

// TestEmptyingTheLogSaysWhenTheFileIsGone is the failure the handler turns into
// an answer. Nothing is silently taken for done.
func TestEmptyingTheLogSaysWhenTheFileIsGone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tunnel-manager.log")

	writer := &lumberjack.Logger{Filename: path, MaxSize: 1}

	err := emptyLogFile(writer, filepath.Join(dir, "not-there", "tunnel-manager.log"))
	if err == nil {
		t.Error("emptying a file that is not there was reported as done")
	}
}
