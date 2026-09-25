package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/crypto/ssh"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var errSSHTestDBClosed = errors.New("ssh test database is closed")

// sshTestConnPool fails every statement so no real database is needed.
type sshTestConnPool struct{}

func (sshTestConnPool) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return nil, errSSHTestDBClosed
}

func (sshTestConnPool) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return nil, errSSHTestDBClosed
}

func (sshTestConnPool) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return nil, errSSHTestDBClosed
}

func (sshTestConnPool) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return sqliteVersionDB.QueryRowContext(ctx, query, args...)
}

func newSSHTestManager(t *testing.T, monitoringIntervalSec int) *Manager {
	t.Helper()

	db, err := gorm.Open(sqlite.Dialector{Conn: sshTestConnPool{}}, &gorm.Config{
		Logger:               logger.Discard,
		DisableAutomaticPing: true,
	})
	if err != nil {
		t.Fatalf("failed to open gorm with test conn pool: %v", err)
	}

	m, err := NewManager(db, zap.NewNop(), newTestCipher(t), monitoringIntervalSec)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	return m
}

func newSSHTestTunnel(t *testing.T, serverAddr string) (*SSHTunnel, *models.Tunnel) {
	t.Helper()

	sshConfig := &ssh.ClientConfig{
		User:            "tester",
		Auth:            []ssh.AuthMethod{ssh.Password("wrong-password")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Second,
	}

	hostID := uint(1)
	spID := uint(1)

	tun, err := NewSSHTunnel(&hostID, &spID, "127.0.0.1:0", "[::1]:0", serverAddr, "127.0.0.1:1",
		sshConfig, zap.NewNop())
	if err != nil {
		t.Fatalf("failed to create tunnel: %v", err)
	}

	return tun, &models.Tunnel{HostID: hostID, SPID: spID, Status: "starting"}
}

// countMonitorGoroutines reports how many goroutines currently sit in monitorConnection.
func countMonitorGoroutines() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return bytes.Count(buf[:n], []byte("tunnel.(*SSHTunnel).monitorConnection"))
		}
		buf = make([]byte, 2*len(buf))
	}
}

func waitMonitorGoroutines(t *testing.T, want int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	got := countMonitorGoroutines()
	for time.Now().Before(deadline) {
		if got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
		got = countMonitorGoroutines()
	}

	if got != want {
		t.Fatalf("monitor goroutines = %d, want %d", got, want)
	}
}

// startClosingListener accepts connections and closes them right away, so ssh.Dial
// fails fast with a non authentication error.
func startClosingListener(t *testing.T) string {
	t.Helper()

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
			_ = conn.Close()
		}
	}()

	return ln.Addr().String()
}

// startSilentListener accepts connections and never sends anything, so an SSH
// handshake against it never completes.
func startSilentListener(t *testing.T) string {
	t.Helper()

	addr, _ := startCountingSilentListener(t)
	return addr
}

// startCountingSilentListener is startSilentListener that also counts the
// connections it accepted.
func startCountingSilentListener(t *testing.T) (string, *atomic.Int64) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	conns := make(chan net.Conn, 16)
	t.Cleanup(func() {
		_ = ln.Close()
		close(conns)
		for conn := range conns {
			_ = conn.Close()
		}
	})

	var accepted atomic.Int64
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			select {
			case conns <- conn:
			default:
				_ = conn.Close()
			}
		}
	}()

	return ln.Addr().String(), &accepted
}

// startRejectingSSHServer speaks SSH but denies every authentication attempt.
func startRejectingSSHServer(t *testing.T) string {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate host key: %v", err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("failed to create signer: %v", err)
	}

	config := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			return nil, errors.New("password rejected")
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
				sshConn, _, _, err := ssh.NewServerConn(conn, config)
				if err != nil {
					_ = conn.Close()
					return
				}
				_ = sshConn.Close()
			}(conn)
		}
	}()

	return ln.Addr().String()
}

func TestMonitorConnectionReturnsOnStop(t *testing.T) {
	m := newSSHTestManager(t, 1)
	tun, tunnel := newSSHTestTunnel(t, "127.0.0.1:1")

	stop := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		tun.monitorConnection(m, tunnel, stop)
		close(exited)
	}()

	close(stop)

	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("monitorConnection did not return after the stop channel was closed")
	}
}

func TestMonitorConnectionReturnsOnDone(t *testing.T) {
	m := newSSHTestManager(t, 1)
	tun, tunnel := newSSHTestTunnel(t, "127.0.0.1:1")

	exited := make(chan struct{})
	go func() {
		tun.monitorConnection(m, tunnel, make(chan struct{}))
		close(exited)
	}()

	close(tun.done)

	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("monitorConnection did not return after done was closed")
	}
}

func TestStartRunsSingleMonitorAndStopEndsIt(t *testing.T) {
	waitMonitorGoroutines(t, 0, 3*time.Second)

	m := newSSHTestManager(t, 1)
	tun, tunnel := newSSHTestTunnel(t, startClosingListener(t))

	returned := make(chan struct{})
	go func() {
		tun.Start(m, tunnel)
		close(returned)
	}()

	waitMonitorGoroutines(t, 1, 3*time.Second)

	// Several connection retries must not add monitor goroutines.
	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := countMonitorGoroutines(); got != 1 {
			t.Fatalf("monitor goroutines = %d while Start is retrying, want 1", got)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := tun.Stop(m); err == nil {
		t.Log("Stop returned no error")
	}

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after Stop")
	}

	waitMonitorGoroutines(t, 0, 3*time.Second)

	if tunnel.RetryCount == 0 {
		t.Fatal("RetryCount was not increased while retrying")
	}
}

func TestStartEndsMonitorOnAuthFailure(t *testing.T) {
	waitMonitorGoroutines(t, 0, 3*time.Second)

	m := newSSHTestManager(t, 1)
	tun, tunnel := newSSHTestTunnel(t, startRejectingSSHServer(t))

	returned := make(chan struct{})
	go func() {
		tun.Start(m, tunnel)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return on authentication failure")
	}

	waitMonitorGoroutines(t, 0, 3*time.Second)

	if tunnel.Status != "error" {
		t.Fatalf("tunnel status = %q, want %q", tunnel.Status, "error")
	}
	if tunnel.RetryCount != 0 {
		t.Fatalf("RetryCount = %d, want 0, credentials the server refuses are being retried",
			tunnel.RetryCount)
	}
}

func TestMonitorCheckTimeoutIsTheInterval(t *testing.T) {
	for _, monitoringIntervalSec := range []int{3, 5, 10, 30, 60, 300} {
		interval := time.Duration(monitoringIntervalSec) * time.Second
		if got := monitorCheckTimeout(interval); got != interval {
			t.Errorf("monitorCheckTimeout(%v) = %v, want the interval itself, "+
				"a silent server is tolerated for one interval", interval, got)
		}
	}
}

func TestMonitorCheckTimeoutHasAFloor(t *testing.T) {
	// 0 and below are rejected by the configuration and sub-second intervals
	// only occur in tests, but a zero timeout means no timeout at all to
	// net.DialTimeout, which is the one outcome that must not happen.
	for _, interval := range []time.Duration{-time.Second, 0, 500 * time.Millisecond, time.Second, 2 * time.Second} {
		if got := monitorCheckTimeout(interval); got != 3*time.Second {
			t.Errorf("monitorCheckTimeout(%v) = %v, want the 3s floor", interval, got)
		}
	}
}

// TestAuthFailureIsRecognized pins the message golang.org/x/crypto/ssh reports
// when every authentication method was refused. The library offers nothing else
// to match on, so this test is what notices a reworded message before a tunnel
// with wrong credentials starts retrying forever.
func TestAuthFailureIsRecognized(t *testing.T) {
	serverAddr := startRejectingSSHServer(t)

	_, err := ssh.Dial("tcp", serverAddr, &ssh.ClientConfig{
		User:            "tester",
		Auth:            []ssh.AuthMethod{ssh.Password("wrong-password")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err == nil {
		t.Fatal("ssh.Dial succeeded against a server that refuses every password")
	}
	if !isAuthFailure(err) {
		t.Fatalf("isAuthFailure did not recognize a refused authentication: %v", err)
	}
	if !isAuthFailure(fmt.Errorf("failed to establish SSH connection: %w", err)) {
		t.Fatalf("isAuthFailure did not recognize a refused authentication once wrapped: %v", err)
	}
}

func TestIsAuthFailureIgnoresOtherErrors(t *testing.T) {
	if isAuthFailure(nil) {
		t.Fatal("isAuthFailure(nil) is true")
	}

	_, err := ssh.Dial("tcp", startClosingListener(t), &ssh.ClientConfig{
		User:            "tester",
		Auth:            []ssh.AuthMethod{ssh.Password("wrong-password")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err == nil {
		t.Fatal("ssh.Dial succeeded against a listener that closes every connection")
	}
	if isAuthFailure(err) {
		t.Fatalf("a connection that never reached authentication was reported as an authentication failure: %v", err)
	}
}

func TestReconnectDoesNotEstablishConnection(t *testing.T) {
	m := newSSHTestManager(t, 1)
	addr, accepted := startCountingSilentListener(t)
	tun, tunnel := newSSHTestTunnel(t, addr)

	client, closeClient := newLoopbackSSHClient(t)
	defer closeClient()

	tun.clientMu.Lock()
	tun.client = client
	tun.clientMu.Unlock()

	returned := make(chan struct{})
	go func() {
		tun.reconnect(m, tunnel, client)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("reconnect did not return")
	}

	time.Sleep(tun.Config.Timeout + 500*time.Millisecond)
	if n := accepted.Load(); n != 0 {
		t.Fatalf("the SSH server accepted %d connections during reconnect, want none", n)
	}

	tun.clientMu.RLock()
	got := tun.client
	tun.clientMu.RUnlock()

	if got != nil {
		t.Fatal("reconnect did not clear the client")
	}
	if tunnel.Status != "reconnecting" {
		t.Fatalf("tunnel status = %q, want %q", tunnel.Status, "reconnecting")
	}
	if tunnel.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1", tunnel.RetryCount)
	}
}

func TestReconnectLeavesANewerClientAlone(t *testing.T) {
	m := newSSHTestManager(t, 1)
	tun, tunnel := newSSHTestTunnel(t, "127.0.0.1:1")

	observed, closeObserved := newLoopbackSSHClient(t)
	defer closeObserved()
	current, closeCurrent := newLoopbackSSHClient(t)
	defer closeCurrent()

	// The monitor read observed from the tunnel and then blocked in its check,
	// which is long enough for the Start loop to establish current.
	_ = observed.Close()
	tun.clientMu.Lock()
	tun.client = current
	tun.clientMu.Unlock()

	tunnel.Status = "connected"

	tun.reconnect(m, tunnel, observed)

	tun.clientMu.RLock()
	got := tun.client
	tun.clientMu.RUnlock()

	if got != current {
		t.Fatal("reconnect closed the connection the Start loop had just established")
	}
	if _, _, err := current.SendRequest("keepalive@tunnel", true, nil); err != nil {
		t.Fatalf("the connection the Start loop established is no longer usable: %v", err)
	}
	if tunnel.Status != "connected" {
		t.Fatalf("tunnel status = %q, want %q, a connected tunnel was reported as reconnecting",
			tunnel.Status, "connected")
	}
	if tunnel.RetryCount != 0 {
		t.Fatalf("RetryCount = %d, want 0, a connection that was already replaced started a retry cycle",
			tunnel.RetryCount)
	}
}

// newLoopbackSSHClient returns a connected ssh.Client backed by an in process server.
func newLoopbackSSHClient(t *testing.T) (*ssh.Client, func()) {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate host key: %v", err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("failed to create signer: %v", err)
	}

	serverConfig := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	serverConfig.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	served := make(chan struct{})
	go func() {
		defer close(served)

		conn, err := ln.Accept()
		if err != nil {
			return
		}
		sshConn, chans, reqs, err := ssh.NewServerConn(conn, serverConfig)
		if err != nil {
			_ = conn.Close()
			return
		}
		go ssh.DiscardRequests(reqs)
		for newChan := range chans {
			_ = newChan.Reject(ssh.Prohibited, "not supported")
		}
		_ = sshConn.Close()
	}()

	client, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{
		User:            "tester",
		Auth:            []ssh.AuthMethod{ssh.Password("any")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	})
	if err != nil {
		_ = ln.Close()
		t.Fatalf("failed to dial loopback ssh server: %v", err)
	}

	return client, func() {
		_ = client.Close()
		_ = ln.Close()
		<-served
	}
}

// startForwardingSSHServer speaks SSH, grants every remote forward request and
// keeps serving. It reports the time of every completed handshake.
func startForwardingSSHServer(t *testing.T) (string, func() []time.Time) {
	t.Helper()

	return startForwardingSSHServerConfirming(t, 12345)
}

// startForwardingSSHServerConfirming is startForwardingSSHServer with the port
// it confirms for a forward chosen by the caller. That port is where the
// forwarded port is probed, so a test about the probe needs one it owns.
func startForwardingSSHServerConfirming(t *testing.T, confirmed uint32) (string, func() []time.Time) {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate host key: %v", err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("failed to create signer: %v", err)
	}

	config := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	config.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	var mu sync.Mutex
	var handshakes []time.Time
	var conns []net.Conn

	t.Cleanup(func() {
		_ = ln.Close()

		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()

			go func(conn net.Conn) {
				defer func() {
					_ = conn.Close()
				}()

				sshConn, chans, reqs, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer func() {
					_ = sshConn.Close()
				}()

				mu.Lock()
				handshakes = append(handshakes, time.Now())
				mu.Unlock()

				go func() {
					for newChan := range chans {
						_ = newChan.Reject(ssh.Prohibited, "not supported")
					}
				}()

				for req := range reqs {
					if !req.WantReply {
						continue
					}
					if req.Type == "tcpip-forward" {
						// The client asked for port 0, so it needs a bound port back.
						_ = req.Reply(true, ssh.Marshal(struct{ Port uint32 }{Port: confirmed}))
						continue
					}
					_ = req.Reply(false, nil)
				}
			}(conn)
		}
	}()

	return ln.Addr().String(), func() []time.Time {
		mu.Lock()
		defer mu.Unlock()

		return append([]time.Time(nil), handshakes...)
	}
}

// waitTunnelClient waits until the tunnel holds a connected client, which only
// happens once the remote listener is up.
func waitTunnelClient(t *testing.T, tun *SSHTunnel, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		tun.clientMu.RLock()
		client := tun.client
		tun.clientMu.RUnlock()

		if client != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("tunnel did not connect in time")
}

// closeTunnelClient drops the SSH connection the way reconnect does, so the
// accept loop sees io.EOF.
func closeTunnelClient(t *testing.T, tun *SSHTunnel) {
	t.Helper()

	tun.clientMu.Lock()
	defer tun.clientMu.Unlock()

	if tun.client == nil {
		t.Fatal("tunnel has no client to close")
	}
	_ = tun.client.Close()
	tun.client = nil
}

func TestEstablishConnectionReportsClosedConnection(t *testing.T) {
	m := newSSHTestManager(t, 1)
	serverAddr, _ := startForwardingSSHServer(t)
	tun, tunnel := newSSHTestTunnel(t, serverAddr)

	errc := make(chan error, 1)
	go func() {
		errc <- tun.establishConnection(m, tunnel)
	}()

	waitTunnelClient(t, tun, 10*time.Second)
	closeTunnelClientOnly(t, tun)

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("establishConnection returned nil on a closed connection, so Start retries with no interval")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("establishConnection did not return after the connection was closed")
	}

	tun.tunnelMu.Lock()
	status, retries, lastError := tunnel.Status, tunnel.RetryCount, tunnel.LastError
	tun.tunnelMu.Unlock()

	if status != "reconnecting" {
		t.Fatalf("tunnel status = %q, want %q, the tunnel is down until Start reconnects",
			status, "reconnecting")
	}
	if retries != 1 {
		t.Fatalf("RetryCount = %d, want 1, the closed connection starts a reconnect cycle", retries)
	}
	if lastError != "" {
		t.Fatalf("LastError = %q, want empty, a closed connection is not an error", lastError)
	}
}

// TestADroppedConnectionIsCountedOnce holds one dropped connection to one retry.
// The monitor that tears the connection down reports it, and the accept loop
// that ends with it is looking at a connection that is no longer the current
// one.
func TestADroppedConnectionIsCountedOnce(t *testing.T) {
	m := newSSHTestManager(t, 1)
	serverAddr, _ := startForwardingSSHServer(t)
	tun, tunnel := newSSHTestTunnel(t, serverAddr)

	errc := make(chan error, 1)
	go func() {
		errc <- tun.establishConnection(m, tunnel)
	}()

	waitTunnelClient(t, tun, 10*time.Second)

	tun.clientMu.RLock()
	client := tun.client
	tun.clientMu.RUnlock()

	tun.reconnect(m, tunnel, client)

	select {
	case err := <-errc:
		if !errors.Is(err, errConnectionClosed) {
			t.Fatalf("establishConnection returned %v, want %v", err, errConnectionClosed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("establishConnection did not return after the connection was closed")
	}

	tun.tunnelMu.Lock()
	status, retries := tunnel.Status, tunnel.RetryCount
	tun.tunnelMu.Unlock()

	if status != "reconnecting" {
		t.Fatalf("tunnel status = %q, want %q", status, "reconnecting")
	}
	if retries != 1 {
		t.Fatalf("RetryCount = %d, want 1, one dropped connection was counted more than once", retries)
	}
}

// closeTunnelClientOnly closes the SSH connection but leaves the field in place,
// the way a peer that goes away does.
func closeTunnelClientOnly(t *testing.T, tun *SSHTunnel) {
	t.Helper()

	tun.clientMu.RLock()
	client := tun.client
	tun.clientMu.RUnlock()

	if client == nil {
		t.Fatal("tunnel has no client to close")
	}
	_ = client.Close()
}

func TestEstablishConnectionClearsClosedClient(t *testing.T) {
	m := newSSHTestManager(t, 1)
	serverAddr, _ := startForwardingSSHServer(t)
	tun, tunnel := newSSHTestTunnel(t, serverAddr)

	errc := make(chan error, 1)
	go func() {
		errc <- tun.establishConnection(m, tunnel)
	}()

	waitTunnelClient(t, tun, 10*time.Second)
	closeTunnelClientOnly(t, tun)

	select {
	case err := <-errc:
		if !errors.Is(err, errConnectionClosed) {
			t.Fatalf("establishConnection returned %v, want %v", err, errConnectionClosed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("establishConnection did not return after the connection was closed")
	}

	tun.clientMu.RLock()
	got := tun.client
	tun.clientMu.RUnlock()

	if got != nil {
		t.Fatal("the closed client was left in place, so the monitor checks it and closes whatever connection " +
			"the Start loop establishes next")
	}
}

func TestEstablishConnectionReportsServerConfirmedPort(t *testing.T) {
	m := newSSHTestManager(t, 1)
	serverAddr, _ := startForwardingSSHServer(t)
	tun, tunnel := newSSHTestTunnel(t, serverAddr)

	errc := make(chan error, 1)
	go func() {
		errc <- tun.establishConnection(m, tunnel)
	}()

	waitTunnelClient(t, tun, 10*time.Second)
	closeTunnelClient(t, tun)

	select {
	case <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("establishConnection did not return after the connection was closed")
	}

	// The test server hands out port 12345 for a request on port 0.
	_, local, _ := readTunnelOpenReach(tun, tunnel)
	if local != "127.0.0.1:12345" {
		t.Fatalf("reported local = %q, want %q, the requested port was not the one the server bound",
			local, "127.0.0.1:12345")
	}
}

func TestStopKeepsTunnelStatusUntouched(t *testing.T) {
	m := newSSHTestManager(t, 5)
	serverAddr, _ := startForwardingSSHServer(t)
	tun, tunnel := newSSHTestTunnel(t, serverAddr)

	returned := make(chan struct{})
	go func() {
		tun.Start(m, tunnel)
		close(returned)
	}()

	waitTunnelClient(t, tun, 10*time.Second)

	_ = tun.Stop(m)

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after Stop")
	}

	// Stop deleted the tunnel row, so writing a status after it would bring the
	// row back.
	tun.tunnelMu.Lock()
	status, retries := tunnel.Status, tunnel.RetryCount
	tun.tunnelMu.Unlock()

	if status != "connected" {
		t.Fatalf("tunnel status = %q, want %q, the status was updated after Stop", status, "connected")
	}
	if retries != 0 {
		t.Fatalf("RetryCount = %d, want 0, it was increased after Stop", retries)
	}
}

// gatedConnPool runs statements on a real database and holds back the first
// statement hold picks until release is closed, closing held once it is
// waiting there.
type gatedConnPool struct {
	*sql.DB
	hold    func(query string) bool
	armed   atomic.Bool
	once    sync.Once
	held    chan struct{}
	release chan struct{}
}

func (p *gatedConnPool) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	if p.armed.Load() && p.hold(query) {
		p.once.Do(func() {
			close(p.held)
			<-p.release
		})
	}

	return p.DB.ExecContext(ctx, query, args...)
}

// TestStopIsNotUndoneByAStatusSaveInFlight holds a status save between its
// isStopped check and its write to the database and runs Stop in that gap.
// gorm turns a save that updates nothing into an insert, so a save that lands
// after Stop deleted the row writes it back, and the status screen goes on
// showing a tunnel that is gone.
func TestStopIsNotUndoneByAStatusSaveInFlight(t *testing.T) {
	sqlDB, err := sql.Open("sqlite", t.TempDir()+"/tunnel-manager.db")
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}
	defer sqlDB.Close()

	pool := &gatedConnPool{
		DB:      sqlDB,
		hold:    func(query string) bool { return strings.HasPrefix(query, "UPDATE") },
		held:    make(chan struct{}),
		release: make(chan struct{}),
	}

	db, err := gorm.Open(sqlite.Dialector{Conn: pool}, &gorm.Config{
		Logger:                 logger.Discard,
		SkipDefaultTransaction: true,
	})
	if err != nil {
		t.Fatalf("failed to open gorm: %v", err)
	}
	if err := db.AutoMigrate(&models.Tunnel{}); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	m, err := NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	tun, tunnel := newSSHTestTunnel(t, startClosingListener(t))
	if err := db.Create(tunnel).Error; err != nil {
		t.Fatalf("failed to create the tunnel row: %v", err)
	}

	pool.armed.Store(true)

	saved := make(chan struct{})
	go func() {
		tun.markReconnecting(m, tunnel)
		close(saved)
	}()

	select {
	case <-pool.held:
	case <-time.After(5 * time.Second):
		t.Fatal("the status save never reached the database")
	}

	stopped := make(chan error, 1)
	go func() {
		stopped <- tun.Stop(m)
	}()

	var stopErr error
	stopReturned := false
	select {
	case stopErr = <-stopped:
		stopReturned = true
	case <-time.After(500 * time.Millisecond):
	}

	close(pool.release)

	select {
	case <-saved:
	case <-time.After(5 * time.Second):
		t.Fatal("the status save did not return")
	}

	if !stopReturned {
		select {
		case stopErr = <-stopped:
		case <-time.After(5 * time.Second):
			t.Fatal("Stop did not return after the status save did")
		}
	}
	if stopErr != nil {
		t.Fatalf("Stop failed: %v", stopErr)
	}

	var rows int64
	if err := db.Model(&models.Tunnel{}).Count(&rows).Error; err != nil {
		t.Fatalf("failed to count tunnel rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("tunnel rows after Stop = %d, want 0, a status save wrote the deleted row back", rows)
	}
}

func TestStartKeepsRetryIntervalAfterEOF(t *testing.T) {
	const cycles = 3

	m := newSSHTestManager(t, 1)
	serverAddr, handshakes := startForwardingSSHServer(t)
	tun, tunnel := newSSHTestTunnel(t, serverAddr)

	returned := make(chan struct{})
	go func() {
		tun.Start(m, tunnel)
		close(returned)
	}()

	for i := 0; i < cycles; i++ {
		waitTunnelClient(t, tun, 10*time.Second)
		closeTunnelClient(t, tun)
	}

	_ = tun.Stop(m)

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after Stop")
	}

	times := handshakes()
	if len(times) < cycles {
		t.Fatalf("completed handshakes = %d, want at least %d", len(times), cycles)
	}

	interval := time.Duration(m.monitoringIntervalSec) * time.Second
	for i := 1; i < cycles; i++ {
		gap := times[i].Sub(times[i-1])
		if gap < interval-100*time.Millisecond {
			t.Fatalf("reconnect %d started %v after the previous connection, want at least %v",
				i, gap, interval)
		}
	}
}

func TestStopIsNotDelayedByRetryInterval(t *testing.T) {
	m := newSSHTestManager(t, 5)
	tun, tunnel := newSSHTestTunnel(t, startClosingListener(t))

	returned := make(chan struct{})
	go func() {
		tun.Start(m, tunnel)
		close(returned)
	}()

	// Let the first connection attempt fail so Start sits in the retry wait.
	time.Sleep(500 * time.Millisecond)

	stoppedAt := time.Now()
	_ = tun.Stop(m)

	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after Stop, it is waiting out the retry interval")
	}

	elapsed := time.Since(stoppedAt)
	if elapsed > 2*time.Second {
		t.Fatalf("Start returned %v after Stop, want well under the %ds retry interval",
			elapsed, m.monitoringIntervalSec)
	}
}

// TestConnectAndReconnectDoNotRaceOnTunnelState runs the two writers of the
// tunnel row at the same time. reconnect is all the monitor does with it and
// establishConnection is what the Start loop does, so this is the pair that
// runs in parallel once Start owns the monitor. It is a -race test, the
// assertions are in the race detector.
func TestConnectAndReconnectDoNotRaceOnTunnelState(t *testing.T) {
	m := newSSHTestManager(t, 1)
	tun, tunnel := newSSHTestTunnel(t, startClosingListener(t))

	client, closeClient := newLoopbackSSHClient(t)
	defer closeClient()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()

		for {
			select {
			case <-stop:
				return
			default:
			}

			// reconnect only tears down the connection it is handed, so the
			// tunnel needs one to hand over on every round.
			tun.clientMu.Lock()
			tun.client = client
			tun.clientMu.Unlock()

			tun.reconnect(m, tunnel, client)
		}
	}()
	go func() {
		defer wg.Done()

		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = tun.establishConnection(m, tunnel)
		}
	}()

	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// startMuteSSHServer completes the SSH handshake and then answers nothing. It
// never services the channel and request streams, so it also stops reading,
// which is how a peer that is gone but whose TCP connection is still open
// behaves.
func startMuteSSHServer(t *testing.T) string {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate host key: %v", err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("failed to create signer: %v", err)
	}

	config := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	config.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	var mu sync.Mutex
	var conns []net.Conn

	t.Cleanup(func() {
		_ = ln.Close()

		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()

			go func(conn net.Conn) {
				_, _, _, err := ssh.NewServerConn(conn, config)
				if err != nil {
					_ = conn.Close()
				}
			}(conn)
		}
	}()

	return ln.Addr().String()
}

// deadlineRecordingConn is a net.Conn that counts the deadlines put on it and
// reports whether it was closed, so a test can tell what was done to the
// connection an SSH client is built on.
type deadlineRecordingConn struct {
	net.Conn
	deadlines atomic.Int32
	closed    atomic.Bool
}

func (c *deadlineRecordingConn) SetDeadline(t time.Time) error {
	c.deadlines.Add(1)
	return c.Conn.SetDeadline(t)
}

func (c *deadlineRecordingConn) SetReadDeadline(t time.Time) error {
	c.deadlines.Add(1)
	return c.Conn.SetReadDeadline(t)
}

func (c *deadlineRecordingConn) SetWriteDeadline(t time.Time) error {
	c.deadlines.Add(1)
	return c.Conn.SetWriteDeadline(t)
}

func (c *deadlineRecordingConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

// dialTestSSHClient connects the way dialSSH does and hands back both the
// client and the connection it was built on, wrapped so that the deadlines put
// on it are counted.
func dialTestSSHClient(t *testing.T, serverAddr string) (*ssh.Client, *deadlineRecordingConn, func()) {
	t.Helper()

	tcpConn, err := net.DialTimeout("tcp", serverAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("failed to dial the test ssh server: %v", err)
	}
	conn := &deadlineRecordingConn{Conn: tcpConn}

	c, chans, reqs, err := ssh.NewClientConn(conn, serverAddr, &ssh.ClientConfig{
		User:            "tester",
		Auth:            []ssh.AuthMethod{ssh.Password("any")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("failed to complete the ssh handshake: %v", err)
	}

	client := ssh.NewClient(c, chans, reqs)

	return client, conn, func() {
		_ = client.Close()
	}
}

// TestSendKeepaliveGivesUpOnAnUnresponsivePeer pins the timeout itself. Without
// one the reply is waited for until TCP stops retransmitting, which on Linux
// takes minutes. Giving up closes the client, and it does so without putting a
// deadline on the connection the forwarded traffic shares.
func TestSendKeepaliveGivesUpOnAnUnresponsivePeer(t *testing.T) {
	client, conn, closeClient := dialTestSSHClient(t, startMuteSSHServer(t))
	defer closeClient()

	const timeout = 300 * time.Millisecond

	result := make(chan error, 1)
	startedAt := time.Now()
	go func() {
		result <- sendKeepalive(client, timeout)
	}()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("sendKeepalive reported a peer that never answered as alive")
		}
		if elapsed := time.Since(startedAt); elapsed > 10*timeout {
			t.Fatalf("sendKeepalive returned after %v, want about the %v timeout", elapsed, timeout)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("sendKeepalive did not return, the keepalive is waiting for TCP to give up")
	}

	if !conn.closed.Load() {
		t.Fatal("sendKeepalive gave up but left the client open, the caller would go on " +
			"forwarding over a connection that stopped answering")
	}

	waited := make(chan struct{})
	go func() {
		_ = client.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("the client is still running after sendKeepalive gave up on it")
	}

	if n := conn.deadlines.Load(); n != 0 {
		t.Fatalf("sendKeepalive put %d deadlines on the connection, a deadline there "+
			"also cuts the forwarded writes in progress", n)
	}
}

// TestKeepaliveRequestReturnsOnceTheClientIsClosed pins what keeps the goroutine
// sendKeepalive leaves behind from leaking: a request that waits for a reply
// that never comes returns as soon as the client is closed.
func TestKeepaliveRequestReturnsOnceTheClientIsClosed(t *testing.T) {
	client, _, closeClient := dialTestSSHClient(t, startMuteSSHServer(t))
	defer closeClient()

	returned := make(chan error, 1)
	go func() {
		_, _, err := client.SendRequest("keepalive@tunnel", true, nil)
		returned <- err
	}()

	// Long enough for the request to be on the wire and waiting.
	time.Sleep(100 * time.Millisecond)
	_ = client.Close()

	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("the request reported a reply from a peer that never sent one")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request is still waiting after the client was closed, " +
			"every keepalive that timed out would leave a goroutine behind")
	}
}

// TestMonitorTickIsNotHeldByAnUnresponsivePeer is the same check one level up:
// a peer that stops answering must cost the monitor one tick, not a TCP
// retransmission timeout.
func TestMonitorTickIsNotHeldByAnUnresponsivePeer(t *testing.T) {
	m := newSSHTestManager(t, 1)
	serverAddr := startMuteSSHServer(t)
	tun, tunnel := newSSHTestTunnel(t, serverAddr)

	client, _, closeClient := dialTestSSHClient(t, serverAddr)
	defer closeClient()

	tun.clientMu.Lock()
	tun.client = client
	tun.clientMu.Unlock()

	stop := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		tun.monitorConnection(m, tunnel, stop)
		close(exited)
	}()
	defer func() {
		close(stop)
		<-exited
	}()

	// The server accepts, so the dial succeeds and the tick reaches the
	// keepalive. One tick to wake, plus the dial and keepalive budgets, is well
	// inside this.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		tun.clientMu.RLock()
		got := tun.client
		tun.clientMu.RUnlock()

		// The monitor writes the row under tunnelMu, so reading it takes the
		// same lock.
		tun.tunnelMu.Lock()
		status := tunnel.Status
		tun.tunnelMu.Unlock()

		if got == nil && status == "reconnecting" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("the monitor still holds a connection to a peer that stopped answering, " +
		"its keepalive has no deadline and the tick is blocked")
}

// TestSendKeepaliveLeavesTheConnectionAlone checks the other half. A deadline
// on the connection, armed or cleared, applies to the forwarded writes in
// progress while it stands, and one of them that outlasts it fails the whole
// connection. A keepalive that is answered leaves no trace on the connection.
func TestSendKeepaliveLeavesTheConnectionAlone(t *testing.T) {
	serverAddr, _ := startForwardingSSHServer(t)

	client, conn, closeClient := dialTestSSHClient(t, serverAddr)
	defer closeClient()

	const timeout = 200 * time.Millisecond

	if err := sendKeepalive(client, timeout); err != nil {
		t.Fatalf("sendKeepalive failed against a server that answers: %v", err)
	}

	if n := conn.deadlines.Load(); n != 0 {
		t.Fatalf("sendKeepalive put %d deadlines on the connection, a deadline there "+
			"also cuts the forwarded writes in progress", n)
	}
	if conn.closed.Load() {
		t.Fatal("sendKeepalive closed a client whose peer answered")
	}

	// Past the timeout the keepalive used. Anything it left armed has torn the
	// connection down by now.
	time.Sleep(4 * timeout)

	for i := 0; i < 3; i++ {
		if _, _, err := client.SendRequest("keepalive@tunnel", true, nil); err != nil {
			t.Fatalf("request %d over the connection failed after the keepalive: %v, "+
				"the keepalive left the connection broken", i, err)
		}
		time.Sleep(2 * timeout)
	}

	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("the connection is no longer usable after the keepalive: %v", err)
	}
}

// startSlowSSHServer completes the SSH handshake and answers every keepalive
// after delay, the way a live server behind a busy or slow link does. It
// reports when each keepalive arrived.
func startSlowSSHServer(t *testing.T, delay time.Duration) (string, func() []time.Time) {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate host key: %v", err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("failed to create signer: %v", err)
	}

	config := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	config.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	var mu sync.Mutex
	var conns []net.Conn
	var arrivals []time.Time

	t.Cleanup(func() {
		_ = ln.Close()

		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()

			go func(conn net.Conn) {
				_, chans, reqs, err := ssh.NewServerConn(conn, config)
				if err != nil {
					_ = conn.Close()
					return
				}

				go func() {
					for newChannel := range chans {
						_ = newChannel.Reject(ssh.Prohibited, "no channels here")
					}
				}()

				for req := range reqs {
					if req.Type != "keepalive@tunnel" {
						if req.WantReply {
							_ = req.Reply(false, nil)
						}
						continue
					}

					mu.Lock()
					arrivals = append(arrivals, time.Now())
					mu.Unlock()

					time.Sleep(delay)
					_ = req.Reply(true, nil)
				}
			}(conn)
		}
	}()

	return ln.Addr().String(), func() []time.Time {
		mu.Lock()
		defer mu.Unlock()

		return append([]time.Time(nil), arrivals...)
	}
}

// TestSlowKeepaliveReplyKeepsTheConnection is what the timeout of one interval
// is for. With the default interval of five seconds the keepalive used to wait
// 1.25s, so a reply held up behind forwarded traffic for longer than that cost
// the connection and every forward on it. A reply that comes inside the
// interval now keeps it.
func TestSlowKeepaliveReplyKeepsTheConnection(t *testing.T) {
	const delay = 1600 * time.Millisecond
	serverAddr, _ := startSlowSSHServer(t, delay)

	client, conn, closeClient := dialTestSSHClient(t, serverAddr)
	defer closeClient()

	checkSSHConnection(serverAddr, client, monitorCheckTimeout(5*time.Second), zap.NewNop(),
		func(extra ...zap.Field) []zap.Field { return extra })

	if conn.closed.Load() {
		t.Fatalf("the check closed a connection whose server answered after %v, "+
			"inside the %v interval", delay, 5*time.Second)
	}
	if _, _, err := client.SendRequest("keepalive@tunnel", true, nil); err != nil {
		t.Fatalf("the connection is no longer usable after a slow keepalive: %v", err)
	}
}

// TestWatchSSHConnectionDoesNotCatchUp pins that the checks never run back to
// back. A check that took longer than the interval used to leave a tick
// pending, and the next check went out the moment the last one ended. The next
// check now waits a whole interval from the end of the last one.
func TestWatchSSHConnectionDoesNotCatchUp(t *testing.T) {
	const (
		interval = 400 * time.Millisecond
		delay    = 800 * time.Millisecond
	)
	serverAddr, arrivals := startSlowSSHServer(t, delay)

	client, conn, closeClient := dialTestSSHClient(t, serverAddr)
	defer closeClient()

	stop := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		watchSSHConnection(stop, interval, serverAddr, func() *ssh.Client { return client },
			zap.NewNop(), func(extra ...zap.Field) []zap.Field { return extra })
		close(exited)
	}()

	deadline := time.Now().Add(15 * time.Second)
	for len(arrivals()) < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	close(stop)
	<-exited

	got := arrivals()
	if len(got) < 3 {
		t.Fatalf("the server saw %d keepalives, want at least 3", len(got))
	}
	if conn.closed.Load() {
		t.Fatal("a keepalive answered inside the 3s floor closed the connection")
	}

	// A check lasts about delay, so a caught up tick sends the next one about
	// delay after the last, while the timer waits delay plus interval.
	for i := 1; i < len(got); i++ {
		if gap := got[i].Sub(got[i-1]); gap < delay+interval/2 {
			t.Fatalf("keepalive %d came %v after the one before, want at least %v, "+
				"the check went out straight after the last one ended", i, gap, delay+interval/2)
		}
	}
}

// startMonitorOnSlowServer runs monitorConnection for a tunnel that holds a
// connection to a server answering keepalives after delay, and hands back the
// tunnel, its row, the client, the connection under it and when the server saw
// each keepalive. The monitor stops when the test ends.
func startMonitorOnSlowServer(t *testing.T, monitoringIntervalSec int, delay time.Duration) (
	*SSHTunnel, *models.Tunnel, *ssh.Client, *deadlineRecordingConn, func() []time.Time) {
	t.Helper()

	m := newSSHTestManager(t, monitoringIntervalSec)
	serverAddr, arrivals := startSlowSSHServer(t, delay)
	tun, tunnel := newSSHTestTunnel(t, serverAddr)

	client, conn, closeClient := dialTestSSHClient(t, serverAddr)
	t.Cleanup(closeClient)

	tun.clientMu.Lock()
	tun.client = client
	tun.clientMu.Unlock()

	tun.tunnelMu.Lock()
	tunnel.Status = "connected"
	tun.tunnelMu.Unlock()

	stop := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		tun.monitorConnection(m, tunnel, stop)
		close(exited)
	}()
	t.Cleanup(func() {
		close(stop)
		<-exited
	})

	return tun, tunnel, client, conn, arrivals
}

// TestMonitorConnectionDoesNotCatchUp is TestWatchSSHConnectionDoesNotCatchUp
// for the loop of a tunnel.
func TestMonitorConnectionDoesNotCatchUp(t *testing.T) {
	const (
		interval = time.Second
		delay    = 1500 * time.Millisecond
	)
	_, _, _, conn, arrivals := startMonitorOnSlowServer(t, int(interval/time.Second), delay)

	deadline := time.Now().Add(20 * time.Second)
	for len(arrivals()) < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	got := arrivals()
	if len(got) < 3 {
		t.Fatalf("the server saw %d keepalives, want at least 3", len(got))
	}
	if conn.closed.Load() {
		t.Fatal("a keepalive answered inside the 3s floor closed the connection")
	}

	// A check lasts about delay, so a caught up tick sends the next one about
	// delay after the last, while the timer waits delay plus interval.
	for i := 1; i < len(got); i++ {
		if gap := got[i].Sub(got[i-1]); gap < delay+interval/2 {
			t.Fatalf("keepalive %d came %v after the one before, want at least %v, "+
				"the check went out straight after the last one ended", i, gap, delay+interval/2)
		}
	}
}

// TestMonitorConnectionKeepsATunnelWithASlowReply is
// TestSlowKeepaliveReplyKeepsTheConnection for a tunnel at the default interval
// of five seconds: a reply that comes after the 1.25s the keepalive used to
// wait, but inside the interval, leaves the tunnel connected.
func TestMonitorConnectionKeepsATunnelWithASlowReply(t *testing.T) {
	const delay = 1600 * time.Millisecond
	tun, tunnel, client, conn, arrivals := startMonitorOnSlowServer(t, 5, delay)

	deadline := time.Now().Add(15 * time.Second)
	for len(arrivals()) < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(arrivals()) < 1 {
		t.Fatal("the monitor sent no keepalive")
	}

	// Long enough for the reply to arrive and the check to finish.
	time.Sleep(delay + 500*time.Millisecond)

	tun.clientMu.RLock()
	current := tun.client
	tun.clientMu.RUnlock()

	tun.tunnelMu.Lock()
	status := tunnel.Status
	retries := tunnel.RetryCount
	tun.tunnelMu.Unlock()

	if conn.closed.Load() || current != client {
		t.Fatalf("the monitor dropped a connection whose server answered after %v, "+
			"inside the 5s interval", delay)
	}
	if status != "connected" || retries != 0 {
		t.Fatalf("status = %q with %d retries, want %q with none, "+
			"a slow reply was taken for a lost server", status, retries, "connected")
	}
}

// forwardTestTimeout bounds every read, write and handover in the forward
// tests. Everything they move goes over loopback between goroutines of this
// process, so a second is already far more than the work needs: the value is
// only high enough that a loaded build machine cannot fail the test.
const forwardTestTimeout = 10 * time.Second

// newForwardTunnel returns a tunnel whose remote address is remoteAddr, which
// is the service forward connects the accepted connection to. forward uses
// neither the SSH configuration nor the manager, so it is built without one.
func newForwardTunnel(t *testing.T, remoteAddr string) *SSHTunnel {
	t.Helper()

	hostID := uint(1)
	spID := uint(1)

	tun, err := NewSSHTunnel(&hostID, &spID, "127.0.0.1:0", "[::1]:0", "127.0.0.1:1", remoteAddr, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("failed to create tunnel: %v", err)
	}

	return tun
}

// startLocalService listens on loopback and hands every accepted connection to
// the test. It stands for the service a tunnel forwards to.
func startLocalService(t *testing.T) (string, <-chan net.Conn) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	conns := make(chan net.Conn, 16)

	t.Cleanup(func() {
		_ = ln.Close()

		for {
			select {
			case conn := <-conns:
				_ = conn.Close()
			default:
				return
			}
		}
	})

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			select {
			case conns <- conn:
			default:
				_ = conn.Close()
			}
		}
	}()

	return ln.Addr().String(), conns
}

// serviceConn waits for the connection forward opened to the service.
func serviceConn(t *testing.T, conns <-chan net.Conn) net.Conn {
	t.Helper()

	select {
	case conn := <-conns:
		return conn
	case <-time.After(forwardTestTimeout):
		t.Fatal("forward never connected to the local service")
		return nil
	}
}

// runForward starts forward on one end of a pipe and returns the other end,
// which stands for the connection the remote listener accepted, together with
// a channel that is closed once forward returned. The deadline on the test end
// keeps a direction that carries nothing from hanging the test.
func runForward(t *testing.T, tun *SSHTunnel, deadline time.Time) (net.Conn, <-chan struct{}) {
	t.Helper()

	remoteSide, tunnelSide := net.Pipe()

	err := remoteSide.SetDeadline(deadline)
	if err != nil {
		t.Fatalf("failed to set the deadline: %v", err)
	}

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		tun.forward(tunnelSide, forwardIdleTimeout)
	}()

	return remoteSide, returned
}

// forwardTestIdleTimeout is the idle bound the tests that have to watch it
// expire hand to forward. No test can wait out forwardIdleTimeout, and this is
// still far longer than the connections of a test stay silent by accident.
const forwardTestIdleTimeout = 200 * time.Millisecond

// forwardTestAnswerSize is how much a service sends back in the tests that
// check an answer arrived whole. It is larger than the buffers of the sockets
// on the way, so an answer that was cut off cannot have been buffered into
// looking complete.
const forwardTestAnswerSize = 4 << 20

// runForwardOverTCP does what runForward does, but over a loopback TCP
// connection instead of a pipe. A pipe cannot be half-closed (net/pipe.go has
// no CloseWrite), so it is the only way to stand in for the connection the
// remote listener accepts, which can.
func runForwardOverTCP(t *testing.T, tun *SSHTunnel, deadline time.Time, idleTimeout time.Duration) (*net.TCPConn, <-chan struct{}) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer func() {
		_ = ln.Close()
	}()

	remoteSide, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("failed to dial the tunnel side: %v", err)
	}
	t.Cleanup(func() {
		_ = remoteSide.Close()
	})

	tunnelSide, err := ln.Accept()
	if err != nil {
		t.Fatalf("failed to accept the tunnel side: %v", err)
	}

	err = remoteSide.SetDeadline(deadline)
	if err != nil {
		t.Fatalf("failed to set the deadline: %v", err)
	}

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		tun.forward(tunnelSide, idleTimeout)
	}()

	tcp, ok := remoteSide.(*net.TCPConn)
	if !ok {
		t.Fatalf("the tunnel side is a %T, not a TCP connection", remoteSide)
	}

	return tcp, returned
}

// TestForwardDeliversTheAnswerAfterTheTunnelSideHalfCloses is the case that
// half-closing exists for: the client sends its request, closes its writing
// side and keeps receiving. Returning on the first of the two copies cut the
// answer off right there.
func TestForwardDeliversTheAnswerAfterTheTunnelSideHalfCloses(t *testing.T) {
	serviceAddr, conns := startLocalService(t)
	tun := newForwardTunnel(t, serviceAddr)

	deadline := time.Now().Add(forwardTestTimeout)
	remoteSide, returned := runForwardOverTCP(t, tun, deadline, forwardIdleTimeout)

	service := serviceConn(t, conns)
	defer func() {
		_ = service.Close()
	}()

	err := service.SetDeadline(deadline)
	if err != nil {
		t.Fatalf("failed to set the deadline: %v", err)
	}

	_, err = remoteSide.Write([]byte("request"))
	if err != nil {
		t.Fatalf("failed to write to the connection forward is serving: %v", err)
	}
	err = remoteSide.CloseWrite()
	if err != nil {
		t.Fatalf("failed to close the writing side of the connection forward is serving: %v", err)
	}

	// The end of the request has to reach the service, or it never knows the
	// request is complete and never answers.
	got, err := io.ReadAll(service)
	if err != nil {
		t.Fatalf("reading the request the service received failed: %v", err)
	}
	if string(got) != "request" {
		t.Fatalf("the service received %q, want %q", got, "request")
	}

	// The service answers a client that is no longer sending. The write runs
	// on its own goroutine because the answer does not fit in the buffers on
	// the way and only moves while the read below drains it.
	answer := bytes.Repeat([]byte("a"), forwardTestAnswerSize)
	answered := make(chan error, 1)
	go func() {
		_, err := service.Write(answer)
		if err != nil {
			answered <- err
			return
		}
		answered <- service.Close()
	}()

	back, err := io.ReadAll(remoteSide)
	if err != nil {
		t.Fatalf("reading the answer through the tunnel failed: %v", err)
	}
	if len(back) != len(answer) {
		t.Fatalf("the answer came back as %d bytes, want %d, it was cut off", len(back), len(answer))
	}
	if !bytes.Equal(back, answer) {
		t.Fatal("the answer that came back is not what the service sent")
	}

	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("the service could not send its answer: %v", err)
		}
	case <-time.After(forwardTestTimeout):
		t.Fatal("the service never finished sending its answer")
	}

	waitForwardReturned(t, returned, "after both directions ended")
}

// TestForwardEndsWhenNothingMovesInEitherDirection covers the peer that
// neither sends nor closes. Both copies sit in a read there, so only the idle
// bound gets forward back.
func TestForwardEndsWhenNothingMovesInEitherDirection(t *testing.T) {
	serviceAddr, conns := startLocalService(t)
	tun := newForwardTunnel(t, serviceAddr)

	deadline := time.Now().Add(forwardTestTimeout)
	remoteSide, returned := runForwardOverTCP(t, tun, deadline, forwardTestIdleTimeout)

	// The service is left open and silent, and so is the tunnel side.
	service := serviceConn(t, conns)
	defer func() {
		_ = service.Close()
	}()

	waitForwardReturned(t, returned, "although neither end sent anything or closed")

	// Both connections are gone with it, or the sockets would be held by a
	// connection nobody is serving anymore.
	_, err := remoteSide.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("the connection from the remote listener is still open after forward gave up on it")
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("reading the connection forward served returned %v, want %v, it was not closed", err, io.EOF)
	}

	err = service.SetDeadline(time.Now().Add(forwardTestTimeout))
	if err != nil {
		t.Fatalf("failed to set the deadline: %v", err)
	}
	_, err = service.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("the connection to the service is still open after forward gave up on it")
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("reading the service connection returned %v, want %v, it was not closed", err, io.EOF)
	}
}

func waitForwardReturned(t *testing.T, returned <-chan struct{}, what string) {
	t.Helper()

	select {
	case <-returned:
	case <-time.After(forwardTestTimeout):
		t.Fatalf("forward did not return %s", what)
	}
}

func TestForwardCarriesBytesBothWays(t *testing.T) {
	serviceAddr, conns := startLocalService(t)
	tun := newForwardTunnel(t, serviceAddr)

	deadline := time.Now().Add(forwardTestTimeout)
	remoteSide, returned := runForward(t, tun, deadline)
	defer func() {
		_ = remoteSide.Close()
	}()

	service := serviceConn(t, conns)
	defer func() {
		_ = service.Close()
	}()

	err := service.SetDeadline(deadline)
	if err != nil {
		t.Fatalf("failed to set the deadline: %v", err)
	}

	// What the remote listener accepted has to reach the service.
	_, err = remoteSide.Write([]byte("request"))
	if err != nil {
		t.Fatalf("failed to write to the connection forward is serving: %v", err)
	}

	got := make([]byte, len("request"))
	_, err = io.ReadFull(service, got)
	if err != nil {
		t.Fatalf("the service never received what was sent to the tunnel: %v", err)
	}
	if string(got) != "request" {
		t.Fatalf("the service received %q, want %q", got, "request")
	}

	// And what the service answers has to come back the other way.
	_, err = service.Write([]byte("answer"))
	if err != nil {
		t.Fatalf("failed to write the answer of the service: %v", err)
	}

	got = make([]byte, len("answer"))
	_, err = io.ReadFull(remoteSide, got)
	if err != nil {
		t.Fatalf("the answer of the service never came back through the tunnel: %v", err)
	}
	if string(got) != "answer" {
		t.Fatalf("the answer that came back is %q, want %q", got, "answer")
	}

	_ = remoteSide.Close()
	waitForwardReturned(t, returned, "after the connection it serves was closed")
}

func TestForwardClosesTheServiceWhenTheTunnelSideCloses(t *testing.T) {
	serviceAddr, conns := startLocalService(t)
	tun := newForwardTunnel(t, serviceAddr)

	deadline := time.Now().Add(forwardTestTimeout)
	remoteSide, returned := runForward(t, tun, deadline)

	service := serviceConn(t, conns)
	defer func() {
		_ = service.Close()
	}()

	err := service.SetDeadline(deadline)
	if err != nil {
		t.Fatalf("failed to set the deadline: %v", err)
	}

	_ = remoteSide.Close()

	waitForwardReturned(t, returned, "after the connection it serves was closed")

	// A service connection left open outlives every tunnel connection and the
	// service runs out of them.
	_, err = service.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("the connection to the service is still open after the tunnel side closed")
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("reading the service connection returned %v, want %v, it was not closed", err, io.EOF)
	}
}

func TestForwardClosesTheTunnelSideWhenTheServiceCloses(t *testing.T) {
	serviceAddr, conns := startLocalService(t)
	tun := newForwardTunnel(t, serviceAddr)

	deadline := time.Now().Add(forwardTestTimeout)
	remoteSide, returned := runForward(t, tun, deadline)
	defer func() {
		_ = remoteSide.Close()
	}()

	service := serviceConn(t, conns)
	_ = service.Close()

	waitForwardReturned(t, returned, "after the service closed the connection")

	_, err := remoteSide.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("the connection from the remote listener is still open after the service closed")
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("reading the connection forward served returned %v, want %v, it was not closed", err, io.EOF)
	}
}

func TestForwardClosesTheTunnelSideWhenTheServiceCannotBeReached(t *testing.T) {
	// Port 1 on loopback needs privileges to bind, so nothing listens there and
	// the dial is refused right away.
	tun := newForwardTunnel(t, "127.0.0.1:1")

	remoteSide, returned := runForward(t, tun, time.Now().Add(forwardTestTimeout))
	defer func() {
		_ = remoteSide.Close()
	}()

	waitForwardReturned(t, returned, "although the service cannot be reached")

	_, err := remoteSide.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("the connection from the remote listener is still open although the service was never reached")
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("reading the connection forward served returned %v, want %v, it was not closed", err, io.EOF)
	}
}

// countForwardGoroutines reports how many goroutines currently sit in forward
// or in one of the copies it started.
func countForwardGoroutines() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return bytes.Count(buf[:n], []byte("tunnel.(*SSHTunnel).forward"))
		}
		buf = make([]byte, 2*len(buf))
	}
}

func waitForwardGoroutines(t *testing.T, want int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	got := countForwardGoroutines()
	for time.Now().Before(deadline) {
		if got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
		got = countForwardGoroutines()
	}

	if got != want {
		t.Fatalf("goroutines in forward = %d, want %d, a served connection left one behind", got, want)
	}
}

func TestForwardLeavesNoGoroutineBehind(t *testing.T) {
	const connections = 5

	waitForwardGoroutines(t, 0, forwardTestTimeout)

	serviceAddr, conns := startLocalService(t)
	tun := newForwardTunnel(t, serviceAddr)

	deadline := time.Now().Add(forwardTestTimeout)

	remoteSides := make([]net.Conn, 0, connections)
	services := make([]net.Conn, 0, connections)
	returns := make([]<-chan struct{}, 0, connections)

	for i := 0; i < connections; i++ {
		remoteSide, returned := runForward(t, tun, deadline)
		remoteSides = append(remoteSides, remoteSide)
		returns = append(returns, returned)

		service := serviceConn(t, conns)
		services = append(services, service)

		err := service.SetDeadline(deadline)
		if err != nil {
			t.Fatalf("failed to set the deadline: %v", err)
		}

		// Both copies are blocked on a read from here on, which is where a
		// connection that is only closed on one side leaves one of them.
		_, err = remoteSide.Write([]byte("request"))
		if err != nil {
			t.Fatalf("failed to write to the connection forward is serving: %v", err)
		}
		_, err = io.ReadFull(service, make([]byte, len("request")))
		if err != nil {
			t.Fatalf("the service never received what was sent to the tunnel: %v", err)
		}
	}

	for _, remoteSide := range remoteSides {
		_ = remoteSide.Close()
	}

	for i, returned := range returns {
		waitForwardReturned(t, returned, fmt.Sprintf("for connection %d after it was closed", i))
	}

	waitForwardGoroutines(t, 0, forwardTestTimeout)

	for _, service := range services {
		_ = service.Close()
	}
}

func TestForwardEndsWhenTheServiceConnectionIsReset(t *testing.T) {
	serviceAddr, conns := startLocalService(t)

	core, logs := observer.New(zapcore.DebugLevel)

	tun := newForwardTunnel(t, serviceAddr)
	tun.logger = zap.New(core)

	deadline := time.Now().Add(forwardTestTimeout)
	remoteSide, returned := runForward(t, tun, deadline)
	defer func() {
		_ = remoteSide.Close()
	}()

	service := serviceConn(t, conns)

	// A byte is put through before anything is broken, so that the reset below
	// lands on a copy that is running rather than on a dial that has not come
	// back yet.
	//
	// Having the service side of the connection is not enough on its own: the
	// kernel completes the handshake and the accept hands the connection over
	// while the Dial in forward is still returning. Resetting in that window
	// makes forward fail at the dial, which is a different error on a different
	// line, and this test then looks for a copy that never ran. On a loaded
	// machine that window is wide: the test failed twenty-two times in thirty
	// under load and not once on an idle one.
	_, err := remoteSide.Write([]byte("request"))
	if err != nil {
		t.Fatalf("failed to write to the connection forward serves: %v", err)
	}

	_, err = io.ReadFull(service, make([]byte, len("request")))
	if err != nil {
		t.Fatalf("the service did not receive what was written to it: %v", err)
	}

	// Linger 0 makes the close send a reset, which is what a service that dies
	// does to the connection. The copy from it then fails with an error that is
	// not the end of the stream.
	tcp, ok := service.(*net.TCPConn)
	if !ok {
		t.Fatalf("the service connection is a %T, not a TCP connection", service)
	}
	err = tcp.SetLinger(0)
	if err != nil {
		t.Fatalf("failed to set linger: %v", err)
	}
	_ = tcp.Close()

	waitForwardReturned(t, returned, "after the service connection was reset")

	_, err = remoteSide.Read(make([]byte, 1))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("reading the connection forward served returned %v, want %v, it was not closed", err, io.EOF)
	}

	// A copy that ended on an error and not on the end of the stream is worth
	// a line, so a tunnel that keeps losing connections can be told apart from
	// one whose peers simply close them.
	if logs.FilterMessage("copy error").Len() == 0 {
		t.Fatalf("a copy that failed was not logged, the entries are %v", logs.All())
	}
}

// startBlackholeListener returns an address that neither answers a connection
// nor refuses one, which is the case the probe timeout exists for.
//
// The socket listens with a backlog of one and nothing ever accepts from it, so
// the connection made here fills the queue and every SYN after it is dropped by
// the kernel instead of being answered. net.Listen has no way to ask for a
// backlog, so the socket is built with the syscall package, and what a full
// queue does is the kernel's own behaviour, so the test that uses this runs on
// Linux alone.
func startBlackholeListener(t *testing.T) string {
	t.Helper()

	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("failed to open a socket: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Close(fd)
	})

	err = syscall.Bind(fd, &syscall.SockaddrInet4{Port: 0, Addr: [4]byte{127, 0, 0, 1}})
	if err != nil {
		t.Fatalf("failed to bind the socket: %v", err)
	}

	err = syscall.Listen(fd, 0)
	if err != nil {
		t.Fatalf("failed to listen with a backlog of one: %v", err)
	}

	bound, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatalf("failed to read the bound address: %v", err)
	}

	sa, ok := bound.(*syscall.SockaddrInet4)
	if !ok {
		t.Fatalf("the bound address is %T, want an IPv4 one", bound)
	}

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(sa.Port))

	filler, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("failed to fill the accept queue: %v", err)
	}
	t.Cleanup(func() {
		_ = filler.Close()
	})

	return addr
}

// closedPort returns an address nothing listens on. The port was listened on a
// moment earlier, so it is one this machine hands out, and it is closed again
// before it is returned.
func closedPort(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	addr := ln.Addr().String()

	err = ln.Close()
	if err != nil {
		t.Fatalf("failed to close the listener: %v", err)
	}

	return addr
}

func TestProbeForwardReachAnswersFromTheHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer func() {
		_ = ln.Close()
	}()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	reach := probeForwardReach(ln.Addr().String(), forwardProbeTimeout, forwardUnreachable)
	if reach != forwardReachable {
		t.Fatalf("reach = %q, want %q, the port answered the handshake", reach, forwardReachable)
	}

	reach = probeForwardReach(closedPort(t), forwardProbeTimeout, forwardUnreachable)
	if reach != forwardUnreachable {
		t.Fatalf("reach = %q, want %q, nothing listens on that port", reach, forwardUnreachable)
	}
}

// TestProbeForwardReachClosesWhatItOpened is what keeps the probe from being a
// socket leak of its own. It runs on the same address many times over, and a
// probe that held what it opened would leave one connection per run on both
// ends of it.
func TestProbeForwardReachClosesWhatItOpened(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer func() {
		_ = ln.Close()
	}()

	var mu sync.Mutex
	var open int
	var peak int
	// done counts the connections the listener has finished with. The probe
	// returns before its close reaches the far side, so the return is not a
	// point at which the listener is known to have seen anything. Counting
	// what it has finished gives the test such a point.
	var done int

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			go func(conn net.Conn) {
				defer func() {
					_ = conn.Close()
				}()

				mu.Lock()
				open++
				if open > peak {
					peak = open
				}
				mu.Unlock()

				// The probe closes as soon as the handshake stands, so this
				// read ends at once. A probe that held the connection would
				// sit here instead and the count would climb.
				_, _ = conn.Read(make([]byte, 1))

				mu.Lock()
				open--
				done++
				mu.Unlock()
			}(conn)
		}
	}()

	// The probes are run one at a time, and the next one waits for the side
	// that accepted the last to have let go of it.
	//
	// The count is kept by the listener, and it is lowered when the read ends,
	// which is when the close of the probe arrives. That arrival is not the
	// return of probeForwardReach: the probe has already returned by the time
	// the FIN is delivered. Started back to back, a probe would be counted
	// while the one before it was still being let go of, and the peak would
	// read as two without a single socket having been held.
	//
	// Waiting here is what makes a peak above one mean what it says. A probe
	// that kept its connection leaves the read of the listener blocked, the
	// listener never finishes with it, and the wait below runs out and says so.
	for i := 0; i < 20; i++ {
		reach := probeForwardReach(ln.Addr().String(), forwardProbeTimeout, forwardUnreachable)
		if reach != forwardReachable {
			t.Fatalf("probe %d: reach = %q, want %q", i, reach, forwardReachable)
		}

		if !waitForFinished(&mu, &done, i+1, 5*time.Second) {
			t.Fatalf("probe %d returned, but the listener had not finished with the connection it "+
				"opened five seconds later, so the probe does not close what it opens", i)
		}
	}

	mu.Lock()
	left := open
	highest := peak
	mu.Unlock()

	if left != 0 {
		t.Fatalf("%d of the 20 probes are still open, so every probe leaves a socket behind", left)
	}
	if highest > 1 {
		t.Fatalf("%d probes were open at once, so a probe outlives the one that follows it", highest)
	}
}

// waitForFinished waits for the counter to reach want and says whether it did
// inside the time given. The counter is read under the lock it is written
// under, and the wait is bounded so that a connection the listener never
// finishes with fails the test rather than hanging it.
func waitForFinished(mu *sync.Mutex, done *int, want int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		mu.Lock()
		reached := *done
		mu.Unlock()

		if reached >= want {
			return true
		}

		if !time.Now().Before(deadline) {
			return false
		}

		time.Sleep(time.Millisecond)
	}
}

// TestProbeForwardReachGivesUpOnAPortThatNeverAnswers pins the bound itself.
// Without one, a port whose SYN is dropped rather than refused is waited on
// until the kernel stops retransmitting, which is around two minutes on Linux,
// and the goroutine and the socket of the probe are held for all of it. It is
// the same reason forwardDialTimeout is bounded, one level up.
func TestProbeForwardReachGivesUpOnAPortThatNeverAnswers(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the silent address is a listening socket whose accept queue is full, and what a full " +
			"queue does with a SYN is the kernel's own behaviour")
	}

	addr := startBlackholeListener(t)

	const timeout = 500 * time.Millisecond

	start := time.Now()
	reach := probeForwardReach(addr, timeout, forwardUnreachable)
	held := time.Since(start)

	if reach != forwardUnreachable {
		t.Fatalf("reach = %q, want %q, the port never answered", reach, forwardUnreachable)
	}
	if held < timeout {
		t.Fatalf("the probe returned after %v, inside its timeout of %v, so the address answered "+
			"and the test measured nothing", held, timeout)
	}
	if held > 4*timeout {
		t.Fatalf("the probe held for %v with a timeout of %v, so a port that drops the packet is "+
			"waited on for as long as the kernel retries", held, timeout)
	}
}

// TestForwardProbeTimeoutMatchesTheForwardDial keeps the two bounds one number.
// They are the same kind of wait against the same kind of peer, and two numbers
// to reason about is one more than there is reason for.
func TestForwardProbeTimeoutMatchesTheForwardDial(t *testing.T) {
	if forwardProbeTimeout != forwardDialTimeout {
		t.Fatalf("forwardProbeTimeout = %v, forwardDialTimeout = %v, want the same bound",
			forwardProbeTimeout, forwardDialTimeout)
	}
	if forwardProbeTimeout <= 0 {
		t.Fatalf("forwardProbeTimeout = %v, so the probe is dialed with no bound at all", forwardProbeTimeout)
	}
}

func TestForwardProbeAddressIsTheServerAtTheConfirmedPort(t *testing.T) {
	tests := []struct {
		name   string
		server string
		port   int
		want   string
	}{
		{name: "IPv4", server: "192.0.2.10:22", port: 18080, want: "192.0.2.10:18080"},
		{name: "IPv6 is bracketed", server: "[2001:db8::1]:22", port: 18080, want: "[2001:db8::1]:18080"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := forwardProbeAddress(resolveTestTCPAddr(t, tt.server), tt.port)
			if got != tt.want {
				t.Fatalf("forwardProbeAddress = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestASilenceIsAReadingOnlyWhereAnAnswerWasExpected holds the probe to what it
// can measure. There is one address of the Host here and it carries one address
// family, so a port asked for on the other family cannot be tried at all, and a
// port asked for on the loopback addresses is on the Host where nothing here
// reaches it. Silence from either is not a port that cannot be reached, and
// written down as one it puts a failure on a tunnel that is carrying traffic.
func TestASilenceIsAReadingOnlyWhereAnAnswerWasExpected(t *testing.T) {
	tests := []struct {
		name    string
		localV4 string
		localV6 string
		server  string
		reach   string
		want    string
	}{
		{"both were answered and the Host is dialled over IPv4", "0.0.0.0:80", "[::]:80",
			"192.0.2.10:22", openReachBoth, forwardUnreachable},
		{"both were answered and the Host is dialled over IPv6", "0.0.0.0:80", "[::]:80",
			"[2001:db8::1]:22", openReachBoth, forwardUnreachable},
		{"the family being dialled was answered", "0.0.0.0:80", "[::]:80",
			"192.0.2.10:22", openReachV4, forwardUnreachable},
		{"the IPv6 request was the one answered, and the Host is dialled over IPv4",
			"0.0.0.0:80", "[::]:80", "192.0.2.10:22", openReachV6, forwardReachUnknown},
		{"the IPv4 request was the one answered, and the Host is dialled over IPv6",
			"0.0.0.0:80", "[::]:80", "[2001:db8::1]:22", openReachV4, forwardReachUnknown},
		{"the ports were asked for on the loopback of the Host", "127.0.0.1:80", "[::1]:80",
			"192.0.2.10:22", openReachBoth, forwardReachUnknown},
		{"the loopback of the Host over IPv6", "127.0.0.1:80", "[::1]:80",
			"[2001:db8::1]:22", openReachBoth, forwardReachUnknown},
		{"nothing is known of what the server answered", "0.0.0.0:80", "[::]:80",
			"192.0.2.10:22", "", forwardReachUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			local := localPair{v4: resolveTestTCPAddr(t, tt.localV4), v6: resolveTestTCPAddr(t, tt.localV6)}
			server := resolveTestTCPAddr(t, tt.server)

			got := forwardProbeSilence(local, server, tt.reach)
			if got != tt.want {
				t.Fatalf("forwardProbeSilence = %q, want %q", got, tt.want)
			}
		})
	}
}

// resolveTestTCPAddr parses an IP literal and port with the failure reported
// here, so that a table of addresses reads as a table.
func resolveTestTCPAddr(t *testing.T, address string) *net.TCPAddr {
	t.Helper()

	addr, err := netip.ParseAddrPort(address)
	if err != nil {
		t.Fatalf("failed to parse %q: %v", address, err)
	}

	return net.TCPAddrFromAddrPort(addr)
}

// readTunnelReach reads the two readings the way the probe writes them, under
// the lock the tunnel row is written with, so the test is not a race of its own.
func readTunnelReach(tun *SSHTunnel, tunnel *models.Tunnel) (string, string) {
	tun.tunnelMu.Lock()
	defer tun.tunnelMu.Unlock()

	return tunnel.ServerBanner, tunnel.ForwardReach
}

// waitTunnelReach waits until the probe has written a reading for the
// connection that is up.
func waitTunnelReach(t *testing.T, tun *SSHTunnel, tunnel *models.Tunnel, timeout time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, reach := readTunnelReach(tun, tunnel)
		if reach != forwardReachUnknown {
			return reach
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("the forwarded port was never measured")

	return ""
}

// waitTunnelBanner waits until the connection has written what the server
// called itself on the row, and returns it.
//
// The banner is waited for rather than the reading of the probe, which is what
// the tunnel is up by a moment earlier. A probe can end on a reading of unknown,
// which is what the row already holds, so there is no value to wait for there.
func waitTunnelBanner(t *testing.T, tun *SSHTunnel, tunnel *models.Tunnel, timeout time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if banner, _ := readTunnelReach(tun, tunnel); banner != "" {
			return banner
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("the connection never wrote what the server called itself")

	return ""
}

// TestEstablishConnectionRecordsWhatTheServerSaid pins the banner on the tunnel
// row. It is what decides which of the things to check is shown for a port that
// did not answer, since what opens a forwarded port differs between servers.
func TestEstablishConnectionRecordsWhatTheServerSaid(t *testing.T) {
	m := newSSHTestManager(t, 1)
	serverAddr, _ := startForwardingSSHServer(t)
	tun, tunnel := newSSHTestTunnel(t, serverAddr)

	errc := make(chan error, 1)
	go func() {
		errc <- tun.establishConnection(m, tunnel)
	}()

	waitTunnelClient(t, tun, 10*time.Second)
	banner := waitTunnelBanner(t, tun, tunnel, 30*time.Second)

	closeTunnelClient(t, tun)

	select {
	case <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("establishConnection did not return after the connection was closed")
	}

	if !strings.HasPrefix(banner, "SSH-2.0-") {
		t.Fatalf("the recorded banner is %q, want what the server sent on the handshake", banner)
	}
}

// TestEstablishConnectionMeasuresTheForwardedPort runs the two readings against
// a port that answers and one that does not. The tunnel is connected either
// way, which is the whole point: the reading is what tells a tunnel that can be
// used from one that cannot.
func TestEstablishConnectionMeasuresTheForwardedPort(t *testing.T) {
	answering, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer func() {
		_ = answering.Close()
	}()

	go func() {
		for {
			conn, err := answering.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	_, answeringPort, err := net.SplitHostPort(answering.Addr().String())
	if err != nil {
		t.Fatalf("failed to read the port that answers: %v", err)
	}

	_, silentPort, err := net.SplitHostPort(closedPort(t))
	if err != nil {
		t.Fatalf("failed to read the port that does not answer: %v", err)
	}

	tests := []struct {
		name string
		port string
		want string
	}{
		{name: "the forwarded port answers", port: answeringPort, want: forwardReachable},
		{name: "the forwarded port does not", port: silentPort, want: forwardUnreachable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port, err := strconv.ParseUint(tt.port, 10, 32)
			if err != nil {
				t.Fatalf("failed to read the port: %v", err)
			}

			m := newSSHTestManager(t, 1)
			serverAddr, _ := startForwardingSSHServerConfirming(t, uint32(port))
			// On the wildcard pair, because that is the scope a silence is a
			// reading of. Ports asked for on the loopback addresses are on the
			// Host and nothing here can dial them, so a probe that gets
			// nothing back from one of those measured nothing.
			tun, tunnel := newScopedSSHTestTunnel(t, serverAddr, "0.0.0.0:0", "[::]:0")

			errc := make(chan error, 1)
			go func() {
				errc <- tun.establishConnection(m, tunnel)
			}()

			waitTunnelClient(t, tun, 10*time.Second)
			got := waitTunnelReach(t, tun, tunnel, 30*time.Second)

			closeTunnelClient(t, tun)

			select {
			case <-errc:
			case <-time.After(5 * time.Second):
				t.Fatal("establishConnection did not return after the connection was closed")
			}

			if got != tt.want {
				t.Fatalf("forward reach = %q, want %q, probed at the port the server confirmed", got, tt.want)
			}
		})
	}
}

// TestRecordForwardReachDropsAReadingOfAnOlderConnection pins what keeps a slow
// probe from writing over a tunnel that reconnected under it. The probe waits
// out its timeout, and the connection it measured may be gone by then, with a
// probe of its own running for the one that replaced it.
func TestRecordForwardReachDropsAReadingOfAnOlderConnection(t *testing.T) {
	m := newSSHTestManager(t, 1)
	serverAddr, _ := startForwardingSSHServer(t)
	tun, tunnel := newSSHTestTunnel(t, serverAddr)

	current, _, cleanup := dialTestSSHClient(t, serverAddr)
	defer cleanup()

	tun.clientMu.Lock()
	tun.client = current
	tun.clientMu.Unlock()

	tunnel.ForwardReach = forwardReachUnknown

	// A client that is not the one the tunnel holds, which is what an older
	// connection is by the time its probe answers.
	older, _, olderCleanup := dialTestSSHClient(t, serverAddr)
	defer olderCleanup()

	tun.recordForwardReach(m, tunnel, older, forwardProbe{address: closedPort(t), silence: forwardUnreachable})

	if _, reach := readTunnelReach(tun, tunnel); reach != forwardReachUnknown {
		t.Fatalf("forward reach = %q, want %q, the reading of a connection that is gone was written "+
			"to the row of the one that replaced it", reach, forwardReachUnknown)
	}

	tun.recordForwardReach(m, tunnel, current, forwardProbe{address: closedPort(t), silence: forwardUnreachable})

	if _, reach := readTunnelReach(tun, tunnel); reach != forwardUnreachable {
		t.Fatalf("forward reach = %q, want %q, the reading of the current connection was dropped",
			reach, forwardUnreachable)
	}
}

// TestADeniedForwardIsNamed holds listenErrorKind to what the SSH library
// actually says.
//
// The library raises the refusal with errors.New and exports nothing to compare
// against, so the only handle on it is the sentence, and a sentence is a thing a
// new version of the library can reword without anything failing to compile.
// What is asked here is not whether the constant matches a string written out
// beside it, which would prove nothing, but whether a server that refuses the
// request produces an error this names: newLoopbackSSHClient answers global
// requests with ssh.DiscardRequests, which replies to every one of them with a
// failure, and that is exactly the refusal.
func TestADeniedForwardIsNamed(t *testing.T) {
	client, done := newLoopbackSSHClient(t)
	defer done()

	listener, err := client.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		_ = listener.Close()
		t.Fatal("the server granted a forward it was meant to refuse")
	}

	if got := listenErrorKind(err); got != errorKindForwardDenied {
		t.Errorf("listenErrorKind(%q) = %q, want %q", err.Error(), got, errorKindForwardDenied)
	}
}

// TestAFailureThatIsNotARefusalIsLeftUnnamed keeps the name off everything
// else. A write that failed on the way out and a server that said no are not
// the same thing to put in front of an operator, and naming both would send the
// screen to advise about settings that had nothing to do with it.
func TestAFailureThatIsNotARefusalIsLeftUnnamed(t *testing.T) {
	for _, err := range []error{
		nil,
		errors.New("ssh: unexpected packet in response to channel open: <nil>"),
		errors.New("use of closed network connection"),
		errors.New("EOF"),
	} {
		if got := listenErrorKind(err); got != "" {
			t.Errorf("listenErrorKind(%v) = %q, want it left unnamed", err, got)
		}
	}
}

// startScopedForwardSSHServer speaks SSH and answers every tcpip-forward
// request by the rule the caller hands in, recording the address each request
// named.
//
// It is what a test about a bind scope needs and the server above cannot give.
// A scope is two addresses, so what has to be observable is which addresses
// were asked for and what becomes of the tunnel when the server opens only one
// of them, and a server that grants everything shows neither.
func startScopedForwardSSHServer(t *testing.T, grant func(addr string) bool) (string, func() []string) {
	t.Helper()

	addr, asked, _ := startScopedForwardSSHServerNotingEnds(t, grant)

	return addr, asked
}

// startScopedForwardSSHServerNotingEnds is startScopedForwardSSHServer that
// also says when a client connection has ended. The server reads the requests
// of a connection until the client goes away, so the end of that loop is the
// moment the server sees the connection closed, and a test that expects the
// client to close its end can wait for it there.
func startScopedForwardSSHServerNotingEnds(t *testing.T, grant func(addr string) bool) (string, func() []string, <-chan struct{}) {
	t.Helper()

	ended := make(chan struct{}, 16)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate host key: %v", err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("failed to create signer: %v", err)
	}

	config := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	config.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	var mu sync.Mutex
	var asked []string
	var conns []net.Conn

	t.Cleanup(func() {
		_ = ln.Close()

		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()

			go func(conn net.Conn) {
				defer func() {
					_ = conn.Close()
				}()

				sshConn, chans, reqs, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer func() {
					_ = sshConn.Close()
				}()

				go func() {
					for newChan := range chans {
						_ = newChan.Reject(ssh.Prohibited, "not supported")
					}
				}()

				for req := range reqs {
					if !req.WantReply {
						continue
					}
					if req.Type != "tcpip-forward" {
						_ = req.Reply(false, nil)
						continue
					}

					var forward struct {
						Addr string
						Port uint32
					}
					if err := ssh.Unmarshal(req.Payload, &forward); err != nil {
						_ = req.Reply(false, nil)
						continue
					}

					mu.Lock()
					asked = append(asked, net.JoinHostPort(forward.Addr, strconv.Itoa(int(forward.Port))))
					mu.Unlock()

					if !grant(forward.Addr) {
						_ = req.Reply(false, nil)
						continue
					}

					_ = req.Reply(true, ssh.Marshal(struct{ Port uint32 }{Port: forward.Port}))
				}

				select {
				case ended <- struct{}{}:
				default:
				}
			}(conn)
		}
	}()

	return ln.Addr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()

		return append([]string(nil), asked...)
	}, ended
}

// newScopedSSHTestTunnel is newSSHTestTunnel with the pair of local addresses
// chosen by the caller, so a test can hand in the pair a bind scope names.
func newScopedSSHTestTunnel(t *testing.T, serverAddr, localV4, localV6 string) (*SSHTunnel, *models.Tunnel) {
	t.Helper()

	sshConfig := &ssh.ClientConfig{
		User:            "tester",
		Auth:            []ssh.AuthMethod{ssh.Password("any")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	}

	hostID := uint(1)
	spID := uint(1)

	tun, err := NewSSHTunnel(&hostID, &spID, localV4, localV6, serverAddr, "127.0.0.1:1",
		sshConfig, zap.NewNop())
	if err != nil {
		t.Fatalf("failed to create tunnel: %v", err)
	}

	return tun, &models.Tunnel{HostID: hostID, SPID: spID, Status: "starting"}
}

// readTunnelOpenReach reads the row under the lock the connection writes it
// under, because the probe of the same connection writes beside it.
func readTunnelOpenReach(tun *SSHTunnel, tunnel *models.Tunnel) (openReach, local, status string) {
	tun.tunnelMu.Lock()
	defer tun.tunnelMu.Unlock()

	return tunnel.OpenReach, tunnel.Local, tunnel.Status
}

// runScopedConnection connects, lets the connection settle and brings it down
// again, returning once establishConnection has.
func runScopedConnection(t *testing.T, m *Manager, tun *SSHTunnel, tunnel *models.Tunnel) {
	t.Helper()

	errc := make(chan error, 1)
	go func() {
		errc <- tun.establishConnection(m, tunnel)
	}()

	waitTunnelClient(t, tun, 10*time.Second)
	closeTunnelClient(t, tun)

	select {
	case <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("establishConnection did not return after the connection was closed")
	}
}

// TestEstablishConnectionOpensBothAddressesOfTheScope holds the tunnel to the
// pair. A scope names one address per family and neither stands in for the
// other, so a connection that asks for one of them gives whoever chose the
// scope half of what they chose, silently.
func TestEstablishConnectionOpensBothAddressesOfTheScope(t *testing.T) {
	cases := []struct {
		name   string
		scope  string
		wantV4 string
		wantV6 string
	}{
		{"loopback", models.BindScopeLoopback, "127.0.0.1:18201", "[::1]:18201"},
		{"wildcard", models.BindScopeWildcard, "0.0.0.0:18201", "[::]:18201"},
		{"empty reads as the wildcard", "", "0.0.0.0:18201", "[::]:18201"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newSSHTestManager(t, 1)
			serverAddr, requested := startScopedForwardSSHServer(t, func(string) bool { return true })

			host := &models.Host{Address: "127.0.0.1", Port: 22}
			sp := &models.ServicePort{ServiceAddress: "127.0.0.1", ServicePort: 1, LocalPort: 18201}
			localV4, localV6, _, _ := tunnelAddresses(host, sp, tc.scope)

			tun, tunnel := newScopedSSHTestTunnel(t, serverAddr, localV4, localV6)
			runScopedConnection(t, m, tun, tunnel)

			asked := requested()
			if len(asked) != 2 {
				t.Fatalf("the tunnel asked for %d forwards (%v), want the two addresses of the scope",
					len(asked), asked)
			}

			for _, want := range []string{tc.wantV4, tc.wantV6} {
				found := false
				for _, got := range asked {
					if got == want {
						found = true
					}
				}
				if !found {
					t.Errorf("the tunnel asked for %v, which does not include %q", asked, want)
				}
			}

			openReach, _, _ := readTunnelOpenReach(tun, tunnel)
			if openReach != openReachBoth {
				t.Errorf("open reach = %q, want %q, the server opened both", openReach, openReachBoth)
			}
		})
	}
}

// TestEstablishConnectionReportsTheHalfTheServerRefused pins what a connection
// leaves on the row when one of the two requests was refused. The tunnel is
// connected, because what opened carries traffic, and the row says which half
// of the scope it is: a screen has nothing else to tell it from a tunnel that
// got everything it asked for.
func TestEstablishConnectionReportsTheHalfTheServerRefused(t *testing.T) {
	cases := []struct {
		name      string
		refuse    string
		wantReach string
		wantLocal string
	}{
		{"the IPv6 address is refused", "::", openReachV4, "0.0.0.0:18202"},
		{"the IPv4 address is refused", "0.0.0.0", openReachV6, "[::]:18202"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newSSHTestManager(t, 1)
			serverAddr, _ := startScopedForwardSSHServer(t, func(addr string) bool {
				return addr != tc.refuse
			})

			tun, tunnel := newScopedSSHTestTunnel(t, serverAddr, "0.0.0.0:18202", "[::]:18202")
			runScopedConnection(t, m, tun, tunnel)

			openReach, local, _ := readTunnelOpenReach(tun, tunnel)
			if openReach != tc.wantReach {
				t.Errorf("open reach = %q, want %q", openReach, tc.wantReach)
			}
			if local != tc.wantLocal {
				t.Errorf("the row names %q to connect to, want %q, the address that opened",
					local, tc.wantLocal)
			}
		})
	}
}

// TestEstablishConnectionFailsWhenNeitherAddressOpens keeps the half-open
// reading off a tunnel that forwards nothing. Nothing is open, so there is no
// reach to report, and what an operator has to be shown is the refusal.
func TestEstablishConnectionFailsWhenNeitherAddressOpens(t *testing.T) {
	m := newSSHTestManager(t, 1)
	serverAddr, requested := startScopedForwardSSHServer(t, func(string) bool { return false })

	tun, tunnel := newScopedSSHTestTunnel(t, serverAddr, "0.0.0.0:18203", "[::]:18203")
	tunnel.OpenReach = openReachBoth

	err := tun.establishConnection(m, tunnel)
	if err == nil {
		t.Fatal("establishConnection returned no error although neither address opened")
	}

	if asked := requested(); len(asked) != 2 {
		t.Errorf("the tunnel asked for %v, want both addresses tried before giving up", asked)
	}

	openReach, _, status := readTunnelOpenReach(tun, tunnel)
	if openReach != "" {
		t.Errorf("open reach = %q, want it emptied: nothing of the scope is open", openReach)
	}
	if status != "error" {
		t.Errorf("status = %q, want %q", status, "error")
	}
	if tunnel.ErrorKind != errorKindForwardDenied {
		t.Errorf("error kind = %q, want %q", tunnel.ErrorKind, errorKindForwardDenied)
	}

	for _, want := range []string{"0.0.0.0:18203", "[::]:18203"} {
		if !strings.Contains(tunnel.LastError, want) {
			t.Errorf("the last error %q does not name %q, so it names one refusal and not both",
				tunnel.LastError, want)
		}
	}
}

// A connection whose forwards were all refused is handed to nobody, so the
// failure has to close it. Left open it would stay with the SSH server for as
// long as this process runs, one more for every retry of the tunnel.
func TestEstablishConnectionClosesTheClientWhenNeitherAddressOpens(t *testing.T) {
	m := newSSHTestManager(t, 1)
	serverAddr, _, ended := startScopedForwardSSHServerNotingEnds(t, func(string) bool { return false })

	tun, tunnel := newScopedSSHTestTunnel(t, serverAddr, "0.0.0.0:18204", "[::]:18204")

	if err := tun.establishConnection(m, tunnel); err == nil {
		t.Fatal("establishConnection returned no error although neither address opened")
	}

	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the SSH server still holds the connection after the forwards were refused")
	}

	tun.clientMu.Lock()
	client := tun.client
	tun.clientMu.Unlock()
	if client != nil {
		t.Error("the tunnel kept a client although the connection failed")
	}
}

// A server that accepts the TCP connection and never sends its banner must not
// hold the dial past the timeout of the configuration, or the tunnel it belongs
// to stops trying to connect.
func TestDialSSHClientGivesUpOnASilentServer(t *testing.T) {
	addr := startSilentListener(t)
	config := &ssh.ClientConfig{
		User:            "tester",
		Auth:            []ssh.AuthMethod{ssh.Password("secret")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         300 * time.Millisecond,
	}

	type result struct {
		client *ssh.Client
		err    error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		client, _, err := dialSSHClient(addr, config)
		done <- result{client: client, err: err}
	}()

	select {
	case r := <-done:
		if r.err == nil {
			_ = r.client.Close()
			t.Fatal("dialSSHClient succeeded against a server that sent nothing")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("dialSSHClient returned after %v, want about %v", elapsed, config.Timeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dialSSHClient is still waiting on a server that sends nothing")
	}
}

// The deadline that bounds the handshake has to be lifted once it is done, or
// the connection dies as soon as the timeout passes.
func TestDialSSHClientKeepsTheConnectionPastTheTimeout(t *testing.T) {
	addr, _ := startForwardingSSHServer(t)
	config := &ssh.ClientConfig{
		User:            "tester",
		Auth:            []ssh.AuthMethod{ssh.Password("secret")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         300 * time.Millisecond,
	}

	client, _, err := dialSSHClient(addr, config)
	if err != nil {
		t.Fatalf("dialSSHClient failed: %v", err)
	}
	defer func() {
		_ = client.Close()
	}()

	time.Sleep(3 * config.Timeout)

	replied := make(chan error, 1)
	go func() {
		_, _, err := client.SendRequest("keepalive@tunnel-manager", true, nil)
		replied <- err
	}()

	select {
	case err := <-replied:
		if err != nil {
			t.Fatalf("request after the timeout failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request after the timeout got no reply")
	}
}

// TestDialAddressWritesAnIPLiteralTheWayTCPAddrDoes pins the text of the Server
// and Remote addresses a log line carries. They used to be resolved up front
// and printed as a net.TCPAddr, and an IP literal has to go on reading the same
// now that they are dialled as they are. A name is left for the dialer.
func TestDialAddressWritesAnIPLiteralTheWayTCPAddrDoes(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"192.0.2.10:22", "192.0.2.10:22"},
		{"[2001:DB8:0::1]:22", "[2001:db8::1]:22"},
		{"[::ffff:192.0.2.10]:22", "192.0.2.10:22"},
		{"localhost:22", "localhost:22"},
		{"ssh.example.com:2222", "ssh.example.com:2222"},
	}

	for _, tt := range tests {
		if got := dialAddress(tt.in); got != tt.want {
			t.Errorf("dialAddress(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestAHostNameIsResolvedWhereTheTunnelConnects connects a tunnel whose server
// is a name rather than an address. The name is looked up by the dial, and the
// probe of the forwarded port takes its address from the connection that was
// made, so a name reaches the same connected state an address does.
func TestAHostNameIsResolvedWhereTheTunnelConnects(t *testing.T) {
	m := newSSHTestManager(t, 1)
	serverAddr, _ := startForwardingSSHServer(t)

	_, port, err := net.SplitHostPort(serverAddr)
	if err != nil {
		t.Fatalf("failed to read the port: %v", err)
	}

	tun, tunnel := newSSHTestTunnel(t, net.JoinHostPort("localhost", port))
	if tun.Server != net.JoinHostPort("localhost", port) {
		t.Fatalf("the tunnel holds the server as %q, want the name as it was given", tun.Server)
	}

	errc := make(chan error, 1)
	go func() {
		errc <- tun.establishConnection(m, tunnel)
	}()

	waitTunnelClient(t, tun, 10*time.Second)

	tun.tunnelMu.Lock()
	status := tunnel.Status
	tun.tunnelMu.Unlock()

	if status != "connected" {
		t.Fatalf("tunnel status = %q, want %q", status, "connected")
	}

	closeTunnelClientOnly(t, tun)

	select {
	case <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("establishConnection did not return after the connection was closed")
	}
}

// TestAHostNameThatDoesNotResolveLeavesATunnelRowThatRetries starts a tunnel
// whose Host is a name no resolver answers. The name is looked up where the
// tunnel connects, so StartTunnel creates the row and the failure lands on it
// the way a refused connection does, with the error and the retries, rather
// than StartTunnel failing before there is a row to show it on.
func TestAHostNameThatDoesNotResolveLeavesATunnelRowThatRetries(t *testing.T) {
	sqlDB, err := sql.Open("sqlite", t.TempDir()+"/tunnel-manager.db")
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(1)

	db, err := gorm.Open(sqlite.Dialector{Conn: sqlDB}, &gorm.Config{
		Logger:                 logger.Discard,
		SkipDefaultTransaction: true,
	})
	if err != nil {
		t.Fatalf("failed to open gorm: %v", err)
	}
	if err := db.AutoMigrate(&models.Tunnel{}); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	m, err := NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	const name = "tunnel-manager-test.invalid"

	host := models.Host{ID: 1, Address: name, Port: 22, User: "user", Password: "pass", Enabled: true}
	sp := models.ServicePort{ID: 2, ServiceAddress: "127.0.0.1", ServicePort: 3306, LocalPort: 13306}

	err = m.StartTunnel(&host, &sp, models.BindScopeWildcard)
	if err != nil {
		t.Fatalf("StartTunnel returned an error for a name that does not resolve: %v", err)
	}
	t.Cleanup(func() {
		_ = m.StopTunnel(host.ID, sp.ID)
	})

	var row models.Tunnel
	found := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		row = models.Tunnel{}
		err := db.Where("host_id = ? AND sp_id = ?", host.ID, sp.ID).First(&row).Error
		found = err == nil
		if found && row.RetryCount >= 1 && strings.Contains(row.LastError, name) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !found {
		t.Fatal("no tunnel row was created for a Host whose name does not resolve")
	}
	if !strings.Contains(row.LastError, name) {
		t.Fatalf("the tunnel row says last error %q, want the failed lookup of %q", row.LastError, name)
	}
	if row.RetryCount < 1 {
		t.Fatalf("RetryCount = %d, want the failed lookup retried", row.RetryCount)
	}
	if row.Status != "error" && row.Status != "reconnecting" {
		t.Fatalf("tunnel status = %q, want error or reconnecting", row.Status)
	}
	if row.Server != net.JoinHostPort(name, "22") {
		t.Fatalf("the tunnel row names the server %q, want %q", row.Server, net.JoinHostPort(name, "22"))
	}
}
