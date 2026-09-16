package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"gorm.io/driver/mysql"
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
	return &sql.Row{}
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
	db, err := gorm.Open(mysql.New(mysql.Config{
		Conn:                      &rootConnPool{tx: tx},
		SkipInitializeWithVersion: true,
	}), &gorm.Config{
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
			*dest = models.Host{ID: 1, IP: "10.0.0.1", Port: 22, User: "root", Enabled: true}
		case *models.ServicePort:
			*dest = models.ServicePort{ID: 2, ServiceIP: "10.0.0.2", ServicePort: 8081, LocalPort: 18081}
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
	const hostBody = `{"ip":"10.0.0.1","port":22,"user":"root","password":"fake-value-1"}` // hook:allow
	const servicePortBody = `{"service_ip":"10.0.0.2","service_port":80,"local_port":8080}`

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
	body := `{"service_ip":"10.0.0.1","service_port":80,"local_port":8080}`
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
	sps := []models.ServicePort{{ID: spID, ServiceIP: "10.0.0.2", ServicePort: 80, LocalPort: 8080}}

	db, txPool := newReconcileStubDB(t, hosts, sps, spID)

	manager, err := tunnel.NewManager(db, zap.NewNop(), cipher, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	e := echo.New()
	e.Validator = &testValidator{validator: validator.New()}
	body := `{"service_ip":"10.0.0.2","service_port":80,"local_port":8080}`
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

// TestWriteHandlersReadTheirRowLockedInsideTheTransaction pins down that a
// handler which writes a row it has read reads it with SELECT ... FOR UPDATE on
// the transaction that does the write. Two requests on the same row would
// otherwise both read the old row and the later write would put back what the
// earlier one changed.
//
// A stub database cannot make one request wait for the other, so what is
// observed here is the statement: the lock is asked for, it is asked for on the
// transaction handle, and the commit is still ahead, which is what makes the
// lock cover the write.
func TestWriteHandlersReadTheirRowLockedInsideTheTransaction(t *testing.T) {
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

			if !strings.Contains(read.sql, "FOR UPDATE") {
				t.Errorf("the row was read without a lock: %s", read.sql)
			}
			if !strings.Contains(read.sql, "`"+tt.table+"`") {
				t.Errorf("sql = %s, want a read of %s", read.sql, tt.table)
			}
			if !read.inTx {
				t.Errorf("the row was read outside the transaction, so the lock covers nothing: %s", read.sql)
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

// TestCreateHandlersLockNothing pins down the other side of it: a create has no
// row to lock yet, and the unique indexes are what keep a duplicate out.
func TestCreateHandlersLockNothing(t *testing.T) {
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

// TestWriteHandlersRollBackWhenTheLockedRowIsGone pins down that the answer for
// a row that is not there leaves no transaction behind. The lock is taken
// inside one now, so the path that finds nothing has one open.
func TestWriteHandlersRollBackWhenTheLockedRowIsGone(t *testing.T) {
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
