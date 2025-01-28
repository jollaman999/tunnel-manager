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

func (m *Manager) StartTunnel(vm *models.VM, sp *models.ServicePort) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	tunnelKey := fmt.Sprintf("%d-%d", vm.ID, sp.LocalPort)

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

	tunnel := models.Tunnel{
		VMID:   vm.ID,
		SPID:   sp.ID,
		Status: "starting",
		Local:  fmt.Sprintf("127.0.0.1:%d", sp.LocalPort),
		Server: fmt.Sprintf("%s:%d", vm.IP, vm.Port),
		Remote: fmt.Sprintf("%s:%d", sp.ServiceIP, sp.ServicePort),
	}

	t, err := NewSSHTunnel(
		&tunnel.VMID,
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

	err = m.db.Where("vm_id = ? AND sp_id = ?", vm.ID, sp.ID).
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

	err = tunnel.Stop(m)
	if err != nil {
		return fmt.Errorf("failed to stop tunnel: %w", err)
	}

	delete(m.tunnels, tunnelKey)

	return nil
}

func (m *Manager) RestoreAllTunnels() error {
	m.mu.Lock()
	var vms []models.VM
	err := m.db.Find(&vms).Error
	if err != nil {
		m.mu.Unlock()
		m.logger.Error("failed to fetch VMs", zap.Error(err))
		return fmt.Errorf("failed to fetch vms: %w", err)
	}

	if len(vms) == 0 {
		m.mu.Unlock()
		m.logger.Info("no VMs to restore")
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

	for _, vm := range vms {
		err = m.db.Unscoped().Where("vm_id = ?", vm.ID).Delete(&models.Tunnel{}).Error
		if err != nil {
			return fmt.Errorf("failed to reset tunnel status for vm_id=%d: %w", vm.ID, err)
		}

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

func (m *Manager) StopAllTunnels() {
	m.mu.Lock()
	var vms []models.VM
	err := m.db.Find(&vms).Error
	if err != nil {
		m.mu.Unlock()
		m.logger.Error("failed to fetch VMs", zap.Error(err))
	}

	var servicePorts []models.ServicePort
	err = m.db.Find(&servicePorts).Error
	if err != nil {
		m.mu.Unlock()
		m.logger.Error(fmt.Sprintf("failed to fetch service ports: %v", err))
	}
	m.mu.Unlock()

	for _, vm := range vms {
		err = m.db.Unscoped().Where("vm_id = ?", vm.ID).Delete(&models.Tunnel{}).Error
		if err != nil {
			m.logger.Error(fmt.Sprintf("failed to reset tunnel status for vm_id=%d: %w", vm.ID, err))
		}

		for _, sp := range servicePorts {
			err = m.StopTunnel(vm.ID, sp.ID)
			if err != nil {
				m.logger.Error("failed to stop tunnel",
					zap.Error(err),
					zap.String("vm_ip", vm.IP),
					zap.Int("service_port", sp.ServicePort))
				continue
			}
		}
	}
}
