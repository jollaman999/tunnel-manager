package tunnel

import (
	"errors"
	"fmt"
	"net"
	"strconv"
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
	// reconcileWake carries the request for a reconcile pass. It holds one
	// wake-up, so a caller never waits for the loop to pick the previous one up.
	reconcileWake chan struct{}
}

func NewManager(db *gorm.DB, logger *zap.Logger, cipher *crypto.Cipher, monitoringIntervalSec int) (*Manager, error) {
	return &Manager{
		db:                    db,
		tunnels:               make(map[string]*SSHTunnel),
		logger:                logger,
		cipher:                cipher,
		monitoringIntervalSec: monitoringIntervalSec,
		reconcileWake:         make(chan struct{}, 1),
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

// tunnelAddresses returns the local, server and remote addresses a tunnel for
// this combination is built from. Both the tunnel and its fingerprint are built
// from these, so the comparison sees what was connected to.
//
// The host and the service are joined with net.JoinHostPort rather than with a
// format string, because an IPv6 address has colons of its own: "2001:db8::1"
// and port 22 written plainly reads "2001:db8::1:22", which no dialer can take
// apart. JoinHostPort puts the brackets in, giving "[2001:db8::1]:22". The
// local address is a literal 0.0.0.0, which is what the sshd on the Host binds
// the forwarded port to and is not an address of ours to translate.
func tunnelAddresses(host *models.Host, sp *models.ServicePort) (local, server, remote string) {
	return fmt.Sprintf("0.0.0.0:%d", sp.LocalPort),
		net.JoinHostPort(host.IP, strconv.Itoa(host.Port)),
		net.JoinHostPort(sp.ServiceIP, strconv.Itoa(sp.ServicePort))
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

	key := tunnelKey(host.ID, sp.ID)
	if _, exists := m.tunnels[key]; exists {
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

	local, server, remote := tunnelAddresses(host, sp)

	tunnel := models.Tunnel{
		HostID: host.ID,
		SPID:   sp.ID,
		Status: "starting",
		Local:  local,
		Server: server,
		Remote: remote,
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

	// The settings this tunnel is connecting with, so a later pass can tell
	// whether the ones it should have are still the same.
	t.connFP = connectionFingerprint(host, sp, password)

	err = m.db.Where("host_id = ? AND sp_id = ?", host.ID, sp.ID).
		Attrs(tunnel).
		FirstOrCreate(&tunnel).Error
	if err != nil {
		return fmt.Errorf("failed to create tunnel information: %w", err)
	}

	// Registered only once nothing is left that can fail, so a tunnel that was
	// not started does not keep the key taken.
	m.tunnels[key] = t

	go func(m *Manager, t *SSHTunnel, tunnel *models.Tunnel) {
		t.Start(m, tunnel)
	}(m, t, &tunnel)

	return nil
}

func (m *Manager) StopTunnel(hostID uint, spID uint) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := tunnelKey(hostID, spID)
	tunnel, exists := m.tunnels[key]
	if !exists {
		return ErrTunnelNotExist
	}

	err := tunnel.Stop(m)
	if err != nil {
		return fmt.Errorf("failed to stop tunnel: %w", err)
	}

	delete(m.tunnels, key)

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

// RestoreAllTunnels starts the tunnels that should be running. It is the first
// reconcile pass: the process holds no tunnel yet, so every combination of the
// desired state is started here. The tunnel rows left by the previous process
// are dropped first, because the SSH connections they describe died with it.
func (m *Manager) RestoreAllTunnels() error {
	err := m.db.Where("1 = 1").Delete(&models.Tunnel{}).Error
	if err != nil {
		m.logger.Error("failed to reset tunnel status", zap.Error(err))
		return fmt.Errorf("failed to reset tunnel status: %w", err)
	}

	result, err := m.Reconcile()
	if err != nil {
		m.logger.Error("failed to restore tunnels", zap.Error(err))
		return err
	}

	m.logger.Info("restored tunnels",
		zap.Int("started", result.Started),
		zap.Int("failed", result.Failed))

	return nil
}

// StopAllTunnels stops every running tunnel. It is a reconcile pass with an
// empty desired state, and it reads no rows: what is running is in m.tunnels,
// and a database that is away on shutdown must not leave a tunnel up.
func (m *Manager) StopAllTunnels() {
	for _, key := range m.runningTunnelKeys() {
		hostID, spID, ok := parseTunnelKey(key)
		if !ok {
			m.logger.Error("a running tunnel is registered under a key that cannot be read",
				zap.String("tunnel_key", key))
			continue
		}

		err := m.StopTunnel(hostID, spID)
		if err != nil {
			if errors.Is(err, ErrTunnelNotExist) {
				m.logger.Debug("no tunnel to stop",
					zap.Uint("host_id", hostID),
					zap.Uint("sp_id", spID))
				continue
			}

			m.logger.Error("failed to stop tunnel",
				zap.Error(err),
				zap.Uint("host_id", hostID),
				zap.Uint("sp_id", spID))
			continue
		}
	}
}
