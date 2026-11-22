package tunnel

import (
	"sync"
	"testing"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// heldDeleteDB is a stub over the row of one tunnel whose delete is held until
// the test lets it go, which is a database that is slow to answer. It records
// whether the row was asked for while a delete of it was still under way.
type heldDeleteDB struct {
	mu       sync.Mutex
	row      *models.Tunnel
	created  int
	deleting bool
	// queriedWhileDeleting is set when the row was looked up while its delete
	// had not finished, which is a start of the same tunnel reaching the row
	// before the stop of the one before it was done with it.
	queriedWhileDeleting bool

	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newHeldDeleteDB(t *testing.T, row *models.Tunnel) (*gorm.DB, *heldDeleteDB) {
	t.Helper()

	held := &heldDeleteDB{
		row:     row,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}

	db := newFailingDB(t)

	err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		held.mu.Lock()
		defer held.mu.Unlock()

		switch dest := tx.Statement.Dest.(type) {
		case *models.Tunnel:
			if held.deleting {
				held.queriedWhileDeleting = true
			}
			if held.row != nil {
				*dest = *held.row
				tx.RowsAffected = 1
			}
		case *[]models.Tunnel:
			if held.row != nil {
				*dest = []models.Tunnel{*held.row}
				tx.RowsAffected = 1
			}
		}
	})
	if err != nil {
		t.Fatalf("failed to replace the query callback: %v", err)
	}

	err = db.Callback().Create().Replace("gorm:create", func(tx *gorm.DB) {
		held.mu.Lock()
		defer held.mu.Unlock()

		if row, ok := tx.Statement.Dest.(*models.Tunnel); ok {
			held.created++
			kept := *row
			held.row = &kept
			tx.RowsAffected = 1
		}
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
		held.deleting = true
		held.mu.Unlock()

		held.once.Do(func() { close(held.entered) })
		<-held.release

		held.mu.Lock()
		held.row = nil
		held.deleting = false
		held.mu.Unlock()
	})
	if err != nil {
		t.Fatalf("failed to replace the delete callback: %v", err)
	}

	return db, held
}

// state reports whether the row is there and how many times one was created.
func (h *heldDeleteDB) state() (bool, int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.row != nil, h.created
}

// TestRequestReconnectDoesNotWaitForATunnelRowBeingDeleted covers a stop whose
// row delete is slow. What runs is answered from memory, so asking to
// reconnect a Host does not wait for the database the stop is waiting on.
func TestRequestReconnectDoesNotWaitForATunnelRowBeingDeleted(t *testing.T) {
	db, held := newHeldDeleteDB(t, &models.Tunnel{HostID: 1, SPID: 2, Status: "connected"})

	m, err := NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	registerStoppedTunnel(t, m, 1, 2)
	registerStoppedTunnel(t, m, 1, 3)

	// Let the delete go after a while whatever happens, so that a call that
	// waits for it is measured rather than left hanging.
	const heldFor = time.Second
	releaseTimer := time.AfterFunc(heldFor, func() { close(held.release) })
	defer releaseTimer.Stop()

	stopped := make(chan error, 1)
	go func() {
		stopped <- m.StopTunnel(1, 2)
	}()

	<-held.entered

	begun := time.Now()
	counts := m.RequestReconnect(1)
	waited := time.Since(begun)

	if err := <-stopped; err != nil {
		t.Fatalf("StopTunnel returned an error: %v", err)
	}

	t.Logf("RequestReconnect waited %v while the row delete was held for %v", waited, heldFor)
	if waited >= heldFor/2 {
		t.Fatalf("RequestReconnect waited %v for a row delete held for %v", waited, heldFor)
	}
	if counts.Tunnels != 1 {
		t.Fatalf("RequestReconnect counted %d running tunnels of the Host, want 1: the one being stopped is not running",
			counts.Tunnels)
	}
}

// TestStartTunnelWaitsForTheStopOfTheSameTunnel covers a start of a tunnel
// while the stop of the one before it under the same key is still deleting its
// row. The two share the row, so a start that read or wrote it before the
// delete finished would have the delete take the row of the new tunnel away.
func TestStartTunnelWaitsForTheStopOfTheSameTunnel(t *testing.T) {
	host := enabledHost(1, true)
	sp := testServicePort(2)

	db, held := newHeldDeleteDB(t, &models.Tunnel{HostID: 1, SPID: 2, Status: "connected"})

	m, err := NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	t.Cleanup(m.StopAllTunnels)

	old := registerStoppedTunnel(t, m, 1, 2)

	stopped := make(chan error, 1)
	go func() {
		stopped <- m.StopTunnel(1, 2)
	}()

	<-held.entered

	started := make(chan error, 1)
	go func() {
		started <- m.StartTunnel(&host, &sp, models.BindScopeWildcard, nil)
	}()

	// Give a start that does not wait the time to reach the row, then let the
	// delete finish.
	time.Sleep(200 * time.Millisecond)
	close(held.release)

	if err := <-stopped; err != nil {
		t.Fatalf("StopTunnel returned an error: %v", err)
	}
	if err := <-started; err != nil {
		t.Fatalf("StartTunnel returned an error: %v", err)
	}

	held.mu.Lock()
	early := held.queriedWhileDeleting
	held.mu.Unlock()
	if early {
		t.Fatal("the start read the tunnel row while the stop of the same tunnel was still deleting it")
	}

	present, created := held.state()
	if !present {
		t.Fatal("the row of the started tunnel is gone: the delete of the stop took it")
	}
	if created != 1 {
		t.Fatalf("the started tunnel created its row %d times, want 1: it took over the row of the stopped one",
			created)
	}

	running := runningKeys(m)[tunnelKey(1, 2)]
	if running == nil || running == old {
		t.Fatal("the started tunnel is not the one registered")
	}
}
