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

type SSHTunnel struct {
	HostID    *uint
	SPID      *uint
	Local     *net.TCPAddr
	Server    *net.TCPAddr
	Remote    *net.TCPAddr
	Config    *ssh.ClientConfig
	client    *ssh.Client
	clientMu  sync.RWMutex
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

func (t *SSHTunnel) reconnect(m *Manager, tunnel *models.Tunnel) {
	t.stopMu.Lock()
	if t.isStopped {
		t.stopMu.Unlock()
		return
	}
	t.stopMu.Unlock()

	tunnel.Status = "reconnecting"
	tunnel.RetryCount++
	t.saveTunnelStatus(m, tunnel)

	t.clientMu.Lock()
	if t.client != nil {
		_ = t.client.Close()
		t.client = nil
	}
	t.clientMu.Unlock()

	err := t.establishConnection(m, tunnel)
	if err != nil {
		t.logger.Error("reconnection failed",
			zap.String("local", t.Local.String()),
			zap.String("server", t.Server.String()),
			zap.String("remote", t.Remote.String()),
			zap.Error(err))
		return
	}

	t.logger.Info("reconnection successful",
		zap.String("local", t.Local.String()),
		zap.String("server", t.Server.String()),
		zap.String("remote", t.Remote.String()))
}

func (t *SSHTunnel) monitorConnection(m *Manager, tunnel *models.Tunnel) {
	ticker := time.NewTicker(time.Duration(m.monitoringIntervalSec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-t.done:
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
					t.reconnect(m, tunnel)
					continue
				}
				_ = conn.Close()

				_, _, err = client.SendRequest("keepalive@tunnel", true, nil)
				if err != nil {
					t.logger.Warn("SSH keepalive check failed, attempting reconnection",
						zap.String("server", t.Server.String()),
						zap.Error(err))
					t.reconnect(m, tunnel)
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

		tunnel.Status = "error"
		tunnel.LastError = err.Error()
		t.saveTunnelStatus(m, tunnel)

		return fmt.Errorf("failed to establish SSH connection: %w", err)
	}

	listener, err := client.Listen("tcp", t.Local.String())
	if err != nil {
		m.logger.Error("failed to start remote listener",
			zap.String("local", t.Local.String()),
			zap.String("server", t.Server.String()),
			zap.String("remote", t.Remote.String()), zap.Error(err))

		tunnel.Status = "error"
		tunnel.LastError = err.Error()
		t.saveTunnelStatus(m, tunnel)

		return fmt.Errorf("failed to start remote listener: %w", err)
	}
	defer func() {
		_ = listener.Close()
	}()

	t.clientMu.Lock()
	t.client = client
	t.clientMu.Unlock()

	tunnel.Status = "connected"
	tunnel.RetryCount = 0
	tunnel.LastError = ""
	tunnel.LastConnectedAt = time.Now()
	t.saveTunnelStatus(m, tunnel)

	t.logger.Info("tunnel connected successfully",
		zap.String("local", t.Local.String()),
		zap.String("server", t.Server.String()),
		zap.String("remote", t.Remote.String()))

	go t.monitorConnection(m, tunnel)

	for {
		conn, err := listener.Accept()
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Temporary() {
				t.logger.Warn("temporary accept error",
					zap.String("local", t.Local.String()),
					zap.String("server", t.Server.String()),
					zap.String("remote", t.Remote.String()), zap.Error(err))
				time.Sleep(time.Second)
				continue
			}

			if err == io.EOF {
				t.logger.Info("connection closed",
					zap.String("local", t.Local.String()),
					zap.String("server", t.Server.String()),
					zap.String("remote", t.Remote.String()))
				return nil
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

func (t *SSHTunnel) Start(m *Manager, tunnel *models.Tunnel) {
	t.logger.Info("attempting to start tunnel",
		zap.String("local", t.Local.String()),
		zap.String("server", t.Server.String()),
		zap.String("remote", t.Remote.String()))

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

				time.Sleep(time.Duration(m.monitoringIntervalSec) * time.Second)

				tunnel.Status = "reconnecting"
				tunnel.RetryCount++
				t.saveTunnelStatus(m, tunnel)
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
