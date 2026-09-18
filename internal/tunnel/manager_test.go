package tunnel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

var errConnPoolClosed = errors.New("connection pool is closed")

// sqliteVersionDB answers the one statement the SQLite dialector runs for
// itself: on being opened it asks the database for its version, so as to know
// which clauses it may build. The pools in this package fail every statement
// and have no database behind them, so that probe is sent to a real in-memory
// one instead. It takes a database because a *sql.Row holds nothing exported
// and cannot be built by hand.
var sqliteVersionDB = func() *sql.DB {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		panic(fmt.Sprintf("failed to open the database the version probe is answered from: %v", err))
	}

	return db
}()

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
	return sqliteVersionDB.QueryRowContext(ctx, query, args...)
}

func newFailingDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Dialector{Conn: failingConnPool{}}, &gorm.Config{
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

	// The failing database stops StartTunnel at the tunnel row, which it only
	// reaches for a Host that is not skipped, so no SSH connection is attempted.
	err = m.StartTunnel(&host, &sp)
	if err == nil {
		t.Fatal("StartTunnel with a failing database returned no error")
	}
	if !strings.Contains(err.Error(), "failed to create tunnel information") {
		t.Fatalf("StartTunnel on an enabled Host stopped before the tunnel row: %v", err)
	}
}

func TestStartTunnelLeavesNoTunnelWhenTheRowCannotBeCreated(t *testing.T) {
	db := newFailingDB(t)

	m, err := NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	host := models.Host{ID: 1, IP: "127.0.0.1", Port: 22, User: "user", Password: "pass", Enabled: true}
	sp := models.ServicePort{ID: 2, ServiceIP: "127.0.0.1", ServicePort: 3306, LocalPort: 13306}

	err = m.StartTunnel(&host, &sp)
	if err == nil {
		t.Fatal("StartTunnel with a failing tunnel row returned no error")
	}

	m.mu.RLock()
	got := len(m.tunnels)
	m.mu.RUnlock()

	if got != 0 {
		t.Fatalf("a tunnel that was not started is registered: len(tunnels)=%d", got)
	}

	// The key has to be free again, otherwise the combination can never be
	// started once the database answers.
	err = m.StartTunnel(&host, &sp)
	if err == nil {
		t.Fatal("StartTunnel with a failing tunnel row returned no error")
	}
	if strings.Contains(err.Error(), "tunnel already exists") {
		t.Fatalf("StartTunnel reported a tunnel that was never started as existing: %v", err)
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

	got, err := m.hostPassword(&host)
	if err != nil {
		t.Fatalf("hostPassword returned an error: %v", err)
	}
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

	got, err := m.hostPassword(&host)
	if err != nil {
		t.Fatalf("hostPassword returned an error: %v", err)
	}
	if got != "s3cr3t" {
		t.Fatal("hostPassword did not fall back to the plaintext value")
	}
}

// newUpdateCountingDB returns a gorm DB that counts the updates it is asked for
// instead of running them, so a test can tell whether a statement would have
// reached the hosts table.
func newUpdateCountingDB(t *testing.T, updates *int) *gorm.DB {
	t.Helper()

	db := newFailingDB(t)

	err := db.Callback().Update().Replace("gorm:update", func(tx *gorm.DB) {
		*updates++
	})
	if err != nil {
		t.Fatalf("failed to replace the update callback: %v", err)
	}

	return db
}

// unmarkedCipherText returns plaintext in the encrypted format that carried no
// marker, which is what the rows written before this change hold.
func unmarkedCipherText(t *testing.T, c *crypto.Cipher, plaintext string) string {
	t.Helper()

	encrypted, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("failed to encrypt: %v", err)
	}

	// The marker ends at the last ':', which base64 never produces.
	unmarked := encrypted[strings.LastIndex(encrypted, ":")+1:]
	if crypto.IsEncrypted(unmarked) {
		t.Fatal("failed to strip the marker")
	}

	decrypted, err := c.Decrypt(unmarked)
	if err != nil || decrypted != plaintext {
		t.Fatalf("the stripped value is not a valid value of the format without a marker: %v", err)
	}

	return unmarked
}

func TestHostPasswordKeepsTheStoredValueWhenTheKeyIsWrong(t *testing.T) {
	stored, err := newTestCipher(t).Encrypt("s3cr3t")
	if err != nil {
		t.Fatalf("failed to encrypt: %v", err)
	}

	updates := 0

	m, err := NewManager(newUpdateCountingDB(t, &updates), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	host := models.Host{ID: 1, IP: "127.0.0.1", Port: 22, User: "user", Password: stored, Enabled: true}

	got, err := m.hostPassword(&host)
	if err == nil {
		t.Fatal("hostPassword accepted a password that does not decrypt with the key in use")
	}
	if !errors.Is(err, crypto.ErrWrongKey) {
		t.Fatalf("hostPassword returned an error that callers cannot tell apart: %v", err)
	}
	if got != "" {
		t.Fatal("hostPassword returned a password although the key is wrong")
	}
	if updates != 0 {
		t.Fatalf("hostPassword sent %d updates to the hosts table with a wrong key", updates)
	}
	if host.Password != stored {
		t.Fatal("hostPassword changed the stored password although the key is wrong")
	}
}

func TestHostPasswordKeepsTheStoredValueWithoutMarkerWhenTheKeyIsWrong(t *testing.T) {
	// This is the case that destroyed passwords: a row written by the release
	// that had no marker yet, read after the key was replaced.
	stored := unmarkedCipherText(t, newTestCipher(t), "s3cr3t")

	updates := 0

	m, err := NewManager(newUpdateCountingDB(t, &updates), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	host := models.Host{ID: 1, IP: "127.0.0.1", Port: 22, User: "user", Password: stored, Enabled: true}

	_, err = m.hostPassword(&host)
	if !errors.Is(err, crypto.ErrWrongKey) {
		t.Fatalf("hostPassword did not report a wrong key: %v", err)
	}
	if updates != 0 {
		t.Fatalf("hostPassword sent %d updates to the hosts table with a wrong key", updates)
	}
	if host.Password != stored {
		t.Fatalf("hostPassword changed the stored value of length %d to one of length %d",
			len(stored), len(host.Password))
	}
}

func TestHostPasswordMigratesPlaintext(t *testing.T) {
	cipher := newTestCipher(t)
	updates := 0

	m, err := NewManager(newUpdateCountingDB(t, &updates), zap.NewNop(), cipher, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	host := models.Host{ID: 1, IP: "127.0.0.1", Port: 22, User: "user", Password: "s3cr3t", Enabled: true}

	got, err := m.hostPassword(&host)
	if err != nil {
		t.Fatalf("hostPassword returned an error: %v", err)
	}
	if got != "s3cr3t" {
		t.Fatal("hostPassword did not return the stored password")
	}
	if updates != 1 {
		t.Fatalf("a plaintext password was updated %d times, want 1", updates)
	}
	if !crypto.IsEncrypted(host.Password) {
		t.Fatal("the migrated password carries no marker")
	}

	decrypted, err := cipher.Decrypt(host.Password)
	if err != nil || decrypted != "s3cr3t" {
		t.Fatalf("the migrated password does not decrypt to the original one: %v", err)
	}
}

func TestHostPasswordMarksTheStoredValueWithoutMarker(t *testing.T) {
	cipher := newTestCipher(t)
	stored := unmarkedCipherText(t, cipher, "s3cr3t")

	updates := 0

	m, err := NewManager(newUpdateCountingDB(t, &updates), zap.NewNop(), cipher, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	host := models.Host{ID: 1, IP: "127.0.0.1", Port: 22, User: "user", Password: stored, Enabled: true}

	got, err := m.hostPassword(&host)
	if err != nil {
		t.Fatalf("hostPassword returned an error: %v", err)
	}
	if got != "s3cr3t" {
		t.Fatal("hostPassword did not return the stored password")
	}
	if updates != 1 {
		t.Fatalf("a value in the format without a marker was updated %d times, want 1", updates)
	}
	if !crypto.IsEncrypted(host.Password) {
		t.Fatal("the stored value did not get the marker")
	}

	decrypted, err := cipher.Decrypt(host.Password)
	if err != nil || decrypted != "s3cr3t" {
		t.Fatalf("the marked value does not decrypt to the original password: %v", err)
	}
}

func TestStartTunnelFailsWhenTheKeyIsWrong(t *testing.T) {
	stored, err := newTestCipher(t).Encrypt("s3cr3t")
	if err != nil {
		t.Fatalf("failed to encrypt: %v", err)
	}

	updates := 0

	core, logs := observer.New(zapcore.DebugLevel)

	m, err := NewManager(newUpdateCountingDB(t, &updates), zap.New(core), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	host := models.Host{ID: 1, IP: "127.0.0.1", Port: 22, User: "user", Password: stored, Enabled: true}
	sp := models.ServicePort{ID: 2, ServiceIP: "127.0.0.1", ServicePort: 3306, LocalPort: 13306}

	err = m.StartTunnel(&host, &sp)
	if !errors.Is(err, crypto.ErrWrongKey) {
		t.Fatalf("StartTunnel did not report a wrong key: %v", err)
	}
	if updates != 0 {
		t.Fatalf("StartTunnel sent %d updates to the hosts table with a wrong key", updates)
	}
	if host.Password != stored {
		t.Fatal("StartTunnel changed the stored password although the key is wrong")
	}

	m.mu.RLock()
	_, exists := m.tunnels["1-2"]
	m.mu.RUnlock()

	if exists {
		t.Fatal("a tunnel was started with a password that could not be decrypted")
	}

	errorLogs := 0
	for _, entry := range logs.All() {
		if entry.Level >= zapcore.ErrorLevel {
			errorLogs++
		}
	}

	if errorLogs == 0 {
		t.Fatal("a password that does not decrypt with the key in use was not logged as an error")
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

func TestStopAllTunnelsStopsEveryRunningTunnel(t *testing.T) {
	// The rows say nothing about what is running: one of the tunnels belongs
	// to a Host that is no longer there, and it still has to be stopped.
	hosts := []models.Host{{ID: 1, IP: "127.0.0.1", Port: 1, User: "user", Enabled: true}}
	sps := []models.ServicePort{{ID: 2, ServiceIP: "127.0.0.1", ServicePort: 8081, LocalPort: 18081}}

	core, logs := observer.New(zapcore.DebugLevel)

	m, err := NewManager(newStubDB(t, hosts, sps, nil), zap.New(core), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	registerStoppedTunnel(t, m, 1, 2)
	registerStoppedTunnel(t, m, 9, 9)

	m.StopAllTunnels()

	for _, entry := range logs.All() {
		if entry.Level >= zapcore.ErrorLevel {
			t.Fatalf("stopping the running tunnels logged %s: %s", entry.Level, entry.Message)
		}
	}

	m.mu.RLock()
	left := len(m.tunnels)
	m.mu.RUnlock()

	if left != 0 {
		t.Fatalf("%d tunnels are still registered after StopAllTunnels", left)
	}
}

func TestStopAllTunnelsLogsNothingWhenNothingRuns(t *testing.T) {
	hosts := []models.Host{{ID: 1, IP: "127.0.0.1", Port: 1, User: "user", Enabled: true}}
	sps := []models.ServicePort{{ID: 2, ServiceIP: "127.0.0.1", ServicePort: 8081, LocalPort: 18081}}

	core, logs := observer.New(zapcore.DebugLevel)

	m, err := NewManager(newStubDB(t, hosts, sps, nil), zap.New(core), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	m.StopAllTunnels()

	if logs.Len() != 0 {
		t.Fatalf("stopping no tunnel at all logged %d entries", logs.Len())
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

// queryHostID returns the host_id a query is filtered by, and whether it
// carries such a condition at all. It reads the clause the code added instead
// of the SQL text, so a query that was built without the condition is seen as
// what it is: a query for every row.
func queryHostID(t *testing.T, tx *gorm.DB) (uint, bool) {
	t.Helper()

	c, ok := tx.Statement.Clauses["WHERE"]
	if !ok {
		return 0, false
	}

	where, ok := c.Expression.(clause.Where)
	if !ok {
		t.Errorf("the query carries a WHERE clause of type %T that this stub cannot read", c.Expression)
		return 0, false
	}

	for _, expr := range where.Exprs {
		cond, ok := expr.(clause.Expr)
		if !ok || cond.SQL != "host_id = ?" || len(cond.Vars) != 1 {
			t.Errorf("the query carries a condition %v that this stub cannot read", expr)
			continue
		}

		hostID, ok := cond.Vars[0].(uint)
		if !ok {
			t.Errorf("the host_id condition was given a %T, not a host identifier", cond.Vars[0])
			continue
		}

		return hostID, true
	}

	return 0, false
}

// tunnelRowsFor returns what a database would answer the query of tx with. A
// query without a host_id condition gets every row, which is what makes the
// difference between the two readers visible.
func tunnelRowsFor(t *testing.T, tx *gorm.DB, rows []models.Tunnel) []models.Tunnel {
	t.Helper()

	hostID, filtered := queryHostID(t, tx)

	// gorm replaces the destination slice before it appends the rows it
	// scanned (gorm, scan.go:293), so an answer with no row at all is an empty
	// slice and not a nil one.
	answer := make([]models.Tunnel, 0, len(rows))
	for _, row := range rows {
		if filtered && row.HostID != hostID {
			continue
		}

		answer = append(answer, row)
	}

	return answer
}

// newTunnelStubDB returns a gorm DB that answers tunnel queries from rows,
// applying the conditions the query itself carries. A non-nil queryErr makes
// every tunnel query fail instead.
func newTunnelStubDB(t *testing.T, rows []models.Tunnel, queryErr error) *gorm.DB {
	t.Helper()

	db := newFailingDB(t)

	err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		dest, ok := tx.Statement.Dest.(*[]models.Tunnel)
		if !ok {
			return
		}

		if queryErr != nil {
			_ = tx.AddError(queryErr)
			return
		}

		*dest = tunnelRowsFor(t, tx, rows)
	})
	if err != nil {
		t.Fatalf("failed to replace the query callback: %v", err)
	}

	return db
}

// testTunnelRow is a stored tunnel of the Host and the service port with these
// identifiers.
func testTunnelRow(hostID, spID uint) models.Tunnel {
	return models.Tunnel{
		HostID: hostID,
		SPID:   spID,
		Status: "connected",
		Local:  fmt.Sprintf("0.0.0.0:%d", 18080+spID),
		Server: fmt.Sprintf("127.0.0.%d:22", hostID),
		Remote: "127.0.0.1:3306",
	}
}

func tunnelKeys(tunnels []models.Tunnel) []string {
	keys := make([]string, 0, len(tunnels))
	for _, tunnel := range tunnels {
		keys = append(keys, tunnelKey(tunnel.HostID, tunnel.SPID))
	}

	return keys
}

func TestGetAllTunnelsReturnsEveryStoredTunnel(t *testing.T) {
	rows := []models.Tunnel{testTunnelRow(1, 1), testTunnelRow(1, 2), testTunnelRow(2, 1)}

	m, err := NewManager(newTunnelStubDB(t, rows, nil), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	got, err := m.GetAllTunnels()
	if err != nil {
		t.Fatalf("GetAllTunnels returned an error: %v", err)
	}
	if got == nil {
		t.Fatal("GetAllTunnels returned no result and no error")
	}

	want := []string{"1-1", "1-2", "2-1"}
	if !reflect.DeepEqual(tunnelKeys(*got), want) {
		t.Fatalf("GetAllTunnels returned %v, want %v", tunnelKeys(*got), want)
	}
	if !reflect.DeepEqual(*got, rows) {
		t.Fatal("GetAllTunnels returned rows that are not the stored ones")
	}
}

func TestGetAllTunnelsReturnsAnEmptyResultWhenNothingIsStored(t *testing.T) {
	m, err := NewManager(newTunnelStubDB(t, nil, nil), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	got, err := m.GetAllTunnels()
	if err != nil {
		t.Fatalf("GetAllTunnels returned an error although there is simply no tunnel: %v", err)
	}
	if got == nil {
		t.Fatal("GetAllTunnels returned a nil result for a database without tunnels, callers dereference it")
	}
	if len(*got) != 0 {
		t.Fatalf("GetAllTunnels returned %d tunnels although none is stored", len(*got))
	}
}

func TestGetAllTunnelsReportsADatabaseFailure(t *testing.T) {
	m, err := NewManager(newTunnelStubDB(t, nil, errConnPoolClosed), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	got, err := m.GetAllTunnels()
	if err == nil {
		t.Fatal("GetAllTunnels reported a database that cannot be read as having no tunnel")
	}
	if !errors.Is(err, errConnPoolClosed) {
		t.Fatalf("GetAllTunnels hid what the database reported: %v", err)
	}
	if got != nil {
		t.Fatal("GetAllTunnels returned a result together with an error")
	}
}

func TestGetHostTunnelsReturnsOnlyTheTunnelsOfThatHost(t *testing.T) {
	rows := []models.Tunnel{testTunnelRow(1, 1), testTunnelRow(2, 1), testTunnelRow(1, 2), testTunnelRow(3, 1)}

	m, err := NewManager(newTunnelStubDB(t, rows, nil), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	got, err := m.GetHostTunnels(1)
	if err != nil {
		t.Fatalf("GetHostTunnels returned an error: %v", err)
	}
	if got == nil {
		t.Fatal("GetHostTunnels returned no result and no error")
	}

	want := []string{"1-1", "1-2"}
	if !reflect.DeepEqual(tunnelKeys(*got), want) {
		t.Fatalf("GetHostTunnels(1) returned %v, want %v, the tunnels of the other hosts are being reported "+
			"as belonging to this one", tunnelKeys(*got), want)
	}
}

func TestGetHostTunnelsReturnsAnEmptyResultForAHostWithoutTunnels(t *testing.T) {
	rows := []models.Tunnel{testTunnelRow(1, 1), testTunnelRow(2, 1)}

	m, err := NewManager(newTunnelStubDB(t, rows, nil), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	got, err := m.GetHostTunnels(9)
	if err != nil {
		t.Fatalf("GetHostTunnels returned an error although the Host simply has no tunnel: %v", err)
	}
	if got == nil {
		t.Fatal("GetHostTunnels returned a nil result for a Host without tunnels, callers dereference it")
	}
	if len(*got) != 0 {
		t.Fatalf("GetHostTunnels(9) returned %d tunnels although none of the stored ones belongs to it", len(*got))
	}
}

func TestGetHostTunnelsReportsADatabaseFailure(t *testing.T) {
	m, err := NewManager(newTunnelStubDB(t, nil, errConnPoolClosed), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	got, err := m.GetHostTunnels(1)
	if err == nil {
		t.Fatal("GetHostTunnels reported a database that cannot be read as the Host having no tunnel")
	}
	if !errors.Is(err, errConnPoolClosed) {
		t.Fatalf("GetHostTunnels hid what the database reported: %v", err)
	}
	if got != nil {
		t.Fatal("GetHostTunnels returned a result together with an error")
	}
}

// statementLog records the statements a stub database was asked for, in the
// order they arrived.
type statementLog struct {
	mu     sync.Mutex
	events []string
}

func (l *statementLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.events = append(l.events, event)
}

func (l *statementLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]string(nil), l.events...)
}

func (l *statementLog) has(event string) bool {
	for _, got := range l.all() {
		if got == event {
			return true
		}
	}

	return false
}

const (
	deleteTunnels = "delete tunnels"
	readHosts     = "read hosts"
)

// newRecordingStubDB returns a writable stub that records the deletes it is
// asked for and the reads of the desired state, so a test can tell in which
// order they were sent. A non-nil deleteErr makes every delete fail.
func newRecordingStubDB(t *testing.T, hosts []models.Host, sps []models.ServicePort, deleteErr error, log *statementLog) *gorm.DB {
	t.Helper()

	db := newWritableStubDB(t, hosts, sps)

	err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		switch dest := tx.Statement.Dest.(type) {
		case *[]models.Host:
			log.add(readHosts)
			*dest = hosts
		case *[]models.ServicePort:
			*dest = sps
		}
	})
	if err != nil {
		t.Fatalf("failed to replace the query callback: %v", err)
	}

	err = db.Callback().Delete().Replace("gorm:delete", func(tx *gorm.DB) {
		log.add(deleteTunnels)
		if deleteErr != nil {
			_ = tx.AddError(deleteErr)
		}
	})
	if err != nil {
		t.Fatalf("failed to replace the delete callback: %v", err)
	}

	return db
}

func TestRestoreAllTunnelsDropsTheStoredRowsBeforeItStartsAnything(t *testing.T) {
	// The rows of the previous process describe SSH connections that died with
	// it, so they have to go before the first pass reads what should run.
	hosts := []models.Host{enabledHost(1, true)}
	sps := []models.ServicePort{testServicePort(2)}

	log := &statementLog{}

	m, err := NewManager(newRecordingStubDB(t, hosts, sps, nil, log), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	t.Cleanup(m.StopAllTunnels)

	err = m.RestoreAllTunnels()
	if err != nil {
		t.Fatalf("RestoreAllTunnels returned an error: %v", err)
	}

	events := log.all()
	if len(events) == 0 || events[0] != deleteTunnels {
		t.Fatalf("RestoreAllTunnels sent %v, want the stored tunnel rows to be dropped first", events)
	}
	if !log.has(readHosts) {
		t.Fatalf("RestoreAllTunnels sent %v, it never read what should be running", events)
	}

	running := runningKeys(m)
	if _, ok := running["1-2"]; !ok || len(running) != 1 {
		t.Fatalf("the tunnels running after RestoreAllTunnels are %v, want the one of the desired state", running)
	}
}

func TestRestoreAllTunnelsStartsNothingForADisabledHost(t *testing.T) {
	hosts := []models.Host{enabledHost(1, false)}
	sps := []models.ServicePort{testServicePort(2)}

	log := &statementLog{}

	m, err := NewManager(newRecordingStubDB(t, hosts, sps, nil, log), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	t.Cleanup(m.StopAllTunnels)

	err = m.RestoreAllTunnels()
	if err != nil {
		t.Fatalf("RestoreAllTunnels returned an error for a disabled Host: %v", err)
	}

	if running := runningKeys(m); len(running) != 0 {
		t.Fatalf("RestoreAllTunnels started %v for a Host that is not enabled", running)
	}

	// The rows still go, or the status of a Host that was disabled while the
	// process was down keeps reading as connected.
	if !log.has(deleteTunnels) {
		t.Fatal("RestoreAllTunnels left the stored tunnel rows of the previous process in place")
	}
}

func TestRestoreAllTunnelsFailsWhenTheStoredRowsCannotBeDropped(t *testing.T) {
	hosts := []models.Host{enabledHost(1, true)}
	sps := []models.ServicePort{testServicePort(2)}

	log := &statementLog{}

	m, err := NewManager(newRecordingStubDB(t, hosts, sps, errConnPoolClosed, log), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	t.Cleanup(m.StopAllTunnels)

	err = m.RestoreAllTunnels()
	if err == nil {
		t.Fatal("RestoreAllTunnels reported a startup in which the stored rows could not be dropped as done")
	}
	if !errors.Is(err, errConnPoolClosed) {
		t.Fatalf("RestoreAllTunnels hid what the database reported: %v", err)
	}

	if log.has(readHosts) {
		t.Fatal("RestoreAllTunnels started tunnels although the rows of the previous process are still there")
	}
	if running := runningKeys(m); len(running) != 0 {
		t.Fatalf("RestoreAllTunnels left %v running after it failed", running)
	}
}

func TestRestoreAllTunnelsFailsWhenTheDesiredStateCannotBeRead(t *testing.T) {
	log := &statementLog{}

	// Only the delete is answered, so the pass that follows it cannot read the
	// hosts and ends before it starts anything.
	db := newFailingDB(t)
	err := db.Callback().Delete().Replace("gorm:delete", func(tx *gorm.DB) {
		log.add(deleteTunnels)
	})
	if err != nil {
		t.Fatalf("failed to replace the delete callback: %v", err)
	}

	m, err := NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	t.Cleanup(m.StopAllTunnels)

	err = m.RestoreAllTunnels()
	if err == nil {
		t.Fatal("RestoreAllTunnels reported a startup that could not read what should be running as done")
	}
	if !log.has(deleteTunnels) {
		t.Fatal("RestoreAllTunnels never dropped the rows of the previous process")
	}
	if running := runningKeys(m); len(running) != 0 {
		t.Fatalf("RestoreAllTunnels left %v running after it failed", running)
	}
}

// TestTunnelAddressesBracketAnIPv6Host pins the bracketing an IPv6 address
// needs. Written as "2001:db8::1:22" the port cannot be told from the last
// group of the address, and nothing that dials can take it apart. The API has
// accepted IPv6 all along, since the validator behind the "ip" tag takes it, so
// what this guards is the only place that made those addresses unusable.
func TestTunnelAddressesBracketAnIPv6Host(t *testing.T) {
	cases := []struct {
		name       string
		hostIP     string
		serviceIP  string
		wantServer string
		wantRemote string
	}{
		{"both v4", "192.0.2.1", "203.0.113.5", "192.0.2.1:22", "203.0.113.5:5432"},
		{"both v6", "2001:db8::1", "2001:db8::25", "[2001:db8::1]:22", "[2001:db8::25]:5432"},
		{"v6 host, v4 service", "::1", "203.0.113.5", "[::1]:22", "203.0.113.5:5432"},
		{"v4 host, v6 service", "192.0.2.1", "::ffff:203.0.113.5", "192.0.2.1:22", "[::ffff:203.0.113.5]:5432"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := &models.Host{IP: tc.hostIP, Port: 22}
			sp := &models.ServicePort{ServiceIP: tc.serviceIP, ServicePort: 5432, LocalPort: 15432}

			local, server, remote := tunnelAddresses(host, sp)

			if server != tc.wantServer {
				t.Errorf("server = %q, want %q", server, tc.wantServer)
			}
			if remote != tc.wantRemote {
				t.Errorf("remote = %q, want %q", remote, tc.wantRemote)
			}

			// Whatever the addresses are, each one has to come apart again into
			// a host and a port. That is what every dialer does with them.
			for name, addr := range map[string]string{"local": local, "server": server, "remote": remote} {
				_, _, err := net.SplitHostPort(addr)
				if err != nil {
					t.Errorf("the %s address %q cannot be split into a host and a port: %v", name, addr, err)
				}
			}
		})
	}
}
