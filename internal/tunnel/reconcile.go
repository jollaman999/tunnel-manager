package tunnel

import (
	"context"
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
	Failed  int
}

// desiredTunnel is one combination that should be running, with the rows
// StartTunnel needs, so the pass does not read them a second time.
type desiredTunnel struct {
	host *models.Host
	sp   *models.ServicePort
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

// Reconcile brings the running tunnels in line with the ones that should be
// running, once. What is missing is started, what is left over is stopped, and
// a combination that is on both sides is not touched, so a tunnel that is up
// is never restarted. A database that cannot be read ends the pass before
// anything is changed.
func (m *Manager) Reconcile() (ReconcileResult, error) {
	var result ReconcileResult

	desired, err := m.desiredTunnels()
	if err != nil {
		return result, err
	}

	running := m.runningTunnelKeys()
	isRunning := make(map[string]bool, len(running))
	for _, key := range running {
		isRunning[key] = true
	}

	for key, want := range desired {
		if isRunning[key] {
			continue
		}

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
	}

	for _, key := range running {
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
		} else if result.Started > 0 || result.Stopped > 0 || result.Failed > 0 {
			m.logger.Info("reconciled tunnels",
				zap.Int("started", result.Started),
				zap.Int("stopped", result.Stopped),
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
