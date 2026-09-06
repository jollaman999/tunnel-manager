package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"errors"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
	"gorm.io/driver/mysql"
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
	return &sql.Row{}
}

func newSSHTestManager(t *testing.T, monitoringIntervalSec int) *Manager {
	t.Helper()

	db, err := gorm.Open(mysql.New(mysql.Config{
		Conn:                      sshTestConnPool{},
		SkipInitializeWithVersion: true,
	}), &gorm.Config{
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

	tun, err := NewSSHTunnel(&hostID, &spID, "127.0.0.1:0", serverAddr, "127.0.0.1:1",
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

	return ln.Addr().String()
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
}

func TestReconnectDoesNotEstablishConnection(t *testing.T) {
	m := newSSHTestManager(t, 1)
	tun, tunnel := newSSHTestTunnel(t, startSilentListener(t))

	client, closeClient := newLoopbackSSHClient(t)
	defer closeClient()

	tun.clientMu.Lock()
	tun.client = client
	tun.clientMu.Unlock()

	returned := make(chan struct{})
	go func() {
		tun.reconnect(m, tunnel)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("reconnect did not return, it is still establishing a connection itself")
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
						_ = req.Reply(true, ssh.Marshal(struct{ Port uint32 }{Port: 12345}))
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
	closeTunnelClient(t, tun)

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("establishConnection returned nil on a closed connection, so Start retries with no interval")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("establishConnection did not return after the connection was closed")
	}

	if tunnel.Status != "connected" {
		t.Fatalf("tunnel status = %q, want %q, a closed connection is not a failure", tunnel.Status, "connected")
	}
	if tunnel.RetryCount != 0 {
		t.Fatalf("RetryCount = %d, want 0, a closed connection is not a failure", tunnel.RetryCount)
	}
	if tunnel.LastError != "" {
		t.Fatalf("LastError = %q, want empty, a closed connection is not a failure", tunnel.LastError)
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
