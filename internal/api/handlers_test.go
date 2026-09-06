package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
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
// Commit/Rollback calls are counted.
type txConnPool struct {
	commits   int
	rollbacks int
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
	p.commits++
	return nil
}

func (p *txConnPool) Rollback() error {
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
	if tx.rollbacks != 1 {
		t.Errorf("rollbacks = %d, want 1", tx.rollbacks)
	}
	if tx.commits != 0 {
		t.Errorf("commits = %d, want 0", tx.commits)
	}
}
