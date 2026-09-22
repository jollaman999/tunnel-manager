package tunnel

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
)

// defaultReconcileIntervalSec is the period the loop falls back to when it is
// given an interval no ticker can be built from. The configuration rejects such
// a value, so it only applies to a caller that built the interval itself.
const defaultReconcileIntervalSec = 5

// ReconcileResult reports what a single pass changed. It is meant to be logged,
// nothing branches on it.
type ReconcileResult struct {
	Started int
	Stopped int
	// Restarted counts the tunnels that were running with connection settings
	// that are no longer the ones they should have, and were stopped and
	// started again in this pass.
	Restarted int
	Failed    int
}

// desiredTunnel is one combination that should be running, with the rows
// StartTunnel needs, so the pass does not read them a second time.
type desiredTunnel struct {
	host *models.Host
	sp   *models.ServicePort
	// bindScope is what the assignment row of this combination stored, and it
	// decides the pair of addresses the forwarded port is asked to be opened
	// on. It is carried here rather than looked up where the tunnel is built,
	// because the assignment row is read by the pass already and is the only
	// place this answer exists.
	bindScope string
}

// connFingerprint stands for the connection settings a tunnel was built from.
// It is an array so it can be compared with ==, and it is never formatted, so
// it cannot reach a log.
type connFingerprint [sha256.Size]byte

// connectionFingerprint derives the fingerprint of the settings a tunnel for
// this combination is built from: the server, remote and local addresses, the
// user, and every credential it logs in with.
//
// The credentials go in as the plaintext they were opened to, not as the values
// the row holds. The stored form is sealed with a fresh nonce every time it is
// written, so the same password would give a different fingerprint on every
// pass and every pass would restart the tunnel.
//
// The private key and its passphrase are in here for the same reason the
// password is. Without them a Host whose key is replaced keeps its tunnel on
// the key it was built with: the save answers as though it took, and the new
// key is not tried until something else drops the connection, which may be
// hours later and looks like the key failing rather than the key never having
// been used.
//
// The bind scope is in here because it decides the local addresses, and those
// are what the tunnel asks the far side to open. A scope that was changed has
// to reach the connection, and the only way a forward changes the address it is
// bound to is by being opened again.
//
// The host key the Host is trusted on is in here because approving one is a
// change to how the connection is made rather than to what it logs in with.
// The trust is read where the tunnel is built, so a tunnel that was refused
// for a key nobody had approved goes on being refused against the empty value
// it was built with. Without this, approving a key would take hold only when
// something else brought the connection down, and a tunnel that was refused
// has no connection left to bring down: it would stay down until the process
// was restarted.
//
// Every value is written with its length in front, so that two different sets
// of settings cannot produce the same input to the hash.
func connectionFingerprint(host *models.Host, sp *models.ServicePort, bindScope string, creds hostCreds) connFingerprint {
	localV4, localV6, server, remote := tunnelAddresses(host, sp, bindScope)

	h := sha256.New()
	for _, value := range []string{
		server, remote, localV4, localV6, host.User,
		creds.password, creds.privateKey, creds.passphrase,
		host.HostKey,
	} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(value), value)
	}

	return connFingerprint(h.Sum(nil))
}

// tunnelKey is the key a tunnel is registered under in m.tunnels.
func tunnelKey(hostID, spID uint) string {
	return fmt.Sprintf("%d-%d", hostID, spID)
}

// parseTunnelKey splits a key of m.tunnels back into the identifiers it was
// built from.
func parseTunnelKey(key string) (uint, uint, bool) {
	hostPart, spPart, found := strings.Cut(key, "-")
	if !found {
		return 0, 0, false
	}

	hostID, err := strconv.ParseUint(hostPart, 10, 0)
	if err != nil {
		return 0, 0, false
	}

	spID, err := strconv.ParseUint(spPart, 10, 0)
	if err != nil {
		return 0, 0, false
	}

	return uint(hostID), uint(spID), true
}

// desiredTunnels reads the state the tunnels should be in: every service port
// that is assigned to a Host, on every Host that is enabled. A Host that is not
// enabled is left out here, so the tunnels of a Host that was just disabled
// count as running without being wanted and are stopped. Its assignments stay
// in the table, so enabling it again brings its tunnels back.
//
// The three tables are read once each and paired in memory. This runs on every
// pass of the reconcile loop, so a statement per assignment would put the
// number of tunnels of this installation onto the database every few seconds.
//
// An assignment naming a Host or a service port that is not there is passed
// over. Nothing can be built from it: the address, the port and the credentials
// all sit on the rows that are gone. It is not logged, because the loop would
// report the same row on every pass for as long as it exists, and it is not
// deleted either, because this is the read side of the loop and a delete here
// would race the handler that is removing the rows.
func (m *Manager) desiredTunnels() (map[string]desiredTunnel, error) {
	var hosts []models.Host
	err := m.db.Find(&hosts).Error
	if err != nil {
		return nil, fmt.Errorf("failed to fetch hosts: %w", err)
	}

	var servicePorts []models.ServicePort
	err = m.db.Find(&servicePorts).Error
	if err != nil {
		return nil, fmt.Errorf("failed to fetch service ports: %w", err)
	}

	var assignments []models.HostServicePort
	err = m.db.Find(&assignments).Error
	if err != nil {
		return nil, fmt.Errorf("failed to fetch service port assignments: %w", err)
	}

	hostByID := make(map[uint]*models.Host, len(hosts))
	for i := range hosts {
		hostByID[hosts[i].ID] = &hosts[i]
	}

	spByID := make(map[uint]*models.ServicePort, len(servicePorts))
	for i := range servicePorts {
		spByID[servicePorts[i].ID] = &servicePorts[i]
	}

	desired := make(map[string]desiredTunnel, len(assignments))
	for _, assignment := range assignments {
		host, ok := hostByID[assignment.HostID]
		if !ok || !host.Enabled {
			continue
		}

		sp, ok := spByID[assignment.SPID]
		if !ok {
			continue
		}

		desired[tunnelKey(host.ID, sp.ID)] = desiredTunnel{
			host:      host,
			sp:        sp,
			bindScope: assignment.BindScope,
		}
	}

	return desired, nil
}

// runningTunnelKeys returns the keys of the tunnels that are running. The lock
// is released before the caller starts or stops anything, because StartTunnel
// and StopTunnel take it themselves.
func (m *Manager) runningTunnelKeys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	keys := make([]string, 0, len(m.tunnels))
	for key := range m.tunnels {
		keys = append(keys, key)
	}

	return keys
}

// runningTunnelFingerprints returns the connection fingerprint of every tunnel
// that is running, keyed the way m.tunnels is. The lock is released before the
// caller starts or stops anything, because StartTunnel and StopTunnel take it
// themselves. A fingerprint is written before its tunnel is registered and
// never again, so reading it here reads the settings that tunnel was built
// from.
func (m *Manager) runningTunnelFingerprints() map[string]connFingerprint {
	m.mu.RLock()
	defer m.mu.RUnlock()

	running := make(map[string]connFingerprint, len(m.tunnels))
	for key, t := range m.tunnels {
		running[key] = t.connFP
	}

	return running
}

// restartTunnel stops a running tunnel and starts it again, which is what a
// change to the connection settings needs: the settings are read when the
// tunnel is built, so an existing one keeps using the old ones. A stop that
// fails leaves the tunnel as it is, and a start that fails leaves nothing
// running under the key. Either way the next pass sees the difference again
// and tries again.
func (m *Manager) restartTunnel(want desiredTunnel) error {
	err := m.StopTunnel(want.host.ID, want.sp.ID)
	if err != nil && !errors.Is(err, ErrTunnelNotExist) {
		return err
	}

	return m.StartTunnel(want.host, want.sp, want.bindScope)
}

// Reconcile brings the running tunnels in line with the ones that should be
// running, once. What is missing is started and what is left over is stopped.
// A combination that is on both sides is left alone as long as it is running on
// the connection settings it should have, and stopped and started again when it
// is not, because those settings are only read when the tunnel is built. A
// database that cannot be read ends the pass before anything is changed.
func (m *Manager) Reconcile() (ReconcileResult, error) {
	var result ReconcileResult

	desired, err := m.desiredTunnels()
	if err != nil {
		return result, err
	}

	running := m.runningTunnelFingerprints()

	for key, want := range desired {
		current, isRunning := running[key]
		if !isRunning {
			err := m.StartTunnel(want.host, want.sp, want.bindScope)
			if err != nil {
				m.logger.Error("failed to start tunnel",
					logid.TunnelStartFailed.Field(),
					zap.Error(err),
					zap.String("host_ip", want.host.IP),
					zap.Int("service_port", want.sp.ServicePort))
				result.Failed++
				continue
			}

			result.Started++
			continue
		}

		// A credential that does not decrypt says nothing about whether the
		// settings changed, and a tunnel restarted on it could not be started
		// again. It keeps running on what it has. hostCredentials logs why.
		creds, err := m.hostCredentials(want.host)
		if err != nil {
			result.Failed++
			continue
		}

		if connectionFingerprint(want.host, want.sp, want.bindScope, creds) == current {
			continue
		}

		err = m.restartTunnel(want)
		if err != nil {
			m.logger.Error("failed to restart a tunnel whose connection settings changed",
				logid.TunnelRestartFailed.Field(),
				zap.Error(err),
				zap.Uint("host_id", want.host.ID),
				zap.Uint("sp_id", want.sp.ID))
			result.Failed++
			continue
		}

		result.Restarted++
	}

	for key := range running {
		_, want := desired[key]
		if want {
			continue
		}

		hostID, spID, ok := parseTunnelKey(key)
		if !ok {
			m.logger.Error("a running tunnel is registered under a key that cannot be read",
				logid.TunnelKeyUnreadable.Field(),
				zap.String("tunnel_key", key))
			result.Failed++
			continue
		}

		err := m.StopTunnel(hostID, spID)
		if err != nil {
			if errors.Is(err, ErrTunnelNotExist) {
				// Already gone, which is what this pass wanted.
				continue
			}

			m.logger.Error("failed to stop tunnel",
				logid.TunnelStopFailed.Field(),
				zap.Error(err),
				zap.Uint("host_id", hostID),
				zap.Uint("sp_id", spID))
			result.Failed++
			continue
		}

		result.Stopped++
	}

	return result, nil
}

// WakeReconcile asks for a reconcile pass. It never blocks and never waits for
// the pass, so a request handler can call it and answer right away. A wake-up
// that is already queued is left as it is, because a pass reads the state at
// the time it runs and so covers every change made before it.
func (m *Manager) WakeReconcile() {
	select {
	case m.reconcileWake <- struct{}{}:
	default:
	}
}

// reconcileInterval returns how long the loop waits for the next pass when no
// wake-up arrives.
func reconcileInterval(intervalSec int) time.Duration {
	if intervalSec <= 0 {
		intervalSec = defaultReconcileIntervalSec
	}

	return time.Duration(intervalSec) * time.Second
}

// RunReconcileLoop reconciles until ctx is done. It runs a pass right away and
// then whenever WakeReconcile is called or the interval elapses. A pass that
// could not read the database is skipped and the loop waits for the next
// reason to run, so a database that is briefly away does not end it.
func (m *Manager) RunReconcileLoop(ctx context.Context, intervalSec int) {
	ticker := time.NewTicker(reconcileInterval(intervalSec))
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		result, err := m.Reconcile()
		if err != nil {
			m.logger.Error("reconcile pass failed, waiting for the next one", logid.TunnelReconcileFailed.Field(), zap.Error(err))
		} else if result.Started > 0 || result.Stopped > 0 || result.Restarted > 0 || result.Failed > 0 {
			m.logger.Info("reconciled tunnels",
				logid.TunnelReconciled.Field(),
				zap.Int("started", result.Started),
				zap.Int("stopped", result.Stopped),
				zap.Int("restarted", result.Restarted),
				zap.Int("failed", result.Failed))
		}

		select {
		case <-ctx.Done():
			return
		case <-m.reconcileWake:
		case <-ticker.C:
		}
	}
}

// DesiredTunnelCount returns how many tunnels should be running. It counts the
// combinations a reconcile pass builds its desired state from, so it cannot
// drift from what the loop tries to start. A count above the number of tunnels
// that exist means a pass could not start all of them.
func (m *Manager) DesiredTunnelCount() (int, error) {
	desired, err := m.desiredTunnels()
	if err != nil {
		return 0, err
	}

	return len(desired), nil
}
