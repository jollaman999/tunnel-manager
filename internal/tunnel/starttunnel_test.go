package tunnel

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// dbHeldFor is how long a test holds the database before letting it go whatever
// happens, so that a call that waits for it is measured rather than left
// hanging. A call that does not wait for it answers in a small part of this.
const dbHeldFor = time.Second

// newSingleConnDB opens a database on a file with the one connection the
// process runs with, so that a transaction left open keeps every other
// statement waiting the way a long one does in the process.
func newSingleConnDB(t *testing.T) *gorm.DB {
	t.Helper()

	sqlDB, err := sql.Open("sqlite", t.TempDir()+"/tunnel-manager.db")
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() {
		_ = sqlDB.Close()
	})

	db, err := gorm.Open(sqlite.Dialector{Conn: sqlDB}, &gorm.Config{
		Logger:                 logger.Discard,
		SkipDefaultTransaction: true,
	})
	if err != nil {
		t.Fatalf("failed to open gorm: %v", err)
	}

	err = db.AutoMigrate(&models.Tunnel{})
	if err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	return db
}

// queryEntered returns a channel that is closed the first time a query whose
// result is read into a value of the type of dest is about to be sent, which
// is the moment the caller starts to wait for the connection.
func queryEntered[T any](t *testing.T, db *gorm.DB, name string) <-chan struct{} {
	t.Helper()

	entered := make(chan struct{})
	var once sync.Once

	err := db.Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(T); ok {
			once.Do(func() { close(entered) })
		}
	})
	if err != nil {
		t.Fatalf("failed to register the query callback: %v", err)
	}

	return entered
}

// holdConnection takes the one connection of db in a transaction and gives it
// back after dbHeldFor.
func holdConnection(t *testing.T, db *gorm.DB) {
	t.Helper()

	tx := db.Begin()
	if tx.Error != nil {
		t.Fatalf("failed to begin the transaction that holds the connection: %v", tx.Error)
	}

	releaseTimer := time.AfterFunc(dbHeldFor, func() { tx.Rollback() })
	t.Cleanup(func() {
		if releaseTimer.Stop() {
			tx.Rollback()
		}
	})
}

// TestStatusCallsDoNotWaitForTheRowOfATunnelBeingStarted covers a start whose
// row is made while another transaction holds the one connection. The calls
// that answer what runs from memory do not wait for that transaction to end.
func TestStatusCallsDoNotWaitForTheRowOfATunnelBeingStarted(t *testing.T) {
	db := newSingleConnDB(t)
	entered := queryEntered[*models.Tunnel](t, db, "test:tunnel_row_entered")

	cipher := newTestCipher(t)

	m, err := NewManager(db, zap.NewNop(), cipher, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	t.Cleanup(m.StopAllTunnels)

	// The password is sealed already, so the start writes nothing to the Host
	// row and the first statement it waits on is the one for its tunnel row.
	// Port 1 on loopback refuses, so the tunnel started makes no connection.
	host := sealedHost(t, cipher, "", "", "pass")
	host.Port = 1
	sp := testServicePort(2)

	holdConnection(t, db)

	started := make(chan error, 1)
	go func() {
		started <- m.StartTunnel(host, &sp, models.BindScopeWildcard, nil)
	}()

	<-entered

	begun := time.Now()
	registered := m.tunnelRegistered(tunnelKey(1, 2))
	counts := m.RequestReconnect(1)
	waited := time.Since(begun)

	if err := <-started; err != nil {
		t.Fatalf("StartTunnel returned an error: %v", err)
	}

	t.Logf("the status calls waited %v while the connection was held for %v", waited, dbHeldFor)
	if waited >= dbHeldFor/2 {
		t.Fatalf("the status calls waited %v for a tunnel row held up for %v", waited, dbHeldFor)
	}
	if !registered {
		t.Fatal("a tunnel being started was not reported as registered, a second start of it would go ahead")
	}
	if counts.Tunnels != 1 {
		t.Fatalf("RequestReconnect counted %d running tunnels of the Host, want 1: the one being started", counts.Tunnels)
	}
}

// TestTakingTheManagerLockDoesNotWaitForAStatusRead covers a status read that
// waits for the one connection. It does not hold m.mu while it waits, so a
// stop that takes the lock does not queue behind it, and the calls that take
// the lock to read after that stop do not queue behind the stop.
func TestTakingTheManagerLockDoesNotWaitForAStatusRead(t *testing.T) {
	tests := []struct {
		name string
		read func(m *Manager) error
	}{
		{
			name: "GetHostTunnels",
			read: func(m *Manager) error {
				_, err := m.GetHostTunnels(1)
				return err
			},
		},
		{
			name: "GetAllTunnels",
			read: func(m *Manager) error {
				_, err := m.GetAllTunnels()
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newSingleConnDB(t)
			entered := queryEntered[*[]models.Tunnel](t, db, "test:tunnel_rows_entered")

			m, err := NewManager(db, zap.NewNop(), newTestCipher(t), 1)
			if err != nil {
				t.Fatalf("failed to create manager: %v", err)
			}

			registerStoppedTunnel(t, m, 1, 3)

			holdConnection(t, db)

			read := make(chan error, 1)
			go func() {
				read <- tt.read(m)
			}()

			<-entered

			stopped := make(chan error, 1)
			go func() {
				stopped <- m.StopTunnel(1, 3)
			}()

			// Give the stop the time to ask for the lock, so that a read lock
			// asked for after it queues behind it if it is still waiting.
			time.Sleep(100 * time.Millisecond)

			begun := time.Now()
			_ = m.RequestReconnect(1)
			waited := time.Since(begun)

			if err := <-read; err != nil {
				t.Fatalf("%s returned an error: %v", tt.name, err)
			}
			if err := <-stopped; err != nil {
				t.Fatalf("StopTunnel returned an error: %v", err)
			}

			t.Logf("RequestReconnect waited %v while %s waited for a connection held for %v", waited, tt.name, dbHeldFor)
			if waited >= dbHeldFor/2 {
				t.Fatalf("RequestReconnect waited %v behind %s waiting for a connection held for %v", waited, tt.name, dbHeldFor)
			}
		})
	}
}

// heldCreateDB is a stub over the row of one tunnel whose create is held until
// the test lets it go, which is a database that is slow to answer. It records
// whether the row was deleted while its create was still under way.
type heldCreateDB struct {
	mu       sync.Mutex
	row      *models.Tunnel
	created  int
	creating bool
	// deletedWhileCreating is set when the row was deleted while its create
	// had not finished, which is a stop of the tunnel reaching the row before
	// the start of it was done with it.
	deletedWhileCreating bool

	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

// newHeldCreateDB returns the stub. With fail set, a create answers with an
// error once it is let go and keeps no row.
func newHeldCreateDB(t *testing.T, fail bool) (*gorm.DB, *heldCreateDB) {
	t.Helper()

	held := &heldCreateDB{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}

	db := newFailingDB(t)

	err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		held.mu.Lock()
		defer held.mu.Unlock()

		if dest, ok := tx.Statement.Dest.(*models.Tunnel); ok && held.row != nil {
			*dest = *held.row
			tx.RowsAffected = 1
		}
	})
	if err != nil {
		t.Fatalf("failed to replace the query callback: %v", err)
	}

	err = db.Callback().Create().Replace("gorm:create", func(tx *gorm.DB) {
		row, ok := tx.Statement.Dest.(*models.Tunnel)
		if !ok {
			return
		}

		held.mu.Lock()
		held.creating = true
		held.mu.Unlock()

		held.once.Do(func() { close(held.entered) })
		<-held.release

		held.mu.Lock()
		defer held.mu.Unlock()

		held.creating = false
		if fail {
			_ = tx.AddError(errConnPoolClosed)
			return
		}

		held.created++
		kept := *row
		held.row = &kept
		tx.RowsAffected = 1
	})
	if err != nil {
		t.Fatalf("failed to replace the create callback: %v", err)
	}

	// A status save of a tunnel that was started is taken as written, so it
	// does not fall through to a create and put a row back.
	err = db.Callback().Update().Replace("gorm:update", func(tx *gorm.DB) {
		tx.RowsAffected = 1
	})
	if err != nil {
		t.Fatalf("failed to replace the update callback: %v", err)
	}

	err = db.Callback().Delete().Replace("gorm:delete", func(tx *gorm.DB) {
		held.mu.Lock()
		defer held.mu.Unlock()

		if held.creating {
			held.deletedWhileCreating = true
		}
		held.row = nil
	})
	if err != nil {
		t.Fatalf("failed to replace the delete callback: %v", err)
	}

	return db, held
}

// state reports whether the row is there, how many times one was created and
// whether it was deleted while its create was under way.
func (h *heldCreateDB) state() (bool, int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.row != nil, h.created, h.deletedWhileCreating
}

// TestStartTunnelRefusesASecondStartOfATunnelBeingStarted covers a start of a
// tunnel whose first start is still making its row. The key is taken, so the
// second one is refused at once rather than after the row is made.
func TestStartTunnelRefusesASecondStartOfATunnelBeingStarted(t *testing.T) {
	db, held := newHeldCreateDB(t, false)

	m, err := NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	t.Cleanup(m.StopAllTunnels)

	host := enabledHost(1, true)
	sp := testServicePort(2)

	releaseTimer := time.AfterFunc(dbHeldFor, func() { close(held.release) })
	defer releaseTimer.Stop()

	started := make(chan error, 1)
	go func() {
		started <- m.StartTunnel(&host, &sp, models.BindScopeWildcard, nil)
	}()

	<-held.entered

	begun := time.Now()
	second := m.StartTunnel(&host, &sp, models.BindScopeWildcard, nil)
	waited := time.Since(begun)

	if err := <-started; err != nil {
		t.Fatalf("the first StartTunnel returned an error: %v", err)
	}

	t.Logf("the second StartTunnel waited %v while the row was held for %v", waited, dbHeldFor)
	if second == nil || second.Error() != "tunnel already exists" {
		t.Fatalf("the second StartTunnel returned %v, want tunnel already exists", second)
	}
	if waited >= dbHeldFor/2 {
		t.Fatalf("the second StartTunnel waited %v for a row held for %v", waited, dbHeldFor)
	}

	present, created, _ := held.state()
	if !present || created != 1 {
		t.Fatalf("the row is there: %v, created %d times, want there and once", present, created)
	}
	if _, ok := runningKeys(m)[tunnelKey(1, 2)]; !ok {
		t.Fatal("the tunnel of the first start is not registered")
	}
}

// TestStopTunnelWaitsForTheStartOfTheSameTunnel covers a stop of a tunnel
// whose start is still making its row. It waits for the start and then stops
// the tunnel that start registered, which is what it did when the start held
// the lock over the row, and it deletes the row only after it was made.
func TestStopTunnelWaitsForTheStartOfTheSameTunnel(t *testing.T) {
	db, held := newHeldCreateDB(t, false)

	m, err := NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	t.Cleanup(m.StopAllTunnels)

	host := enabledHost(1, true)
	sp := testServicePort(2)

	started := make(chan error, 1)
	go func() {
		started <- m.StartTunnel(&host, &sp, models.BindScopeWildcard, nil)
	}()

	<-held.entered

	stopped := make(chan error, 1)
	go func() {
		stopped <- m.StopTunnel(1, 2)
	}()

	// Give a stop that does not wait the time to finish, then let the row be
	// made.
	time.Sleep(200 * time.Millisecond)
	select {
	case err := <-stopped:
		t.Fatalf("StopTunnel returned %v while the start of the tunnel was still making its row", err)
	default:
	}
	close(held.release)

	if err := <-started; err != nil {
		t.Fatalf("StartTunnel returned an error: %v", err)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("StopTunnel returned an error: %v, want the started tunnel stopped", err)
	}

	present, created, early := held.state()
	if early {
		t.Fatal("the stop deleted the tunnel row while the start was still making it")
	}
	if present || created != 1 {
		t.Fatalf("the row is there: %v, created %d times, want made once and then deleted", present, created)
	}
	if len(runningKeys(m)) != 0 {
		t.Fatalf("tunnels left registered after the stop: %v", runningKeys(m))
	}
}

// TestStopTunnelOfAStartThatFailsFindsNothing covers a stop of a tunnel whose
// start then fails to make its row. The start leaves the key free, and the
// stop that waited for it finds no tunnel, as it did when the start held the
// lock over the row.
func TestStopTunnelOfAStartThatFailsFindsNothing(t *testing.T) {
	db, held := newHeldCreateDB(t, true)

	m, err := NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	t.Cleanup(m.StopAllTunnels)

	host := enabledHost(1, true)
	sp := testServicePort(2)

	started := make(chan error, 1)
	go func() {
		started <- m.StartTunnel(&host, &sp, models.BindScopeWildcard, nil)
	}()

	<-held.entered

	stopped := make(chan error, 1)
	go func() {
		stopped <- m.StopTunnel(1, 2)
	}()

	time.Sleep(100 * time.Millisecond)
	close(held.release)

	if err := <-started; err == nil {
		t.Fatal("StartTunnel returned no error for a row that could not be made")
	}
	if err := <-stopped; !errors.Is(err, ErrTunnelNotExist) {
		t.Fatalf("StopTunnel returned %v, want ErrTunnelNotExist for a start that failed", err)
	}
	if m.tunnelRegistered(tunnelKey(1, 2)) {
		t.Fatal("a start that failed left its key taken")
	}
}
