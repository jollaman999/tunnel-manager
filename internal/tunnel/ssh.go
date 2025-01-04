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
	VMID   *uint
	SPID   *uint
	Local  *net.TCPAddr
	Server *net.TCPAddr
	Remote *net.TCPAddr
	Config *ssh.ClientConfig
	client *ssh.Client
	mu     sync.RWMutex
	done   chan bool
	logger *zap.Logger
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

	t.mu.Lock()
	if t.client != nil {
		_ = t.client.Close()
		t.client = nil
	}
	t.mu.Unlock()

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
			t.mu.RLock()
			client := t.client
			t.mu.RUnlock()

			if client != nil {
				_, _, err := client.SendRequest("keepalive@tunnel", true, nil)
				if err != nil {
					t.logger.Warn("connection check failed", zap.Error(err))
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

	t.mu.RLock()
	client := t.client
	t.mu.RUnlock()

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
		t.logger.Error("copy error", zap.Error(err))
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

	t.mu.Lock()
	t.client = client
	t.mu.Unlock()

	err = m.db.Model(&models.Tunnel{}).
		Where("vm_id = ?", t.VMID).
		Where("sp_id = ?", t.SPID).
		Update("status", "connected").
		Update("last_connected_at", time.Now()).Error
	if err != nil {
		m.logger.Error("failed to update tunnel connected status", zap.Error(err))
	}
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

func (t *SSHTunnel) Stop() error {
	close(t.done)
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.client != nil {
		err := t.client.Close()
		t.client = nil
		if err != nil {
			return fmt.Errorf("failed to close SSH client: %w", err)
		}
	}
	return nil
}
