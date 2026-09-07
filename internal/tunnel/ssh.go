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
	Local    *net.TCPAddr
	Server   *net.TCPAddr
	Remote   *net.TCPAddr
	Config   *ssh.ClientConfig
	client   *ssh.Client
	clientMu sync.RWMutex
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
	_ = t.client.Close()
	t.client = nil
	t.clientMu.Unlock()

	if !t.markReconnecting(m, tunnel) {
		return
	}

	t.logger.Info("closed current connection, waiting for the tunnel to be re-established",
		zap.String("local", t.Local.String()),
		zap.String("server", t.Server.String()),
		zap.String("remote", t.Remote.String()))
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
			t.clientMu.RUnlock()

			if client != nil {
				conn, err := net.DialTimeout("tcp", t.Server.String(),
					time.Duration(m.monitoringIntervalSec)*time.Second)
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

				_, _, err = client.SendRequest("keepalive@tunnel", true, nil)
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

func (t *SSHTunnel) forward(localConn net.Conn) {
	defer func() {
		_ = localConn.Close()
	}()

	remoteConn, err := net.Dial("tcp", t.Remote.String())
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

	errc := make(chan error, 2)
	go func() {
		_, err := io.Copy(localConn, remoteConn)
		errc <- err
	}()
	go func() {
		_, err := io.Copy(remoteConn, localConn)
		errc <- err
	}()

	err = <-errc
	if err != nil && err != io.EOF {
		t.logger.Debug("copy error", zap.Error(err))
	}
}

func (t *SSHTunnel) establishConnection(m *Manager, tunnel *models.Tunnel) error {
	client, err := ssh.Dial("tcp", t.Server.String(), t.Config)
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
	t.clientMu.Unlock()

	// Addr reports the requested address with the port the server confirmed,
	// so only the port is known to be real here.
	localAddr := listener.Addr().String()

	if t.Local.IP.IsUnspecified() {
		t.logger.Info("a wildcard local address was requested, the SSH server binds it to loopback only unless GatewayPorts is enabled",
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
	t.saveTunnelStatus(m, tunnel)
	t.tunnelMu.Unlock()

	t.logger.Info("tunnel connected successfully",
		zap.String("local", t.Local.String()),
		zap.String("server", t.Server.String()),
		zap.String("remote", t.Remote.String()))

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
		go t.forward(conn)
	}
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

				if strings.Contains(err.Error(), "unable to authenticate") {
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
	}
	t.clientMu.Unlock()

	err := m.db.Where("host_id = ? and sp_id = ?", t.HostID, t.SPID).
		Delete(&models.Tunnel{}).Error
	if err != nil {
		return fmt.Errorf("failed to delete tunnel: %w", err)
	}

	return nil
}
