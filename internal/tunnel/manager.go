package tunnel

import (
	"fmt"
	"sync"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
	"gorm.io/gorm"
)

type Manager struct {
	db                    *gorm.DB
	tunnels               map[string]*SSHTunnel
	mu                    sync.RWMutex
	logger                *zap.Logger
	monitoringIntervalSec int
}

func NewManager(db *gorm.DB, logger *zap.Logger, monitoringIntervalSec int) (*Manager, error) {
	return &Manager{
		db:                    db,
		tunnels:               make(map[string]*SSHTunnel),
		logger:                logger,
		monitoringIntervalSec: monitoringIntervalSec,
	}, nil
}

func (m *Manager) StartTunnel(host *models.Host, sp *models.ServicePort) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	tunnelKey := fmt.Sprintf("%d-%d", host.ID, sp.ID)
	if _, exists := m.tunnels[tunnelKey]; exists {
		return fmt.Errorf("tunnel already exists")
	}

	sshConfig := &ssh.ClientConfig{
		User: host.User,
		Auth: []ssh.AuthMethod{
			ssh.Password(host.Password),
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

	m.tunnels[tunnelKey] = t

	err = m.db.Where("host_id = ? AND sp_id = ?", host.ID, sp.ID).
		Attrs(tunnel).
		FirstOrCreate(&tunnel).Error
	if err != nil {
		return fmt.Errorf("failed to create tunnel information: %w", err)
	}

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
		return fmt.Errorf("tunnel does not exist")
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
	}

	var servicePorts []models.ServicePort
	err = m.db.Find(&servicePorts).Error
	if err != nil {
		m.mu.Unlock()
		m.logger.Error(fmt.Sprintf("failed to fetch service ports: %v", err))
	}
	m.mu.Unlock()

	for _, host := range hosts {
		err = m.db.Unscoped().Where("host_id = ?", host.ID).Delete(&models.Tunnel{}).Error
		if err != nil {
			m.logger.Error(fmt.Sprintf("failed to reset tunnel status for host_id=%d: %w", host.ID, err))
		}

		for _, sp := range servicePorts {
			err = m.StopTunnel(host.ID, sp.ID)
			if err != nil {
				m.logger.Error("failed to stop tunnel",
					zap.Error(err),
					zap.String("host_ip", host.IP),
					zap.Int("service_port", sp.ServicePort))
				continue
			}
		}
	}
}
