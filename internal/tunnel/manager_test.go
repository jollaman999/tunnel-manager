package tunnel

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var errConnPoolClosed = errors.New("connection pool is closed")

// failingConnPool makes every statement fail without touching a real database.
type failingConnPool struct{}

func (failingConnPool) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return nil, errConnPoolClosed
}

func (failingConnPool) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return nil, errConnPoolClosed
}

func (failingConnPool) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return nil, errConnPoolClosed
}

func (failingConnPool) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return &sql.Row{}
}

func newFailingDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(mysql.New(mysql.Config{
		Conn:                      failingConnPool{},
		SkipInitializeWithVersion: true,
	}), &gorm.Config{
		Logger:               logger.Discard,
		DisableAutomaticPing: true,
	})
	if err != nil {
		t.Fatalf("failed to open gorm with failing conn pool: %v", err)
	}

	return db
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

func TestStopAllTunnelsWithFailingDB(t *testing.T) {
	db := newFailingDB(t)

	m, err := NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.StopAllTunnels()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StopAllTunnels did not return")
	}

	locked := make(chan struct{})
	go func() {
		m.mu.Lock()
		m.mu.Unlock()
		close(locked)
	}()

	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Fatal("mutex is still held after StopAllTunnels")
	}
}

func TestStartTunnelSkipsDisabledHost(t *testing.T) {
	db := newFailingDB(t)

	m, err := NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	host := models.Host{ID: 1, IP: "127.0.0.1", Port: 22, User: "user", Password: "pass", Enabled: false}
	sp := models.ServicePort{ID: 2, ServiceIP: "127.0.0.1", ServicePort: 3306, LocalPort: 13306}

	err = m.StartTunnel(&host, &sp)
	if err != nil {
		t.Fatalf("StartTunnel on disabled Host returned an error: %v", err)
	}

	m.mu.RLock()
	got := len(m.tunnels)
	_, exists := m.tunnels["1-2"]
	m.mu.RUnlock()

	if exists || got != 0 {
		t.Fatalf("tunnel was created for a disabled Host: len(tunnels)=%d", got)
	}
}

func TestStartTunnelProceedsForEnabledHost(t *testing.T) {
	db := newFailingDB(t)

	m, err := NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	host := models.Host{ID: 1, IP: "127.0.0.1", Port: 22, User: "user", Password: "pass", Enabled: true}
	sp := models.ServicePort{ID: 2, ServiceIP: "127.0.0.1", ServicePort: 3306, LocalPort: 13306}

	// The failing database stops StartTunnel right after the tunnel is registered,
	// so no SSH connection is attempted.
	err = m.StartTunnel(&host, &sp)
	if err == nil {
		t.Fatal("StartTunnel with a failing database returned no error")
	}

	m.mu.RLock()
	_, exists := m.tunnels["1-2"]
	m.mu.RUnlock()

	if !exists {
		t.Fatal("tunnel was not created for an enabled Host")
	}
}

func TestHostPasswordDecryptsStoredValue(t *testing.T) {
	db := newFailingDB(t)
	cipher := newTestCipher(t)

	m, err := NewManager(db, zap.NewNop(), cipher, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	encrypted, err := cipher.Encrypt("s3cr3t")
	if err != nil {
		t.Fatalf("failed to encrypt: %v", err)
	}

	host := models.Host{ID: 1, IP: "127.0.0.1", Port: 22, User: "user", Password: encrypted, Enabled: true}

	got := m.hostPassword(&host)
	if got != "s3cr3t" {
		t.Fatal("hostPassword did not return the stored password")
	}
	if host.Password != encrypted {
		t.Fatal("hostPassword rewrote a password that was already encrypted")
	}
}

func TestHostPasswordFallsBackToPlaintext(t *testing.T) {
	// The database rejects every statement, so the re-encryption cannot be
	// stored. Authenticating still has to work with the plaintext value.
	db := newFailingDB(t)

	m, err := NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	host := models.Host{ID: 1, IP: "127.0.0.1", Port: 22, User: "user", Password: "s3cr3t", Enabled: true}

	got := m.hostPassword(&host)
	if got != "s3cr3t" {
		t.Fatal("hostPassword did not fall back to the plaintext value")
	}
}

// newStubDB returns a gorm DB that answers Host and ServicePort queries from
// memory, so no statement reaches the failing connection pool. A non-nil
// deleteErr makes every delete fail.
func newStubDB(t *testing.T, hosts []models.Host, sps []models.ServicePort, deleteErr error) *gorm.DB {
	t.Helper()

	db := newFailingDB(t)

	err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		switch dest := tx.Statement.Dest.(type) {
		case *[]models.Host:
			*dest = hosts
		case *[]models.ServicePort:
			*dest = sps
		}
	})
	if err != nil {
		t.Fatalf("failed to replace the query callback: %v", err)
	}

	err = db.Callback().Delete().Replace("gorm:delete", func(tx *gorm.DB) {
		if deleteErr != nil {
			_ = tx.AddError(deleteErr)
		}
	})
	if err != nil {
		t.Fatalf("failed to replace the delete callback: %v", err)
	}

	return db
}

func TestStopTunnelReportsMissingTunnel(t *testing.T) {
	m, err := NewManager(newFailingDB(t), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	err = m.StopTunnel(1, 2)
	if err == nil {
		t.Fatal("StopTunnel on a tunnel that is not running returned no error")
	}
	if !errors.Is(err, ErrTunnelNotExist) {
		t.Fatalf("StopTunnel returned an error that callers cannot tell apart: %v", err)
	}
}

func TestStopAllTunnelsDoesNotReportMissingTunnelsAsError(t *testing.T) {
	hosts := []models.Host{{ID: 1, IP: "127.0.0.1", Port: 22, User: "user", Password: "pass", Enabled: false}}
	sps := []models.ServicePort{
		{ID: 1, ServiceIP: "127.0.0.1", ServicePort: 8081, LocalPort: 18081},
		{ID: 2, ServiceIP: "127.0.0.1", ServicePort: 8082, LocalPort: 18082},
		{ID: 3, ServiceIP: "127.0.0.1", ServicePort: 8083, LocalPort: 18083},
	}

	core, logs := observer.New(zapcore.DebugLevel)

	m, err := NewManager(newStubDB(t, hosts, sps, nil), zap.New(core), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	m.StopAllTunnels()

	for _, entry := range logs.All() {
		if entry.Level >= zapcore.ErrorLevel {
			t.Fatalf("stopping tunnels that are not running logged %s: %s", entry.Level, entry.Message)
		}
	}

	got := logs.FilterMessage("no tunnel to stop").Len()
	if got != len(sps) {
		t.Fatalf("no tunnel to stop was logged %d times, want %d", got, len(sps))
	}
}

func TestStopAllTunnelsReportsFailureToStopAsError(t *testing.T) {
	hosts := []models.Host{{ID: 1, IP: "127.0.0.1", Port: 22, User: "user", Password: "pass", Enabled: true}}
	sps := []models.ServicePort{{ID: 2, ServiceIP: "127.0.0.1", ServicePort: 8081, LocalPort: 18081}}

	core, logs := observer.New(zapcore.DebugLevel)

	m, err := NewManager(newStubDB(t, hosts, sps, errConnPoolClosed), zap.New(core), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	hostID, spID := hosts[0].ID, sps[0].ID
	tunnel, err := NewSSHTunnel(&hostID, &spID, "0.0.0.0:18081", "127.0.0.1:22", "127.0.0.1:8081", nil, zap.NewNop())
	if err != nil {
		t.Fatalf("failed to create tunnel: %v", err)
	}

	m.mu.Lock()
	m.tunnels["1-2"] = tunnel
	m.mu.Unlock()

	m.StopAllTunnels()

	if logs.FilterMessage("failed to stop tunnel").Len() != 1 {
		t.Fatal("a tunnel that could not be stopped was not logged as an error")
	}
	for _, entry := range logs.FilterMessage("failed to stop tunnel").All() {
		if entry.Level != zapcore.ErrorLevel {
			t.Fatalf("failed to stop tunnel was logged at %s", entry.Level)
		}
	}
}
