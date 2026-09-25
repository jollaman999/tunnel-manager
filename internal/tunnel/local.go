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

// LocalForwardKey names one local forward: the Host that carries it and which
// forward of that Host it is. It is the key of the table (models.LocalForward),
// so a running forward is registered and reported under the same pair the row
// is read and written by, and neither has to be looked up to reach the other.
type LocalForwardKey struct {
	HostID uint
	Number uint
}

// localForwardKeyOf is the key of one row.
func localForwardKeyOf(lf *models.LocalForward) LocalForwardKey {
	return LocalForwardKey{HostID: lf.HostID, Number: lf.Number}
}

// LocalForwardState is what a running local forward reports about itself. It
// is kept in memory rather than in a table: a local forward has no row of its
// own to write it to, and what it says is only true of this process anyway.
type LocalForwardState struct {
	Status          string    `json:"status"`
	LastError       string    `json:"last_error"`
	RetryCount      int       `json:"retry_count"`
	LastConnectedAt time.Time `json:"last_connected_at"`
	// ForwardReach is what the reachability probe measured of the target over
	// the connection that stands, in the words a tunnel writes to
	// models.Tunnel.ForwardReach. It is "unknown" from the moment a connection
	// stands until the probe of that connection answers.
	ForwardReach string `json:"forward_reach"`
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
	// id is which forward of its Host this is. Every line about the forward
	// carries it beside hostID, and the two together are the row.
	id     uint
	hostID uint
	// listen is the pair of addresses of this machine the local port is
	// opened on, one per address family.
	listen localPair
	server string
	target string
	config *ssh.ClientConfig
	// connFP is written before the forward is registered and never again,
	// the way SSHTunnel.connFP is.
	connFP   connFingerprint
	interval time.Duration

	client   *ssh.Client
	clientMu sync.RWMutex

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
		net.JoinHostPort(host.Address, strconv.Itoa(host.Port)),
		net.JoinHostPort(lf.TargetAddress, strconv.Itoa(lf.TargetPort))
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

	return &localTunnel{
		id:       lf.Number,
		hostID:   host.ID,
		listen:   localPair{v4: v4, v6: v6},
		server:   dialAddress(serverAddr),
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
		zap.Uint("local_forward_number", f.id),
		zap.Uint("host_id", f.hostID),
		zap.String("local", f.listen.String()),
		zap.String("server", f.server),
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
	client, _, err := dialSSHClient(f.server, f.config)
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
		// What the last connection measured says nothing about this one, which
		// may go to a Host that was reconfigured or to a target that came back
		// in between, so the reading goes back to unknown until the probe below
		// answers for the connection that stands. A tunnel row is emptied the
		// same way on connecting (ssh.go, Start).
		state.ForwardReach = forwardReachUnknown
	})

	// Measured here and not on every pass, for the reason the tunnels measure
	// it once a connection stands: what decides it is the target and the way
	// to it from the Host, neither of which changes under a connection that
	// stands, while a probe per status read would be one connection to the
	// target per forward per reader.
	go f.recordForwardReach(client)

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

// probeLocalForwardReach reports whether the target answers a connection
// opened from the Host, in the words models.Tunnel.ForwardReach carries.
//
// A silence is written down as the target being unreachable, where a tunnel
// writes one down as unknown. The difference is what does the dialling. A
// tunnel probes the forwarded port from this process, which is not the machine
// the port was opened on and cannot try every address it may have been opened
// at, so nothing coming back may mean only that the probe was in no position
// to see it. This probe asks the Host to dial the target, which is the very
// thing every connection to the local port asks of it, over the connection
// those connections are carried on: what the probe is told is what a client of
// this forward would be told a moment later, so there is nothing here that a
// silence could mean instead.
//
// The connection is closed as soon as it stands, for the reason the tunnel
// probe closes its own: the dial is the whole measurement and a probe left
// open is a channel held for the life of the connection.
func probeLocalForwardReach(client *ssh.Client, target string, timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	conn, err := client.DialContext(ctx, "tcp", target)
	if err != nil {
		return forwardUnreachable
	}
	_ = conn.Close()

	return forwardReachable
}

// recordForwardReach measures the target and writes the reading to the state
// of the forward.
//
// It runs beside the accept loop rather than in it, for the reason the tunnel
// runs its own probe apart: the dial waits out its whole bound against a
// target that answers nothing, and the loop it would hold is the one carrying
// the traffic of the forward.
//
// A reading taken on a connection that is no longer the current one is
// dropped. The forward reconnected while the probe was waiting, a probe of its
// own is running for the connection that replaced it, and writing here would
// put the reading of a connection that is gone on the forward that is up.
func (f *localTunnel) recordForwardReach(measured *ssh.Client) {
	reach := probeLocalForwardReach(measured, f.target, forwardProbeTimeout)

	f.clientMu.RLock()
	current := f.client
	f.clientMu.RUnlock()

	if current != measured {
		return
	}

	f.setState(func(state *LocalForwardState) {
		state.ForwardReach = reach
	})

	if reach != forwardUnreachable {
		return
	}

	// Logged under the id the same failure carries when a client runs into it,
	// because it is that failure: the Host could not reach the target. What is
	// different is only that nobody was waiting on this one.
	f.logger.Warn("the target of the local forward did not answer a connection dialled from the Host, so "+
		"the forward is connected but carries nothing. Check that the target is listening and that the "+
		"Host is allowed to reach it, and that the SSH server allows this connection to open one "+
		"(AllowTcpForwarding and PermitOpen in sshd_config for OpenSSH)",
		append([]zap.Field{logid.TunnelLocalForwardTargetDialFailed.Field()}, f.fields()...)...)
}

// monitor checks the SSH connection every interval the way
// SSHTunnel.monitorConnection does, and closes one that no longer answers.
// Closing it is all it does: establish sees the connection end and takes the
// forward down to be connected again.
func (f *localTunnel) monitor(stop <-chan struct{}) {
	watchSSHConnection(stop, f.interval, f.server, f.currentClient, f.logger, f.fields)
}

func (f *localTunnel) currentClient() *ssh.Client {
	f.clientMu.RLock()
	defer f.clientMu.RUnlock()

	return f.client
}

// watchSSHConnection is the loop of monitor, shared with the SOCKS5 proxies,
// which hold their connection the same way. current returns the connection
// that stands, or a nil client while there is none, and fields are what every
// line about the owner carries. It waits on a timer set again after each check
// for the reason SSHTunnel.monitorConnection does.
func watchSSHConnection(stop <-chan struct{}, interval time.Duration, server string,
	current func() *ssh.Client, logger *zap.Logger, fields func(extra ...zap.Field) []zap.Field) {
	timer := time.NewTimer(interval)
	defer timer.Stop()

	timeout := monitorCheckTimeout(interval)

	for {
		select {
		case <-stop:
			return
		case <-timer.C:
			checkSSHConnection(server, current(), timeout, logger, fields)
			timer.Reset(interval)
		}
	}
}

// checkSSHConnection is one check of watchSSHConnection. It is
// SSHTunnel.checkConnection for a connection that has nothing to report but
// its end: a client that fails the check is closed, and a nil one is skipped.
func checkSSHConnection(server string, client *ssh.Client, timeout time.Duration,
	logger *zap.Logger, fields func(extra ...zap.Field) []zap.Field) {
	if client == nil {
		return
	}

	conn, err := net.DialTimeout("tcp", server, timeout)
	if err != nil {
		logger.Warn("SSH connection lost, attempting reconnection",
			append([]zap.Field{logid.TunnelServerUnreachable.Field()}, fields(zap.Error(err))...)...)
		_ = client.Close()
		return
	}
	_ = conn.Close()

	err = sendKeepalive(client, timeout)
	if err != nil {
		logger.Warn("SSH keepalive check failed, attempting reconnection",
			append([]zap.Field{logid.TunnelKeepaliveFailed.Field()}, fields(zap.Error(err))...)...)
		_ = client.Close()
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

	if _, exists := m.localForwards[localForwardKeyOf(lf)]; exists {
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

	m.localForwards[localForwardKeyOf(lf)] = f

	go f.Start()

	return nil
}

// stopLocalForward stops the forward of one row. It returns ErrTunnelNotExist
// for one that is not running, which the callers pass over the way they do for
// a tunnel.
func (m *Manager) stopLocalForward(key LocalForwardKey) error {
	m.localMu.Lock()
	f, exists := m.localForwards[key]
	if exists {
		delete(m.localForwards, key)
	}
	m.localMu.Unlock()

	if !exists {
		return ErrTunnelNotExist
	}

	f.Stop()

	return nil
}

// runningLocalForwardFingerprints returns the fingerprint of every running
// local forward, keyed by the row it is.
func (m *Manager) runningLocalForwardFingerprints() map[LocalForwardKey]connFingerprint {
	m.localMu.RLock()
	defer m.localMu.RUnlock()

	running := make(map[LocalForwardKey]connFingerprint, len(m.localForwards))
	for key, f := range m.localForwards {
		running[key] = f.connFP
	}

	return running
}

// LocalForwardStatus returns what the local forward of one row reports. The
// second value is false when no forward runs for it, which is a row that is
// switched off, one whose Host is disabled or one the reconcile pass has not
// reached yet.
func (m *Manager) LocalForwardStatus(key LocalForwardKey) (LocalForwardState, bool) {
	m.localMu.RLock()
	f, exists := m.localForwards[key]
	m.localMu.RUnlock()

	if !exists {
		return LocalForwardState{}, false
	}

	return f.snapshot(), true
}

// LocalForwardStatuses returns what every running local forward reports,
// keyed by the row it is, so a list is answered without a call per row.
func (m *Manager) LocalForwardStatuses() map[LocalForwardKey]LocalForwardState {
	m.localMu.RLock()
	forwards := make(map[LocalForwardKey]*localTunnel, len(m.localForwards))
	for key, f := range m.localForwards {
		forwards[key] = f
	}
	m.localMu.RUnlock()

	states := make(map[LocalForwardKey]LocalForwardState, len(forwards))
	for key, f := range forwards {
		states[key] = f.snapshot()
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
func (m *Manager) desiredLocalForwardsOf(hostByID map[uint]*models.Host) (map[LocalForwardKey]desiredLocalForward, error) {
	var rows []models.LocalForward
	err := m.db.Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("failed to fetch local forwards: %w", err)
	}

	desired := make(map[LocalForwardKey]desiredLocalForward, len(rows))
	for i := range rows {
		if !rows[i].Enabled {
			continue
		}

		host, ok := hostByID[rows[i].HostID]
		if !ok || !host.Enabled {
			continue
		}

		desired[localForwardKeyOf(&rows[i])] = desiredLocalForward{host: host, lf: &rows[i]}
	}

	return desired, nil
}

// DesiredLocalForwardCount returns how many local forwards should be running.
// It is taken from the last pass that completed, the way DesiredTunnelCount is
// over the tunnels, so it cannot drift from what the loop tries to start, and
// before the first pass it is counted with the code a pass uses. A count above
// the number that report connected means a forward that should be up is not.
func (m *Manager) DesiredLocalForwardCount() (int, error) {
	counts, ok := m.keptDesiredCounts()
	if ok {
		return counts.localForwards, nil
	}

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
func (m *Manager) reconcileLocalForwards(desired map[LocalForwardKey]desiredLocalForward, result *ReconcileResult) {
	running := m.runningLocalForwardFingerprints()

	for key, want := range desired {
		current, isRunning := running[key]
		if !isRunning {
			err := m.startLocalForward(want.host, want.lf)
			if err != nil {
				m.logger.Error("failed to start tunnel",
					logid.TunnelStartFailed.Field(),
					zap.Error(err),
					zap.Uint("local_forward_number", want.lf.Number),
					zap.String("host_ip", want.host.Address),
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
		// which leaves nothing in the way of starting it again. The row is
		// stopped before it is started again, so a forward whose local port
		// changed has let go of the old one before the new one is opened.
		_ = m.stopLocalForward(key)

		err = m.startLocalForward(want.host, want.lf)
		if err != nil {
			m.logger.Error("failed to restart a tunnel whose connection settings changed",
				logid.TunnelRestartFailed.Field(),
				zap.Error(err),
				zap.Uint("local_forward_number", want.lf.Number),
				zap.Uint("host_id", want.host.ID))
			result.Failed++
			continue
		}

		result.Restarted++
	}

	for key := range running {
		if _, want := desired[key]; want {
			continue
		}

		err := m.stopLocalForward(key)
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
	for key := range m.runningLocalForwardFingerprints() {
		_ = m.stopLocalForward(key)
	}
}
