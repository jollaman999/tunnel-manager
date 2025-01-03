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

type TunnelKey struct {
	VMID      uint
	LocalPort int
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

	// Update tunnel status in database
	status := &models.Tunnel{
		VMID:   vm.ID,
		Status: "starting",
	}
	if err := m.db.Create(status).Error; err != nil {
		return fmt.Errorf("failed to create tunnel status: %w", err)
	}

	// Start tunnel in background
	go func(m *Manager, tunnel *SSHTunnel, status *models.Tunnel) {
		err := tunnel.Start(m.monitoringIntervalSec)
		if err != nil {
			m.logger.Error("tunnel error",
				zap.Uint("vm_id", vm.ID),
				zap.Int("local_port", sp.LocalPort),
				zap.Error(err))

			status.Status = "error"
			status.LastError = err.Error()

			if err := m.db.Save(status).Error; err != nil {
				m.logger.Error("failed to update tunnel error status", zap.Error(err))
			}
			return
		}

		status.Status = "connected"
		status.LastConnectedAt = time.Now()

		if err := m.db.Save(status).Error; err != nil {
			m.logger.Error("failed to update tunnel connected status", zap.Error(err))
		}
	}(m, tunnel, status)

	return nil
}

func (m *Manager) StopTunnel(vmID uint, spID uint) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var sp models.ServicePort
	if err := m.db.First(&sp, spID).Error; err != nil {
		return fmt.Errorf("service port not found: %w", err)
	}

	tunnelKey := fmt.Sprintf("%d-%d", vmID, sp.LocalPort)
	tunnel, exists := m.tunnels[tunnelKey]
	if !exists {
		return fmt.Errorf("tunnel does not exist")
	}

	if err := tunnel.Stop(); err != nil {
		return fmt.Errorf("failed to stop tunnel: %w", err)
	}

	delete(m.tunnels, tunnelKey)

	// Update status in database
	if err := m.db.Model(&models.Tunnel{}).
		Where("vm_id = ?", vmID).
		Update("status", "stopped").Error; err != nil {
		return fmt.Errorf("failed to update tunnel status: %w", err)
	}

	return nil
}

func (m *Manager) RestartTunnel(vmID uint, spID uint) error {
	// First stop the tunnel
	if err := m.StopTunnel(vmID, spID); err != nil {
		m.logger.Warn("failed to stop tunnel for restart",
			zap.Error(err))
	}

	// Get VM and ServicePort info
	var vm models.VM
	if err := m.db.First(&vm, vmID).Error; err != nil {
		return fmt.Errorf("vm not found: %w", err)
	}

	var sp models.ServicePort
	if err := m.db.First(&sp, spID).Error; err != nil {
		return fmt.Errorf("service port not found: %w", err)
	}

	// Start the tunnel again
	return m.StartTunnel(&vm, &sp)
}

func (m *Manager) GetActiveTunnels() map[string]*SSHTunnel {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Return a copy of the tunnels map
	tunnels := make(map[string]*SSHTunnel)
	for k, v := range m.tunnels {
		tunnels[k] = v
	}

	return tunnels
}

func (m *Manager) GetVMActiveTunnels(vmID uint) map[string]*SSHTunnel {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Filter tunnels for specific VM
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
	if err := m.db.Preload("ServicePorts").Preload("Tunnels").Find(&vms).Error; err != nil {
		m.logger.Error("failed to fetch VMs", zap.Error(err))
		return fmt.Errorf("failed to fetch vms: %w", err)
	}

	var servicePorts []models.ServicePort
	if err := m.db.Preload("VM").Find(&servicePorts).Error; err != nil {
		return fmt.Errorf("failed to fetch service ports: %w", err)
	}

	for _, vm := range vms {
		for _, sp := range servicePorts {
			if err := m.StartTunnel(&vm, &sp); err != nil {
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
