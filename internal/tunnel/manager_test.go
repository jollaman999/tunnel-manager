package tunnel

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
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

func TestStopAllTunnelsWithFailingDB(t *testing.T) {
	db := newFailingDB(t)

	m, err := NewManager(db, zap.NewNop(), 1)
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

	m, err := NewManager(db, zap.NewNop(), 1)
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

	m, err := NewManager(db, zap.NewNop(), 1)
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
