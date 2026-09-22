package tunnel

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
	"gorm.io/gorm"
)

// ErrTunnelNotExist reports that the tunnel to stop is not running. Stopping a
// tunnel that was never started is not a failure, so callers that stop tunnels
// in bulk can tell it apart with errors.Is.
var ErrTunnelNotExist = errors.New("tunnel does not exist")

// KeyError marks the refusals that are about the private key that was handed
// in, as against a cipher that could not open a stored one or a database that
// could not be read. The caller answers the first with a bad request carrying
// this message, because the message is what says what to do about the key, and
// the second with a plain failure.
//
// Nothing built here carries the key or the passphrase. The message names what
// is wrong with the key, never what is in it, so it is safe to answer with and
// safe to log.
type KeyError struct {
	Reason string
}

func (e *KeyError) Error() string {
	return e.Reason
}

func keyError(format string, args ...any) error {
	return &KeyError{Reason: fmt.Sprintf(format, args...)}
}

// ParsePrivateKey turns a PEM private key into the signer an SSH connection is
// built with. It is exported because the key is read twice: once where a Host
// is registered, so that a key that cannot be used is refused while the
// operator is still looking at the box they pasted it into, and once here,
// where the connection is made.
//
// The key is parsed without the passphrase first. That is what tells a key that
// is protected by one from a key that is simply broken: the library reports the
// first as a PassphraseMissingError, and the difference decides whether the
// operator is asked for a passphrase or told the file is not a key. It also
// means a passphrase sent along with a key that has none is ignored rather than
// refused, since the library takes that combination as an error of its own.
func ParsePrivateKey(keyPEM string, passphrase string) (ssh.Signer, error) {
	keyPEM = strings.TrimSpace(keyPEM)
	if keyPEM == "" {
		return nil, keyError("no private key was given")
	}

	// The PEM is checked for its opening line before it is parsed, so that a
	// file that is plainly not a key, a public key or a certificate among them,
	// is answered with what it is rather than with "no key found".
	if !strings.Contains(keyPEM, "-----BEGIN") {
		return nil, keyError("the private key is not PEM: no -----BEGIN----- line was found in it. " +
			"Paste the private key file itself, not the public key beside it")
	}

	// The trailing newline is put back on. A PEM block that ends without one is
	// what a paste out of a terminal often is, and pem.Decode takes no block
	// that does not end its last line.
	block := []byte(keyPEM + "\n")

	signer, err := ssh.ParsePrivateKey(block)
	if err == nil {
		return signer, nil
	}

	var locked *ssh.PassphraseMissingError
	if !errors.As(err, &locked) {
		return nil, keyError("the private key cannot be read: %v", err)
	}

	if passphrase == "" {
		return nil, keyError("the private key is protected by a passphrase. " +
			"Register it together with the passphrase that opens it")
	}

	signer, err = ssh.ParsePrivateKeyWithPassphrase(block, []byte(passphrase))
	if err == nil {
		return signer, nil
	}

	if errors.Is(err, x509.IncorrectPasswordError) {
		return nil, keyError("the passphrase does not open the private key")
	}

	return nil, keyError("the private key cannot be read with the passphrase that was given: %v", err)
}

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
//
// A Host that carries no password at all comes back empty and nothing is
// written. That is the Host registered with a private key alone, and without
// this the empty value would be taken for a password that was stored before
// passwords were encrypted and be sealed into the row: the Host would then
// carry a password that is the empty string, which is offered to it on every
// connection and cannot be told from one that was chosen.
func (m *Manager) hostPassword(host *models.Host) (string, error) {
	if host.Password == "" {
		return "", nil
	}

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
			logid.TunnelHostPasswordUndecryptable.Field(),
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
			logid.TunnelHostPasswordEncryptFailed.Field(),
			zap.Uint("host_id", host.ID),
			zap.String("host_ip", host.IP),
			zap.Error(err))
		return
	}

	err = m.db.Model(&models.Host{}).Where("id = ?", host.ID).Update("password", encrypted).Error
	if err != nil {
		m.logger.Warn("failed to store the encrypted password of the Host",
			logid.TunnelHostPasswordStoreFailed.Field(),
			zap.Uint("host_id", host.ID),
			zap.String("host_ip", host.IP),
			zap.Error(err))
		return
	}

	host.Password = encrypted
	m.logger.Info("replaced the stored password of the Host with an encrypted one",
		logid.TunnelHostPasswordEncrypted.Field(),
		zap.Uint("host_id", host.ID),
		zap.String("host_ip", host.IP))
}

// hostAuth returns what to offer the Host to authenticate with, along with the
// credentials those methods were built from, which is what the connection
// fingerprint is taken over.
//
// The key comes first and the password second, which is the order the SSH
// protocol tries them in: a method that is refused is followed by the next one.
// A Host that carries both is therefore still reachable with its password while
// the key that was just registered is not yet the one the Host knows, so
// putting a key on a running installation cannot take its tunnels down.
// hostCreds is every secret a connection to a Host is built from, opened out
// of the row. The connection fingerprint is taken over all of them, so that a
// Host whose key is replaced is noticed the same way one whose password is.
type hostCreds struct {
	password   string
	privateKey string
	passphrase string
}

// hostCredentials opens what the Host row holds sealed. An error means a value
// is there and cannot be opened, which is not the same as a Host that carries
// none: the caller leaves the tunnel on what it already has rather than taking
// it down for a secret nobody can read.
func (m *Manager) hostCredentials(host *models.Host) (hostCreds, error) {
	password, err := m.hostPassword(host)
	if err != nil {
		return hostCreds{}, err
	}

	privateKey, err := m.hostSealed(host, "private key", host.PrivateKey)
	if err != nil {
		return hostCreds{}, err
	}

	passphrase, err := m.hostSealed(host, "key passphrase", host.KeyPassphrase)
	if err != nil {
		return hostCreds{}, err
	}

	return hostCreds{password: password, privateKey: privateKey, passphrase: passphrase}, nil
}

func (m *Manager) hostAuth(host *models.Host) ([]ssh.AuthMethod, hostCreds, error) {
	var methods []ssh.AuthMethod

	signer, keyErr := m.hostSigner(host)
	if signer != nil {
		methods = append(methods, ssh.PublicKeys(signer))
	}

	creds, err := m.hostCredentials(host)
	if err != nil {
		return nil, hostCreds{}, err
	}

	if creds.password != "" {
		methods = append(methods, ssh.Password(creds.password))
	}

	if len(methods) == 0 {
		// A key that could not be built is the reason there is nothing to
		// connect with, when there was one, so that is what is reported rather
		// than a Host that carries nothing at all.
		if keyErr != nil {
			return nil, hostCreds{}, keyErr
		}

		return nil, hostCreds{}, fmt.Errorf("the Host carries neither a private key nor a password (host_id=%d)", host.ID)
	}

	if keyErr != nil {
		// The password is there, so the tunnel is made with it. hostSigner
		// logged what is wrong with the key.
		m.logger.Warn("connecting to the Host with its password alone, because its stored "+
			"private key cannot be used",
			logid.TunnelHostKeyUnusablePasswordUsed.Field(),
			zap.Uint("host_id", host.ID),
			zap.String("host_ip", host.IP))
	}

	return methods, creds, nil
}

// hostSigner returns the signer built from the stored private key of the Host,
// or nil when the Host carries no key. An error means the Host carries one that
// cannot be used, which is not the same thing: the caller falls back to the
// password with it and reports it only when there is no password to fall back
// on.
//
// Nothing of the key reaches the log. What is written is which Host it belongs
// to and what is wrong with it, never a line of the key itself and never the
// passphrase.
func (m *Manager) hostSigner(host *models.Host) (ssh.Signer, error) {
	if host.PrivateKey == "" {
		return nil, nil
	}

	keyPEM, err := m.hostSealed(host, "private key", host.PrivateKey)
	if err != nil {
		return nil, err
	}

	passphrase, err := m.hostSealed(host, "key passphrase", host.KeyPassphrase)
	if err != nil {
		return nil, err
	}

	signer, err := ParsePrivateKey(keyPEM, passphrase)
	if err != nil {
		m.logger.Error("the stored private key of the Host cannot be read",
			logid.TunnelHostKeyUnreadable.Field(),
			zap.Uint("host_id", host.ID),
			zap.String("host_ip", host.IP),
			zap.Error(err))

		return nil, fmt.Errorf("failed to read the stored private key of the Host (host_id=%d): %w", host.ID, err)
	}

	return signer, nil
}

// hostSealed opens a value the Host row holds sealed. An empty value stays
// empty, because that is a Host that carries none rather than one that cannot
// be opened.
//
// Unlike the password, nothing is written back here. The key and its passphrase
// have been sealed since the day they could be registered, so there is no row
// holding a plaintext one to migrate: a value that does not open is the wrong
// encryption key, and overwriting it would destroy the key it holds. A value
// that was never sealed is one somebody put into the row by hand, and it is
// used as it stands.
func (m *Manager) hostSealed(host *models.Host, what string, stored string) (string, error) {
	if stored == "" {
		return "", nil
	}

	plaintext, err := m.cipher.Decrypt(stored)
	if err == nil {
		return plaintext, nil
	}

	if errors.Is(err, crypto.ErrNotEncrypted) {
		return stored, nil
	}

	m.logger.Error("the stored "+what+" of the Host does not decrypt with the encryption key in use, "+
		"leaving it as it is. Check that the configured key file is the one it was stored with",
		logid.TunnelHostSecretUndecryptable.Field(),
		zap.String("secret", what),
		zap.Uint("host_id", host.ID),
		zap.String("host_ip", host.IP),
		zap.Error(err))

	return "", fmt.Errorf("failed to decrypt the stored %s of the Host (host_id=%d): %w", what, host.ID, err)
}

// The four addresses the two bind scopes are made of, one pair per scope and
// one address per family within a pair. A scope is what is stored and what
// somebody chose; these are what a forward request can actually carry, since
// the protocol asks for one address at a time.
const (
	loopbackBindAddressV4 = "127.0.0.1"
	loopbackBindAddressV6 = "::1"
	wildcardBindAddressV4 = "0.0.0.0"
	wildcardBindAddressV6 = "::"
)

// bindScopeAddresses returns the two addresses an assignment of this scope asks
// its forwarded port to be opened on, one per address family.
//
// Anything that is not the loopback scope gives the wildcard pair, the empty
// value among them. That is the reading models.HostServicePort.BindScope is
// documented with: every row written before the column existed holds the empty
// value, and those tunnels were requested on the wildcard, so reading it as
// anything else would narrow what they reach on a startup that was asked for
// nothing of the sort. A value that is neither word cannot be stored, since
// the column carries a rule refusing it, and one somebody wrote by hand is
// answered the same way rather than by leaving the tunnel with no address.
func bindScopeAddresses(scope string) (v4, v6 string) {
	if scope == models.BindScopeLoopback {
		return loopbackBindAddressV4, loopbackBindAddressV6
	}

	return wildcardBindAddressV4, wildcardBindAddressV6
}

// tunnelAddresses returns the local, server and remote addresses a tunnel for
// this combination is built from. Both the tunnel and its fingerprint are built
// from these, so the comparison sees what was connected to.
//
// There are two local addresses because a scope names a pair, and the tunnel
// asks for both. They are returned as a pair rather than as one address chosen
// here, because the two families do not stand in for each other: whoever chose
// a scope chose it for the machine, and half of it is not what they asked for.
//
// All of them are joined with net.JoinHostPort rather than with a format
// string, because an IPv6 address has colons of its own: "2001:db8::1" and port
// 22 written plainly reads "2001:db8::1:22", which no dialer can take apart.
// JoinHostPort puts the brackets in, giving "[2001:db8::1]:22". The local
// addresses go through it too, since ::1 and :: are among the ones a Host is
// asked to bind.
func tunnelAddresses(host *models.Host, sp *models.ServicePort, bindScope string) (localV4, localV6, server, remote string) {
	bindV4, bindV6 := bindScopeAddresses(bindScope)
	port := strconv.Itoa(sp.LocalPort)

	return net.JoinHostPort(bindV4, port),
		net.JoinHostPort(bindV6, port),
		net.JoinHostPort(host.IP, strconv.Itoa(host.Port)),
		net.JoinHostPort(sp.ServiceIP, strconv.Itoa(sp.ServicePort))
}

// The two statuses a tunnel is left in when the host key check refused the
// connection. They stand apart from the other statuses because they are the
// only ones an operator answers rather than fixes: every other failure is
// something to put right on the Host or on the way to it, and these two are a
// question about the identity of the server that only a person can settle.
//
// They are exported because the approval API and the screens branch on them.
// A Host that has never been approved and a Host whose key changed under it
// are shown differently and approved differently, and which of the two it is
// is decided here, where the keys were compared.
const (
	// StatusHostKeyUnapproved is a Host that carries no trusted key. The
	// server presented one, nobody has said it is the right one, and there is
	// nothing to compare it against until somebody does.
	StatusHostKeyUnapproved = "host_key_unapproved"
	// StatusHostKeyMismatch is a Host that carries a trusted key and was
	// presented a different one. Either the server was rebuilt and given a
	// new key, or the connection is not reaching the server at all, and the
	// two cannot be told apart from here.
	StatusHostKeyMismatch = "host_key_mismatch"
)

// MarshalHostKey writes a public key the way a Host row holds one:
// "<algorithm> <base64>", an authorized_keys line without the comment that may
// follow it.
//
// Every key that is stored and every key that is compared goes through this
// one function, so the comparison is a string comparison and cannot fail on
// two spellings of the same key.
func MarshalHostKey(key ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

// HostKeyFingerprint returns the SHA256 fingerprint of a key held in the form
// a Host row holds one: "SHA256:" followed by the digest in unpadded base64,
// which is what ssh(1) prints and what ssh-keygen -lf reports for the key file
// on the server. That is the form an operator can compare; the key in full is
// a line nobody reads to the end.
//
// A stored value that is empty or that cannot be read as a key gives an empty
// string. Neither is a failure worth reporting: the first is a Host that
// carries no key, which is the normal state of one that has never been
// approved, and the second is a row somebody wrote by hand, which is answered
// by showing no fingerprint rather than by taking the screen down.
func HostKeyFingerprint(stored string) string {
	stored = strings.TrimSpace(stored)
	if stored == "" {
		return ""
	}

	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(stored))
	if err != nil {
		return ""
	}

	return ssh.FingerprintSHA256(key)
}

// HostKeyError reports that the SSH server is not the one the Host is trusted
// on. It is raised inside the handshake, which runs the host key check before
// any authentication, so a server that fails it is never offered the password
// or the private key of the Host.
//
// It carries the status the tunnel row is left in, so that what is shown and
// what is asked before the key is approved follow from the refusal itself
// rather than from a second comparison somewhere else.
type HostKeyError struct {
	// Status is StatusHostKeyUnapproved or StatusHostKeyMismatch.
	Status string
	// Presented is the key the server offered, in the stored form.
	Presented string
	// Trusted is the key the Host is trusted on, in the stored form, and is
	// empty for a Host that carries none.
	Trusted string
}

func (e *HostKeyError) Error() string {
	if e.Status == StatusHostKeyMismatch {
		return fmt.Sprintf("the SSH server presented the host key %s, and this Host is trusted on %s. "+
			"Either the server was rebuilt and given a new key, or this connection is not reaching the "+
			"server it is meant for. Find out which before approving the key that was presented",
			HostKeyFingerprint(e.Presented), HostKeyFingerprint(e.Trusted))
	}

	return fmt.Sprintf("the SSH server presented the host key %s and no host key has been approved for "+
		"this Host. Compare it with the fingerprint the server itself reports, ssh-keygen -lf on its host "+
		"key file, and approve it to let the tunnels connect", HostKeyFingerprint(e.Presented))
}

// hostKeyRefusal returns the host key refusal inside err, and nil when err is
// not one. The library wraps whatever the callback returned into the error it
// reports the handshake with (x/crypto/ssh, NewClientConn), so the refusal is
// unwrapped out of it rather than compared against.
func hostKeyRefusal(err error) *HostKeyError {
	var refusal *HostKeyError
	if errors.As(err, &refusal) {
		return refusal
	}

	return nil
}

// hostKeyCallback builds what the handshake asks whether the server that
// answered is the one this Host is trusted on.
//
// A Host that carries no approved key reaches nothing. Trusting the first key
// that is presented is what an ssh client does and is far less work for
// whoever registers a Host, but it settles the question at the one moment it
// cannot be answered: somebody on the path while a Host is registered has
// their own key written down as the trusted one, and every connection after
// that is checked against it and passes. Registering a Host happens rarely and
// reading a fingerprint once is cheap, while a trust fixed on the wrong key is
// never noticed at all.
//
// Whatever was presented is written to the Host row so that there is something
// to show and to approve. It is written under PendingHostKey and never under
// HostKey, because nothing this end sees makes a key the right one; the only
// thing that does is a person who compared the fingerprint with the server.
//
// What the trust is compared against is read here, where the tunnel is built,
// and not in the callback. A key approved in the meantime changes the
// connection fingerprint of the tunnel, and a reconcile pass then builds the
// tunnel again, rather than the trust of a connection that already stands
// changing underneath it.
func (m *Manager) hostKeyCallback(host *models.Host) ssh.HostKeyCallback {
	hostID, hostIP, trusted := host.ID, host.IP, host.HostKey

	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		presented := MarshalHostKey(key)
		if trusted != "" && presented == trusted {
			return nil
		}

		refusal := &HostKeyError{
			Status:    StatusHostKeyUnapproved,
			Presented: presented,
			Trusted:   trusted,
		}
		if trusted != "" {
			refusal.Status = StatusHostKeyMismatch
		}

		err := m.db.Model(&models.Host{}).Where("id = ?", hostID).
			Update("pending_host_key", presented).Error
		if err != nil {
			// The connection is refused either way. A key that could not be
			// written down is one nobody can approve, which is worse than the
			// refusal and is no reason to let the connection through. It is
			// carried on the refusal instead of being logged on its own, so
			// that it reaches the tunnel row and the line the caller writes
			// about the connection that was not made.
			return fmt.Errorf("%w. The key that was presented could not be stored for approval "+
				"(host_id=%d, host_ip=%s): %v", refusal, hostID, hostIP, err)
		}

		return refusal
	}
}

// StartTunnel builds and starts the tunnel for one assignment. bindScope is
// what that assignment stored, models.HostServicePort.BindScope, and it decides
// the pair of addresses the forwarded port is asked to be opened on. It is
// passed in rather than read here because the reconcile pass has already read
// every assignment row, and reading it again would put a statement per tunnel
// onto the database every few seconds.
func (m *Manager) StartTunnel(host *models.Host, sp *models.ServicePort, bindScope string) error {
	if !host.Enabled {
		m.logger.Info("skipped starting tunnel for disabled Host",
			logid.TunnelStartSkippedHostDisabled.Field(),
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

	auth, creds, err := m.hostAuth(host)
	if err != nil {
		return err
	}

	sshConfig := &ssh.ClientConfig{
		User:            host.User,
		Auth:            auth,
		HostKeyCallback: m.hostKeyCallback(host),
		Timeout:         time.Second * 10,
	}

	localV4, localV6, server, remote := tunnelAddresses(host, sp, bindScope)

	tunnel := models.Tunnel{
		HostID: host.ID,
		SPID:   sp.ID,
		Status: "starting",
		// The row carries one address because it is the one to connect to,
		// and the scope asks for two. The IPv4 one of the pair stands here
		// until a connection says which of the two opened, which is what
		// every row held before the pair existed and so is what a reader that
		// has not been changed goes on seeing.
		Local:  localV4,
		Server: server,
		Remote: remote,
		// Nothing has been measured on a tunnel that is only being started,
		// and the row says so rather than leaving the field empty: an empty
		// reading and one that was taken must not read alike.
		ForwardReach: forwardReachUnknown,
		// OpenReach is left empty for the same reason, except that for it the
		// empty value is the one that says nothing has been measured. Nothing
		// has been asked of the far side yet, so there is no half of the pair
		// to report as missing.
		OpenReach: "",
	}

	t, err := NewSSHTunnel(
		&tunnel.HostID,
		&tunnel.SPID,
		localV4,
		localV6,
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
	t.connFP = connectionFingerprint(host, sp, bindScope, creds)

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
		m.logger.Error(fmt.Sprintf("failed to fetch Host's tunnels (host_id=%d)", hostID),
			logid.TunnelHostTunnelsFetchFailed.Field(),
			zap.Uint("host_id", hostID),
			zap.Error(err))
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
		m.logger.Error("failed to fetch tunnels", logid.TunnelTunnelsFetchFailed.Field(), zap.Error(err))
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
		m.logger.Error("failed to reset tunnel status", logid.TunnelStatusResetFailed.Field(), zap.Error(err))
		return fmt.Errorf("failed to reset tunnel status: %w", err)
	}

	result, err := m.Reconcile()
	if err != nil {
		m.logger.Error("failed to restore tunnels", logid.TunnelRestoreFailed.Field(), zap.Error(err))
		return err
	}

	m.logger.Info("restored tunnels",
		logid.TunnelRestored.Field(),
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
				logid.TunnelKeyUnreadable.Field(),
				zap.String("tunnel_key", key))
			continue
		}

		err := m.StopTunnel(hostID, spID)
		if err != nil {
			if errors.Is(err, ErrTunnelNotExist) {
				m.logger.Debug("no tunnel to stop",
					logid.TunnelStopSkippedNotRunning.Field(),
					zap.Uint("host_id", hostID),
					zap.Uint("sp_id", spID))
				continue
			}

			m.logger.Error("failed to stop tunnel",
				logid.TunnelStopFailed.Field(),
				zap.Error(err),
				zap.Uint("host_id", hostID),
				zap.Uint("sp_id", spID))
			continue
		}
	}
}
