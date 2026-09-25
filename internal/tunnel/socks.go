package tunnel

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
)

// The parts of RFC 1928 the proxy speaks: version 5, no authentication, the
// CONNECT command, and the three address types.
const (
	socksVersion = 0x05

	socksMethodNoAuth       = 0x00
	socksMethodNoAcceptable = 0xFF

	socksCmdConnect = 0x01

	socksAtypIPv4   = 0x01
	socksAtypDomain = 0x03
	socksAtypIPv6   = 0x04

	socksRepSucceeded           = 0x00
	socksRepGeneralFailure      = 0x01
	socksRepNotAllowed          = 0x02
	socksRepNetworkUnreachable  = 0x03
	socksRepHostUnreachable     = 0x04
	socksRepConnectionRefused   = 0x05
	socksRepCommandNotSupported = 0x07
	socksRepAddressNotSupported = 0x08
)

// socksHandshakeTimeout bounds reading the greeting and the request, and
// writing the answers to them. A client that connects and sends nothing would
// otherwise hold a goroutine and a socket for as long as it cares to.
const socksHandshakeTimeout = 10 * time.Second

// socksRefusalLogInterval is how often a refused source is logged at most per
// proxy. A proxy on the wildcard is reachable by whatever scans the network,
// and a line per attempt would bury everything else in the file.
const socksRefusalLogInterval = time.Minute

// SocksState is what a running SOCKS5 proxy reports about itself. It is kept
// in memory for the reasons LocalForwardState is, and carries the same
// statuses.
type SocksState struct {
	Status          string    `json:"status"`
	LastError       string    `json:"last_error"`
	RetryCount      int       `json:"retry_count"`
	LastConnectedAt time.Time `json:"last_connected_at"`
}

// ParseAllowedSources reads models.Host.SocksAllowedSources: addresses and
// CIDR blocks separated by commas or white space. An address stands for the
// block holding that address alone. An empty list lets every source in, and
// comes back as nil.
//
// IPv4 is kept as IPv4 whichever way it is written, so that ::ffff:192.0.2.1
// and 192.0.2.1 name the same source, and a source compared against the list
// is unmapped the same way.
func ParseAllowedSources(s string) ([]netip.Prefix, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})

	var prefixes []netip.Prefix
	for _, field := range fields {
		if strings.Contains(field, "/") {
			prefix, err := netip.ParsePrefix(field)
			if err != nil {
				return nil, fmt.Errorf("%q is not a CIDR block: %w", field, err)
			}

			addr := prefix.Addr()
			bits := prefix.Bits()
			if addr.Is4In6() {
				if bits < 96 {
					return nil, fmt.Errorf("%q is an IPv4-mapped block shorter than /96", field)
				}
				addr = addr.Unmap()
				bits -= 96
			}

			prefixes = append(prefixes, netip.PrefixFrom(addr, bits).Masked())
			continue
		}

		addr, err := netip.ParseAddr(field)
		if err != nil {
			return nil, fmt.Errorf("%q is neither an IP address nor a CIDR block", field)
		}
		if addr.Zone() != "" {
			return nil, fmt.Errorf("%q carries a zone, which a source is never compared with", field)
		}

		addr = addr.Unmap()
		prefixes = append(prefixes, netip.PrefixFrom(addr, addr.BitLen()))
	}

	return prefixes, nil
}

// sourceAllowed reports whether a connection from remote may use a proxy
// whose allowed sources are allowed. A remote that is not a TCP address with
// an IP is never allowed by a list, since there is nothing to compare.
func sourceAllowed(allowed []netip.Prefix, remote net.Addr) bool {
	if len(allowed) == 0 {
		return true
	}

	tcp, ok := remote.(*net.TCPAddr)
	if !ok {
		return false
	}

	addr, ok := netip.AddrFromSlice(tcp.IP)
	if !ok {
		return false
	}
	addr = addr.Unmap()

	for _, prefix := range allowed {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

// errSocksRefused is what readSocksRequest returns once it has answered the
// client with a refusal, so the caller only closes the connection.
var errSocksRefused = errors.New("SOCKS5 request refused")

// readSocksRequest reads the greeting and the request of one client and
// returns the target it asked for, written as host:port. A domain name is
// returned as it was sent, so the Host resolves it. What it refuses it answers
// with the reply RFC 1928 has for it before returning; what it cannot read it
// returns without answering.
func readSocksRequest(conn io.ReadWriter) (string, error) {
	var head [2]byte
	_, err := io.ReadFull(conn, head[:])
	if err != nil {
		return "", fmt.Errorf("failed to read the greeting: %w", err)
	}
	if head[0] != socksVersion {
		return "", fmt.Errorf("the client speaks SOCKS version %d", head[0])
	}

	methods := make([]byte, head[1])
	_, err = io.ReadFull(conn, methods)
	if err != nil {
		return "", fmt.Errorf("failed to read the authentication methods: %w", err)
	}

	method := byte(socksMethodNoAcceptable)
	for _, offered := range methods {
		if offered == socksMethodNoAuth {
			method = socksMethodNoAuth
			break
		}
	}

	_, err = conn.Write([]byte{socksVersion, method})
	if err != nil {
		return "", fmt.Errorf("failed to answer the greeting: %w", err)
	}
	if method == socksMethodNoAcceptable {
		return "", fmt.Errorf("%w: the client offered no method without authentication", errSocksRefused)
	}

	var request [4]byte
	_, err = io.ReadFull(conn, request[:])
	if err != nil {
		return "", fmt.Errorf("failed to read the request: %w", err)
	}
	if request[0] != socksVersion {
		return "", fmt.Errorf("the request is of SOCKS version %d", request[0])
	}

	var host string
	switch request[3] {
	case socksAtypIPv4:
		var ip [4]byte
		_, err = io.ReadFull(conn, ip[:])
		host = netip.AddrFrom4(ip).String()
	case socksAtypIPv6:
		var ip [16]byte
		_, err = io.ReadFull(conn, ip[:])
		host = netip.AddrFrom16(ip).String()
	case socksAtypDomain:
		var length [1]byte
		_, err = io.ReadFull(conn, length[:])
		if err == nil {
			name := make([]byte, length[0])
			_, err = io.ReadFull(conn, name)
			host = string(name)
		}
	default:
		_ = writeSocksReply(conn, socksRepAddressNotSupported)
		return "", fmt.Errorf("%w: address type %d", errSocksRefused, request[3])
	}
	if err != nil {
		return "", fmt.Errorf("failed to read the address: %w", err)
	}

	var port [2]byte
	_, err = io.ReadFull(conn, port[:])
	if err != nil {
		return "", fmt.Errorf("failed to read the port: %w", err)
	}

	if request[1] != socksCmdConnect {
		_ = writeSocksReply(conn, socksRepCommandNotSupported)
		return "", fmt.Errorf("%w: command %d", errSocksRefused, request[1])
	}

	if host == "" {
		_ = writeSocksReply(conn, socksRepGeneralFailure)
		return "", fmt.Errorf("%w: an empty domain name", errSocksRefused)
	}

	return net.JoinHostPort(host, strconv.Itoa(int(port[0])<<8|int(port[1]))), nil
}

// writeSocksReply answers a request. The bound address is left at 0.0.0.0:0:
// the connection is made by the Host, and the address it made it from is not
// something the SSH channel tells this end.
func writeSocksReply(conn io.Writer, rep byte) error {
	_, err := conn.Write([]byte{socksVersion, rep, 0x00, socksAtypIPv4, 0, 0, 0, 0, 0, 0})
	return err
}

// socksReplyFor picks the reply for a target the Host could not reach. The
// SSH channel carries a reason code and a message, and only the code for a
// refusal by configuration is specific, so the rest is read out of the
// message, which OpenSSH fills with strerror or the resolver error. What is
// not recognised is a general failure.
func socksReplyFor(err error) byte {
	if errors.Is(err, context.DeadlineExceeded) {
		return socksRepHostUnreachable
	}

	var openErr *ssh.OpenChannelError
	if !errors.As(err, &openErr) {
		return socksRepGeneralFailure
	}

	if openErr.Reason == ssh.Prohibited {
		return socksRepNotAllowed
	}

	message := strings.ToLower(openErr.Message)
	switch {
	case strings.Contains(message, "refused"):
		return socksRepConnectionRefused
	case strings.Contains(message, "network is unreachable"):
		return socksRepNetworkUnreachable
	case strings.Contains(message, "no route to host"),
		strings.Contains(message, "host is unreachable"),
		strings.Contains(message, "timed out"),
		strings.Contains(message, "timeout"),
		strings.Contains(message, "name or service not known"),
		strings.Contains(message, "no such host"),
		strings.Contains(message, "no address associated"),
		strings.Contains(message, "temporary failure in name resolution"):
		return socksRepHostUnreachable
	}

	return socksRepGeneralFailure
}

// socksTunnel carries the SOCKS5 proxy of one Host: an SSH connection to the
// Host, and on this machine a listener pair on the proxy port whose clients
// name a target on every connection, which the Host dials.
//
// It is built the way localTunnel is, connects, watches and retries the same
// way, and differs in that the target is read from each connection rather
// than fixed, and a connection from a source that is not allowed is closed
// before anything is read from it.
type socksTunnel struct {
	hostID  uint
	listen  localPair
	server  string
	allowed []netip.Prefix
	config  *ssh.ClientConfig
	// connFP is written before the proxy is registered and never again, the
	// way localTunnel.connFP is.
	connFP   connFingerprint
	interval time.Duration
	// maxInterval and backoff are localTunnel.maxInterval and
	// localTunnel.backoff for a proxy.
	maxInterval time.Duration
	backoff     reconnectBackoff

	client   *ssh.Client
	clientMu sync.RWMutex

	stateMu sync.Mutex
	state   SocksState

	refusedMu     sync.Mutex
	refusedLogged time.Time
	refusedSince  int

	done     chan struct{}
	stopOnce sync.Once
	// listening is localTunnel.listening for the proxy port.
	listening sync.WaitGroup
	logger    *zap.Logger
}

// socksAddresses returns the listen pair and the SSH server of the proxy of a
// Host. Both the proxy and its fingerprint are built from these.
func socksAddresses(host *models.Host) (listenV4, listenV6, server string) {
	bindV4, bindV6 := bindScopeAddresses(host.SocksBindScope)
	port := strconv.Itoa(host.SocksPort)

	return net.JoinHostPort(bindV4, port),
		net.JoinHostPort(bindV6, port),
		net.JoinHostPort(host.Address, strconv.Itoa(host.Port))
}

// socksFingerprint is localForwardFingerprint for the proxy of a Host, with
// the allowed sources in place of the target: a list that was changed has to
// reach the proxy, and it is read where the proxy is built.
func socksFingerprint(host *models.Host, creds hostCreds) connFingerprint {
	listenV4, listenV6, server := socksAddresses(host)

	h := sha256.New()
	for _, value := range []string{
		server, listenV4, listenV6, host.User,
		creds.password, creds.privateKey, creds.passphrase,
		host.HostKey, host.SocksAllowedSources,
	} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(value), value)
	}

	return connFingerprint(h.Sum(nil))
}

func newSocksTunnel(host *models.Host, config *ssh.ClientConfig, interval time.Duration,
	logger *zap.Logger) (*socksTunnel, error) {
	listenV4, listenV6, serverAddr := socksAddresses(host)

	allowed, err := ParseAllowedSources(host.SocksAllowedSources)
	if err != nil {
		return nil, fmt.Errorf("failed to read the allowed sources: %w", err)
	}

	v4, err := net.ResolveTCPAddr("tcp4", listenV4)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve local address: %w", err)
	}

	v6, err := net.ResolveTCPAddr("tcp6", listenV6)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve local IPv6 address: %w", err)
	}

	return &socksTunnel{
		hostID:   host.ID,
		listen:   localPair{v4: v4, v6: v6},
		server:   dialAddress(serverAddr),
		allowed:  allowed,
		config:   config,
		interval: interval,
		state:    SocksState{Status: localStatusStarting},
		done:     make(chan struct{}),
		logger:   logger,
	}, nil
}

// fields are what every line about this proxy carries.
func (p *socksTunnel) fields(extra ...zap.Field) []zap.Field {
	return append([]zap.Field{
		zap.Uint("host_id", p.hostID),
		zap.String("local", p.listen.String()),
		zap.String("server", p.server),
	}, extra...)
}

func (p *socksTunnel) snapshot() SocksState {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()

	return p.state
}

func (p *socksTunnel) setState(change func(state *SocksState)) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()

	change(&p.state)
}

func (p *socksTunnel) stopped() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *socksTunnel) currentClient() *ssh.Client {
	p.clientMu.RLock()
	defer p.clientMu.RUnlock()

	return p.client
}

// errSocksStopped is errLocalForwardStopped for a proxy.
var errSocksStopped = errors.New("SOCKS5 proxy stopped")

// establish connects, opens the proxy port and serves what it accepts until
// the SSH connection or a listener ends. It is localTunnel.establish with the
// lines of a proxy.
func (p *socksTunnel) establish() error {
	client, _, err := dialSSHClient(p.server, p.config)
	if err != nil {
		p.logger.Error("failed to establish SSH connection",
			append([]zap.Field{logid.TunnelSshConnectFailed.Field()}, p.fields(zap.Error(err))...)...)

		p.setState(func(state *SocksState) {
			state.Status = connectFailureStatus(err)
			state.LastError = err.Error()
		})

		return fmt.Errorf("failed to establish SSH connection: %w", err)
	}

	opened, err := openLocalListeners(p.listen)
	if err != nil {
		_ = client.Close()

		p.logger.Error("failed to open the port of the SOCKS5 proxy",
			append([]zap.Field{logid.TunnelSocksListenFailed.Field()}, p.fields(zap.Error(err))...)...)

		p.setState(func(state *SocksState) {
			state.Status = localStatusError
			state.LastError = err.Error()
		})

		return fmt.Errorf("failed to open the proxy port: %w", err)
	}

	p.clientMu.Lock()
	if p.stopped() {
		opened.close()
		p.clientMu.Unlock()
		_ = client.Close()

		return errSocksStopped
	}
	p.listening.Add(1)
	p.client = client
	p.clientMu.Unlock()

	defer func() {
		opened.close()
		p.listening.Done()
	}()

	p.backoff.reset()

	p.setState(func(state *SocksState) {
		state.Status = localStatusConnected
		state.RetryCount = 0
		state.LastError = ""
		state.LastConnectedAt = time.Now()
	})

	p.logger.Info("SOCKS5 proxy connected",
		append([]zap.Field{logid.TunnelSocksConnected.Field()},
			p.fields(zap.String("open_reach", opened.reach), zap.NamedError("refused", opened.refused))...)...)

	listeners := opened.listeners()
	ends := make(chan error, len(listeners))

	var accepting sync.WaitGroup
	for _, listener := range listeners {
		accepting.Add(1)

		go func(listener net.Listener) {
			defer accepting.Done()

			ends <- p.accept(listener, client)
		}(listener)
	}

	lost := make(chan struct{})
	go func() {
		_ = client.Wait()
		close(lost)
	}()

	var acceptErr error
	select {
	case <-lost:
	case acceptErr = <-ends:
	}

	opened.close()
	accepting.Wait()

	p.clientMu.Lock()
	if p.client == client {
		p.client = nil
	}
	p.clientMu.Unlock()
	_ = client.Close()
	<-lost

	if p.stopped() {
		return errSocksStopped
	}

	if acceptErr != nil {
		p.logger.Error("listener accept error",
			append([]zap.Field{logid.TunnelListenerAcceptFailed.Field()}, p.fields(zap.Error(acceptErr))...)...)
	} else {
		p.logger.Info("connection closed",
			append([]zap.Field{logid.TunnelConnectionClosed.Field()}, p.fields()...)...)
	}

	p.setState(func(state *SocksState) {
		state.Status = localStatusReconnecting
		state.RetryCount++
		if acceptErr != nil {
			state.LastError = acceptErr.Error()
		}
	})

	return errConnectionClosed
}

// accept serves what one listener accepts and returns what ended it. A
// listener that establish closed itself ends with nil.
func (p *socksTunnel) accept(listener net.Listener, client *ssh.Client) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}

			return err
		}

		if !sourceAllowed(p.allowed, conn.RemoteAddr()) {
			_ = conn.Close()
			p.refused(conn.RemoteAddr())
			continue
		}

		go p.serve(conn, client, forwardIdleTimeout)
	}
}

// refused logs a connection from a source that is not allowed, at most once
// every socksRefusalLogInterval, with the number refused since the last line.
func (p *socksTunnel) refused(source net.Addr) {
	p.refusedMu.Lock()
	p.refusedSince++
	if !p.refusedLogged.IsZero() && time.Since(p.refusedLogged) < socksRefusalLogInterval {
		p.refusedMu.Unlock()
		return
	}
	count := p.refusedSince
	p.refusedSince = 0
	p.refusedLogged = time.Now()
	p.refusedMu.Unlock()

	p.logger.Info("refused a connection to the SOCKS5 proxy from a source that is not allowed",
		append([]zap.Field{logid.TunnelSocksSourceRefused.Field()},
			p.fields(zap.String("source", source.String()), zap.Int("refused", count))...)...)
}

// serve reads the request of one client, dials the target it names from the
// Host, answers, and joins the two. The dial is bounded by forwardDialTimeout
// for the reason localTunnel.forward bounds it.
//
// A handshake that fails and a target that cannot be reached are logged at
// debug level: both are what a browser produces in the ordinary course of
// things, and the client is told either way.
func (p *socksTunnel) serve(conn net.Conn, client *ssh.Client, idleTimeout time.Duration) {
	defer func() {
		_ = conn.Close()
	}()

	_ = conn.SetDeadline(time.Now().Add(socksHandshakeTimeout))

	target, err := readSocksRequest(conn)
	if err != nil {
		p.logger.Debug("SOCKS5 handshake failed",
			append([]zap.Field{logid.TunnelSocksHandshakeFailed.Field()},
				p.fields(zap.String("source", conn.RemoteAddr().String()), zap.Error(err))...)...)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), forwardDialTimeout)
	remoteConn, err := client.DialContext(ctx, "tcp", target)
	cancel()

	_ = conn.SetWriteDeadline(time.Now().Add(socksHandshakeTimeout))

	if err != nil {
		rep := socksReplyFor(err)
		_ = writeSocksReply(conn, rep)

		p.logger.Debug("failed to reach the target of the SOCKS5 proxy through the Host",
			append([]zap.Field{logid.TunnelSocksTargetDialFailed.Field()},
				p.fields(zap.String("target", target), zap.Int("reply", int(rep)), zap.Error(err))...)...)
		return
	}
	defer func() {
		_ = remoteConn.Close()
	}()

	err = writeSocksReply(conn, socksRepSucceeded)
	if err != nil {
		return
	}

	_ = conn.SetDeadline(time.Time{})

	joinConns(conn, remoteConn, idleTimeout, p.logger)
}

// Start runs the proxy until Stop, and gives up early on the refusals that
// localTunnel.Start gives up on.
func (p *socksTunnel) Start() {
	p.logger.Info("attempting to start tunnel",
		append([]zap.Field{logid.TunnelStarting.Field()}, p.fields()...)...)

	stopMonitor := make(chan struct{})
	var monitorWg sync.WaitGroup

	monitorWg.Add(1)
	go func() {
		defer monitorWg.Done()
		watchSSHConnection(stopMonitor, p.interval, p.server, p.currentClient, p.logger, p.fields)
	}()

	defer func() {
		close(stopMonitor)
		monitorWg.Wait()
	}()

	p.backoff = newReconnectBackoff(p.interval, p.maxInterval)

	for !p.stopped() {
		err := p.establish()
		if errors.Is(err, errSocksStopped) {
			return
		}

		if errors.Is(err, errConnectionClosed) {
			if !waitUnlessDone(p.done, p.interval) {
				return
			}
			continue
		}

		if isAuthFailure(err) || hostKeyRefusal(err) != nil {
			p.logger.Error("SOCKS5 proxy connection failed and will not be retried until its settings change",
				append([]zap.Field{logid.TunnelSocksConnectFailedGivingUp.Field()}, p.fields(zap.Error(err))...)...)
			return
		}

		wait := p.backoff.failed()
		p.logger.Error("SOCKS5 proxy connection failed, retrying in "+strconv.Itoa(retryInSec(wait))+" seconds",
			append([]zap.Field{logid.TunnelSocksConnectFailedRetrying.Field()},
				p.fields(zap.Int("retry_in_sec", retryInSec(wait)), zap.Error(err))...)...)

		if !waitUnlessDone(p.done, wait) {
			return
		}

		p.setState(func(state *SocksState) {
			state.Status = localStatusReconnecting
			state.RetryCount++
		})
	}
}

// Stop is localTunnel.Stop for a proxy: the proxy port is free by the time it
// returns.
func (p *socksTunnel) Stop() {
	p.stopOnce.Do(func() {
		p.clientMu.Lock()
		close(p.done)
		if p.client != nil {
			_ = p.client.Close()
		}
		p.clientMu.Unlock()
	})

	p.listening.Wait()
}

// startSocks builds and starts the proxy of one Host. The caller has checked
// that the Host is enabled and asks for one.
func (m *Manager) startSocks(host *models.Host) error {
	m.socksMu.Lock()
	defer m.socksMu.Unlock()

	if _, exists := m.socksProxies[host.ID]; exists {
		return fmt.Errorf("SOCKS5 proxy already exists")
	}

	auth, creds, err := m.hostAuth(host)
	if err != nil {
		return err
	}

	config := &ssh.ClientConfig{
		User:            host.User,
		Auth:            auth,
		HostKeyCallback: m.hostKeyCallback(host),
		Timeout:         time.Second * 10,
	}

	p, err := newSocksTunnel(host, config, time.Duration(m.monitoringIntervalSec)*time.Second, m.logger)
	if err != nil {
		return fmt.Errorf("failed to create SOCKS5 proxy: %w", err)
	}

	p.connFP = socksFingerprint(host, creds)
	p.maxInterval = time.Duration(m.reconnectMaxIntervalSec) * time.Second

	m.socksProxies[host.ID] = p

	go p.Start()

	return nil
}

// stopSocks stops the proxy of one Host, and returns ErrTunnelNotExist for one
// that is not running.
func (m *Manager) stopSocks(hostID uint) error {
	m.socksMu.Lock()
	p, exists := m.socksProxies[hostID]
	if exists {
		delete(m.socksProxies, hostID)
	}
	m.socksMu.Unlock()

	if !exists {
		return ErrTunnelNotExist
	}

	p.Stop()

	return nil
}

// runningSocksFingerprints returns the fingerprint of every running proxy,
// keyed by the ID of its Host.
func (m *Manager) runningSocksFingerprints() map[uint]connFingerprint {
	m.socksMu.RLock()
	defer m.socksMu.RUnlock()

	running := make(map[uint]connFingerprint, len(m.socksProxies))
	for id, p := range m.socksProxies {
		running[id] = p.connFP
	}

	return running
}

// SocksStatus returns what the SOCKS5 proxy of the Host reports. The second
// value is false when no proxy runs for it: one that is not asked for, a Host
// that is disabled, or one the reconcile pass has not reached yet.
func (m *Manager) SocksStatus(hostID uint) (SocksState, bool) {
	m.socksMu.RLock()
	p, exists := m.socksProxies[hostID]
	m.socksMu.RUnlock()

	if !exists {
		return SocksState{}, false
	}

	return p.snapshot(), true
}

// SocksStatuses returns what every running SOCKS5 proxy reports, keyed by the
// ID of its Host.
func (m *Manager) SocksStatuses() map[uint]SocksState {
	m.socksMu.RLock()
	proxies := make(map[uint]*socksTunnel, len(m.socksProxies))
	for id, p := range m.socksProxies {
		proxies[id] = p
	}
	m.socksMu.RUnlock()

	states := make(map[uint]SocksState, len(proxies))
	for id, p := range proxies {
		states[id] = p.snapshot()
	}

	return states
}

// desiredSocksOf returns the Hosts whose proxy should be running: enabled,
// with the proxy switched on and a port to open it on.
func desiredSocksOf(hostByID map[uint]*models.Host) map[uint]*models.Host {
	desired := make(map[uint]*models.Host)
	for id, host := range hostByID {
		if host.Enabled && host.SocksEnabled && host.SocksPort > 0 {
			desired[id] = host
		}
	}

	return desired
}

// reconcileSocks is the SOCKS5 half of a pass, with the rules
// reconcileLocalForwards applies.
func (m *Manager) reconcileSocks(desired map[uint]*models.Host, result *ReconcileResult) {
	running := m.runningSocksFingerprints()

	for id, host := range desired {
		current, isRunning := running[id]
		if !isRunning {
			err := m.startSocks(host)
			if err != nil {
				m.logger.Error("failed to start tunnel",
					logid.TunnelStartFailed.Field(),
					zap.Error(err),
					zap.Uint("host_id", id),
					zap.String("host_ip", host.Address),
					zap.Int("socks_port", host.SocksPort))
				result.Failed++
				continue
			}

			result.Started++
			continue
		}

		creds, err := m.hostCredentials(host)
		if err != nil {
			result.Failed++
			continue
		}

		if socksFingerprint(host, creds) == current {
			continue
		}

		// The only error stopping returns is a proxy that is already gone.
		_ = m.stopSocks(id)

		err = m.startSocks(host)
		if err != nil {
			m.logger.Error("failed to restart a tunnel whose connection settings changed",
				logid.TunnelRestartFailed.Field(),
				zap.Error(err),
				zap.Uint("host_id", id),
				zap.Int("socks_port", host.SocksPort))
			result.Failed++
			continue
		}

		result.Restarted++
	}

	for id := range running {
		if _, want := desired[id]; want {
			continue
		}

		err := m.stopSocks(id)
		if err != nil {
			continue
		}

		result.Stopped++
	}
}

// stopAllSocks stops every running proxy, reading no rows.
func (m *Manager) stopAllSocks() {
	for id := range m.runningSocksFingerprints() {
		_ = m.stopSocks(id)
	}
}
