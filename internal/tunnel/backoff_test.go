package tunnel

import (
	"errors"
	"fmt"
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

// retryReasons returns the retry_reason of every line that starts with message,
// in the order they were written, and the empty string for a line with none.
func retryReasons(logs *observer.ObservedLogs, message string) []string {
	var reasons []string

	for _, entry := range logs.All() {
		if !strings.HasPrefix(entry.Message, message) {
			continue
		}

		reason, _ := entry.ContextMap()["retry_reason"].(string)
		reasons = append(reasons, reason)
	}

	return reasons
}

// assertRetryReasons checks that the first n lines that start with message
// carry want as their retry_reason.
func assertRetryReasons(t *testing.T, logs *observer.ObservedLogs, message string, n int, want string) {
	t.Helper()

	for i, got := range retryReasons(logs, message)[:n] {
		if got != want {
			t.Fatalf("retry line %d carries retry_reason %q, want %q", i+1, got, want)
		}
	}
}

// prohibitingRoute is a Host reached through a Host that refuses the channel to
// it as administratively prohibited: the address of the Host at the end, and
// the route to it.
func prohibitingRoute(t *testing.T) (string, []sshHop) {
	t.Helper()

	jump := startJumpTestServer(t)
	jump.prohibit.Store(true)
	target := startJumpTestServer(t)

	return target.addr, []sshHop{{hostID: 2, addr: jump.addr, config: testClientConfig(time.Second)}}
}

// TestATunnelBehindAProhibitingJumpWaitsTheCeiling is a tunnel whose jump Host
// refuses the channel to the Host at the end. That refusal is put right in the
// settings of the jump Host and nowhere else, so every attempt waits the
// ceiling from the first, rather than the interval doubling towards it.
func TestATunnelBehindAProhibitingJumpWaitsTheCeiling(t *testing.T) {
	m := newSSHTestManager(t, 1)
	m.SetReconnectMaxInterval(3)

	server, jumps := prohibitingRoute(t)
	tun, tunnel := newSSHTestTunnel(t, server)
	tun.jumps = jumps

	core, logs := observer.New(zapcore.InfoLevel)
	tun.logger = zap.New(core)

	returned := make(chan struct{})
	go func() {
		tun.Start(m, tunnel)
		close(returned)
	}()

	got := waitRetryLines(t, logs, "connection failed, retrying in", 2)

	_ = tun.Stop(m)
	<-returned

	if want := []int64{3, 3}; !sameInts(got, want) {
		t.Fatalf("the tunnel waited %v seconds, want %v", got, want)
	}
	assertRetryReasons(t, logs, "connection failed, retrying in", 2, retryReasonJumpProhibited)
}

func TestALocalForwardBehindAProhibitingJumpWaitsTheCeiling(t *testing.T) {
	server, jumps := prohibitingRoute(t)
	host, port := splitTestAddr(t, server)

	h := &models.Host{ID: 1, Address: host, Port: port, User: "tester"}
	lf := &models.LocalForward{
		Number: 7, HostID: 1, BindScope: models.BindScopeLoopback, LocalPort: freeDualStackPort(t),
		TargetAddress: "127.0.0.1", TargetPort: 1, Enabled: true,
	}

	core, logs := observer.New(zapcore.InfoLevel)

	f, err := newLocalTunnel(lf, h, testClientConfig(time.Second), time.Second, zap.New(core))
	if err != nil {
		t.Fatalf("failed to build the forward: %v", err)
	}
	f.jumps = jumps
	f.maxInterval = 3 * time.Second

	returned := make(chan struct{})
	go func() {
		f.Start()
		close(returned)
	}()

	got := waitRetryLines(t, logs, "local forward connection failed, retrying in", 2)

	f.Stop()
	<-returned

	if want := []int64{3, 3}; !sameInts(got, want) {
		t.Fatalf("the forward waited %v seconds, want %v", got, want)
	}
	assertRetryReasons(t, logs, "local forward connection failed, retrying in", 2, retryReasonJumpProhibited)
}

func TestASocksProxyBehindAProhibitingJumpWaitsTheCeiling(t *testing.T) {
	server, jumps := prohibitingRoute(t)
	host := socksHostOn(t, server)

	core, logs := observer.New(zapcore.InfoLevel)

	p, err := newSocksTunnel(host, testClientConfig(time.Second), time.Second, zap.New(core))
	if err != nil {
		t.Fatalf("failed to build the proxy: %v", err)
	}
	p.jumps = jumps
	p.maxInterval = 3 * time.Second

	returned := make(chan struct{})
	go func() {
		p.Start()
		close(returned)
	}()

	got := waitRetryLines(t, logs, "SOCKS5 proxy connection failed, retrying in", 2)

	p.Stop()
	<-returned

	if want := []int64{3, 3}; !sameInts(got, want) {
		t.Fatalf("the proxy waited %v seconds, want %v", got, want)
	}
	assertRetryReasons(t, logs, "SOCKS5 proxy connection failed, retrying in", 2, retryReasonJumpProhibited)
}

// TestADeniedRemotePortIsRetriedAtTheInterval is a tunnel whose Host refuses to
// open the forwarded port on the first five connections, the way sshd does
// for a while after a session went away without closing, since it still holds
// the port for it. The first refusals in a row are tried again at the interval,
// and those past them go back to doubling. The tunnel connects once the Host
// lets the port go.
func TestADeniedRemotePortIsRetriedAtTheInterval(t *testing.T) {
	m := newSSHTestManager(t, 1)
	m.SetReconnectMaxInterval(8)

	server := startJumpTestServer(t)
	server.denyForwardConns.Store(5)

	tun, tunnel := newSSHTestTunnel(t, server.addr)
	tun.Config = testClientConfig(time.Second)

	core, logs := observer.New(zapcore.InfoLevel)
	tun.logger = zap.New(core)

	returned := make(chan struct{})
	go func() {
		tun.Start(m, tunnel)
		close(returned)
	}()
	defer func() {
		_ = tun.Stop(m)
		<-returned
	}()

	got := waitRetryLines(t, logs, "connection failed, retrying in", 5)
	if want := []int64{1, 1, 1, 1, 2}; !sameInts(got, want) {
		t.Fatalf("the tunnel waited %v seconds, want %v", got, want)
	}
	assertRetryReasons(t, logs, "connection failed, retrying in", 5, retryReasonForwardDenied)

	waitFor(t, 20*time.Second, "the tunnel to connect", func() bool {
		return server.handshakes.Load() >= 6 && server.active.Load() >= 1
	})
}

// errForwardDenied is what establishConnection fails with when the SSH server
// refused both addresses of the forwarded port.
var errForwardDenied = errors.New("failed to start remote listener: the SSH server opened neither address " +
	"of the bind scope (127.0.0.1:80: " + listenDeniedMessage + "; [::1]:80: " + listenDeniedMessage + ")")

// errJumpProhibited is what a connection fails with when the Host it passes
// through is prohibited from opening the channel to the next.
var errJumpProhibited = fmt.Errorf("failed to establish SSH connection: %w", &JumpError{Seq: 1, HostID: 2,
	Address: "jump.example.com:22", Err: &ssh.OpenChannelError{Reason: ssh.Prohibited, Message: "prohibited"}})

func TestTheWaitAfterAFailureFollowsWhatFailed(t *testing.T) {
	s := time.Second
	failure := errors.New("dial tcp: connection refused")

	cases := []struct {
		name     string
		interval time.Duration
		max      time.Duration
		errs     []error
		want     []time.Duration
	}{
		{"a prohibited jump waits the ceiling from the first", 5 * s, 60 * s,
			[]error{errJumpProhibited, errJumpProhibited}, []time.Duration{60 * s, 60 * s}},
		{"a prohibited jump under a ceiling below the interval waits the interval", 5 * s, 3 * s,
			[]error{errJumpProhibited}, []time.Duration{5 * s}},
		{"a denied port is tried at the interval three times, then doubles", 5 * s, 60 * s,
			[]error{errForwardDenied, errForwardDenied, errForwardDenied, errForwardDenied, errForwardDenied},
			[]time.Duration{5 * s, 5 * s, 5 * s, 5 * s, 10 * s}},
		{"a denied port after failures waits the interval, then doubles on from them", 5 * s, 60 * s,
			[]error{failure, failure, errForwardDenied, errForwardDenied, errForwardDenied, errForwardDenied},
			[]time.Duration{5 * s, 10 * s, 5 * s, 5 * s, 5 * s, 20 * s}},
		{"another failure ends a run of denials", 5 * s, 60 * s,
			[]error{errForwardDenied, errForwardDenied, errForwardDenied, failure, errForwardDenied},
			[]time.Duration{5 * s, 5 * s, 5 * s, 5 * s, 5 * s}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newReconnectBackoff(tc.interval, tc.max)

			got := make([]time.Duration, 0, len(tc.errs))
			for _, err := range tc.errs {
				wait, _ := b.after(err)
				got = append(got, wait)
			}

			if !sameWaits(got, tc.want) {
				t.Fatalf("waits = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAConnectionEndsARunOfDenials(t *testing.T) {
	b := newReconnectBackoff(5*time.Second, 60*time.Second)

	for i := 0; i < 3; i++ {
		_, _ = b.after(errForwardDenied)
	}
	b.reset()

	got := make([]time.Duration, 0, 3)
	for i := 0; i < 3; i++ {
		wait, reason := b.after(errForwardDenied)
		if reason != retryReasonForwardDenied {
			t.Fatalf("a denied port is given the reason %q, want %q", reason, retryReasonForwardDenied)
		}
		got = append(got, wait)
	}

	if want := []time.Duration{5 * time.Second, 5 * time.Second, 5 * time.Second}; !sameWaits(got, want) {
		t.Fatalf("waits after a connection = %v, want %v", got, want)
	}
}
