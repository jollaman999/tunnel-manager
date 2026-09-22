package database

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/settings"
	"github.com/jollaman999/tunnel-manager/internal/tlsserve"
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
		l.logger.Info(fmt.Sprintf(msg, data...), logid.DatabaseGormMessage.Field())
	}
}

func (l *zapGormLogger) Warn(_ context.Context, msg string, data ...interface{}) {
	if l.currentLevel() >= gormlogger.Warn {
		l.logger.Warn(fmt.Sprintf(msg, data...), logid.DatabaseGormMessage.Field())
	}
}

func (l *zapGormLogger) Error(_ context.Context, msg string, data ...interface{}) {
	if l.currentLevel() >= gormlogger.Error {
		l.logger.Error(fmt.Sprintf(msg, data...), logid.DatabaseGormMessage.Field())
	}
}

// redactedStatement is what the log carries in place of a statement that
// touches the account table.
//
// The whole statement is thrown away rather than the values picked out of it,
// because what has to be kept out of the log is the password hash and there is
// no reliable way to tell which of the rendered values is the hash: gorm hands
// the logger one string with the bound values already written into it
// (Dialector.Explain), so picking a value out means parsing SQL, and a parser
// that is wrong once writes the hash to a file that is read back through
// GET /api/logs. What a redacted line loses is which column of the one-row
// account table was being written, which is worth less than the hash is
// dangerous. Everything the line is otherwise read for - the ID, the row
// count, how long it took and the error - is still beside it, so a failed or
// slow write to the account table is still reported as one.
const redactedStatement = "<redacted: statement on the user table>"

// userTableStatement matches a statement that names the account table.
//
// The table is "user" rather than the "users" gorm would pluralize it to; the
// name is pinned by models.User.TableName, and the driver quotes every
// identifier with backticks. The name is only looked for where a table name
// can stand, after the keywords that introduce one, because the bare word
// appears elsewhere as well: models.Host has a User field, so a statement on
// the hosts table carries a `user` column, and matching the word anywhere
// would redact the host statements too. Those are what an operator turns
// tracing on to read.
//
// The quoting alternatives are there so that a statement written by hand
// rather than built by gorm is caught as well. The unquoted alternative is
// last, as the quoted forms are what the driver produces.
var userTableStatement = regexp.MustCompile("(?i)\\b(?:from|into|update|join|table)\\s+(?:`user`|\"user\"|\\[user\\]|user\\b)")

// maskStatement hides a statement that touches the account table and hands
// every other one back as it is.
//
// It is applied to what the logger writes rather than to what gorm builds,
// because the statement has to stay whole on its way to SQLite. Only the
// copy that goes into the log is replaced.
func maskStatement(sql string) string {
	if userTableStatement.MatchString(sql) {
		return redactedStatement
	}

	return sql
}

func (l *zapGormLogger) Trace(_ context.Context, begin time.Time, fc func() (string, int64), err error) {
	level := l.currentLevel()

	if level <= gormlogger.Silent {
		return
	}

	elapsed := time.Since(begin)
	// The ID goes in here rather than beside the message, because every branch
	// below hands the fields on as a slice and a field written beside the
	// message would be one argument too many for that.
	fields := func(id logid.ID) []zap.Field {
		sql, rows := fc()
		return []zap.Field{
			id.Field(),
			zap.String("sql", maskStatement(sql)),
			zap.Int64("rows", rows),
			zap.Float64("elapsed_ms", float64(elapsed.Nanoseconds())/1e6),
		}
	}

	switch {
	case err != nil && level >= gormlogger.Error && !errors.Is(err, gormlogger.ErrRecordNotFound):
		l.logger.Error("query failed", append(fields(logid.DatabaseQueryFailed), zap.Error(err))...)
	case elapsed > slowQueryThreshold && level >= gormlogger.Warn:
		l.logger.Warn("slow query", append(fields(logid.DatabaseQuerySlow),
			zap.Float64("slow_threshold_ms", float64(slowQueryThreshold.Nanoseconds())/1e6))...)
	case level >= gormlogger.Info:
		l.logger.Debug("query", fields(logid.DatabaseQuery)...)
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

// databaseDirMode and databaseFileMode are what the database file and the
// directory holding it are left at.
//
// The file holds the bcrypt hash of the account password, the registered Hosts
// with the account names they are reached under, and the certificate this
// installation is served with. Every one of those is readable by any local user
// of the machine while the file is 0644, which is what the sqlite driver
// creates it as, so the file is narrowed to the account this process runs as
// and so is the directory around it. The key file beside it has been created
// this way from the start (internal/crypto), and this is the same rule applied
// to the file that key protects the contents of.
const (
	databaseDirMode  os.FileMode = 0700
	databaseFileMode os.FileMode = 0600
)

// databaseSidecars are the files SQLite keeps beside the database in WAL mode.
// The write-ahead log holds the rows of a transaction that has not been
// checkpointed into the database file yet, and the shared-memory file is the
// index into it, so both carry what the database file carries and are narrowed
// with it.
//
// SQLite makes them with the mode of the database file, so the ones it opens
// from here on follow the file on their own. What is narrowed below are the
// ones an earlier release left lying there, which the driver reuses as they
// are rather than creating again.
var databaseSidecars = []string{"-wal", "-shm"}

// chmod is os.Chmod. It is held in a variable so that a test can put a failure
// in its place: this process owns every file it makes, chmod on a file one owns
// does not fail, and what is worth pinning here is that a mode which cannot be
// set does not stop the startup.
var chmod = os.Chmod

// tightenPermissions takes the group and the rest of the machine off the
// database file, the files SQLite keeps beside it and the directory they are
// in.
//
// It runs on every startup rather than only on the one that creates the file.
// An installation laid down by an earlier release has a 0644 database sitting
// there, and creating new files narrowly would leave exactly the deployments
// that already hold secrets as open as they were.
//
// A file that is not there is passed over. The write-ahead log and the shared
// memory file exist only while a connection is open and are removed when the
// last one closes cleanly, so their absence is the normal state of a database
// nobody is using rather than something to report.
//
// Every path is tried before the failures are handed back together, because
// they fail for the same reason when they fail at all - a file system that
// carries no Unix modes, or files owned by another account - and stopping at
// the first one would name one path while leaving the rest untouched.
//
// On Windows os.Chmod writes no mode. It turns the read-only attribute on for a
// mode with no owner write bit and off for one that has it, so 0600 leaves the
// file writable, which is what a file this process writes to has to be. Nothing
// is narrowed there and nothing fails over it either, which is why this is one
// implementation and not a pair of platform files.
func tightenPermissions(absPath string) error {
	var problems []error

	dir := filepath.Dir(absPath)

	err := chmod(dir, databaseDirMode)
	if err != nil {
		problems = append(problems, fmt.Errorf("failed to set the permission of the database directory %q: %w",
			dir, err))
	}

	paths := make([]string, 0, len(databaseSidecars)+1)
	paths = append(paths, absPath)

	for _, sidecar := range databaseSidecars {
		paths = append(paths, absPath+sidecar)
	}

	for _, path := range paths {
		err := chmod(path, databaseFileMode)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			problems = append(problems, fmt.Errorf("failed to set the permission of %q: %w", path, err))
		}
	}

	return errors.Join(problems...)
}

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

// assignmentBatchSize is how many assignments go into one INSERT. Writing them
// a row at a time would be one statement per pair, and the pairs multiply:
// every Host of an installation against every service port of it. SQLite also
// binds a limited number of parameters per statement, so they cannot all go in
// one either, and this size is well inside that limit at three parameters a
// row.
const assignmentBatchSize = 200

// fillHostServicePorts writes the assignment an installation was already
// running on: every service port on every Host. Until the table existed that
// combination was not stored anywhere, it was what the reconcile loop assumed,
// so an upgrade that left the table empty would be read as "no Host carries
// anything" and would take every tunnel down.
//
// It is called on the startup that creates the table and on no other. Running
// it again on a later startup would put back the assignments the operator has
// since removed, which is the whole of what the table is for.
//
// A Host that is disabled is filled in as well. Which Hosts run tunnels is
// decided where they are reconciled, and a Host left without assignments here
// would come back from being enabled with no tunnels at all.
func fillHostServicePorts(db *gorm.DB, logger *zap.Logger) error {
	var hostIDs []uint
	err := db.Model(&models.Host{}).Pluck("id", &hostIDs).Error
	if err != nil {
		return fmt.Errorf("failed to read the hosts to assign the service ports to: %w", err)
	}

	var spIDs []uint
	err = db.Model(&models.ServicePort{}).Pluck("id", &spIDs).Error
	if err != nil {
		return fmt.Errorf("failed to read the service ports to assign: %w", err)
	}

	// A fresh install has neither, and there is nothing to say about it. The
	// assignments of what does not exist yet are made as it is created.
	if len(hostIDs) == 0 || len(spIDs) == 0 {
		return nil
	}

	assignments := make([]models.HostServicePort, 0, len(hostIDs)*len(spIDs))
	for _, hostID := range hostIDs {
		for _, spID := range spIDs {
			assignments = append(assignments, models.HostServicePort{HostID: hostID, SPID: spID})
		}
	}

	err = db.CreateInBatches(assignments, assignmentBatchSize).Error
	if err != nil {
		return fmt.Errorf("failed to store the service port assignments: %w", err)
	}

	logger.Info("assigned every service port to every host, as the installation was running before they were stored",
		logid.DatabaseServicePortsFilled.Field(),
		zap.Int("hosts", len(hostIDs)),
		zap.Int("service_ports", len(spIDs)),
		zap.Int("assignments", len(assignments)))

	return nil
}

// hostBindAddressColumn is the column the release before this one kept the
// answer in, back when it was one answer for the whole Host. It is written out
// here rather than read off models.Host, which no longer carries the field:
// gorm adds columns and never removes them, so the column is what a running
// installation of that release holds and this is the only name left for it.
//
// bindScopeColumn is where the answer lives now, on the assignment. Whether it
// is already there is how an upgrade is told from a restart.
const (
	hostBindAddressColumn = "bind_address"
	bindScopeColumn       = "bind_scope"
)

// fillBindScopes carries the answer of the release before this one onto the
// assignments of the Host it was stored on.
//
// It runs on the startup that adds the column to the assignments and on no
// other. A later pass would write the Host-wide answer over whatever has been
// chosen per assignment since, and the whole point of the column is that the
// assignments of one Host may differ.
//
// Only the Hosts that were bound to a loopback address are written. Every
// other answer, the empty one and an address of some interface of the machine
// among them, comes out as the wildcard, which is what the empty value in the
// new column already means, so those rows are left as they are. What must not
// happen is the other direction: a Host that was pinned to loopback coming back
// from an upgrade open to everything, which is reach handed out by a startup
// rather than by a person.
func fillBindScopes(db *gorm.DB) error {
	type hostBindAddress struct {
		ID          uint
		BindAddress string
	}

	var stored []hostBindAddress
	err := db.Model(&models.Host{}).Select("id", hostBindAddressColumn).Scan(&stored).Error
	if err != nil {
		return fmt.Errorf("failed to read the bind addresses of the hosts: %w", err)
	}

	loopback := make([]uint, 0, len(stored))
	for _, host := range stored {
		address := net.ParseIP(host.BindAddress)
		if address == nil || !address.IsLoopback() {
			continue
		}

		loopback = append(loopback, host.ID)
	}

	if len(loopback) == 0 {
		return nil
	}

	err = db.Model(&models.HostServicePort{}).Where("host_id IN ?", loopback).
		Update(bindScopeColumn, models.BindScopeLoopback).Error
	if err != nil {
		return fmt.Errorf("failed to carry the bind addresses onto the service port assignments: %w", err)
	}

	return nil
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
	err = os.MkdirAll(filepath.Dir(absPath), databaseDirMode)
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

	// Whether the assignments are already stored is asked before AutoMigrate
	// runs, because afterwards the answer is yes on every startup: AutoMigrate
	// creates the table and says nothing about having done so. What the answer
	// decides is below.
	hadAssignments := db.Migrator().HasTable(&models.HostServicePort{})

	// Where the bind scope has to come from is asked here for the same reason.
	// After AutoMigrate the assignments carry the column whatever the file held
	// a moment ago, so this is the last point at which an installation coming
	// from the release that stored the answer on the Host can be told from one
	// that has been running with the column for a while. An installation from
	// before that release has no such column to read and is passed over here.
	carriesHostBindAddress := db.Migrator().HasColumn(&models.Host{}, hostBindAddressColumn) &&
		!db.Migrator().HasColumn(&models.HostServicePort{}, bindScopeColumn)

	err = db.AutoMigrate(
		&models.Host{},
		&models.ServicePort{},
		&models.HostServicePort{},
		&models.Tunnel{},
		&models.User{},
		&settings.Settings{},
		&tlsserve.Certificate{},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to migrate database: %w", err)
	}

	// The mode is set once the migration has run, so that the write-ahead log
	// and the shared memory file the driver made along the way are narrowed
	// with the database file rather than left behind by a pass that ran before
	// they existed.
	//
	// What it reports is dropped on purpose, and this is the whole of the
	// handling. A mode that cannot be set is not a reason to refuse to serve:
	// the database opened, every Host in it can be reached, and an installation
	// on a file system that carries no Unix modes would otherwise stop coming
	// up over a file it has no way of narrowing. It is not logged either, since
	// a line of this application carries an identifier the Logs screen
	// translates it by and there is no identifier for this yet.
	_ = tightenPermissions(absPath)

	if !hadAssignments {
		err = fillHostServicePorts(db, logger)
		if err != nil {
			return nil, nil, err
		}
	}

	// This follows the fill above rather than leading it, so that assignments
	// made a moment ago by an upgrade are given the answer of their Host as
	// well. An installation that reaches both of these is one that stored
	// neither the assignments nor a scope, and its tunnels are as open as its
	// Hosts were.
	if carriesHostBindAddress {
		err = fillBindScopes(db)
		if err != nil {
			return nil, nil, err
		}
	}

	logger.Info("opened the database", logid.DatabaseOpened.Field(), zap.String("path", absPath))

	return db, level, nil
}
