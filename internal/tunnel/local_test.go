package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
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
	"golang.org/x/crypto/ssh"
	"gorm.io/gorm"
)

// localTestTimeout bounds every wait in these tests. The monitoring interval
// is a second, so a reconnect takes about that long and this leaves room.
const localTestTimeout = 10 * time.Second

// directSSHServer is an SSH server that accepts the password "pass" and opens
// every direct-tcpip channel it is asked for, which is what a Host does for a
// local forward.
type directSSHServer struct {
	addr string
	key  ssh.PublicKey

	mu      sync.Mutex
	dialed  []string
	conns   []net.Conn
	handled atomic.Int64
}

func (s *directSSHServer) targets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.dialed...)
}

// drop closes every SSH connection the server holds, the way a Host that went
// away does.
func (s *directSSHServer) drop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, conn := range s.conns {
		_ = conn.Close()
	}
	s.conns = nil
}

func startDirectSSHServer(t *testing.T) *directSSHServer {
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
		PasswordCallback: func(_ ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if string(password) != "pass" {
				return nil, errors.New("password rejected")
			}
			return nil, nil
		},
	}
	config.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	s := &directSSHServer{addr: ln.Addr().String(), key: signer.PublicKey()}

	t.Cleanup(func() {
		_ = ln.Close()
		s.drop()
	})

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			s.mu.Lock()
			s.conns = append(s.conns, conn)
			s.mu.Unlock()

			go s.serve(conn, config)
		}
	}()

	return s
}

func (s *directSSHServer) serve(conn net.Conn, config *ssh.ServerConfig) {
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

	s.handled.Add(1)

	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "direct-tcpip" {
			_ = newChan.Reject(ssh.UnknownChannelType, "not supported")
			continue
		}

		var target struct {
			DestAddr string
			DestPort uint32
			OrigAddr string
			OrigPort uint32
		}
		err := ssh.Unmarshal(newChan.ExtraData(), &target)
		if err != nil {
			_ = newChan.Reject(ssh.ConnectionFailed, "bad payload")
			continue
		}

		address := net.JoinHostPort(target.DestAddr, strconv.Itoa(int(target.DestPort)))

		s.mu.Lock()
		s.dialed = append(s.dialed, address)
		s.mu.Unlock()

		upstream, err := net.DialTimeout("tcp", address, localTestTimeout)
		if err != nil {
			_ = newChan.Reject(ssh.ConnectionFailed, err.Error())
			continue
		}

		channel, requests, err := newChan.Accept()
		if err != nil {
			_ = upstream.Close()
			continue
		}
		go ssh.DiscardRequests(requests)

		go func() {
			_, _ = io.Copy(channel, upstream)
			_ = channel.CloseWrite()
		}()
		go func() {
			_, _ = io.Copy(upstream, channel)
			_ = upstream.(*net.TCPConn).CloseWrite()
		}()
	}
}

// startEchoService answers every connection with prefix followed by what was
// sent, once the client has closed its writing side.
func startEchoService(t *testing.T, prefix string) (string, int) {
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

			go func(conn net.Conn) {
				defer func() {
					_ = conn.Close()
				}()

				_ = conn.SetDeadline(time.Now().Add(localTestTimeout))

				got, err := io.ReadAll(conn)
				if err != nil {
					return
				}
				_, _ = conn.Write(append([]byte(prefix), got...))
			}(conn)
		}
	}()

	return "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
}

// freeDualStackPort returns a port nothing listens on at the moment, taken from
// the dual-stack wildcard so that it is free in both families.
func freeDualStackPort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("failed to find a free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	return port
}

// newLocalForwardStubDB answers Host and LocalForward reads from what the
// caller holds, read afresh on every query so a change made between two
// passes is what the next one sees, and accepts every write.
func newLocalForwardStubDB(t *testing.T, mu *sync.Mutex, hosts *[]models.Host, forwards *[]models.LocalForward) *gorm.DB {
	t.Helper()

	db := newFailingDB(t)

	err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		mu.Lock()
		defer mu.Unlock()

		switch dest := tx.Statement.Dest.(type) {
		case *[]models.Host:
			*dest = append([]models.Host(nil), *hosts...)
		case *[]models.LocalForward:
			*dest = append([]models.LocalForward(nil), *forwards...)
		}
	})
	if err != nil {
		t.Fatalf("failed to replace the query callback: %v", err)
	}

	for _, replace := range []func() error{
		func() error { return db.Callback().Create().Replace("gorm:create", func(*gorm.DB) {}) },
		func() error { return db.Callback().Update().Replace("gorm:update", func(*gorm.DB) {}) },
		func() error { return db.Callback().Delete().Replace("gorm:delete", func(*gorm.DB) {}) },
	} {
		err = replace()
		if err != nil {
			t.Fatalf("failed to replace a write callback: %v", err)
		}
	}

	return db
}

// localForwardFixture is a manager with one Host that points at a direct-tcpip
// server and one local forward carried by it.
type localForwardFixture struct {
	m        *Manager
	server   *directSSHServer
	mu       sync.Mutex
	hosts    []models.Host
	forwards []models.LocalForward
}

func newLocalForwardFixture(t *testing.T, scope string, localPort int, targetIP string, targetPort int) *localForwardFixture {
	t.Helper()

	server := startDirectSSHServer(t)

	serverHost, serverPort, err := net.SplitHostPort(server.addr)
	if err != nil {
		t.Fatalf("failed to split %s: %v", server.addr, err)
	}
	port, err := strconv.Atoi(serverPort)
	if err != nil {
		t.Fatalf("failed to read the port of %s: %v", server.addr, err)
	}

	f := &localForwardFixture{server: server}
	f.hosts = []models.Host{{
		ID: 1, IP: serverHost, Port: port, User: "tester", Password: "pass", // hook:allow
		HostKey: MarshalHostKey(server.key), Enabled: true,
	}}
	f.forwards = []models.LocalForward{{
		ID: 7, HostID: 1, BindScope: scope, LocalPort: localPort, TargetIP: targetIP, TargetPort: targetPort,
	}}

	m, err := NewManager(newLocalForwardStubDB(t, &f.mu, &f.hosts, &f.forwards), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	t.Cleanup(m.StopAllTunnels)

	f.m = m

	return f
}

func (f *localForwardFixture) change(edit func(hosts []models.Host, forwards *[]models.LocalForward)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	edit(f.hosts, &f.forwards)
}

func (f *localForwardFixture) reconcile(t *testing.T) ReconcileResult {
	t.Helper()

	result, err := f.m.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}

	return result
}

func waitLocalStatus(t *testing.T, m *Manager, id uint, what string, done func(LocalForwardState) bool) LocalForwardState {
	t.Helper()

	var last LocalForwardState
	waitFor(t, localTestTimeout, what, func() bool {
		state, ok := m.LocalForwardStatus(id)
		last = state
		return ok && done(state)
	})

	return last
}

func isConnected(state LocalForwardState) bool {
	return state.Status == localStatusConnected
}

// exchange sends payload to the local port at address and returns what came
// back once the far end closed.
func exchange(t *testing.T, address, payload string) string {
	t.Helper()

	conn, err := net.DialTimeout("tcp", address, localTestTimeout)
	if err != nil {
		t.Fatalf("failed to connect to the local port %s: %v", address, err)
	}
	defer func() {
		_ = conn.Close()
	}()

	_ = conn.SetDeadline(time.Now().Add(localTestTimeout))

	_, err = conn.Write([]byte(payload))
	if err != nil {
		t.Fatalf("failed to write to the local port: %v", err)
	}

	err = conn.(*net.TCPConn).CloseWrite()
	if err != nil {
		t.Fatalf("failed to half-close the connection to the local port: %v", err)
	}

	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("failed to read the answer through the local port: %v", err)
	}

	return string(got)
}

// ipv6LoopbackAvailable reports whether ::1 can be listened on here, which a
// container without IPv6 cannot do.
func ipv6LoopbackAvailable() bool {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		return false
	}
	_ = ln.Close()

	return true
}

// portIsFree reports whether the port can be listened on again on the IPv4
// loopback, which is what a forward that was stopped has to leave behind.
func portIsFree(port int) bool {
	ln, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = ln.Close()

	return true
}

// TestALocalForwardCarriesAConnectionThroughTheHost is the forward end to end:
// a client of the local port reaches the target the Host dials, and the answer
// comes back the same way, for both scopes.
func TestALocalForwardCarriesAConnectionThroughTheHost(t *testing.T) {
	for _, scope := range []string{models.BindScopeLoopback, models.BindScopeWildcard} {
		t.Run(scope, func(t *testing.T) {
			targetIP, targetPort := startEchoService(t, "echo:")
			localPort := freeDualStackPort(t)

			f := newLocalForwardFixture(t, scope, localPort, targetIP, targetPort)

			result := f.reconcile(t)
			if result.Started != 1 || result.Failed != 0 {
				t.Fatalf("the first pass reported started=%d failed=%d, want 1/0", result.Started, result.Failed)
			}

			state := waitLocalStatus(t, f.m, 7, "the local forward to connect", isConnected)
			if state.LastConnectedAt.IsZero() || state.RetryCount != 0 || state.LastError != "" {
				t.Fatalf("a connected forward reports %+v", state)
			}

			local := net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort))
			if got := exchange(t, local, "ping"); got != "echo:ping" {
				t.Fatalf("the answer through the local port was %q, want %q", got, "echo:ping")
			}

			if ipv6LoopbackAvailable() {
				local6 := net.JoinHostPort("::1", strconv.Itoa(localPort))
				if got := exchange(t, local6, "ping6"); got != "echo:ping6" {
					t.Fatalf("the answer through the IPv6 half was %q, want %q", got, "echo:ping6")
				}
			}

			want := net.JoinHostPort(targetIP, strconv.Itoa(targetPort))
			for _, got := range f.server.targets() {
				if got != want {
					t.Fatalf("the Host was asked to dial %q, want %q", got, want)
				}
			}
			if len(f.server.targets()) == 0 {
				t.Fatal("the Host was never asked to dial the target")
			}

			f.m.StopAllTunnels()

			if states := f.m.LocalForwardStatuses(); len(states) != 0 {
				t.Fatalf("local forwards still run after StopAllTunnels: %v", states)
			}
			if !portIsFree(localPort) {
				t.Fatal("the local port is still held after StopAllTunnels")
			}
		})
	}
}

// TestReconcileFollowsTheLocalForwardRows holds the pass to the rows: a Host
// that is disabled takes its forwards down and frees their ports, enabling it
// brings them back, a changed target rebuilds the forward on the new one, and
// a row that is deleted is stopped.
func TestReconcileFollowsTheLocalForwardRows(t *testing.T) {
	targetIP, targetPort := startEchoService(t, "first:")
	_, otherPort := startEchoService(t, "second:")
	localPort := freeDualStackPort(t)
	local := net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort))

	f := newLocalForwardFixture(t, models.BindScopeLoopback, localPort, targetIP, targetPort)

	f.reconcile(t)
	waitLocalStatus(t, f.m, 7, "the local forward to connect", isConnected)

	if result := f.reconcile(t); result != (ReconcileResult{}) {
		t.Fatalf("a pass over nothing that changed reported %+v", result)
	}

	f.change(func(hosts []models.Host, _ *[]models.LocalForward) {
		hosts[0].Enabled = false
	})

	result := f.reconcile(t)
	if result.Stopped != 1 {
		t.Fatalf("disabling the Host reported %+v, want one stopped", result)
	}
	if _, ok := f.m.LocalForwardStatus(7); ok {
		t.Fatal("the local forward of a disabled Host still reports a status")
	}
	if !portIsFree(localPort) {
		t.Fatal("the local port is still held after the Host was disabled")
	}

	f.change(func(hosts []models.Host, _ *[]models.LocalForward) {
		hosts[0].Enabled = true
	})

	result = f.reconcile(t)
	if result.Started != 1 {
		t.Fatalf("enabling the Host again reported %+v, want one started", result)
	}
	waitLocalStatus(t, f.m, 7, "the local forward to connect again", isConnected)

	f.m.localMu.RLock()
	before := f.m.localForwards[7]
	f.m.localMu.RUnlock()

	f.change(func(_ []models.Host, forwards *[]models.LocalForward) {
		(*forwards)[0].TargetPort = otherPort
	})

	result = f.reconcile(t)
	if result.Restarted != 1 || result.Started != 0 || result.Stopped != 0 {
		t.Fatalf("changing the target reported %+v, want one restarted", result)
	}

	f.m.localMu.RLock()
	after := f.m.localForwards[7]
	f.m.localMu.RUnlock()

	if after == nil || after == before {
		t.Fatal("the forward kept running on the target it was built with")
	}

	waitLocalStatus(t, f.m, 7, "the rebuilt local forward to connect", isConnected)

	if got := exchange(t, local, "ping"); got != "second:ping" {
		t.Fatalf("the answer after the target changed was %q, want %q", got, "second:ping")
	}

	f.change(func(_ []models.Host, forwards *[]models.LocalForward) {
		*forwards = nil
	})

	result = f.reconcile(t)
	if result.Stopped != 1 {
		t.Fatalf("deleting the row reported %+v, want one stopped", result)
	}
	if states := f.m.LocalForwardStatuses(); len(states) != 0 {
		t.Fatalf("a deleted row still has a forward running: %v", states)
	}
	if !portIsFree(localPort) {
		t.Fatal("the local port is still held after its row was deleted")
	}
}

// TestALocalPortInUseLeavesTheForwardInError is the port being taken by
// something else on this machine. The SSH connection stands, but nothing can
// be carried, and the status has to say why.
func TestALocalPortInUseLeavesTheForwardInError(t *testing.T) {
	targetIP, targetPort := startEchoService(t, "echo:")

	// A dual-stack wildcard listener, which holds the port in both families
	// and so refuses both halves of the pair.
	taken, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("failed to take a port: %v", err)
	}
	defer func() {
		_ = taken.Close()
	}()
	localPort := taken.Addr().(*net.TCPAddr).Port

	f := newLocalForwardFixture(t, models.BindScopeWildcard, localPort, targetIP, targetPort)
	f.reconcile(t)

	state := waitLocalStatus(t, f.m, 7, "the local forward to report the port in use", func(state LocalForwardState) bool {
		return state.Status == localStatusError
	})

	if !strings.Contains(state.LastError, "address already in use") {
		t.Fatalf("the forward reports the error %q, want the port in use named", state.LastError)
	}
	if f.server.handled.Load() == 0 {
		t.Fatal("the error was reported before the SSH connection was made")
	}
}

// TestALocalForwardRefusedOnItsHostKeyStopsTrying is the refusal of a Host
// that has no approved key, which a local forward gives up on the way a
// tunnel does, leaving the status that says so.
func TestALocalForwardRefusedOnItsHostKeyStopsTrying(t *testing.T) {
	targetIP, targetPort := startEchoService(t, "echo:")

	f := newLocalForwardFixture(t, models.BindScopeLoopback, freeDualStackPort(t), targetIP, targetPort)
	f.change(func(hosts []models.Host, _ *[]models.LocalForward) {
		hosts[0].HostKey = ""
	})

	f.reconcile(t)

	waitLocalStatus(t, f.m, 7, "the local forward to report the host key", func(state LocalForwardState) bool {
		return state.Status == StatusHostKeyUnapproved
	})

	// The interval is a second, so a forward that kept retrying would have
	// reached the server again by now.
	time.Sleep(1500 * time.Millisecond)

	state, _ := f.m.LocalForwardStatus(7)
	if state.Status != StatusHostKeyUnapproved || state.RetryCount != 0 {
		t.Fatalf("the refused forward went on to %+v", state)
	}
}

// TestALocalForwardReconnectsAfterTheHostDropsIt is the SSH connection going
// away under a forward that stands: the forward waits the interval, connects
// again and carries traffic once more.
func TestALocalForwardReconnectsAfterTheHostDropsIt(t *testing.T) {
	targetIP, targetPort := startEchoService(t, "echo:")
	localPort := freeDualStackPort(t)
	local := net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort))

	f := newLocalForwardFixture(t, models.BindScopeLoopback, localPort, targetIP, targetPort)
	f.reconcile(t)

	first := waitLocalStatus(t, f.m, 7, "the local forward to connect", isConnected)

	f.server.drop()

	waitLocalStatus(t, f.m, 7, "the local forward to connect again", func(state LocalForwardState) bool {
		return state.Status == localStatusConnected && state.LastConnectedAt.After(first.LastConnectedAt)
	})

	if got := exchange(t, local, "again"); got != "echo:again" {
		t.Fatalf("the answer after reconnecting was %q, want %q", got, "echo:again")
	}
	if handled := f.server.handled.Load(); handled < 2 {
		t.Fatalf("the server saw %d SSH connections, want a second one after the drop", handled)
	}
}
