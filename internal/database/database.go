package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/settings"
	"go.uber.org/zap"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Queries slower than this are reported without logging every statement, though
// only up to the "warn" level. "error" and above report failed queries alone.
const slowQueryThreshold = 200 * time.Millisecond

// gormLogLevel maps an application log level to the level gorm understands.
// gorm only logs every statement at Info, which is far too noisy for the
// default setup, so plain SQL tracing is kept for "debug" only. "info" and
// "warn" report failed and slow queries, while "error" and above report
// failed queries alone.
func gormLogLevel(level string) gormlogger.LogLevel {
	switch level {
	case "debug":
		return gormlogger.Info
	case "info", "warn":
		return gormlogger.Warn
	default:
		return gormlogger.Error
	}
}

// LogLevel is the level the database logger writes at while the process runs.
//
// It is a handle rather than a value because the level is one of the stored
// settings: the Settings screen writes it, and the change has to reach the
// logger that gorm is already writing through. That is the same reason zap is
// given an AtomicLevel here, and the two are set together. Without it the level
// would be the one the process started on, and the screen, which says the log
// level takes hold as it is saved, would be telling the operator something that
// is only half true: the application lines would follow and the statements gorm
// reports, which is what "debug" is usually turned on for, would not.
type LogLevel struct {
	// value holds a gormlogger.LogLevel, which is an int. It is read and
	// written atomically because the goroutine that runs a query reads it while
	// the goroutine that serves a request to the Settings screen writes it, and
	// those are different goroutines every time.
	value atomic.Int32
}

// NewLogLevel returns a handle set to the level the application names.
func NewLogLevel(level string) *LogLevel {
	handle := &LogLevel{}
	handle.Set(level)

	return handle
}

// Set puts an application log level on the handle. It takes the same names the
// rest of the application uses, so the one place that maps them to what gorm
// understands stays gormLogLevel above.
func (l *LogLevel) Set(level string) {
	l.value.Store(int32(gormLogLevel(level)))
}

// get is what the logger reads before it writes a line.
func (l *LogLevel) get() gormlogger.LogLevel {
	return gormlogger.LogLevel(l.value.Load())
}

// zapGormLogger sends gorm output through zap so that the format, the log file
// and the level are the same as for the rest of the application.
type zapGormLogger struct {
	logger *zap.Logger
	// level is what this logger writes at when it is pinned to one level, and
	// shared is the handle it follows when it is not. A logger that carries a
	// handle reads the level off it and leaves level alone.
	//
	// Both are here because the two kinds of logger want different things. The
	// one the database is opened with has to follow the stored setting, which
	// changes while the process runs. The one LogMode hands back was asked for
	// at a level by the caller (db.Debug() is that call), and it has to stay
	// there: a session that asked to see its statements must not go quiet
	// because the stored setting says "info".
	level  gormlogger.LogLevel
	shared *LogLevel
}

// currentLevel is what this logger writes at right now. Every branch below
// reads it through here rather than off the struct, so that a logger following
// the handle picks up a level that was stored a moment ago, and it is read once
// per call so that one line is not written against two different levels.
func (l *zapGormLogger) currentLevel() gormlogger.LogLevel {
	if l.shared != nil {
		return l.shared.get()
	}

	return l.level
}

// LogMode hands back a logger pinned to level and leaves this one as it was.
// gorm calls it per session, so a logger that changed itself would drag the
// level of one session into every other one.
//
// The copy drops the handle on purpose. It was asked for at a level, and
// following the stored setting afterwards would take away the very thing the
// caller asked for. The original keeps the handle, so what is stored still
// reaches everything that did not ask for a level of its own.
func (l *zapGormLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	newLogger := *l
	newLogger.level = level
	newLogger.shared = nil

	return &newLogger
}

func (l *zapGormLogger) Info(_ context.Context, msg string, data ...interface{}) {
	if l.currentLevel() >= gormlogger.Info {
		l.logger.Info(fmt.Sprintf(msg, data...))
	}
}

func (l *zapGormLogger) Warn(_ context.Context, msg string, data ...interface{}) {
	if l.currentLevel() >= gormlogger.Warn {
		l.logger.Warn(fmt.Sprintf(msg, data...))
	}
}

func (l *zapGormLogger) Error(_ context.Context, msg string, data ...interface{}) {
	if l.currentLevel() >= gormlogger.Error {
		l.logger.Error(fmt.Sprintf(msg, data...))
	}
}

func (l *zapGormLogger) Trace(_ context.Context, begin time.Time, fc func() (string, int64), err error) {
	level := l.currentLevel()

	if level <= gormlogger.Silent {
		return
	}

	elapsed := time.Since(begin)
	fields := func() []zap.Field {
		sql, rows := fc()
		return []zap.Field{
			zap.String("sql", sql),
			zap.Int64("rows", rows),
			zap.Float64("elapsed_ms", float64(elapsed.Nanoseconds())/1e6),
		}
	}

	switch {
	case err != nil && level >= gormlogger.Error && !errors.Is(err, gormlogger.ErrRecordNotFound):
		l.logger.Error("query failed", append(fields(), zap.Error(err))...)
	case elapsed > slowQueryThreshold && level >= gormlogger.Warn:
		l.logger.Warn("slow query", append(fields(),
			zap.Float64("slow_threshold_ms", float64(slowQueryThreshold.Nanoseconds())/1e6))...)
	case level >= gormlogger.Info:
		l.logger.Debug("query", fields()...)
	}
}

// busyTimeout is how long a statement waits for the database file to be free
// before it gives up with SQLITE_BUSY. SQLite refuses a busy file on the spot
// unless it is told to wait, and the caller sees "database is locked" rather
// than a slow request. Within this process the single connection below already
// puts the statements in a queue, so what the wait covers is another process
// holding the file: a second copy of tunnel-manager started by mistake, or a
// backup reading it. Five seconds is long enough for either to let go of a
// database that holds a few dozen rows, and short enough that a file held for
// good is reported instead of the request hanging.
const busyTimeout = 5 * time.Second

// sqliteDSN assembles what the driver is opened with. It is kept apart from the
// open so that the parameters can be read back without touching a file.
//
// journal_mode(WAL) is the mode this setup was measured in. A reader and the
// writer do not shut each other out in it, which matters because the reconcile
// loop reads the Hosts on every pass while a request is writing one, and the
// log it writes ahead survives a process that is killed mid-write.
//
// Both are set through _pragma parameters, which the driver runs on every
// connection it opens. busy_timeout has to be set that way because it is a
// property of the connection rather than of the file. journal_mode is stored in
// the file once, but it is written here as well so that a database file created
// elsewhere is put into WAL on the first open rather than staying in the
// rollback journal mode it was made with.
func sqliteDSN(path string) string {
	return fmt.Sprintf("%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)",
		path, busyTimeout.Milliseconds())
}

// NewDatabase opens the database file and hands back the handle its logger
// follows along with it. The caller holds that handle so that the level can be
// put right once the stored settings are read, which is after this returns:
// what opens the database cannot read a setting that is inside it.
func NewDatabase(path string, logger *zap.Logger, logLevel string) (*gorm.DB, *LogLevel, error) {
	// The path is resolved once and everything below uses the result. The
	// configured value may be relative, and a relative path is read against the
	// working directory, which differs between running from the repository,
	// from the container and from systemd, so the file that was opened is only
	// named by the absolute form.
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to resolve the database path %q: %w", path, err)
	}

	// SQLite creates the database file but not the directories above it, so a
	// first startup against a path that does not exist yet would fail to open
	// with nothing created.
	err = os.MkdirAll(filepath.Dir(absPath), 0755)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create the database directory %q: %w", filepath.Dir(absPath), err)
	}

	// The level is on the handle, which the logger reads before every line, and
	// on the logger as well. The handle is what decides while it is there; the
	// value beside it is what the logger falls back to if it is ever built
	// without one, and starting the process quiet would hide a failed query.
	level := NewLogLevel(logLevel)

	config := &gorm.Config{
		Logger: &zapGormLogger{
			logger: logger.Named("gorm"),
			level:  gormLogLevel(logLevel),
			shared: level,
		},
		NowFunc: func() time.Time {
			return time.Now().UTC()
		},
	}

	db, err := gorm.Open(sqlite.Open(sqliteDSN(absPath)), config)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open the database: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to reach the connection pool: %w", err)
	}

	// One connection is what puts the writes in a queue. SQLite takes one
	// writer at a time and refuses the rest, and SELECT ... FOR UPDATE, which
	// held the write handlers apart under MySQL, is not built into SQLite SQL
	// at all: gorm drops the clause without a word and without an error, so it
	// protected nothing here. With a single connection every statement, from a
	// handler or from the reconcile loop, waits its turn in the pool instead.
	//
	// It is done here rather than around the handlers because the handlers are
	// not the only writers. The reconcile loop and the SSH code write rows of
	// their own, so a lock held in the API would leave those outside it, while
	// a pool of one has no way around it.
	//
	// Reads queue up with the writes, which is affordable: what is stored is a
	// few dozen Hosts and service ports and the queries run over them.
	sqlDB.SetMaxOpenConns(1)

	err = db.AutoMigrate(
		&models.Host{},
		&models.ServicePort{},
		&models.Tunnel{},
		&models.User{},
		&settings.Settings{},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to migrate database: %w", err)
	}

	logger.Info("opened the database", zap.String("path", absPath))

	return db, level, nil
}
