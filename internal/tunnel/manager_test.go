package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/crypto/ssh"
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

	host := models.Host{ID: 1, Address: "127.0.0.1", Port: 22, User: "user", Password: "pass", Enabled: false}
	sp := models.ServicePort{ID: 2, ServiceAddress: "127.0.0.1", ServicePort: 3306, LocalPort: 13306}

	err = m.StartTunnel(&host, &sp, models.BindScopeWildcard)
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

	host := models.Host{ID: 1, Address: "127.0.0.1", Port: 22, User: "user", Password: "pass", Enabled: true}
	sp := models.ServicePort{ID: 2, ServiceAddress: "127.0.0.1", ServicePort: 3306, LocalPort: 13306}

	// The failing database stops StartTunnel at the tunnel row, which it only
	// reaches for a Host that is not skipped, so no SSH connection is attempted.
	err = m.StartTunnel(&host, &sp, models.BindScopeWildcard)
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

	host := models.Host{ID: 1, Address: "127.0.0.1", Port: 22, User: "user", Password: "pass", Enabled: true}
	sp := models.ServicePort{ID: 2, ServiceAddress: "127.0.0.1", ServicePort: 3306, LocalPort: 13306}

	err = m.StartTunnel(&host, &sp, models.BindScopeWildcard)
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
	err = m.StartTunnel(&host, &sp, models.BindScopeWildcard)
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

	host := models.Host{ID: 1, Address: "127.0.0.1", Port: 22, User: "user", Password: encrypted, Enabled: true}

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

	host := models.Host{ID: 1, Address: "127.0.0.1", Port: 22, User: "user", Password: "s3cr3t", Enabled: true}

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

	host := models.Host{ID: 1, Address: "127.0.0.1", Port: 22, User: "user", Password: stored, Enabled: true}

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

	host := models.Host{ID: 1, Address: "127.0.0.1", Port: 22, User: "user", Password: stored, Enabled: true}

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

	host := models.Host{ID: 1, Address: "127.0.0.1", Port: 22, User: "user", Password: "s3cr3t", Enabled: true}

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

	host := models.Host{ID: 1, Address: "127.0.0.1", Port: 22, User: "user", Password: stored, Enabled: true}

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

	host := models.Host{ID: 1, Address: "127.0.0.1", Port: 22, User: "user", Password: stored, Enabled: true}
	sp := models.ServicePort{ID: 2, ServiceAddress: "127.0.0.1", ServicePort: 3306, LocalPort: 13306}

	err = m.StartTunnel(&host, &sp, models.BindScopeWildcard)
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

// allAssignments pairs every Host with every service port. That is the state
// the assignment table is filled with the first time it is created, and it is
// what the tests that are not about the assignments themselves run against, so
// that they pin down what they did before the table existed.
func allAssignments(hosts []models.Host, sps []models.ServicePort) []models.HostServicePort {
	assignments := make([]models.HostServicePort, 0, len(hosts)*len(sps))
	for i := range hosts {
		for j := range sps {
			assignments = append(assignments, models.HostServicePort{
				HostID: hosts[i].ID,
				SPID:   sps[j].ID,
			})
		}
	}

	return assignments
}

// newStubDB returns a stub over hosts and sps with every combination of the two
// assigned. A non-nil deleteErr makes every delete fail.
func newStubDB(t *testing.T, hosts []models.Host, sps []models.ServicePort, deleteErr error) *gorm.DB {
	t.Helper()

	return newAssignedStubDB(t, hosts, sps, allAssignments(hosts, sps), deleteErr)
}

// newAssignedStubDB returns a gorm DB that answers Host, ServicePort and
// assignment queries from memory, so no statement reaches the failing
// connection pool. A non-nil deleteErr makes every delete fail.
func newAssignedStubDB(t *testing.T, hosts []models.Host, sps []models.ServicePort,
	assignments []models.HostServicePort, deleteErr error) *gorm.DB {
	t.Helper()

	db := newFailingDB(t)

	err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		switch dest := tx.Statement.Dest.(type) {
		case *[]models.Host:
			*dest = hosts
		case *[]models.ServicePort:
			*dest = sps
		case *[]models.HostServicePort:
			*dest = assignments
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

func TestStopTunnelUnregistersTheTunnelWhenTheRowCannotBeDeleted(t *testing.T) {
	hosts := []models.Host{{ID: 1, Address: "127.0.0.1", Port: 1, User: "user", Enabled: true}}
	sps := []models.ServicePort{{ID: 2, ServiceAddress: "127.0.0.1", ServicePort: 8081, LocalPort: 18081}}

	m, err := NewManager(newStubDB(t, hosts, sps, errConnPoolClosed), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	tun := registerStoppedTunnel(t, m, 1, 2)

	err = m.StopTunnel(1, 2)
	if err == nil {
		t.Fatal("StopTunnel returned no error when the row could not be deleted")
	}
	if !errors.Is(err, errConnPoolClosed) {
		t.Fatalf("StopTunnel returned an error that does not carry the failed delete: %v", err)
	}

	select {
	case <-tun.done:
	default:
		t.Fatal("the tunnel was not stopped")
	}

	if _, exists := runningKeys(m)["1-2"]; exists {
		t.Fatal("a stopped tunnel is still registered after its row could not be deleted")
	}
}

func TestStopAllTunnelsStopsEveryRunningTunnel(t *testing.T) {
	// The rows say nothing about what is running: one of the tunnels belongs
	// to a Host that is no longer there, and it still has to be stopped.
	hosts := []models.Host{{ID: 1, Address: "127.0.0.1", Port: 1, User: "user", Enabled: true}}
	sps := []models.ServicePort{{ID: 2, ServiceAddress: "127.0.0.1", ServicePort: 8081, LocalPort: 18081}}

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
	hosts := []models.Host{{ID: 1, Address: "127.0.0.1", Port: 1, User: "user", Enabled: true}}
	sps := []models.ServicePort{{ID: 2, ServiceAddress: "127.0.0.1", ServicePort: 8081, LocalPort: 18081}}

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
	hosts := []models.Host{{ID: 1, Address: "127.0.0.1", Port: 22, User: "user", Password: "pass", Enabled: true}}
	sps := []models.ServicePort{{ID: 2, ServiceAddress: "127.0.0.1", ServicePort: 8081, LocalPort: 18081}}

	core, logs := observer.New(zapcore.DebugLevel)

	m, err := NewManager(newStubDB(t, hosts, sps, errConnPoolClosed), zap.New(core), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	hostID, spID := hosts[0].ID, sps[0].ID
	tunnel, err := NewSSHTunnel(&hostID, &spID, "0.0.0.0:18081", "[::]:18081", "127.0.0.1:22", "127.0.0.1:8081", nil, zap.NewNop())
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

	assignments := allAssignments(hosts, sps)

	err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		switch dest := tx.Statement.Dest.(type) {
		case *[]models.Host:
			log.add(readHosts)
			*dest = hosts
		case *[]models.ServicePort:
			*dest = sps
		case *[]models.HostServicePort:
			*dest = assignments
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
			host := &models.Host{Address: tc.hostIP, Port: 22}
			sp := &models.ServicePort{ServiceAddress: tc.serviceIP, ServicePort: 5432, LocalPort: 15432}

			localV4, localV6, server, remote := tunnelAddresses(host, sp, models.BindScopeWildcard)

			if server != tc.wantServer {
				t.Errorf("server = %q, want %q", server, tc.wantServer)
			}
			if remote != tc.wantRemote {
				t.Errorf("remote = %q, want %q", remote, tc.wantRemote)
			}

			// Whatever the addresses are, each one has to come apart again into
			// a host and a port. That is what every dialer does with them.
			for name, addr := range map[string]string{
				"local v4": localV4, "local v6": localV6, "server": server, "remote": remote,
			} {
				_, _, err := net.SplitHostPort(addr)
				if err != nil {
					t.Errorf("the %s address %q cannot be split into a host and a port: %v", name, addr, err)
				}
			}
		})
	}
}

// testPrivateKey returns a fresh private key in PEM, protected by passphrase
// when one is given, along with the public key that goes with it.
//
// The key is generated rather than written into the source. A PEM block of a
// private key in a repository reads as a key that leaked whether it is one or
// not, and a key that is made here belongs to this test run alone.
func testPrivateKey(t *testing.T, passphrase string) (string, ssh.PublicKey) {
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

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("failed to build a signer: %v", err)
	}

	return string(pem.EncodeToMemory(block)), signer.PublicKey()
}

func TestParsePrivateKeyReadsAKeyThatHasNoPassphrase(t *testing.T) {
	keyPEM, public := testPrivateKey(t, "")

	signer, err := ParsePrivateKey(keyPEM, "")
	if err != nil {
		t.Fatalf("ParsePrivateKey returned an error: %v", err)
	}

	if !bytes.Equal(signer.PublicKey().Marshal(), public.Marshal()) {
		t.Fatal("ParsePrivateKey returned a signer for another key")
	}
}

// TestParsePrivateKeyIgnoresAPassphraseTheKeyDoesNotNeed covers the operator
// who fills both boxes for a key that is not protected. The library refuses
// that pair on its own, and a refusal there would read as a key that is broken.
func TestParsePrivateKeyIgnoresAPassphraseTheKeyDoesNotNeed(t *testing.T) {
	keyPEM, public := testPrivateKey(t, "")

	signer, err := ParsePrivateKey(keyPEM, "not the passphrase of anything")
	if err != nil {
		t.Fatalf("ParsePrivateKey returned an error: %v", err)
	}

	if !bytes.Equal(signer.PublicKey().Marshal(), public.Marshal()) {
		t.Fatal("ParsePrivateKey returned a signer for another key")
	}
}

func TestParsePrivateKeyReadsAKeyWithItsPassphrase(t *testing.T) {
	keyPEM, public := testPrivateKey(t, "the passphrase of the test")

	signer, err := ParsePrivateKey(keyPEM, "the passphrase of the test")
	if err != nil {
		t.Fatalf("ParsePrivateKey returned an error: %v", err)
	}

	if !bytes.Equal(signer.PublicKey().Marshal(), public.Marshal()) {
		t.Fatal("ParsePrivateKey returned a signer for another key")
	}
}

// TestParsePrivateKeyRefusalsSayWhatIsWrong is the whole of what a refusal has
// to do: come back as a KeyError, so the API answers it as a bad request, and
// say which of the three things went wrong, so the operator knows whether to
// go back to the key box or to the passphrase box.
func TestParsePrivateKeyRefusalsSayWhatIsWrong(t *testing.T) {
	locked, _ := testPrivateKey(t, "the passphrase of the test")
	open, _ := testPrivateKey(t, "")

	cases := []struct {
		name       string
		keyPEM     string
		passphrase string
		want       string
	}{
		{"not PEM", "this is not a key at all", "", "not PEM"},
		{"PEM that holds no key", "-----BEGIN NOT A KEY-----\nnonsense\n-----END NOT A KEY-----", "",
			"cannot be read"},
		{"a passphrase that was not given", locked, "", "protected by a passphrase"},
		{"a passphrase that is wrong", locked, "not the passphrase", "does not open"},
		{"no key at all", "   ", "", "no private key"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			signer, err := ParsePrivateKey(tc.keyPEM, tc.passphrase)
			if err == nil {
				t.Fatal("ParsePrivateKey took a key it should have refused")
			}
			if signer != nil {
				t.Fatal("ParsePrivateKey returned a signer along with the refusal")
			}

			var refused *KeyError
			if !errors.As(err, &refused) {
				t.Fatalf("the refusal is not a KeyError: %v", err)
			}
			if !strings.Contains(refused.Error(), tc.want) {
				t.Fatalf("the refusal is %q, which does not say %q", refused.Error(), tc.want)
			}
			if strings.Contains(refused.Error(), tc.passphrase) && tc.passphrase != "" {
				t.Fatalf("the refusal carries the passphrase: %q", refused.Error())
			}
		})
	}

	// A key that is fine is not refused by any of the above.
	_, err := ParsePrivateKey(open, "")
	if err != nil {
		t.Fatalf("a key that is in order was refused: %v", err)
	}
}

// sealedHost returns a Host whose key, passphrase and password are sealed with
// the cipher, the way the API stores them.
func sealedHost(t *testing.T, c *crypto.Cipher, keyPEM, passphrase, password string) *models.Host {
	t.Helper()

	seal := func(value string) string {
		if value == "" {
			return ""
		}

		sealed, err := c.Encrypt(value)
		if err != nil {
			t.Fatalf("failed to encrypt: %v", err)
		}

		return sealed
	}

	return &models.Host{
		ID:            1,
		Address:       "127.0.0.1",
		Port:          22,
		User:          "user",
		Password:      seal(password),
		PrivateKey:    seal(keyPEM),
		KeyPassphrase: seal(passphrase),
		Enabled:       true,
	}
}

// authMethodNames is what hostAuth built, named by type. The SSH protocol
// offers the methods in the order they are in, so the order of this list is the
// order the Host is asked with.
func authMethodNames(methods []ssh.AuthMethod) []string {
	names := make([]string, 0, len(methods))
	for _, method := range methods {
		names = append(names, fmt.Sprintf("%T", method))
	}

	return names
}

func TestHostAuthOffersTheKeyBeforeThePassword(t *testing.T) {
	cipher := newTestCipher(t)
	keyPEM, _ := testPrivateKey(t, "")

	m, err := NewManager(newFailingDB(t), zap.NewNop(), cipher, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	methods, creds, err := m.hostAuth(sealedHost(t, cipher, keyPEM, "", "s3cr3t"))
	if err != nil {
		t.Fatalf("hostAuth returned an error: %v", err)
	}

	names := authMethodNames(methods)
	if len(names) != 2 {
		t.Fatalf("hostAuth built %d methods (%v), want the key and the password", len(names), names)
	}
	if !strings.Contains(names[0], "publicKey") {
		t.Fatalf("the first method is %q, want the key", names[0])
	}
	if !strings.Contains(names[1], "password") {
		t.Fatalf("the second method is %q, want the password", names[1])
	}
	if creds.password != "s3cr3t" {
		t.Fatal("hostAuth did not hand back the password the fingerprint is taken over")
	}
}

func TestHostAuthOffersOnlyWhatTheHostCarries(t *testing.T) {
	cipher := newTestCipher(t)
	keyPEM, _ := testPrivateKey(t, "")

	m, err := NewManager(newFailingDB(t), zap.NewNop(), cipher, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	cases := []struct {
		name     string
		host     *models.Host
		wantWith string
	}{
		{"the key alone", sealedHost(t, cipher, keyPEM, "", ""), "publicKey"},
		{"the password alone", sealedHost(t, cipher, "", "", "s3cr3t"), "password"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			methods, _, err := m.hostAuth(tc.host)
			if err != nil {
				t.Fatalf("hostAuth returned an error: %v", err)
			}

			names := authMethodNames(methods)
			if len(names) != 1 {
				t.Fatalf("hostAuth built %d methods (%v), want one", len(names), names)
			}
			if !strings.Contains(names[0], tc.wantWith) {
				t.Fatalf("the method is %q, want %q", names[0], tc.wantWith)
			}
		})
	}
}

// TestHostAuthReportsAHostWithNothingToLogInWith is the case a Host cannot be
// registered in any more, and could still be reached by a row written by hand.
// It must not come out as an SSH connection that offers nothing and hangs.
func TestHostAuthReportsAHostWithNothingToLogInWith(t *testing.T) {
	m, err := NewManager(newFailingDB(t), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	host := &models.Host{ID: 7, Address: "127.0.0.1", Port: 22, User: "user", Enabled: true}

	methods, _, err := m.hostAuth(host)
	if err == nil {
		t.Fatal("hostAuth built a connection for a Host that carries no way in")
	}
	if methods != nil {
		t.Fatal("hostAuth returned methods along with the error")
	}
	if !strings.Contains(err.Error(), "neither a private key nor a password") {
		t.Fatalf("the error does not say what is missing: %v", err)
	}
}

// TestHostAuthFallsBackToThePasswordWhenTheKeyCannotBeRead is what keeps a
// running installation up. A key that cannot be built is not a reason to stop
// connecting to a Host that also carries a password.
func TestHostAuthFallsBackToThePasswordWhenTheKeyCannotBeRead(t *testing.T) {
	cipher := newTestCipher(t)

	core, logs := observer.New(zapcore.DebugLevel)

	m, err := NewManager(newFailingDB(t), zap.New(core), cipher, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	host := sealedHost(t, cipher, "this is not a key at all", "", "s3cr3t")

	methods, creds, err := m.hostAuth(host)
	if err != nil {
		t.Fatalf("hostAuth returned an error: %v", err)
	}

	names := authMethodNames(methods)
	if len(names) != 1 || !strings.Contains(names[0], "password") {
		t.Fatalf("hostAuth built %v, want the password alone", names)
	}
	if creds.password != "s3cr3t" {
		t.Fatal("hostAuth did not hand back the password")
	}

	errors := 0
	for _, entry := range logs.All() {
		if entry.Level >= zapcore.ErrorLevel {
			errors++
		}
	}
	if errors == 0 {
		t.Fatal("a stored key that cannot be read was not logged")
	}
}

// TestHostAuthReportsAKeyThatDoesNotDecrypt covers the Host that carries a key
// and nothing else, sealed with another encryption key. There is nothing to
// fall back on, and the row is left exactly as it is.
func TestHostAuthReportsAKeyThatDoesNotDecrypt(t *testing.T) {
	keyPEM, _ := testPrivateKey(t, "")
	stored := sealedHost(t, newTestCipher(t), keyPEM, "", "")

	updates := 0

	m, err := NewManager(newUpdateCountingDB(t, &updates), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	sealed := stored.PrivateKey

	_, _, err = m.hostAuth(stored)
	if !errors.Is(err, crypto.ErrWrongKey) {
		t.Fatalf("hostAuth did not report a wrong encryption key: %v", err)
	}
	if updates != 0 {
		t.Fatalf("hostAuth sent %d updates to the hosts table with a wrong encryption key", updates)
	}
	if stored.PrivateKey != sealed {
		t.Fatal("hostAuth changed the stored private key although the encryption key is wrong")
	}
}

// startKeyAndPasswordSSHServer speaks SSH and records the methods it is offered
// in the order they arrive. The key it accepts is the one that is handed in,
// and the password it accepts is accept; anything else is refused, which is
// what makes the client move on to the next method.
func startKeyAndPasswordSSHServer(t *testing.T, accepted ssh.PublicKey, accept string,
	offered *[]string, mu *sync.Mutex) string {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate host key: %v", err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("failed to create signer: %v", err)
	}

	note := func(method string) {
		mu.Lock()
		*offered = append(*offered, method)
		mu.Unlock()
	}

	config := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			note("publickey")

			if accepted != nil && bytes.Equal(key.Marshal(), accepted.Marshal()) {
				return &ssh.Permissions{}, nil
			}

			return nil, errors.New("key refused")
		},
		PasswordCallback: func(_ ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			note("password")

			if accept != "" && string(password) == accept {
				return &ssh.Permissions{}, nil
			}

			return nil, errors.New("password refused")
		},
	}
	config.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			go func(conn net.Conn) {
				sshConn, chans, reqs, err := ssh.NewServerConn(conn, config)
				if err != nil {
					_ = conn.Close()
					return
				}

				go ssh.DiscardRequests(reqs)
				go func() {
					for newChannel := range chans {
						_ = newChannel.Reject(ssh.Prohibited, "nothing is served here")
					}
				}()

				_ = sshConn.Close()
			}(conn)
		}
	}()

	return ln.Addr().String()
}

// TestTheHostIsAskedWithTheKeyFirst is the order as the protocol sees it rather
// than as the slice is built. The server records what it was offered, so what
// is pinned here is what a real sshd would have been asked with.
func TestTheHostIsAskedWithTheKeyFirst(t *testing.T) {
	cipher := newTestCipher(t)
	keyPEM, public := testPrivateKey(t, "")

	cases := []struct {
		name string
		// accepted is the key the server takes, and accept the password it
		// takes. An empty one of either is a server that refuses it.
		accepted ssh.PublicKey
		accept   string
		want     []string
	}{
		{"the key is taken", public, "", []string{"publickey"}},
		{"the key is refused and the password is taken", nil, "s3cr3t", []string{"publickey", "password"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var offered []string

			addr := startKeyAndPasswordSSHServer(t, tc.accepted, tc.accept, &offered, &mu)

			m, err := NewManager(newFailingDB(t), zap.NewNop(), cipher, 1)
			if err != nil {
				t.Fatalf("failed to create manager: %v", err)
			}

			methods, _, err := m.hostAuth(sealedHost(t, cipher, keyPEM, "", "s3cr3t"))
			if err != nil {
				t.Fatalf("hostAuth returned an error: %v", err)
			}

			client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
				User:            "user",
				Auth:            methods,
				HostKeyCallback: ssh.InsecureIgnoreHostKey(),
				Timeout:         10 * time.Second,
			})
			if err != nil {
				t.Fatalf("the connection was not made: %v", err)
			}
			_ = client.Close()

			mu.Lock()
			got := append([]string(nil), offered...)
			mu.Unlock()

			// The client offers "publickey" once to ask whether the key would
			// be taken and once to prove it, so the same method twice in a row
			// is one attempt.
			if !reflect.DeepEqual(dedupeAdjacent(got), tc.want) {
				t.Fatalf("the Host was asked with %v, want %v", got, tc.want)
			}
		})
	}
}

// dedupeAdjacent drops a repeat of the method just before it.
func dedupeAdjacent(methods []string) []string {
	out := make([]string, 0, len(methods))
	for _, method := range methods {
		if len(out) > 0 && out[len(out)-1] == method {
			continue
		}
		out = append(out, method)
	}

	return out
}

// TestHostPasswordLeavesAHostThatCarriesNoneAlone is what keeps a Host
// registered with a key alone from growing a password. The empty value looks
// exactly like a password stored before passwords were encrypted, and the
// migration would seal it into the row, after which the Host is offered an
// empty password on every connection and nothing can tell it from one that was
// chosen. The reconcile pass reads the password of every Host it watches, so
// this runs on every pass.
func TestHostPasswordLeavesAHostThatCarriesNoneAlone(t *testing.T) {
	updates := 0

	m, err := NewManager(newUpdateCountingDB(t, &updates), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	host := models.Host{ID: 1, Address: "127.0.0.1", Port: 22, User: "user", Enabled: true}

	password, err := m.hostPassword(&host)
	if err != nil {
		t.Fatalf("hostPassword returned an error: %v", err)
	}
	if password != "" {
		t.Fatalf("hostPassword returned %q for a Host that carries no password", password)
	}
	if updates != 0 {
		t.Fatalf("hostPassword sent %d updates for a Host that carries no password", updates)
	}
	if host.Password != "" {
		t.Fatalf("hostPassword put %q in the row of a Host that carries no password", host.Password)
	}
}

// newTunnelCreateStubDB returns a gorm DB whose queries find nothing, which is
// what sends FirstOrCreate down its create path, and which records the tunnel
// row that is created there instead of writing one.
func newTunnelCreateStubDB(t *testing.T, created *models.Tunnel) *gorm.DB {
	t.Helper()

	db := newFailingDB(t)

	err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		tx.RowsAffected = 0
	})
	if err != nil {
		t.Fatalf("failed to replace the query callback: %v", err)
	}

	err = db.Callback().Create().Replace("gorm:create", func(tx *gorm.DB) {
		row, ok := tx.Statement.Dest.(*models.Tunnel)
		if !ok {
			return
		}

		*created = *row
	})
	if err != nil {
		t.Fatalf("failed to replace the create callback: %v", err)
	}

	return db
}

// TestStartTunnelWritesARowThatSaysNothingWasMeasured pins what a tunnel row
// carries before it is connected. A row that is only being started has no
// reading of the forwarded port, and it says so rather than leaving the field
// empty: a port that was never tried must not read like one that was.
func TestStartTunnelWritesARowThatSaysNothingWasMeasured(t *testing.T) {
	var created models.Tunnel

	m, err := NewManager(newTunnelCreateStubDB(t, &created), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	// A port nothing listens on, so the connection the tunnel makes in the
	// background is refused at once rather than waiting on a handshake.
	_, port, err := net.SplitHostPort(closedPort(t))
	if err != nil {
		t.Fatalf("failed to read the port: %v", err)
	}

	hostPort, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("failed to read the port: %v", err)
	}

	host := models.Host{ID: 1, Address: "127.0.0.1", Port: hostPort, User: "user", Password: "pass", Enabled: true}
	sp := models.ServicePort{ID: 2, ServiceAddress: "127.0.0.1", ServicePort: 3306, LocalPort: 13306}

	err = m.StartTunnel(&host, &sp, models.BindScopeWildcard)
	if err != nil {
		t.Fatalf("StartTunnel returned an error: %v", err)
	}
	t.Cleanup(func() {
		_ = m.StopTunnel(host.ID, sp.ID)
	})

	if created.ForwardReach != forwardReachUnknown {
		t.Fatalf("the created row says forward reach %q, want %q, nothing has been measured yet",
			created.ForwardReach, forwardReachUnknown)
	}
	if created.ServerBanner != "" {
		t.Fatalf("the created row carries a banner %q before a handshake happened", created.ServerBanner)
	}
}

// testHostKey generates a public key to stand for the one an SSH server
// presents.
func testHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()

	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate a host key: %v", err)
	}

	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatalf("failed to read the generated host key: %v", err)
	}

	return key
}

// TestAHostKeyIsStoredAsOneLineAndShownAsItsFingerprint pins both halves of
// the form the Host row holds a key in. The stored form is what two keys are
// compared as, so it has to be one line and the same line for the same key,
// and the fingerprint is what is put in front of an operator, so it has to be
// the string ssh(1) prints for that key and nothing of our own.
func TestAHostKeyIsStoredAsOneLineAndShownAsItsFingerprint(t *testing.T) {
	public := testHostKey(t)
	stored := MarshalHostKey(public)

	if strings.ContainsAny(stored, "\r\n") {
		t.Fatalf("the stored form %q is not a single line", stored)
	}
	if fields := strings.Fields(stored); len(fields) != 2 {
		t.Fatalf("the stored form %q is not \"<algorithm> <base64>\"", stored)
	}

	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(stored))
	if err != nil {
		t.Fatalf("the stored form cannot be read back as a key: %v", err)
	}
	if !bytes.Equal(parsed.Marshal(), public.Marshal()) {
		t.Fatal("the stored form reads back as a different key")
	}

	fingerprint := HostKeyFingerprint(stored)
	if fingerprint != ssh.FingerprintSHA256(public) {
		t.Fatalf("the fingerprint of the stored key is %q, want %q",
			fingerprint, ssh.FingerprintSHA256(public))
	}
	if !strings.HasPrefix(fingerprint, "SHA256:") {
		t.Fatalf("the fingerprint %q does not say which digest it is", fingerprint)
	}
	if strings.Contains(fingerprint, "=") {
		t.Fatalf("the fingerprint %q is padded, which is not what ssh(1) prints", fingerprint)
	}

	// A Host that carries no key and a row somebody wrote by hand both leave
	// the screen without a fingerprint rather than taking it down.
	for _, stored := range []string{"", "   ", "this is not a key"} {
		if got := HostKeyFingerprint(stored); got != "" {
			t.Fatalf("HostKeyFingerprint(%q) = %q, want no fingerprint at all", stored, got)
		}
	}
}

// newPendingHostKeyStubDB returns a stub that records every value written to
// the pending_host_key column, so a test can see what a refused connection
// wrote down for approval. A non-nil updateErr makes every update fail.
func newPendingHostKeyStubDB(t *testing.T, written *[]string, mu *sync.Mutex, updateErr error) *gorm.DB {
	t.Helper()

	db := newFailingDB(t)

	err := db.Callback().Update().Replace("gorm:update", func(tx *gorm.DB) {
		if updateErr != nil {
			_ = tx.AddError(updateErr)

			return
		}

		values, ok := tx.Statement.Dest.(map[string]interface{})
		if !ok {
			return
		}

		value, ok := values["pending_host_key"].(string)
		if !ok {
			return
		}

		mu.Lock()
		*written = append(*written, value)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("failed to replace the update callback: %v", err)
	}

	// A tunnel row that is saved reaches the create callback, because gorm
	// falls back to an insert when the update it ran changed no row.
	err = db.Callback().Create().Replace("gorm:create", func(tx *gorm.DB) {})
	if err != nil {
		t.Fatalf("failed to replace the create callback: %v", err)
	}

	return db
}

// startHostKeySSHServer speaks SSH with a host key of its own and asks nothing
// of the client. It hands back the address, the key it presents and a count of
// the connections it accepted, which is what says whether a client that was
// refused kept trying.
func startHostKeySSHServer(t *testing.T) (string, ssh.PublicKey, *atomic.Int64) {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate host key: %v", err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("failed to create signer: %v", err)
	}

	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		_ = ln.Close()
	})

	var accepted atomic.Int64

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			accepted.Add(1)

			go func(conn net.Conn) {
				sshConn, chans, reqs, err := ssh.NewServerConn(conn, config)
				if err != nil {
					_ = conn.Close()

					return
				}

				go ssh.DiscardRequests(reqs)
				go func() {
					for newChannel := range chans {
						_ = newChannel.Reject(ssh.Prohibited, "nothing is served here")
					}
				}()

				// Held open until the test is over, so a client that got
				// through the handshake is not raced by a server closing
				// under it.
				<-done
				_ = sshConn.Close()
			}(conn)
		}
	}()

	return ln.Addr().String(), signer.PublicKey(), &accepted
}

// TestTheHostKeyDecidesWhetherTheConnectionIsMade runs the three answers
// against a handshake with a real SSH server rather than against the callback
// on its own. What is pinned that way is the key as x/crypto/ssh presents it
// against the form the Host row holds, which is the comparison that has to
// hold for a Host to connect at all.
func TestTheHostKeyDecidesWhetherTheConnectionIsMade(t *testing.T) {
	addr, presented, _ := startHostKeySSHServer(t)
	stored := MarshalHostKey(presented)

	cases := []struct {
		name    string
		trusted string
		// want is the status the refusal carries, and an empty one is a
		// connection that is made.
		want string
	}{
		{"a Host that carries no approved key", "", StatusHostKeyUnapproved},
		{"a Host that is trusted on another key", MarshalHostKey(testHostKey(t)), StatusHostKeyMismatch},
		{"a Host that is trusted on the key the server presents", stored, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var written []string

			m, err := NewManager(newPendingHostKeyStubDB(t, &written, &mu, nil),
				zap.NewNop(), newTestCipher(t), 1)
			if err != nil {
				t.Fatalf("failed to create manager: %v", err)
			}

			host := &models.Host{ID: 7, Address: "127.0.0.1", Port: 22, User: "user",
				HostKey: tc.trusted, Enabled: true}

			client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
				User:            "user",
				HostKeyCallback: m.hostKeyCallback(host),
				Timeout:         10 * time.Second,
			})

			mu.Lock()
			got := append([]string(nil), written...)
			mu.Unlock()

			if tc.want == "" {
				if err != nil {
					t.Fatalf("the connection to a server presenting the approved key was refused: %v", err)
				}
				_ = client.Close()

				if len(got) != 0 {
					t.Fatalf("the approved key was written down for approval: %v", got)
				}

				return
			}

			if err == nil {
				_ = client.Close()
				t.Fatal("the connection was made to a server whose key is not the approved one")
			}

			refusal := hostKeyRefusal(err)
			if refusal == nil {
				t.Fatalf("the handshake failed with %v, which is not a host key refusal", err)
			}
			if refusal.Status != tc.want {
				t.Fatalf("the refusal carries the status %q, want %q", refusal.Status, tc.want)
			}
			if refusal.Presented != stored {
				t.Fatalf("the refusal carries the key %q, want the one the server presented, %q",
					refusal.Presented, stored)
			}
			if !strings.Contains(refusal.Error(), ssh.FingerprintSHA256(presented)) {
				t.Fatalf("the refusal %q does not name the fingerprint that was presented", refusal.Error())
			}

			if !reflect.DeepEqual(got, []string{stored}) {
				t.Fatalf("the keys written down for approval are %v, want the presented one alone, %v",
					got, []string{stored})
			}
		})
	}
}

// TestAHostKeyThatCannotBeWrittenDownStillRefusesTheConnection is the database
// being away while a key is refused. Letting the connection through would make
// a failure to write a row into a way past the check, and the refusal says
// that the key could not be stored so that the operator is not left looking
// for an approval that never appeared.
func TestAHostKeyThatCannotBeWrittenDownStillRefusesTheConnection(t *testing.T) {
	addr, presented, _ := startHostKeySSHServer(t)

	var mu sync.Mutex
	var written []string

	m, err := NewManager(newPendingHostKeyStubDB(t, &written, &mu, errConnPoolClosed),
		zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	host := &models.Host{ID: 7, Address: "127.0.0.1", Port: 22, User: "user", Enabled: true}

	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "user",
		HostKeyCallback: m.hostKeyCallback(host),
		Timeout:         10 * time.Second,
	})
	if err == nil {
		_ = client.Close()
		t.Fatal("the connection was made although the key that was presented could not be written down")
	}

	refusal := hostKeyRefusal(err)
	if refusal == nil {
		t.Fatalf("the handshake failed with %v, which is not a host key refusal", err)
	}
	if refusal.Presented != MarshalHostKey(presented) {
		t.Fatal("the refusal does not carry the key the server presented")
	}
	if !strings.Contains(err.Error(), "could not be stored") {
		t.Fatalf("the refusal %q does not say that the key could not be stored", err.Error())
	}
}

// TestATunnelRefusedOnItsHostKeyStopsTrying is what keeps the refusal from
// filling the log. Nothing a tunnel does turns an unapproved key into an
// approved one, so a tunnel that retried would connect, be refused and write a
// line every interval for as long as the process runs, once per tunnel of the
// Host. It gives up instead, leaves the row saying which refusal it was, and
// the reconcile pass builds it again once the key is approved, which
// TestReconcileRebuildsATunnelWhenItsHostKeyIsApproved covers.
func TestATunnelRefusedOnItsHostKeyStopsTrying(t *testing.T) {
	addr, presented, accepted := startHostKeySSHServer(t)

	var mu sync.Mutex
	var written []string

	m, err := NewManager(newPendingHostKeyStubDB(t, &written, &mu, nil),
		zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	host := &models.Host{ID: 1, Address: "127.0.0.1", Port: 22, User: "user", Password: "pass", Enabled: true}

	hostID, spID := host.ID, uint(2)
	tun, err := NewSSHTunnel(&hostID, &spID, "0.0.0.0:18099", "[::]:18099", addr, "127.0.0.1:1",
		&ssh.ClientConfig{
			User:            host.User,
			Auth:            []ssh.AuthMethod{ssh.Password("pass")},
			HostKeyCallback: m.hostKeyCallback(host),
			Timeout:         10 * time.Second,
		}, zap.NewNop())
	if err != nil {
		t.Fatalf("failed to create tunnel: %v", err)
	}

	tunnel := &models.Tunnel{HostID: hostID, SPID: spID, Status: "starting"}

	returned := make(chan struct{})
	go func() {
		tun.Start(m, tunnel)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		_ = tun.Stop(m)
		t.Fatal("the Start loop is still going at a connection the host key check refuses")
	}

	tun.tunnelMu.Lock()
	status := tunnel.Status
	tun.tunnelMu.Unlock()

	if status != StatusHostKeyUnapproved {
		t.Fatalf("the tunnel row says %q, want %q", status, StatusHostKeyUnapproved)
	}

	// The monitoring interval is a second, so a loop that kept retrying would
	// have connected several more times by now.
	time.Sleep(2500 * time.Millisecond)

	if got := accepted.Load(); got != 1 {
		t.Fatalf("the server was connected to %d times, want the one attempt that was refused", got)
	}

	mu.Lock()
	got := append([]string(nil), written...)
	mu.Unlock()

	if !reflect.DeepEqual(got, []string{MarshalHostKey(presented)}) {
		t.Fatalf("the keys written down for approval are %v, want the presented one alone", got)
	}
}

// TestBindScopeNamesAPairOfAddresses pins the two addresses each scope asks
// for. The two families do not stand in for each other, so a scope that came
// out as one address would leave whoever chose it reachable over one of them
// and not the other, with nothing on the screen saying so.
func TestBindScopeNamesAPairOfAddresses(t *testing.T) {
	cases := []struct {
		name   string
		scope  string
		wantV4 string
		wantV6 string
	}{
		{"loopback", models.BindScopeLoopback, "127.0.0.1", "::1"},
		{"wildcard", models.BindScopeWildcard, "0.0.0.0", "::"},
		{"the empty value is the wildcard", "", "0.0.0.0", "::"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotV4, gotV6 := bindScopeAddresses(tc.scope)
			if gotV4 != tc.wantV4 || gotV6 != tc.wantV6 {
				t.Fatalf("bindScopeAddresses(%q) = (%q, %q), want (%q, %q)",
					tc.scope, gotV4, gotV6, tc.wantV4, tc.wantV6)
			}
		})
	}
}

// TestTunnelAddressesCarriesTheScopeOfTheAssignment holds the local addresses
// to the assignment rather than to a constant. Before the scope was read here
// every forward was requested on 0.0.0.0, so a tunnel that ignores it is a
// tunnel that opens a port to everything on a Host somebody asked to keep it
// off.
func TestTunnelAddressesCarriesTheScopeOfTheAssignment(t *testing.T) {
	host := &models.Host{Address: "192.0.2.1", Port: 22}
	sp := &models.ServicePort{ServiceAddress: "203.0.113.5", ServicePort: 5432, LocalPort: 15432}

	cases := []struct {
		scope  string
		wantV4 string
		wantV6 string
	}{
		{models.BindScopeLoopback, "127.0.0.1:15432", "[::1]:15432"},
		{models.BindScopeWildcard, "0.0.0.0:15432", "[::]:15432"},
		{"", "0.0.0.0:15432", "[::]:15432"},
	}

	for _, tc := range cases {
		t.Run(tc.scope, func(t *testing.T) {
			localV4, localV6, _, _ := tunnelAddresses(host, sp, tc.scope)
			if localV4 != tc.wantV4 {
				t.Errorf("local IPv4 address = %q, want %q", localV4, tc.wantV4)
			}
			if localV6 != tc.wantV6 {
				t.Errorf("local IPv6 address = %q, want %q", localV6, tc.wantV6)
			}
		})
	}
}
