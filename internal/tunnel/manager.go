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
	// reconnectMaxIntervalSec is the ceiling of the wait before a failed
	// connection is tried again (reconnectBackoff). It is zero until
	// SetReconnectMaxInterval is called, which leaves every wait at the
	// monitoring interval.
	reconnectMaxIntervalSec int
	// reconcileWake carries the request for a reconcile pass. It holds one
	// wake-up, so a caller never waits for the loop to pick the previous one up.
	reconcileWake chan struct{}
	// localForwards holds the running local forwards by the Host and the
	// number each of them is the row of, which is what the table is keyed by.
	// It is not keyed by the local port, which is unique over the table as
	// well: a forward moved to another port would then be registered under a
	// key that changed with it, and a pass that moves it would start the
	// forward on the new port before stopping the one on the old. It has a
	// lock of its own, since nothing ever needs it together with the tunnels.
	localForwards map[LocalForwardKey]*localTunnel
	localMu       sync.RWMutex
	// socksProxies holds the running SOCKS5 proxies by the ID of their Host,
	// under a lock of its own for the same reason.
	socksProxies map[uint]*socksTunnel
	socksMu      sync.RWMutex
	// desiredCounts is how many tunnels and local forwards the last pass that
	// completed wanted running. The status screen asks for these every few
	// seconds for every browser that has it open, and counting them afresh
	// reads four tables in full each time, so they are taken from the pass,
	// which reads those tables anyway. desiredCounted is false until a pass
	// has completed.
	desiredCounts  desiredCounts
	desiredCounted bool
	desiredMu      sync.Mutex

	// reconnectHosts are the Hosts whose connections the next reconcile pass
	// is to drop and make again, which is what RequestReconnect asks for. The
	// pass takes the set and empties it, so a request is served once.
	reconnectHosts map[uint]bool
	reconnectMu    sync.Mutex

	// stopping holds, under m.mu, the tunnels that StopTunnel has taken out of
	// m.tunnels and is still stopping, by the same key. The channel is closed
	// once the stop is done. The row of a tunnel is keyed by its Host and its
	// service port, which is the key here, so a stop that is still deleting
	// it would take the row of a tunnel started again under the key in the
	// meantime; StartTunnel waits on the channel for that reason.
	stopping map[string]chan struct{}

	// starting holds, under m.mu, the tunnels that StartTunnel has registered
	// in m.tunnels and is still making the row of, by the same key. The
	// channel is closed once the row is made, or once the tunnel is taken out
	// of m.tunnels again because it could not be. A stop of the tunnel waits on
	// it, since the row is what the stop deletes and the start may yet fail.
	starting map[string]chan struct{}

	// signers holds the private key of every Host as it was last read, so
	// that the key is read once rather than once for every connection made
	// with it. What is kept and when it is read again is at signerCache.
	signers signerCache
}

// parseHostKey is what a stored private key is read with, rather than
// ParsePrivateKey straight, so that a test can count how often a key is read
// and hold a read open. A key with a passphrase is run through bcrypt to be
// opened, which takes a sizeable part of a second, and how often that happens
// and what waits on it are what those tests are about.
var parseHostKey = ParsePrivateKey

// signerCache keeps, for every Host, the signer its stored private key was
// read into, along with the stored key and passphrase it was read from.
//
// The stored values are what an entry is matched against, sealed as they are
// in the row. A key or a passphrase that is replaced is sealed afresh and no
// longer matches, so it is read again without anything having to say it
// changed. Matching the sealed values rather than the opened ones also means a
// lookup opens nothing.
//
// A key that could not be read is kept as well, with the reason. Reading a key
// gives the same answer every time for the same key and passphrase, and a
// wrong passphrase costs the full bcrypt run to find out, so without this a
// Host with a wrong passphrase would pay that run for every forward of it on
// every pass. A value that could not be opened by the cipher is not kept,
// since that is not a reading of the key and opening is cheap.
//
// It has a lock of its own, which is held only to look an entry up or to put
// one in. The key is read outside it, so reading the key of one Host never
// holds up another.
type signerCache struct {
	mu      sync.Mutex
	entries map[uint]signerEntry
}

type signerEntry struct {
	privateKey string
	passphrase string
	signer     ssh.Signer
	err        error
}

// lookup returns what the key of the Host was read into when it was read from
// these stored values, and false when it has not been.
func (c *signerCache) lookup(hostID uint, privateKey, passphrase string) (signerEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[hostID]
	if !ok || entry.privateKey != privateKey || entry.passphrase != passphrase {
		return signerEntry{}, false
	}

	return entry, true
}

// keep records what the key of the Host was read into from these stored
// values, in place of whatever was kept for the Host before.
func (c *signerCache) keep(hostID uint, privateKey, passphrase string, signer ssh.Signer, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.entries == nil {
		c.entries = make(map[uint]signerEntry)
	}

	c.entries[hostID] = signerEntry{privateKey: privateKey, passphrase: passphrase, signer: signer, err: err}
}

// forget drops what was kept for the Host, which is a Host that carries no
// key any more.
func (c *signerCache) forget(hostID uint) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.entries, hostID)
}

// retain drops what was kept for every Host that is not among hosts, which is
// a Host that was removed. A reconcile pass calls it with the Hosts it read.
func (c *signerCache) retain(hosts map[uint]*models.Host) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for hostID := range c.entries {
		if _, ok := hosts[hostID]; !ok {
			delete(c.entries, hostID)
		}
	}
}

// desiredCounts is how many of each sort of forward should be running.
type desiredCounts struct {
	tunnels       int
	localForwards int
}

func NewManager(db *gorm.DB, logger *zap.Logger, cipher *crypto.Cipher, monitoringIntervalSec int) (*Manager, error) {
	return &Manager{
		db:                    db,
		tunnels:               make(map[string]*SSHTunnel),
		stopping:              make(map[string]chan struct{}),
		starting:              make(map[string]chan struct{}),
		logger:                logger,
		cipher:                cipher,
		monitoringIntervalSec: monitoringIntervalSec,
		reconcileWake:         make(chan struct{}, 1),
		localForwards:         make(map[LocalForwardKey]*localTunnel),
		socksProxies:          make(map[uint]*socksTunnel),
		reconnectHosts:        make(map[uint]bool),
	}, nil
}

// SetReconnectMaxInterval sets the ceiling of the wait before a failed
// connection is tried again. It is read by every tunnel, local forward and
// SOCKS5 proxy as it starts and never locked, so it is called before the first
// of them is started, which is where the startup calls it. A change stored on
// the Settings screen reaches it at the next start, the way the monitoring
// interval does.
func (m *Manager) SetReconnectMaxInterval(sec int) {
	m.reconnectMaxIntervalSec = sec
}

// reconnectBackoff returns the backoff a connect loop starts on.
func (m *Manager) reconnectBackoff() reconnectBackoff {
	return newReconnectBackoff(time.Duration(m.monitoringIntervalSec)*time.Second,
		time.Duration(m.reconnectMaxIntervalSec)*time.Second)
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
			zap.String("host_ip", host.Address),
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
			zap.String("host_ip", host.Address),
			zap.Error(err))
		return
	}

	err = m.db.Model(&models.Host{}).Where("id = ?", host.ID).Update("password", encrypted).Error
	if err != nil {
		m.logger.Warn("failed to store the encrypted password of the Host",
			logid.TunnelHostPasswordStoreFailed.Field(),
			zap.Uint("host_id", host.ID),
			zap.String("host_ip", host.Address),
			zap.Error(err))
		return
	}

	host.Password = encrypted
	m.logger.Info("replaced the stored password of the Host with an encrypted one",
		logid.TunnelHostPasswordEncrypted.Field(),
		zap.Uint("host_id", host.ID),
		zap.String("host_ip", host.Address))
}

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

// hostAuth returns what to offer the Host to authenticate with, along with the
// credentials those methods were built from, which is what the connection
// fingerprint is taken over.
//
// The key comes first and the password second, which is the order the SSH
// protocol tries them in: a method that is refused is followed by the next one.
// A Host that carries both is therefore still reachable with its password while
// the key that was just registered is not yet the one the Host knows, so
// putting a key on a running installation cannot take its tunnels down.
//
// The credentials are opened before the key is read. A value that does not
// open ends the call either way, and opening it first means it is opened and
// reported once rather than once by each of the two.
func (m *Manager) hostAuth(host *models.Host) ([]ssh.AuthMethod, hostCreds, error) {
	var methods []ssh.AuthMethod

	creds, err := m.hostCredentials(host)
	if err != nil {
		return nil, hostCreds{}, err
	}

	signer, keyErr := m.hostSigner(host)
	if signer != nil {
		methods = append(methods, ssh.PublicKeys(signer))
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
		// The password is there, so the tunnel is made with it. What is
		// wrong with the key goes with the line, since hostSigner reports it
		// only when the key is read.
		m.logger.Warn("connecting to the Host with its password alone, because its stored "+
			"private key cannot be used",
			logid.TunnelHostKeyUnusablePasswordUsed.Field(),
			zap.Uint("host_id", host.ID),
			zap.String("host_ip", host.Address),
			zap.Error(keyErr))
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
//
// The key is read once for the stored values it is read from and kept in
// m.signers, and a key that cannot be read is reported when it is read and not
// again. Every pass tries the tunnels of such a Host again, one for each of
// its ports, and each of them answered from what was kept would otherwise
// write the same line. Each try that fails says why on a line of its own, and
// a key that is replaced is read and reported afresh.
func (m *Manager) hostSigner(host *models.Host) (ssh.Signer, error) {
	if host.PrivateKey == "" {
		m.signers.forget(host.ID)
		return nil, nil
	}

	kept, ok := m.signers.lookup(host.ID, host.PrivateKey, host.KeyPassphrase)
	signer, err := kept.signer, kept.err
	if !ok {
		keyPEM, sealErr := m.hostSealed(host, "private key", host.PrivateKey)
		if sealErr != nil {
			return nil, sealErr
		}

		passphrase, sealErr := m.hostSealed(host, "key passphrase", host.KeyPassphrase)
		if sealErr != nil {
			return nil, sealErr
		}

		signer, err = parseHostKey(keyPEM, passphrase)
		m.signers.keep(host.ID, host.PrivateKey, host.KeyPassphrase, signer, err)

		if err != nil {
			m.logger.Error("the stored private key of the Host cannot be read",
				logid.TunnelHostKeyUnreadable.Field(),
				zap.Uint("host_id", host.ID),
				zap.String("host_ip", host.Address),
				zap.Error(err))
		}
	}
	if err != nil {
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
		zap.String("host_ip", host.Address),
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
		net.JoinHostPort(host.Address, strconv.Itoa(host.Port)),
		net.JoinHostPort(sp.ServiceAddress, strconv.Itoa(sp.ServicePort))
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
	hostID, hostIP, trusted := host.ID, host.Address, host.HostKey

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

// MaxJumps is the most Hosts a jump route may pass through on its way to the
// Host it is for. Every step waits out the connection timeout on its own when
// it does not answer, so a route this long already takes the better part of a
// minute and a half to give up on.
const MaxJumps = 8

// StatusJumpHostDisabled is a tunnel, a local forward or a SOCKS5 proxy whose
// Host is reached through a Host that is disabled. Nothing is dialled for it:
// a disabled Host is one nobody is to connect to, and passing through it is
// connecting to it. The route is kept, so enabling the Host passed through
// brings the connection back.
//
// It is exported because the screens and the API name it, the way they name
// the host key refusals.
const StatusJumpHostDisabled = "jump_host_disabled"

// jumpHop is one step of the jump route of a Host as a pass read it: the ID
// the row names, the Host of that ID, and the credentials opened out of it.
// host is nil when no Host carries the ID, which is a route left naming a Host
// that was removed.
type jumpHop struct {
	id    uint
	host  *models.Host
	creds hostCreds
}

// jumpRoutes reads every jump route, keyed by the Host each is for, with the
// Hosts of each in the order they are passed through. It is one statement for
// the whole installation, for the reason desiredTunnels reads each table once.
func (m *Manager) jumpRoutes() (map[uint][]uint, error) {
	var rows []models.HostJump

	err := m.db.Order("host_id, seq").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("failed to fetch jump routes: %w", err)
	}

	routes := make(map[uint][]uint)
	for _, row := range rows {
		routes[row.HostID] = append(routes[row.HostID], row.JumpHostID)
	}

	return routes, nil
}

// hostRoute is the jump route of one Host as a pass read it, or why it could
// not be read. err is a credential of a Host passed through that does not open,
// which is answered the way one of the Host itself is: what runs keeps running
// and nothing new is started on it.
type hostRoute struct {
	hops []jumpHop
	err  error
}

// hostRoutesOf opens the jump route of every Host that has one. It is taken
// once per pass rather than per tunnel, because every tunnel, local forward
// and proxy of a Host shares the route, and each step opens the credentials of
// a Host.
func (m *Manager) hostRoutesOf(hostByID map[uint]*models.Host, routes map[uint][]uint) map[uint]hostRoute {
	opened := make(map[uint]hostRoute, len(routes))

	for hostID, ids := range routes {
		if _, ok := hostByID[hostID]; !ok {
			continue
		}

		hops, err := m.jumpHopsOf(ids, hostByID)
		opened[hostID] = hostRoute{hops: hops, err: err}
	}

	return opened
}

// jumpHopsOf opens the steps of one stored route. An ID no Host carries is
// kept as a step with no Host, so that the route is refused where it is
// started, with a reason, rather than passed over here without one.
func (m *Manager) jumpHopsOf(ids []uint, hostByID map[uint]*models.Host) ([]jumpHop, error) {
	hops := make([]jumpHop, 0, len(ids))

	for i, id := range ids {
		host, ok := hostByID[id]
		if !ok {
			hops = append(hops, jumpHop{id: id})
			continue
		}

		creds, err := m.hostCredentials(host)
		if err != nil {
			return nil, &JumpError{Seq: i + 1, HostID: host.ID, Address: hostServer(host), Err: err}
		}

		hops = append(hops, jumpHop{id: id, host: host, creds: creds})
	}

	return hops, nil
}

// hostServer is the address the SSH server of a Host is dialled at.
func hostServer(host *models.Host) string {
	return dialAddress(net.JoinHostPort(host.Address, strconv.Itoa(host.Port)))
}

// routeRefusal is a jump route that is not dialled at all, and the status and
// the reason what it was for is left in. It is decided before anything is
// dialled, from what is stored, so that no attempt is made that could only
// fail or that would connect to a Host nobody is to connect to.
//
// jump is the same refusal as the reason a connection stopped on its route,
// naming the step and the Host where the refusal is about one of them.
type routeRefusal struct {
	status string
	reason string
	jump   jumpFailure
}

func (r *routeRefusal) Error() string {
	return r.reason
}

// checkJumpRoute refuses a route that cannot be dialled as it is stored. The
// API refuses these where a route is written, and this is what stands where
// one was written some other way, into the table by hand or by a release that
// did not ask.
//
// A route longer than MaxJumps, one naming a Host that is not there, one that
// passes through the Host it is for and one that passes through a Host twice
// are errors in what is stored, and are checked first over the whole route. A
// route that passes through itself goes round in a circle, and passing through
// a Host twice is a longer way to the same place with a second login on it. A
// Host passed through that is disabled comes after those, since it is a route
// that is right and waits for the Host to be enabled.
func checkJumpRoute(host *models.Host, hops []jumpHop) *routeRefusal {
	if len(hops) > MaxJumps {
		return &routeRefusal{status: localStatusError, reason: fmt.Sprintf(
			"the jump route of this Host has %d jump hosts, and a route may have %d at most",
			len(hops), MaxJumps), jump: jumpFailure{reason: JumpReasonRoute}}
	}

	seen := make(map[uint]int, len(hops))
	for i, hop := range hops {
		if hop.host == nil {
			return &routeRefusal{status: localStatusError, reason: fmt.Sprintf(
				"jump %d names host #%d, which is not registered", i+1, hop.id),
				jump: jumpFailure{seq: i + 1, hostID: hop.id, reason: JumpReasonRoute}}
		}

		if hop.id == host.ID {
			return &routeRefusal{status: localStatusError, reason: fmt.Sprintf(
				"jump %d (host #%d %s) is this Host itself", i+1, hop.id, hostServer(hop.host)),
				jump: jumpFailure{seq: i + 1, hostID: hop.id, reason: JumpReasonRoute}}
		}

		if first, ok := seen[hop.id]; ok {
			return &routeRefusal{status: localStatusError, reason: fmt.Sprintf(
				"jump %d (host #%d %s) is jump %d as well", i+1, hop.id, hostServer(hop.host), first),
				jump: jumpFailure{seq: i + 1, hostID: hop.id, reason: JumpReasonRoute}}
		}
		seen[hop.id] = i + 1
	}

	for i, hop := range hops {
		if !hop.host.Enabled {
			return &routeRefusal{status: StatusJumpHostDisabled, reason: fmt.Sprintf(
				"jump %d (host #%d %s) is disabled, so the route to this Host is not dialled",
				i+1, hop.id, hostServer(hop.host)),
				jump: jumpFailure{seq: i + 1, hostID: hop.id, reason: JumpReasonDisabled}}
		}
	}

	return nil
}

// jumpRoute builds what the connection to host is made through: one hop per
// Host of its jump route, each with the user, the credentials and the trusted
// host key of that Host. A host key that is refused on a hop is written to the
// row of the Host passed through, which is the one that presented it and the
// one an approval has to be given to.
//
// A route that is not to be dialled comes back as a refusal and no hops. A
// Host passed through that has nothing to log in with is an error, the way
// one the connection is for is at StartTunnel.
func (m *Manager) jumpRoute(host *models.Host, hops []jumpHop) ([]sshHop, *routeRefusal, error) {
	if len(hops) == 0 {
		return nil, nil, nil
	}

	if refusal := checkJumpRoute(host, hops); refusal != nil {
		return nil, refusal, nil
	}

	route := make([]sshHop, 0, len(hops))
	for i, hop := range hops {
		auth, _, err := m.hostAuth(hop.host)
		if err != nil {
			return nil, nil, &JumpError{Seq: i + 1, HostID: hop.id, Address: hostServer(hop.host), Err: err}
		}

		route = append(route, sshHop{
			hostID: hop.id,
			addr:   hostServer(hop.host),
			config: m.clientConfig(hop.host, auth),
		})
	}

	return route, nil, nil
}

// clientConfig is what a connection to one Host is made with. The tunnels, the
// local forwards, the SOCKS5 proxies and every Host a jump route passes through
// are connected to with it, so each of them is checked against the key its own
// Host is trusted on.
func (m *Manager) clientConfig(host *models.Host, auth []ssh.AuthMethod) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:            host.User,
		Auth:            auth,
		HostKeyCallback: m.hostKeyCallback(host),
		Timeout:         time.Second * 10,
	}
}

// jumpFingerprintValues is what the jump route of a Host puts into the
// fingerprint of a connection to it: for every Host passed through, where it
// is, who logs in there and with what, the key it is trusted on and whether it
// is enabled. Those are what the connection is made through, so a change to
// any of them has to reach the connection the way a change to the Host itself
// does, and enabling a Host passed through is what brings back the connections
// that were waiting on it.
//
// A Host with no route puts nothing in, so its fingerprint is the one it had
// before routes existed and an upgrade does not rebuild every connection.
func jumpFingerprintValues(hops []jumpHop) []string {
	if len(hops) == 0 {
		return nil
	}

	values := []string{"jumps", strconv.Itoa(len(hops))}
	for _, hop := range hops {
		values = append(values, strconv.FormatUint(uint64(hop.id), 10))

		if hop.host == nil {
			values = append(values, "missing")
			continue
		}

		values = append(values, "present",
			hop.host.Address, strconv.Itoa(hop.host.Port), hop.host.User,
			hop.creds.password, hop.creds.privateKey, hop.creds.passphrase,
			hop.host.HostKey, strconv.FormatBool(hop.host.Enabled))
	}

	return values
}

// StartTunnel builds and starts the tunnel for one assignment. bindScope is
// what that assignment stored, models.HostServicePort.BindScope, and it decides
// the pair of addresses the forwarded port is asked to be opened on. It is
// passed in rather than read here because the reconcile pass has already read
// every assignment row, and reading it again would put a statement per tunnel
// onto the database every few seconds. jumps is the jump route of the Host,
// read by the pass for the same reason, and empty for a Host that is connected
// to directly.
func (m *Manager) StartTunnel(host *models.Host, sp *models.ServicePort, bindScope string, jumps []jumpHop) error {
	if !host.Enabled {
		m.logger.Info("skipped starting tunnel for disabled Host",
			logid.TunnelStartSkippedHostDisabled.Field(),
			zap.Uint("host_id", host.ID),
			zap.String("host_ip", host.Address),
			zap.Int("service_port", sp.ServicePort))
		return nil
	}

	key := tunnelKey(host.ID, sp.ID)
	if m.tunnelRegistered(key) {
		return fmt.Errorf("tunnel already exists")
	}

	// Everything the tunnel is built from is made before m.mu is taken.
	// Reading the private key of the Host and of every Host its route passes
	// through can take a good part of a second for a key with a passphrase, and m.mu
	// is what the status calls wait on. None of it reads m.tunnels, which is
	// all m.mu guards: the credentials come off the Host rows the caller read,
	// the signer is kept under a lock of its own, and the rest is the cipher,
	// the logger and the database, each of which is safe to share.
	auth, creds, err := m.hostAuth(host)
	if err != nil {
		return err
	}

	route, refusal, err := m.jumpRoute(host, jumps)
	if err != nil {
		return err
	}

	sshConfig := m.clientConfig(host, auth)

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

	t.jumps = route
	t.refusal = refusal

	// The settings this tunnel is connecting with, so a later pass can tell
	// whether the ones it should have are still the same.
	t.connFP = connectionFingerprint(host, sp, bindScope, creds, jumps)

	m.mu.Lock()

	// A stop of the tunnel under this key that has not finished is waited
	// for, since it is still to delete the row this start is about to make.
	for {
		stopped, stopping := m.stopping[key]
		if !stopping {
			break
		}

		m.mu.Unlock()
		<-stopped
		m.mu.Lock()
	}

	// Asked again under the lock, since another start of the same tunnel can
	// have registered it while this one was being built.
	if _, exists := m.tunnels[key]; exists {
		m.mu.Unlock()
		return fmt.Errorf("tunnel already exists")
	}

	// The tunnel is registered before its row is made, and the row is made
	// with m.mu let go. Making it takes as long as the database does, and
	// every status call and every pass waits on m.mu. Registered, the key is
	// taken for that time, so another start of the same tunnel is refused, and
	// whatever reads m.tunnels finds the tunnel there, which is what it found
	// when it had to wait for the row to be made under the lock. The key is
	// marked in m.starting until the row is made, which StopTunnel waits on.
	started := make(chan struct{})
	m.tunnels[key] = t
	m.starting[key] = started
	m.mu.Unlock()

	err = m.db.Where("host_id = ? AND sp_id = ?", host.ID, sp.ID).
		Attrs(tunnel).
		FirstOrCreate(&tunnel).Error

	if err == nil {
		go func(m *Manager, t *SSHTunnel, tunnel *models.Tunnel) {
			t.Start(m, tunnel)
		}(m, t, &tunnel)
	}

	m.mu.Lock()
	delete(m.starting, key)
	// A tunnel without a row was not started, so it does not keep the key
	// taken. Nothing else took it out in the meantime: a start of the same
	// tunnel is refused and a stop of it waits for this one.
	if err != nil {
		delete(m.tunnels, key)
	}
	m.mu.Unlock()
	close(started)

	if err != nil {
		return fmt.Errorf("failed to create tunnel information: %w", err)
	}

	return nil
}

// tunnelRegistered reports whether a tunnel runs under key, or is being started
// under it and its row is still being made. StartTunnel asks it before
// building anything, so that a tunnel that is running costs no reading of its
// key, and asks again under the lock before it registers one.
func (m *Manager) tunnelRegistered(key string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	_, exists := m.tunnels[key]
	return exists
}

// StopTunnel stops the tunnel running under the Host and the service port and
// deletes its row.
//
// m.mu is held only to take the tunnel out of m.tunnels and not while it is
// stopped. Stopping it waits for a write of its status that is under way and
// then deletes its row, both of which take as long as the database does, and
// every status call and every pass waits on m.mu. The key is marked in
// m.stopping for that time, which StartTunnel waits on.
//
// A tunnel whose start is still making its row is waited for, and stopped once
// the row is made. That is the tunnel the stop found when the start made the
// row under the lock, and a stop that went ahead would delete a row the start
// then makes. A start that could not make the row leaves nothing to stop.
func (m *Manager) StopTunnel(hostID uint, spID uint) error {
	key := tunnelKey(hostID, spID)

	m.mu.Lock()
	for {
		started, starting := m.starting[key]
		if !starting {
			break
		}

		m.mu.Unlock()
		<-started
		m.mu.Lock()
	}

	tunnel, exists := m.tunnels[key]
	if !exists {
		m.mu.Unlock()
		return ErrTunnelNotExist
	}

	stopped := make(chan struct{})
	delete(m.tunnels, key)
	m.stopping[key] = stopped
	m.mu.Unlock()

	err := tunnel.Stop(m)

	m.mu.Lock()
	delete(m.stopping, key)
	m.mu.Unlock()
	close(stopped)

	if err != nil {
		return fmt.Errorf("failed to stop tunnel: %w", err)
	}

	return nil
}

func (m *Manager) GetHostTunnels(hostID uint) (*[]models.Tunnel, error) {
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

// StopAllTunnels stops every running tunnel, local forward and SOCKS5 proxy.
// It is a reconcile pass with an empty desired state, and it reads no rows:
// what is running is in m.tunnels, m.localForwards and m.socksProxies, and a
// database that is away on shutdown must not leave a tunnel up.
func (m *Manager) StopAllTunnels() {
	defer m.stopAllSocks()
	defer m.stopAllLocalForwards()

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
