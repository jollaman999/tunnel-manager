package tunnel

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

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
}

// connFingerprint stands for the connection settings a tunnel was built from.
// It is an array so it can be compared with ==, and it is never formatted, so
// it cannot reach a log.
type connFingerprint [sha256.Size]byte

// connectionFingerprint derives the fingerprint of the settings a tunnel for
// this combination is built from: the server, remote and local addresses, the
// user and the password.
//
// The password goes in as the plaintext hostPassword returns, not as the value
// the row holds. The stored form is sealed with a fresh nonce every time it is
// written, so the same password would give a different fingerprint on every
// pass and every pass would restart the tunnel.
//
// Every value is written with its length in front, so that two different sets
// of settings cannot produce the same input to the hash.
func connectionFingerprint(host *models.Host, sp *models.ServicePort, password string) connFingerprint {
	local, server, remote := tunnelAddresses(host, sp)

	h := sha256.New()
	for _, value := range []string{server, remote, local, host.User, password} {
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
// on every Host that is enabled. A Host that is not enabled is left out here,
// so the tunnels of a Host that was just disabled count as running without
// being wanted and are stopped.
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

	desired := make(map[string]desiredTunnel)
	for i := range hosts {
		if !hosts[i].Enabled {
			continue
		}

		for j := range servicePorts {
			desired[tunnelKey(hosts[i].ID, servicePorts[j].ID)] = desiredTunnel{
				host: &hosts[i],
				sp:   &servicePorts[j],
			}
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

	return m.StartTunnel(want.host, want.sp)
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
			err := m.StartTunnel(want.host, want.sp)
			if err != nil {
				m.logger.Error("failed to start tunnel",
					zap.Error(err),
					zap.String("host_ip", want.host.IP),
					zap.Int("service_port", want.sp.ServicePort))
				result.Failed++
				continue
			}

			result.Started++
			continue
		}

		// A password that does not decrypt says nothing about whether the
		// settings changed, and a tunnel restarted on it could not be started
		// again. It keeps running on what it has. hostPassword logs why.
		password, err := m.hostPassword(want.host)
		if err != nil {
			result.Failed++
			continue
		}

		if connectionFingerprint(want.host, want.sp, password) == current {
			continue
		}

		err = m.restartTunnel(want)
		if err != nil {
			m.logger.Error("failed to restart a tunnel whose connection settings changed",
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
			m.logger.Error("reconcile pass failed, waiting for the next one", zap.Error(err))
		} else if result.Started > 0 || result.Stopped > 0 || result.Restarted > 0 || result.Failed > 0 {
			m.logger.Info("reconciled tunnels",
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
