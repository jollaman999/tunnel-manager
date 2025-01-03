package tunnel

import (
	"errors"
	"fmt"
	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"time"
)

type SSHTunnel struct {
	Local  *net.TCPAddr
	Server *net.TCPAddr
	Remote *net.TCPAddr
	Config *ssh.ClientConfig
	client *ssh.Client
	mu     sync.RWMutex
	done   chan bool
	logger *zap.Logger
}

func NewSSHTunnel(localAddr, serverAddr, remoteAddr string, sshConfig *ssh.ClientConfig, logger *zap.Logger) (*SSHTunnel, error) {
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
		Local:  local,
		Server: server,
		Remote: remote,
		Config: sshConfig,
		done:   make(chan bool),
		logger: logger,
	}, nil
}

func (t *SSHTunnel) Start(monitoringIntervalSec int) error {
	t.logger.Info("attempting to start tunnel",
		zap.String("local", t.Local.String()),
		zap.String("server", t.Server.String()),
		zap.String("remote", t.Remote.String()))

	for {
		select {
		case <-t.done:
			return nil
		default:
			err := t.establishConnection(monitoringIntervalSec)
			if err != nil {
				t.logger.Error("connection failed, retrying in "+strconv.Itoa(monitoringIntervalSec)+" seconds",
					zap.Error(err))
				time.Sleep(time.Duration(monitoringIntervalSec) * time.Second)
				continue
			}
		}
	}
}

func (t *SSHTunnel) establishConnection(monitoringIntervalSec int) error {
	// Initialize SSH connection
	client, err := ssh.Dial("tcp", t.Server.String(), t.Config)
	if err != nil {
		return fmt.Errorf("failed to establish SSH connection: %w", err)
	}

	listener, err := client.Listen("tcp", t.Local.String())
	if err != nil {
		return fmt.Errorf("failed to start remote listener: %w", err)
	}
	defer func() {
		_ = listener.Close()
	}()

	t.mu.Lock()
	t.client = client
	t.mu.Unlock()

	t.logger.Info("tunnel connected successfully",
		zap.String("local", t.Local.String()),
		zap.String("server", t.Server.String()),
		zap.String("remote", t.Remote.String()))

	// Connection monitoring
	go t.monitorConnection(monitoringIntervalSec)

	// Handle incoming connections
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

func (t *SSHTunnel) monitorConnection(monitoringIntervalSec int) {
	ticker := time.NewTicker(time.Duration(monitoringIntervalSec) * time.Second)
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
					t.reconnect(monitoringIntervalSec)
				}
			}
		}
	}
}

func (t *SSHTunnel) reconnect(monitoringIntervalSec int) {
	t.mu.Lock()
	if t.client != nil {
		_ = t.client.Close()
		t.client = nil
	}
	t.mu.Unlock()

	err := t.establishConnection(monitoringIntervalSec)
	if err != nil {
		t.logger.Error("reconnection failed",
			zap.String("server", t.Server.String()),
			zap.Error(err))
		return
	}

	t.logger.Info("reconnection successful",
		zap.String("server", t.Server.String()))
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

	// Bidirectional copy
	errc := make(chan error, 2)
	go func() {
		_, err := io.Copy(localConn, remoteConn)
		errc <- err
	}()
	go func() {
		_, err := io.Copy(remoteConn, localConn)
		errc <- err
	}()

	// Wait for either copy to complete
	err = <-errc
	if err != nil && err != io.EOF {
		t.logger.Error("copy error", zap.Error(err))
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

func loadPrivateKey(path string) (ssh.Signer, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read private key: %w", err)
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %w", err)
	}

	return signer, nil
}
