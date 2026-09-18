package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/models"
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

// zapGormLogger sends gorm output through zap so that the format, the log file
// and the level are the same as for the rest of the application.
type zapGormLogger struct {
	logger *zap.Logger
	level  gormlogger.LogLevel
}

func (l *zapGormLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	newLogger := *l
	newLogger.level = level
	return &newLogger
}

func (l *zapGormLogger) Info(_ context.Context, msg string, data ...interface{}) {
	if l.level >= gormlogger.Info {
		l.logger.Info(fmt.Sprintf(msg, data...))
	}
}

func (l *zapGormLogger) Warn(_ context.Context, msg string, data ...interface{}) {
	if l.level >= gormlogger.Warn {
		l.logger.Warn(fmt.Sprintf(msg, data...))
	}
}

func (l *zapGormLogger) Error(_ context.Context, msg string, data ...interface{}) {
	if l.level >= gormlogger.Error {
		l.logger.Error(fmt.Sprintf(msg, data...))
	}
}

func (l *zapGormLogger) Trace(_ context.Context, begin time.Time, fc func() (string, int64), err error) {
	if l.level <= gormlogger.Silent {
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
	case err != nil && l.level >= gormlogger.Error && !errors.Is(err, gormlogger.ErrRecordNotFound):
		l.logger.Error("query failed", append(fields(), zap.Error(err))...)
	case elapsed > slowQueryThreshold && l.level >= gormlogger.Warn:
		l.logger.Warn("slow query", append(fields(),
			zap.Float64("slow_threshold_ms", float64(slowQueryThreshold.Nanoseconds())/1e6))...)
	case l.level >= gormlogger.Info:
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

func NewDatabase(path string, logger *zap.Logger, logLevel string) (*gorm.DB, error) {
	// The path is resolved once and everything below uses the result. The
	// configured value may be relative, and a relative path is read against the
	// working directory, which differs between running from the repository,
	// from the container and from systemd, so the file that was opened is only
	// named by the absolute form.
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve the database path %q: %w", path, err)
	}

	// SQLite creates the database file but not the directories above it, so a
	// first startup against a path that does not exist yet would fail to open
	// with nothing created.
	err = os.MkdirAll(filepath.Dir(absPath), 0755)
	if err != nil {
		return nil, fmt.Errorf("failed to create the database directory %q: %w", filepath.Dir(absPath), err)
	}

	config := &gorm.Config{
		Logger: &zapGormLogger{
			logger: logger.Named("gorm"),
			level:  gormLogLevel(logLevel),
		},
		NowFunc: func() time.Time {
			return time.Now().UTC()
		},
	}

	db, err := gorm.Open(sqlite.Open(sqliteDSN(absPath)), config)
	if err != nil {
		return nil, fmt.Errorf("failed to open the database: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("failed to reach the connection pool: %w", err)
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
	)
	if err != nil {
		return nil, fmt.Errorf("failed to migrate database: %w", err)
	}

	logger.Info("opened the database", zap.String("path", absPath))

	return db, nil
}
