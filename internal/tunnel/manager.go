package tunnel

import (
	"fmt"
	"strings"
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

func (m *Manager) StartTunnel(vm *models.VM, sp *models.ServicePort) error {
	tunnelKey := fmt.Sprintf("%d-%d", vm.ID, sp.LocalPort)

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.tunnels[tunnelKey]; exists {
		return fmt.Errorf("tunnel already exists")
	}

	sshConfig := &ssh.ClientConfig{
		User: vm.User,
		Auth: []ssh.AuthMethod{
			ssh.Password(vm.Password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Second * 10,
	}

	tunnel, err := NewSSHTunnel(
		fmt.Sprintf("127.0.0.1:%d", sp.LocalPort),
		fmt.Sprintf("%s:%d", vm.IP, vm.Port),
		fmt.Sprintf("%s:%d", sp.ServiceIP, sp.ServicePort),
		sshConfig,
		m.logger,
	)
	if err != nil {
		return fmt.Errorf("failed to create tunnel: %w", err)
	}

	m.tunnels[tunnelKey] = tunnel

	status := &models.Tunnel{
		VMID:   vm.ID,
		Status: "starting",
	}

	err = m.db.Create(status).Error
	if err != nil {
		return fmt.Errorf("failed to create tunnel status: %w", err)
	}

	go func(m *Manager, tunnel *SSHTunnel, status *models.Tunnel) {
		err := tunnel.Start(m, status)
		if err != nil {
			m.logger.Error("tunnel error",
				zap.Uint("vm_id", vm.ID),
				zap.Int("local_port", sp.LocalPort),
				zap.Error(err))

			status.Status = "error"
			status.LastError = err.Error()

			err = m.db.Save(status).Error
			if err != nil {
				m.logger.Error("failed to update tunnel error status", zap.Error(err))
			}
			return
		}
	}(m, tunnel, status)

	return nil
}

func (m *Manager) StopTunnel(vmID uint, spID uint) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var sp models.ServicePort
	err := m.db.First(&sp, spID).Error
	if err != nil {
		return fmt.Errorf("service port not found: %w", err)
	}

	tunnelKey := fmt.Sprintf("%d-%d", vmID, sp.LocalPort)
	tunnel, exists := m.tunnels[tunnelKey]
	if !exists {
		return fmt.Errorf("tunnel does not exist")
	}

	err = tunnel.Stop()
	if err != nil {
		return fmt.Errorf("failed to stop tunnel: %w", err)
	}

	delete(m.tunnels, tunnelKey)

	err = m.db.Model(&models.Tunnel{}).
		Where("vm_id = ?", vmID).
		Update("status", "stopped").Error
	if err != nil {
		return fmt.Errorf("failed to update tunnel status: %w", err)
	}

	return nil
}

func (m *Manager) RestartTunnel(vmID uint, spID uint) error {
	err := m.StopTunnel(vmID, spID)
	if err != nil {
		m.logger.Warn("failed to stop tunnel for restart",
			zap.Error(err))
	}

	var vm models.VM
	err = m.db.First(&vm, vmID).Error
	if err != nil {
		return fmt.Errorf("vm not found: %w", err)
	}

	var sp models.ServicePort
	err = m.db.First(&sp, spID).Error
	if err != nil {
		return fmt.Errorf("service port not found: %w", err)
	}

	return m.StartTunnel(&vm, &sp)
}

func (m *Manager) GetActiveTunnels() map[string]*SSHTunnel {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tunnels := make(map[string]*SSHTunnel)
	for k, v := range m.tunnels {
		tunnels[k] = v
	}

	return tunnels
}

func (m *Manager) GetVMActiveTunnels(vmID uint) map[string]*SSHTunnel {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tunnels := make(map[string]*SSHTunnel)
	prefix := fmt.Sprintf("%d-", vmID)

	for k, v := range m.tunnels {
		if strings.HasPrefix(k, prefix) {
			tunnels[k] = v
		}
	}

	return tunnels
}

func (m *Manager) RestoreAllTunnels() error {
	var vms []models.VM
	err := m.db.Preload("Tunnels").Find(&vms).Error
	if err != nil {
		m.logger.Error("failed to fetch VMs", zap.Error(err))
		return fmt.Errorf("failed to fetch vms: %w", err)
	}

	var servicePorts []models.ServicePort
	err = m.db.Find(&servicePorts).Error
	if err != nil {
		return fmt.Errorf("failed to fetch service ports: %w", err)
	}

	for _, vm := range vms {
		for _, sp := range servicePorts {
			err = m.StartTunnel(&vm, &sp)
			if err != nil {
				m.logger.Error("failed to restore tunnel",
					zap.Error(err),
					zap.String("vm_ip", vm.IP),
					zap.Int("service_port", sp.ServicePort))
				continue
			}
		}
	}

	return nil
}
