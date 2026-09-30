package tunnel

import (
	"errors"

	"github.com/jollaman999/tunnel-manager/internal/logid"
	"go.uber.org/zap"
)

// ReconnectCounts is how many forwards of one Host were running when it was
// asked to reconnect, which is how many the next pass drops and makes again.
type ReconnectCounts struct {
	Tunnels       int `json:"tunnels"`
	LocalForwards int `json:"local_forwards"`
	Socks         int `json:"socks"`
}

// RequestReconnect asks for every connection of one Host to be dropped and made
// again: its service port tunnels, its local forwards and its SOCKS5 proxy.
//
// It is what a change made on the Host needs and nothing here can see. The SSH
// server reads its configuration once for each connection, so a forward that
// was opened before GatewayPorts was changed stays where it was opened, and a
// request for it again on the same connection is answered under the old
// setting. Only a new connection is served under the new one.
//
// Nothing is stopped here. The Host is written down and the loop is woken, and
// the pass that follows stops and starts what runs over it, the way it does
// for a Host whose settings changed. What comes back is what was running now,
// so that a Host with nothing running can be told so.
func (m *Manager) RequestReconnect(hostID uint) ReconnectCounts {
	counts := m.runningOf(hostID)

	m.reconnectMu.Lock()
	m.reconnectHosts[hostID] = true
	m.reconnectMu.Unlock()

	m.WakeReconcile()

	return counts
}

// runningOf counts what runs over one Host.
func (m *Manager) runningOf(hostID uint) ReconnectCounts {
	var counts ReconnectCounts

	for _, key := range m.runningTunnelKeys() {
		if id, _, ok := parseTunnelKey(key); ok && id == hostID {
			counts.Tunnels++
		}
	}

	for key := range m.runningLocalForwardFingerprints() {
		if key.HostID == hostID {
			counts.LocalForwards++
		}
	}

	if _, ok := m.runningSocksFingerprints()[hostID]; ok {
		counts.Socks = 1
	}

	return counts
}

// takeReconnects hands back the Hosts asked to reconnect since the last pass
// and empties the set.
func (m *Manager) takeReconnects() map[uint]bool {
	m.reconnectMu.Lock()
	defer m.reconnectMu.Unlock()

	taken := m.reconnectHosts
	m.reconnectHosts = make(map[uint]bool)

	return taken
}

// dropReconnecting stops everything that runs over the Hosts asked to
// reconnect. The pass it is called from starts them again, since they are
// still wanted and are no longer running, and each stop is counted as a
// restart of what the pass then starts.
//
// A stop that fails leaves that forward running on its old connection, which
// is logged; the Host is not asked about again, since the request was served
// as far as it could be.
func (m *Manager) dropReconnecting(result *ReconcileResult) {
	hosts := m.takeReconnects()
	if len(hosts) == 0 {
		return
	}

	for _, key := range m.runningTunnelKeys() {
		hostID, spID, ok := parseTunnelKey(key)
		if !ok || !hosts[hostID] {
			continue
		}

		err := m.StopTunnel(hostID, spID)
		if err != nil && !errors.Is(err, ErrTunnelNotExist) {
			m.logger.Error("failed to stop a tunnel to reconnect it",
				logid.TunnelStopFailed.Field(),
				zap.Error(err),
				zap.Uint("host_id", hostID),
				zap.Uint("sp_id", spID))
			result.Failed++
		}
	}

	for key := range m.runningLocalForwardFingerprints() {
		if !hosts[key.HostID] {
			continue
		}

		err := m.stopLocalForward(key)
		if err != nil && !errors.Is(err, ErrTunnelNotExist) {
			m.logger.Error("failed to stop a local forward to reconnect it",
				logid.TunnelStopFailed.Field(),
				zap.Error(err),
				zap.Uint("host_id", key.HostID),
				zap.Uint("number", key.Number))
			result.Failed++
		}
	}

	for hostID := range m.runningSocksFingerprints() {
		if !hosts[hostID] {
			continue
		}

		err := m.stopSocks(hostID)
		if err != nil && !errors.Is(err, ErrTunnelNotExist) {
			m.logger.Error("failed to stop a SOCKS5 proxy to reconnect it",
				logid.TunnelStopFailed.Field(),
				zap.Error(err),
				zap.Uint("host_id", hostID))
			result.Failed++
		}
	}

	for hostID := range hosts {
		m.logger.Info("dropped the connections of a Host to make them again",
			logid.TunnelReconnectRequested.Field(),
			zap.Uint("host_id", hostID))
	}
}
