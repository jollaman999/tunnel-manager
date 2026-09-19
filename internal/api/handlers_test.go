package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/go-playground/validator/v10"
	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/crypto/ssh"
	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
	gormlogger "gorm.io/gorm/logger"
)

func newTestCipher(t *testing.T) *crypto.Cipher {
	t.Helper()

	key, err := crypto.LoadOrCreateKey(filepath.Join(t.TempDir(), "test.key"))
	if err != nil {
		t.Fatalf("failed to create a key: %v", err)
	}

	c, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("failed to create a cipher: %v", err)
	}

	return c
}

var errQueryFailed = errors.New("query failed")

// txConnPool stands in for a real transaction. Writes succeed, reads fail, and
// Commit/Rollback calls are counted. A tunnel that is running writes from a
// goroutine of its own, so the counters are guarded.
type txConnPool struct {
	mu        sync.Mutex
	commits   int
	rollbacks int
}

// counts reports how often the transaction was committed and rolled back.
func (p *txConnPool) counts() (commits, rollbacks int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.commits, p.rollbacks
}

func (p *txConnPool) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return nil, errQueryFailed
}

func (p *txConnPool) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return fakeResult{}, nil
}

func (p *txConnPool) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	// An INSERT arrives here rather than at ExecContext, because the SQLite
	// dialector reads the generated id back with RETURNING and that makes the
	// statement a query. The one column the clause names is handed back from a
	// real database, since a *sql.Rows holds nothing exported and cannot be
	// built by hand. Everything else is a read, which fails on purpose.
	if strings.Contains(query, "RETURNING `id`") {
		return sqliteVersionDB.QueryContext(ctx, "SELECT 1 AS id")
	}

	return nil, errQueryFailed
}

func (p *txConnPool) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return &sql.Row{}
}

func (p *txConnPool) Commit() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.commits++
	return nil
}

func (p *txConnPool) Rollback() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.rollbacks++
	return nil
}

// rootConnPool hands out txConnPool on Begin and fails every direct statement,
// so a query issued outside the transaction can be told apart from one issued
// inside it.
// sqliteVersionDB answers the one statement the SQLite dialector runs for
// itself: on being opened it asks the database for its version, so as to know
// which clauses it may build. The pools here answer gorm callbacks rather than
// SQL and have no database behind them, so that probe is sent to a real
// in-memory one instead. It takes a database because a *sql.Row holds nothing
// exported and can only come from one.
var sqliteVersionDB = func() *sql.DB {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		panic(fmt.Sprintf("failed to open the database the version probe is answered from: %v", err))
	}

	return db
}()

type rootConnPool struct {
	tx *txConnPool
}

func (p *rootConnPool) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return nil, errQueryFailed
}

func (p *rootConnPool) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return nil, errQueryFailed
}

func (p *rootConnPool) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return nil, errQueryFailed
}

func (p *rootConnPool) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return sqliteVersionDB.QueryRowContext(ctx, query, args...)
}

func (p *rootConnPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	return p.tx, nil
}

type fakeResult struct{}

func (fakeResult) LastInsertId() (int64, error) { return 1, nil }

func (fakeResult) RowsAffected() (int64, error) { return 1, nil }

type testValidator struct {
	validator *validator.Validate
}

func (v *testValidator) Validate(i interface{}) error {
	return v.validator.Struct(i)
}

func newTxRecordingDB(t *testing.T) (*gorm.DB, *txConnPool) {
	t.Helper()

	tx := &txConnPool{}
	db, err := gorm.Open(sqlite.Dialector{Conn: &rootConnPool{tx: tx}}, &gorm.Config{
		Logger:               gormlogger.Discard,
		DisableAutomaticPing: true,
	})
	if err != nil {
		t.Fatalf("failed to open gorm with recording conn pool: %v", err)
	}

	return db, tx
}

// wakeRecorder stands in for the tunnel manager. It counts the reconcile
// wake-ups the handlers ask for and remembers how many commits had gone through
// when the first one arrived, so a wake-up that was sent before the transaction
// was committed can be told apart from one sent after it.
type wakeRecorder struct {
	tx *txConnPool

	mu                 sync.Mutex
	wakes              int
	commitsAtFirstWake int
}

func (r *wakeRecorder) WakeReconcile() {
	commits, _ := r.tx.counts()

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.wakes == 0 {
		r.commitsAtFirstWake = commits
	}
	r.wakes++
}

// counts reports how many wake-ups arrived and how many commits had gone
// through when the first one did.
func (r *wakeRecorder) counts() (wakes, commitsAtFirstWake int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.wakes, r.commitsAtFirstWake
}

func (r *wakeRecorder) DesiredTunnelCount() (int, error) {
	return 0, errQueryFailed
}

func (r *wakeRecorder) GetAllTunnels() (*[]models.Tunnel, error) {
	return nil, errQueryFailed
}

func (r *wakeRecorder) GetHostTunnels(hostID uint) (*[]models.Tunnel, error) {
	return nil, errQueryFailed
}

// newWriteStubDB returns a gorm DB that answers the reads of the write handlers
// from memory. The writes go through the recording transaction, so what a
// request committed can be read off the counters.
func newWriteStubDB(t *testing.T) (*gorm.DB, *txConnPool) {
	t.Helper()

	db, txPool := newTxRecordingDB(t)

	err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		switch dest := tx.Statement.Dest.(type) {
		case *models.Host:
			*dest = models.Host{ID: 1, IP: "192.0.2.1", Port: 22, User: "root", Enabled: true}
		case *models.ServicePort:
			*dest = models.ServicePort{ID: 2, ServiceIP: "192.0.2.2", ServicePort: 8081, LocalPort: 18081}
		}
		tx.RowsAffected = 1
	})
	if err != nil {
		t.Fatalf("failed to replace the query callback: %v", err)
	}

	return db, txPool
}

// TestWriteHandlersWakeTheReconcileLoopAfterTheCommit pins down that every
// handler that writes rows asks for a reconcile pass, and that it asks once the
// transaction is through. A pass that runs before the commit reads the state as
// it was and leaves the tunnels of the rows the request wrote alone.
func TestWriteHandlersWakeTheReconcileLoopAfterTheCommit(t *testing.T) {
	const hostBody = `{"ip":"192.0.2.1","port":22,"user":"root","password":"fake-value-1"}` // hook:allow
	const servicePortBody = `{"service_ip":"192.0.2.2","service_port":80,"local_port":8080}`

	tests := []struct {
		name   string
		method string
		target string
		body   string
		param  string
		status int
		call   func(*Handler, echo.Context) error
	}{
		{
			name:   "create host",
			method: http.MethodPost,
			target: "/api/host",
			body:   hostBody,
			status: http.StatusCreated,
			call:   (*Handler).CreateHost,
		},
		{
			name:   "update host",
			method: http.MethodPut,
			target: "/api/host/1",
			body:   `{"description":"new","enabled":true}`,
			param:  "1",
			status: http.StatusOK,
			call:   (*Handler).UpdateHost,
		},
		{
			name:   "delete host",
			method: http.MethodDelete,
			target: "/api/host/1",
			param:  "1",
			status: http.StatusOK,
			call:   (*Handler).DeleteHost,
		},
		{
			name:   "create service port",
			method: http.MethodPost,
			target: "/api/service-port",
			body:   servicePortBody,
			status: http.StatusCreated,
			call:   (*Handler).CreateServicePort,
		},
		{
			name:   "update service port",
			method: http.MethodPut,
			target: "/api/service-port/2",
			body:   servicePortBody,
			param:  "2",
			status: http.StatusOK,
			call:   (*Handler).UpdateServicePort,
		},
		{
			name:   "delete service port",
			method: http.MethodDelete,
			target: "/api/service-port/2",
			param:  "2",
			status: http.StatusOK,
			call:   (*Handler).DeleteServicePort,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, txPool := newWriteStubDB(t)
			manager := &wakeRecorder{tx: txPool}

			e := echo.New()
			e.Validator = &testValidator{validator: validator.New()}
			req := httptest.NewRequest(tt.method, tt.target, strings.NewReader(tt.body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			if tt.param != "" {
				c.SetParamNames("id")
				c.SetParamValues(tt.param)
			}

			h := NewHandler(db, manager, zap.NewNop(), newTestCipher(t))

			err := tt.call(h, c)
			if err != nil {
				t.Fatalf("the handler returned an error: %v", err)
			}
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, tt.status, rec.Body.String())
			}

			commits, rollbacks := txPool.counts()
			if commits != 1 || rollbacks != 0 {
				t.Fatalf("commits = %d, rollbacks = %d, want 1 and 0", commits, rollbacks)
			}

			wakes, commitsAtFirstWake := manager.counts()
			if wakes != 1 {
				t.Fatalf("reconcile wake-ups = %d, want 1", wakes)
			}
			if commitsAtFirstWake != 1 {
				t.Fatalf("commits at the wake-up = %d, want 1: the loop was woken before the transaction was committed", commitsAtFirstWake)
			}
		})
	}
}

// newFailingCreateDB returns a gorm DB whose reads are answered from memory and
// whose creates fail.
func newFailingCreateDB(t *testing.T) (*gorm.DB, *txConnPool) {
	t.Helper()

	db, txPool := newWriteStubDB(t)

	err := db.Callback().Create().Replace("gorm:create", func(tx *gorm.DB) {
		_ = tx.AddError(errQueryFailed)
	})
	if err != nil {
		t.Fatalf("failed to replace the create callback: %v", err)
	}

	return db, txPool
}

// TestCreateServicePortDoesNotWakeTheLoopOnRollback pins down that a request
// that stored nothing does not ask for a reconcile pass either.
func TestCreateServicePortDoesNotWakeTheLoopOnRollback(t *testing.T) {
	db, txPool := newFailingCreateDB(t)
	manager := &wakeRecorder{tx: txPool}

	e := echo.New()
	e.Validator = &testValidator{validator: validator.New()}
	body := `{"service_ip":"192.0.2.1","service_port":80,"local_port":8080}`
	req := httptest.NewRequest(http.MethodPost, "/api/service-port", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	h := NewHandler(db, manager, zap.NewNop(), newTestCipher(t))

	err := h.CreateServicePort(c)
	if err != nil {
		t.Fatalf("CreateServicePort returned error: %v", err)
	}

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Failed to create service port") {
		t.Fatalf("body = %s, want a service port creation failure", rec.Body.String())
	}

	commits, rollbacks := txPool.counts()
	if rollbacks != 1 {
		t.Errorf("rollbacks = %d, want 1", rollbacks)
	}
	if commits != 0 {
		t.Errorf("commits = %d, want 0", commits)
	}

	wakes, _ := manager.counts()
	if wakes != 0 {
		t.Errorf("reconcile wake-ups = %d, want 0", wakes)
	}
}

// newReconcileStubDB returns a gorm DB that answers both the write of
// CreateServicePort and the reads of a reconcile pass from memory. The service
// port row is created with spID, and the tunnel row a pass would write is
// reported as already stored.
func newReconcileStubDB(t *testing.T, hosts []models.Host, sps []models.ServicePort, spID uint) (*gorm.DB, *txConnPool) {
	t.Helper()

	db, txPool := newTxRecordingDB(t)

	err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		switch dest := tx.Statement.Dest.(type) {
		case *[]models.Host:
			*dest = hosts
			tx.RowsAffected = int64(len(hosts))
		case *[]models.ServicePort:
			*dest = sps
			tx.RowsAffected = int64(len(sps))
		case *models.Tunnel:
			tx.RowsAffected = 1
		}
	})
	if err != nil {
		t.Fatalf("failed to replace the query callback: %v", err)
	}

	err = db.Callback().Create().Replace("gorm:create", func(tx *gorm.DB) {
		if dest, ok := tx.Statement.Dest.(*models.ServicePort); ok {
			dest.ID = spID
		}
		tx.RowsAffected = 1
	})
	if err != nil {
		t.Fatalf("failed to replace the create callback: %v", err)
	}

	return db, txPool
}

// TestCreateServicePortIsCreatedWhenNoTunnelCanBeStarted pins down what the
// reconcile loop changed about this API: the answer reports that the row was
// stored, and a tunnel that cannot be built does not turn it into a failure.
// Before, one Host that could not be reached rolled the row back and answered
// 500.
func TestCreateServicePortIsCreatedWhenNoTunnelCanBeStarted(t *testing.T) {
	cipher := newTestCipher(t)
	otherCipher := newTestCipher(t)

	// The stored password is sealed with another key, so a reconcile pass gives
	// up on the tunnel before it dials anything.
	password, err := otherCipher.Encrypt("fake-value-1") // hook:allow
	if err != nil {
		t.Fatalf("failed to encrypt: %v", err)
	}

	const spID uint = 7
	hosts := []models.Host{{ID: 1, IP: "127.0.0.1", Port: 1, User: "root", Password: password, Enabled: true}}
	sps := []models.ServicePort{{ID: spID, ServiceIP: "192.0.2.2", ServicePort: 80, LocalPort: 8080}}

	db, txPool := newReconcileStubDB(t, hosts, sps, spID)

	manager, err := tunnel.NewManager(db, zap.NewNop(), cipher, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	e := echo.New()
	e.Validator = &testValidator{validator: validator.New()}
	body := `{"service_ip":"192.0.2.2","service_port":80,"local_port":8080}`
	req := httptest.NewRequest(http.MethodPost, "/api/service-port", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	h := NewHandler(db, manager, zap.NewNop(), cipher)

	err = h.CreateServicePort(c)
	if err != nil {
		t.Fatalf("CreateServicePort returned error: %v", err)
	}

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	commits, rollbacks := txPool.counts()
	if commits != 1 || rollbacks != 0 {
		t.Fatalf("commits = %d, rollbacks = %d, want 1 and 0", commits, rollbacks)
	}

	// The pass the request asked for is run here, because the loop is wired up
	// in main. That it fails is what the request was answered regardless of.
	result, err := manager.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if result.Started != 0 {
		t.Errorf("started = %d, want 0", result.Started)
	}
	if result.Failed != 1 {
		t.Errorf("failed = %d, want 1: the tunnel of the service port was expected to fail to start", result.Failed)
	}
}

// readRecord is one read a handler made, as the database layer saw it: the SQL
// that was built for it, whether it went out on the transaction, and how many
// commits had gone through by then.
type readRecord struct {
	sql     string
	inTx    bool
	commits int
}

// readRecorder collects the reads of a request. gorm may call back from another
// goroutine, so the access is guarded.
type readRecorder struct {
	mu    sync.Mutex
	reads []readRecord
}

func (r *readRecorder) record(rec readRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.reads = append(r.reads, rec)
}

func (r *readRecorder) all() []readRecord {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]readRecord(nil), r.reads...)
}

// newReadRecordingDB returns the write stub with the SQL of every read written
// down. The statement is built the way the query callback of gorm builds it, so
// what is recorded is what would have been sent.
func newReadRecordingDB(t *testing.T, notFound bool) (*gorm.DB, *txConnPool, *readRecorder) {
	t.Helper()

	db, txPool := newWriteStubDB(t)
	reads := &readRecorder{}

	err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		callbacks.BuildQuerySQL(tx)
		commits, _ := txPool.counts()
		reads.record(readRecord{
			sql:     tx.Statement.SQL.String(),
			inTx:    tx.Statement.ConnPool == gorm.ConnPool(txPool),
			commits: commits,
		})
		tx.Statement.SQL.Reset()

		if notFound {
			_ = tx.AddError(gorm.ErrRecordNotFound)
			return
		}

		switch dest := tx.Statement.Dest.(type) {
		case *models.Host:
			*dest = models.Host{ID: 1, IP: "192.0.2.1", Port: 22, User: "root", Enabled: true}
		case *models.ServicePort:
			*dest = models.ServicePort{ID: 2, ServiceIP: "192.0.2.2", ServicePort: 8081, LocalPort: 18081}
		}
		tx.RowsAffected = 1
	})
	if err != nil {
		t.Fatalf("failed to replace the query callback: %v", err)
	}

	return db, txPool, reads
}

// TestWriteHandlersReadTheirRowInsideTheTransaction pins down that a handler
// which writes a row it has read reads it on the transaction that does the
// write, with the commit still ahead. Two requests on the same row would
// otherwise both read the old row and the later write would put back what the
// earlier one changed.
//
// What holds the two apart is the single database connection that
// database.NewDatabase opens: the second transaction waits in the pool until
// the first has committed, so the read below it sees what the first one wrote.
// That only covers the write if the read is inside the transaction, which is
// what is observed here. SELECT ... FOR UPDATE used to stand here as well, and
// the statement is checked for it because gorm leaves that clause out of SQLite
// SQL without a word and without an error, so a lock written back in would
// protect nothing while looking as though it did.
func TestWriteHandlersReadTheirRowInsideTheTransaction(t *testing.T) {
	const servicePortBody = `{"service_ip":"192.0.2.2","service_port":80,"local_port":8080}`

	tests := []struct {
		name   string
		method string
		target string
		body   string
		param  string
		table  string
		call   func(*Handler, echo.Context) error
	}{
		{
			name:   "update host",
			method: http.MethodPut,
			target: "/api/host/1",
			body:   `{"description":"new","enabled":true}`,
			param:  "1",
			table:  "hosts",
			call:   (*Handler).UpdateHost,
		},
		{
			name:   "delete host",
			method: http.MethodDelete,
			target: "/api/host/1",
			param:  "1",
			table:  "hosts",
			call:   (*Handler).DeleteHost,
		},
		{
			name:   "update service port",
			method: http.MethodPut,
			target: "/api/service-port/2",
			body:   servicePortBody,
			param:  "2",
			table:  "service_ports",
			call:   (*Handler).UpdateServicePort,
		},
		{
			name:   "delete service port",
			method: http.MethodDelete,
			target: "/api/service-port/2",
			param:  "2",
			table:  "service_ports",
			call:   (*Handler).DeleteServicePort,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, txPool, reads := newReadRecordingDB(t, false)
			manager := &wakeRecorder{tx: txPool}

			e := echo.New()
			e.Validator = &testValidator{validator: validator.New()}
			req := httptest.NewRequest(tt.method, tt.target, strings.NewReader(tt.body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.SetParamNames("id")
			c.SetParamValues(tt.param)

			h := NewHandler(db, manager, zap.NewNop(), newTestCipher(t))

			err := tt.call(h, c)
			if err != nil {
				t.Fatalf("the handler returned an error: %v", err)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
			}

			all := reads.all()
			if len(all) != 1 {
				t.Fatalf("reads = %d, want 1: %v", len(all), all)
			}
			read := all[0]

			if strings.Contains(read.sql, "FOR UPDATE") {
				t.Errorf("the read asks for a row lock that SQLite does not take: %s", read.sql)
			}
			if !strings.Contains(read.sql, "`"+tt.table+"`") {
				t.Errorf("sql = %s, want a read of %s", read.sql, tt.table)
			}
			if !read.inTx {
				t.Errorf("the row was read outside the transaction, so the write does not cover it: %s", read.sql)
			}
			if read.commits != 0 {
				t.Errorf("commits at the read = %d, want 0: the transaction that writes was already through", read.commits)
			}

			commits, rollbacks := txPool.counts()
			if commits != 1 || rollbacks != 0 {
				t.Fatalf("commits = %d, rollbacks = %d, want 1 and 0", commits, rollbacks)
			}
		})
	}
}

// TestCreateHandlersReadNothingFirst pins down the other side of it: a create
// has no row to read yet, and the unique indexes are what keep a duplicate out.
func TestCreateHandlersReadNothingFirst(t *testing.T) {
	tests := []struct {
		name   string
		target string
		body   string
		call   func(*Handler, echo.Context) error
	}{
		{
			name:   "create host",
			target: "/api/host",
			body:   `{"ip":"192.0.2.1","port":22,"user":"root","password":"fake-value-1"}`, // hook:allow
			call:   (*Handler).CreateHost,
		},
		{
			name:   "create service port",
			target: "/api/service-port",
			body:   `{"service_ip":"192.0.2.2","service_port":80,"local_port":8080}`,
			call:   (*Handler).CreateServicePort,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, txPool, reads := newReadRecordingDB(t, false)
			manager := &wakeRecorder{tx: txPool}

			e := echo.New()
			e.Validator = &testValidator{validator: validator.New()}
			req := httptest.NewRequest(http.MethodPost, tt.target, strings.NewReader(tt.body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)

			h := NewHandler(db, manager, zap.NewNop(), newTestCipher(t))

			err := tt.call(h, c)
			if err != nil {
				t.Fatalf("the handler returned an error: %v", err)
			}
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
			}

			all := reads.all()
			if len(all) != 0 {
				t.Fatalf("reads = %d, want 0: %v", len(all), all)
			}
		})
	}
}

// TestWriteHandlersRollBackWhenTheRowIsGone pins down that the answer for a row
// that is not there leaves no transaction behind. The read is made inside one,
// so the path that finds nothing has one open.
func TestWriteHandlersRollBackWhenTheRowIsGone(t *testing.T) {
	const servicePortBody = `{"service_ip":"192.0.2.2","service_port":80,"local_port":8080}`

	tests := []struct {
		name   string
		method string
		target string
		body   string
		call   func(*Handler, echo.Context) error
	}{
		{
			name:   "update host",
			method: http.MethodPut,
			target: "/api/host/1",
			body:   `{"description":"new"}`,
			call:   (*Handler).UpdateHost,
		},
		{
			name:   "delete host",
			method: http.MethodDelete,
			target: "/api/host/1",
			call:   (*Handler).DeleteHost,
		},
		{
			name:   "update service port",
			method: http.MethodPut,
			target: "/api/service-port/2",
			body:   servicePortBody,
			call:   (*Handler).UpdateServicePort,
		},
		{
			name:   "delete service port",
			method: http.MethodDelete,
			target: "/api/service-port/2",
			call:   (*Handler).DeleteServicePort,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, txPool, _ := newReadRecordingDB(t, true)
			manager := &wakeRecorder{tx: txPool}

			e := echo.New()
			e.Validator = &testValidator{validator: validator.New()}
			req := httptest.NewRequest(tt.method, tt.target, strings.NewReader(tt.body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.SetParamNames("id")
			c.SetParamValues("1")

			h := NewHandler(db, manager, zap.NewNop(), newTestCipher(t))

			err := tt.call(h, c)
			if err != nil {
				t.Fatalf("the handler returned an error: %v", err)
			}
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
			}

			commits, rollbacks := txPool.counts()
			if commits != 0 || rollbacks != 1 {
				t.Fatalf("commits = %d, rollbacks = %d, want 0 and 1", commits, rollbacks)
			}

			wakes, _ := manager.counts()
			if wakes != 0 {
				t.Errorf("reconcile wake-ups = %d, want 0", wakes)
			}
		})
	}
}

// newRowsDB returns a database of its own holding these rows. The stubs above
// answer statements without keeping them, which is what the transaction tests
// need and the opposite of what a list needs: a page is counted with COUNT and
// cut out with LIMIT and OFFSET, so what the list handlers do is SQL and is
// asked of SQLite itself.
func newRowsDB(t *testing.T, hosts []models.Host, sps []models.ServicePort, tunnels []models.Tunnel) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "tunnel-manager.db")), &gorm.Config{
		Logger: gormlogger.Discard,
	})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	err = db.AutoMigrate(&models.Host{}, &models.ServicePort{}, &models.Tunnel{})
	if err != nil {
		t.Fatalf("failed to migrate the database: %v", err)
	}

	for _, host := range hosts {
		// Enabled carries a default of true, and gorm writes that default in
		// place of a field that holds a zero value, onto the row and onto the
		// struct it was given. A Host stored as disabled comes back enabled, so
		// what the caller asked for is kept here and written by name once the
		// row is there, which is the one thing a disabled Host in a test is for.
		enabled := host.Enabled

		err = db.Create(&host).Error
		if err != nil {
			t.Fatalf("failed to store a Host: %v", err)
		}

		err = db.Model(&models.Host{}).Where("id = ?", host.ID).Update("enabled", enabled).Error
		if err != nil {
			t.Fatalf("failed to store whether a Host is enabled: %v", err)
		}
	}

	for _, sp := range sps {
		err = db.Create(&sp).Error
		if err != nil {
			t.Fatalf("failed to store a service port: %v", err)
		}
	}

	for _, row := range tunnels {
		err = db.Create(&row).Error
		if err != nil {
			t.Fatalf("failed to store a tunnel: %v", err)
		}
	}

	return db
}

// statusHost is a Host with an address of its own, because the rows are stored
// in a database now and two Hosts cannot share an IP.
func statusHost(id uint, enabled bool) models.Host {
	return models.Host{ID: id, IP: fmt.Sprintf("192.0.2.%d", id), Port: 22, User: "root", Enabled: enabled}
}

// statusServicePort is a service port that no other one collides with: the
// service address and port are unique together, and so is the local port.
func statusServicePort(id uint) models.ServicePort {
	return models.ServicePort{ID: id, ServiceIP: "198.51.100.10", ServicePort: 8080 + int(id), LocalPort: 18080 + int(id)}
}

func statusTunnel(hostID, spID uint, status string) models.Tunnel {
	return models.Tunnel{HostID: hostID, SPID: spID, Status: status}
}

// TestGetStatusReportsTheTunnelsThatShouldBeRunning pins down desired_tunnels:
// the number of tunnels the reconcile loop works towards, counted by the
// manager from the state a pass builds. Without it an answer of three tunnels
// out of three connected says nothing about a fourth that could not be started.
func TestGetStatusReportsTheTunnelsThatShouldBeRunning(t *testing.T) {
	tests := []struct {
		name          string
		hosts         []models.Host
		sps           []models.ServicePort
		tunnels       []models.Tunnel
		wantDesired   int
		wantTotal     int
		wantConnected int
	}{
		{
			name:  "every service port on every enabled host",
			hosts: []models.Host{statusHost(1, true), statusHost(2, true)},
			sps:   []models.ServicePort{statusServicePort(1), statusServicePort(2), statusServicePort(3)},
			tunnels: []models.Tunnel{
				statusTunnel(1, 1, "connected"), statusTunnel(1, 2, "connected"), statusTunnel(1, 3, "connected"),
				statusTunnel(2, 1, "connected"), statusTunnel(2, 2, "connected"), statusTunnel(2, 3, "error"),
			},
			wantDesired:   6,
			wantTotal:     6,
			wantConnected: 5,
		},
		{
			name:  "a host that is not enabled is not wanted",
			hosts: []models.Host{statusHost(1, true), statusHost(2, false)},
			sps:   []models.ServicePort{statusServicePort(1), statusServicePort(2), statusServicePort(3)},
			tunnels: []models.Tunnel{
				statusTunnel(1, 1, "connected"), statusTunnel(1, 2, "connected"), statusTunnel(1, 3, "connected"),
			},
			wantDesired:   3,
			wantTotal:     3,
			wantConnected: 3,
		},
		{
			name:          "no service port leaves nothing to run",
			hosts:         []models.Host{statusHost(1, true), statusHost(2, true)},
			sps:           nil,
			tunnels:       nil,
			wantDesired:   0,
			wantTotal:     0,
			wantConnected: 0,
		},
		{
			name:  "tunnels that could not be started are the difference",
			hosts: []models.Host{statusHost(1, true), statusHost(2, true)},
			sps:   []models.ServicePort{statusServicePort(1), statusServicePort(2), statusServicePort(3)},
			tunnels: []models.Tunnel{
				statusTunnel(1, 1, "connected"), statusTunnel(1, 2, "connected"), statusTunnel(1, 3, "connected"),
			},
			wantDesired:   6,
			wantTotal:     3,
			wantConnected: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cipher := newTestCipher(t)
			db := newRowsDB(t, tt.hosts, tt.sps, tt.tunnels)

			manager, err := tunnel.NewManager(db, zap.NewNop(), cipher, 1)
			if err != nil {
				t.Fatalf("failed to create manager: %v", err)
			}

			e := echo.New()
			req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)

			h := NewHandler(db, manager, zap.NewNop(), cipher)

			err = h.GetStatus(c)
			if err != nil {
				t.Fatalf("GetStatus returned error: %v", err)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
			}

			var resp struct {
				Success bool                   `json:"success"`
				Data    map[string]interface{} `json:"data"`
			}
			err = json.Unmarshal(rec.Body.Bytes(), &resp)
			if err != nil {
				t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
			}
			if !resp.Success {
				t.Fatalf("success = false, body: %s", rec.Body.String())
			}

			_, ok := resp.Data["desired_tunnels"]
			if !ok {
				t.Fatalf("the answer carries no desired_tunnels, body: %s", rec.Body.String())
			}

			fields := []struct {
				key  string
				want int
			}{
				{"desired_tunnels", tt.wantDesired},
				{"total_tunnels", tt.wantTotal},
				{"connected_tunnels", tt.wantConnected},
			}
			for _, field := range fields {
				got, ok := resp.Data[field.key].(float64)
				if !ok {
					t.Fatalf("%s is not a number in the answer, body: %s", field.key, rec.Body.String())
				}
				if int(got) != field.want {
					t.Errorf("%s = %d, want %d", field.key, int(got), field.want)
				}
			}

			// The count the answer carries is the size of the desired state of
			// a pass, which is what the loop starts tunnels from.
			count, err := manager.DesiredTunnelCount()
			if err != nil {
				t.Fatalf("DesiredTunnelCount returned error: %v", err)
			}
			if count != tt.wantDesired {
				t.Fatalf("the manager desires %d tunnels while the answer says %d", count, tt.wantDesired)
			}
		})
	}
}

// countFailingManager answers the tunnel rows but cannot say how many tunnels
// should be running.
type countFailingManager struct {
	tunnels []models.Tunnel
}

func (m *countFailingManager) WakeReconcile() {}

func (m *countFailingManager) DesiredTunnelCount() (int, error) {
	return 0, errQueryFailed
}

func (m *countFailingManager) GetAllTunnels() (*[]models.Tunnel, error) {
	return &m.tunnels, nil
}

func (m *countFailingManager) GetHostTunnels(hostID uint) (*[]models.Tunnel, error) {
	return &m.tunnels, nil
}

// TestGetStatusFailsWhenTheDesiredCountCannotBeRead pins down that a count that
// could not be read is an error. An answer that left the field out would read
// as every tunnel that should run being there.
func TestGetStatusFailsWhenTheDesiredCountCannotBeRead(t *testing.T) {
	cipher := newTestCipher(t)
	db, _ := newTxRecordingDB(t)
	manager := &countFailingManager{tunnels: []models.Tunnel{statusTunnel(1, 1, "connected")}}

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	h := NewHandler(db, manager, zap.NewNop(), cipher)

	err := h.GetStatus(c)
	if err != nil {
		t.Fatalf("GetStatus returned error: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "desired_tunnels") {
		t.Errorf("the failed answer carries desired_tunnels: %s", rec.Body.String())
	}
}

// storedHostPassword is the sealed SSH password a stored Host row carries. The
// read handlers are asked whether it turns up in what they answer.
const storedHostPassword = "fake-value-1" // hook:allow

// newHostStubDB returns the write stub with the single Host read answered by
// host, so a row can be set up field by field and the answer read against it.
func newHostStubDB(t *testing.T, host models.Host) *gorm.DB {
	t.Helper()

	db, _ := newWriteStubDB(t)

	err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		if dest, ok := tx.Statement.Dest.(*models.Host); ok {
			*dest = host
		}
		tx.RowsAffected = 1
	})
	if err != nil {
		t.Fatalf("failed to replace the query callback: %v", err)
	}

	return db
}

// newReadFailingDB returns the write stub with every read failing, which is the
// database being unreachable rather than the row being gone.
func newReadFailingDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, _ := newWriteStubDB(t)

	err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		_ = tx.AddError(errQueryFailed)
	})
	if err != nil {
		t.Fatalf("failed to replace the query callback: %v", err)
	}

	return db
}

// storedHost returns the Host row the read handlers are asked about.
func storedHost() models.Host {
	return models.Host{
		ID:          1,
		IP:          "192.0.2.1",
		Port:        22,
		User:        "root",
		Password:    storedHostPassword,
		Description: "the host of the test",
		Enabled:     true,
	}
}

// getRequest builds a GET request with one path parameter, the way echo hands
// one to a handler.
func getRequest(t *testing.T, target, param, value string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()

	e := echo.New()
	e.Validator = &testValidator{validator: validator.New()}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if param != "" {
		c.SetParamNames(param)
		c.SetParamValues(value)
	}

	return c, rec
}

// decodeResponse reads the answer of a handler as success plus a data object.
func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder) (bool, map[string]interface{}) {
	t.Helper()

	var resp struct {
		Success bool                   `json:"success"`
		Data    map[string]interface{} `json:"data"`
		Error   string                 `json:"error"`
	}

	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil {
		t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
	}

	return resp.Success, resp.Data
}

// TestGetHostAnswersTheStoredRow pins the shape of the answer: the fields of
// the row the API is asked for, under data, with success set.
func TestGetHostAnswersTheStoredRow(t *testing.T) {
	db := newHostStubDB(t, storedHost())

	c, rec := getRequest(t, "/api/host/1", "id", "1")
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	err := h.GetHost(c)
	if err != nil {
		t.Fatalf("GetHost returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	success, data := decodeResponse(t, rec)
	if !success {
		t.Fatalf("success = false, body: %s", rec.Body.String())
	}

	fields := map[string]interface{}{
		"id":          float64(1),
		"ip":          "192.0.2.1",
		"port":        float64(22),
		"user":        "root",
		"description": "the host of the test",
		"enabled":     true,
	}
	for key, want := range fields {
		got, ok := data[key]
		if !ok {
			t.Errorf("the answer carries no %s, body: %s", key, rec.Body.String())
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
}

// TestGetHostDoesNotAnswerWithTheSSHPassword is what keeps the sealed SSH
// password of a Host inside the process. Everyone who may read a Host may make
// this request, and an answer that carries the password hands out the way into
// every machine the row names.
func TestGetHostDoesNotAnswerWithTheSSHPassword(t *testing.T) {
	db := newHostStubDB(t, storedHost())

	c, rec := getRequest(t, "/api/host/1", "id", "1")
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	err := h.GetHost(c)
	if err != nil {
		t.Fatalf("GetHost returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if strings.Contains(rec.Body.String(), storedHostPassword) {
		t.Fatalf("the answer carries the SSH password of the Host: %s", rec.Body.String())
	}

	_, data := decodeResponse(t, rec)
	for _, key := range []string{"password", "Password"} {
		_, ok := data[key]
		if ok {
			t.Fatalf("the answer carries a %s field: %s", key, rec.Body.String())
		}
	}
}

// TestGetHostStatusDoesNotAnswerWithTheSSHPassword covers the second way the
// row leaves the process. The status answer carries the Host itself, so the
// same password could ride along with it.
func TestGetHostStatusDoesNotAnswerWithTheSSHPassword(t *testing.T) {
	db := newHostStubDB(t, storedHost())
	manager := &countFailingManager{tunnels: []models.Tunnel{statusTunnel(1, 1, "connected")}}

	c, rec := getRequest(t, "/api/status/host/1", "hostId", "1")
	h := NewHandler(db, manager, zap.NewNop(), newTestCipher(t))

	err := h.GetHostStatus(c)
	if err != nil {
		t.Fatalf("GetHostStatus returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if strings.Contains(rec.Body.String(), storedHostPassword) {
		t.Fatalf("the answer carries the SSH password of the Host: %s", rec.Body.String())
	}

	_, data := decodeResponse(t, rec)
	host, ok := data["host"].(map[string]interface{})
	if !ok {
		t.Fatalf("the answer carries no host object, body: %s", rec.Body.String())
	}
	_, ok = host["password"]
	if ok {
		t.Fatalf("the host of the answer carries a password field: %s", rec.Body.String())
	}
}

// TestGetServicePortAnswersTheStoredRow pins the shape of the answer of the
// other single row read.
func TestGetServicePortAnswersTheStoredRow(t *testing.T) {
	db, _ := newWriteStubDB(t)

	c, rec := getRequest(t, "/api/service-port/2", "id", "2")
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	err := h.GetServicePort(c)
	if err != nil {
		t.Fatalf("GetServicePort returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	success, data := decodeResponse(t, rec)
	if !success {
		t.Fatalf("success = false, body: %s", rec.Body.String())
	}

	fields := map[string]interface{}{
		"id":           float64(2),
		"service_ip":   "192.0.2.2",
		"service_port": float64(8081),
		"local_port":   float64(18081),
	}
	for key, want := range fields {
		got, ok := data[key]
		if !ok {
			t.Errorf("the answer carries no %s, body: %s", key, rec.Body.String())
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
}

// TestListServicePortsAnswersEveryStoredRow pins that the page carries the rows
// under items, so a client that reads data.items[0] finds a service port there,
// along with the total the pager is drawn from.
func TestListServicePortsAnswersEveryStoredRow(t *testing.T) {
	sps := []models.ServicePort{statusServicePort(1), statusServicePort(2)}
	db := newRowsDB(t, nil, sps, nil)

	c, rec := getRequest(t, "/api/service-port", "", "")
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	err := h.ListServicePorts(c)
	if err != nil {
		t.Fatalf("ListServicePorts returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Items []models.ServicePort `json:"items"`
			Total int                  `json:"total"`
			Page  int                  `json:"page"`
			Size  int                  `json:"size"`
		} `json:"data"`
	}
	err = json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil {
		t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
	}
	if !resp.Success {
		t.Fatalf("success = false, body: %s", rec.Body.String())
	}

	if len(resp.Data.Items) != len(sps) {
		t.Fatalf("the answer carries %d service ports, want %d, body: %s",
			len(resp.Data.Items), len(sps), rec.Body.String())
	}
	for i, want := range sps {
		if resp.Data.Items[i].ID != want.ID || resp.Data.Items[i].LocalPort != want.LocalPort {
			t.Errorf("service port %d = %+v, want id %d and local port %d",
				i, resp.Data.Items[i], want.ID, want.LocalPort)
		}
	}
	if resp.Data.Total != len(sps) {
		t.Errorf("total = %d, want %d", resp.Data.Total, len(sps))
	}
	if resp.Data.Page != 1 || resp.Data.Size != 10 {
		t.Errorf("page = %d and size = %d, want the first page of ten", resp.Data.Page, resp.Data.Size)
	}
}

// TestListServicePortsReportsAReadThatFailed pins that a list which could not
// be read is an error and not an empty array. An empty array reads as there
// being no service port, which is what a client would act on.
func TestListServicePortsReportsAReadThatFailed(t *testing.T) {
	db := newReadFailingDB(t)

	c, rec := getRequest(t, "/api/service-port", "", "")
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	err := h.ListServicePorts(c)
	if err != nil {
		t.Fatalf("ListServicePorts returned error: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}

	success, _ := decodeResponse(t, rec)
	if success {
		t.Fatalf("success = true on a read that failed, body: %s", rec.Body.String())
	}
}

// readHandler is one of the reads an id is handed to.
type readHandler struct {
	name  string
	param string
	call  func(*Handler, echo.Context) error
}

// readHandlers are the reads that take an id. GetHostStatus reads it from
// another parameter, which is part of what is pinned here: a handler that reads
// the wrong parameter never sees the id of the request.
var readHandlers = []readHandler{
	{name: "get host", param: "id", call: (*Handler).GetHost},
	{name: "get service port", param: "id", call: (*Handler).GetServicePort},
	{name: "get host status", param: "hostId", call: (*Handler).GetHostStatus},
}

// newReadManager returns a tunnel manager that answers one connected tunnel, so
// the status handler gets past the manager and the answer is about the read.
func newReadManager() *countFailingManager {
	return &countFailingManager{tunnels: []models.Tunnel{statusTunnel(1, 1, "connected")}}
}

// TestReadHandlersRejectAnIDThatIsNotANumber pins that a path segment which is
// no number is turned down before the database is asked. It is the client that
// is wrong, and the answer has to say so rather than report a failed read.
func TestReadHandlersRejectAnIDThatIsNotANumber(t *testing.T) {
	for _, tt := range readHandlers {
		t.Run(tt.name, func(t *testing.T) {
			db := newHostStubDB(t, storedHost())

			c, rec := getRequest(t, "/api/read/not-a-number", tt.param, "not-a-number")
			h := NewHandler(db, newReadManager(), zap.NewNop(), newTestCipher(t))

			err := tt.call(h, c)
			if err != nil {
				t.Fatalf("the handler returned an error: %v", err)
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}

			success, _ := decodeResponse(t, rec)
			if success {
				t.Fatalf("success = true on an id that is not a number, body: %s", rec.Body.String())
			}
		})
	}
}

// TestReadHandlersAnswerNotFoundForARowThatIsGone pins that a row which is not
// stored is a 404 and not an answer carrying an empty row.
func TestReadHandlersAnswerNotFoundForARowThatIsGone(t *testing.T) {
	for _, tt := range readHandlers {
		t.Run(tt.name, func(t *testing.T) {
			db, _, _ := newReadRecordingDB(t, true)

			c, rec := getRequest(t, "/api/read/9", tt.param, "9")
			h := NewHandler(db, newReadManager(), zap.NewNop(), newTestCipher(t))

			err := tt.call(h, c)
			if err != nil {
				t.Fatalf("the handler returned an error: %v", err)
			}
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
			}

			success, data := decodeResponse(t, rec)
			if success {
				t.Fatalf("success = true for a row that is not there, body: %s", rec.Body.String())
			}
			if len(data) != 0 {
				t.Fatalf("the answer carries data for a row that is not there: %s", rec.Body.String())
			}
		})
	}
}

// TestReadHandlersDoNotAnswerARowWhenTheReadFails pins that a database which
// did not answer is not turned into a row. A read that failed is a 500: the
// 404 it used to be sent whoever is on call to look for a row that is stored.
func TestReadHandlersDoNotAnswerARowWhenTheReadFails(t *testing.T) {
	for _, tt := range readHandlers {
		t.Run(tt.name, func(t *testing.T) {
			db := newReadFailingDB(t)

			c, rec := getRequest(t, "/api/read/1", tt.param, "1")
			h := NewHandler(db, newReadManager(), zap.NewNop(), newTestCipher(t))

			err := tt.call(h, c)
			if err != nil {
				t.Fatalf("the handler returned an error: %v", err)
			}
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
			}

			success, data := decodeResponse(t, rec)
			if success {
				t.Fatalf("success = true on a read that failed, body: %s", rec.Body.String())
			}
			if len(data) != 0 {
				t.Fatalf("the answer carries data while the read failed: %s", rec.Body.String())
			}
		})
	}
}

// TestGetHostStatusCountsTheTunnelsOfTheHost pins the two numbers the status of
// one Host is read off: how many tunnel rows it has and how many of them are
// connected. A connected count that took every row would report a Host whose
// tunnels are all down as healthy.
func TestGetHostStatusCountsTheTunnelsOfTheHost(t *testing.T) {
	tests := []struct {
		name          string
		tunnels       []models.Tunnel
		wantTotal     int
		wantConnected int
	}{
		{
			name:          "no tunnel at all",
			tunnels:       nil,
			wantTotal:     0,
			wantConnected: 0,
		},
		{
			name:          "every tunnel connected",
			tunnels:       []models.Tunnel{statusTunnel(1, 1, "connected"), statusTunnel(1, 2, "connected")},
			wantTotal:     2,
			wantConnected: 2,
		},
		{
			name: "the ones that are not connected are the difference",
			tunnels: []models.Tunnel{
				statusTunnel(1, 1, "connected"),
				statusTunnel(1, 2, "error"),
				statusTunnel(1, 3, "connecting"),
			},
			wantTotal:     3,
			wantConnected: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newHostStubDB(t, storedHost())
			manager := &countFailingManager{tunnels: tt.tunnels}

			c, rec := getRequest(t, "/api/status/host/1", "hostId", "1")
			h := NewHandler(db, manager, zap.NewNop(), newTestCipher(t))

			err := h.GetHostStatus(c)
			if err != nil {
				t.Fatalf("GetHostStatus returned error: %v", err)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
			}

			success, data := decodeResponse(t, rec)
			if !success {
				t.Fatalf("success = false, body: %s", rec.Body.String())
			}

			host, ok := data["host"].(map[string]interface{})
			if !ok {
				t.Fatalf("the answer carries no host object, body: %s", rec.Body.String())
			}
			if host["id"] != float64(1) || host["ip"] != "192.0.2.1" {
				t.Errorf("the answer is about another host: %v", host)
			}

			counts := []struct {
				key  string
				want int
			}{
				{"total_tunnels", tt.wantTotal},
				{"connected_tunnels", tt.wantConnected},
			}
			for _, count := range counts {
				got, ok := data[count.key].(float64)
				if !ok {
					t.Fatalf("%s is not a number in the answer, body: %s", count.key, rec.Body.String())
				}
				if int(got) != count.want {
					t.Errorf("%s = %d, want %d", count.key, int(got), count.want)
				}
			}
		})
	}
}

// TestGetHostStatusReportsTunnelsItCouldNotRead pins that a status which could
// not be built is an error. Zero tunnels out of zero reads as a Host that has
// nothing to run, which is not what a failed read says.
func TestGetHostStatusReportsTunnelsItCouldNotRead(t *testing.T) {
	db := newHostStubDB(t, storedHost())

	c, rec := getRequest(t, "/api/status/host/1", "hostId", "1")
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	err := h.GetHostStatus(c)
	if err != nil {
		t.Fatalf("GetHostStatus returned error: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}

	success, data := decodeResponse(t, rec)
	if success {
		t.Fatalf("success = true while the tunnels could not be read, body: %s", rec.Body.String())
	}
	if len(data) != 0 {
		t.Fatalf("the answer carries counts while the tunnels could not be read: %s", rec.Body.String())
	}
}

// errPathHandler is one of the handlers that read a single row by id. inTx says
// whether the read is made inside a transaction, which is what decides if a
// rollback is owed on the way out.
type errPathHandler struct {
	name   string
	method string
	target string
	body   string
	param  string
	inTx   bool
	call   func(*Handler, echo.Context) error
}

// errPathHandlers are the reads of a single row. What is pinned over them is
// the answer for a row that is not stored against the answer for a read that
// did not go through.
var errPathHandlers = []errPathHandler{
	{name: "get host", method: http.MethodGet, target: "/api/host/1", param: "id", call: (*Handler).GetHost},
	{
		name:   "update host",
		method: http.MethodPut,
		target: "/api/host/1",
		body:   `{"description":"new"}`,
		param:  "id",
		inTx:   true,
		call:   (*Handler).UpdateHost,
	},
	{name: "delete host", method: http.MethodDelete, target: "/api/host/1", param: "id", inTx: true, call: (*Handler).DeleteHost},
	{name: "get service port", method: http.MethodGet, target: "/api/service-port/1", param: "id", call: (*Handler).GetServicePort},
	{
		name:   "update service port",
		method: http.MethodPut,
		target: "/api/service-port/1",
		body:   `{"service_ip":"192.0.2.2","service_port":80,"local_port":8080}`,
		param:  "id",
		inTx:   true,
		call:   (*Handler).UpdateServicePort,
	},
	{name: "delete service port", method: http.MethodDelete, target: "/api/service-port/1", param: "id", inTx: true, call: (*Handler).DeleteServicePort},
	{name: "get host status", method: http.MethodGet, target: "/api/status/host/1", param: "hostId", call: (*Handler).GetHostStatus},
}

// errPathDB returns the write stub with every read answered by readErr, so the
// same handler can be run against a row that is gone and against a database
// that did not answer.
func errPathDB(t *testing.T, readErr error) (*gorm.DB, *txConnPool) {
	t.Helper()

	db, txPool := newWriteStubDB(t)

	err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		_ = tx.AddError(readErr)
	})
	if err != nil {
		t.Fatalf("failed to replace the query callback: %v", err)
	}

	return db, txPool
}

// errPathRequest builds the request of one handler, body and path parameter
// included.
func errPathRequest(t *testing.T, tt errPathHandler) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()

	e := echo.New()
	e.Validator = &testValidator{validator: validator.New()}
	req := httptest.NewRequest(tt.method, tt.target, strings.NewReader(tt.body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames(tt.param)
	c.SetParamValues("1")

	return c, rec
}

// errPathLogger returns a logger whose entries can be read back, so that a
// cause which is kept out of the answer can be looked for in the log.
func errPathLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zap.ErrorLevel)

	return zap.New(core), logs
}

// errPathLoggedCause reports whether an error entry carries want, in its
// message or in the error it was given.
func errPathLoggedCause(logs *observer.ObservedLogs, want string) bool {
	for _, entry := range logs.All() {
		if entry.Level != zapcore.ErrorLevel {
			continue
		}
		if strings.Contains(entry.Message, want) {
			return true
		}
		for _, field := range entry.Context {
			if field.Interface != nil && strings.Contains(fmt.Sprint(field.Interface), want) {
				return true
			}
		}
	}

	return false
}

// errPathCheckAnswer reads the answer of a failed request: it reports no
// success, carries no row, and says nothing about the query or the driver that
// failed. The text of the database is for the log, not for a client that is
// only allowed to know that its id was not found.
func errPathCheckAnswer(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	success, data := decodeResponse(t, rec)
	if success {
		t.Fatalf("success = true on a failed request, body: %s", rec.Body.String())
	}
	if len(data) != 0 {
		t.Fatalf("the answer carries data while the read did not bring a row: %s", rec.Body.String())
	}

	for _, leak := range []string{errQueryFailed.Error(), gorm.ErrRecordNotFound.Error()} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("the answer carries the text of the database (%q): %s", leak, rec.Body.String())
		}
	}
}

// TestErrPathHandlersAnswerNotFoundForARowThatIsGone pins the 404 side of the
// split over all six handlers, and that the transaction the write handlers
// opened is rolled back rather than committed.
func TestErrPathHandlersAnswerNotFoundForARowThatIsGone(t *testing.T) {
	for _, tt := range errPathHandlers {
		t.Run(tt.name, func(t *testing.T) {
			db, txPool := errPathDB(t, gorm.ErrRecordNotFound)
			logger, logs := errPathLogger()

			c, rec := errPathRequest(t, tt)
			h := NewHandler(db, &wakeRecorder{tx: txPool}, logger, newTestCipher(t))

			err := tt.call(h, c)
			if err != nil {
				t.Fatalf("the handler returned an error: %v", err)
			}
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
			}

			errPathCheckAnswer(t, rec)

			if logs.Len() != 0 {
				t.Errorf("a row that is not stored was logged as an error: %v", logs.All())
			}

			if tt.inTx {
				commits, rollbacks := txPool.counts()
				if commits != 0 || rollbacks != 1 {
					t.Fatalf("commits = %d, rollbacks = %d, want 0 and 1", commits, rollbacks)
				}
			}
		})
	}
}

// TestErrPathHandlersAnswerServerErrorWhenTheReadFails is the other side: a
// database that did not answer is a 500 with the cause in the log. Answered as
// a 404 it reads as a row that was never stored, and the search starts at the
// client instead of at the database.
func TestErrPathHandlersAnswerServerErrorWhenTheReadFails(t *testing.T) {
	for _, tt := range errPathHandlers {
		t.Run(tt.name, func(t *testing.T) {
			db, txPool := errPathDB(t, errQueryFailed)
			logger, logs := errPathLogger()

			c, rec := errPathRequest(t, tt)
			h := NewHandler(db, &wakeRecorder{tx: txPool}, logger, newTestCipher(t))

			err := tt.call(h, c)
			if err != nil {
				t.Fatalf("the handler returned an error: %v", err)
			}
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
			}

			errPathCheckAnswer(t, rec)

			if !errPathLoggedCause(logs, errQueryFailed.Error()) {
				t.Fatalf("the read that failed left no cause in the log: %v", logs.All())
			}

			if tt.inTx {
				commits, rollbacks := txPool.counts()
				if commits != 0 || rollbacks != 1 {
					t.Fatalf("commits = %d, rollbacks = %d, want 0 and 1", commits, rollbacks)
				}
			}
		})
	}
}

// TestErrPathWrappedNotFoundIsStillNotFound pins that the split is made with
// errors.Is. gorm wraps the error it returns once a callback or a plugin has
// been through it, and a comparison by equality would turn the row that is not
// stored into a failure of this server.
func TestErrPathWrappedNotFoundIsStillNotFound(t *testing.T) {
	for _, tt := range errPathHandlers {
		t.Run(tt.name, func(t *testing.T) {
			db, txPool := errPathDB(t, fmt.Errorf("select from the replica: %w", gorm.ErrRecordNotFound))
			logger, logs := errPathLogger()

			c, rec := errPathRequest(t, tt)
			h := NewHandler(db, &wakeRecorder{tx: txPool}, logger, newTestCipher(t))

			err := tt.call(h, c)
			if err != nil {
				t.Fatalf("the handler returned an error: %v", err)
			}
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
			}

			errPathCheckAnswer(t, rec)

			if logs.Len() != 0 {
				t.Errorf("a row that is not stored was logged as an error: %v", logs.All())
			}

			if tt.inTx {
				commits, rollbacks := txPool.counts()
				if commits != 0 || rollbacks != 1 {
					t.Fatalf("commits = %d, rollbacks = %d, want 0 and 1", commits, rollbacks)
				}
			}
		})
	}
}

// causeOnlyLeaks is what a failed answer must not carry. The text of the
// database names the query, the driver and the server it ran on, and none of
// that is the client's to act on: the cause belongs in the log alone.
var causeOnlyLeaks = []string{errQueryFailed.Error(), "gorm", "sql:", "sqlite"}

// causeOnlyBeginFailingPool is the root pool with the start of a transaction
// failing. It is the one failure a callback cannot stand in for, because the
// handler never gets a transaction to hang one on.
type causeOnlyBeginFailingPool struct {
	rootConnPool
}

func (p *causeOnlyBeginFailingPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	return nil, errQueryFailed
}

// causeOnlyCommitFailingTx counts like the recording transaction and refuses to
// commit.
type causeOnlyCommitFailingTx struct {
	txConnPool
}

func (p *causeOnlyCommitFailingTx) Commit() error {
	_ = p.txConnPool.Commit()

	return errQueryFailed
}

// causeOnlyCommitFailingRoot hands out the transaction that cannot be
// committed.
type causeOnlyCommitFailingRoot struct {
	rootConnPool
	failing *causeOnlyCommitFailingTx
}

func (p *causeOnlyCommitFailingRoot) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	return p.failing, nil
}

// causeOnlyDB opens gorm on pool with the reads answered from memory, so a
// handler is handed the row it asks for and fails where the pool was made to
// fail rather than on the read before it.
func causeOnlyDB(t *testing.T, pool gorm.ConnPool) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Dialector{Conn: pool}, &gorm.Config{
		Logger:               gormlogger.Discard,
		DisableAutomaticPing: true,
	})
	if err != nil {
		t.Fatalf("failed to open gorm with the failing conn pool: %v", err)
	}

	err = db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		switch dest := tx.Statement.Dest.(type) {
		case *models.Host:
			*dest = models.Host{ID: 1, IP: "192.0.2.1", Port: 22, User: "root", Enabled: true}
		case *models.ServicePort:
			*dest = models.ServicePort{ID: 2, ServiceIP: "192.0.2.2", ServicePort: 8081, LocalPort: 18081}
		case *models.User:
			*dest = models.User{ID: 1, SetupRequired: true}
		}
		tx.RowsAffected = 1
	})
	if err != nil {
		t.Fatalf("failed to replace the query callback: %v", err)
	}

	return db
}

// causeOnlyWriteFailingDB returns the write stub with the statement of one
// processor failing, which is a write the database refused rather than a row
// that was not there.
func causeOnlyWriteFailingDB(t *testing.T, processor string) (*gorm.DB, *txConnPool) {
	t.Helper()

	db, txPool := newWriteStubDB(t)

	fail := func(tx *gorm.DB) {
		_ = tx.AddError(errQueryFailed)
	}

	var err error
	switch processor {
	case "create":
		err = db.Callback().Create().Replace("gorm:create", fail)
	case "update":
		err = db.Callback().Update().Replace("gorm:update", fail)
	case "delete":
		err = db.Callback().Delete().Replace("gorm:delete", fail)
	default:
		t.Fatalf("no such processor: %s", processor)
	}
	if err != nil {
		t.Fatalf("failed to replace the %s callback: %v", processor, err)
	}

	return db, txPool
}

// causeOnlyCheckAnswer reads a failed answer: it reports no success, carries no
// data, says nothing of the database that failed, and the cause it left out is
// in the log.
func causeOnlyCheckAnswer(t *testing.T, rec *httptest.ResponseRecorder, logs *observer.ObservedLogs) {
	t.Helper()

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}

	success, data := decodeResponse(t, rec)
	if success {
		t.Fatalf("success = true on a request that failed, body: %s", rec.Body.String())
	}
	if len(data) != 0 {
		t.Fatalf("the answer carries data although the request failed: %s", rec.Body.String())
	}

	body := strings.ToLower(rec.Body.String())
	for _, leak := range causeOnlyLeaks {
		if strings.Contains(body, strings.ToLower(leak)) {
			t.Fatalf("the answer carries the text of the database (%q): %s", leak, rec.Body.String())
		}
	}

	if !errPathLoggedCause(logs, errQueryFailed.Error()) {
		t.Fatalf("the failure left no cause in the log: %v", logs.All())
	}

	// Which failure the handler answered for is logged, so that a case which
	// starts failing earlier than it means to is visible in the output rather
	// than passing on an answer about something else.
	t.Logf("answer: %s, log: %v", rec.Body.String(), logs.All())
}

// causeOnlyWriteCase is one write handler together with the write whose failure
// it answers for.
type causeOnlyWriteCase struct {
	errPathHandler
	// processor is the gorm processor of the statement this handler issues.
	processor string
}

// causeOnlyHostBody and causeOnlyServicePortBody are bodies that get past the
// validator, so the request reaches the transaction rather than being refused
// before it.
const causeOnlyHostBody = `{"ip":"192.0.2.1","port":22,"user":"root","password":"fake-value-1"}` // hook:allow
const causeOnlyServicePortBody = `{"service_ip":"192.0.2.2","service_port":80,"local_port":8080}`

// causeOnlyWriteCases are the six handlers that write inside a transaction.
var causeOnlyWriteCases = []causeOnlyWriteCase{
	{
		errPathHandler: errPathHandler{
			name:   "create host",
			method: http.MethodPost,
			target: "/api/host",
			body:   causeOnlyHostBody,
			call:   (*Handler).CreateHost,
		},
		processor: "create",
	},
	{
		errPathHandler: errPathHandler{
			name:   "update host",
			method: http.MethodPut,
			target: "/api/host/1",
			body:   `{"description":"new"}`,
			param:  "id",
			call:   (*Handler).UpdateHost,
		},
		processor: "update",
	},
	{
		errPathHandler: errPathHandler{
			name:   "delete host",
			method: http.MethodDelete,
			target: "/api/host/1",
			param:  "id",
			call:   (*Handler).DeleteHost,
		},
		processor: "delete",
	},
	{
		errPathHandler: errPathHandler{
			name:   "create service port",
			method: http.MethodPost,
			target: "/api/service-port",
			body:   causeOnlyServicePortBody,
			call:   (*Handler).CreateServicePort,
		},
		processor: "create",
	},
	{
		errPathHandler: errPathHandler{
			name:   "update service port",
			method: http.MethodPut,
			target: "/api/service-port/2",
			body:   causeOnlyServicePortBody,
			param:  "id",
			call:   (*Handler).UpdateServicePort,
		},
		processor: "update",
	},
	{
		errPathHandler: errPathHandler{
			name:   "delete service port",
			method: http.MethodDelete,
			target: "/api/service-port/2",
			param:  "id",
			call:   (*Handler).DeleteServicePort,
		},
		processor: "delete",
	},
}

// TestWriteHandlersKeepACauseThatCannotStartATransactionOutOfTheAnswer covers
// the first failure of every write: the database did not hand out a
// transaction, and the client is told that the request did not go through and
// nothing else.
func TestWriteHandlersKeepACauseThatCannotStartATransactionOutOfTheAnswer(t *testing.T) {
	for _, tt := range causeOnlyWriteCases {
		t.Run(tt.name, func(t *testing.T) {
			db := causeOnlyDB(t, &causeOnlyBeginFailingPool{})
			logger, logs := errPathLogger()

			c, rec := errPathRequest(t, tt.errPathHandler)
			h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, logger, newTestCipher(t))

			err := tt.call(h, c)
			if err != nil {
				t.Fatalf("the handler returned an error: %v", err)
			}

			causeOnlyCheckAnswer(t, rec, logs)
		})
	}
}

// TestWriteHandlersKeepACommitThatFailedOutOfTheAnswer covers the last one: the
// statements went through and the commit did not, so the request stored nothing
// and no reconcile pass is asked for either.
func TestWriteHandlersKeepACommitThatFailedOutOfTheAnswer(t *testing.T) {
	for _, tt := range causeOnlyWriteCases {
		t.Run(tt.name, func(t *testing.T) {
			failing := &causeOnlyCommitFailingTx{}
			db := causeOnlyDB(t, &causeOnlyCommitFailingRoot{failing: failing})
			logger, logs := errPathLogger()

			manager := &wakeRecorder{tx: &failing.txConnPool}
			c, rec := errPathRequest(t, tt.errPathHandler)
			h := NewHandler(db, manager, logger, newTestCipher(t))

			err := tt.call(h, c)
			if err != nil {
				t.Fatalf("the handler returned an error: %v", err)
			}

			causeOnlyCheckAnswer(t, rec, logs)

			wakes, _ := manager.counts()
			if wakes != 0 {
				t.Errorf("reconcile wake-ups = %d, want 0 after a commit that failed", wakes)
			}
		})
	}
}

// TestWriteHandlersKeepAWriteThatFailedOutOfTheAnswer covers the statement in
// between: the row was read and the write on it was refused.
func TestWriteHandlersKeepAWriteThatFailedOutOfTheAnswer(t *testing.T) {
	for _, tt := range causeOnlyWriteCases {
		t.Run(tt.name, func(t *testing.T) {
			db, txPool := causeOnlyWriteFailingDB(t, tt.processor)
			logger, logs := errPathLogger()

			c, rec := errPathRequest(t, tt.errPathHandler)
			h := NewHandler(db, &wakeRecorder{tx: txPool}, logger, newTestCipher(t))

			err := tt.call(h, c)
			if err != nil {
				t.Fatalf("the handler returned an error: %v", err)
			}

			causeOnlyCheckAnswer(t, rec, logs)

			commits, rollbacks := txPool.counts()
			if commits != 0 || rollbacks != 1 {
				t.Fatalf("commits = %d, rollbacks = %d, want 0 and 1", commits, rollbacks)
			}
		})
	}
}

// TestReadHandlersKeepTheCauseOutOfTheAnswer covers the reads that take no id:
// the two listings, and the two statuses whose counts are read through the
// tunnel manager rather than through gorm.
func TestReadHandlersKeepTheCauseOutOfTheAnswer(t *testing.T) {
	tests := []struct {
		name    string
		db      func(*testing.T) *gorm.DB
		manager func() tunnelManager
		target  string
		param   string
		call    func(*Handler, echo.Context) error
	}{
		{
			name:    "list hosts",
			db:      newReadFailingDB,
			manager: func() tunnelManager { return &wakeRecorder{tx: &txConnPool{}} },
			target:  "/api/host",
			call:    (*Handler).ListHosts,
		},
		{
			name:    "list service ports",
			db:      newReadFailingDB,
			manager: func() tunnelManager { return &wakeRecorder{tx: &txConnPool{}} },
			target:  "/api/service-port",
			call:    (*Handler).ListServicePorts,
		},
		{
			name:    "status of every tunnel",
			db:      newReadFailingDB,
			manager: func() tunnelManager { return &wakeRecorder{tx: &txConnPool{}} },
			target:  "/api/status",
			call:    (*Handler).GetStatus,
		},
		{
			name: "status with a count that cannot be read",
			db:   newReadFailingDB,
			manager: func() tunnelManager {
				return &countFailingManager{tunnels: []models.Tunnel{statusTunnel(1, 1, "connected")}}
			},
			target: "/api/status",
			call:   (*Handler).GetStatus,
		},
		{
			name: "status of one host",
			db: func(t *testing.T) *gorm.DB {
				return newHostStubDB(t, storedHost())
			},
			manager: func() tunnelManager { return &wakeRecorder{tx: &txConnPool{}} },
			target:  "/api/status/host/1",
			param:   "hostId",
			call:    (*Handler).GetHostStatus,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, logs := errPathLogger()

			c, rec := getRequest(t, tt.target, tt.param, "1")
			h := NewHandler(tt.db(t), tt.manager(), logger, newTestCipher(t))

			err := tt.call(h, c)
			if err != nil {
				t.Fatalf("the handler returned an error: %v", err)
			}

			causeOnlyCheckAnswer(t, rec, logs)
		})
	}
}

// hostFixture is a Handler over a database of its own, so that what a request
// stored can be read back. The stubs above answer statements without keeping
// them, which is what the transaction tests need and the opposite of what a
// sealed key needs: the point here is the value that landed in the row.
type hostFixture struct {
	e      *echo.Echo
	h      *Handler
	db     *gorm.DB
	cipher *crypto.Cipher
}

func newHostFixture(t *testing.T) *hostFixture {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "tunnel-manager.db")), &gorm.Config{
		Logger: gormlogger.Discard,
	})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	err = db.AutoMigrate(&models.Host{})
	if err != nil {
		t.Fatalf("failed to migrate the database: %v", err)
	}

	e := echo.New()
	e.Validator = &testValidator{validator: validator.New()}

	cipher := newTestCipher(t)

	return &hostFixture{
		e:      e,
		h:      NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), cipher),
		db:     db,
		cipher: cipher,
	}
}

// call sends one request to a handler and hands back what it answered.
func (f *hostFixture) call(t *testing.T, method, target, body, param, value string,
	handler func(echo.Context) error) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := f.e.NewContext(req, rec)

	if param != "" {
		c.SetParamNames(param)
		c.SetParamValues(value)
	}

	err := handler(c)
	if err != nil {
		t.Fatalf("the handler returned an error: %v", err)
	}

	return rec
}

func (f *hostFixture) createHost(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()

	return f.call(t, http.MethodPost, "/api/host", body, "", "", f.h.CreateHost)
}

func (f *hostFixture) updateHost(t *testing.T, id, body string) *httptest.ResponseRecorder {
	t.Helper()

	return f.call(t, http.MethodPut, "/api/host/"+id, body, "id", id, f.h.UpdateHost)
}

// storedHostRow is the row as the database holds it, secrets and all.
func (f *hostFixture) storedHostRow(t *testing.T, id uint) models.Host {
	t.Helper()

	var host models.Host

	err := f.db.First(&host, id).Error
	if err != nil {
		t.Fatalf("failed to read the stored Host: %v", err)
	}

	return host
}

// hostCount is how many Hosts are stored. A refusal that still wrote a row is
// worse than the refusal it answered with.
func (f *hostFixture) hostCount(t *testing.T) int64 {
	t.Helper()

	var count int64

	err := f.db.Model(&models.Host{}).Count(&count).Error
	if err != nil {
		t.Fatalf("failed to count the Hosts: %v", err)
	}

	return count
}

// testPrivateKeyPEM returns a fresh private key in PEM, protected by passphrase
// when one is given. It is generated rather than written into the source: a PEM
// block of a private key in a repository reads as a key that leaked, and a key
// made here belongs to this test run alone.
func testPrivateKeyPEM(t *testing.T, passphrase string) string {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate a key: %v", err)
	}

	var block *pem.Block

	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(priv, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	}
	if err != nil {
		t.Fatalf("failed to marshal the key: %v", err)
	}

	return string(pem.EncodeToMemory(block))
}

// jsonString is a value as it is written inside a request body. A PEM block is
// many lines, and the newlines have to arrive as newlines.
func jsonString(t *testing.T, value string) string {
	t.Helper()

	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("failed to encode %q: %v", value, err)
	}

	return string(encoded)
}

// TestCreateHostSealsThePrivateKey is the shape of what a registered key leaves
// behind. A database file that is copied off the machine must not carry the key
// into every Host that trusts it, so what is in the row is the sealed form and
// nothing of the PEM.
func TestCreateHostSealsThePrivateKey(t *testing.T) {
	f := newHostFixture(t)
	keyPEM := testPrivateKeyPEM(t, "")

	body := `{"ip":"192.0.2.10","port":22,"user":"operator","private_key":` +
		jsonString(t, keyPEM) + `}`

	rec := f.createHost(t, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	host := f.storedHostRow(t, 1)

	if !crypto.IsEncrypted(host.PrivateKey) {
		t.Fatalf("the stored key does not carry the marker of an encrypted value: %q", host.PrivateKey)
	}
	if strings.Contains(host.PrivateKey, "BEGIN") || strings.Contains(host.PrivateKey, keyPEM) {
		t.Fatal("the private key is in the row as it was pasted")
	}

	opened, err := f.cipher.Decrypt(host.PrivateKey)
	if err != nil {
		t.Fatalf("the stored key does not open with the cipher it was stored with: %v", err)
	}
	if opened != strings.TrimSpace(keyPEM) {
		t.Fatal("the stored key is not the one that was registered")
	}

	// A Host registered with a key alone carries no password at all, rather
	// than a sealed empty string, which the tunnel would offer to the Host.
	if host.Password != "" {
		t.Fatalf("the password of a Host registered with a key alone is %q", host.Password)
	}
	if host.KeyPassphrase != "" {
		t.Fatalf("the passphrase of a key that has none is %q", host.KeyPassphrase)
	}

	// The answer is the row, so this is the second place the key could leave.
	if strings.Contains(rec.Body.String(), "BEGIN") || strings.Contains(rec.Body.String(), "private_key") {
		t.Fatalf("the answer carries the private key: %s", rec.Body.String())
	}
}

func TestCreateHostSealsTheKeyPassphrase(t *testing.T) {
	f := newHostFixture(t)
	keyPEM := testPrivateKeyPEM(t, "the passphrase of the test")

	body := `{"ip":"192.0.2.10","port":22,"user":"operator","private_key":` +
		jsonString(t, keyPEM) + `,"key_passphrase":"the passphrase of the test"}`

	rec := f.createHost(t, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	host := f.storedHostRow(t, 1)

	if !crypto.IsEncrypted(host.KeyPassphrase) {
		t.Fatalf("the stored passphrase does not carry the marker of an encrypted value: %q",
			host.KeyPassphrase)
	}
	if strings.Contains(rec.Body.String(), "the passphrase of the test") {
		t.Fatalf("the answer carries the passphrase: %s", rec.Body.String())
	}

	opened, err := f.cipher.Decrypt(host.KeyPassphrase)
	if err != nil {
		t.Fatalf("the stored passphrase does not open: %v", err)
	}
	if opened != "the passphrase of the test" {
		t.Fatal("the stored passphrase is not the one that was registered")
	}
}

// TestCreateHostRefusesAHostWithNoWayIn covers the Host that could not be
// logged in to at all. The password stopped being required when a key became
// one of the ways in, and nothing else would have caught it.
func TestCreateHostRefusesAHostWithNoWayIn(t *testing.T) {
	f := newHostFixture(t)

	rec := f.createHost(t, `{"ip":"192.0.2.10","port":22,"user":"operator"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	var answer struct {
		Error string `json:"error"`
	}
	err := json.Unmarshal(rec.Body.Bytes(), &answer)
	if err != nil {
		t.Fatalf("failed to read the answer: %v", err)
	}

	for _, want := range []string{"private key", "password"} {
		if !strings.Contains(answer.Error, want) {
			t.Fatalf("the refusal %q does not name %q", answer.Error, want)
		}
	}

	if f.hostCount(t) != 0 {
		t.Fatal("a Host was stored although the request was refused")
	}
}

// TestCreateHostRefusesAKeyThatCannotBeUsed is the whole point of reading the
// key where it is registered. A password is only found to be wrong by the Host
// that refuses it, at which point the operator is looking at a tunnel that will
// not come up; a key has a form, and what is wrong with it is said here, next
// to the box it was pasted into.
func TestCreateHostRefusesAKeyThatCannotBeUsed(t *testing.T) {
	locked := testPrivateKeyPEM(t, "the passphrase of the test")

	cases := []struct {
		name       string
		keyPEM     string
		passphrase string
		want       string
	}{
		{"not PEM", "this is not a key at all", "", "not PEM"},
		{"a passphrase that was not given", locked, "", "protected by a passphrase"},
		{"a passphrase that is wrong", locked, "not the passphrase", "does not open"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newHostFixture(t)

			body := `{"ip":"192.0.2.10","port":22,"user":"operator","private_key":` +
				jsonString(t, tc.keyPEM) + `,"key_passphrase":` + jsonString(t, tc.passphrase) + `}`

			rec := f.createHost(t, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest,
					rec.Body.String())
			}

			var answer struct {
				Error string `json:"error"`
			}
			err := json.Unmarshal(rec.Body.Bytes(), &answer)
			if err != nil {
				t.Fatalf("failed to read the answer: %v", err)
			}

			if !strings.Contains(answer.Error, tc.want) {
				t.Fatalf("the refusal %q does not say %q", answer.Error, tc.want)
			}
			if tc.passphrase != "" && strings.Contains(answer.Error, tc.passphrase) {
				t.Fatalf("the refusal carries the passphrase: %q", answer.Error)
			}
			// The lines of the key itself are what must not come back. The
			// message names the BEGIN line an operator should look for, which
			// is a word about the form and not a line of anybody's key.
			for _, line := range keyBodyLines(tc.keyPEM) {
				if strings.Contains(rec.Body.String(), line) {
					t.Fatalf("the refusal carries a line of the key: %s", rec.Body.String())
				}
			}

			if f.hostCount(t) != 0 {
				t.Fatal("a Host was stored although the key was refused")
			}
		})
	}
}

// keyBodyLines is the base64 body of a PEM block, without the BEGIN and END
// lines around it. Those are the lines that are the key.
func keyBodyLines(keyPEM string) []string {
	var body []string

	for _, line := range strings.Split(keyPEM, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "-----") {
			continue
		}

		body = append(body, line)
	}

	return body
}

// TestUpdateHostKeepsTheStoredKeyWhenTheBoxIsEmpty is the rule the edit form is
// drawn around: a key that is stored is never shown, so the box is empty every
// time it is opened, and an empty box has to mean "leave it alone" rather than
// "take it away". It is the password rule, applied to the key.
func TestUpdateHostKeepsTheStoredKeyWhenTheBoxIsEmpty(t *testing.T) {
	f := newHostFixture(t)
	keyPEM := testPrivateKeyPEM(t, "the passphrase of the test")

	rec := f.createHost(t, `{"ip":"192.0.2.10","port":22,"user":"operator","private_key":`+
		jsonString(t, keyPEM)+`,"key_passphrase":"the passphrase of the test"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("the Host was not created: %s", rec.Body.String())
	}

	before := f.storedHostRow(t, 1)

	rec = f.updateHost(t, "1", `{"description":"renamed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	after := f.storedHostRow(t, 1)

	if after.PrivateKey != before.PrivateKey {
		t.Fatal("an update that sent no key changed the stored key")
	}
	if after.KeyPassphrase != before.KeyPassphrase {
		t.Fatal("an update that sent no key changed the stored passphrase")
	}
	if after.Description != "renamed" {
		t.Fatalf("the description is %q, so the update did not land", after.Description)
	}
}

// TestUpdateHostReplacesTheKeyAndItsPassphraseTogether holds that the pair that
// was checked is the pair that is stored. A new key left beside the passphrase
// of the old one is a pair nothing ever checked, and it would be found out by
// the Host refusing the connection.
func TestUpdateHostReplacesTheKeyAndItsPassphraseTogether(t *testing.T) {
	f := newHostFixture(t)
	locked := testPrivateKeyPEM(t, "the passphrase of the test")
	open := testPrivateKeyPEM(t, "")

	rec := f.createHost(t, `{"ip":"192.0.2.10","port":22,"user":"operator","private_key":`+
		jsonString(t, locked)+`,"key_passphrase":"the passphrase of the test"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("the Host was not created: %s", rec.Body.String())
	}

	rec = f.updateHost(t, "1", `{"private_key":`+jsonString(t, open)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	host := f.storedHostRow(t, 1)

	opened, err := f.cipher.Decrypt(host.PrivateKey)
	if err != nil {
		t.Fatalf("the stored key does not open: %v", err)
	}
	if opened != strings.TrimSpace(open) {
		t.Fatal("the stored key is not the one the update sent")
	}
	if host.KeyPassphrase != "" {
		t.Fatal("the passphrase of the key that was replaced is still stored")
	}
}

// TestUpdateHostRefusesAPassphraseOnItsOwn covers the operator who types a
// passphrase into the edit form and leaves the key box empty. The two are only
// right together, and a passphrase stored beside a key nothing checked it
// against would take the Host down at the next connection instead of here.
func TestUpdateHostRefusesAPassphraseOnItsOwn(t *testing.T) {
	f := newHostFixture(t)
	keyPEM := testPrivateKeyPEM(t, "the passphrase of the test")

	rec := f.createHost(t, `{"ip":"192.0.2.10","port":22,"user":"operator","private_key":`+
		jsonString(t, keyPEM)+`,"key_passphrase":"the passphrase of the test"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("the Host was not created: %s", rec.Body.String())
	}

	before := f.storedHostRow(t, 1)

	rec = f.updateHost(t, "1", `{"key_passphrase":"another passphrase"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	after := f.storedHostRow(t, 1)

	if after.KeyPassphrase != before.KeyPassphrase {
		t.Fatal("the refused request changed the stored passphrase")
	}
}

// TestHostAnswersNeverCarryThePrivateKey covers the three ways a Host row
// leaves the process at once. Everyone who may read a Host may make these
// requests, and a key in the answer is a key into every machine that trusts it.
func TestHostAnswersNeverCarryThePrivateKey(t *testing.T) {
	f := newHostFixture(t)
	keyPEM := testPrivateKeyPEM(t, "the passphrase of the test")

	rec := f.createHost(t, `{"ip":"192.0.2.10","port":22,"user":"operator","private_key":`+
		jsonString(t, keyPEM)+`,"key_passphrase":"the passphrase of the test","password":"fake-value-1"}`) // hook:allow
	if rec.Code != http.StatusCreated {
		t.Fatalf("the Host was not created: %s", rec.Body.String())
	}

	answers := map[string]*httptest.ResponseRecorder{
		"create": rec,
		"list":   f.call(t, http.MethodGet, "/api/host", "", "", "", f.h.ListHosts),
		"read":   f.call(t, http.MethodGet, "/api/host/1", "", "id", "1", f.h.GetHost),
	}

	for name, answer := range answers {
		body := answer.Body.String()

		for _, forbidden := range []string{"BEGIN", "private_key", "key_passphrase",
			"the passphrase of the test", "fake-value-1"} { // hook:allow
			if strings.Contains(body, forbidden) {
				t.Fatalf("the %s answer carries %q: %s", name, forbidden, body)
			}
		}
	}
}

// listEndpoint is one of the lists a page is asked of. The three answer in two
// shapes, so the names a test reads an answer by are kept here: the Hosts and
// the service ports carry their page under items with the number of rows beside
// it, and the status carries its page under tunnels with the counts of the
// whole installation.
type listEndpoint struct {
	name     string
	target   string
	call     func(*Handler, echo.Context) error
	itemsKey string
	totalKey string
	idKey    string
}

var listEndpoints = []listEndpoint{
	{
		name:     "hosts",
		target:   "/api/host",
		call:     (*Handler).ListHosts,
		itemsKey: "items",
		totalKey: "total",
		idKey:    "id",
	},
	{
		name:     "service ports",
		target:   "/api/service-port",
		call:     (*Handler).ListServicePorts,
		itemsKey: "items",
		totalKey: "total",
		idKey:    "id",
	},
	{
		name:     "tunnels",
		target:   "/api/status",
		call:     (*Handler).GetStatus,
		itemsKey: "tunnels",
		totalKey: "total_tunnels",
		idKey:    "host_id",
	},
}

// newListRows is a Handler over a database holding rows Hosts, rows service
// ports and rows tunnel rows, one tunnel per Host, so that all three lists are
// the same length and a page of any of them can be checked the same way.
func newListRows(t *testing.T, rows int) (*gorm.DB, *tunnel.Manager) {
	t.Helper()

	hosts := make([]models.Host, 0, rows)
	sps := make([]models.ServicePort, 0, rows)
	tunnels := make([]models.Tunnel, 0, rows)

	for id := 1; id <= rows; id++ {
		hosts = append(hosts, statusHost(uint(id), true))
		sps = append(sps, statusServicePort(uint(id)))
		tunnels = append(tunnels, statusTunnel(uint(id), 1, "connected"))
	}

	db := newRowsDB(t, hosts, sps, tunnels)

	manager, err := tunnel.NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	return db, manager
}

// answerOf sends one list request with this query string and hands back what it
// answered.
func answerOf(t *testing.T, db *gorm.DB, manager tunnelManager, endpoint listEndpoint,
	query string) *httptest.ResponseRecorder {
	t.Helper()

	target := endpoint.target
	if query != "" {
		target += "?" + query
	}

	c, rec := getRequest(t, target, "", "")
	h := NewHandler(db, manager, zap.NewNop(), newTestCipher(t))

	err := endpoint.call(h, c)
	if err != nil {
		t.Fatalf("%s returned an error: %v", endpoint.name, err)
	}

	return rec
}

// pageOf reads a page out of an answer: the identifiers of the rows it carries,
// how many rows there are in all, and which page of which size this is.
func pageOf(t *testing.T, rec *httptest.ResponseRecorder, endpoint listEndpoint) (ids []int, total, page, size int) {
	t.Helper()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	success, data := decodeResponse(t, rec)
	if !success {
		t.Fatalf("success = false, body: %s", rec.Body.String())
	}

	rows, ok := data[endpoint.itemsKey].([]interface{})
	if !ok {
		t.Fatalf("the answer carries no %s array, body: %s", endpoint.itemsKey, rec.Body.String())
	}

	ids = make([]int, 0, len(rows))
	for _, row := range rows {
		fields, ok := row.(map[string]interface{})
		if !ok {
			t.Fatalf("a row of the page is not an object, body: %s", rec.Body.String())
		}

		id, ok := fields[endpoint.idKey].(float64)
		if !ok {
			t.Fatalf("a row of the page carries no %s, body: %s", endpoint.idKey, rec.Body.String())
		}

		ids = append(ids, int(id))
	}

	return ids, answerNumber(t, rec, data, endpoint.totalKey),
		answerNumber(t, rec, data, "page"), answerNumber(t, rec, data, "size")
}

// answerNumber reads one number out of the data of an answer.
func answerNumber(t *testing.T, rec *httptest.ResponseRecorder, data map[string]interface{}, key string) int {
	t.Helper()

	value, ok := data[key].(float64)
	if !ok {
		t.Fatalf("%s is not a number in the answer, body: %s", key, rec.Body.String())
	}

	return int(value)
}

// TestListsAnswerTenRowsWithoutBeingAsked pins the default: a request that
// names no size is answered with the first ten rows and says so, rather than
// with everything that is stored.
func TestListsAnswerTenRowsWithoutBeingAsked(t *testing.T) {
	for _, endpoint := range listEndpoints {
		t.Run(endpoint.name, func(t *testing.T) {
			db, manager := newListRows(t, 25)

			ids, total, page, size := pageOf(t, answerOf(t, db, manager, endpoint, ""), endpoint)

			if len(ids) != 10 {
				t.Fatalf("the page carries %d rows, want 10: %v", len(ids), ids)
			}
			if page != 1 || size != 10 {
				t.Errorf("page = %d and size = %d, want the first page of ten", page, size)
			}
			if total != 25 {
				t.Errorf("total = %d, want 25", total)
			}
		})
	}
}

// TestListsAnswerTheSizeThatWasAskedFor pins that every size that is offered is
// served, and served in full.
func TestListsAnswerTheSizeThatWasAskedFor(t *testing.T) {
	for _, endpoint := range listEndpoints {
		for _, size := range []int{20, 30, 50, 100} {
			t.Run(fmt.Sprintf("%s of %d", endpoint.name, size), func(t *testing.T) {
				db, manager := newListRows(t, 100)

				query := fmt.Sprintf("size=%d", size)
				ids, total, page, answered := pageOf(t, answerOf(t, db, manager, endpoint, query), endpoint)

				if len(ids) != size {
					t.Fatalf("the page carries %d rows, want %d", len(ids), size)
				}
				if answered != size {
					t.Errorf("size = %d, want %d", answered, size)
				}
				if page != 1 {
					t.Errorf("page = %d, want 1", page)
				}
				if total != 100 {
					t.Errorf("total = %d, want 100", total)
				}
			})
		}
	}
}

// TestListsRefuseASizeTheyDoNotServe pins that a size outside the list is
// refused rather than served. A size a request can pick freely is a way to ask
// for every row in one answer, which is what the paging is here to prevent, and
// a size that is quietly rounded would hand back a page nobody asked for.
func TestListsRefuseASizeTheyDoNotServe(t *testing.T) {
	sizes := []string{"7", "1000", "0", "-10", "ten", ""}

	for _, endpoint := range listEndpoints {
		for _, size := range sizes {
			t.Run(fmt.Sprintf("%s of %q", endpoint.name, size), func(t *testing.T) {
				db, manager := newListRows(t, 25)

				rec := answerOf(t, db, manager, endpoint, "size="+size)

				// An empty size is the one that is not refused: a query string
				// that carries the name and no value is a request that named no
				// size, and it is answered with the default.
				if size == "" {
					if rec.Code != http.StatusOK {
						t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
					}

					return
				}

				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
				}

				success, _ := decodeResponse(t, rec)
				if success {
					t.Fatalf("success = true on a size that is not served, body: %s", rec.Body.String())
				}
				if !strings.Contains(rec.Body.String(), "10, 20, 30, 50, 100") {
					t.Errorf("the refusal does not say which sizes are served: %s", rec.Body.String())
				}
			})
		}
	}
}

// TestListPagesNeitherOverlapNorSkip pins the order the pages are cut out in:
// the pages of a list, put back together, are the rows that are stored, each of
// them once. A list without a stated order would let one row sit on two pages
// while another is on none.
func TestListPagesNeitherOverlapNorSkip(t *testing.T) {
	for _, endpoint := range listEndpoints {
		t.Run(endpoint.name, func(t *testing.T) {
			db, manager := newListRows(t, 25)

			seen := make(map[int]int)
			all := make([]int, 0, 25)

			for _, page := range []int{1, 2, 3} {
				query := fmt.Sprintf("page=%d&size=10", page)
				ids, total, answered, size := pageOf(t, answerOf(t, db, manager, endpoint, query), endpoint)

				want := 10
				if page == 3 {
					want = 5
				}
				if len(ids) != want {
					t.Fatalf("page %d carries %d rows, want %d: %v", page, len(ids), want, ids)
				}
				if answered != page || size != 10 {
					t.Errorf("page %d was answered as page %d of size %d", page, answered, size)
				}
				if total != 25 {
					t.Errorf("total = %d on page %d, want 25", total, page)
				}

				for _, id := range ids {
					seen[id]++
					all = append(all, id)
				}
			}

			if len(all) != 25 {
				t.Fatalf("the three pages carry %d rows together, want 25: %v", len(all), all)
			}
			for id, times := range seen {
				if times != 1 {
					t.Errorf("row %d is on %d pages, want 1", id, times)
				}
			}
			for id := 1; id <= 25; id++ {
				if seen[id] != 1 {
					t.Errorf("row %d is on no page", id)
				}
			}
		})
	}
}

// TestAPageBeyondTheLastIsTheLastPage pins that a page that is not there is
// answered with the last one. Rows are deleted while a screen is open, so the
// page a client is on can be gone by the time it asks again, and an error there
// would leave that screen empty instead of showing the rows that are left.
func TestAPageBeyondTheLastIsTheLastPage(t *testing.T) {
	for _, endpoint := range listEndpoints {
		t.Run(endpoint.name, func(t *testing.T) {
			db, manager := newListRows(t, 25)

			ids, total, page, size := pageOf(t, answerOf(t, db, manager, endpoint, "page=99&size=10"), endpoint)

			if page != 3 {
				t.Fatalf("page = %d, want the last page 3, body of a list of %d rows", page, total)
			}
			if len(ids) != 5 {
				t.Errorf("the last page carries %d rows, want 5: %v", len(ids), ids)
			}
			if size != 10 || total != 25 {
				t.Errorf("size = %d and total = %d, want 10 and 25", size, total)
			}
		})
	}
}

// TestAPageBelowTheFirstIsTheFirstPage pins the other end: a page of zero or
// below is read as page 1 rather than refused.
func TestAPageBelowTheFirstIsTheFirstPage(t *testing.T) {
	for _, endpoint := range listEndpoints {
		for _, asked := range []string{"0", "-3"} {
			t.Run(endpoint.name+" of "+asked, func(t *testing.T) {
				db, manager := newListRows(t, 25)

				ids, _, page, _ := pageOf(t, answerOf(t, db, manager, endpoint, "page="+asked), endpoint)

				if page != 1 {
					t.Fatalf("page = %d, want 1", page)
				}
				if len(ids) != 10 {
					t.Errorf("the page carries %d rows, want 10", len(ids))
				}
			})
		}
	}
}

// TestAListWithNoRowsIsTheEmptyFirstPage pins what a list answers when nothing
// is stored: an empty array, no rows in all, and the first page. A client draws
// a list out of that array, so it is there and empty rather than null.
func TestAListWithNoRowsIsTheEmptyFirstPage(t *testing.T) {
	for _, endpoint := range listEndpoints {
		t.Run(endpoint.name, func(t *testing.T) {
			db, manager := newListRows(t, 0)

			rec := answerOf(t, db, manager, endpoint, "")
			ids, total, page, size := pageOf(t, rec, endpoint)

			if len(ids) != 0 {
				t.Fatalf("the page carries %d rows although none is stored: %v", len(ids), ids)
			}
			if total != 0 {
				t.Errorf("total = %d, want 0", total)
			}
			if page != 1 || size != 10 {
				t.Errorf("page = %d and size = %d, want the first page of ten", page, size)
			}
			if !strings.Contains(rec.Body.String(), `"`+endpoint.itemsKey+`":[]`) {
				t.Errorf("the empty page is not an empty array: %s", rec.Body.String())
			}
		})
	}
}

// TestGetStatusCountsEveryTunnelAndNotThePage is what the paging of the status
// stands or falls on. The three counts are about the installation: a page of
// ten out of twenty-five tunnels still says twenty-five rows and says how many
// of all of them are connected. Counted over the page instead, the screen would
// report the page size as the number of tunnels and call an installation with
// nothing connected on page one entirely disconnected.
func TestGetStatusCountsEveryTunnelAndNotThePage(t *testing.T) {
	const rows = 25
	const connected = 20

	hosts := make([]models.Host, 0, rows)
	tunnels := make([]models.Tunnel, 0, rows)

	for id := 1; id <= rows; id++ {
		hosts = append(hosts, statusHost(uint(id), true))

		status := "connected"
		if id > connected {
			status = "error"
		}

		tunnels = append(tunnels, statusTunnel(uint(id), 1, status))
	}

	db := newRowsDB(t, hosts, []models.ServicePort{statusServicePort(1)}, tunnels)

	manager, err := tunnel.NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	c, rec := getRequest(t, "/api/status?page=1&size=10", "", "")
	h := NewHandler(db, manager, zap.NewNop(), newTestCipher(t))

	err = h.GetStatus(c)
	if err != nil {
		t.Fatalf("GetStatus returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	success, data := decodeResponse(t, rec)
	if !success {
		t.Fatalf("success = false, body: %s", rec.Body.String())
	}

	page, ok := data["tunnels"].([]interface{})
	if !ok {
		t.Fatalf("the answer carries no tunnels array, body: %s", rec.Body.String())
	}
	if len(page) != 10 {
		t.Fatalf("the page carries %d tunnels, want 10, body: %s", len(page), rec.Body.String())
	}

	counts := []struct {
		key  string
		want int
	}{
		{"total_tunnels", rows},
		{"connected_tunnels", connected},
		{"desired_tunnels", rows},
	}
	for _, count := range counts {
		got := answerNumber(t, rec, data, count.key)
		if got != count.want {
			t.Errorf("%s = %d, want %d, which is what is stored and not the page of ten",
				count.key, got, count.want)
		}
	}
}

// TestCreateHostStoresTheEnabledItWasGiven pins down that a Host asked for as
// disabled is stored disabled, and that one which says nothing is enabled.
//
// It was neither. The field was not on the create request at all, so a Host
// could only be registered enabled, and it began connecting before anyone had
// a chance to say otherwise. The column carried a database default of true as
// well, which is the part worth keeping a test over: gorm leaves a field out of
// an insert when it holds the zero value and the column has a default, so even
// once the request carried the field, storing false stored true. Naming the
// column in Select does not change it. The default is gone from the model and
// the value is always written.
func TestCreateHostStoresTheEnabledItWasGiven(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"a Host that says nothing", `{"ip":"192.0.2.10","port":22,"user":"operator","password":"the password of the Host"}`, true},
		{"a Host asked for as enabled", `{"ip":"192.0.2.11","port":22,"user":"operator","password":"the password of the Host","enabled":true}`, true},
		{"a Host asked for as disabled", `{"ip":"192.0.2.12","port":22,"user":"operator","password":"the password of the Host","enabled":false}`, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newHostFixture(t)

			rec := f.createHost(t, tc.body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
			}

			var answer struct {
				Data struct {
					ID      uint `json:"id"`
					Enabled bool `json:"enabled"`
				} `json:"data"`
			}
			err := json.Unmarshal(rec.Body.Bytes(), &answer)
			if err != nil {
				t.Fatalf("failed to read the answer: %v", err)
			}
			if answer.Data.Enabled != tc.want {
				t.Errorf("the answer says enabled is %v, want %v", answer.Data.Enabled, tc.want)
			}

			// What the answer says and what the row holds are two claims. The
			// one that decides whether a tunnel is built is the row.
			var stored models.Host
			err = f.db.First(&stored, answer.Data.ID).Error
			if err != nil {
				t.Fatalf("failed to read the stored Host: %v", err)
			}
			if stored.Enabled != tc.want {
				t.Errorf("the stored Host has enabled %v, want %v", stored.Enabled, tc.want)
			}
		})
	}
}

// TestGetStatusCarriesWhatWasMeasuredOfTheForwardedPort pins the two readings
// on the tunnel rows of the status answer. A tunnel that says connected while
// its forwarded port answers nobody is the case they are there for, and the
// status screen reads them off this answer by these names.
func TestGetStatusCarriesWhatWasMeasuredOfTheForwardedPort(t *testing.T) {
	reachable := statusTunnel(1, 1, "connected")
	reachable.ServerBanner = "SSH-2.0-OpenSSH_10.5p1 Ubuntu-1ubuntu2"
	reachable.ForwardReach = "reachable"

	unreachable := statusTunnel(1, 2, "connected")
	unreachable.ServerBanner = "SSH-2.0-dropbear_2022.83"
	unreachable.ForwardReach = "unreachable"

	cipher := newTestCipher(t)
	db := newRowsDB(t,
		[]models.Host{statusHost(1, true)},
		[]models.ServicePort{statusServicePort(1), statusServicePort(2)},
		[]models.Tunnel{reachable, unreachable})

	manager, err := tunnel.NewManager(db, zap.NewNop(), cipher, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	h := NewHandler(db, manager, zap.NewNop(), cipher)

	err = h.GetStatus(c)
	if err != nil {
		t.Fatalf("GetStatus returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Data struct {
			Tunnels []struct {
				HostID       uint   `json:"host_id"`
				SPID         uint   `json:"sp_id"`
				ServerBanner string `json:"server_banner"`
				ForwardReach string `json:"forward_reach"`
			} `json:"tunnels"`
		} `json:"data"`
	}
	err = json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil {
		t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
	}

	if len(resp.Data.Tunnels) != 2 {
		t.Fatalf("the answer carries %d tunnels, want 2, body: %s", len(resp.Data.Tunnels), rec.Body.String())
	}

	for _, want := range []models.Tunnel{reachable, unreachable} {
		found := false

		for _, got := range resp.Data.Tunnels {
			if got.HostID != want.HostID || got.SPID != want.SPID {
				continue
			}

			found = true

			if got.ServerBanner != want.ServerBanner {
				t.Fatalf("tunnel %d-%d carries the banner %q, want %q",
					want.HostID, want.SPID, got.ServerBanner, want.ServerBanner)
			}
			if got.ForwardReach != want.ForwardReach {
				t.Fatalf("tunnel %d-%d carries the reading %q, want %q",
					want.HostID, want.SPID, got.ForwardReach, want.ForwardReach)
			}
		}

		if !found {
			t.Fatalf("the answer carries no tunnel %d-%d, body: %s", want.HostID, want.SPID, rec.Body.String())
		}
	}
}
