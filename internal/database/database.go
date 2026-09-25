package database

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/crypto"
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

// Files returns the database file at absPath and the files SQLite keeps beside
// it, which hold what the database file holds.
func Files(absPath string) []string {
	paths := make([]string, 0, len(databaseSidecars)+1)
	paths = append(paths, absPath)

	for _, sidecar := range databaseSidecars {
		paths = append(paths, absPath+sidecar)
	}

	return paths
}

// chmod is os.Chmod. It is held in a variable so that a test can put a failure
// in its place: this process owns every file it makes, chmod on a file one owns
// does not fail, and what is worth pinning here is that a mode which cannot be
// set does not stop the startup.
var chmod = os.Chmod

// tightenPermissions takes the group and the rest of the machine off the
// database file and the files SQLite keeps beside it, and off the directory
// they are in when madeDir says this startup made it.
//
// It runs on every startup rather than only on the one that creates the file.
// An installation laid down by an earlier release has a 0644 database sitting
// there, and creating new files narrowly would leave exactly the deployments
// that already hold secrets as open as they were.
//
// The directory is set only when this program made it, because one that was
// already there may be shared with whatever else the operator keeps in it.
// Pointing the database into /tmp would otherwise turn /tmp into 0700 and drop
// the sticky bit that keeps one user from removing the files of another. The
// secrets are in the files, which are narrowed wherever they are, so what a
// directory left as it was still shows is the names of the files in it.
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
func tightenPermissions(absPath string, madeDir bool) error {
	var problems []error

	if madeDir {
		// MkdirAll wrote the mode through the umask, which may have taken
		// owner bits away as well, so it is written again here.
		dir := filepath.Dir(absPath)

		err := chmod(dir, databaseDirMode)
		if err != nil {
			problems = append(problems, fmt.Errorf("failed to set the permission of the database directory %q: %w",
				dir, err))
		}
	}

	for _, path := range Files(absPath) {
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
			assignments = append(assignments, models.HostServicePort{HostID: hostID, SPID: spID, Enabled: true})
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

// assignmentEnabledColumn is the column that says whether the tunnel of an
// assignment runs.
const assignmentEnabledColumn = "enabled"

// fillAssignmentsEnabled switches on every assignment that holds nothing in
// the column that says whether it runs. Each of them was running: an
// assignment written before the column existed is one AutoMigrate filled with
// NULL, and so is one written since by a release that does not know the
// column, which is what an installation taken back a release and brought
// forward again holds. NULL reads back as false, and left as it is it would
// take those tunnels down on a startup nobody asked for that.
//
// It runs on every startup rather than on the one that adds the column, which
// is where it differs from fillLocalForwardsEnabled. What it reaches is NULL
// and nothing else, and nothing this release writes is NULL: an assignment
// switched off holds false, so a restart leaves it off. The rows a downgraded
// release wrote are what a pass on the upgrade alone would miss.
func fillAssignmentsEnabled(db *gorm.DB) error {
	err := db.Model(&models.HostServicePort{}).Where(assignmentEnabledColumn+" IS NULL").
		Update(assignmentEnabledColumn, true).Error
	if err != nil {
		return fmt.Errorf("failed to switch on the service port assignments stored before they could be switched off: %w", err)
	}

	return nil
}

// localForwardsTable is the table the local forwards are stored in. It is
// spelled out because the rebuild below names it in SQL of its own, where
// there is no model for gorm to take the name off.
const localForwardsTable = "local_forwards"

// localForwardEnabledColumn is the column that says whether a local forward
// runs. Whether it is already there is how the startup that adds it is told
// from the ones after.
const localForwardEnabledColumn = "enabled"

// fillLocalForwardsEnabled switches on every local forward stored before the
// column existed. Each of them was running, and AutoMigrate leaves NULL in the
// column it adds, which reads back as false and would take them all down on
// the upgrade.
//
// It runs on the startup that adds the column and on no other, so that a
// forward switched off since is not switched back on by a restart.
func fillLocalForwardsEnabled(db *gorm.DB) error {
	err := db.Model(&models.LocalForward{}).Where("1 = 1").
		Update(localForwardEnabledColumn, true).Error
	if err != nil {
		return fmt.Errorf("failed to switch on the local forwards stored before they could be switched off: %w", err)
	}

	return nil
}

// localForwardNumberColumn is the column that says which forward of its Host a
// row is. Whether it is already there is how a database whose local forwards
// are still keyed by one table-wide id is told from one that has been moved
// over.
//
// localForwardsBeforeNumbers is where those rows wait while the table is
// rebuilt around them. It is made and dropped inside one transaction, so
// nothing outside numberLocalForwardsByHost ever sees it.
const (
	localForwardNumberColumn   = "number"
	localForwardsBeforeNumbers = "local_forwards_before_numbers"
)

// numberLocalForwardsByHost rebuilds the local forwards on the key they carry
// now, the Host and the number together, and hands every stored row its
// number: 1 upwards per Host, in the order of the id they had. That order is
// the order they were made in and the order the list has always been drawn in,
// so the number a forward comes out with is the place it was already shown in.
//
// The table is rebuilt rather than altered because SQLite cannot change the
// primary key of a table, and AutoMigrate cannot ask it to: against a table
// still carrying id it would try to add number to rows that exist as a column
// that may not be null, which SQLite refuses outright, and the startup would
// stop there. So this runs before AutoMigrate and creates the new table with
// AutoMigrate itself, which keeps the shape in the model rather than in DDL
// written out a second time here. The pass below then finds the table already
// as the model asks for it and does nothing to it.
//
// The whole of it is one transaction. A rebuild that fails halfway would
// otherwise leave an installation with its forwards in a table it no longer
// opens, so either the rows come out numbered or the table is the one that
// went in. The row count is compared before the old table is dropped for the
// same reason: a copy that lost rows ends the transaction rather than the
// rows.
//
// It is called on the startup that finds the old shape and on no other, and a
// database that is already keyed by the pair is not brought here at all, so
// running the program twice over one file numbers it once.
func numberLocalForwardsByHost(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var before int64
		err := tx.Table(localForwardsTable).Count(&before).Error
		if err != nil {
			return fmt.Errorf("failed to count the local forwards to number: %w", err)
		}

		err = tx.Migrator().RenameTable(localForwardsTable, localForwardsBeforeNumbers)
		if err != nil {
			return fmt.Errorf("failed to set the local forwards aside: %w", err)
		}

		// The indexes came along with the table under their own names, and an
		// index name is held once for the whole database, so the ones the
		// rebuilt table is given would collide with them. They are dropped
		// rather than renamed: the table they are on is dropped a few lines
		// below.
		var indexes []string
		err = tx.Raw("SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = ? AND sql IS NOT NULL",
			localForwardsBeforeNumbers).Scan(&indexes).Error
		if err != nil {
			return fmt.Errorf("failed to read the indexes of the local forwards: %w", err)
		}

		for _, name := range indexes {
			err = tx.Exec(fmt.Sprintf("DROP INDEX %s", quoteIdentifier(name))).Error
			if err != nil {
				return fmt.Errorf("failed to drop an index of the local forwards: %w", err)
			}
		}

		err = tx.AutoMigrate(&models.LocalForward{})
		if err != nil {
			return fmt.Errorf("failed to build the local forwards on the host and the number: %w", err)
		}

		// Every column the rows were stored in that the rebuilt table still
		// has is carried over as it stands, so a value is never read into this
		// process and written back. The columns are asked for rather than
		// listed because what an installation holds depends on the release it
		// is coming from: one from before a column was added does not have it,
		// and a column filled in after this by the passes that follow
		// AutoMigrate is not one to copy.
		columns, err := sharedColumns(tx, localForwardsBeforeNumbers, localForwardsTable)
		if err != nil {
			return err
		}

		if len(columns) == 0 {
			return fmt.Errorf("the stored local forwards have no column in common with the table they move to")
		}

		quoted := make([]string, 0, len(columns))
		for _, name := range columns {
			quoted = append(quoted, quoteIdentifier(name))
		}

		copied := strings.Join(quoted, ", ")
		err = tx.Exec(fmt.Sprintf(
			"INSERT INTO %s (%s, %s) SELECT %s, ROW_NUMBER() OVER (PARTITION BY host_id ORDER BY id) FROM %s",
			quoteIdentifier(localForwardsTable), copied, quoteIdentifier(localForwardNumberColumn),
			copied, quoteIdentifier(localForwardsBeforeNumbers))).Error
		if err != nil {
			return fmt.Errorf("failed to number the local forwards by host: %w", err)
		}

		var after int64
		err = tx.Table(localForwardsTable).Count(&after).Error
		if err != nil {
			return fmt.Errorf("failed to count the numbered local forwards: %w", err)
		}

		if after != before {
			return fmt.Errorf("numbering the local forwards by host moved %d of %d rows", after, before)
		}

		err = tx.Migrator().DropTable(localForwardsBeforeNumbers)
		if err != nil {
			return fmt.Errorf("failed to drop the local forwards that were set aside: %w", err)
		}

		return nil
	})
}

// addressRename is one name an address used to be stored under and the name it
// is stored under now, on the table of model. The same shape carries the
// columns and the unique indexes over them, since an index is named after the
// columns it covers and has to follow them.
type addressRename struct {
	model interface{}
	from  string
	to    string
}

// The columns and indexes that said IP from when an address was the only thing
// they took. They hold a host name as well now, and carry the word for both.
var (
	addressColumnRenames = []addressRename{
		{model: &models.Host{}, from: "ip", to: "address"},
		{model: &models.ServicePort{}, from: "service_ip", to: "service_address"},
		{model: &models.LocalForward{}, from: "target_ip", to: "target_address"},
	}
	addressIndexRenames = []addressRename{
		{model: &models.Host{}, from: "idx_hosts_ip", to: "idx_hosts_address"},
		{model: &models.ServicePort{}, from: "idx_service_ip_port", to: "idx_service_address_port"},
	}
)

// renameAddressColumns moves the addresses of a database written before the
// columns were renamed onto the names the model carries, and the unique
// indexes over them with them.
//
// It runs before AutoMigrate because AutoMigrate cannot rename. Against a table
// still holding the old column it would add the new one beside it as a column
// that may not be null, which SQLite refuses on a table with rows, and a
// startup that got past that would read every address back empty. It also runs
// before numberLocalForwardsByHost, which carries over only the columns the old
// and the rebuilt table share, so a target still under its old name would be
// left behind.
//
// SQLite points an index at the renamed column by itself, so the indexes only
// change name. They are renamed rather than left to AutoMigrate, which would
// build a second unique index under the new name and keep the old one beside
// it.
//
// Each step asks first whether the old name is there and the new one is not,
// so a database that has been through this, or one that was created with the
// new names, is left as it is. The whole of it is one transaction, so a
// failure leaves the names that went in.
func renameAddressColumns(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		migrator := tx.Migrator()

		for _, rename := range addressColumnRenames {
			if !migrator.HasColumn(rename.model, rename.from) || migrator.HasColumn(rename.model, rename.to) {
				continue
			}

			err := migrator.RenameColumn(rename.model, rename.from, rename.to)
			if err != nil {
				return fmt.Errorf("failed to rename the column %s to %s: %w", rename.from, rename.to, err)
			}
		}

		for _, rename := range addressIndexRenames {
			if !migrator.HasIndex(rename.model, rename.from) || migrator.HasIndex(rename.model, rename.to) {
				continue
			}

			err := migrator.RenameIndex(rename.model, rename.from, rename.to)
			if err != nil {
				return fmt.Errorf("failed to rename the index %s to %s: %w", rename.from, rename.to, err)
			}
		}

		return nil
	})
}

// sharedColumns is the columns two tables both have, in the order the first one
// holds them.
func sharedColumns(tx *gorm.DB, table string, other string) ([]string, error) {
	held, err := columnsOf(tx, table)
	if err != nil {
		return nil, err
	}

	against, err := columnsOf(tx, other)
	if err != nil {
		return nil, err
	}

	has := make(map[string]bool, len(against))
	for _, name := range against {
		has[name] = true
	}

	shared := make([]string, 0, len(held))
	for _, name := range held {
		if has[name] {
			shared = append(shared, name)
		}
	}

	return shared, nil
}

// columnsOf is the columns of one table, in the order it holds them.
func columnsOf(tx *gorm.DB, table string) ([]string, error) {
	var names []string

	err := tx.Raw("SELECT name FROM pragma_table_info(?)", table).Scan(&names).Error
	if err != nil {
		return nil, fmt.Errorf("failed to read the columns of %s: %w", table, err)
	}

	return names, nil
}

// quoteIdentifier writes a table or column name the way SQL reads one whatever
// it is spelled with. The names reaching it are the ones this package and the
// model wrote, and the doubling is what keeps that true of a name read back out
// of the database.
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
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
	// with nothing created. On Windows the directories made here are kept to
	// the owner, SYSTEM and Administrators, and what SQLite creates in them
	// takes that on.
	//
	// Whether the directory was there is asked first, because it decides
	// whether tightenPermissions below may set its mode: a directory this
	// program makes is the database's alone, while one that was already there
	// is left as the operator has it.
	_, err = os.Stat(filepath.Dir(absPath))
	madeDir := errors.Is(err, os.ErrNotExist)

	err = crypto.MkdirAllPrivate(filepath.Dir(absPath), databaseDirMode)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create the database directory %q: %w", filepath.Dir(absPath), err)
	}

	// The directory may be one the operator made, and on Windows a file SQLite
	// creates in it takes its DACL, which under C:\ lets every user read it.
	// So the files SQLite would create are made here first, empty and with the
	// DACL of the owner, SYSTEM and Administrators, and SQLite opens them as
	// they are: an empty file is an empty database, an empty write-ahead log
	// has nothing to replay, and SQLite empties the shared memory file itself
	// when it is the first to open it. On Unix this does nothing, and
	// tightenPermissions below narrows the mode instead. A path that holds
	// something other than a file is left for the open to refuse, without
	// files made beside it.
	info, err := os.Stat(absPath)
	if err != nil || info.Mode().IsRegular() {
		for _, path := range Files(absPath) {
			err = crypto.ReservePrivateFile(path)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to create %q: %w", path, err)
			}
		}
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

	// The addresses are moved onto their new names before anything below reads
	// the tables by the model. Why it cannot be left to AutoMigrate is at
	// renameAddressColumns.
	err = renameAddressColumns(db)
	if err != nil {
		return nil, nil, err
	}

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

	// Asked here for the same reason: after AutoMigrate the column is there
	// whatever the file held. A table that is not there yet holds no rows to
	// switch on.
	addsLocalForwardEnabled := db.Migrator().HasTable(&models.LocalForward{}) &&
		!db.Migrator().HasColumn(&models.LocalForward{}, localForwardEnabledColumn)

	// Whether the local forwards are still keyed by one table-wide id is asked
	// here for the same reason, and unlike the answers above it is acted on
	// before AutoMigrate instead of after. AutoMigrate has no way of changing a
	// primary key, so the rows are moved onto the new one first and the pass
	// below finds the table already as the model asks for it. Why it cannot be
	// left to AutoMigrate, and why the move is one transaction, is at
	// numberLocalForwardsByHost.
	if db.Migrator().HasTable(&models.LocalForward{}) &&
		!db.Migrator().HasColumn(&models.LocalForward{}, localForwardNumberColumn) {
		err = numberLocalForwardsByHost(db)
		if err != nil {
			return nil, nil, err
		}
	}

	err = db.AutoMigrate(
		&models.Host{},
		&models.ServicePort{},
		&models.HostServicePort{},
		&models.LocalForward{},
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
	_ = tightenPermissions(absPath, madeDir)

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

	// This follows the fill of the assignments as well. Those are written
	// switched on already, so what it finds is what was stored before.
	err = fillAssignmentsEnabled(db)
	if err != nil {
		return nil, nil, err
	}

	if addsLocalForwardEnabled {
		err = fillLocalForwardsEnabled(db)
		if err != nil {
			return nil, nil, err
		}
	}

	logger.Info("opened the database", logid.DatabaseOpened.Field(), zap.String("path", absPath))

	return db, level, nil
}
