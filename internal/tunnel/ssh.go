package tunnel

import (
	"errors"
	"fmt"
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

type SSHTunnel struct {
	HostID *uint
	SPID   *uint
	// Local is the address requested for the remote listener. The
	// tcpip-forward reply carries a port and nothing else, so the SSH server
	// never confirms which address it bound.
	Local  *net.TCPAddr
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

func NewSSHTunnel(hostID, spID *uint, localAddr, serverAddr, remoteAddr string, sshConfig *ssh.ClientConfig, logger *zap.Logger) (*SSHTunnel, error) {
	local, err := net.ResolveTCPAddr("tcp", localAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve local address: %w", err)
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
		Local:  local,
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
		m.logger.Error("failed to update tunnel connected status", zap.Error(err))
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
// connection opened from this process.
//
// That is the whole of what can be measured from this end. The reply to a
// tcpip-forward request carries a port and no address, so the SSH server never
// says which address it bound, and there is no shell on the far side to ask.
//
// What comes back is where the port was reached from, never why it was not.
// A server that bound the port to loopback alone and a firewall that drops the
// packet are the same refusal seen from here, and reporting either as the cause
// would send the operator to fix a machine that is not the one at fault.
//
// The connection is closed as soon as it stands. The handshake is the whole
// measurement, and a probe left open is a socket held for the life of the
// tunnel, one more on every reconnect.
func probeForwardReach(address string, timeout time.Duration) string {
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return forwardUnreachable
	}
	_ = conn.Close()

	return forwardReachable
}

// forwardProbeAddress is where the forwarded port is tried from here: the
// machine the SSH server runs on, at the port the server confirmed for the
// forward.
//
// It is not the address the listener reports. That one is the address that was
// asked for, a wildcard 0.0.0.0, which is not an address to dial and would only
// ever reach this machine.
func forwardProbeAddress(server *net.TCPAddr, port int) string {
	return net.JoinHostPort(server.IP.String(), strconv.Itoa(port))
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
func (t *SSHTunnel) recordForwardReach(m *Manager, tunnel *models.Tunnel, measured *ssh.Client, address string) {
	reach := probeForwardReach(address, forwardProbeTimeout)

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
		zap.String("probed", address),
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
			zap.String("local", t.Local.String()),
			zap.String("server", t.Server.String()),
			zap.String("remote", t.Remote.String()),
			zap.Error(err))
		return
	}
	defer func() {
		_ = remoteConn.Close()
	}()

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
				t.logger.Debug("copy error", zap.Error(err))
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
	addr := t.Server.String()

	conn, err := net.DialTimeout("tcp", addr, t.Config.Timeout)
	if err != nil {
		return nil, nil, err
	}

	c, chans, reqs, err := ssh.NewClientConn(conn, addr, t.Config)
	if err != nil {
		return nil, nil, err
	}

	return ssh.NewClient(c, chans, reqs), conn, nil
}

func (t *SSHTunnel) establishConnection(m *Manager, tunnel *models.Tunnel) error {
	client, clientConn, err := t.dialSSH()
	if err != nil {
		m.logger.Error("failed to establish SSH connection",
			zap.String("local", t.Local.String()),
			zap.String("server", t.Server.String()),
			zap.String("remote", t.Remote.String()), zap.Error(err))

		t.tunnelMu.Lock()
		tunnel.Status = "error"
		tunnel.LastError = err.Error()
		t.saveTunnelStatus(m, tunnel)
		t.tunnelMu.Unlock()

		return fmt.Errorf("failed to establish SSH connection: %w", err)
	}

	listener, err := client.Listen("tcp", t.Local.String())
	if err != nil {
		m.logger.Error("failed to start remote listener",
			zap.String("local", t.Local.String()),
			zap.String("server", t.Server.String()),
			zap.String("remote", t.Remote.String()), zap.Error(err))

		t.tunnelMu.Lock()
		tunnel.Status = "error"
		tunnel.LastError = err.Error()
		t.saveTunnelStatus(m, tunnel)
		t.tunnelMu.Unlock()

		return fmt.Errorf("failed to start remote listener: %w", err)
	}
	defer func() {
		_ = listener.Close()
	}()

	t.clientMu.Lock()
	t.client = client
	t.clientConn = clientConn
	t.clientMu.Unlock()

	// Addr reports the requested address with the port the server confirmed,
	// so only the port is known to be real here. That port is what the probe
	// below is aimed at, and it is taken out of the address the library builds
	// (x/crypto/ssh, tcpListener.Addr). A listener that reports anything else
	// leaves the port that was asked to be forwarded, which is the port the
	// server confirmed in every case but a request for port 0.
	localAddr := listener.Addr().String()

	boundPort := t.Local.Port
	if bound, ok := listener.Addr().(*net.TCPAddr); ok {
		boundPort = bound.Port
	}

	if t.Local.IP.IsUnspecified() {
		t.logger.Info("a wildcard local address was requested, and the address here is the one that was asked for: "+
			"the SSH server opens the listener and never says which address it bound, so whether the port is on "+
			"every address or on loopback alone is not known from this line. The probe that follows says whether "+
			"it answered, and if it did not, what to change is the setting that opens a forwarded port on the "+
			"SSH server or the firewall on the way",
			zap.String("local", localAddr),
			zap.String("server", t.Server.String()),
			zap.String("remote", t.Remote.String()))
	}

	t.tunnelMu.Lock()
	tunnel.Local = localAddr
	tunnel.Status = "connected"
	tunnel.RetryCount = 0
	tunnel.LastError = ""
	tunnel.LastConnectedAt = time.Now()
	tunnel.ServerBanner = string(client.ServerVersion())
	// What the last connection measured says nothing about this one, which may
	// be to a server that was reconfigured in between, so the reading goes back
	// to unknown until the probe below answers for the connection that is up.
	tunnel.ForwardReach = forwardReachUnknown
	t.saveTunnelStatus(m, tunnel)
	t.tunnelMu.Unlock()

	t.logger.Info("tunnel connected successfully",
		zap.String("local", t.Local.String()),
		zap.String("server", t.Server.String()),
		zap.String("remote", t.Remote.String()))

	// Measured here and not on every pass. What decides it is the
	// configuration of the SSH server, which does not change under a
	// connection that stands, while a probe per status read would be one
	// connection per tunnel per reader.
	go t.recordForwardReach(m, tunnel, client, forwardProbeAddress(t.Server, boundPort))

	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, io.EOF) {
				t.logger.Info("connection closed",
					zap.String("local", t.Local.String()),
					zap.String("server", t.Server.String()),
					zap.String("remote", t.Remote.String()))

				// Drop the dead client, or the monitor keeps checking it and
				// tears down the connection the Start loop establishes next.
				t.clientMu.Lock()
				if t.client == client {
					_ = t.client.Close()
					t.client = nil
					t.clientConn = nil
				}
				t.clientMu.Unlock()

				t.markReconnecting(m, tunnel)

				return errConnectionClosed
			}

			m.logger.Error("listener accept error",
				zap.String("local", t.Local.String()),
				zap.String("server", t.Server.String()),
				zap.String("remote", t.Remote.String()), zap.Error(err))

			return fmt.Errorf("listener accept error: %w", err)
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

				if isAuthFailure(err) {
					t.logger.Error("connection failed",
						zap.String("local", t.Local.String()),
						zap.String("server", t.Server.String()),
						zap.String("remote", t.Remote.String()),
						zap.Error(err))
					return
				}

				t.logger.Error("connection failed, retrying in "+strconv.Itoa(m.monitoringIntervalSec)+" seconds",
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
