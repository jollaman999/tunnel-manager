package tunnel

import (
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/crypto/ssh"
)

// waitsOf returns the waits of n failures in a row.
func waitsOf(b *reconnectBackoff, n int) []time.Duration {
	waits := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		waits = append(waits, b.failed())
	}

	return waits
}

func sameWaits(got, want []time.Duration) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}

	return true
}

func TestReconnectBackoffDoublesUpToTheCeiling(t *testing.T) {
	s := time.Second

	cases := []struct {
		name     string
		interval time.Duration
		max      time.Duration
		want     []time.Duration
	}{
		{"the defaults", 5 * s, 60 * s, []time.Duration{5 * s, 10 * s, 20 * s, 40 * s, 60 * s, 60 * s, 60 * s}},
		{"a ceiling that is not a doubling of the interval", 5 * s, 30 * s,
			[]time.Duration{5 * s, 10 * s, 20 * s, 30 * s, 30 * s}},
		// A ceiling below the interval does not retry sooner than the
		// interval. It is reached at once, and every wait is the interval.
		{"a ceiling below the interval", 5 * s, 3 * s, []time.Duration{5 * s, 5 * s, 5 * s}},
		{"a ceiling equal to the interval", 5 * s, 5 * s, []time.Duration{5 * s, 5 * s, 5 * s}},
		// A manager whose ceiling was never set waits the interval, which is
		// what every connect loop did before the ceiling existed.
		{"no ceiling", 5 * s, 0, []time.Duration{5 * s, 5 * s, 5 * s}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newReconnectBackoff(tc.interval, tc.max)

			got := waitsOf(&b, len(tc.want))
			if !sameWaits(got, tc.want) {
				t.Fatalf("waits = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReconnectBackoffStartsOverAfterAConnection(t *testing.T) {
	b := newReconnectBackoff(5*time.Second, 60*time.Second)

	_ = waitsOf(&b, 4)
	b.reset()

	got := waitsOf(&b, 3)
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second}
	if !sameWaits(got, want) {
		t.Fatalf("waits after a connection = %v, want %v", got, want)
	}
}

func TestTheManagerHandsTheCeilingToTheBackoff(t *testing.T) {
	m := newSSHTestManager(t, 2)
	m.SetReconnectMaxInterval(7)

	b := m.reconnectBackoff()

	got := waitsOf(&b, 4)
	want := []time.Duration{2 * time.Second, 4 * time.Second, 7 * time.Second, 7 * time.Second}
	if !sameWaits(got, want) {
		t.Fatalf("waits = %v, want %v", got, want)
	}
}

// retryWaits returns the retry_in_sec of every line that starts with message,
// in the order they were written.
func retryWaits(logs *observer.ObservedLogs, message string) []int64 {
	var waits []int64

	for _, entry := range logs.All() {
		if !strings.HasPrefix(entry.Message, message) {
			continue
		}

		wait, _ := entry.ContextMap()["retry_in_sec"].(int64)
		waits = append(waits, wait)
	}

	return waits
}

// waitRetryLines waits for n lines that start with message and returns their
// waits. It also checks that each line says in its text the wait it carries.
func waitRetryLines(t *testing.T, logs *observer.ObservedLogs, message string, n int) []int64 {
	t.Helper()

	waitFor(t, 20*time.Second, strconv.Itoa(n)+" retry lines", func() bool {
		return len(retryWaits(logs, message)) >= n
	})

	for _, entry := range logs.All() {
		if !strings.HasPrefix(entry.Message, message) {
			continue
		}

		wait, _ := entry.ContextMap()["retry_in_sec"].(int64)
		if !strings.Contains(entry.Message, "retrying in "+strconv.FormatInt(wait, 10)+" seconds") {
			t.Errorf("the line %q carries retry_in_sec %d, which its text does not state", entry.Message, wait)
		}
	}

	return retryWaits(logs, message)[:n]
}

func sameInts(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}

	return true
}

// TestATunnelWaitsLongerAfterEachFailure runs the connect loop of a tunnel
// against a server that closes every connection, and reads the waits off the
// lines it writes.
func TestATunnelWaitsLongerAfterEachFailure(t *testing.T) {
	m := newSSHTestManager(t, 1)
	m.SetReconnectMaxInterval(2)

	tun, tunnel := newSSHTestTunnel(t, startClosingListener(t))

	core, logs := observer.New(zapcore.InfoLevel)
	tun.logger = zap.New(core)

	returned := make(chan struct{})
	go func() {
		tun.Start(m, tunnel)
		close(returned)
	}()

	got := waitRetryLines(t, logs, "connection failed, retrying in", 3)

	_ = tun.Stop(m)
	<-returned

	if want := []int64{1, 2, 2}; !sameInts(got, want) {
		t.Fatalf("the tunnel waited %v seconds, want %v", got, want)
	}
}

// socksHostOn is a Host whose SSH server is at address, with its SOCKS5 proxy
// on the loopback pair.
func socksHostOn(t *testing.T, address string) *models.Host {
	t.Helper()

	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("failed to split %s: %v", address, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("failed to read the port of %s: %v", address, err)
	}

	return &models.Host{
		ID: 1, Address: host, Port: port, User: "tester", Password: "pass", // hook:allow
		SocksEnabled: true, SocksPort: freeDualStackPort(t), SocksBindScope: models.BindScopeLoopback,
	}
}

func TestASocksProxyWaitsLongerAfterEachFailure(t *testing.T) {
	host := socksHostOn(t, startClosingListener(t))

	config := &ssh.ClientConfig{
		User:            "tester",
		Auth:            []ssh.AuthMethod{ssh.Password("pass")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Second,
	}

	core, logs := observer.New(zapcore.InfoLevel)

	p, err := newSocksTunnel(host, config, time.Second, zap.New(core))
	if err != nil {
		t.Fatalf("failed to build the proxy: %v", err)
	}
	p.maxInterval = 2 * time.Second

	returned := make(chan struct{})
	go func() {
		p.Start()
		close(returned)
	}()

	got := waitRetryLines(t, logs, "SOCKS5 proxy connection failed, retrying in", 3)

	p.Stop()
	<-returned

	if want := []int64{1, 2, 2}; !sameInts(got, want) {
		t.Fatalf("the proxy waited %v seconds, want %v", got, want)
	}
}

// gatedProxy passes TCP connections on to backend while it is open, and closes
// them on accept while it is shut, which is an SSH server that is down and
// comes back without its address changing.
type gatedProxy struct {
	addr string
	open atomic.Bool
}

func startGatedProxy(t *testing.T, backend string) *gatedProxy {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	g := &gatedProxy{addr: ln.Addr().String()}

	var conns sync.WaitGroup

	t.Cleanup(func() {
		_ = ln.Close()
		conns.Wait()
	})

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			if !g.open.Load() {
				_ = conn.Close()
				continue
			}

			upstream, err := net.Dial("tcp", backend)
			if err != nil {
				_ = conn.Close()
				continue
			}

			conns.Add(1)
			go func() {
				defer conns.Done()

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
				_ = conn.Close()
				_ = upstream.Close()
				<-done
			}()
		}
	}()

	return g
}

// TestALocalForwardStartsOverAfterItConnected fails a local forward twice,
// lets it connect, takes the Host away and fails it again. The first failure
// after the connection waits the interval, not the doubled wait the failures
// before it had reached.
func TestALocalForwardStartsOverAfterItConnected(t *testing.T) {
	server := startDirectSSHServer(t)
	gate := startGatedProxy(t, server.addr)

	host, portText, err := net.SplitHostPort(gate.addr)
	if err != nil {
		t.Fatalf("failed to split %s: %v", gate.addr, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("failed to read the port of %s: %v", gate.addr, err)
	}

	targetIP, targetPort := startEchoService(t, "echo:")

	h := &models.Host{ID: 1, Address: host, Port: port, User: "tester"}
	lf := &models.LocalForward{
		Number: 7, HostID: 1, BindScope: models.BindScopeLoopback, LocalPort: freeDualStackPort(t),
		TargetAddress: targetIP, TargetPort: targetPort, Enabled: true,
	}

	config := &ssh.ClientConfig{
		User:            "tester",
		Auth:            []ssh.AuthMethod{ssh.Password("pass")},
		HostKeyCallback: ssh.FixedHostKey(server.key),
		Timeout:         time.Second,
	}

	core, logs := observer.New(zapcore.InfoLevel)

	f, err := newLocalTunnel(lf, h, config, time.Second, zap.New(core))
	if err != nil {
		t.Fatalf("failed to build the forward: %v", err)
	}
	f.maxInterval = 4 * time.Second

	returned := make(chan struct{})
	go func() {
		f.Start()
		close(returned)
	}()
	defer func() {
		f.Stop()
		<-returned
	}()

	const message = "local forward connection failed, retrying in"

	before := waitRetryLines(t, logs, message, 2)
	if want := []int64{1, 2}; !sameInts(before, want) {
		t.Fatalf("the forward waited %v seconds before it connected, want %v", before, want)
	}

	gate.open.Store(true)

	waitFor(t, 20*time.Second, "the local forward to connect", func() bool {
		return f.snapshot().Status == localStatusConnected
	})

	gate.open.Store(false)
	server.drop()

	after := waitRetryLines(t, logs, message, 3)
	if after[2] != 1 {
		t.Fatalf("the first failure after the forward connected waited %d seconds, want the interval of 1", after[2])
	}
}
