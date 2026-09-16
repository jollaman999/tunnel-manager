package database

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	gormlogger "gorm.io/gorm/logger"
)

// The credentials below are made up. Nothing is served with them and the only
// address they are ever pointed at is a loopback port that is already closed.
const (
	testUser     = "tunnel"
	testPassword = "not-a-real-one" // hook:allow
	testDBName   = "tunnel_manager"
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

// closedPort hands back a loopback port that nothing listens on, so the connect
// attempt is refused at once instead of waiting out a timeout.
func closedPort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to take a loopback port: %v", err)
	}

	port := listener.Addr().(*net.TCPAddr).Port

	if err := listener.Close(); err != nil {
		t.Fatalf("failed to release the loopback port: %v", err)
	}

	return port
}

// TestNewDatabaseWrapsAConnectionFailure checks the error the caller gets when
// there is nothing to connect to. The address the driver reports comes from the
// assembled DSN, so it also shows that the host and the port were placed in it.
func TestNewDatabaseWrapsAConnectionFailure(t *testing.T) {
	core, _ := observer.New(zapcore.DebugLevel)

	port := closedPort(t)

	db, err := NewDatabase("127.0.0.1", port, testUser, testPassword, testDBName, zap.New(core), "info")
	if err == nil {
		t.Fatalf("a closed port produced a usable handle: %v", db)
	}

	if db != nil {
		t.Fatalf("a handle was returned along with the error: %v", db)
	}

	if !strings.HasPrefix(err.Error(), "failed to connect to database: ") {
		t.Fatalf("the error is %q, want it wrapped as a connection failure", err)
	}

	if errors.Unwrap(err) == nil {
		t.Fatalf("the error %q does not carry what the driver reported", err)
	}

	address := fmt.Sprintf("127.0.0.1:%d", port)
	if !strings.Contains(err.Error(), address) {
		t.Fatalf("the error is %q, want the %s the DSN pointed at", err, address)
	}
}

// TestNewDatabaseKeepsThePasswordOutOfTheError holds the DSN, password and all,
// out of what is handed back and logged when the connection cannot be made.
func TestNewDatabaseKeepsThePasswordOutOfTheError(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)

	_, err := NewDatabase("127.0.0.1", closedPort(t), testUser, testPassword, testDBName, zap.New(core), "info")
	if err == nil {
		t.Fatalf("a closed port produced a usable handle")
	}

	if strings.Contains(err.Error(), testPassword) {
		t.Fatalf("the error carries the password: %q", err)
	}

	for _, entry := range logs.All() {
		line := entry.Message + " " + fmt.Sprint(entry.ContextMap())
		if strings.Contains(line, testPassword) {
			t.Fatalf("a log entry carries the password: %q", line)
		}
	}
}

// TestNewDatabaseReportsTheFailureThroughTheGivenLogger shows that the gorm
// logger built here is the one gorm was handed: gorm reports a failed open
// through its own logger, and the line has to come out under the "gorm" name
// rather than going to stderr on its own.
func TestNewDatabaseReportsTheFailureThroughTheGivenLogger(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)

	_, err := NewDatabase("127.0.0.1", closedPort(t), testUser, testPassword, testDBName, zap.New(core), "error")
	if err == nil {
		t.Fatalf("a closed port produced a usable handle")
	}

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
