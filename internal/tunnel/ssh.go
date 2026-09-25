package tunnel

import (
	"errors"
	"fmt"
	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// errConnectionClosed reports that the peer closed the tunnel connection. It is
// not a failure, but the Start loop still has to wait before reconnecting.
var errConnectionClosed = errors.New("connection closed")

// localPair is the two addresses a bind scope asks the forwarded port to be
// opened on, one per address family. A forward request carries one address, so
// a scope is two requests, and both of them belong to the one tunnel: the row
// stays the assignment somebody made rather than splitting into a line per
// family.
//
// It has a String of its own so that a log line about the tunnel names both.
// Naming one would say the tunnel reaches half of what it asked for.
type localPair struct {
	v4 *net.TCPAddr
	v6 *net.TCPAddr
}

func (p localPair) String() string {
	return p.v4.String() + " and " + p.v6.String()
}

// port is the local port of the pair. Both addresses carry the same one,
// because a scope is two addresses for the one service port.
func (p localPair) port() int {
	return p.v4.Port
}

type SSHTunnel struct {
	HostID *uint
	SPID   *uint
	// Local is the pair of addresses requested for the remote listeners. The
	// tcpip-forward reply carries a port and nothing else, so the SSH server
	// never confirms which address it bound, only that it bound something.
	Local  localPair
	Server *net.TCPAddr
	Remote *net.TCPAddr
	Config *ssh.ClientConfig
	// connFP is the fingerprint of the connection settings this tunnel was
	// built from. A reconcile pass compares it with the fingerprint of the
	// settings the tunnel should have. It is written before the tunnel is
	// registered with the manager and never again, so it needs no lock of its
	// own, and it is never formatted, so it cannot reach a log.
	connFP connFingerprint
	client *ssh.Client
	// clientConn is the connection client was built on. ssh.Client hides it,
	// and the monitor needs it to put a deadline on the keepalive it sends. It
	// is guarded by clientMu together with client, so a reader always gets the
	// connection that belongs to the client it read.
	clientConn net.Conn
	clientMu   sync.RWMutex
	// tunnelMu serializes the tunnel row the Start loop and the monitor both
	// write. It is taken before stopMu and never after it.
	tunnelMu  sync.Mutex
	done      chan bool
	isStopped bool
	stopMu    sync.Mutex
	logger    *zap.Logger
}

// NewSSHTunnel builds the tunnel of one assignment. It takes two local
// addresses rather than one because that is what a bind scope names, and both
// of them are asked for on the same connection.
func NewSSHTunnel(hostID, spID *uint, localV4Addr, localV6Addr, serverAddr, remoteAddr string,
	sshConfig *ssh.ClientConfig, logger *zap.Logger) (*SSHTunnel, error) {
	localV4, err := net.ResolveTCPAddr("tcp", localV4Addr)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve local address: %w", err)
	}

	localV6, err := net.ResolveTCPAddr("tcp", localV6Addr)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve local IPv6 address: %w", err)
	}

	server, err := net.ResolveTCPAddr("tcp", serverAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve server address: %w", err)
	}

	remote, err := net.ResolveTCPAddr("tcp", remoteAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve remote address: %w", err)
	}

	return &SSHTunnel{
		HostID: hostID,
		SPID:   spID,
		Local:  localPair{v4: localV4, v6: localV6},
		Server: server,
		Remote: remote,
		Config: sshConfig,
		done:   make(chan bool),
		logger: logger,
	}, nil
}

func (t *SSHTunnel) saveTunnelStatus(m *Manager, tunnel *models.Tunnel) {
	t.stopMu.Lock()
	if t.isStopped {
		t.stopMu.Unlock()
		return
	}
	t.stopMu.Unlock()

	err := m.db.Save(tunnel).Error
	if err != nil {
		m.logger.Error("failed to update tunnel connected status", logid.TunnelStatusSaveFailed.Field(), zap.Error(err))
	}
}

// markReconnecting reports that the tunnel is down and waiting for the next
// connection attempt. It reports whether the status was updated, which does not
// happen once Stop deleted the tunnel row.
func (t *SSHTunnel) markReconnecting(m *Manager, tunnel *models.Tunnel) bool {
	t.tunnelMu.Lock()
	defer t.tunnelMu.Unlock()

	t.stopMu.Lock()
	if t.isStopped {
		t.stopMu.Unlock()
		return false
	}
	t.stopMu.Unlock()

	tunnel.Status = "reconnecting"
	tunnel.RetryCount++
	t.saveTunnelStatus(m, tunnel)

	return true
}

// reconnect tears down the connection the caller observed. A connection that is
// no longer the current one is left alone, because the Start loop already
// replaced it and the tunnel is up again.
func (t *SSHTunnel) reconnect(m *Manager, tunnel *models.Tunnel, observed *ssh.Client) {
	t.clientMu.Lock()
	if observed == nil || t.client != observed {
		t.clientMu.Unlock()
		return
	}
	// Closing the client closes clientConn with it (ssh, connection.Close).
	_ = t.client.Close()
	t.client = nil
	t.clientConn = nil
	t.clientMu.Unlock()

	if !t.markReconnecting(m, tunnel) {
		return
	}

	t.logger.Info("closed current connection, waiting for the tunnel to be re-established",
		logid.TunnelReconnectWaiting.Field(),
		zap.String("local", t.Local.String()),
		zap.String("server", t.Server.String()),
		zap.String("remote", t.Remote.String()))
}

// minMonitorDialTimeout guards the derived dial timeout against a monitoring
// interval that is not a whole second. The configuration rejects an interval
// below one second, so it never applies to a running tunnel.
const minMonitorDialTimeout = 500 * time.Millisecond

// monitorDialTimeout returns how long the monitor waits for the TCP handshake
// with the SSH server. It is half of the monitoring interval, so that a server
// that stopped answering cannot hold one check for the whole interval and halve
// the monitoring rate, while a slow network still gets as much time to answer as
// the interval allows.
func monitorDialTimeout(monitoringIntervalSec int) time.Duration {
	interval := time.Duration(monitoringIntervalSec) * time.Second
	if interval <= minMonitorDialTimeout {
		return minMonitorDialTimeout
	}

	return interval / 2
}

// monitorKeepaliveTimeout returns how long the monitor waits for the reply to
// its keepalive. The dial that runs first may already have spent half of the
// interval, so the keepalive gets half of what is left. A check a stuck peer
// holds then still ends inside its own tick, which is what keeps the monitoring
// rate at the configured interval.
func monitorKeepaliveTimeout(monitoringIntervalSec int) time.Duration {
	return monitorDialTimeout(monitoringIntervalSec) / 2
}

// sendKeepalive asks the SSH server for a reply and gives up after timeout.
// SendRequest waits on a channel the connection's read loop fills and offers no
// way to stop waiting, so the deadline has to go on the connection underneath
// it. A peer that neither answers nor refuses would otherwise hold the call
// until TCP gives up retransmitting, which takes minutes.
func sendKeepalive(client *ssh.Client, conn net.Conn, timeout time.Duration) error {
	err := conn.SetDeadline(time.Now().Add(timeout))
	if err != nil {
		return fmt.Errorf("failed to set the keepalive deadline: %w", err)
	}
	defer func() {
		// An armed deadline would time out the forwarded traffic that follows.
		_ = conn.SetDeadline(time.Time{})
	}()

	_, _, err = client.SendRequest("keepalive@tunnel", true, nil)

	return err
}

func (t *SSHTunnel) monitorConnection(m *Manager, tunnel *models.Tunnel, stop <-chan struct{}) {
	ticker := time.NewTicker(time.Duration(m.monitoringIntervalSec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-t.done:
			return
		case <-stop:
			return
		case <-ticker.C:
			t.clientMu.RLock()
			client := t.client
			clientConn := t.clientConn
			t.clientMu.RUnlock()

			if client != nil {
				conn, err := net.DialTimeout("tcp", t.Server.String(),
					monitorDialTimeout(m.monitoringIntervalSec))
				if err != nil {
					t.logger.Warn("SSH connection lost, attempting reconnection",
						logid.TunnelServerUnreachable.Field(),
						zap.String("local", t.Local.String()),
						zap.String("server", t.Server.String()),
						zap.String("remote", t.Remote.String()),
						zap.Error(err))
					t.reconnect(m, tunnel, client)
					continue
				}
				_ = conn.Close()

				err = sendKeepalive(client, clientConn,
					monitorKeepaliveTimeout(m.monitoringIntervalSec))
				if err != nil {
					t.logger.Warn("SSH keepalive check failed, attempting reconnection",
						logid.TunnelKeepaliveFailed.Field(),
						zap.String("server", t.Server.String()),
						zap.Error(err))
					t.reconnect(m, tunnel, client)
				}
			}
		}
	}
}

// halfCloser is a connection whose writing side can be closed on its own. That
// is what carries the end of a stream to the peer while the other direction
// keeps running. Both connections forward joins have it: remoteConn comes from
// net.Dial and is a *net.TCPConn, and localConn is what the remote listener
// accepted, an *ssh.chanConn (ssh/tcpip.go, tcpListener.Accept) that promotes
// CloseWrite from the ssh.Channel it embeds (ssh/channel.go, Channel).
type halfCloser interface {
	CloseWrite() error
}

// forwardIdleTimeout bounds how long forward holds a connection on which
// nothing moves in either direction. It is not a response time: what travels
// here is whatever the forwarded service speaks, and a live session may well
// sit between two messages, so the bound has to be far longer than any such
// gap. It only exists because a peer that neither sends nor closes would
// otherwise pin this goroutine and both sockets for as long as the process
// runs. An hour is longer than any middlebox on the path keeps a silent flow
// alive (NAT and load balancer idle limits are counted in minutes), so a
// connection cut here is one the path had already dropped.
const forwardIdleTimeout = time.Hour

// forwardDialTimeout bounds the TCP handshake with the forwarded service.
//
// Without one, a service that answers a SYN with nothing at all is waited on
// for as long as the kernel retries, which is around two minutes on Linux. The
// client that opened the tunnel connection has given up long before that, and
// what it leaves behind is a goroutine and a socket per connection: a backend
// that a firewall drops silently, rather than refuses, is enough to pile those
// up. Every other dial in this package is already bounded, the SSH one by the
// client configuration and the monitor by its own interval, and this was the
// one that was not.
//
// Ten seconds is the same bound the SSH dial uses, so there is one number to
// reason about. The forwarded service is reachable from this process by the
// definition of what a tunnel is for, so ten seconds is not a network distance
// being allowed for but a backend that is not answering.
const forwardDialTimeout = 10 * time.Second

// The three readings models.Tunnel.ForwardReach carries. A forwarded port is
// reachable from here, is not, or has not been measured yet, and the third is
// a value of its own because a port nobody has tried must not read as one that
// was tried and answered.
const (
	forwardReachable    = "reachable"
	forwardUnreachable  = "unreachable"
	forwardReachUnknown = "unknown"
)

// The three readings models.Tunnel.OpenReach carries once a connection stands:
// the pair of addresses the scope named both opened, or only the IPv4 one did,
// or only the IPv6 one did. A connection on which neither opened writes none of
// them, because that is a tunnel that failed rather than one that reaches half
// of what was asked for.
const (
	openReachBoth = "both"
	openReachV4   = "ipv4"
	openReachV6   = "ipv6"
)

// errorKindForwardDenied is the one reading models.Tunnel.ErrorKind carries. It
// is the SSH server refusing to open the forwarded port, which is the failure
// the screen has somewhere to send the operator for.
const errorKindForwardDenied = "forward_denied"

// listenDeniedMessage is what x/crypto/ssh says when the server answers the
// request to open the port with a refusal (ssh/tcpip.go, Client.ListenTCP).
//
// It is compared as text because the library raises it with errors.New and
// exports nothing to compare against, so there is no sentinel and no type. That
// makes the comparison a thing that a new version of the library can quietly
// break, which is why it is written down once, here, rather than at the screen:
// a test holds this string to the library, and everything else reads the name
// this program gives it.
const listenDeniedMessage = "ssh: tcpip-forward request denied by peer"

// listenErrorKind names the failure of opening the forwarded port, for the one
// failure that is named. Everything else is left unnamed rather than guessed
// at: a write that failed on the way and a server that refused are not the same
// thing to tell an operator about.
func listenErrorKind(err error) string {
	if err != nil && strings.Contains(err.Error(), listenDeniedMessage) {
		return errorKindForwardDenied
	}

	return ""
}

// forwardProbeTimeout bounds the TCP handshake of the reachability probe, and
// it is there for the reason forwardDialTimeout above is.
//
// A port a firewall drops answers a SYN with nothing at all, and a dial with no
// bound on it then waits for the kernel to stop retransmitting, which is around
// two minutes on Linux. The probe holds a goroutine and a socket for as long as
// it waits and it is run again on every reconnect, so an address that never
// answers would pile those up on a tunnel that keeps dropping.
//
// Ten seconds is what forwardDialTimeout uses, so there is one number to reason
// about. The SSH server was reached a moment earlier over the same network, so
// what is being allowed for here is not a distance but a port that does not
// answer.
const forwardProbeTimeout = 10 * time.Second

// probeForwardReach reports whether the forwarded port answers a TCP
// connection opened from this process, and writes down a silence as the caller
// says a silence is worth here.
//
// An answer is the one piece of evidence this end can hold: the port carried a
// connection, so it is open at the address that was dialled. Nothing else here
// is evidence of anything. The reply to a tcpip-forward request carries a port
// and no address, so the SSH server never says which address it bound, and
// there is no shell on the far side to ask.
//
// What comes back is where the port was reached from, never why it was not.
// A server that bound the port to loopback alone and a firewall that drops the
// packet are the same silence seen from here, and reporting either as the cause
// would send the operator to fix a machine that is not the one at fault.
//
// The connection is closed as soon as it stands. The handshake is the whole
// measurement, and a probe left open is a socket held for the life of the
// tunnel, one more on every reconnect.
func probeForwardReach(address string, timeout time.Duration, silence string) string {
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return silence
	}
	_ = conn.Close()

	return forwardReachable
}

// forwardProbe is what the reachability probe of one connection dials and what
// its silence is worth. The two travel together because they are one decision:
// there is a single address this end can try, and whether nothing coming back
// from it says anything about the port depends on what was asked for there.
type forwardProbe struct {
	address string
	silence string
}

// forwardProbeAddress is where the forwarded port is tried from here: the
// machine the SSH server runs on, at the port the server confirmed for the
// forward.
//
// It is not the address the listener reports. That one is the address that was
// asked for, a wildcard or a loopback address, and neither is an address to
// dial from here: the wildcard is no address at all and the loopback one is
// this machine rather than the Host.
//
// It is the one address of the Host this program holds, so it carries one
// address family. The other family of the Host is not known here, and a port
// that was asked for on it cannot be tried at all.
func forwardProbeAddress(server *net.TCPAddr, port int) string {
	return net.JoinHostPort(server.IP.String(), strconv.Itoa(port))
}

// forwardProbeSilence is the reading a probe that gets no answer is written
// down as.
//
// A silence is a measurement only where an answer was to be expected. It says
// the forwarded port is not reachable at the address of the Host this program
// holds, which is what an operator has to be told about a tunnel that reads as
// connected. Where an answer was not to be expected it says nothing at all, and
// writing it down as a port that cannot be reached would put a failure on a
// tunnel that is doing what was asked of it.
//
// Two things make a silence worth nothing. The probe can only dial the address
// family of the Host address, so a scope whose request for that family was
// turned down was never confirmed for the family being tried. And a scope that
// asks for the loopback addresses asks for them on the Host, which nothing here
// can reach, however well the port is carrying traffic on that machine.
//
// The probe is run in both cases all the same, because an answer is evidence
// and a request that was turned down is not evidence of a port that is closed:
// an SSH server told to bind every interface opens both families on the first
// request and turns the second one down, and it binds every interface for a
// request that named the loopback address too.
func forwardProbeSilence(local localPair, server *net.TCPAddr, reach string) string {
	asked := local.v6
	answered := reach == openReachBoth || reach == openReachV6

	if server.IP.To4() != nil {
		asked = local.v4
		answered = reach == openReachBoth || reach == openReachV4
	}

	if !answered || asked.IP.IsLoopback() {
		return forwardReachUnknown
	}

	return forwardUnreachable
}

// recordForwardReach measures the forwarded port and writes the reading to the
// tunnel row.
//
// It runs beside the accept loop rather than in it. The probe waits out its
// whole timeout against an address that drops the packet, and the loop it would
// hold is the one that carries the forwarded traffic.
//
// A reading taken on a connection that is no longer the current one is dropped.
// The tunnel reconnected while the probe was waiting, a probe of its own is
// running for the new connection, and writing here would put the reading of a
// connection that is gone on the row of the one that replaced it.
func (t *SSHTunnel) recordForwardReach(m *Manager, tunnel *models.Tunnel, measured *ssh.Client, probe forwardProbe) {
	reach := probeForwardReach(probe.address, forwardProbeTimeout, probe.silence)

	t.clientMu.RLock()
	current := t.client
	t.clientMu.RUnlock()

	if current != measured {
		return
	}

	// The banner is taken under the same lock the reading is written under.
	// Read after it, it would be read while the Start loop may be writing the
	// banner of the connection that came next.
	t.tunnelMu.Lock()
	tunnel.ForwardReach = reach
	banner := tunnel.ServerBanner
	t.saveTunnelStatus(m, tunnel)
	t.tunnelMu.Unlock()

	if reach != forwardUnreachable {
		return
	}

	t.logger.Warn("the forwarded port did not answer a connection from here, so the tunnel is connected "+
		"but may not be usable. The SSH server may have bound the port to loopback alone, or something "+
		"on the way may be dropping it, and the two cannot be told apart from here, so check both. On the "+
		"SSH server it is the setting that opens a forwarded port to addresses other than loopback, "+
		"GatewayPorts in sshd_config for OpenSSH and the -a flag on the command line for Dropbear, and the "+
		"server has to be restarted for a change to it. Between here and the Host it is the firewall that "+
		"the port has to be open through",
		logid.TunnelForwardUnreachable.Field(),
		zap.String("probed", probe.address),
		zap.String("server_banner", banner),
		zap.String("local", t.Local.String()),
		zap.String("server", t.Server.String()),
		zap.String("remote", t.Remote.String()))
}

// countingReader counts what was read from src, which is how forward tells a
// connection that is idle from one that is merely slow. The count only has to
// change while bytes flow, so a plain atomic add is enough.
type countingReader struct {
	src   io.Reader
	moved *atomic.Int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	r.moved.Add(int64(n))

	return n, err
}

// isSelfClosed reports whether a copy ended because forward closed the
// connection under it. That is a teardown forward started itself, not a
// failure of a peer, and logging it would make every closed connection look
// like a broken one.
func isSelfClosed(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe)
}

// forward joins a connection the remote listener accepted to the service the
// tunnel points at and returns once both directions have ended. Waiting for
// both is what keeps an answer whole: a client that sent its request and then
// closed only its writing side is still waiting to receive, and returning on
// the first of the two copies would close the connection under that answer.
// idleTimeout bounds a connection on which nothing moves at all, see
// forwardIdleTimeout.
func (t *SSHTunnel) forward(localConn net.Conn, idleTimeout time.Duration) {
	defer func() {
		_ = localConn.Close()
	}()

	remoteConn, err := net.DialTimeout("tcp", t.Remote.String(), forwardDialTimeout)
	if err != nil {
		t.logger.Error("failed to dial remote service",
			logid.TunnelRemoteDialFailed.Field(),
			zap.String("local", t.Local.String()),
			zap.String("server", t.Server.String()),
			zap.String("remote", t.Remote.String()),
			zap.Error(err))
		return
	}
	defer func() {
		_ = remoteConn.Close()
	}()

	joinConns(localConn, remoteConn, idleTimeout, t.logger)
}

// joinConns carries bytes between the two connections until both directions
// have ended, the way forward describes, and is shared with the local forwards,
// which join the same two kinds of connection the other way round. It closes
// both where nothing more can be carried, and leaves closing them on the way out
// to the caller.
func joinConns(localConn, remoteConn net.Conn, idleTimeout time.Duration, logger *zap.Logger) {
	// closeBoth ends the connection in both directions. It is called where
	// nothing can be carried any further, and it is also what releases a copy
	// that is blocked in a read.
	closeBoth := func() {
		_ = localConn.Close()
		_ = remoteConn.Close()
	}

	var moved atomic.Int64
	ended := make(chan struct{}, 2)

	copyOneWay := func(dst, src net.Conn) {
		defer func() {
			ended <- struct{}{}
		}()

		_, err := io.Copy(dst, &countingReader{src: src, moved: &moved})
		if err != nil && !errors.Is(err, io.EOF) {
			if !isSelfClosed(err) {
				logger.Debug("copy error", logid.TunnelForwardCopyFailed.Field(), zap.Error(err))
			}

			// The stream broke. What is left of it cannot be delivered, and
			// the other direction has no peer left to deliver it to either.
			closeBoth()

			return
		}

		// src reached the end of its stream. Half-closing dst passes that end
		// on and leaves the other direction running, which is what a client
		// that half-closed after its request needs to receive the answer.
		// It takes both sides for that: a src that cannot be half-closed can
		// only have closed the whole connection, and a dst that cannot be
		// half-closed has no way of being told the stream ended other than
		// being closed.
		dstHalf, dstCanHalfClose := dst.(halfCloser)
		_, srcCanHalfClose := src.(halfCloser)
		if !dstCanHalfClose || !srcCanHalfClose {
			closeBoth()

			return
		}

		_ = dstHalf.CloseWrite()
	}

	go copyOneWay(localConn, remoteConn)
	go copyOneWay(remoteConn, localConn)

	timer := time.NewTimer(idleTimeout)
	defer timer.Stop()

	seen := moved.Load()
	for left := 2; left > 0; {
		select {
		case <-ended:
			left--
		case <-timer.C:
			if now := moved.Load(); now != seen {
				seen = now
				timer.Reset(idleTimeout)

				continue
			}

			// Nothing was carried for a whole timeout, so both copies are
			// waiting on peers that send nothing and close nothing. Closing
			// releases them, and the loop then collects both.
			closeBoth()
		}
	}
}

// dialSSH connects to the SSH server and hands back the net.Conn the client was
// built on, which ssh.Dial does not expose. It does what ssh.Dial does
// (ssh/client.go, Dial): dial with the timeout from the configuration, run the
// handshake, and wrap the result. NewClientConn closes the connection itself
// when the handshake fails, so a failure here leaves nothing open.
func (t *SSHTunnel) dialSSH() (*ssh.Client, net.Conn, error) {
	return dialSSHClient(t.Server.String(), t.Config)
}

// dialSSHClient is dialSSH for any server and configuration, so the local
// forwards connect the same way the tunnels do.
func dialSSHClient(addr string, config *ssh.ClientConfig) (*ssh.Client, net.Conn, error) {
	conn, err := net.DialTimeout("tcp", addr, config.Timeout)
	if err != nil {
		return nil, nil, err
	}

	// ClientConfig.Timeout bounds only the TCP connect (ssh/client.go, Dial),
	// so a server that accepts and never sends its banner would hold the
	// handshake, and with it the retries of the tunnel, forever.
	if config.Timeout > 0 {
		if err := conn.SetDeadline(time.Now().Add(config.Timeout)); err != nil {
			_ = conn.Close()
			return nil, nil, err
		}
	}

	c, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		return nil, nil, err
	}

	// The deadline was for the handshake. Left in place it would cut the
	// tunnel traffic once the timeout passes.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = c.Close()
		return nil, nil, err
	}

	return ssh.NewClient(c, chans, reqs), conn, nil
}

// connectFailureStatus is the status a connection that was not made leaves on
// the tunnel row. A host key that was refused is a state of its own rather
// than a plain error: it is the one failure that is answered on the screen
// instead of being fixed on the Host, and which of the two refusals it was
// decides what the operator is shown and what is asked of them.
func connectFailureStatus(err error) string {
	if refusal := hostKeyRefusal(err); refusal != nil {
		return refusal.Status
	}

	return "error"
}

func (t *SSHTunnel) establishConnection(m *Manager, tunnel *models.Tunnel) error {
	client, clientConn, err := t.dialSSH()
	if err != nil {
		m.logger.Error("failed to establish SSH connection",
			logid.TunnelSshConnectFailed.Field(),
			zap.String("local", t.Local.String()),
			zap.String("server", t.Server.String()),
			zap.String("remote", t.Remote.String()), zap.Error(err))

		t.tunnelMu.Lock()
		tunnel.Status = connectFailureStatus(err)
		tunnel.LastError = err.Error()
		t.saveTunnelStatus(m, tunnel)
		t.tunnelMu.Unlock()

		return fmt.Errorf("failed to establish SSH connection: %w", err)
	}

	opened, err := t.openForwards(client)
	if err != nil {
		// The client is kept on the tunnel only once a forward is open, so on
		// this way out nothing else would ever close it. Closing it closes
		// clientConn as well, the connection it was built on.
		_ = client.Close()

		m.logger.Error("failed to start remote listener",
			logid.TunnelRemoteListenerFailed.Field(),
			zap.String("local", t.Local.String()),
			zap.String("server", t.Server.String()),
			zap.String("remote", t.Remote.String()), zap.Error(err))

		t.tunnelMu.Lock()
		tunnel.Status = "error"
		tunnel.LastError = err.Error()
		tunnel.ErrorKind = listenErrorKind(err)
		// Nothing of the pair is open, so what the last connection opened is
		// no longer true of this one. It goes back to the empty value rather
		// than staying at what it was, or a tunnel that is now failing would
		// go on reporting the reach of a connection that is gone.
		tunnel.OpenReach = ""
		// What the Host said was listening was said about a port that is not
		// open now, so it goes with it.
		tunnel.ListenAddresses = ""
		// The banner is written on the way out as well as on the way in. What
		// the screen says about a refusal differs by which server refused, and
		// the handshake is behind us by the time we are here, so the one thing
		// that tells them apart is known and would otherwise be thrown away.
		tunnel.ServerBanner = string(client.ServerVersion())
		t.saveTunnelStatus(m, tunnel)
		t.tunnelMu.Unlock()

		return fmt.Errorf("failed to start remote listener: %w", err)
	}
	defer opened.close()

	t.clientMu.Lock()
	t.client = client
	t.clientConn = clientConn
	t.clientMu.Unlock()

	boundPort := opened.confirmedPort(t.Local.port())

	t.tunnelMu.Lock()
	tunnel.Local = opened.shown()
	tunnel.Status = "connected"
	tunnel.RetryCount = 0
	tunnel.LastError = ""
	tunnel.ErrorKind = ""
	tunnel.LastConnectedAt = time.Now()
	tunnel.ServerBanner = string(client.ServerVersion())
	// What the last connection measured says nothing about this one, which may
	// be to a server that was reconfigured in between, so the reading goes back
	// to unknown until the probe below answers for the connection that is up.
	tunnel.ForwardReach = forwardReachUnknown
	// Which halves of the pair opened is a fact about this connection for the
	// same reason, and it was settled a moment ago for this one.
	tunnel.OpenReach = opened.reach
	// And so is what the Host has listening on the port, which this connection
	// has not asked about yet. It goes back to the empty value, which reads as
	// not known, rather than staying at what the last connection was told.
	tunnel.ListenAddresses = ""
	t.saveTunnelStatus(m, tunnel)
	t.tunnelMu.Unlock()

	t.logger.Info("tunnel connected successfully",
		logid.TunnelConnected.Field(),
		zap.String("local", t.Local.String()),
		zap.String("open_reach", opened.reach),
		zap.String("server", t.Server.String()),
		zap.String("remote", t.Remote.String()))

	// Measured here and not on every pass. What decides it is the
	// configuration of the SSH server, which does not change under a
	// connection that stands, while a probe per status read would be one
	// connection per tunnel per reader.
	go t.recordForwardReach(m, tunnel, client, forwardProbe{
		address: forwardProbeAddress(t.Server, boundPort),
		silence: forwardProbeSilence(t.Local, t.Server, opened.reach),
	})

	// Asked here and not on every pass for the same reason, and on a goroutine
	// of its own rather than after the probe so that neither waits out the
	// bound of the other. What the two write is a field each, under the lock
	// the row is written under.
	go t.recordListenAddresses(m, tunnel, client, boundPort)

	return t.acceptForwards(m, tunnel, client, opened)
}

// openForwards asks the SSH server to open both addresses of the bind scope
// and reports which of them it did.
//
// Both are asked for on the one connection, because they are the one
// assignment. The far side answers each request on its own and may refuse one
// of them, and a refusal of one is not a failure of the tunnel: what opened
// carries traffic, and what did not is reported so that whoever chose the
// scope can see they got half of it.
//
// Both being refused is a failure, and the error names both refusals. The two
// cannot be reported apart: the row holds one LastError, and a caller shown
// only the first would go and look at the address family that may not be the
// one at fault.
func (t *SSHTunnel) openForwards(client *ssh.Client) (*openForwards, error) {
	listenerV4, errV4 := client.Listen("tcp", t.Local.v4.String())
	listenerV6, errV6 := client.Listen("tcp", t.Local.v6.String())

	switch {
	case errV4 != nil && errV6 != nil:
		return nil, fmt.Errorf("the SSH server opened neither address of the bind scope (%s: %v; %s: %v)",
			t.Local.v4, errV4, t.Local.v6, errV6)
	case errV6 != nil:
		return &openForwards{v4: listenerV4, reach: openReachV4, refused: errV6}, nil
	case errV4 != nil:
		return &openForwards{v6: listenerV6, reach: openReachV6, refused: errV4}, nil
	}

	return &openForwards{v4: listenerV4, v6: listenerV6, reach: openReachBoth}, nil
}

// openForwards is what came of asking for the two addresses of a bind scope.
type openForwards struct {
	// v4 and v6 are the forwards that opened. For a remote forward either is
	// nil when the SSH server refused that address. For a local forward or a
	// SOCKS5 proxy either is nil when this machine could not open it, except
	// on Windows, where the wildcard scope is opened as one dual-stack socket
	// on [::] held in v6: v4 is then nil although IPv4 is reached, and reach
	// is openReachBoth (local_listen_windows.go). They are never both nil: a
	// connection on which neither opened is a failure and never reaches here.
	v4 net.Listener
	v6 net.Listener
	// reach is what models.Tunnel.OpenReach is left at, one of openReachBoth,
	// openReachV4 and openReachV6.
	reach string
	// refused is why the address that did not open was not opened, as the SSH
	// server said it for a remote forward and as this machine said it for a
	// local one, and is nil when both opened. It is kept for the line that is logged
	// about it rather than for the row, because a tunnel that carries traffic
	// over one family is connected and not in error.
	refused error
	// closeOnce guards the forwards against being closed twice. The connection
	// closes them on the way out and the accept loops close them to end each
	// other, and the two orders overlap.
	closeOnce sync.Once
}

// listeners returns the forwards that opened, in the order their addresses were
// asked for.
func (o *openForwards) listeners() []net.Listener {
	opened := make([]net.Listener, 0, 2)
	for _, listener := range []net.Listener{o.v4, o.v6} {
		if listener != nil {
			opened = append(opened, listener)
		}
	}

	return opened
}

// shown returns the address the tunnel row carries, which is the one to
// connect to. Both addresses of a pair are open in the ordinary case and only
// one of them can be in the row, so the IPv4 one is preferred: that is the
// address every row held before a scope named two, and a screen reading the
// row goes on seeing what it saw. When only the IPv6 address opened it is that
// one, because the row has to name an address that answers.
//
// It is the address the listener reports rather than the one that was asked
// for, since that is the requested address with the port the SSH server
// confirmed (x/crypto/ssh, tcpListener.Addr), and the port is the part a
// request for port 0 does not know in advance.
func (o *openForwards) shown() string {
	return o.listeners()[0].Addr().String()
}

// confirmedPort returns the port the SSH server bound the forwards on, falling
// back to requested for a listener that reports an address of another kind.
//
// Only the port of the reported address is known to be real: the address
// itself is the one that was asked for, which the reply to a tcpip-forward
// request never confirms. A listener that reports anything but a TCP address
// leaves the port that was asked to be forwarded, which is the port the server
// confirmed in every case but a request for port 0.
func (o *openForwards) confirmedPort(requested int) int {
	if bound, ok := o.listeners()[0].Addr().(*net.TCPAddr); ok {
		return bound.Port
	}

	return requested
}

func (o *openForwards) close() {
	o.closeOnce.Do(func() {
		for _, listener := range o.listeners() {
			_ = listener.Close()
		}
	})
}

// acceptForwards carries what the forwards of this connection accept, and
// returns when the first of them ends.
//
// The first to end ends the connection rather than leaving the other running.
// Both forwards belong to the one assignment and to the one SSH connection, so
// a forward that ended on its own leaves the tunnel reaching half of what it
// was built for, with nothing that would ever open the other half again short
// of connecting anew. What ended the first is what the caller is told, and the
// second ends with it: closing a forward ends its accept with io.EOF
// (x/crypto/ssh, forwardList.remove closing the channel tcpListener.Accept
// reads).
func (t *SSHTunnel) acceptForwards(m *Manager, tunnel *models.Tunnel, client *ssh.Client, opened *openForwards) error {
	listeners := opened.listeners()

	// Buffered for every loop, so the ones that are not read from still end.
	ends := make(chan error, len(listeners))

	var accepting sync.WaitGroup

	for _, listener := range listeners {
		accepting.Add(1)

		go func(listener net.Listener) {
			defer accepting.Done()

			ends <- t.acceptForward(listener)
		}(listener)
	}

	err := <-ends

	opened.close()
	accepting.Wait()

	if errors.Is(err, io.EOF) {
		t.logger.Info("connection closed",
			logid.TunnelConnectionClosed.Field(),
			zap.String("local", t.Local.String()),
			zap.String("server", t.Server.String()),
			zap.String("remote", t.Remote.String()))

		// Drop the dead client, or the monitor keeps checking it and
		// tears down the connection the Start loop establishes next.
		//
		// A client that is no longer the current one was dropped by
		// reconnect, which already reported the tunnel as reconnecting,
		// and reporting it here again would count one drop twice.
		t.clientMu.Lock()
		current := t.client == client
		if current {
			_ = t.client.Close()
			t.client = nil
			t.clientConn = nil
		}
		t.clientMu.Unlock()

		if current {
			t.markReconnecting(m, tunnel)
		}

		return errConnectionClosed
	}

	m.logger.Error("listener accept error",
		logid.TunnelListenerAcceptFailed.Field(),
		zap.String("local", t.Local.String()),
		zap.String("server", t.Server.String()),
		zap.String("remote", t.Remote.String()), zap.Error(err))

	return fmt.Errorf("listener accept error: %w", err)
}

// acceptForward carries the connections one forward accepts and returns what
// ended it.
func (t *SSHTunnel) acceptForward(listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}

		go t.forward(conn, forwardIdleTimeout)
	}
}

// authFailureMessage is what golang.org/x/crypto/ssh reports when the server
// refused every configured authentication method. The library builds it with
// fmt.Errorf and offers no error type or sentinel to match on, so the text is
// the only handle there is (ssh/client_auth.go, clientAuthenticate).
// TestAuthFailureIsRecognized fails if a library update rewords it.
const authFailureMessage = "ssh: unable to authenticate"

// isAuthFailure reports whether the server refused the credentials of the
// tunnel. Retrying such a connection never succeeds.
func isAuthFailure(err error) bool {
	if err == nil {
		return false
	}

	return strings.Contains(err.Error(), authFailureMessage)
}

// waitBeforeRetry waits for the retry interval and reports whether the tunnel
// should keep running.
func (t *SSHTunnel) waitBeforeRetry(m *Manager) bool {
	timer := time.NewTimer(time.Duration(m.monitoringIntervalSec) * time.Second)
	defer timer.Stop()

	select {
	case <-t.done:
		return false
	case <-timer.C:
		return true
	}
}

func (t *SSHTunnel) Start(m *Manager, tunnel *models.Tunnel) {
	t.logger.Info("attempting to start tunnel",
		logid.TunnelStarting.Field(),
		zap.String("local", t.Local.String()),
		zap.String("server", t.Server.String()),
		zap.String("remote", t.Remote.String()))

	stopMonitor := make(chan struct{})
	var monitorWg sync.WaitGroup

	monitorWg.Add(1)
	go func() {
		defer monitorWg.Done()
		t.monitorConnection(m, tunnel, stopMonitor)
	}()

	defer func() {
		close(stopMonitor)
		monitorWg.Wait()
	}()

	for {
		select {
		case <-t.done:
			return
		default:
			t.stopMu.Lock()
			if t.isStopped {
				t.stopMu.Unlock()
				return
			}
			t.stopMu.Unlock()

			err := t.establishConnection(m, tunnel)
			if err != nil {
				if errors.Is(err, errConnectionClosed) {
					if !t.waitBeforeRetry(m) {
						return
					}
					continue
				}

				// Credentials the server refused and a host key that was not
				// approved are both refusals that no number of attempts turns
				// into a connection. The first needs the credentials of the
				// Host changed and the second needs a person to compare a
				// fingerprint, and neither happens because a tunnel tried
				// again. Retrying would write a line every interval, for
				// every tunnel of that Host, for as long as the process runs,
				// and the log an operator has to read to find out why would
				// be the thing burying it.
				//
				// Giving up does not leave the tunnel unreachable. Both of
				// those changes are changes to the connection settings, so a
				// reconcile pass sees a fingerprint that no longer matches
				// and builds the tunnel again, and until then the tunnel row
				// says which refusal it was.
				if isAuthFailure(err) || hostKeyRefusal(err) != nil {
					t.logger.Error("connection failed",
						logid.TunnelConnectFailedGivingUp.Field(),
						zap.String("local", t.Local.String()),
						zap.String("server", t.Server.String()),
						zap.String("remote", t.Remote.String()),
						zap.Error(err))
					return
				}

				t.logger.Error("connection failed, retrying in "+strconv.Itoa(m.monitoringIntervalSec)+" seconds",
					logid.TunnelConnectFailedRetrying.Field(),
					zap.Int("retry_in_sec", m.monitoringIntervalSec),
					zap.String("local", t.Local.String()),
					zap.String("server", t.Server.String()),
					zap.String("remote", t.Remote.String()),
					zap.Error(err))

				if !t.waitBeforeRetry(m) {
					return
				}

				t.tunnelMu.Lock()
				tunnel.Status = "reconnecting"
				tunnel.RetryCount++
				t.saveTunnelStatus(m, tunnel)
				t.tunnelMu.Unlock()
			}
		}
	}
}

func (t *SSHTunnel) Stop(m *Manager) error {
	t.tunnelMu.Lock()
	defer t.tunnelMu.Unlock()

	t.stopMu.Lock()
	if t.isStopped {
		t.stopMu.Unlock()
		return nil
	}

	defer func() {
		t.isStopped = true
		t.stopMu.Unlock()
	}()

	close(t.done)

	t.clientMu.Lock()
	if t.client != nil {
		_ = t.client.Close()
		t.client = nil
		t.clientConn = nil
	}
	t.clientMu.Unlock()

	err := m.db.Where("host_id = ? and sp_id = ?", t.HostID, t.SPID).
		Delete(&models.Tunnel{}).Error
	if err != nil {
		return fmt.Errorf("failed to delete tunnel: %w", err)
	}

	return nil
}
