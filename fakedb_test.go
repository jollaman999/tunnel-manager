package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// sqliteVersion is what the fake driver answers the version probe with. On
// being opened the SQLite dialector asks the database for its version, so as to
// know which clauses it may build, and a version below 3.35 would put gorm on
// the path that does not read the generated id back with RETURNING. The number
// is kept above it so that the statements the fake driver is handed are the
// ones a real database would be handed.
const sqliteVersion = "3.41.0"

// versionQuery is the statement the dialector makes for itself. It is answered
// but not recorded: it is the driver asking after itself, not the code under
// test writing a row.
const versionQuery = "select sqlite_version()"

// The database roundtrip of the startup is exercised against a fake sql driver
// that records the statements instead of sending them. It answers the two
// statements the account setup makes: a count that reports an empty table and
// an insert that reports one row.

// recordedStatement is one statement the fake driver was handed.
type recordedStatement struct {
	query string
	args  []driver.NamedValue
}

// statementRecorder collects what the fake driver was handed. The database
// handle is used from one goroutine here, but sql.DB is free to call the driver
// from another, so the access is guarded.
type statementRecorder struct {
	mu         sync.Mutex
	statements []recordedStatement
}

func (r *statementRecorder) record(query string, args []driver.NamedValue) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statements = append(r.statements, recordedStatement{query: query, args: args})
}

func (r *statementRecorder) all() []recordedStatement {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedStatement(nil), r.statements...)
}

type fakeConn struct {
	recorder *statementRecorder
	// rowCount is what the count of the account table answers.
	rowCount int64
}

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("the fake driver does not prepare statements")
}

func (c *fakeConn) Close() error { return nil }

func (c *fakeConn) Begin() (driver.Tx, error) { return fakeTx{}, nil }

func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.recorder.record(query, args)
	return fakeResult{}, nil
}

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if query == versionQuery {
		return &fakeRows{columns: []string{"version"}, values: []driver.Value{sqliteVersion}}, nil
	}

	c.recorder.record(query, args)

	// An insert arrives here rather than at ExecContext, because the SQLite
	// dialector reads the generated id back with RETURNING and that makes the
	// statement a query.
	if strings.Contains(query, "RETURNING") {
		return &fakeRows{columns: []string{"id"}, values: []driver.Value{int64(1)}}, nil
	}

	// The only other query the account setup makes is the count of the table.
	return &fakeRows{columns: []string{"count"}, values: []driver.Value{c.rowCount}}, nil
}

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

type fakeResult struct{}

func (fakeResult) LastInsertId() (int64, error) { return 1, nil }
func (fakeResult) RowsAffected() (int64, error) { return 1, nil }

type fakeRows struct {
	columns []string
	values  []driver.Value
	done    bool
}

func (r *fakeRows) Columns() []string { return r.columns }

func (r *fakeRows) Close() error { return nil }

func (r *fakeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	copy(dest, r.values)
	r.done = true
	return nil
}

// fakeConnector hands out connections that report to the same recorder and
// answer the count with the same number.
type fakeConnector struct {
	recorder *statementRecorder
	rowCount int64
}

func (c *fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeConn{recorder: c.recorder, rowCount: c.rowCount}, nil
}

func (c *fakeConnector) Driver() driver.Driver { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, fmt.Errorf("the fake driver is opened through its connector")
}

// newRecordingDB returns a database handle over the fake driver, whose count
// answers rowCount, and the recorder that holds what it was handed.
func newRecordingDB(t *testing.T, rowCount int64) (*sql.DB, *statementRecorder) {
	t.Helper()

	recorder := &statementRecorder{}
	db := sql.OpenDB(&fakeConnector{recorder: recorder, rowCount: rowCount})

	t.Cleanup(func() {
		_ = db.Close()
	})

	return db, recorder
}
