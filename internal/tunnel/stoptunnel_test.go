package tunnel

import (
	"io"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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

// TestARestartWaitsForTheOldConnectionToLetThePortGo stops a tunnel while its
// connection is still asking the Host for the forwarded port and starts it
// again at once, which is what a reconnect and a change to the settings of a
// tunnel do. The Host is held so that the old connection has the IPv4 address
// of the port open and is still waiting for the IPv6 one when a new
// connection asks for the IPv4 address. A stop that does not wait for the old
// connection to end lets the new one ask then, and the Host refuses it the
// address the old one holds, which leaves the tunnel on one family or none.
// The old connection is let go on its own after two seconds, so a stop that
// waits finds it ended and the new connection opens both addresses.
func TestARestartWaitsForTheOldConnectionToLetThePortGo(t *testing.T) {
	f := newJumpFixture(t, nil)
	f.route(t)
	f.addTunnel(t, "127.0.0.1", 1, freeDualStackPort(t))

	host := f.host(t, 1)

	var sp models.ServicePort
	err := f.db.First(&sp, 1).Error
	if err != nil {
		t.Fatalf("failed to read the service port: %v", err)
	}

	oldAsked := make(chan struct{})
	releaseOld := make(chan struct{})
	oldOpenedV4 := make(chan struct{})
	newAnsweredV4 := make(chan struct{})

	var mu sync.Mutex
	newOpened := make(map[string]bool)

	f.target.onForward(func(seq int64, addr string) func(opened bool) {
		switch {
		case seq == 1 && addr == "127.0.0.1":
			close(oldAsked)
			<-releaseOld
			return func(bool) { close(oldOpenedV4) }
		case seq == 1:
			select {
			case <-newAnsweredV4:
			case <-time.After(2 * time.Second):
			}
		case seq == 2:
			if addr == "127.0.0.1" {
				select {
				case <-oldOpenedV4:
				case <-time.After(2 * time.Second):
				}
			}

			return func(opened bool) {
				mu.Lock()
				newOpened[addr] = opened
				mu.Unlock()

				if addr == "127.0.0.1" {
					close(newAnsweredV4)
				}
			}
		}

		return nil
	})

	err = f.m.StartTunnel(&host, &sp, models.BindScopeLoopback, nil)
	if err != nil {
		t.Fatalf("StartTunnel returned an error: %v", err)
	}

	select {
	case <-oldAsked:
	case <-time.After(localTestTimeout):
		close(releaseOld)
		t.Fatal("the tunnel never asked for its forwarded port")
	}

	restarted := make(chan error, 1)
	go func() {
		err := f.m.StopTunnel(1, 1)
		if err == nil {
			err = f.m.StartTunnel(&host, &sp, models.BindScopeLoopback, nil)
		}
		restarted <- err
	}()

	close(releaseOld)

	select {
	case err := <-restarted:
		if err != nil {
			t.Fatalf("restarting the tunnel returned an error: %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("restarting the tunnel did not return")
	}

	waitFor(t, localTestTimeout, "the Host to answer the new connection for both addresses", func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(newOpened) == 2
	})

	mu.Lock()
	opened := map[string]bool{"127.0.0.1": newOpened["127.0.0.1"], "::1": newOpened["::1"]}
	mu.Unlock()
	if !opened["127.0.0.1"] || !opened["::1"] {
		t.Fatalf("the first connection of the restarted tunnel opened %v, want both addresses: "+
			"it asked while the stopped connection still held the port", opened)
	}

	row := f.waitTunnelRow(t, "the restarted tunnel to connect", func(row models.Tunnel) bool {
		return row.Status == "connected"
	})
	if row.OpenReach != openReachBoth {
		t.Fatalf("the restarted tunnel is connected with open_reach %q, want %q", row.OpenReach, openReachBoth)
	}
}

// TestStopsWaitForTheirStartsAgainstOneLimit is the wait of a stop of several
// tunnels, which is what the shutdown does. It is one limit for all of them
// rather than one each, or a shutdown would take the limit once for every
// tunnel still connecting. A Start that never began or has ended is not waited
// for, one that ends within the limit is not reported, and every one still
// running when the limit runs out is logged.
func TestStopsWaitForTheirStartsAgainstOneLimit(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	m := &Manager{logger: zap.New(core)}

	endedEarly := make(chan struct{})
	close(endedEarly)

	endsInTime := make(chan struct{})
	time.AfterFunc(100*time.Millisecond, func() { close(endsInTime) })

	stopped := []*stoppingTunnel{
		{hostID: 1, spID: 1, tunnel: &SSHTunnel{}},
		{hostID: 2, spID: 1, tunnel: &SSHTunnel{ended: endedEarly}},
		{hostID: 3, spID: 1, tunnel: &SSHTunnel{ended: make(chan struct{})}},
		{hostID: 4, spID: 1, tunnel: &SSHTunnel{ended: endsInTime}},
		{hostID: 5, spID: 1, tunnel: &SSHTunnel{ended: make(chan struct{})}},
	}

	const limit = 500 * time.Millisecond

	began := time.Now()
	m.waitForStarts(stopped, &stopWait{limit: limit})
	took := time.Since(began)

	if took < limit || took >= 2*limit {
		t.Fatalf("the wait took %v, want the one limit of %v for all of them", took, limit)
	}

	var reported []int
	for _, entry := range logs.FilterField(logid.TunnelStopWaitTimedOut.Field()).All() {
		hostID, _ := entry.ContextMap()["host_id"].(uint64)
		reported = append(reported, int(hostID))
	}
	sort.Ints(reported)

	if len(reported) != 2 || reported[0] != 3 || reported[1] != 5 {
		t.Fatalf("the Starts reported as not ended are of Hosts %v, want [3 5]", reported)
	}
}

// startHoldingProxy passes the TCP connections it accepts on to backend, but
// not before release is called. accepted is sent on when the first one
// arrives. A connection made through it is held before the SSH server has said
// a word, which is a dial that is still under way.
func startHoldingProxy(t *testing.T, backend string) (addr string, accepted <-chan struct{}, release func()) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	arrived := make(chan struct{}, 1)
	held := make(chan struct{})

	var once sync.Once
	var conns sync.WaitGroup

	t.Cleanup(func() {
		once.Do(func() { close(held) })
		_ = ln.Close()
		conns.Wait()
	})

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			select {
			case arrived <- struct{}{}:
			default:
			}

			conns.Add(1)
			go func() {
				defer conns.Done()
				defer func() {
					_ = conn.Close()
				}()

				<-held

				upstream, err := net.Dial("tcp", backend)
				if err != nil {
					return
				}
				defer func() {
					_ = upstream.Close()
				}()

				done := make(chan struct{}, 2)
				go func() {
					_, _ = io.Copy(upstream, conn)
					done <- struct{}{}
				}()
				go func() {
					_, _ = io.Copy(conn, upstream)
					done <- struct{}{}
				}()

				<-done
			}()
		}
	}()

	return ln.Addr().String(), arrived, func() { once.Do(func() { close(held) }) }
}

// TestAStopWhileDialingAsksForNoPort stops a tunnel while its connection to
// the Host is still being made. The connection is let through after the stop,
// and the tunnel must see the stop before it asks for the forwarded port, or
// the Host opens the port for a connection that is about to close and holds it
// against the tunnel started next.
func TestAStopWhileDialingAsksForNoPort(t *testing.T) {
	m := newSSHTestManager(t, 1)

	server := startJumpTestServer(t)

	var asked atomic.Int64
	server.onForward(func(int64, string) func(bool) {
		asked.Add(1)
		return nil
	})

	addr, accepted, release := startHoldingProxy(t, server.addr)

	tun, tunnel := newSSHTestTunnel(t, addr)
	tun.Config = testClientConfig(5 * time.Second)

	returned := make(chan struct{})
	go func() {
		tun.Start(m, tunnel)
		close(returned)
	}()

	select {
	case <-accepted:
	case <-time.After(localTestTimeout):
		t.Fatal("the tunnel never dialed the Host")
	}

	_ = tun.Stop(m)
	release()

	select {
	case <-returned:
	case <-time.After(localTestTimeout):
		t.Fatal("Start did not return after Stop")
	}

	waitFor(t, localTestTimeout, "the held connection to get through to the Host", func() bool {
		return server.handshakes.Load() == 1
	})
	if n := asked.Load(); n != 0 {
		t.Fatalf("the stopped tunnel asked the Host for %d forwarded ports, want none", n)
	}

	waitFor(t, localTestTimeout, "the SSH connection to be closed", func() bool {
		return server.active.Load() == 0
	})
}
