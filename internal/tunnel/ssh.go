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
	"sync"
	"time"
)

type SSHTunnel struct {
	VMID      *uint
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

func NewSSHTunnel(vmID, spID *uint, localAddr, serverAddr, remoteAddr string, sshConfig *ssh.ClientConfig, logger *zap.Logger) (*SSHTunnel, error) {
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
		VMID:   vmID,
		SPID:   spID,
		Local:  local,
		Server: server,
		Remote: remote,
		Config: sshConfig,
		done:   make(chan bool),
		logger: logger,
	}, nil
}

func (t *SSHTunnel) reconnect(m *Manager) {
	t.stopMu.Lock()
	if t.isStopped {
		t.stopMu.Unlock()
		return
	}
	t.stopMu.Unlock()

	var tunnel models.Tunnel
	err := m.db.Model(&models.Tunnel{}).
		Where("vm_id = ?", t.VMID).
		Where("sp_id = ?", t.SPID).
		First(&tunnel).Error
	if err != nil {
		m.logger.Error("failed to get previous tunnel", zap.Error(err))
	}

	err = m.db.Model(&models.Tunnel{}).
		Where("vm_id = ?", t.VMID).
		Where("sp_id = ?", t.SPID).
		Update("status", "reconnecting").
		Update("retry_count", tunnel.RetryCount+1).Error
	if err != nil {
		m.logger.Error("failed to update tunnel connected status", zap.Error(err))
	}

	t.clientMu.Lock()
	if t.client != nil {
		_ = t.client.Close()
		t.client = nil
	}
	t.clientMu.Unlock()

	err = t.establishConnection(m)
	if err != nil {
		t.logger.Error("reconnection failed",
			zap.String("server", t.Server.String()),
			zap.Error(err))
		return
	}

	t.logger.Info("reconnection successful",
		zap.String("server", t.Server.String()))
}

func (t *SSHTunnel) monitorConnection(m *Manager) {
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
						zap.String("server", t.Server.String()),
						zap.Error(err))
					t.reconnect(m)
					continue
				}
				_ = conn.Close()

				_, _, err = client.SendRequest("keepalive@tunnel", true, nil)
				if err != nil {
					t.logger.Warn("SSH keepalive check failed, attempting reconnection",
						zap.String("server", t.Server.String()),
						zap.Error(err))
					t.reconnect(m)
				}
			}
		}
	}
}

func (t *SSHTunnel) forward(localConn net.Conn) {
	defer func() {
		_ = localConn.Close()
	}()

	t.clientMu.RLock()
	client := t.client
	t.clientMu.RUnlock()

	if client == nil {
		t.logger.Error("ssh client is nil during forward")
		return
	}

	remoteConn, err := client.Dial("tcp", t.Remote.String())
	if err != nil {
		t.logger.Error("failed to dial remote service",
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

func (t *SSHTunnel) establishConnection(m *Manager) error {
	client, err := ssh.Dial("tcp", t.Server.String(), t.Config)
	if err != nil {
		return fmt.Errorf("failed to establish SSH connection: %w", err)
	}

	listener, err := client.Listen("tcp", t.Local.String())
	if err != nil {
		err = fmt.Errorf("failed to start remote listener: %w", err)

		err = m.db.Model(&models.Tunnel{}).
			Where("vm_id = ?", t.VMID).
			Where("sp_id = ?", t.SPID).
			Update("status", "error").
			Update("last_error", err.Error()).Error
		if err != nil {
			m.logger.Error("failed to update tunnel connected status", zap.Error(err))
		}

		return err
	}
	defer func() {
		_ = listener.Close()
	}()

	t.clientMu.Lock()
	t.client = client
	t.clientMu.Unlock()

	err = m.db.Model(&models.Tunnel{}).
		Where("vm_id = ?", t.VMID).
		Where("sp_id = ?", t.SPID).
		Update("status", "connected").
		Update("last_connected_at", time.Now()).Error
	if err != nil {
		m.logger.Error("failed to update tunnel connected status", zap.Error(err))
	}

	t.logger.Info("tunnel connected successfully",
		zap.String("local", t.Local.String()),
		zap.String("server", t.Server.String()),
		zap.String("remote", t.Remote.String()))

	go t.monitorConnection(m)

	for {
		conn, err := listener.Accept()
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Temporary() {
				t.logger.Warn("temporary accept error", zap.Error(err))
				time.Sleep(time.Second)
				continue
			}
			return fmt.Errorf("listener accept error: %w", err)
		}
		go t.forward(conn)
	}
}

func (t *SSHTunnel) Start(m *Manager) error {
	t.logger.Info("attempting to start tunnel",
		zap.String("local", t.Local.String()),
		zap.String("server", t.Server.String()),
		zap.String("remote", t.Remote.String()))

	for {
		select {
		case <-t.done:
			return nil
		default:
			err := t.establishConnection(m)
			if err != nil {
				t.logger.Error("connection failed, retrying in "+strconv.Itoa(m.monitoringIntervalSec)+" seconds",
					zap.Error(err))
				time.Sleep(time.Duration(m.monitoringIntervalSec) * time.Second)
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
	t.isStopped = true
	t.stopMu.Unlock()

	close(t.done)

	t.clientMu.Lock()
	if t.client != nil {
		_ = t.client.Close()
		t.client = nil
	}
	t.clientMu.Unlock()

	tx := m.db.Begin()
	err := tx.Error
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}

	var tunnel models.Tunnel
	err = tx.Set("gorm:pessimistic_lock", true).
		Where("vm_id = ? and sp_id = ?", t.VMID, t.SPID).
		First(&tunnel).Error
	if err != nil {
		return fmt.Errorf("failed to lock tunnel: %w", err)
	}

	err = tx.Unscoped().Where("vm_id = ? and sp_id = ?", t.VMID, t.SPID).
		Delete(&models.Tunnel{}).Error
	if err != nil {
		return fmt.Errorf("failed to delete tunnel: %w", err)
	}

	err = tx.Commit().Error
	if err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}
