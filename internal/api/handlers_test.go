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
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func boolPtr(b bool) *bool {
	return &b
}

func intPtr(i int) *int {
	return &i
}

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

func TestResolveTunnelActions(t *testing.T) {
	cipher := newTestCipher(t)

	baseHost := func(enabled bool) models.Host {
		return models.Host{
			ID:          1,
			IP:          "10.0.0.1",
			Port:        22,
			User:        "root",
			Password:    "pass",
			Description: "old",
			Enabled:     enabled,
		}
	}

	tests := []struct {
		name         string
		host         models.Host
		req          models.UpdateHostRequest
		finalEnabled bool
		needStop     bool
		needStart    bool
	}{
		{
			name:         "enabled host disabled by request",
			host:         baseHost(true),
			req:          models.UpdateHostRequest{Enabled: boolPtr(false)},
			finalEnabled: false,
			needStop:     true,
			needStart:    false,
		},
		{
			name:         "disabled host enabled by request",
			host:         baseHost(false),
			req:          models.UpdateHostRequest{Enabled: boolPtr(true)},
			finalEnabled: true,
			needStop:     false,
			needStart:    true,
		},
		{
			name:         "enabled host with changed connection info",
			host:         baseHost(true),
			req:          models.UpdateHostRequest{IP: "10.0.0.2", Port: intPtr(2222), User: "admin", Password: "new"},
			finalEnabled: true,
			needStop:     true,
			needStart:    true,
		},
		{
			name:         "disabled host with changed connection info only",
			host:         baseHost(false),
			req:          models.UpdateHostRequest{IP: "10.0.0.2", Port: intPtr(2222), User: "admin", Password: "new"},
			finalEnabled: false,
			needStop:     false,
			needStart:    false,
		},
		{
			name:         "disabled host with changed description only",
			host:         baseHost(false),
			req:          models.UpdateHostRequest{Description: "new"},
			finalEnabled: false,
			needStop:     false,
			needStart:    false,
		},
		{
			name:         "enabled host with changed description only",
			host:         baseHost(true),
			req:          models.UpdateHostRequest{Description: "new"},
			finalEnabled: true,
			needStop:     false,
			needStart:    false,
		},
		{
			name:         "enabled host with same connection info",
			host:         baseHost(true),
			req:          models.UpdateHostRequest{IP: "10.0.0.1", Port: intPtr(22), User: "root", Password: "pass"},
			finalEnabled: true,
			needStop:     false,
			needStart:    false,
		},
		{
			name:         "disabled host enabled with changed connection info",
			host:         baseHost(false),
			req:          models.UpdateHostRequest{IP: "10.0.0.2", Enabled: boolPtr(true)},
			finalEnabled: true,
			needStop:     false,
			needStart:    true,
		},
		{
			name:         "enabled host disabled with changed connection info",
			host:         baseHost(true),
			req:          models.UpdateHostRequest{IP: "10.0.0.2", Enabled: boolPtr(false)},
			finalEnabled: false,
			needStop:     true,
			needStart:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := tt.host
			req := tt.req

			finalEnabled, needStop, needStart := resolveTunnelActions(&req, &host, cipher)
			if finalEnabled != tt.finalEnabled {
				t.Errorf("finalEnabled = %v, want %v", finalEnabled, tt.finalEnabled)
			}
			if needStop != tt.needStop {
				t.Errorf("needStop = %v, want %v", needStop, tt.needStop)
			}
			if needStart != tt.needStart {
				t.Errorf("needStart = %v, want %v", needStart, tt.needStart)
			}
		})
	}
}

// TestResolveTunnelActionsWithEncryptedPassword pins down that a stored
// password, which is a ciphertext, is never compared against the plaintext of
// the request. Comparing them directly would restart every tunnel of the host
// on every update.
func TestResolveTunnelActionsWithEncryptedPassword(t *testing.T) {
	cipher := newTestCipher(t)

	storedPassword, err := cipher.Encrypt("pass")
	if err != nil {
		t.Fatalf("failed to encrypt: %v", err)
	}

	host := func() models.Host {
		return models.Host{
			ID:       1,
			IP:       "10.0.0.1",
			Port:     22,
			User:     "root",
			Password: storedPassword,
			Enabled:  true,
		}
	}

	tests := []struct {
		name      string
		req       models.UpdateHostRequest
		needStop  bool
		needStart bool
	}{
		{
			name:      "update without a password",
			req:       models.UpdateHostRequest{Description: "new"},
			needStop:  false,
			needStart: false,
		},
		{
			name:      "update with the password that is already stored",
			req:       models.UpdateHostRequest{Password: "pass"},
			needStop:  false,
			needStart: false,
		},
		{
			name:      "update with a different password",
			req:       models.UpdateHostRequest{Password: "other"},
			needStop:  true,
			needStart: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := host()
			req := tt.req

			finalEnabled, needStop, needStart := resolveTunnelActions(&req, &h, cipher)
			if !finalEnabled {
				t.Errorf("finalEnabled = %v, want %v", finalEnabled, true)
			}
			if needStop != tt.needStop {
				t.Errorf("needStop = %v, want %v", needStop, tt.needStop)
			}
			if needStart != tt.needStart {
				t.Errorf("needStart = %v, want %v", needStart, tt.needStart)
			}
		})
	}
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

func TestCreateServicePortRollsBackOnHostFetchFailure(t *testing.T) {
	db, tx := newTxRecordingDB(t)

	e := echo.New()
	e.Validator = &testValidator{validator: validator.New()}
	body := `{"service_ip":"10.0.0.1","service_port":80,"local_port":8080}`
	req := httptest.NewRequest(http.MethodPost, "/api/service-ports", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	h := NewHandler(db, nil, zap.NewNop(), newTestCipher(t))

	err := h.CreateServicePort(c)
	if err != nil {
		t.Fatalf("CreateServicePort returned error: %v", err)
	}

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Failed to fetch Hosts") {
		t.Fatalf("body = %s, want host fetch failure", rec.Body.String())
	}
	commits, rollbacks := tx.counts()
	if rollbacks != 1 {
		t.Errorf("rollbacks = %d, want 1", rollbacks)
	}
	if commits != 0 {
		t.Errorf("commits = %d, want 0", commits)
	}
}

// newStubQueryDB returns a gorm DB that answers queries from memory. The tunnel
// row is reported as already stored so StartTunnel does not have to write one,
// creating any row fails, and deleting a tunnel row fails too, so stopping a
// tunnel that is running fails for a reason other than the tunnel not being
// there.
func newStubQueryDB(t *testing.T, host models.Host, sps []models.ServicePort) *gorm.DB {
	t.Helper()

	db, _ := newTxRecordingDB(t)

	err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		switch dest := tx.Statement.Dest.(type) {
		case *models.Host:
			*dest = host
			tx.RowsAffected = 1
		case *[]models.Host:
			*dest = []models.Host{host}
			tx.RowsAffected = 1
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
		_ = tx.AddError(errQueryFailed)
	})
	if err != nil {
		t.Fatalf("failed to replace the create callback: %v", err)
	}

	err = db.Callback().Delete().Replace("gorm:delete", func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*models.Tunnel); ok {
			_ = tx.AddError(errQueryFailed)
		}
	})
	if err != nil {
		t.Fatalf("failed to replace the delete callback: %v", err)
	}

	return db
}

func newDeleteHostContext(hostID string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodDelete, "/api/hosts/"+hostID, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues(hostID)

	return c, rec
}

func TestDeleteHostDoesNotWarnWhenNoTunnelIsRunning(t *testing.T) {
	host := models.Host{ID: 1, IP: "10.0.0.1", Port: 22, User: "root", Password: "pass", Enabled: false}
	sps := []models.ServicePort{
		{ID: 1, ServiceIP: "10.0.0.2", ServicePort: 8081, LocalPort: 18081},
		{ID: 2, ServiceIP: "10.0.0.2", ServicePort: 8082, LocalPort: 18082},
		{ID: 3, ServiceIP: "10.0.0.2", ServicePort: 8083, LocalPort: 18083},
	}

	db := newStubQueryDB(t, host, sps)
	core, logs := observer.New(zapcore.DebugLevel)

	manager, err := tunnel.NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	c, rec := newDeleteHostContext("1")
	h := NewHandler(db, manager, zap.New(core), newTestCipher(t))

	err = h.DeleteHost(c)
	if err != nil {
		t.Fatalf("DeleteHost returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	for _, entry := range logs.All() {
		if entry.Level >= zapcore.WarnLevel {
			t.Fatalf("deleting a Host with no running tunnel logged %s: %s", entry.Level, entry.Message)
		}
	}

	got := logs.FilterMessage("no tunnel to stop").Len()
	if got != len(sps) {
		t.Fatalf("no tunnel to stop was logged %d times, want %d", got, len(sps))
	}
}

func TestDeleteHostWarnsWhenTunnelCannotBeStopped(t *testing.T) {
	cipher := newTestCipher(t)

	password, err := cipher.Encrypt("fake-value-1")
	if err != nil {
		t.Fatalf("failed to encrypt: %v", err)
	}

	// Port 1 on loopback refuses at once, so the tunnel that does start never
	// reaches the network.
	host := models.Host{ID: 1, IP: "127.0.0.1", Port: 1, User: "root", Password: password, Enabled: true}
	sps := []models.ServicePort{{ID: 2, ServiceIP: "10.0.0.2", ServicePort: 8081, LocalPort: 18081}}

	db := newStubQueryDB(t, host, sps)
	core, logs := observer.New(zapcore.DebugLevel)

	manager, err := tunnel.NewManager(db, zap.NewNop(), cipher, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	// The tunnel row is already stored, so the tunnel is registered and runs.
	err = manager.StartTunnel(&host, &sps[0])
	if err != nil {
		t.Fatalf("StartTunnel returned error: %v", err)
	}

	c, rec := newDeleteHostContext("1")
	h := NewHandler(db, manager, zap.New(core), cipher)

	err = h.DeleteHost(c)
	if err != nil {
		t.Fatalf("DeleteHost returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	entries := logs.FilterMessage("failed to stop tunnel").All()
	if len(entries) != 1 {
		t.Fatalf("failed to stop tunnel was logged %d times, want 1", len(entries))
	}
	if entries[0].Level != zapcore.WarnLevel {
		t.Fatalf("failed to stop tunnel was logged at %s, want %s", entries[0].Level, zapcore.WarnLevel)
	}
	if logs.FilterMessage("no tunnel to stop").Len() != 0 {
		t.Fatal("a tunnel that could not be stopped was reported as not running")
	}
}

// newCreateServicePortStubDB returns a gorm DB that answers the queries
// CreateServicePort and StartTunnel make from memory. Creating the service port
// row succeeds and gives it spID, the tunnel row is reported as already stored
// so StartTunnel does not have to write one, and deleting a tunnel row succeeds
// so a tunnel that was started can be stopped again.
func newCreateServicePortStubDB(t *testing.T, hosts []models.Host, spID uint) (*gorm.DB, *txConnPool) {
	t.Helper()

	db, txPool := newTxRecordingDB(t)

	err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		switch dest := tx.Statement.Dest.(type) {
		case *[]models.Host:
			*dest = hosts
			tx.RowsAffected = int64(len(hosts))
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
			tx.RowsAffected = 1
		}
	})
	if err != nil {
		t.Fatalf("failed to replace the create callback: %v", err)
	}

	err = db.Callback().Delete().Replace("gorm:delete", func(tx *gorm.DB) {
		tx.RowsAffected = 1
	})
	if err != nil {
		t.Fatalf("failed to replace the delete callback: %v", err)
	}

	return db, txPool
}

// TestCreateServicePortStopsStartedTunnelsOnRollback pins down that a tunnel
// that was started for a service port the request rolls back does not keep
// running. The service port row is gone after the rollback, so no later request
// reads it and stops that tunnel.
func TestCreateServicePortStopsStartedTunnelsOnRollback(t *testing.T) {
	cipher := newTestCipher(t)
	otherCipher := newTestCipher(t)

	startedPassword, err := cipher.Encrypt("fake-value-1")
	if err != nil {
		t.Fatalf("failed to encrypt: %v", err)
	}

	// The second Host is stored under another key, so StartTunnel gives up
	// before it dials anything.
	failingPassword, err := otherCipher.Encrypt("fake-value-2")
	if err != nil {
		t.Fatalf("failed to encrypt: %v", err)
	}

	// Port 1 on loopback refuses at once, so the tunnel that does start never
	// reaches the network.
	hosts := []models.Host{
		{ID: 1, IP: "127.0.0.1", Port: 1, User: "root", Password: startedPassword, Enabled: true},
		{ID: 2, IP: "127.0.0.1", Port: 1, User: "root", Password: failingPassword, Enabled: true},
	}

	const spID uint = 7
	db, txPool := newCreateServicePortStubDB(t, hosts, spID)

	manager, err := tunnel.NewManager(db, zap.NewNop(), cipher, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	e := echo.New()
	e.Validator = &testValidator{validator: validator.New()}
	body := `{"service_ip":"10.0.0.2","service_port":80,"local_port":8080}`
	req := httptest.NewRequest(http.MethodPost, "/api/service-ports", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	h := NewHandler(db, manager, zap.NewNop(), cipher)

	err = h.CreateServicePort(c)
	if err != nil {
		t.Fatalf("CreateServicePort returned error: %v", err)
	}

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Failed to start new tunnel") {
		t.Fatalf("body = %s, want a tunnel start failure", rec.Body.String())
	}
	// Only the rollback is counted here. Deleting the tunnel row runs in a
	// transaction of its own, so the commit count says nothing about the
	// transaction this request opened.
	_, rollbacks := txPool.counts()
	if rollbacks != 1 {
		t.Errorf("rollbacks = %d, want 1", rollbacks)
	}

	// Stopping the tunnel of the first Host once more is the only way to tell
	// from here whether the manager still holds it. Reporting that it is not
	// there is what the rollback has to leave behind.
	err = manager.StopTunnel(hosts[0].ID, spID)
	if !errors.Is(err, tunnel.ErrTunnelNotExist) {
		t.Fatalf("StopTunnel after the rollback = %v, want %v: the tunnel started for the rolled back service port is still running",
			err, tunnel.ErrTunnelNotExist)
	}
}
