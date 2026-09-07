package tunnel

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
	"gorm.io/gorm"
)

// ErrTunnelNotExist reports that the tunnel to stop is not running. Stopping a
// tunnel that was never started is not a failure, so callers that stop tunnels
// in bulk can tell it apart with errors.Is.
var ErrTunnelNotExist = errors.New("tunnel does not exist")

type Manager struct {
	db                    *gorm.DB
	tunnels               map[string]*SSHTunnel
	mu                    sync.RWMutex
	logger                *zap.Logger
	cipher                *crypto.Cipher
	monitoringIntervalSec int
}

func NewManager(db *gorm.DB, logger *zap.Logger, cipher *crypto.Cipher, monitoringIntervalSec int) (*Manager, error) {
	return &Manager{
		db:                    db,
		tunnels:               make(map[string]*SSHTunnel),
		logger:                logger,
		cipher:                cipher,
		monitoringIntervalSec: monitoringIntervalSec,
	}, nil
}

// hostPassword returns the password to authenticate to the Host with. A stored
// value that was never encrypted is stored encrypted before it is used, and so
// is one that is still in the encrypted format that carried no marker. A value
// that is encrypted but does not open with the key in use is left exactly as it
// is and reported as an error, because overwriting it destroys the password.
func (m *Manager) hostPassword(host *models.Host) (string, error) {
	password, err := m.cipher.Decrypt(host.Password)
	if err == nil {
		if !crypto.IsEncrypted(host.Password) {
			m.storeEncryptedPassword(host, password)
		}
		return password, nil
	}

	if !errors.Is(err, crypto.ErrNotEncrypted) {
		m.logger.Error("the stored password of the Host does not decrypt with the encryption key in use, "+
			"leaving it as it is. Check that the configured key file is the one the password was stored with",
			zap.Uint("host_id", host.ID),
			zap.String("host_ip", host.IP),
			zap.Error(err))
		return "", fmt.Errorf("failed to decrypt the stored password of the Host (host_id=%d): %w", host.ID, err)
	}

	password = host.Password
	m.storeEncryptedPassword(host, password)

	return password, nil
}

// storeEncryptedPassword writes the encrypted form of password to the Host row.
// Failing to store it does not keep the password from being used, so it is only
// logged.
func (m *Manager) storeEncryptedPassword(host *models.Host, password string) {
	encrypted, err := m.cipher.Encrypt(password)
	if err != nil {
		m.logger.Warn("failed to encrypt the stored password of the Host",
			zap.Uint("host_id", host.ID),
			zap.String("host_ip", host.IP),
			zap.Error(err))
		return
	}

	err = m.db.Model(&models.Host{}).Where("id = ?", host.ID).Update("password", encrypted).Error
	if err != nil {
		m.logger.Warn("failed to store the encrypted password of the Host",
			zap.Uint("host_id", host.ID),
			zap.String("host_ip", host.IP),
			zap.Error(err))
		return
	}

	host.Password = encrypted
	m.logger.Info("replaced the stored password of the Host with an encrypted one",
		zap.Uint("host_id", host.ID),
		zap.String("host_ip", host.IP))
}

func (m *Manager) StartTunnel(host *models.Host, sp *models.ServicePort) error {
	if !host.Enabled {
		m.logger.Info("skipped starting tunnel for disabled Host",
			zap.Uint("host_id", host.ID),
			zap.String("host_ip", host.IP),
			zap.Int("service_port", sp.ServicePort))
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	tunnelKey := fmt.Sprintf("%d-%d", host.ID, sp.ID)
	if _, exists := m.tunnels[tunnelKey]; exists {
		return fmt.Errorf("tunnel already exists")
	}

	password, err := m.hostPassword(host)
	if err != nil {
		return err
	}

	sshConfig := &ssh.ClientConfig{
		User: host.User,
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Second * 10,
	}

	tunnel := models.Tunnel{
		HostID: host.ID,
		SPID:   sp.ID,
		Status: "starting",
		Local:  fmt.Sprintf("0.0.0.0:%d", sp.LocalPort),
		Server: fmt.Sprintf("%s:%d", host.IP, host.Port),
		Remote: fmt.Sprintf("%s:%d", sp.ServiceIP, sp.ServicePort),
	}

	t, err := NewSSHTunnel(
		&tunnel.HostID,
		&tunnel.SPID,
		tunnel.Local,
		tunnel.Server,
		tunnel.Remote,
		sshConfig,
		m.logger,
	)
	if err != nil {
		return fmt.Errorf("failed to create tunnel: %w", err)
	}

	err = m.db.Where("host_id = ? AND sp_id = ?", host.ID, sp.ID).
		Attrs(tunnel).
		FirstOrCreate(&tunnel).Error
	if err != nil {
		return fmt.Errorf("failed to create tunnel information: %w", err)
	}

	// Registered only once nothing is left that can fail, so a tunnel that was
	// not started does not keep the key taken.
	m.tunnels[tunnelKey] = t

	go func(m *Manager, t *SSHTunnel, tunnel *models.Tunnel) {
		t.Start(m, tunnel)
	}(m, t, &tunnel)

	return nil
}

func (m *Manager) StopTunnel(hostID uint, spID uint) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	tunnelKey := fmt.Sprintf("%d-%d", hostID, spID)
	tunnel, exists := m.tunnels[tunnelKey]
	if !exists {
		return ErrTunnelNotExist
	}

	err := tunnel.Stop(m)
	if err != nil {
		return fmt.Errorf("failed to stop tunnel: %w", err)
	}

	delete(m.tunnels, tunnelKey)

	return nil
}

func (m *Manager) GetHostTunnels(hostID uint) (*[]models.Tunnel, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var tunnels []models.Tunnel
	err := m.db.Where("host_id = ?", hostID).Find(&tunnels).Error
	if err != nil {
		m.logger.Error(fmt.Sprintf("failed to fetch Host's tunnels (host_id=%d)", hostID), zap.Error(err))
		return nil, fmt.Errorf("failed to fetch Host's tunnels (host_id=%d): %w", hostID, err)
	}

	return &tunnels, nil
}

func (m *Manager) GetAllTunnels() (*[]models.Tunnel, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var tunnels []models.Tunnel
	err := m.db.Find(&tunnels).Error
	if err != nil {
		m.logger.Error("failed to fetch tunnels", zap.Error(err))
		return nil, fmt.Errorf("failed to fetch tunnels: %w", err)
	}

	return &tunnels, nil
}

func (m *Manager) RestoreAllTunnels() error {
	m.mu.Lock()
	var hosts []models.Host
	err := m.db.Find(&hosts).Error
	if err != nil {
		m.mu.Unlock()
		m.logger.Error("failed to fetch Hosts", zap.Error(err))
		return fmt.Errorf("failed to fetch hosts: %w", err)
	}

	if len(hosts) == 0 {
		m.mu.Unlock()
		m.logger.Info("no Hosts to restore")
		return nil
	}

	var servicePorts []models.ServicePort
	err = m.db.Find(&servicePorts).Error
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("failed to fetch service ports: %w", err)
	}

	if len(servicePorts) == 0 {
		m.mu.Unlock()
		m.logger.Info("no service ports to restore")
		return nil
	}

	m.mu.Unlock()

	for _, host := range hosts {
		err = m.db.Unscoped().Where("host_id = ?", host.ID).Delete(&models.Tunnel{}).Error
		if err != nil {
			return fmt.Errorf("failed to reset tunnel status for host_id=%d: %w", host.ID, err)
		}

		for _, sp := range servicePorts {
			err = m.StartTunnel(&host, &sp)
			if err != nil {
				m.logger.Error("failed to restore tunnel",
					zap.Error(err),
					zap.String("host_ip", host.IP),
					zap.Int("service_port", sp.ServicePort))
				continue
			}
		}
	}

	return nil
}

func (m *Manager) StopAllTunnels() {
	m.mu.Lock()
	var hosts []models.Host
	err := m.db.Find(&hosts).Error
	if err != nil {
		m.mu.Unlock()
		m.logger.Error("failed to fetch Hosts", zap.Error(err))
		return
	}

	var servicePorts []models.ServicePort
	err = m.db.Find(&servicePorts).Error
	if err != nil {
		m.mu.Unlock()
		m.logger.Error(fmt.Sprintf("failed to fetch service ports: %v", err))
		return
	}
	m.mu.Unlock()

	for _, host := range hosts {
		err = m.db.Unscoped().Where("host_id = ?", host.ID).Delete(&models.Tunnel{}).Error
		if err != nil {
			m.logger.Error(fmt.Sprintf("failed to reset tunnel status for host_id=%d", host.ID), zap.Error(err))
		}

		for _, sp := range servicePorts {
			err = m.StopTunnel(host.ID, sp.ID)
			if err != nil {
				if errors.Is(err, ErrTunnelNotExist) {
					m.logger.Debug("no tunnel to stop",
						zap.String("host_ip", host.IP),
						zap.Int("service_port", sp.ServicePort))
					continue
				}

				m.logger.Error("failed to stop tunnel",
					zap.Error(err),
					zap.String("host_ip", host.IP),
					zap.Int("service_port", sp.ServicePort))
				continue
			}
		}
	}
}
