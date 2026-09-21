package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// newTraceLogger returns a gorm logger at level together with the record of
// everything it writes, so a call can be judged by its output alone. The
// observer is opened at Debug so that no entry is dropped before it is seen.
func newTraceLogger(level gormlogger.LogLevel) (*zapGormLogger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)

	return &zapGormLogger{logger: zap.New(core), level: level}, logs
}

// statement stands in for the callback gorm hands to Trace.
func statement(sql string, rows int64) func() (string, int64) {
	return func() (string, int64) {
		return sql, rows
	}
}

// onlyEntry fails unless exactly one line was logged, since a branch that fires
// twice or not at all is as wrong as one that logs the wrong thing.
func onlyEntry(t *testing.T, logs *observer.ObservedLogs) observer.LoggedEntry {
	t.Helper()

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("expected one log entry, got %d: %v", len(entries), entries)
	}

	return entries[0]
}

func requireNothingLogged(t *testing.T, logs *observer.ObservedLogs) {
	t.Helper()

	if entries := logs.All(); len(entries) != 0 {
		t.Fatalf("expected nothing to be logged, got %d entries: %v", len(entries), entries)
	}
}

func requireField(t *testing.T, entry observer.LoggedEntry, key string, want interface{}) {
	t.Helper()

	fields := entry.ContextMap()

	got, ok := fields[key]
	if !ok {
		t.Fatalf("the %q field is missing from %v", key, fields)
	}

	if got != want {
		t.Fatalf("the %q field is %v, want %v", key, got, want)
	}
}

// levelName keeps the subtest names readable, as the gorm levels are plain ints.
func levelName(level gormlogger.LogLevel) string {
	switch level {
	case gormlogger.Silent:
		return "silent"
	case gormlogger.Error:
		return "error"
	case gormlogger.Warn:
		return "warn"
	case gormlogger.Info:
		return "info"
	default:
		return fmt.Sprintf("level(%d)", level)
	}
}

// TestGormLogLevelKeepsStatementTracingToDebug pins the mapping down in both
// directions: only "debug" reaches Info, where gorm traces every statement, and
// anything unknown falls back to the quietest level rather than the loudest.
func TestGormLogLevelKeepsStatementTracingToDebug(t *testing.T) {
	cases := []struct {
		level string
		want  gormlogger.LogLevel
	}{
		{level: "debug", want: gormlogger.Info},
		{level: "info", want: gormlogger.Warn},
		{level: "warn", want: gormlogger.Warn},
		{level: "error", want: gormlogger.Error},
		{level: "fatal", want: gormlogger.Error},
		{level: "", want: gormlogger.Error},
		{level: "DEBUG", want: gormlogger.Error},
	}

	for _, test := range cases {
		t.Run(test.level, func(t *testing.T) {
			if got := gormLogLevel(test.level); got != test.want {
				t.Fatalf("gormLogLevel(%q) is %s, want %s", test.level, levelName(got), levelName(test.want))
			}
		})
	}
}

// TestLogModeLeavesTheOriginalLoggerUntouched holds the copy in LogMode. gorm
// calls LogMode per session, so a logger that changed itself would drag the
// level of one session into every other one.
func TestLogModeLeavesTheOriginalLoggerUntouched(t *testing.T) {
	original, logs := newTraceLogger(gormlogger.Silent)

	copied, ok := original.LogMode(gormlogger.Info).(*zapGormLogger)
	if !ok {
		t.Fatalf("LogMode did not return a *zapGormLogger")
	}

	if copied == original {
		t.Fatalf("LogMode returned the original logger instead of a copy")
	}

	if original.level != gormlogger.Silent {
		t.Fatalf("the original level became %s, want %s",
			levelName(original.level), levelName(gormlogger.Silent))
	}

	if copied.level != gormlogger.Info {
		t.Fatalf("the copied level is %s, want %s",
			levelName(copied.level), levelName(gormlogger.Info))
	}

	// The copy must still write where the original writes, and the original
	// must stay silent now that the copy is louder.
	copied.Trace(context.Background(), time.Now(), statement("SELECT 1", 1), nil)
	original.Trace(context.Background(), time.Now(), statement("SELECT 2", 1), nil)

	entry := onlyEntry(t, logs)
	requireField(t, entry, "sql", "SELECT 1")
}

// TestInfoWarnErrorAreDroppedBelowTheirLevel walks every level so that a
// comparison turned the wrong way shows up as a line that should not be there.
func TestInfoWarnErrorAreDroppedBelowTheirLevel(t *testing.T) {
	cases := []struct {
		level     gormlogger.LogLevel
		wantInfo  bool
		wantWarn  bool
		wantError bool
	}{
		{level: gormlogger.Silent},
		{level: gormlogger.Error, wantError: true},
		{level: gormlogger.Warn, wantWarn: true, wantError: true},
		{level: gormlogger.Info, wantInfo: true, wantWarn: true, wantError: true},
	}

	for _, test := range cases {
		t.Run(levelName(test.level), func(t *testing.T) {
			calls := []struct {
				name  string
				write func(logger *zapGormLogger)
				zap   zapcore.Level
				want  bool
			}{
				{
					name:  "info",
					write: func(l *zapGormLogger) { l.Info(context.Background(), "rows %d", 3) },
					zap:   zapcore.InfoLevel,
					want:  test.wantInfo,
				},
				{
					name:  "warn",
					write: func(l *zapGormLogger) { l.Warn(context.Background(), "rows %d", 3) },
					zap:   zapcore.WarnLevel,
					want:  test.wantWarn,
				},
				{
					name:  "error",
					write: func(l *zapGormLogger) { l.Error(context.Background(), "rows %d", 3) },
					zap:   zapcore.ErrorLevel,
					want:  test.wantError,
				},
			}

			for _, call := range calls {
				t.Run(call.name, func(t *testing.T) {
					logger, logs := newTraceLogger(test.level)
					call.write(logger)

					if !call.want {
						requireNothingLogged(t, logs)
						return
					}

					entry := onlyEntry(t, logs)
					if entry.Level != call.zap {
						t.Fatalf("the entry is at %s, want %s", entry.Level, call.zap)
					}

					// The arguments gorm passes belong in the message.
					if entry.Message != "rows 3" {
						t.Fatalf("the message is %q, want %q", entry.Message, "rows 3")
					}
				})
			}
		})
	}
}

// TestSilentTraceLogsNothingAndBuildsNoStatement covers the early return. The
// callback renders the SQL, so it must not be called either.
func TestSilentTraceLogsNothingAndBuildsNoStatement(t *testing.T) {
	logger, logs := newTraceLogger(gormlogger.Silent)

	built := false
	fc := func() (string, int64) {
		built = true
		return "SELECT 1", 1
	}

	// A failing and slow query, which every other level would report.
	logger.Trace(context.Background(), time.Now().Add(-time.Second), fc, errors.New("connection reset"))

	requireNothingLogged(t, logs)

	if built {
		t.Fatalf("the statement was rendered even though the logger is silent")
	}
}

// TestRecordNotFoundIsNotLoggedAsAFailedQuery is the reason the exception is
// there: "no rows" is how a lookup reports an absent row, so every such lookup
// would otherwise leave an error line behind.
func TestRecordNotFoundIsNotLoggedAsAFailedQuery(t *testing.T) {
	for _, level := range []gormlogger.LogLevel{gormlogger.Error, gormlogger.Warn, gormlogger.Info} {
		t.Run(levelName(level), func(t *testing.T) {
			logger, logs := newTraceLogger(level)

			logger.Trace(context.Background(), time.Now(),
				statement("SELECT * FROM `users` WHERE `username` = 'nobody'", 0),
				gormlogger.ErrRecordNotFound)

			for _, entry := range logs.All() {
				if entry.Level >= zapcore.ErrorLevel || entry.Message == "query failed" {
					t.Fatalf("a missing row was logged as %q at %s", entry.Message, entry.Level)
				}
			}
		})
	}
}

// TestWrappedRecordNotFoundIsNotLoggedAsAFailedQuery covers the same exception
// for an error that carries the sentinel further down the chain, which is what
// a comparison against the sentinel alone would miss.
func TestWrappedRecordNotFoundIsNotLoggedAsAFailedQuery(t *testing.T) {
	logger, logs := newTraceLogger(gormlogger.Warn)

	wrapped := fmt.Errorf("find user: %w", gormlogger.ErrRecordNotFound)

	logger.Trace(context.Background(), time.Now(), statement("SELECT 1", 0), wrapped)

	requireNothingLogged(t, logs)
}

// TestAFailedQueryIsLoggedWithTheStatement keeps the fields that make an error
// line worth reading.
func TestAFailedQueryIsLoggedWithTheStatement(t *testing.T) {
	logger, logs := newTraceLogger(gormlogger.Warn)

	failure := errors.New("connection reset by peer")

	logger.Trace(context.Background(), time.Now().Add(-50*time.Millisecond),
		statement("UPDATE `hosts` SET `enabled` = true", 2), failure)

	entry := onlyEntry(t, logs)

	if entry.Level != zapcore.ErrorLevel {
		t.Fatalf("the entry is at %s, want %s", entry.Level, zapcore.ErrorLevel)
	}

	if entry.Message != "query failed" {
		t.Fatalf("the message is %q, want %q", entry.Message, "query failed")
	}

	requireField(t, entry, "sql", "UPDATE `hosts` SET `enabled` = true")
	requireField(t, entry, "rows", int64(2))
	// zap keeps the error under its own key, rendered as its message.
	requireField(t, entry, "error", failure.Error())

	elapsed, ok := entry.ContextMap()["elapsed_ms"].(float64)
	if !ok {
		t.Fatalf("the elapsed_ms field is missing from %v", entry.ContextMap())
	}

	if elapsed < 50 {
		t.Fatalf("elapsed_ms is %v, want at least 50", elapsed)
	}
}

// TestAFailedQueryIsReportedBeforeItIsReportedAsSlow holds the order of the
// branches. A query that failed after a long wait has to be reported as a
// failure, since the failure is the part that has to be acted on.
func TestAFailedQueryIsReportedBeforeItIsReportedAsSlow(t *testing.T) {
	logger, logs := newTraceLogger(gormlogger.Info)

	logger.Trace(context.Background(), time.Now().Add(-5*slowQueryThreshold),
		statement("SELECT * FROM `tunnels`", 0), errors.New("connection reset by peer"))

	entry := onlyEntry(t, logs)

	if entry.Message != "query failed" {
		t.Fatalf("the message is %q, want %q", entry.Message, "query failed")
	}
}

// TestASlowQueryIsLoggedWithTheThresholdItPassed checks the warning and the
// threshold that goes with it, so the reader can tell what "slow" meant. The
// elapsed time comes from a begin in the past, so nothing here has to wait.
func TestASlowQueryIsLoggedWithTheThresholdItPassed(t *testing.T) {
	logger, logs := newTraceLogger(gormlogger.Warn)

	logger.Trace(context.Background(), time.Now().Add(slowQueryThreshold*-2),
		statement("SELECT * FROM `service_ports`", 7), nil)

	entry := onlyEntry(t, logs)

	if entry.Level != zapcore.WarnLevel {
		t.Fatalf("the entry is at %s, want %s", entry.Level, zapcore.WarnLevel)
	}

	if entry.Message != "slow query" {
		t.Fatalf("the message is %q, want %q", entry.Message, "slow query")
	}

	requireField(t, entry, "sql", "SELECT * FROM `service_ports`")
	requireField(t, entry, "rows", int64(7))
	requireField(t, entry, "slow_threshold_ms", float64(200))

	elapsed, ok := entry.ContextMap()["elapsed_ms"].(float64)
	if !ok {
		t.Fatalf("the elapsed_ms field is missing from %v", entry.ContextMap())
	}

	if elapsed <= 200 {
		t.Fatalf("elapsed_ms is %v, want more than the 200 threshold", elapsed)
	}
}

// TestAQueryUnderTheThresholdIsNotSlow guards the comparison itself, which a
// test that only feeds in slow queries would leave open.
func TestAQueryUnderTheThresholdIsNotSlow(t *testing.T) {
	logger, logs := newTraceLogger(gormlogger.Warn)

	logger.Trace(context.Background(), time.Now().Add(-slowQueryThreshold/2),
		statement("SELECT * FROM `hosts`", 1), nil)

	requireNothingLogged(t, logs)
}

// TestASlowQueryIsNotReportedAtTheErrorLevel is the level check in the slow
// branch: at "error" the setup asked for failed queries alone.
func TestASlowQueryIsNotReportedAtTheErrorLevel(t *testing.T) {
	logger, logs := newTraceLogger(gormlogger.Error)

	logger.Trace(context.Background(), time.Now().Add(slowQueryThreshold*-2),
		statement("SELECT * FROM `service_ports`", 7), nil)

	requireNothingLogged(t, logs)
}

// TestEveryStatementIsTracedAtDebug covers the last branch. The line goes out
// at zap's Debug even though gorm calls the level Info, which is what keeps
// statement tracing tied to the application "debug" level.
func TestEveryStatementIsTracedAtDebug(t *testing.T) {
	logger, logs := newTraceLogger(gormlogger.Info)

	logger.Trace(context.Background(), time.Now(), statement("SELECT * FROM `hosts`", 3), nil)

	entry := onlyEntry(t, logs)

	if entry.Level != zapcore.DebugLevel {
		t.Fatalf("the entry is at %s, want %s", entry.Level, zapcore.DebugLevel)
	}

	if entry.Message != "query" {
		t.Fatalf("the message is %q, want %q", entry.Message, "query")
	}

	requireField(t, entry, "sql", "SELECT * FROM `hosts`")
	requireField(t, entry, "rows", int64(3))

	if _, ok := entry.ContextMap()["elapsed_ms"].(float64); !ok {
		t.Fatalf("the elapsed_ms field is missing from %v", entry.ContextMap())
	}

	// The threshold only belongs on a line that reports a slow query.
	if _, ok := entry.ContextMap()["slow_threshold_ms"]; ok {
		t.Fatalf("a plain query carries slow_threshold_ms: %v", entry.ContextMap())
	}
}

// TestAPlainQueryIsNotTracedBelowDebug is the other half: at "info" and "warn"
// the log must not fill up with every statement that ran.
func TestAPlainQueryIsNotTracedBelowDebug(t *testing.T) {
	for _, level := range []gormlogger.LogLevel{gormlogger.Error, gormlogger.Warn} {
		t.Run(levelName(level), func(t *testing.T) {
			logger, logs := newTraceLogger(level)

			logger.Trace(context.Background(), time.Now(), statement("SELECT 1", 1), nil)

			requireNothingLogged(t, logs)
		})
	}
}

// newTestDatabase opens a database under a directory of the test that does not
// exist yet, so every test also walks the path that a first startup takes.
func newTestDatabase(t *testing.T) (*gorm.DB, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "state", "tunnel-manager.db")
	core, _ := observer.New(zapcore.DebugLevel)

	db, _, err := NewDatabase(path, zap.New(core), "error")
	if err != nil {
		t.Fatalf("failed to open the database at %s: %v", path, err)
	}

	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	return db, path
}

// tableNames returns the tables the database holds, leaving out the ones SQLite
// keeps for itself.
func tableNames(t *testing.T, db *gorm.DB) []string {
	t.Helper()

	var names []string
	err := db.Raw("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'").
		Scan(&names).Error
	if err != nil {
		t.Fatalf("failed to read the table list: %v", err)
	}

	sort.Strings(names)

	return names
}

// TestNewDatabaseBuildsTheFileUnderADirectoryThatIsNotThereYet covers the first
// startup of a fresh install. SQLite creates the database file but not the
// directories above it, so a path that does not exist yet would otherwise fail
// to open with nothing created and nothing said about the directory.
func TestNewDatabaseBuildsTheFileUnderADirectoryThatIsNotThereYet(t *testing.T) {
	db, path := newTestDatabase(t)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the database file was not created: %v", err)
	}
	if info.Size() == 0 {
		t.Fatalf("the database file at %s is empty", path)
	}

	want := []string{"host_service_ports", "hosts", "service_ports", "settings", "tls_certificates", "tunnels", "user"}
	got := tableNames(t, db)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the tables are %v, want %v", got, want)
	}
}

// TestNewDatabaseReportsTheResolvedPath holds that the absolute path is what is
// logged. A configured path may be relative, and a relative path is read against
// the working directory, which differs between running from the repository, from
// the container and from systemd, so the value as written names no file.
func TestNewDatabaseReportsTheResolvedPath(t *testing.T) {
	dir := t.TempDir()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to read the working directory: %v", err)
	}

	err = os.Chdir(dir)
	if err != nil {
		t.Fatalf("failed to move into %s: %v", dir, err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(wd)
	})

	core, logs := observer.New(zapcore.DebugLevel)

	db, _, err := NewDatabase(filepath.Join("state", "tunnel-manager.db"), zap.New(core), "error")
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	entries := logs.FilterMessage("opened the database").All()
	if len(entries) != 1 {
		t.Fatalf("the open was logged %d times, want once: %v", len(entries), logs.AllUntimed())
	}

	logged, ok := entries[0].ContextMap()["path"].(string)
	if !ok {
		t.Fatalf("the path is missing from %v", entries[0].ContextMap())
	}

	if !filepath.IsAbs(logged) {
		t.Fatalf("the logged path %q is relative, so it names no file on its own", logged)
	}

	// The symlink is resolved on both sides because the temporary directory of
	// a test is reached through one on macOS.
	wantDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("failed to resolve %s: %v", dir, err)
	}
	gotDir, err := filepath.EvalSymlinks(filepath.Dir(filepath.Dir(logged)))
	if err != nil {
		t.Fatalf("failed to resolve %s: %v", logged, err)
	}

	if gotDir != wantDir {
		t.Fatalf("the logged path is %q, want it under %q", logged, wantDir)
	}
}

// TestNewDatabaseWrapsAnOpenFailure checks what the caller gets when the path
// cannot be opened as a database. The startup has nothing to retry here, so the
// reason has to travel back whole.
func TestNewDatabaseWrapsAnOpenFailure(t *testing.T) {
	// A directory is a path that exists and that SQLite cannot open as a
	// database file, which is what an operator hits by pointing database.path
	// at a directory.
	path := t.TempDir()

	core, logs := observer.New(zapcore.DebugLevel)

	db, _, err := NewDatabase(path, zap.New(core), "error")
	if err == nil {
		t.Fatalf("a directory produced a usable handle: %v", db)
	}
	if db != nil {
		t.Fatalf("a handle was returned along with the error: %v", db)
	}
	if !strings.HasPrefix(err.Error(), "failed to open the database: ") {
		t.Fatalf("the error is %q, want it wrapped as an open failure", err)
	}
	if errors.Unwrap(err) == nil {
		t.Fatalf("the error %q does not carry what the driver reported", err)
	}

	// gorm reports a failed open through the logger it was handed, which is the
	// one built here, so the line has to come out under the "gorm" name rather
	// than going to stderr on its own.
	entries := logs.All()
	if len(entries) == 0 {
		t.Fatalf("the failure was not reported through the given logger")
	}
	for _, entry := range entries {
		if entry.LoggerName != "gorm" {
			t.Fatalf("an entry came out under %q, want %q", entry.LoggerName, "gorm")
		}
	}
}

// TestTheDSNCarriesThePragmas reads the parameters back out of the assembled
// DSN. They only reach the driver as text, so a name it does not know would be
// dropped without a word and leave the database in the mode it was made with.
func TestTheDSNCarriesThePragmas(t *testing.T) {
	dsn := sqliteDSN("/var/lib/tunnel-manager/tunnel-manager.db")

	path, params, found := strings.Cut(dsn, "?")
	if !found {
		t.Fatalf("the DSN %q carries no parameters", dsn)
	}
	if path != "/var/lib/tunnel-manager/tunnel-manager.db" {
		t.Fatalf("the DSN points at %q, want the path it was given", path)
	}

	want := []string{"_pragma=journal_mode(WAL)", fmt.Sprintf("_pragma=busy_timeout(%d)", busyTimeout.Milliseconds())}
	for _, param := range want {
		if !strings.Contains(params, param) {
			t.Fatalf("the DSN %q is missing %s", dsn, param)
		}
	}
}

// TestThePragmasReachTheConnection is the other half: the DSN carrying them
// proves nothing unless the connection came up with them. Both are read back
// from the open database.
func TestThePragmasReachTheConnection(t *testing.T) {
	db, _ := newTestDatabase(t)

	var journalMode string
	err := db.Raw("PRAGMA journal_mode").Scan(&journalMode).Error
	if err != nil {
		t.Fatalf("failed to read the journal mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Fatalf("the journal mode is %q, want wal", journalMode)
	}

	var busy int64
	err = db.Raw("PRAGMA busy_timeout").Scan(&busy).Error
	if err != nil {
		t.Fatalf("failed to read the busy timeout: %v", err)
	}
	if busy != busyTimeout.Milliseconds() {
		t.Fatalf("the busy timeout is %d ms, want %d", busy, busyTimeout.Milliseconds())
	}
}

// writeSpan is when one write transaction started and when it ended.
type writeSpan struct {
	start time.Time
	end   time.Time
}

// runTwoWriteTransactions sends two transactions at the same Host row at once,
// each holding the row for hold, and reports when each of them ran and what it
// came back with. Each one changes a field of its own, so a write that was lost
// shows up as a field that stayed empty.
func runTwoWriteTransactions(t *testing.T, db *gorm.DB, hold time.Duration) ([]writeSpan, []error) {
	t.Helper()

	err := db.Create(&models.Host{IP: "192.0.2.10", Port: 22, User: "root", Password: "x"}).Error
	if err != nil {
		t.Fatalf("failed to store the Host the writes act on: %v", err)
	}

	spans := make([]writeSpan, 2)
	errs := make([]error, 2)

	var wg sync.WaitGroup

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = db.Transaction(func(tx *gorm.DB) error {
				spans[i].start = time.Now()

				var host models.Host
				err := tx.First(&host, 1).Error
				if err != nil {
					spans[i].end = time.Now()
					return err
				}

				// The transaction is held open on purpose, so that two of them
				// running at once cannot be missed for want of time.
				time.Sleep(hold)

				if i == 0 {
					host.Description = "first"
				} else {
					host.User = "second"
				}

				err = tx.Save(&host).Error
				spans[i].end = time.Now()

				return err
			})
		}(i)
	}

	wg.Wait()

	return spans, errs
}

func spansOverlap(spans []writeSpan) bool {
	return spans[0].start.Before(spans[1].end) && spans[1].start.Before(spans[0].end)
}

// TestOneConnectionPutsTheWritesInAQueue is what stands in for the row lock
// that was removed. SQLite takes one writer at a time, and gorm leaves
// SELECT ... FOR UPDATE out of SQLite SQL without a word, so the pool of one
// connection that NewDatabase opens is the only thing holding two write
// transactions apart.
//
// The control below shows what the same two transactions do without it: they
// run at the same time, one of them is refused with "database is locked", and
// the change it carried is gone.
func TestOneConnectionPutsTheWritesInAQueue(t *testing.T) {
	const hold = 150 * time.Millisecond

	db, _ := newTestDatabase(t)

	spans, errs := runTwoWriteTransactions(t, db, hold)

	if spansOverlap(spans) {
		t.Fatalf("the two write transactions ran at the same time: %v", spans)
	}

	for i, err := range errs {
		if err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}
	}

	var host models.Host
	err := db.First(&host, 1).Error
	if err != nil {
		t.Fatalf("failed to read the Host back: %v", err)
	}

	if host.Description != "first" {
		t.Errorf("the description is %q, want the first write to have landed", host.Description)
	}
	if host.User != "second" {
		t.Errorf("the user is %q, want the second write to have landed", host.User)
	}

	t.Logf("overlap=%v errs=%v description=%q user=%q",
		spansOverlap(spans), errs, host.Description, host.User)
}

// TestWithoutTheSingleConnectionTheWritesCollide is the control. It opens the
// same file with the same pragmas and leaves the pool unbounded, which is the
// state the code would be in if SetMaxOpenConns(1) were dropped.
func TestWithoutTheSingleConnectionTheWritesCollide(t *testing.T) {
	const hold = 150 * time.Millisecond

	path := filepath.Join(t.TempDir(), "tunnel-manager.db")

	db, err := gorm.Open(sqlite.Open(sqliteDSN(path)), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	err = db.AutoMigrate(&models.Host{})
	if err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	spans, errs := runTwoWriteTransactions(t, db, hold)

	if !spansOverlap(spans) {
		t.Skipf("the two transactions did not run at the same time, so there was nothing to collide: %v", spans)
	}

	refused := 0
	for _, err := range errs {
		if err != nil && strings.Contains(err.Error(), "database is locked") {
			refused++
		}
	}

	if refused == 0 {
		t.Fatalf("no write was refused although the two ran at the same time: %v", errs)
	}

	var host models.Host
	err = db.First(&host, 1).Error
	if err != nil {
		t.Fatalf("failed to read the Host back: %v", err)
	}

	t.Logf("overlap=%v errs=%v description=%q user=%q",
		spansOverlap(spans), errs, host.Description, host.User)
}

// newObservedDatabase opens a database together with the handle its logger
// follows and the record of what that logger writes. It is what the tests below
// need that newTestDatabase does not hand back: the level is what is under
// test, and it is judged by the lines that came out of a real query.
func newObservedDatabase(t *testing.T, level string) (*gorm.DB, *LogLevel, *observer.ObservedLogs) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "state", "tunnel-manager.db")
	core, logs := observer.New(zapcore.DebugLevel)

	db, handle, err := NewDatabase(path, zap.New(core), level)
	if err != nil {
		t.Fatalf("failed to open the database at %s: %v", path, err)
	}

	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	return db, handle, logs
}

// tracedStatements counts the statements that were traced, which is the line
// the level under test decides on.
func tracedStatements(logs *observer.ObservedLogs) int {
	return logs.FilterMessage("query").Len()
}

// TestTheStoredLevelReachesALoggerThatIsAlreadyInUse is the whole point of the
// handle. The logger is built once, at startup, and the level is stored later
// from the Settings screen, so a level that only counted at build time would
// leave the screen promising something that does not happen.
func TestTheStoredLevelReachesALoggerThatIsAlreadyInUse(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	handle := NewLogLevel("info")
	logger := &zapGormLogger{logger: zap.New(core), level: gormLogLevel("info"), shared: handle}

	logger.Trace(context.Background(), time.Now(), statement("SELECT 1", 1), nil)
	if traced := tracedStatements(logs); traced != 0 {
		t.Fatalf("%d statements were traced at info, want none", traced)
	}

	handle.Set("debug")

	logger.Trace(context.Background(), time.Now(), statement("SELECT 2", 1), nil)
	if traced := tracedStatements(logs); traced != 1 {
		t.Fatalf("%d statements were traced after debug was stored, want one", traced)
	}

	handle.Set("info")

	logger.Trace(context.Background(), time.Now(), statement("SELECT 3", 1), nil)
	if traced := tracedStatements(logs); traced != 1 {
		t.Fatalf("%d statements were traced after info was stored again, want the one from before", traced)
	}
}

// TestLogModeKeepsTheLevelItWasAskedFor is the other half of the handle. A
// session that asked for a level (db.Debug() is that call) has to stay at it,
// so the copy stops following the handle while the logger it was copied from
// keeps following it.
func TestLogModeKeepsTheLevelItWasAskedFor(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	handle := NewLogLevel("info")
	original := &zapGormLogger{logger: zap.New(core), level: gormLogLevel("info"), shared: handle}

	pinned, ok := original.LogMode(gormlogger.Info).(*zapGormLogger)
	if !ok {
		t.Fatalf("LogMode did not return a *zapGormLogger")
	}

	if pinned.shared != nil {
		t.Fatalf("the copy still follows the handle, so the level it was asked for can be taken away")
	}

	if original.shared != handle {
		t.Fatalf("the original stopped following the handle")
	}

	// Storing the quietest level must not silence a session that asked to see
	// its statements.
	handle.Set("error")

	pinned.Trace(context.Background(), time.Now(), statement("SELECT 1", 1), nil)
	if traced := tracedStatements(logs); traced != 1 {
		t.Fatalf("%d statements were traced by the pinned copy, want one", traced)
	}

	original.Trace(context.Background(), time.Now(), statement("SELECT 2", 1), nil)
	if traced := tracedStatements(logs); traced != 1 {
		t.Fatalf("the original traced a statement at the stored level error")
	}
}

// TestTheHandleChangesWhatARealQueryWrites runs the same query at both levels
// through gorm itself. The test above builds the logger by hand, and this one
// answers whether the handle is where gorm actually reads the level from.
func TestTheHandleChangesWhatARealQueryWrites(t *testing.T) {
	db, handle, logs := newObservedDatabase(t, "info")

	var hosts []models.Host

	err := db.Find(&hosts).Error
	if err != nil {
		t.Fatalf("the query failed: %v", err)
	}
	if traced := tracedStatements(logs); traced != 0 {
		t.Fatalf("%d statements were traced at info, want none", traced)
	}

	handle.Set("debug")

	err = db.Find(&hosts).Error
	if err != nil {
		t.Fatalf("the query failed: %v", err)
	}

	traced := tracedStatements(logs)
	if traced == 0 {
		t.Fatalf("no statement was traced after debug was stored")
	}

	handle.Set("info")

	err = db.Find(&hosts).Error
	if err != nil {
		t.Fatalf("the query failed: %v", err)
	}
	if after := tracedStatements(logs); after != traced {
		t.Fatalf("%d statements were traced after info was stored again, want the %d from before",
			after, traced)
	}
}

// TestTheLevelCanBeChangedWhileQueriesRun is why the level is an atomic rather
// than a plain field. The save arrives on the goroutine that serves the request
// while queries run on others, so the two meet on every save, and a field
// written from one goroutine and read from another is a data race whatever the
// values happen to be.
//
// The writer keeps flipping the level until the readers have run all their
// queries, rather than flipping a fixed number of times: a loop that is over
// before the first query is issued never overlaps with one, and a test that
// does not overlap reports nothing. Run with -race.
func TestTheLevelCanBeChangedWhileQueriesRun(t *testing.T) {
	db, handle, _ := newObservedDatabase(t, "info")

	const readers = 4
	const queriesPerReader = 25

	var wg sync.WaitGroup

	wg.Add(readers)
	for reader := 0; reader < readers; reader++ {
		go func() {
			defer wg.Done()

			for query := 0; query < queriesPerReader; query++ {
				var hosts []models.Host
				_ = db.Find(&hosts).Error
			}
		}()
	}

	done := make(chan struct{})
	written := make(chan struct{})

	go func() {
		defer close(written)

		for {
			select {
			case <-done:
				return
			default:
			}

			handle.Set("debug")
			handle.Set("info")
		}
	}()

	wg.Wait()
	close(done)
	<-written
}

// TestNewDatabaseKeepsTheHostsThatAreAlreadyStored is the migration this
// installation is carried through when the private key columns arrive. The
// table it is run against is the one the earlier versions built: a password
// that is NOT NULL and no column for a key.
//
// What it holds is that a row written by the version before this one is still
// there afterwards, with the password it was stored with, and that the columns
// the key needs were added rather than the table being rebuilt empty. A
// migration that drops a Host takes down every tunnel of that Host, and the
// password it held exists nowhere else.
func TestNewDatabaseKeepsTheHostsThatAreAlreadyStored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "tunnel-manager.db")

	err := os.MkdirAll(filepath.Dir(path), 0755)
	if err != nil {
		t.Fatalf("failed to create the directory of the database: %v", err)
	}

	// The table as the version before this one created it, written out rather
	// than migrated from an old model, because that model is gone from the
	// source and the shape it left behind is what a running installation holds.
	old, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	statements := []string{
		"CREATE TABLE `hosts` (`id` integer PRIMARY KEY AUTOINCREMENT," +
			"`ip` text NOT NULL,`port` integer NOT NULL,`user` text NOT NULL," +
			"`password` text NOT NULL,`description` text,`enabled` numeric DEFAULT true," +
			"`created_at` datetime,`updated_at` datetime)",
		"CREATE UNIQUE INDEX `idx_hosts_ip` ON `hosts`(`ip`)",
		"INSERT INTO `hosts` (`ip`,`port`,`user`,`password`,`description`,`enabled`," +
			"`created_at`,`updated_at`) VALUES ('192.0.2.10',22,'operator'," +
			"'tmenc:v1:the-sealed-password-of-the-test','a host of the version before'," +
			"true,'2026-09-01 00:00:00','2026-09-01 00:00:00')",
	}

	for _, sql := range statements {
		err = old.Exec(sql).Error
		if err != nil {
			t.Fatalf("failed to build the old table: %v", err)
		}
	}

	oldDB, err := old.DB()
	if err != nil {
		t.Fatalf("failed to reach the connection pool: %v", err)
	}
	err = oldDB.Close()
	if err != nil {
		t.Fatalf("failed to close the old handle: %v", err)
	}

	core, _ := observer.New(zapcore.DebugLevel)

	db, _, err := NewDatabase(path, zap.New(core), "error")
	if err != nil {
		t.Fatalf("the migration failed: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	var hosts []models.Host
	err = db.Find(&hosts).Error
	if err != nil {
		t.Fatalf("failed to read the Hosts: %v", err)
	}

	if len(hosts) != 1 {
		t.Fatalf("%d Hosts are stored after the migration, want the one that was there", len(hosts))
	}

	host := hosts[0]

	if host.IP != "192.0.2.10" || host.User != "operator" || host.Port != 22 {
		t.Fatalf("the stored Host came back as %+v", host)
	}
	if host.Password != "tmenc:v1:the-sealed-password-of-the-test" {
		t.Fatal("the stored password of the Host did not survive the migration")
	}
	if !host.Enabled {
		t.Fatal("the Host was disabled by the migration")
	}
	if host.PrivateKey != "" || host.KeyPassphrase != "" {
		t.Fatal("the migration put something in the key columns of a Host that carries none")
	}

	// The new columns are there to be written, and the password may now be
	// left out: that is the Host registered with a key alone.
	err = db.Create(&models.Host{
		IP:            "192.0.2.11",
		Port:          22,
		User:          "operator",
		PrivateKey:    "tmenc:v1:the-sealed-key-of-the-test",
		KeyPassphrase: "tmenc:v1:the-sealed-passphrase-of-the-test",
		Enabled:       true,
	}).Error
	if err != nil {
		t.Fatalf("a Host with a key and no password was not stored: %v", err)
	}

	var withKey models.Host
	err = db.Where("ip = ?", "192.0.2.11").First(&withKey).Error
	if err != nil {
		t.Fatalf("failed to read the Host back: %v", err)
	}
	if withKey.PrivateKey != "tmenc:v1:the-sealed-key-of-the-test" {
		t.Fatalf("the key came back as %q", withKey.PrivateKey)
	}
	if withKey.Password != "" {
		t.Fatalf("the password of a Host that carries none came back as %q", withKey.Password)
	}
}

// newDatabaseFromBefore builds the database of an installation that ran before
// the assignments were stored: the Hosts and the service ports are there and
// the table that pairs them is not. It is written through the models rather
// than by hand because those two tables are unchanged by this migration, and
// what the test is about is the table that is missing.
func newDatabaseFromBefore(t *testing.T, hosts []models.Host, servicePorts []models.ServicePort) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "state", "tunnel-manager.db")

	err := os.MkdirAll(filepath.Dir(path), 0755)
	if err != nil {
		t.Fatalf("failed to create the directory of the database: %v", err)
	}

	old, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	err = old.AutoMigrate(&models.Host{}, &models.ServicePort{}, &models.Tunnel{})
	if err != nil {
		t.Fatalf("failed to build the tables as they were: %v", err)
	}

	for i := range hosts {
		err = old.Create(&hosts[i]).Error
		if err != nil {
			t.Fatalf("failed to store the Host %s: %v", hosts[i].IP, err)
		}
	}

	for i := range servicePorts {
		err = old.Create(&servicePorts[i]).Error
		if err != nil {
			t.Fatalf("failed to store the service port %d: %v", servicePorts[i].LocalPort, err)
		}
	}

	sqlDB, err := old.DB()
	if err != nil {
		t.Fatalf("failed to reach the connection pool: %v", err)
	}
	err = sqlDB.Close()
	if err != nil {
		t.Fatalf("failed to close the old handle: %v", err)
	}

	return path
}

// openAndClose runs a startup against path and closes the handle again, so that
// a test can open the same file more than once the way restarting does.
func openAndClose(t *testing.T, path string) {
	t.Helper()

	core, _ := observer.New(zapcore.DebugLevel)

	db, _, err := NewDatabase(path, zap.New(core), "error")
	if err != nil {
		t.Fatalf("the startup against %s failed: %v", path, err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to reach the connection pool: %v", err)
	}
	err = sqlDB.Close()
	if err != nil {
		t.Fatalf("failed to close the handle: %v", err)
	}
}

// storedAssignments returns the pairs the database holds, read with SQL rather
// than through the model so that the test says what is in the table.
func storedAssignments(t *testing.T, db *gorm.DB) []string {
	t.Helper()

	var pairs []string
	err := db.Raw("SELECT host_id || ':' || sp_id FROM host_service_ports ORDER BY host_id, sp_id").
		Scan(&pairs).Error
	if err != nil {
		t.Fatalf("failed to read the assignments: %v", err)
	}

	return pairs
}

// TestTheFirstStartupAssignsEveryServicePortToEveryHost covers the upgrade of
// an installation that is running. Every Host carried every service port
// before the assignments were stored anywhere, so a table left empty would be
// read as "no Host carries anything" and would take every tunnel down.
func TestTheFirstStartupAssignsEveryServicePortToEveryHost(t *testing.T) {
	path := newDatabaseFromBefore(t,
		[]models.Host{
			{IP: "192.0.2.10", Port: 22, User: "operator", Enabled: true},
			// The second Host is disabled on purpose. Whether a Host runs
			// tunnels is decided where they are reconciled, and one left
			// without assignments here would come back from being enabled
			// with no tunnels at all.
			{IP: "192.0.2.11", Port: 22, User: "operator", Enabled: false},
		},
		[]models.ServicePort{
			{ServiceIP: "198.51.100.20", ServicePort: 8080, LocalPort: 18080},
			{ServiceIP: "198.51.100.20", ServicePort: 8081, LocalPort: 18081},
			{ServiceIP: "198.51.100.21", ServicePort: 8080, LocalPort: 18082},
		},
	)

	core, _ := observer.New(zapcore.DebugLevel)

	db, _, err := NewDatabase(path, zap.New(core), "error")
	if err != nil {
		t.Fatalf("the migration failed: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	want := []string{"1:1", "1:2", "1:3", "2:1", "2:2", "2:3"}
	got := storedAssignments(t, db)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the assignments are %v, want %v", got, want)
	}
}

// TestALaterStartupDoesNotPutBackARemovedAssignment is the other half of it.
// The table is filled once, on the startup that creates it, because filling it
// on every startup would undo what the operator took away, which is the whole
// of what the table is for.
func TestALaterStartupDoesNotPutBackARemovedAssignment(t *testing.T) {
	path := newDatabaseFromBefore(t,
		[]models.Host{
			{IP: "192.0.2.10", Port: 22, User: "operator", Enabled: true},
			{IP: "192.0.2.11", Port: 22, User: "operator", Enabled: true},
		},
		[]models.ServicePort{
			{ServiceIP: "198.51.100.20", ServicePort: 8080, LocalPort: 18080},
			{ServiceIP: "198.51.100.20", ServicePort: 8081, LocalPort: 18081},
			{ServiceIP: "198.51.100.21", ServicePort: 8080, LocalPort: 18082},
		},
	)

	openAndClose(t, path)

	removing, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	err = removing.Exec("DELETE FROM host_service_ports WHERE host_id = 1 AND sp_id = 2").Error
	if err != nil {
		t.Fatalf("failed to remove an assignment: %v", err)
	}

	removingDB, err := removing.DB()
	if err != nil {
		t.Fatalf("failed to reach the connection pool: %v", err)
	}
	err = removingDB.Close()
	if err != nil {
		t.Fatalf("failed to close the handle: %v", err)
	}

	core, _ := observer.New(zapcore.DebugLevel)

	db, _, err := NewDatabase(path, zap.New(core), "error")
	if err != nil {
		t.Fatalf("the second startup failed: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	want := []string{"1:1", "1:3", "2:1", "2:2", "2:3"}
	got := storedAssignments(t, db)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the assignments are %v, want %v", got, want)
	}
}

// TestAFreshInstallIsAssignedNothingAndSaysNothing holds the startup of an
// install that has neither a Host nor a service port. There is nothing to
// assign, and nothing to report about having assigned it.
func TestAFreshInstallIsAssignedNothingAndSaysNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "tunnel-manager.db")
	core, logs := observer.New(zapcore.DebugLevel)

	db, _, err := NewDatabase(path, zap.New(core), "error")
	if err != nil {
		t.Fatalf("the first startup of a fresh install failed: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	if pairs := storedAssignments(t, db); len(pairs) != 0 {
		t.Fatalf("a fresh install was given the assignments %v", pairs)
	}

	for _, entry := range logs.All() {
		if entry.Level >= zapcore.WarnLevel {
			t.Fatalf("the startup of a fresh install wrote %s: %s", entry.Level, entry.Message)
		}
		if strings.Contains(entry.Message, "assigned") {
			t.Fatalf("the startup of a fresh install reported an assignment: %s", entry.Message)
		}
	}
}

// TestTheAssignmentsAreWrittenInBatches holds that the rows do not go in one at
// a time. An installation stores every Host against every service port, so the
// rows multiply, and a statement per row is what that turns into on the very
// startup an upgrade is waiting on.
func TestTheAssignmentsAreWrittenInBatches(t *testing.T) {
	var hosts []models.Host
	for i := 0; i < 20; i++ {
		hosts = append(hosts, models.Host{
			IP:      fmt.Sprintf("192.0.2.%d", i+10),
			Port:    22,
			User:    "operator",
			Enabled: true,
		})
	}

	var servicePorts []models.ServicePort
	for i := 0; i < 20; i++ {
		servicePorts = append(servicePorts, models.ServicePort{
			ServiceIP:   "198.51.100.20",
			ServicePort: 8080 + i,
			LocalPort:   18080 + i,
		})
	}

	path := newDatabaseFromBefore(t, hosts, servicePorts)
	core, logs := observer.New(zapcore.DebugLevel)

	db, _, err := NewDatabase(path, zap.New(core), "debug")
	if err != nil {
		t.Fatalf("the migration failed: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	var count int64
	err = db.Model(&models.HostServicePort{}).Count(&count).Error
	if err != nil {
		t.Fatalf("failed to count the assignments: %v", err)
	}
	if count != 400 {
		t.Fatalf("%d assignments are stored, want 400", count)
	}

	inserts := 0
	for _, entry := range logs.All() {
		sql, ok := entry.ContextMap()["sql"].(string)
		if ok && strings.Contains(sql, "INSERT INTO `host_service_ports`") {
			inserts++
		}
	}

	if inserts == 0 {
		t.Fatal("no insert into the assignment table was traced")
	}

	// 400 rows at assignmentBatchSize a statement. The test names the number
	// the size gives rather than a loose bound, so that a change to the size
	// is a change to be looked at.
	want := (400 + assignmentBatchSize - 1) / assignmentBatchSize
	if inserts != want {
		t.Fatalf("the 400 assignments took %d statements, want %d", inserts, want)
	}
}

// theHashOfTheTest stands in for what the account table holds. It is written
// out rather than produced with bcrypt so that the test names the very string
// it then looks for, and so that the check does not depend on the hashing
// package.
const theHashOfTheTest = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

// requireNoHashAnywhere fails if the hash reached any part of any line. It
// looks at the whole of every entry rather than at the sql field alone,
// because the point is that the value is not in the log file, and a field
// added later would carry it just as well.
func requireNoHashAnywhere(t *testing.T, logs *observer.ObservedLogs) {
	t.Helper()

	for _, entry := range logs.All() {
		rendered := fmt.Sprintf("%s %v", entry.Message, entry.ContextMap())
		if strings.Contains(rendered, theHashOfTheTest) {
			t.Fatalf("the password hash is in the log: %s", rendered)
		}
	}
}

// TestMaskStatementHidesTheAccountTableAndLeavesTheRest walks the forms a
// statement can take. The account table is "user", which is also the name of a
// column of the hosts table, so the cases below are as much about what must
// not be redacted as about what must.
func TestMaskStatementHidesTheAccountTableAndLeavesTheRest(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want bool
	}{
		{
			name: "insert as the driver writes it",
			sql:  "INSERT INTO `user` (`username`,`password_hash`) VALUES ('operator','" + theHashOfTheTest + "')",
			want: true,
		},
		{
			name: "update as the driver writes it",
			sql:  "UPDATE `user` SET `password_hash`='" + theHashOfTheTest + "' WHERE `id` = 1",
			want: true,
		},
		{
			name: "select",
			sql:  "SELECT * FROM `user` WHERE `username` = 'operator' ORDER BY `user`.`id` LIMIT 1",
			want: true,
		},
		{
			name: "delete",
			sql:  "DELETE FROM `user` WHERE `id` = 1",
			want: true,
		},
		{
			name: "unquoted",
			sql:  "UPDATE user SET password_hash = 'x'",
			want: true,
		},
		{
			name: "double quoted",
			sql:  "SELECT * FROM \"user\"",
			want: true,
		},
		{
			name: "the migration that builds it",
			sql:  "CREATE TABLE `user` (`id` integer PRIMARY KEY AUTOINCREMENT,`password_hash` text NOT NULL)",
			want: true,
		},
		{
			name: "broken over lines",
			sql:  "SELECT *\n\tFROM `user`\n\tWHERE `id` = 1",
			want: true,
		},
		{
			name: "the user column of a host",
			sql:  "UPDATE `hosts` SET `user`='operator',`updated_at`='2026-09-22 00:00:00' WHERE `id` = 1",
			want: false,
		},
		{
			name: "a host read back by its user",
			sql:  "SELECT `id`,`ip`,`user` FROM `hosts` WHERE `user` = 'operator'",
			want: false,
		},
		{
			name: "the updated_at column is not the update keyword",
			sql:  "SELECT `updated_at` FROM `tunnels`",
			want: false,
		},
		{
			name: "another table",
			sql:  "INSERT INTO `host_service_ports` (`host_id`,`sp_id`) VALUES (1,2)",
			want: false,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := maskStatement(test.sql)

			if test.want {
				if got != redactedStatement {
					t.Fatalf("maskStatement(%q) is %q, want it redacted", test.sql, got)
				}
				return
			}

			if got != test.sql {
				t.Fatalf("maskStatement(%q) is %q, want it unchanged", test.sql, got)
			}
		})
	}
}

// TestAFailedAccountWriteIsReportedWithoutTheHash is the branch that made this
// worth doing: a failed query is logged at the level the application runs at
// by default, so an INSERT into the account table that hits a constraint would
// write the password hash into a file that is read back through GET /api/logs.
//
// What has to survive the redaction is everything the line is read for, so the
// row count, the elapsed time and the error are checked as well.
func TestAFailedAccountWriteIsReportedWithoutTheHash(t *testing.T) {
	logger, logs := newTraceLogger(gormlogger.Error)

	failure := errors.New("UNIQUE constraint failed: user.username")

	logger.Trace(context.Background(), time.Now().Add(-50*time.Millisecond),
		statement("INSERT INTO `user` (`username`,`password_hash`,`setup_required`) VALUES ('operator','"+
			theHashOfTheTest+"',false)", 1), failure)

	entry := onlyEntry(t, logs)

	if entry.Message != "query failed" {
		t.Fatalf("the message is %q, want %q", entry.Message, "query failed")
	}

	requireField(t, entry, "sql", redactedStatement)
	requireField(t, entry, "rows", int64(1))
	requireField(t, entry, "error", failure.Error())
	requireNoHashAnywhere(t, logs)

	if _, ok := entry.ContextMap()["elapsed_ms"].(float64); !ok {
		t.Fatalf("the elapsed_ms field is missing from %v", entry.ContextMap())
	}
}

// TestTheAccountTableIsRedactedOnEverySlowAndTracedLine covers the other two
// branches, which all read the statement through the same closure. A guard put
// on one of them alone would leave the hash in the log of an installation that
// runs at debug, or of one where the write was slow.
func TestTheAccountTableIsRedactedOnEverySlowAndTracedLine(t *testing.T) {
	sql := "UPDATE `user` SET `password_hash`='" + theHashOfTheTest + "' WHERE `id` = 1"

	cases := []struct {
		name  string
		level gormlogger.LogLevel
		begin time.Time
		want  string
	}{
		{
			name:  "slow",
			level: gormlogger.Warn,
			begin: time.Now().Add(slowQueryThreshold * -2),
			want:  "slow query",
		},
		{
			name:  "traced",
			level: gormlogger.Info,
			begin: time.Now(),
			want:  "query",
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			logger, logs := newTraceLogger(test.level)

			logger.Trace(context.Background(), test.begin, statement(sql, 1), nil)

			entry := onlyEntry(t, logs)

			if entry.Message != test.want {
				t.Fatalf("the message is %q, want %q", entry.Message, test.want)
			}

			requireField(t, entry, "sql", redactedStatement)
			requireField(t, entry, "rows", int64(1))
			requireNoHashAnywhere(t, logs)
		})
	}
}

// TestAFailedQueryOnAnotherTableKeepsItsStatement is the other half: the
// redaction has to be narrow enough that the log is still worth reading. The
// statement carries the user column of the hosts table, which is the value a
// match on the bare word would trip over.
func TestAFailedQueryOnAnotherTableKeepsItsStatement(t *testing.T) {
	logger, logs := newTraceLogger(gormlogger.Error)

	sql := "UPDATE `hosts` SET `user`='operator',`enabled`=true WHERE `id` = 1"

	logger.Trace(context.Background(), time.Now(), statement(sql, 1), errors.New("database is locked"))

	entry := onlyEntry(t, logs)

	requireField(t, entry, "sql", sql)
}

// TestARealFailedAccountWriteDoesNotLeakTheHash runs the statement through
// gorm and the driver rather than through a hand-built callback, which is what
// answers whether the string the logger is handed in the running program is
// the one the redaction catches. gorm writes the bound values into that string
// itself (Dialector.Explain), so the hash is in it before the logger sees it.
//
// The database is opened at "error", the level an installation runs at by
// default, so the line under test is one an operator would actually have.
func TestARealFailedAccountWriteDoesNotLeakTheHash(t *testing.T) {
	db, _, logs := newObservedDatabase(t, "error")

	err := db.Create(&models.User{ID: 1, Username: "operator", PasswordHash: theHashOfTheTest}).Error
	if err != nil {
		t.Fatalf("failed to store the account: %v", err)
	}

	// The same ID again, which SQLite refuses. It is the failure an account
	// write hits in the running program: a constraint, reported through the
	// logger with the statement that ran.
	err = db.Create(&models.User{ID: 1, Username: "operator", PasswordHash: theHashOfTheTest}).Error
	if err == nil {
		t.Fatal("the second account row was stored, so nothing failed to be logged")
	}

	failures := logs.FilterMessage("query failed").All()
	if len(failures) == 0 {
		t.Fatalf("the failed account write was not logged at all: %v", logs.All())
	}

	for _, entry := range failures {
		requireField(t, entry, "sql", redactedStatement)
	}

	requireNoHashAnywhere(t, logs)
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

// requireSidecarsAreClosed holds the write-ahead log and the shared memory file
// to the mode of the database file. They exist only while a connection is open,
// so one that is not there is passed over rather than failed on.
func requireSidecarsAreClosed(t *testing.T, path string) {
	t.Helper()

	for _, sidecar := range databaseSidecars {
		beside := path + sidecar

		_, err := os.Stat(beside)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		requireMode(t, beside, databaseFileMode)
	}
}

// TestANewDatabaseIsClosedToTheRestOfTheMachine covers the first startup. The
// file holds the hash of the account password, the registered Hosts and the
// certificate of this installation, and the sqlite driver creates it at 0644,
// which hands all of that to any other local user of the machine.
func TestANewDatabaseIsClosedToTheRestOfTheMachine(t *testing.T) {
	skipWithoutFileModes(t)

	_, path := newTestDatabase(t)

	requireMode(t, path, databaseFileMode)
	requireMode(t, filepath.Dir(path), databaseDirMode)
	requireSidecarsAreClosed(t, path)
}

// TestADatabaseFromAnEarlierReleaseIsNarrowedOnTheNextStartup is the half that
// matters to a deployment that is already running. Creating new files narrowly
// does nothing for the installations that hold the secrets today, so the
// startup sets the mode of what it finds as well as of what it makes.
func TestADatabaseFromAnEarlierReleaseIsNarrowedOnTheNextStartup(t *testing.T) {
	skipWithoutFileModes(t)

	db, path := newTestDatabase(t)

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to reach the connection pool: %v", err)
	}

	err = sqlDB.Close()
	if err != nil {
		t.Fatalf("failed to close the database of the first startup: %v", err)
	}

	// What an earlier release left behind: the file as the driver created it
	// and the directory as MkdirAll made it.
	dir := filepath.Dir(path)

	err = os.Chmod(path, 0644)
	if err != nil {
		t.Fatalf("failed to put the database back to the mode of an earlier release: %v", err)
	}

	err = os.Chmod(dir, 0755)
	if err != nil {
		t.Fatalf("failed to put the directory back to the mode of an earlier release: %v", err)
	}

	core, _ := observer.New(zapcore.DebugLevel)

	again, _, err := NewDatabase(path, zap.New(core), "error")
	if err != nil {
		t.Fatalf("the second startup failed to open the database: %v", err)
	}

	t.Cleanup(func() {
		sqlDB, err := again.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	requireMode(t, path, databaseFileMode)
	requireMode(t, dir, databaseDirMode)
	requireSidecarsAreClosed(t, path)
}

// TestAModeThatCannotBeSetDoesNotStopTheStartup pins what happens where the
// mode cannot be written: a container volume on a file system that carries no
// Unix permissions, or files owned by another account. The database opened and
// every Host in it can be reached, so refusing to serve over a file this
// process has no way of narrowing would take a running installation away for
// nothing.
func TestAModeThatCannotBeSetDoesNotStopTheStartup(t *testing.T) {
	original := chmod

	t.Cleanup(func() {
		chmod = original
	})

	chmod = func(string, os.FileMode) error {
		return errors.New("this file system carries no permissions")
	}

	path := filepath.Join(t.TempDir(), "state", "tunnel-manager.db")
	core, _ := observer.New(zapcore.DebugLevel)

	db, _, err := NewDatabase(path, zap.New(core), "error")
	if err != nil {
		t.Fatalf("the startup was stopped by a mode that could not be set: %v", err)
	}

	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	// The handle is not only non-nil but usable, since a startup that came up
	// on a database it cannot query is no better than one that stopped.
	if !db.Migrator().HasTable(&models.Host{}) {
		t.Fatalf("the database that was opened holds no hosts table")
	}
}

// TestTightenPermissionsReportsEveryPathItCouldNotSet holds the gathering. The
// paths fail for the same reason when they fail at all, and a report that named
// the first one would leave whoever reads it fixing one file of several.
func TestTightenPermissionsReportsEveryPathItCouldNotSet(t *testing.T) {
	original := chmod

	t.Cleanup(func() {
		chmod = original
	})

	chmod = func(string, os.FileMode) error {
		return errors.New("this file system carries no permissions")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "tunnel-manager.db")

	err := os.WriteFile(path, []byte("x"), 0644)
	if err != nil {
		t.Fatalf("failed to write the file that stands in for the database: %v", err)
	}

	err = tightenPermissions(path)
	if err == nil {
		t.Fatal("a chmod that fails everywhere was reported as done")
	}

	for _, named := range []string{dir, path} {
		if !strings.Contains(err.Error(), named) {
			t.Errorf("the report does not name %s: %v", named, err)
		}
	}
}

// TestTightenPermissionsPassesOverAFileThatIsNotThere covers the database
// nobody has open. The write-ahead log and the shared memory file are removed
// when the last connection closes cleanly, so their absence is the normal state
// of such a file rather than something to report.
func TestTightenPermissionsPassesOverAFileThatIsNotThere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tunnel-manager.db")

	err := os.WriteFile(path, []byte("x"), 0644)
	if err != nil {
		t.Fatalf("failed to write the file that stands in for the database: %v", err)
	}

	err = tightenPermissions(path)
	if err != nil {
		t.Fatalf("a database with no files beside it was reported as a failure: %v", err)
	}
}
