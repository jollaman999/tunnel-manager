package tunnel

import (
	"fmt"
	"net"
	"strconv"

	"github.com/jollaman999/tunnel-manager/internal/alert"
	"github.com/jollaman999/tunnel-manager/internal/models"
)

// statusConnected is the status a tunnel row carries while its connection
// stands, the word local forwards and SOCKS5 proxies report as well.
const statusConnected = localStatusConnected

// serverHost is the Host part of a server address, which is the address the
// Host was registered under.
func serverHost(server string) string {
	host, _, err := net.SplitHostPort(server)
	if err != nil {
		return server
	}

	return host
}

// runningTunnel is what an alert needs of a running tunnel that the row does
// not say plainly.
type runningTunnel struct {
	host string
	port int
}

// AlertConditions returns every running tunnel, local forward and SOCKS5 proxy
// with whether it is connected, which is what the alert watcher scans.
//
// Only what runs is returned. A forward that is switched off, removed or on a
// disabled Host is not running, and that is not an outage. A tunnel that runs
// and has no row yet, which is one being started or stopped at this moment,
// is left out until the next scan rather than reported as either.
//
// The status of a tunnel is read from its row, which is what the Status screen
// reads, while local forwards and proxies report theirs from memory.
func (m *Manager) AlertConditions() ([]alert.Condition, error) {
	m.mu.RLock()
	running := make(map[string]runningTunnel, len(m.tunnels))
	for key, t := range m.tunnels {
		running[key] = runningTunnel{host: serverHost(t.Server), port: t.Local.port()}
	}
	m.mu.RUnlock()

	var conditions []alert.Condition

	if len(running) > 0 {
		var rows []models.Tunnel

		err := m.db.Find(&rows).Error
		if err != nil {
			return nil, fmt.Errorf("failed to fetch tunnels: %w", err)
		}

		for _, row := range rows {
			key := tunnelKey(row.HostID, row.SPID)

			t, ok := running[key]
			if !ok {
				continue
			}

			conditions = append(conditions, alert.Condition{
				Key:       alert.KindServicePort + ":" + key,
				Kind:      alert.KindServicePort,
				Host:      t.host,
				LocalPort: t.port,
				Connected: row.Status == statusConnected,
				LastError: row.LastError,
			})
		}
	}

	m.localMu.RLock()
	forwards := make(map[LocalForwardKey]*localTunnel, len(m.localForwards))
	for key, f := range m.localForwards {
		forwards[key] = f
	}
	m.localMu.RUnlock()

	for key, f := range forwards {
		state := f.snapshot()

		conditions = append(conditions, alert.Condition{
			Key:       alert.KindLocalForward + ":" + strconv.FormatUint(uint64(key.HostID), 10) + "-" + strconv.FormatUint(uint64(key.Number), 10),
			Kind:      alert.KindLocalForward,
			Host:      serverHost(f.server),
			LocalPort: f.listen.port(),
			Connected: state.Status == statusConnected,
			LastError: state.LastError,
		})
	}

	m.socksMu.RLock()
	proxies := make(map[uint]*socksTunnel, len(m.socksProxies))
	for id, p := range m.socksProxies {
		proxies[id] = p
	}
	m.socksMu.RUnlock()

	for id, p := range proxies {
		state := p.snapshot()

		conditions = append(conditions, alert.Condition{
			Key:       alert.KindSocks + ":" + strconv.FormatUint(uint64(id), 10),
			Kind:      alert.KindSocks,
			Host:      serverHost(p.server),
			LocalPort: p.listen.port(),
			Connected: state.Status == statusConnected,
			LastError: state.LastError,
		})
	}

	return conditions, nil
}
