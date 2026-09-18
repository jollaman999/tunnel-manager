package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

	db, err := NewDatabase(path, zap.New(core), "error")
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

	want := []string{"hosts", "service_ports", "settings", "tunnels", "user"}
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

	db, err := NewDatabase(filepath.Join("state", "tunnel-manager.db"), zap.New(core), "error")
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

	db, err := NewDatabase(path, zap.New(core), "error")
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
