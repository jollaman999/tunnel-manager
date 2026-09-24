package tunnel

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
)

// The statuses a local forward reports. The two host key refusals are the
// exported StatusHostKeyUnapproved and StatusHostKeyMismatch, which a failed
// connection leaves through connectFailureStatus the way it does on a tunnel.
const (
	localStatusStarting     = "starting"
	localStatusConnected    = "connected"
	localStatusReconnecting = "reconnecting"
	localStatusError        = "error"
)

// LocalForwardState is what a running local forward reports about itself. It
// is kept in memory rather than in a table: a local forward has no row of its
// own to write it to, and what it says is only true of this process anyway.
type LocalForwardState struct {
	Status          string    `json:"status"`
	LastError       string    `json:"last_error"`
	RetryCount      int       `json:"retry_count"`
	LastConnectedAt time.Time `json:"last_connected_at"`
}

// localTunnel carries one local forward: an SSH connection to the Host, and
// on this machine a listener pair on the local port whose connections are
// dialled onward from the Host to the target.
//
// It follows SSHTunnel in how it connects, watches and retries, and differs in
// where the listeners are. They are opened here with net.Listen, after the SSH
// connection stands, and closed whenever it falls, so a client of the local
// port is refused outright while there is no Host to carry it rather than
// being accepted and dropped.
type localTunnel struct {
	id     uint
	hostID uint
	// listen is the pair of addresses of this machine the local port is
	// opened on, one per address family.
	listen localPair
	server *net.TCPAddr
	target string
	config *ssh.ClientConfig
	// connFP is written before the forward is registered and never again,
	// the way SSHTunnel.connFP is.
	connFP   connFingerprint
	interval time.Duration

	client     *ssh.Client
	clientConn net.Conn
	clientMu   sync.RWMutex

	stateMu sync.Mutex
	state   LocalForwardState

	done     chan struct{}
	stopOnce sync.Once
	// listening counts the listener pairs that are open, so Stop can wait for
	// the local port to be free before a forward rebuilt on it starts. It is
	// only added to under clientMu while done is still open, which keeps the
	// Add ahead of the Wait in Stop.
	listening sync.WaitGroup
	logger    *zap.Logger
}

// LocalForwardAddresses returns the listen pair, the SSH server and the target
// of a local forward. Both the forward and its fingerprint are built from
// these, the way tunnelAddresses is used for the tunnels.
//
// It is exported because the status answer carries those addresses on the rows
// it makes for the local forwards, and where a forward listens is decided by
// the bind scope. Working that out a second time in the API would be a second
// place keeping that rule, and a row would then say one thing while the
// forward did another.
func LocalForwardAddresses(host *models.Host, lf *models.LocalForward) (listenV4, listenV6, server, target string) {
	bindV4, bindV6 := bindScopeAddresses(lf.BindScope)
	port := strconv.Itoa(lf.LocalPort)

	return net.JoinHostPort(bindV4, port),
		net.JoinHostPort(bindV6, port),
		net.JoinHostPort(host.IP, strconv.Itoa(host.Port)),
		net.JoinHostPort(lf.TargetIP, strconv.Itoa(lf.TargetPort))
}

// localForwardFingerprint is connectionFingerprint for a local forward: the
// server, the user, every credential, the trusted host key, the listen pair
// and the target, each with its length in front. The reasons each of them is
// in there are the ones given at connectionFingerprint.
func localForwardFingerprint(host *models.Host, lf *models.LocalForward, creds hostCreds) connFingerprint {
	listenV4, listenV6, server, target := LocalForwardAddresses(host, lf)

	h := sha256.New()
	for _, value := range []string{
		server, target, listenV4, listenV6, host.User,
		creds.password, creds.privateKey, creds.passphrase,
		host.HostKey,
	} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(value), value)
	}

	return connFingerprint(h.Sum(nil))
}

func newLocalTunnel(lf *models.LocalForward, host *models.Host, config *ssh.ClientConfig,
	interval time.Duration, logger *zap.Logger) (*localTunnel, error) {
	listenV4, listenV6, serverAddr, target := LocalForwardAddresses(host, lf)

	v4, err := net.ResolveTCPAddr("tcp4", listenV4)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve local address: %w", err)
	}

	v6, err := net.ResolveTCPAddr("tcp6", listenV6)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve local IPv6 address: %w", err)
	}

	server, err := net.ResolveTCPAddr("tcp", serverAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve server address: %w", err)
	}

	return &localTunnel{
		id:       lf.ID,
		hostID:   host.ID,
		listen:   localPair{v4: v4, v6: v6},
		server:   server,
		target:   target,
		config:   config,
		interval: interval,
		state:    LocalForwardState{Status: localStatusStarting},
		done:     make(chan struct{}),
		logger:   logger,
	}, nil
}

// fields are what every line about this forward carries.
func (f *localTunnel) fields(extra ...zap.Field) []zap.Field {
	return append([]zap.Field{
		zap.Uint("local_forward_id", f.id),
		zap.Uint("host_id", f.hostID),
		zap.String("local", f.listen.String()),
		zap.String("server", f.server.String()),
		zap.String("target", f.target),
	}, extra...)
}

func (f *localTunnel) snapshot() LocalForwardState {
	f.stateMu.Lock()
	defer f.stateMu.Unlock()

	return f.state
}

func (f *localTunnel) setState(change func(state *LocalForwardState)) {
	f.stateMu.Lock()
	defer f.stateMu.Unlock()

	change(&f.state)
}

func (f *localTunnel) stopped() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

// errLocalForwardStopped is what establish returns when Stop ended it, which
// is not a failure and is not retried.
var errLocalForwardStopped = errors.New("local forward stopped")

// establish connects, opens the local port and carries what it accepts until
// the SSH connection or a listener ends.
func (f *localTunnel) establish() error {
	client, clientConn, err := dialSSHClient(f.server.String(), f.config)
	if err != nil {
		f.logger.Error("failed to establish SSH connection",
			append([]zap.Field{logid.TunnelSshConnectFailed.Field()}, f.fields(zap.Error(err))...)...)

		f.setState(func(state *LocalForwardState) {
			state.Status = connectFailureStatus(err)
			state.LastError = err.Error()
		})

		return fmt.Errorf("failed to establish SSH connection: %w", err)
	}

	opened, err := openLocalListeners(f.listen)
	if err != nil {
		_ = client.Close()

		f.logger.Error("failed to open the local port of the local forward",
			append([]zap.Field{logid.TunnelLocalForwardListenFailed.Field()}, f.fields(zap.Error(err))...)...)

		f.setState(func(state *LocalForwardState) {
			state.Status = localStatusError
			state.LastError = err.Error()
		})

		return fmt.Errorf("failed to open the local port: %w", err)
	}

	f.clientMu.Lock()
	if f.stopped() {
		// Closed under the lock, so Stop, which is waiting for it, returns
		// with the port already free.
		opened.close()
		f.clientMu.Unlock()
		_ = client.Close()

		return errLocalForwardStopped
	}
	f.listening.Add(1)
	f.client = client
	f.clientConn = clientConn
	f.clientMu.Unlock()

	defer func() {
		opened.close()
		f.listening.Done()
	}()

	f.setState(func(state *LocalForwardState) {
		state.Status = localStatusConnected
		state.RetryCount = 0
		state.LastError = ""
		state.LastConnectedAt = time.Now()
	})

	// refused is the half of the pair that did not open, and is nil when both
	// did, in which case zap leaves the field out.
	f.logger.Info("local forward connected",
		append([]zap.Field{logid.TunnelLocalForwardConnected.Field()},
			f.fields(zap.String("open_reach", opened.reach), zap.NamedError("refused", opened.refused))...)...)

	listeners := opened.listeners()
	ends := make(chan error, len(listeners))

	var accepting sync.WaitGroup
	for _, listener := range listeners {
		accepting.Add(1)

		go func(listener net.Listener) {
			defer accepting.Done()

			ends <- f.accept(listener, client)
		}(listener)
	}

	// client.Wait returns once the SSH connection is gone, whether the far
	// side dropped it, the monitor closed it or Stop did.
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

	f.clientMu.Lock()
	if f.client == client {
		f.client = nil
		f.clientConn = nil
	}
	f.clientMu.Unlock()
	_ = client.Close()
	<-lost

	if f.stopped() {
		return errLocalForwardStopped
	}

	if acceptErr != nil {
		f.logger.Error("listener accept error",
			append([]zap.Field{logid.TunnelListenerAcceptFailed.Field()}, f.fields(zap.Error(acceptErr))...)...)
	} else {
		f.logger.Info("connection closed",
			append([]zap.Field{logid.TunnelConnectionClosed.Field()}, f.fields()...)...)
	}

	f.setState(func(state *LocalForwardState) {
		state.Status = localStatusReconnecting
		state.RetryCount++
		if acceptErr != nil {
			state.LastError = acceptErr.Error()
		}
	})

	return errConnectionClosed
}

// accept carries what one listener accepts and returns what ended it. A
// listener that establish closed itself ends with nil.
func (f *localTunnel) accept(listener net.Listener, client *ssh.Client) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}

			return err
		}

		go f.forward(conn, client, forwardIdleTimeout)
	}
}

// forward joins a connection the local port accepted to the target, dialled
// from the Host. The dial is bounded by forwardDialTimeout for the reason it
// bounds the dial of a tunnel: the Host waits on the target, and a target that
// answers nothing would otherwise hold the connection for as long as the Host
// cares to retry.
func (f *localTunnel) forward(localConn net.Conn, client *ssh.Client, idleTimeout time.Duration) {
	defer func() {
		_ = localConn.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), forwardDialTimeout)
	remoteConn, err := client.DialContext(ctx, "tcp", f.target)
	cancel()
	if err != nil {
		f.logger.Error("failed to reach the target of the local forward through the Host",
			append([]zap.Field{logid.TunnelLocalForwardTargetDialFailed.Field()}, f.fields(zap.Error(err))...)...)
		return
	}
	defer func() {
		_ = remoteConn.Close()
	}()

	joinConns(localConn, remoteConn, idleTimeout, f.logger)
}

// monitor checks the SSH connection every interval the way
// SSHTunnel.monitorConnection does, and closes one that no longer answers.
// Closing it is all it does: establish sees the connection end and takes the
// forward down to be connected again.
func (f *localTunnel) monitor(stop <-chan struct{}) {
	watchSSHConnection(stop, f.interval, f.server.String(), f.currentClient, f.logger, f.fields)
}

func (f *localTunnel) currentClient() (*ssh.Client, net.Conn) {
	f.clientMu.RLock()
	defer f.clientMu.RUnlock()

	return f.client, f.clientConn
}

// watchSSHConnection is the loop of monitor, shared with the SOCKS5 proxies,
// which hold their connection the same way. current returns the connection
// that stands, or a nil client while there is none, and fields are what every
// line about the owner carries.
func watchSSHConnection(stop <-chan struct{}, interval time.Duration, server string,
	current func() (*ssh.Client, net.Conn), logger *zap.Logger, fields func(extra ...zap.Field) []zap.Field) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	intervalSec := int(interval / time.Second)

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			client, clientConn := current()

			if client == nil {
				continue
			}

			conn, err := net.DialTimeout("tcp", server, monitorDialTimeout(intervalSec))
			if err != nil {
				logger.Warn("SSH connection lost, attempting reconnection",
					append([]zap.Field{logid.TunnelServerUnreachable.Field()}, fields(zap.Error(err))...)...)
				_ = client.Close()
				continue
			}
			_ = conn.Close()

			err = sendKeepalive(client, clientConn, monitorKeepaliveTimeout(intervalSec))
			if err != nil {
				logger.Warn("SSH keepalive check failed, attempting reconnection",
					append([]zap.Field{logid.TunnelKeepaliveFailed.Field()}, fields(zap.Error(err))...)...)
				_ = client.Close()
			}
		}
	}
}

// waitBeforeRetry waits for the retry interval and reports whether the
// forward should keep running.
func (f *localTunnel) waitBeforeRetry() bool {
	return waitUnlessDone(f.done, f.interval)
}

// waitUnlessDone waits for wait and reports false if done closes first. It is
// waitBeforeRetry for anything that retries on an interval until it is
// stopped.
func waitUnlessDone(done <-chan struct{}, wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-done:
		return false
	case <-timer.C:
		return true
	}
}

// Start runs the forward until Stop, and gives up early on the refusals that
// SSHTunnel.Start gives up on, for the reasons given there.
func (f *localTunnel) Start() {
	f.logger.Info("attempting to start tunnel",
		append([]zap.Field{logid.TunnelStarting.Field()}, f.fields()...)...)

	stopMonitor := make(chan struct{})
	var monitorWg sync.WaitGroup

	monitorWg.Add(1)
	go func() {
		defer monitorWg.Done()
		f.monitor(stopMonitor)
	}()

	defer func() {
		close(stopMonitor)
		monitorWg.Wait()
	}()

	for !f.stopped() {
		err := f.establish()
		if errors.Is(err, errLocalForwardStopped) {
			return
		}

		if errors.Is(err, errConnectionClosed) {
			if !f.waitBeforeRetry() {
				return
			}
			continue
		}

		if isAuthFailure(err) || hostKeyRefusal(err) != nil {
			f.logger.Error("local forward connection failed and will not be retried until its settings change",
				append([]zap.Field{logid.TunnelLocalForwardConnectFailedGivingUp.Field()}, f.fields(zap.Error(err))...)...)
			return
		}

		retryInSec := int(f.interval / time.Second)
		f.logger.Error("local forward connection failed, retrying in "+strconv.Itoa(retryInSec)+" seconds",
			append([]zap.Field{logid.TunnelLocalForwardConnectFailedRetrying.Field()},
				f.fields(zap.Int("retry_in_sec", retryInSec), zap.Error(err))...)...)

		if !f.waitBeforeRetry() {
			return
		}

		f.setState(func(state *LocalForwardState) {
			state.Status = localStatusReconnecting
			state.RetryCount++
		})
	}
}

// Stop ends the forward and waits for its listeners to be closed, so the
// local port is free by the time a forward rebuilt on it starts. It does not
// wait for a connection attempt that is under way, which opens nothing once it
// sees the forward stopped.
func (f *localTunnel) Stop() {
	f.stopOnce.Do(func() {
		f.clientMu.Lock()
		close(f.done)
		if f.client != nil {
			_ = f.client.Close()
		}
		f.clientMu.Unlock()
	})

	f.listening.Wait()
}

// startLocalForward builds and starts the forward of one row. The caller has
// read the Host already, and a Host that is not enabled is not asked for.
func (m *Manager) startLocalForward(host *models.Host, lf *models.LocalForward) error {
	m.localMu.Lock()
	defer m.localMu.Unlock()

	if _, exists := m.localForwards[lf.ID]; exists {
		return fmt.Errorf("local forward already exists")
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

	f, err := newLocalTunnel(lf, host, config, time.Duration(m.monitoringIntervalSec)*time.Second, m.logger)
	if err != nil {
		return fmt.Errorf("failed to create local forward: %w", err)
	}

	f.connFP = localForwardFingerprint(host, lf, creds)

	m.localForwards[lf.ID] = f

	go f.Start()

	return nil
}

// stopLocalForward stops the forward of one row. It returns
// ErrTunnelNotExist for one that is not running, which the callers pass over
// the way they do for a tunnel.
func (m *Manager) stopLocalForward(id uint) error {
	m.localMu.Lock()
	f, exists := m.localForwards[id]
	if exists {
		delete(m.localForwards, id)
	}
	m.localMu.Unlock()

	if !exists {
		return ErrTunnelNotExist
	}

	f.Stop()

	return nil
}

// runningLocalForwardFingerprints returns the fingerprint of every running
// local forward, keyed by its row ID.
func (m *Manager) runningLocalForwardFingerprints() map[uint]connFingerprint {
	m.localMu.RLock()
	defer m.localMu.RUnlock()

	running := make(map[uint]connFingerprint, len(m.localForwards))
	for id, f := range m.localForwards {
		running[id] = f.connFP
	}

	return running
}

// LocalForwardStatus returns what the local forward of the row id reports.
// The second value is false when no forward runs for it, which is a row that
// is switched off, one whose Host is disabled or one the reconcile pass has not
// reached yet.
func (m *Manager) LocalForwardStatus(id uint) (LocalForwardState, bool) {
	m.localMu.RLock()
	f, exists := m.localForwards[id]
	m.localMu.RUnlock()

	if !exists {
		return LocalForwardState{}, false
	}

	return f.snapshot(), true
}

// LocalForwardStatuses returns what every running local forward reports,
// keyed by its row ID, so a list is answered without a call per row.
func (m *Manager) LocalForwardStatuses() map[uint]LocalForwardState {
	m.localMu.RLock()
	forwards := make(map[uint]*localTunnel, len(m.localForwards))
	for id, f := range m.localForwards {
		forwards[id] = f
	}
	m.localMu.RUnlock()

	states := make(map[uint]LocalForwardState, len(forwards))
	for id, f := range forwards {
		states[id] = f.snapshot()
	}

	return states
}

// desiredLocalForward is one local forward that should be running, with the
// Host it is carried by.
type desiredLocalForward struct {
	host *models.Host
	lf   *models.LocalForward
}

// desiredLocalForwardsOf reads the local forwards that should be running:
// every row that is enabled and whose Host is there and enabled. A row naming a
// Host that is not there is passed over for the reasons desiredTunnels passes
// over an assignment.
func (m *Manager) desiredLocalForwardsOf(hostByID map[uint]*models.Host) (map[uint]desiredLocalForward, error) {
	var rows []models.LocalForward
	err := m.db.Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("failed to fetch local forwards: %w", err)
	}

	desired := make(map[uint]desiredLocalForward, len(rows))
	for i := range rows {
		if !rows[i].Enabled {
			continue
		}

		host, ok := hostByID[rows[i].HostID]
		if !ok || !host.Enabled {
			continue
		}

		desired[rows[i].ID] = desiredLocalForward{host: host, lf: &rows[i]}
	}

	return desired, nil
}

// DesiredLocalForwardCount returns how many local forwards should be running.
// It counts what a pass builds its desired state from, the way
// DesiredTunnelCount does over the tunnels, so it cannot drift from what the
// loop tries to start. A count above the number that report connected means a
// forward that should be up is not.
func (m *Manager) DesiredLocalForwardCount() (int, error) {
	hostByID, err := m.hostsByID()
	if err != nil {
		return 0, err
	}

	desired, err := m.desiredLocalForwardsOf(hostByID)
	if err != nil {
		return 0, err
	}

	return len(desired), nil
}

// reconcileLocalForwards is the local forward half of a pass, with the rules
// Reconcile applies to the tunnels: start what is missing, rebuild what runs
// on settings it should no longer have, stop what is not wanted.
func (m *Manager) reconcileLocalForwards(desired map[uint]desiredLocalForward, result *ReconcileResult) {
	running := m.runningLocalForwardFingerprints()

	for id, want := range desired {
		current, isRunning := running[id]
		if !isRunning {
			err := m.startLocalForward(want.host, want.lf)
			if err != nil {
				m.logger.Error("failed to start tunnel",
					logid.TunnelStartFailed.Field(),
					zap.Error(err),
					zap.Uint("local_forward_id", id),
					zap.String("host_ip", want.host.IP),
					zap.Int("local_port", want.lf.LocalPort))
				result.Failed++
				continue
			}

			result.Started++
			continue
		}

		creds, err := m.hostCredentials(want.host)
		if err != nil {
			result.Failed++
			continue
		}

		if localForwardFingerprint(want.host, want.lf, creds) == current {
			continue
		}

		// The only error stopping returns is a forward that is already gone,
		// which leaves nothing in the way of starting it again.
		_ = m.stopLocalForward(id)

		err = m.startLocalForward(want.host, want.lf)
		if err != nil {
			m.logger.Error("failed to restart a tunnel whose connection settings changed",
				logid.TunnelRestartFailed.Field(),
				zap.Error(err),
				zap.Uint("local_forward_id", id),
				zap.Uint("host_id", want.host.ID))
			result.Failed++
			continue
		}

		result.Restarted++
	}

	for id := range running {
		if _, want := desired[id]; want {
			continue
		}

		err := m.stopLocalForward(id)
		if err != nil {
			// The only error is a forward that is already gone, which is what
			// this pass wanted.
			continue
		}

		result.Stopped++
	}
}

// stopAllLocalForwards stops every running local forward. Like StopAllTunnels
// it reads no rows.
func (m *Manager) stopAllLocalForwards() {
	for id := range m.runningLocalForwardFingerprints() {
		_ = m.stopLocalForward(id)
	}
}
