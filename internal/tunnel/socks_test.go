package tunnel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
)

// socksFixture is a manager with one Host that points at a direct-tcpip
// server and asks for a SOCKS5 proxy on the IPv4 and IPv6 loopback.
type socksFixture struct {
	m      *Manager
	server *directSSHServer
	mu     sync.Mutex
	hosts  []models.Host
	none   []models.LocalForward
}

func newSocksFixture(t *testing.T, port int, allowed string) *socksFixture {
	t.Helper()

	server := startDirectSSHServer(t)

	serverHost, serverPort, err := net.SplitHostPort(server.addr)
	if err != nil {
		t.Fatalf("failed to split %s: %v", server.addr, err)
	}
	sshPort, err := strconv.Atoi(serverPort)
	if err != nil {
		t.Fatalf("failed to read the port of %s: %v", server.addr, err)
	}

	f := &socksFixture{server: server}
	f.hosts = []models.Host{{
		ID: 3, IP: serverHost, Port: sshPort, User: "tester", Password: "pass", // hook:allow
		HostKey: MarshalHostKey(server.key), Enabled: true,
		SocksEnabled: true, SocksPort: port, SocksBindScope: models.BindScopeLoopback,
		SocksAllowedSources: allowed,
	}}

	m, err := NewManager(newLocalForwardStubDB(t, &f.mu, &f.hosts, &f.none), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	t.Cleanup(m.StopAllTunnels)

	f.m = m

	return f
}

func (f *socksFixture) change(edit func(host *models.Host)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	edit(&f.hosts[0])
}

func (f *socksFixture) reconcile(t *testing.T) ReconcileResult {
	t.Helper()

	result, err := f.m.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}

	return result
}

func waitSocksConnected(t *testing.T, m *Manager, hostID uint) SocksState {
	t.Helper()

	var last SocksState
	waitFor(t, localTestTimeout, "the SOCKS5 proxy to connect", func() bool {
		state, ok := m.SocksStatus(hostID)
		last = state
		return ok && state.Status == localStatusConnected
	})

	return last
}

func dialSocks(t *testing.T, port int) net.Conn {
	t.Helper()

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), localTestTimeout)
	if err != nil {
		t.Fatalf("failed to connect to the SOCKS5 port %d: %v", port, err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
	})

	_ = conn.SetDeadline(time.Now().Add(localTestTimeout))

	return conn
}

func readExactly(t *testing.T, conn net.Conn, n int, what string) []byte {
	t.Helper()

	got := make([]byte, n)
	_, err := io.ReadFull(conn, got)
	if err != nil {
		t.Fatalf("failed to read %s: %v", what, err)
	}

	return got
}

// socksGreet offers the method without authentication and requires it back.
func socksGreet(t *testing.T, conn net.Conn) {
	t.Helper()

	_, err := conn.Write([]byte{0x05, 0x01, 0x00})
	if err != nil {
		t.Fatalf("failed to send the greeting: %v", err)
	}

	if got := readExactly(t, conn, 2, "the method"); !bytes.Equal(got, []byte{0x05, 0x00}) {
		t.Fatalf("the greeting was answered with %v, want [5 0]", got)
	}
}

// socksRequest sends one request and returns the reply code.
func socksRequest(t *testing.T, conn net.Conn, cmd byte, atyp byte, addr []byte, port int) byte {
	t.Helper()

	request := []byte{0x05, cmd, 0x00, atyp}
	if atyp == socksAtypDomain {
		request = append(request, byte(len(addr)))
	}
	request = append(request, addr...)
	request = append(request, byte(port>>8), byte(port))

	_, err := conn.Write(request)
	if err != nil {
		t.Fatalf("failed to send the request: %v", err)
	}

	reply := readExactly(t, conn, 10, "the reply")
	if reply[0] != 0x05 || reply[2] != 0x00 || reply[3] != socksAtypIPv4 {
		t.Fatalf("the reply is not shaped as RFC 1928 has it: %v", reply)
	}

	return reply[1]
}

// socksExchange sends payload over a connection the proxy has joined to its
// target and returns what came back once the target closed.
func socksExchange(t *testing.T, conn net.Conn, payload string) string {
	t.Helper()

	_, err := conn.Write([]byte(payload))
	if err != nil {
		t.Fatalf("failed to write through the proxy: %v", err)
	}

	err = conn.(*net.TCPConn).CloseWrite()
	if err != nil {
		t.Fatalf("failed to half-close the connection to the proxy: %v", err)
	}

	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("failed to read the answer through the proxy: %v", err)
	}

	return string(got)
}

// TestSocksConnectCarriesAnIPv4AndADomainTarget is the proxy end to end: a
// client names the target by its IPv4 address and by a name, and both reach
// it through the Host. The name is handed to the Host as it was sent, which
// is where it is resolved.
func TestSocksConnectCarriesAnIPv4AndADomainTarget(t *testing.T) {
	targetIP, targetPort := startEchoService(t, "echo:")
	port := freeDualStackPort(t)

	f := newSocksFixture(t, port, "198.51.100.0/24, 127.0.0.0/8")

	result := f.reconcile(t)
	if result.Started != 1 || result.Failed != 0 {
		t.Fatalf("the first pass reported %+v, want one started", result)
	}

	state := waitSocksConnected(t, f.m, 3)
	if state.LastConnectedAt.IsZero() || state.RetryCount != 0 || state.LastError != "" {
		t.Fatalf("a connected proxy reports %+v", state)
	}

	ip := netip.MustParseAddr(targetIP).As4()

	conn := dialSocks(t, port)
	socksGreet(t, conn)
	if rep := socksRequest(t, conn, socksCmdConnect, socksAtypIPv4, ip[:], targetPort); rep != socksRepSucceeded {
		t.Fatalf("CONNECT to %s:%d was answered with %d", targetIP, targetPort, rep)
	}
	if got := socksExchange(t, conn, "ping"); got != "echo:ping" {
		t.Fatalf("the answer through the proxy was %q, want %q", got, "echo:ping")
	}

	conn = dialSocks(t, port)
	socksGreet(t, conn)
	if rep := socksRequest(t, conn, socksCmdConnect, socksAtypDomain, []byte("localhost"), targetPort); rep != socksRepSucceeded {
		t.Fatalf("CONNECT to localhost:%d was answered with %d", targetPort, rep)
	}
	if got := socksExchange(t, conn, "name"); got != "echo:name" {
		t.Fatalf("the answer through the proxy by name was %q, want %q", got, "echo:name")
	}

	want := map[string]bool{
		net.JoinHostPort(targetIP, strconv.Itoa(targetPort)):    true,
		net.JoinHostPort("localhost", strconv.Itoa(targetPort)): true,
	}
	for _, got := range f.server.targets() {
		if !want[got] {
			t.Fatalf("the Host was asked to dial %q, want one of %v", got, want)
		}
		delete(want, got)
	}
	if len(want) != 0 {
		t.Fatalf("the Host was never asked to dial %v", want)
	}
}

// TestSocksRefusesAClientWithoutTheNoAuthMethod is a client that offers only
// username and password, which the proxy does not do.
func TestSocksRefusesAClientWithoutTheNoAuthMethod(t *testing.T) {
	port := freeDualStackPort(t)
	f := newSocksFixture(t, port, "")
	f.reconcile(t)
	waitSocksConnected(t, f.m, 3)

	conn := dialSocks(t, port)
	_, err := conn.Write([]byte{0x05, 0x01, 0x02})
	if err != nil {
		t.Fatalf("failed to send the greeting: %v", err)
	}

	if got := readExactly(t, conn, 2, "the method"); !bytes.Equal(got, []byte{0x05, 0xFF}) {
		t.Fatalf("the greeting was answered with %v, want [5 255]", got)
	}

	rest, _ := io.ReadAll(conn)
	if len(rest) != 0 {
		t.Fatalf("the proxy went on after refusing the methods: %v", rest)
	}
}

// TestSocksRefusesACommandOtherThanConnect is BIND, which the proxy does not
// carry, answered with "command not supported".
func TestSocksRefusesACommandOtherThanConnect(t *testing.T) {
	port := freeDualStackPort(t)
	f := newSocksFixture(t, port, "")
	f.reconcile(t)
	waitSocksConnected(t, f.m, 3)

	conn := dialSocks(t, port)
	socksGreet(t, conn)

	if rep := socksRequest(t, conn, 0x02, socksAtypIPv4, []byte{127, 0, 0, 1}, 80); rep != socksRepCommandNotSupported {
		t.Fatalf("BIND was answered with %d, want %d", rep, socksRepCommandNotSupported)
	}
	if len(f.server.targets()) != 0 {
		t.Fatalf("the Host was asked to dial for a refused command: %v", f.server.targets())
	}
}

// TestSocksClosesASourceThatIsNotAllowed is a client from 127.0.0.1 against a
// list that holds only 192.0.2.0/24. It is closed before anything is said to
// it.
func TestSocksClosesASourceThatIsNotAllowed(t *testing.T) {
	port := freeDualStackPort(t)
	f := newSocksFixture(t, port, "192.0.2.0/24")
	f.reconcile(t)
	waitSocksConnected(t, f.m, 3)

	conn := dialSocks(t, port)

	// Nothing is sent, so a proxy that let the source in would wait for the
	// greeting until its handshake timeout rather than close at once.
	began := time.Now()
	got, err := io.ReadAll(conn)
	if waited := time.Since(began); waited >= socksHandshakeTimeout/2 {
		t.Fatalf("the connection was held for %v before it was closed", waited)
	}
	if len(got) != 0 {
		t.Fatalf("a source that is not allowed was answered with %v", got)
	}
	if err != nil && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("the connection of a source that is not allowed ended with %v, want it closed", err)
	}
	if len(f.server.targets()) != 0 {
		t.Fatalf("the Host was asked to dial for a source that is not allowed: %v", f.server.targets())
	}
}

// TestSocksAnswersATargetTheHostCannotReach is a CONNECT to a port nothing
// listens on. The Host refuses the channel with the dial error, and the client
// is told the connection was refused rather than that it succeeded.
func TestSocksAnswersATargetTheHostCannotReach(t *testing.T) {
	port := freeDualStackPort(t)
	closed := freeDualStackPort(t)

	f := newSocksFixture(t, port, "")
	f.reconcile(t)
	waitSocksConnected(t, f.m, 3)

	conn := dialSocks(t, port)
	socksGreet(t, conn)

	rep := socksRequest(t, conn, socksCmdConnect, socksAtypIPv4, []byte{127, 0, 0, 1}, closed)
	if rep == socksRepSucceeded {
		t.Fatal("CONNECT to a closed port was answered as a success")
	}
	if rep != socksRepConnectionRefused {
		t.Fatalf("CONNECT to a closed port was answered with %d, want %d", rep, socksRepConnectionRefused)
	}
}

// TestSocksReconcileFollowsTheHost holds the pass to the Host row: the proxy
// starts when asked for, is left alone when nothing changed, is rebuilt on a
// new port or a new list, and is stopped with its port freed when it is
// switched off, when the Host is disabled, and by StopAllTunnels.
func TestSocksReconcileFollowsTheHost(t *testing.T) {
	targetIP, targetPort := startEchoService(t, "echo:")
	ip := netip.MustParseAddr(targetIP).As4()
	port := freeDualStackPort(t)

	f := newSocksFixture(t, port, "")

	roundTrip := func(port int) {
		t.Helper()

		conn := dialSocks(t, port)
		socksGreet(t, conn)
		if rep := socksRequest(t, conn, socksCmdConnect, socksAtypIPv4, ip[:], targetPort); rep != socksRepSucceeded {
			t.Fatalf("CONNECT through port %d was answered with %d", port, rep)
		}
		if got := socksExchange(t, conn, "ping"); got != "echo:ping" {
			t.Fatalf("the answer through port %d was %q", port, got)
		}
	}

	if result := f.reconcile(t); result.Started != 1 {
		t.Fatalf("a Host asking for a proxy reported %+v, want one started", result)
	}
	waitSocksConnected(t, f.m, 3)
	roundTrip(port)

	if result := f.reconcile(t); result != (ReconcileResult{}) {
		t.Fatalf("a pass over nothing that changed reported %+v", result)
	}

	moved := freeDualStackPort(t)
	f.change(func(host *models.Host) {
		host.SocksPort = moved
	})
	if result := f.reconcile(t); result.Restarted != 1 || result.Started != 0 || result.Stopped != 0 {
		t.Fatalf("moving the port reported %+v, want one restarted", result)
	}
	if !portIsFree(port) {
		t.Fatal("the old port is still held after the proxy moved")
	}
	waitSocksConnected(t, f.m, 3)
	roundTrip(moved)

	f.change(func(host *models.Host) {
		host.SocksAllowedSources = "192.0.2.0/24"
	})
	if result := f.reconcile(t); result.Restarted != 1 {
		t.Fatalf("changing the allowed sources reported %+v, want one restarted", result)
	}

	f.change(func(host *models.Host) {
		host.SocksAllowedSources = ""
		host.SocksEnabled = false
	})
	if result := f.reconcile(t); result.Stopped != 1 {
		t.Fatalf("switching the proxy off reported %+v, want one stopped", result)
	}
	if _, ok := f.m.SocksStatus(3); ok {
		t.Fatal("a proxy that was switched off still reports a status")
	}
	if !portIsFree(moved) {
		t.Fatal("the port is still held after the proxy was switched off")
	}

	f.change(func(host *models.Host) {
		host.SocksEnabled = true
	})
	if result := f.reconcile(t); result.Started != 1 {
		t.Fatalf("switching the proxy on again reported %+v, want one started", result)
	}
	waitSocksConnected(t, f.m, 3)

	f.change(func(host *models.Host) {
		host.Enabled = false
	})
	if result := f.reconcile(t); result.Stopped != 1 {
		t.Fatalf("disabling the Host reported %+v, want one stopped", result)
	}
	if !portIsFree(moved) {
		t.Fatal("the port is still held after the Host was disabled")
	}

	f.change(func(host *models.Host) {
		host.Enabled = true
		host.SocksPort = 0
	})
	if result := f.reconcile(t); result != (ReconcileResult{}) {
		t.Fatalf("a proxy with no port reported %+v, want nothing started", result)
	}

	f.change(func(host *models.Host) {
		host.SocksPort = moved
	})
	if result := f.reconcile(t); result.Started != 1 {
		t.Fatalf("giving the proxy a port reported %+v, want one started", result)
	}
	waitSocksConnected(t, f.m, 3)

	f.m.StopAllTunnels()

	if states := f.m.SocksStatuses(); len(states) != 0 {
		t.Fatalf("proxies still run after StopAllTunnels: %v", states)
	}
	if !portIsFree(moved) {
		t.Fatal("the port is still held after StopAllTunnels")
	}
}

// TestSocksParseAllowedSources is the list as a person writes it, the forms
// of IPv4 that name the same source, and what is refused.
func TestSocksParseAllowedSources(t *testing.T) {
	got, err := ParseAllowedSources(" 192.0.2.0/24,198.51.100.7\n2001:db8::/32 ::ffff:203.0.113.0/120 ,, ::ffff:203.0.113.9 ")
	if err != nil {
		t.Fatalf("a list of valid sources was refused: %v", err)
	}

	want := []netip.Prefix{
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.51.100.7/32"),
		netip.MustParsePrefix("2001:db8::/32"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("203.0.113.9/32"),
	}
	if len(got) != len(want) {
		t.Fatalf("the list came back as %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the list came back as %v, want %v", got, want)
		}
	}

	empty, err := ParseAllowedSources(" , ")
	if err != nil || empty != nil {
		t.Fatalf("an empty list came back as %v, %v", empty, err)
	}

	masked, err := ParseAllowedSources("192.0.2.77/24")
	if err != nil || len(masked) != 1 || masked[0] != netip.MustParsePrefix("192.0.2.0/24") {
		t.Fatalf("a block with host bits came back as %v, %v", masked, err)
	}

	for _, bad := range []string{"example.com", "192.0.2.0/33", "192.0.2.1/", "2001:db8::1%eth0", "::ffff:0:0/64"} {
		_, err := ParseAllowedSources("192.0.2.1, " + bad)
		if err == nil {
			t.Errorf("%q was taken as a source", bad)
		}
	}
}

// TestSocksSourceAllowedUnmapsTheRemote is a remote address as the kernel
// hands it over on a dual-stack socket, IPv4 inside IPv6, against a list that
// names it as IPv4.
func TestSocksSourceAllowedUnmapsTheRemote(t *testing.T) {
	allowed, err := ParseAllowedSources("192.0.2.0/24, 2001:db8::1")
	if err != nil {
		t.Fatalf("failed to parse the list: %v", err)
	}

	cases := []struct {
		ip   string
		want bool
	}{
		{"192.0.2.5", true},
		{"::ffff:192.0.2.5", true},
		{"198.51.100.5", false},
		{"::ffff:198.51.100.5", false},
		{"2001:db8::1", true},
		{"2001:db8::2", false},
	}

	for _, c := range cases {
		addr := &net.TCPAddr{IP: net.ParseIP(c.ip), Port: 40000}
		if got := sourceAllowed(allowed, addr); got != c.want {
			t.Errorf("%s allowed=%v, want %v", c.ip, got, c.want)
		}
	}

	mapped := &net.TCPAddr{IP: netip.MustParseAddr("::ffff:192.0.2.5").AsSlice(), Port: 40000}
	if !sourceAllowed(allowed, mapped) {
		t.Error("a 16-byte IPv4-mapped remote was not matched against its IPv4 block")
	}

	if !sourceAllowed(nil, &net.TCPAddr{IP: net.ParseIP("203.0.113.1")}) {
		t.Error("an empty list refused a source")
	}
}

// socksScript is a client that has sent in and reads nothing, so what the
// proxy writes back is kept in out.
type socksScript struct {
	in  io.Reader
	out bytes.Buffer
}

func (s *socksScript) Read(p []byte) (int, error) {
	return s.in.Read(p)
}

func (s *socksScript) Write(p []byte) (int, error) {
	return s.out.Write(p)
}

// TestSocksRequestReadsEveryAddressType reads the three address types and the
// type the proxy does not know, without a connection.
func TestSocksRequestReadsEveryAddressType(t *testing.T) {
	greeting := []byte{0x05, 0x02, 0x02, 0x00}

	cases := []struct {
		name    string
		request []byte
		want    string
	}{
		{"ipv4", []byte{0x05, 0x01, 0x00, 0x01, 203, 0, 113, 4, 0x01, 0xBB}, "203.0.113.4:443"},
		{"ipv6", append(append([]byte{0x05, 0x01, 0x00, 0x04}, netip.MustParseAddr("2001:db8::7").AsSlice()...), 0x00, 0x50), "[2001:db8::7]:80"},
		{"domain", append(append([]byte{0x05, 0x01, 0x00, 0x03, 11}, "example.org"...), 0x1F, 0x90), "example.org:8080"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			script := &socksScript{in: bytes.NewReader(append(append([]byte(nil), greeting...), c.request...))}

			got, err := readSocksRequest(script)
			if err != nil {
				t.Fatalf("the request was refused: %v", err)
			}
			if got != c.want {
				t.Fatalf("the target came back as %q, want %q", got, c.want)
			}
			if !bytes.Equal(script.out.Bytes(), []byte{0x05, 0x00}) {
				t.Fatalf("what was written back is %v, want only the method", script.out.Bytes())
			}
		})
	}

	script := &socksScript{in: bytes.NewReader(append(append([]byte(nil), greeting...), 0x05, 0x01, 0x00, 0x09))}
	_, err := readSocksRequest(script)
	if !errors.Is(err, errSocksRefused) {
		t.Fatalf("an unknown address type came back as %v", err)
	}
	if written := script.out.Bytes(); len(written) != 12 || written[3] != socksRepAddressNotSupported {
		t.Fatalf("an unknown address type was answered with %v", written)
	}
}

// TestSocksReplyForReadsTheChannelRefusal maps what a Host says about a
// target it could not reach onto the replies of RFC 1928.
func TestSocksReplyForReadsTheChannelRefusal(t *testing.T) {
	cases := []struct {
		err  error
		want byte
	}{
		{&ssh.OpenChannelError{Reason: ssh.ConnectionFailed, Message: "Connection refused"}, socksRepConnectionRefused},
		{&ssh.OpenChannelError{Reason: ssh.ConnectionFailed, Message: "Network is unreachable"}, socksRepNetworkUnreachable},
		{&ssh.OpenChannelError{Reason: ssh.ConnectionFailed, Message: "No route to host"}, socksRepHostUnreachable},
		{&ssh.OpenChannelError{Reason: ssh.ConnectionFailed, Message: "Name or service not known"}, socksRepHostUnreachable},
		{&ssh.OpenChannelError{Reason: ssh.Prohibited, Message: "open failed"}, socksRepNotAllowed},
		{&ssh.OpenChannelError{Reason: ssh.ConnectionFailed, Message: "something else"}, socksRepGeneralFailure},
		{context.DeadlineExceeded, socksRepHostUnreachable},
		{errors.New("unexpected"), socksRepGeneralFailure},
	}

	for _, c := range cases {
		if got := socksReplyFor(c.err); got != c.want {
			t.Errorf("%v was answered with %d, want %d", c.err, got, c.want)
		}
	}
}
