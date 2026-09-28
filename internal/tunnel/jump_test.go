package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// jumpTestServer is an SSH server that stands in for a Host on a jump route or
// at the end of one. It accepts the password "pass", opens every direct-tcpip
// channel it is asked for, which is what a Host passed through does for the
// next one, and opens every port tcpip-forward asks for, carrying what that
// port accepts back as forwarded-tcpip channels, which is what the Host of a
// tunnel does.
type jumpTestServer struct {
	addr string
	key  ssh.PublicKey

	mu     sync.Mutex
	dialed []string
	conns  []net.Conn

	handshakes atomic.Int64
	active     atomic.Int64
}

func (s *jumpTestServer) targets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.dialed...)
}

// drop closes every connection the server holds, the way a Host that went away
// does.
func (s *jumpTestServer) drop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, conn := range s.conns {
		_ = conn.Close()
	}
	s.conns = nil
}

func startJumpTestServer(t *testing.T) *jumpTestServer {
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

	s := &jumpTestServer{addr: ln.Addr().String(), key: signer.PublicKey()}

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

func (s *jumpTestServer) serve(conn net.Conn, config *ssh.ServerConfig) {
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

	s.handshakes.Add(1)
	s.active.Add(1)
	defer s.active.Add(-1)

	var listenersMu sync.Mutex
	var listeners []net.Listener
	defer func() {
		listenersMu.Lock()
		defer listenersMu.Unlock()

		for _, ln := range listeners {
			_ = ln.Close()
		}
	}()

	go func() {
		for req := range reqs {
			if req.Type != "tcpip-forward" {
				if req.WantReply {
					_ = req.Reply(req.Type == "cancel-tcpip-forward", nil)
				}
				continue
			}

			var asked struct {
				Addr string
				Port uint32
			}
			if ssh.Unmarshal(req.Payload, &asked) != nil {
				_ = req.Reply(false, nil)
				continue
			}

			ln, err := net.Listen("tcp", net.JoinHostPort(asked.Addr, strconv.Itoa(int(asked.Port))))
			if err != nil {
				_ = req.Reply(false, nil)
				continue
			}

			listenersMu.Lock()
			listeners = append(listeners, ln)
			listenersMu.Unlock()

			bound := uint32(ln.Addr().(*net.TCPAddr).Port)
			_ = req.Reply(true, ssh.Marshal(struct{ Port uint32 }{Port: bound}))

			go forwardAccepted(sshConn, ln, asked.Addr, bound)
		}
	}()

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
		if ssh.Unmarshal(newChan.ExtraData(), &target) != nil {
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

		go joinTestChannel(channel, upstream)
	}
}

// forwardAccepted carries what a forwarded port accepts back to the client as
// forwarded-tcpip channels (RFC 4254, 7.2).
func forwardAccepted(sshConn *ssh.ServerConn, ln net.Listener, addr string, port uint32) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}

		origin := conn.RemoteAddr().(*net.TCPAddr)
		payload := ssh.Marshal(struct {
			Addr     string
			Port     uint32
			OrigAddr string
			OrigPort uint32
		}{addr, port, origin.IP.String(), uint32(origin.Port)})

		channel, requests, err := sshConn.OpenChannel("forwarded-tcpip", payload)
		if err != nil {
			_ = conn.Close()
			continue
		}
		go ssh.DiscardRequests(requests)

		go joinTestChannel(channel, conn)
	}
}

// joinTestChannel carries bytes between a channel and a TCP connection, passing
// the end of each direction on as a half-close.
func joinTestChannel(channel ssh.Channel, conn net.Conn) {
	var done sync.WaitGroup
	done.Add(2)

	go func() {
		defer done.Done()
		_, _ = io.Copy(channel, conn)
		_ = channel.CloseWrite()
	}()
	go func() {
		defer done.Done()
		_, _ = io.Copy(conn, channel)
		_ = conn.(*net.TCPConn).CloseWrite()
	}()

	done.Wait()
	_ = channel.Close()
	_ = conn.Close()
}

// hostAt is the Host row of a test server, trusted on the key the server
// presents.
func hostAt(t *testing.T, id uint, s *jumpTestServer) models.Host {
	t.Helper()

	address, port := splitTestAddr(t, s.addr)

	return models.Host{
		ID: id, Address: address, Port: port, User: "tester", Password: "pass", // hook:allow
		HostKey: MarshalHostKey(s.key), Enabled: true,
	}
}

func splitTestAddr(t *testing.T, addr string) (string, int) {
	t.Helper()

	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("failed to split %s: %v", addr, err)
	}

	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("failed to read the port of %s: %v", addr, err)
	}

	return host, port
}

// jumpFixture is a manager over a real database holding two Hosts: Host 1,
// which is target, reached through Host 2, which is jump. Nothing is assigned
// to Host 1 yet; each test adds what it runs.
type jumpFixture struct {
	db     *gorm.DB
	m      *Manager
	jump   *jumpTestServer
	target *jumpTestServer
}

func newJumpFixture(t *testing.T, edit func(target, jump *models.Host)) *jumpFixture {
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

	err = db.AutoMigrate(&models.Host{}, &models.HostJump{}, &models.ServicePort{},
		&models.HostServicePort{}, &models.LocalForward{}, &models.Tunnel{})
	if err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	f := &jumpFixture{db: db, jump: startJumpTestServer(t), target: startJumpTestServer(t)}

	target := hostAt(t, 1, f.target)
	jump := hostAt(t, 2, f.jump)
	if edit != nil {
		edit(&target, &jump)
	}

	for _, host := range []*models.Host{&target, &jump} {
		err = db.Create(host).Error
		if err != nil {
			t.Fatalf("failed to store Host %d: %v", host.ID, err)
		}
	}

	f.route(t, 2)

	m, err := NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	t.Cleanup(m.StopAllTunnels)

	f.m = m

	return f
}

// route stores the jump route of Host 1 as the Hosts given, in order.
func (f *jumpFixture) route(t *testing.T, ids ...uint) {
	t.Helper()

	err := f.db.Where("host_id = ?", 1).Delete(&models.HostJump{}).Error
	if err != nil {
		t.Fatalf("failed to clear the route: %v", err)
	}

	for i, id := range ids {
		err = f.db.Create(&models.HostJump{HostID: 1, Seq: uint(i + 1), JumpHostID: id}).Error
		if err != nil {
			t.Fatalf("failed to store the route: %v", err)
		}
	}
}

// addTunnel assigns a service port on serviceIP:servicePort to Host 1, opened
// on the loopback of the Host at localPort.
func (f *jumpFixture) addTunnel(t *testing.T, serviceIP string, servicePort, localPort int) {
	t.Helper()

	err := f.db.Create(&models.ServicePort{ID: 1, ServiceAddress: serviceIP, ServicePort: servicePort,
		LocalPort: localPort}).Error
	if err != nil {
		t.Fatalf("failed to store the service port: %v", err)
	}

	err = f.db.Create(&models.HostServicePort{HostID: 1, SPID: 1, BindScope: models.BindScopeLoopback,
		Enabled: true}).Error
	if err != nil {
		t.Fatalf("failed to store the assignment: %v", err)
	}
}

// addLocalForward gives Host 1 a local forward from localPort on the loopback
// of this machine to targetIP:targetPort.
func (f *jumpFixture) addLocalForward(t *testing.T, localPort int, targetIP string, targetPort int) {
	t.Helper()

	err := f.db.Create(&models.LocalForward{HostID: 1, Number: 1, BindScope: models.BindScopeLoopback,
		LocalPort: localPort, TargetAddress: targetIP, TargetPort: targetPort, Enabled: true}).Error
	if err != nil {
		t.Fatalf("failed to store the local forward: %v", err)
	}
}

func (f *jumpFixture) reconcile(t *testing.T) ReconcileResult {
	t.Helper()

	result, err := f.m.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}

	return result
}

// waitTunnelRow waits for the row of the tunnel of Host 1 to satisfy done and
// returns it.
func (f *jumpFixture) waitTunnelRow(t *testing.T, what string, done func(models.Tunnel) bool) models.Tunnel {
	t.Helper()

	var row models.Tunnel
	waitFor(t, localTestTimeout, what, func() bool {
		row = models.Tunnel{}
		err := f.db.Where("host_id = ? AND sp_id = ?", 1, 1).First(&row).Error
		return err == nil && done(row)
	})

	return row
}

func (f *jumpFixture) host(t *testing.T, id uint) models.Host {
	t.Helper()

	var host models.Host
	err := f.db.First(&host, id).Error
	if err != nil {
		t.Fatalf("failed to read Host %d: %v", id, err)
	}

	return host
}

// jumpPrefix is what the error of a connection that failed at the Host
// passed through starts with.
func (f *jumpFixture) jumpPrefix() string {
	return "jump 1 (host #2 " + f.jump.addr + ")"
}

func assertOnlyDialed(t *testing.T, s *jumpTestServer, want string) {
	t.Helper()

	targets := s.targets()
	if len(targets) == 0 {
		t.Fatalf("the server was never asked to open a channel to %s", want)
	}
	for _, got := range targets {
		if got != want {
			t.Fatalf("the server was asked to open a channel to %s, want only %s: %v", got, want, targets)
		}
	}
}

// assertJumpFailure checks where on the route a connection is reported to
// have stopped and why.
func assertJumpFailure(t *testing.T, what string, seq int, hostID uint, reason string, want jumpFailure) {
	t.Helper()

	got := jumpFailure{seq: seq, hostID: hostID, reason: reason}
	if got != want {
		t.Fatalf("%s reports jump_seq %d, jump_host_id %d, jump_reason %q, want %d, %d, %q",
			what, got.seq, got.hostID, got.reason, want.seq, want.hostID, want.reason)
	}
}

// TestAJumpRouteCarriesARemoteTunnel is a tunnel of a Host reached through
// another: the Host passed through opens a channel to the Host at the end and
// to nothing else, the Host at the end opens the forwarded port, and what that
// port accepts reaches the service on this machine.
func TestAJumpRouteCarriesARemoteTunnel(t *testing.T) {
	serviceIP, servicePort := startEchoService(t, "echo:")
	localPort := freeDualStackPort(t)

	f := newJumpFixture(t, nil)
	f.addTunnel(t, serviceIP, servicePort, localPort)

	result := f.reconcile(t)
	if result.Started != 1 || result.Failed != 0 {
		t.Fatalf("the first pass reported %+v, want one started", result)
	}

	f.waitTunnelRow(t, "the tunnel to connect", func(row models.Tunnel) bool {
		return row.Status == statusConnected
	})

	if got := exchange(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)), "ping"); got != "echo:ping" {
		t.Fatalf("the answer through the tunnel was %q, want %q", got, "echo:ping")
	}

	assertOnlyDialed(t, f.jump, f.target.addr)

	if f.target.handshakes.Load() == 0 {
		t.Fatal("the Host at the end of the route was never logged into")
	}
}

// TestAJumpRouteCarriesALocalForward is a local forward of a Host reached
// through another: the target is dialled by the Host at the end, over a
// connection the Host passed through carries.
func TestAJumpRouteCarriesALocalForward(t *testing.T) {
	targetIP, targetPort := startEchoService(t, "echo:")
	localPort := freeDualStackPort(t)

	f := newJumpFixture(t, nil)
	f.addLocalForward(t, localPort, targetIP, targetPort)

	result := f.reconcile(t)
	if result.Started != 1 || result.Failed != 0 {
		t.Fatalf("the first pass reported %+v, want one started", result)
	}

	waitLocalStatus(t, f.m, LocalForwardKey{HostID: 1, Number: 1}, "the local forward to connect", isConnected)

	if got := exchange(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)), "ping"); got != "echo:ping" {
		t.Fatalf("the answer through the local forward was %q, want %q", got, "echo:ping")
	}

	assertOnlyDialed(t, f.jump, f.target.addr)
	assertOnlyDialed(t, f.target, net.JoinHostPort(targetIP, strconv.Itoa(targetPort)))
}

// TestAJumpRouteCarriesASocksProxy is the SOCKS5 proxy of a Host reached
// through another.
func TestAJumpRouteCarriesASocksProxy(t *testing.T) {
	targetIP, targetPort := startEchoService(t, "echo:")
	socksPort := freeDualStackPort(t)

	f := newJumpFixture(t, func(target, _ *models.Host) {
		target.SocksEnabled = true
		target.SocksPort = socksPort
		target.SocksBindScope = models.BindScopeLoopback
	})

	result := f.reconcile(t)
	if result.Started != 1 || result.Failed != 0 {
		t.Fatalf("the first pass reported %+v, want one started", result)
	}

	waitSocksConnected(t, f.m, 1)

	ip := netip.MustParseAddr(targetIP).As4()

	conn := dialSocks(t, socksPort)
	socksGreet(t, conn)
	if rep := socksRequest(t, conn, socksCmdConnect, socksAtypIPv4, ip[:], targetPort); rep != socksRepSucceeded {
		t.Fatalf("CONNECT to %s:%d was answered with %d", targetIP, targetPort, rep)
	}
	if got := socksExchange(t, conn, "ping"); got != "echo:ping" {
		t.Fatalf("the answer through the proxy was %q, want %q", got, "echo:ping")
	}

	assertOnlyDialed(t, f.jump, f.target.addr)
	assertOnlyDialed(t, f.target, net.JoinHostPort(targetIP, strconv.Itoa(targetPort)))
}

// TestAnUnapprovedKeyOfAJumpHostIsAskedOfThatHost is a Host passed through
// whose key nobody has approved. The key it presented is waiting on its own
// row, the Host at the end is left as it was and never reached, and the
// tunnel says which step of the route refused.
func TestAnUnapprovedKeyOfAJumpHostIsAskedOfThatHost(t *testing.T) {
	f := newJumpFixture(t, func(_, jump *models.Host) {
		jump.HostKey = ""
	})
	f.addTunnel(t, "127.0.0.1", 1, freeDualStackPort(t))

	f.reconcile(t)

	row := f.waitTunnelRow(t, "the tunnel to be refused on the host key", func(row models.Tunnel) bool {
		return row.Status == StatusHostKeyUnapproved
	})
	if !strings.HasPrefix(row.LastError, f.jumpPrefix()+": ") {
		t.Fatalf("the tunnel says %q, want it to start with %q", row.LastError, f.jumpPrefix())
	}
	assertJumpFailure(t, "the tunnel", row.JumpSeq, row.JumpHostID, row.JumpReason,
		jumpFailure{seq: 1, hostID: 2, reason: JumpReasonHostKey})

	jump := f.host(t, 2)
	if jump.PendingHostKey != MarshalHostKey(f.jump.key) {
		t.Fatalf("the Host passed through holds %q waiting, want the key it presented", jump.PendingHostKey)
	}

	target := f.host(t, 1)
	if target.PendingHostKey != "" || target.HostKey != MarshalHostKey(f.target.key) {
		t.Fatalf("the Host at the end was changed: waiting %q, trusted %q", target.PendingHostKey, target.HostKey)
	}

	if n := f.target.handshakes.Load(); n != 0 {
		t.Fatalf("the Host at the end was logged into %d times behind a refused key", n)
	}
}

// TestADisabledJumpHostIsNotDialled is a route through a Host that is
// disabled. Nothing is dialled, the tunnel and the local forward say which
// step it is, and enabling the Host brings both up.
func TestADisabledJumpHostIsNotDialled(t *testing.T) {
	serviceIP, servicePort := startEchoService(t, "echo:")
	targetIP, targetPort := startEchoService(t, "local:")

	f := newJumpFixture(t, func(_, jump *models.Host) {
		jump.Enabled = false
	})
	f.addTunnel(t, serviceIP, servicePort, freeDualStackPort(t))
	f.addLocalForward(t, freeDualStackPort(t), targetIP, targetPort)

	result := f.reconcile(t)
	if result.Started != 2 || result.Failed != 0 {
		t.Fatalf("the first pass reported %+v, want both started", result)
	}

	row := f.waitTunnelRow(t, "the tunnel to report the disabled Host", func(row models.Tunnel) bool {
		return row.Status == StatusJumpHostDisabled
	})
	if !strings.HasPrefix(row.LastError, f.jumpPrefix()+" is disabled") {
		t.Fatalf("the tunnel says %q, want it to name %q as disabled", row.LastError, f.jumpPrefix())
	}
	assertJumpFailure(t, "the tunnel", row.JumpSeq, row.JumpHostID, row.JumpReason,
		jumpFailure{seq: 1, hostID: 2, reason: JumpReasonDisabled})

	state := waitLocalStatus(t, f.m, LocalForwardKey{HostID: 1, Number: 1}, "the local forward to report the disabled Host",
		func(state LocalForwardState) bool { return state.Status == StatusJumpHostDisabled })
	if !strings.HasPrefix(state.LastError, f.jumpPrefix()+" is disabled") {
		t.Fatalf("the local forward says %q, want it to name %q as disabled", state.LastError, f.jumpPrefix())
	}
	assertJumpFailure(t, "the local forward", state.JumpSeq, state.JumpHostID, state.JumpReason,
		jumpFailure{seq: 1, hostID: 2, reason: JumpReasonDisabled})

	if n := f.jump.handshakes.Load() + f.target.handshakes.Load(); n != 0 {
		t.Fatalf("%d connections were made through a disabled Host", n)
	}

	if again := f.reconcile(t); again.Started != 0 || again.Restarted != 0 || again.Failed != 0 {
		t.Fatalf("a pass over the same route reported %+v, want nothing done", again)
	}

	err := f.db.Model(&models.Host{}).Where("id = ?", 2).Update("enabled", true).Error
	if err != nil {
		t.Fatalf("failed to enable the Host passed through: %v", err)
	}

	result = f.reconcile(t)
	if result.Restarted != 2 || result.Failed != 0 {
		t.Fatalf("the pass after enabling the Host passed through reported %+v, want both restarted", result)
	}

	row = f.waitTunnelRow(t, "the tunnel to connect", func(row models.Tunnel) bool {
		return row.Status == statusConnected
	})
	assertJumpFailure(t, "the connected tunnel", row.JumpSeq, row.JumpHostID, row.JumpReason, jumpFailure{})

	state = waitLocalStatus(t, f.m, LocalForwardKey{HostID: 1, Number: 1}, "the local forward to connect", isConnected)
	assertJumpFailure(t, "the connected local forward", state.JumpSeq, state.JumpHostID, state.JumpReason,
		jumpFailure{})
}

// TestAJumpHostThatDoesNotAnswerIsNamed is a route whose first step refuses
// the connection. The error says which step and which Host it was.
func TestAJumpHostThatDoesNotAnswerIsNamed(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()

	f := newJumpFixture(t, func(_, jump *models.Host) {
		jump.Address, jump.Port = splitTestAddr(t, closedAddr)
	})
	f.addTunnel(t, "127.0.0.1", 1, freeDualStackPort(t))

	f.reconcile(t)

	want := "jump 1 (host #2 " + closedAddr + "): "
	row := f.waitTunnelRow(t, "the tunnel to fail", func(row models.Tunnel) bool {
		return row.LastError != ""
	})
	if !strings.HasPrefix(row.LastError, want) {
		t.Fatalf("the tunnel says %q, want it to start with %q", row.LastError, want)
	}
	if row.Status != localStatusError && row.Status != localStatusReconnecting {
		t.Fatalf("the tunnel status is %q, want error or reconnecting", row.Status)
	}
	assertJumpFailure(t, "the tunnel", row.JumpSeq, row.JumpHostID, row.JumpReason,
		jumpFailure{seq: 1, hostID: 2, reason: JumpReasonDial})
}

// TestARouteNamingAHostThatIsNotThereIsRefused is a route left naming a Host
// that was removed. Nothing is dialled, and the tunnel says which step names
// which Host.
func TestARouteNamingAHostThatIsNotThereIsRefused(t *testing.T) {
	f := newJumpFixture(t, nil)
	f.route(t, 9)
	f.addTunnel(t, "127.0.0.1", 1, freeDualStackPort(t))

	f.reconcile(t)

	row := f.waitTunnelRow(t, "the tunnel to be refused", func(row models.Tunnel) bool {
		return row.LastError != ""
	})
	if row.Status != localStatusError || row.LastError != "jump 1 names host #9, which is not registered" {
		t.Fatalf("the tunnel is %q with %q", row.Status, row.LastError)
	}
	assertJumpFailure(t, "the tunnel", row.JumpSeq, row.JumpHostID, row.JumpReason,
		jumpFailure{seq: 1, hostID: 9, reason: JumpReasonRoute})

	if n := f.jump.handshakes.Load() + f.target.handshakes.Load(); n != 0 {
		t.Fatalf("%d connections were made on a route naming a Host that is not there", n)
	}
}

// TestCheckJumpRouteRefusesWhatCannotBeDialled covers the routes that are
// wrong as they are stored, which the API refuses and a row written some other
// way can still hold.
func TestCheckJumpRouteRefusesWhatCannotBeDialled(t *testing.T) {
	target := &models.Host{ID: 1, Address: "192.0.2.1", Port: 22, Enabled: true}
	hop := func(id uint) jumpHop {
		return jumpHop{id: id, host: &models.Host{ID: id, Address: "192.0.2." + strconv.Itoa(int(id)), Port: 22,
			Enabled: true}}
	}

	long := make([]jumpHop, 0, MaxJumps+1)
	for i := 0; i <= MaxJumps; i++ {
		long = append(long, hop(uint(i+10)))
	}

	cases := []struct {
		name   string
		hops   []jumpHop
		status string
		reason string
		jump   jumpFailure
	}{
		{"itself", []jumpHop{hop(2), {id: 1, host: target}}, localStatusError,
			"jump 2 (host #1 192.0.2.1:22) is this Host itself",
			jumpFailure{seq: 2, hostID: 1, reason: JumpReasonRoute}},
		{"twice", []jumpHop{hop(2), hop(3), hop(2)}, localStatusError,
			"jump 3 (host #2 192.0.2.2:22) is jump 1 as well",
			jumpFailure{seq: 3, hostID: 2, reason: JumpReasonRoute}},
		{"missing", []jumpHop{hop(2), {id: 7}}, localStatusError,
			"jump 2 names host #7, which is not registered",
			jumpFailure{seq: 2, hostID: 7, reason: JumpReasonRoute}},
		{"too long", long, localStatusError,
			"the jump route of this Host passes through 9 Hosts, and a route may pass through 8 at most",
			jumpFailure{reason: JumpReasonRoute}},
		{"disabled", []jumpHop{hop(2), {id: 3, host: &models.Host{ID: 3, Address: "192.0.2.3", Port: 22}}},
			StatusJumpHostDisabled, "jump 2 (host #3 192.0.2.3:22) is disabled, so the route to this Host is not dialled",
			jumpFailure{seq: 2, hostID: 3, reason: JumpReasonDisabled}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refusal := checkJumpRoute(target, tc.hops)
			if refusal == nil {
				t.Fatal("the route was not refused")
			}
			if refusal.status != tc.status || refusal.reason != tc.reason {
				t.Fatalf("the route was refused as %q with %q, want %q with %q",
					refusal.status, refusal.reason, tc.status, tc.reason)
			}
			if refusal.jump != tc.jump {
				t.Fatalf("the route was refused at %+v, want %+v", refusal.jump, tc.jump)
			}
		})
	}

	if refusal := checkJumpRoute(target, long[:MaxJumps]); refusal != nil {
		t.Fatalf("a route of %d Hosts was refused: %s", MaxJumps, refusal.reason)
	}
}

// testClientConfig is what the tests dial a jumpTestServer with.
func testClientConfig(timeout time.Duration) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:            "tester",
		Auth:            []ssh.AuthMethod{ssh.Password("pass")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         timeout,
	}
}

// TestClosingTheEndOfARouteClosesTheWholeOfIt is the client handed back for a
// route of two: closing it closes the connection to the Host passed through as
// well, and so does the Host at the end dropping it.
func TestClosingTheEndOfARouteClosesTheWholeOfIt(t *testing.T) {
	for _, how := range []string{"closed here", "dropped by the far end"} {
		t.Run(how, func(t *testing.T) {
			jump := startJumpTestServer(t)
			target := startJumpTestServer(t)

			client, _, err := dialSSHClient([]sshHop{
				{hostID: 2, addr: jump.addr, config: testClientConfig(localTestTimeout)},
				{hostID: 1, addr: target.addr, config: testClientConfig(localTestTimeout)},
			})
			if err != nil {
				t.Fatalf("failed to connect over the route: %v", err)
			}

			if jump.active.Load() != 1 || target.active.Load() != 1 {
				t.Fatalf("the route holds %d and %d connections, want one each",
					jump.active.Load(), target.active.Load())
			}

			if how == "closed here" {
				_ = client.Close()
			} else {
				target.drop()
			}

			waitFor(t, localTestTimeout, "every connection of the route to close", func() bool {
				return jump.active.Load() == 0 && target.active.Load() == 0
			})

			_ = client.Close()
		})
	}
}

// TestAHopThatSendsNothingIsGivenUpOn is a Host at the end of a route that
// takes the channel and never sends its banner. A channel takes no deadline,
// so the handshake is bounded some other way, and the route is closed.
func TestAHopThatSendsNothingIsGivenUpOn(t *testing.T) {
	jump := startJumpTestServer(t)
	silent := startSilentListener(t)

	start := time.Now()
	_, _, err := dialSSHClient([]sshHop{
		{hostID: 2, addr: jump.addr, config: testClientConfig(localTestTimeout)},
		{hostID: 1, addr: silent, config: testClientConfig(300 * time.Millisecond)},
	})
	if err == nil {
		t.Fatal("a Host that sends nothing was connected to")
	}
	if !strings.Contains(err.Error(), "did not finish within 300ms") {
		t.Fatalf("the error is %q, want the handshake to have run out of time", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("giving up took %v", elapsed)
	}

	waitFor(t, localTestTimeout, "the connection to the Host passed through to close", func() bool {
		return jump.active.Load() == 0
	})
}

// TestAFailureAtAJumpNamesTheStep is the error of a route whose first step
// refuses the login. It is a JumpError naming the step, and still reads as a
// refused login, so the connection gives up on it the way it does on one of
// the Host itself.
func TestAFailureAtAJumpNamesTheStep(t *testing.T) {
	jump := startJumpTestServer(t)
	target := startJumpTestServer(t)

	wrong := testClientConfig(localTestTimeout)
	wrong.Auth = []ssh.AuthMethod{ssh.Password("wrong")}

	_, _, err := dialSSHClient([]sshHop{
		{hostID: 2, addr: jump.addr, config: wrong},
		{hostID: 1, addr: target.addr, config: testClientConfig(localTestTimeout)},
	})

	var jumpErr *JumpError
	if !errors.As(err, &jumpErr) || jumpErr.Seq != 1 || jumpErr.HostID != 2 || jumpErr.Address != jump.addr {
		t.Fatalf("the error %v does not name the first step", err)
	}
	if !isAuthFailure(err) {
		t.Fatalf("the error %v does not read as a refused login", err)
	}
	if target.handshakes.Load() != 0 {
		t.Fatal("the Host at the end was reached behind a refused login")
	}
}

// TestAFailureAtAJumpIsGivenItsReason is the reason each way a step of the
// route fails comes out as: a login it refused, a key it presented that is not
// trusted, and a Host passed through that is not reached, whether it refuses
// the connection, takes it and sends nothing, or is behind a step that cannot
// open a channel to it. A failure at the Host at the end is not one of the
// route at all.
func TestAFailureAtAJumpIsGivenItsReason(t *testing.T) {
	jump := startJumpTestServer(t)
	target := startJumpTestServer(t)

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()

	wrong := testClientConfig(localTestTimeout)
	wrong.Auth = []ssh.AuthMethod{ssh.Password("wrong")}

	untrusted := testClientConfig(localTestTimeout)
	untrusted.HostKeyCallback = func(string, net.Addr, ssh.PublicKey) error {
		return &HostKeyError{Status: StatusHostKeyUnapproved}
	}

	short := testClientConfig(300 * time.Millisecond)

	cases := []struct {
		name  string
		route []sshHop
		want  jumpFailure
	}{
		{"a refused login", []sshHop{
			{hostID: 2, addr: jump.addr, config: wrong},
			{hostID: 1, addr: target.addr, config: testClientConfig(localTestTimeout)},
		}, jumpFailure{seq: 1, hostID: 2, reason: JumpReasonAuth}},
		{"a key that is not trusted", []sshHop{
			{hostID: 2, addr: jump.addr, config: untrusted},
			{hostID: 1, addr: target.addr, config: testClientConfig(localTestTimeout)},
		}, jumpFailure{seq: 1, hostID: 2, reason: JumpReasonHostKey}},
		{"a refused connection", []sshHop{
			{hostID: 2, addr: closedAddr, config: testClientConfig(localTestTimeout)},
			{hostID: 1, addr: target.addr, config: testClientConfig(localTestTimeout)},
		}, jumpFailure{seq: 1, hostID: 2, reason: JumpReasonDial}},
		{"a Host that sends nothing", []sshHop{
			{hostID: 2, addr: startSilentListener(t), config: short},
			{hostID: 1, addr: target.addr, config: testClientConfig(localTestTimeout)},
		}, jumpFailure{seq: 1, hostID: 2, reason: JumpReasonDial}},
		{"a channel that does not open", []sshHop{
			{hostID: 2, addr: jump.addr, config: testClientConfig(localTestTimeout)},
			{hostID: 3, addr: closedAddr, config: testClientConfig(localTestTimeout)},
			{hostID: 1, addr: target.addr, config: testClientConfig(localTestTimeout)},
		}, jumpFailure{seq: 2, hostID: 3, reason: JumpReasonDial}},
		{"the Host at the end", []sshHop{
			{hostID: 2, addr: jump.addr, config: testClientConfig(localTestTimeout)},
			{hostID: 1, addr: target.addr, config: wrong},
		}, jumpFailure{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, _, err := dialSSHClient(tc.route)
			if err == nil {
				_ = client.Close()
				t.Fatal("the route was connected over")
			}

			if got := jumpFailureOf(err); got != tc.want {
				t.Fatalf("the error %v reads as %+v, want %+v", err, got, tc.want)
			}
		})
	}

	if got := jumpFailureOf(nil); got != (jumpFailure{}) {
		t.Fatalf("no error reads as %+v, want nothing", got)
	}
}

// TestASocksProxyNamesTheJumpHostThatRefusedTheLogin is the SOCKS5 proxy of a
// Host reached through a Host that refuses the login. The proxy says which
// step it was and why, and once the password of the Host passed through is put
// right it connects and says nothing about the route any more.
func TestASocksProxyNamesTheJumpHostThatRefusedTheLogin(t *testing.T) {
	socksPort := freeDualStackPort(t)

	f := newJumpFixture(t, func(target, jump *models.Host) {
		target.SocksEnabled = true
		target.SocksPort = socksPort
		target.SocksBindScope = models.BindScopeLoopback
		jump.Password = "wrong" // hook:allow
	})

	f.reconcile(t)

	var state SocksState
	waitFor(t, localTestTimeout, "the SOCKS5 proxy to be refused", func() bool {
		var ok bool
		state, ok = f.m.SocksStatus(1)
		return ok && state.Status == localStatusError
	})
	if !strings.HasPrefix(state.LastError, f.jumpPrefix()+": ") {
		t.Fatalf("the proxy says %q, want it to start with %q", state.LastError, f.jumpPrefix())
	}
	assertJumpFailure(t, "the SOCKS5 proxy", state.JumpSeq, state.JumpHostID, state.JumpReason,
		jumpFailure{seq: 1, hostID: 2, reason: JumpReasonAuth})

	err := f.db.Model(&models.Host{}).Where("id = ?", 2).Update("password", "pass").Error // hook:allow
	if err != nil {
		t.Fatalf("failed to put the password of the Host passed through right: %v", err)
	}

	result := f.reconcile(t)
	if result.Restarted != 1 || result.Failed != 0 {
		t.Fatalf("the pass after the password was put right reported %+v, want the proxy restarted", result)
	}

	state = waitSocksConnected(t, f.m, 1)
	assertJumpFailure(t, "the connected SOCKS5 proxy", state.JumpSeq, state.JumpHostID, state.JumpReason,
		jumpFailure{})
}

// v3160Fingerprints are the fingerprints v3.16.0 gives the settings below,
// taken by running its connectionFingerprint, localForwardFingerprint and
// socksFingerprint over them. A Host with no jump route has to come out the
// same, or the first pass after an upgrade rebuilds every connection.
const (
	v3160TunnelFingerprint = "cf26ecea3ee17f6b7e731219447b37c0f8b6fcf46b562945057027249c62401a"
	v3160LocalFingerprint  = "a8ea1f70cf5e8374ef2d130a58497f0a7d41dc3326fc826b4d65efba6e3832ef"
	v3160SocksFingerprint  = "d04e7cb121ba961a5c5c40b3f3eda51d1db8ba09aa6b7d73e45675fa917d8f04"
)

func fingerprintTestSettings() (*models.Host, *models.ServicePort, *models.LocalForward, hostCreds) {
	host := &models.Host{ID: 1, Address: "192.0.2.1", Port: 22, User: "operator", HostKey: "ssh-ed25519 AAAA",
		Enabled: true, SocksEnabled: true, SocksPort: 1080, SocksBindScope: models.BindScopeLoopback,
		SocksAllowedSources: "127.0.0.0/8"}
	sp := &models.ServicePort{ID: 2, ServiceAddress: "198.51.100.2", ServicePort: 3306, LocalPort: 13306}
	lf := &models.LocalForward{HostID: 1, Number: 1, BindScope: models.BindScopeWildcard, LocalPort: 15432,
		TargetAddress: "198.51.100.5", TargetPort: 5432, AllowedSources: "192.0.2.0/24"}
	creds := hostCreds{password: "secret", privateKey: "key", passphrase: "phrase"} // hook:allow

	return host, sp, lf, creds
}

// TestAHostWithNoRouteKeepsItsFingerprint holds the fingerprints of a Host
// with no jump route to what v3.16.0 gave them.
func TestAHostWithNoRouteKeepsItsFingerprint(t *testing.T) {
	host, sp, lf, creds := fingerprintTestSettings()

	for _, tc := range []struct {
		name string
		got  connFingerprint
		want string
	}{
		{"tunnel", connectionFingerprint(host, sp, models.BindScopeLoopback, creds, nil), v3160TunnelFingerprint},
		{"local forward", localForwardFingerprint(host, lf, creds, nil), v3160LocalFingerprint},
		{"SOCKS5 proxy", socksFingerprint(host, creds, nil), v3160SocksFingerprint},
	} {
		if got := hex.EncodeToString(tc.got[:]); got != tc.want {
			t.Errorf("the %s fingerprint is %s, v3.16.0 gave %s", tc.name, got, tc.want)
		}
	}
}

// TestTheFingerprintFollowsTheHostsPassedThrough changes one thing about the
// Host passed through at a time, and every change has to reach the
// fingerprint of all three, as does putting a route on at all.
func TestTheFingerprintFollowsTheHostsPassedThrough(t *testing.T) {
	host, sp, lf, creds := fingerprintTestSettings()

	jumpHost := func() *models.Host {
		return &models.Host{ID: 2, Address: "192.0.2.2", Port: 22, User: "jumper", HostKey: "ssh-ed25519 BBBB",
			Enabled: true}
	}
	jumpCreds := hostCreds{password: "jump", privateKey: "jkey", passphrase: "jphrase"} // hook:allow

	fingerprints := func(hops []jumpHop) [3]connFingerprint {
		return [3]connFingerprint{
			connectionFingerprint(host, sp, models.BindScopeLoopback, creds, hops),
			localForwardFingerprint(host, lf, creds, hops),
			socksFingerprint(host, creds, hops),
		}
	}

	base := fingerprints([]jumpHop{{id: 2, host: jumpHost(), creds: jumpCreds}})

	if direct := fingerprints(nil); direct == base {
		t.Fatal("putting a route on did not change the fingerprints")
	}

	changes := map[string]func(host *models.Host, creds *hostCreds){
		"address":     func(h *models.Host, _ *hostCreds) { h.Address = "192.0.2.3" },
		"port":        func(h *models.Host, _ *hostCreds) { h.Port = 2222 },
		"user":        func(h *models.Host, _ *hostCreds) { h.User = "other" },
		"password":    func(_ *models.Host, c *hostCreds) { c.password = "other" },
		"private key": func(_ *models.Host, c *hostCreds) { c.privateKey = "other" },
		"passphrase":  func(_ *models.Host, c *hostCreds) { c.passphrase = "other" },
		"host key":    func(h *models.Host, _ *hostCreds) { h.HostKey = "ssh-ed25519 CCCC" },
		"enabled":     func(h *models.Host, _ *hostCreds) { h.Enabled = false },
	}

	for name, change := range changes {
		changed, changedCreds := jumpHost(), jumpCreds
		change(changed, &changedCreds)

		got := fingerprints([]jumpHop{{id: 2, host: changed, creds: changedCreds}})
		for i := range got {
			if got[i] == base[i] {
				t.Errorf("changing the %s of the Host passed through left fingerprint %d as it was", name, i)
			}
		}
	}

	other := fingerprints([]jumpHop{{id: 3, host: jumpHost(), creds: jumpCreds}})
	missing := fingerprints([]jumpHop{{id: 2}})
	longer := fingerprints([]jumpHop{{id: 2, host: jumpHost(), creds: jumpCreds}, {id: 4, host: jumpHost(),
		creds: jumpCreds}})
	for i := range base {
		if other[i] == base[i] || missing[i] == base[i] || longer[i] == base[i] {
			t.Errorf("a route through other Hosts left fingerprint %d as it was", i)
		}
	}
}
