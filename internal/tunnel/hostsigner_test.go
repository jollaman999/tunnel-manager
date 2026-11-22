package tunnel

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
)

// countKeyReads puts a counter in front of the reading of stored private keys
// and returns it. The real reading still happens, so a key with a passphrase
// costs what it costs in service.
func countKeyReads(t *testing.T) *atomic.Int32 {
	t.Helper()

	var reads atomic.Int32
	saved := parseHostKey
	parseHostKey = func(keyPEM, passphrase string) (ssh.Signer, error) {
		reads.Add(1)
		return saved(keyPEM, passphrase)
	}
	t.Cleanup(func() {
		parseHostKey = saved
	})

	return &reads
}

// servicePortsOf returns n service ports with IDs from 1, for starting n
// tunnels of one Host.
func servicePortsOf(n int) []models.ServicePort {
	sps := make([]models.ServicePort, 0, n)
	for i := 1; i <= n; i++ {
		sps = append(sps, testServicePort(uint(i)))
	}

	return sps
}

// startOnFailingDB starts the tunnel of the Host for sp on a manager whose
// database refuses everything. That stops StartTunnel at the tunnel row, which
// comes after the key was read, so the key is read and nothing is connected.
func startOnFailingDB(t *testing.T, m *Manager, host *models.Host, sp *models.ServicePort) error {
	t.Helper()

	err := m.StartTunnel(host, sp, models.BindScopeWildcard, nil)
	if err == nil {
		t.Fatal("StartTunnel with a failing database returned no error")
	}

	return err
}

// TestStartTunnelReadsTheKeyOfAHostOnceForAllOfItsPorts covers a Host with a
// passphrase key and several service ports. The key is the same for all of
// them, and reading it is a bcrypt run, so it is read once and not once per
// port.
func TestStartTunnelReadsTheKeyOfAHostOnceForAllOfItsPorts(t *testing.T) {
	const ports = 4

	reads := countKeyReads(t)
	cipher := newTestCipher(t)

	m, err := NewManager(newFailingDB(t), zap.NewNop(), cipher, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	keyPEM, _ := testPrivateKey(t, "the passphrase of the test")
	host := sealedHost(t, cipher, keyPEM, "the passphrase of the test", "")

	for _, sp := range servicePortsOf(ports) {
		err := startOnFailingDB(t, m, host, &sp)
		if !strings.Contains(err.Error(), "failed to create tunnel information") {
			t.Fatalf("StartTunnel stopped before the tunnel row: %v", err)
		}
	}

	t.Logf("key reads for %d ports of one Host: %d", ports, reads.Load())
	if got := reads.Load(); got != 1 {
		t.Fatalf("the key of the Host was read %d times for %d ports, want 1", got, ports)
	}
}

// TestStartTunnelReadsAReplacedKeyAgain covers a key that is replaced on the
// Host. The kept signer belongs to the stored values it was read from, so the
// new ones are read and the connection is made with the new key.
func TestStartTunnelReadsAReplacedKeyAgain(t *testing.T) {
	reads := countKeyReads(t)
	cipher := newTestCipher(t)

	m, err := NewManager(newFailingDB(t), zap.NewNop(), cipher, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	oldPEM, _ := testPrivateKey(t, "the old passphrase")
	host := sealedHost(t, cipher, oldPEM, "the old passphrase", "")
	sps := servicePortsOf(3)

	startOnFailingDB(t, m, host, &sps[0])

	newPEM, newPublic := testPrivateKey(t, "the new passphrase")
	replaced := sealedHost(t, cipher, newPEM, "the new passphrase", "")
	host.PrivateKey, host.KeyPassphrase = replaced.PrivateKey, replaced.KeyPassphrase

	startOnFailingDB(t, m, host, &sps[1])
	startOnFailingDB(t, m, host, &sps[2])

	if got := reads.Load(); got != 2 {
		t.Fatalf("the key was read %d times over a replacement, want 2: once per key", got)
	}

	signer, err := m.hostSigner(host)
	if err != nil {
		t.Fatalf("hostSigner returned an error: %v", err)
	}
	if string(signer.PublicKey().Marshal()) != string(newPublic.Marshal()) {
		t.Fatal("the signer is the one of the key that was replaced")
	}
}

// TestHostSignerReadsAKeyWithAWrongPassphraseOnce covers a Host whose
// passphrase does not open its key. The answer is the same every time for the
// same stored values, so the bcrypt run is paid once, and every start is still
// refused with the reason.
func TestHostSignerReadsAKeyWithAWrongPassphraseOnce(t *testing.T) {
	reads := countKeyReads(t)
	cipher := newTestCipher(t)

	m, err := NewManager(newFailingDB(t), zap.NewNop(), cipher, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	keyPEM, _ := testPrivateKey(t, "the passphrase of the test")
	host := sealedHost(t, cipher, keyPEM, "not the passphrase", "")

	for _, sp := range servicePortsOf(3) {
		err := startOnFailingDB(t, m, host, &sp)

		var refused *KeyError
		if !errors.As(err, &refused) || !strings.Contains(err.Error(), "does not open") {
			t.Fatalf("StartTunnel with a wrong passphrase returned %v, want the key refusal", err)
		}
	}

	if got := reads.Load(); got != 1 {
		t.Fatalf("a key with a wrong passphrase was read %d times, want 1", got)
	}

	fixed := sealedHost(t, cipher, keyPEM, "the passphrase of the test", "")
	host.KeyPassphrase = fixed.KeyPassphrase

	_, err = m.hostSigner(host)
	if err != nil {
		t.Fatalf("the key was refused after its passphrase was put right: %v", err)
	}
	if got := reads.Load(); got != 2 {
		t.Fatalf("the key was read %d times after its passphrase was put right, want 2", got)
	}
}

// TestHostSignerForgetsTheKeyOfAHostThatDroppedIt covers a Host whose key is
// taken off it, leaving the password. Nothing of the key is kept for it.
func TestHostSignerForgetsTheKeyOfAHostThatDroppedIt(t *testing.T) {
	cipher := newTestCipher(t)

	m, err := NewManager(newFailingDB(t), zap.NewNop(), cipher, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	keyPEM, _ := testPrivateKey(t, "")
	host := sealedHost(t, cipher, keyPEM, "", "")

	_, err = m.hostSigner(host)
	if err != nil {
		t.Fatalf("hostSigner returned an error: %v", err)
	}

	host.PrivateKey = ""

	signer, err := m.hostSigner(host)
	if signer != nil || err != nil {
		t.Fatalf("hostSigner of a Host with no key returned %v, %v", signer, err)
	}
	if _, ok := m.signers.lookup(host.ID, "", ""); ok {
		t.Fatal("a signer is kept for a Host that carries no key")
	}
	if n := len(m.signers.entries); n != 0 {
		t.Fatalf("%d signers are kept, want none", n)
	}
}

// TestReconcileLetsGoOfTheKeysOfRemovedHosts covers a Host that is removed. A
// pass reads every Host, and the key of one that is not among them is dropped.
func TestReconcileLetsGoOfTheKeysOfRemovedHosts(t *testing.T) {
	hosts := []models.Host{enabledHost(1, true)}

	m, err := NewManager(newStubDB(t, hosts, nil, nil), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	t.Cleanup(m.StopAllTunnels)

	m.signers.keep(1, "key of 1", "", nil, nil)
	m.signers.keep(2, "key of 2", "", nil, nil)

	_, err = m.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}

	if _, ok := m.signers.lookup(1, "key of 1", ""); !ok {
		t.Fatal("the key of a Host that is still there was dropped")
	}
	if _, ok := m.signers.lookup(2, "key of 2", ""); ok {
		t.Fatal("the key of a removed Host is still kept")
	}
}

// holdKeyReads makes every reading of a stored key report on arrived and then
// wait for release to be closed. The read is refused once it is let go, so
// nothing is connected.
func holdKeyReads(t *testing.T) (arrived chan struct{}, release chan struct{}) {
	t.Helper()

	arrived = make(chan struct{}, 8)
	release = make(chan struct{})

	saved := parseHostKey
	parseHostKey = func(string, string) (ssh.Signer, error) {
		arrived <- struct{}{}
		<-release
		return nil, keyError("held by the test")
	}
	t.Cleanup(func() {
		parseHostKey = saved
	})

	return arrived, release
}

// awaitWithin runs call and reports whether it returned inside the time given.
func awaitWithin(call func(), within time.Duration) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		call()
	}()

	select {
	case <-done:
		return true
	case <-time.After(within):
		return false
	}
}

// TestGetHostTunnelsDoesNotWaitForAKeyBeingRead covers the status calls while a
// tunnel is being started with a passphrase key. The key is read before the
// start takes any lock the status calls could wait on, so they answer at once
// rather than after the bcrypt run.
func TestGetHostTunnelsDoesNotWaitForAKeyBeingRead(t *testing.T) {
	cipher := newTestCipher(t)

	m, err := NewManager(newFailingDB(t), zap.NewNop(), cipher, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	entered := make(chan struct{})
	var parseTook time.Duration

	saved := parseHostKey
	parseHostKey = func(keyPEM, passphrase string) (ssh.Signer, error) {
		close(entered)
		begun := time.Now()
		signer, err := saved(keyPEM, passphrase)
		parseTook = time.Since(begun)
		return signer, err
	}
	t.Cleanup(func() {
		parseHostKey = saved
	})

	keyPEM, _ := testPrivateKey(t, "the passphrase of the test")
	host := sealedHost(t, cipher, keyPEM, "the passphrase of the test", "")
	sp := testServicePort(1)

	started := make(chan struct{})
	go func() {
		defer close(started)
		_ = m.StartTunnel(host, &sp, models.BindScopeWildcard, nil)
	}()

	<-entered
	begun := time.Now()
	_, _ = m.GetHostTunnels(host.ID)
	waited := time.Since(begun)

	<-started

	t.Logf("reading the key took %v, GetHostTunnels waited %v", parseTook, waited)
	if waited*2 >= parseTook {
		t.Fatalf("GetHostTunnels waited %v while the key was read in %v: it waited for the read", waited, parseTook)
	}
}

// TestStartTunnelRegistersOneOfTwoStartsOfTheSameTunnel covers two starts of
// one tunnel that both read the key before either took the lock. The second
// one to take it finds the first registered and leaves it.
func TestStartTunnelRegistersOneOfTwoStartsOfTheSameTunnel(t *testing.T) {
	_, port, err := net.SplitHostPort(closedPort(t))
	if err != nil {
		t.Fatalf("failed to read the port: %v", err)
	}

	hostPort, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("failed to read the port: %v", err)
	}

	cipher := newTestCipher(t)

	m, err := NewManager(newWritableStubDB(t, nil, nil), zap.NewNop(), cipher, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	t.Cleanup(m.StopAllTunnels)

	keyPEM, _ := testPrivateKey(t, "")
	host := sealedHost(t, cipher, keyPEM, "", "s3cr3t")
	host.Port = hostPort
	sp := testServicePort(1)

	arrived, release := holdKeyReads(t)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = m.StartTunnel(host, &sp, models.BindScopeWildcard, nil)
		}()
	}

	for i := 0; i < 2; i++ {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			close(release)
			wg.Wait()
			t.Fatalf("only %d of two starts reached the key: the other waited for the lock", i)
		}
	}
	close(release)
	wg.Wait()

	var started, refused int
	for _, err := range errs {
		switch {
		case err == nil:
			started++
		case strings.Contains(err.Error(), "tunnel already exists"):
			refused++
		default:
			t.Fatalf("StartTunnel returned %v", err)
		}
	}
	if started != 1 || refused != 1 {
		t.Fatalf("%d starts registered the tunnel and %d were refused, want one of each", started, refused)
	}

	m.mu.RLock()
	running := len(m.tunnels)
	m.mu.RUnlock()
	if running != 1 {
		t.Fatalf("%d tunnels are registered, want 1", running)
	}
}

// TestStartLocalForwardDoesNotHoldItsLockWhileTheKeyIsRead covers the local
// forwards, which are started the way the tunnels are and answer their own
// status calls under m.localMu.
func TestStartLocalForwardDoesNotHoldItsLockWhileTheKeyIsRead(t *testing.T) {
	cipher := newTestCipher(t)

	m, err := NewManager(newFailingDB(t), zap.NewNop(), cipher, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	keyPEM, _ := testPrivateKey(t, "")
	host := sealedHost(t, cipher, keyPEM, "", "")
	lf := models.LocalForward{HostID: host.ID, Number: 1, LocalPort: 18501, TargetAddress: "127.0.0.1", TargetPort: 1}

	arrived, release := holdKeyReads(t)

	done := make(chan error, 1)
	go func() {
		done <- m.startLocalForward(host, &lf, nil)
	}()

	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("startLocalForward did not reach the key")
	}

	answered := awaitWithin(func() { m.LocalForwardStatuses() }, time.Second)
	close(release)

	if err := <-done; err == nil {
		t.Fatal("startLocalForward with a refused key and no password returned no error")
	}
	if !answered {
		t.Fatal("LocalForwardStatuses waited for the key of a forward being started")
	}
}

// TestStartSocksDoesNotHoldItsLockWhileTheKeyIsRead is the same for the SOCKS5
// proxies and m.socksMu.
func TestStartSocksDoesNotHoldItsLockWhileTheKeyIsRead(t *testing.T) {
	cipher := newTestCipher(t)

	m, err := NewManager(newFailingDB(t), zap.NewNop(), cipher, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	keyPEM, _ := testPrivateKey(t, "")
	host := sealedHost(t, cipher, keyPEM, "", "")

	arrived, release := holdKeyReads(t)

	done := make(chan error, 1)
	go func() {
		done <- m.startSocks(host, nil)
	}()

	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("startSocks did not reach the key")
	}

	answered := awaitWithin(func() { m.SocksStatuses() }, time.Second)
	close(release)

	if err := <-done; err == nil {
		t.Fatal("startSocks with a refused key and no password returned no error")
	}
	if !answered {
		t.Fatal("SocksStatuses waited for the key of a proxy being started")
	}
}
