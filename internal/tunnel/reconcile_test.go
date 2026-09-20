package tunnel

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"
)

// enabledHost is a Host a tunnel can be built for without any connection being
// made: port 1 on loopback refuses right away, so a tunnel that is started ends
// its first attempt without leaving the machine.
func enabledHost(id uint, enabled bool) models.Host {
	return models.Host{ID: id, IP: "127.0.0.1", Port: 1, User: "user", Password: "pass", Enabled: enabled}
}

func testServicePort(id uint) models.ServicePort {
	return models.ServicePort{ID: id, ServiceIP: "127.0.0.1", ServicePort: 1, LocalPort: 18080 + int(id)}
}

// newWritableStubDB returns a stub that answers Host and ServicePort queries and
// accepts writes, so StartTunnel gets past the tunnel row and registers the
// tunnel. No statement reaches the failing connection pool.
func newWritableStubDB(t *testing.T, hosts []models.Host, sps []models.ServicePort) *gorm.DB {
	t.Helper()

	db := newStubDB(t, hosts, sps, nil)

	err := db.Callback().Create().Replace("gorm:create", func(tx *gorm.DB) {})
	if err != nil {
		t.Fatalf("failed to replace the create callback: %v", err)
	}

	err = db.Callback().Update().Replace("gorm:update", func(tx *gorm.DB) {})
	if err != nil {
		t.Fatalf("failed to replace the update callback: %v", err)
	}

	return db
}

// registerStoppedTunnel puts a tunnel into the map of running tunnels without
// starting it, so a test can work on a tunnel the manager sees as running while
// no SSH connection exists.
func registerStoppedTunnel(t *testing.T, m *Manager, hostID, spID uint) *SSHTunnel {
	t.Helper()

	tun, err := NewSSHTunnel(&hostID, &spID, "0.0.0.0:18081", "127.0.0.1:1", "127.0.0.1:1", nil, zap.NewNop())
	if err != nil {
		t.Fatalf("failed to create tunnel: %v", err)
	}

	m.mu.Lock()
	m.tunnels[tunnelKey(hostID, spID)] = tun
	m.mu.Unlock()

	return tun
}

func runningKeys(m *Manager) map[string]*SSHTunnel {
	m.mu.RLock()
	defer m.mu.RUnlock()

	running := make(map[string]*SSHTunnel, len(m.tunnels))
	for key, tun := range m.tunnels {
		running[key] = tun
	}

	return running
}

func waitFor(t *testing.T, timeout time.Duration, what string, done func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}

	if !done() {
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestReconcileStartsWhatIsMissing(t *testing.T) {
	hosts := []models.Host{enabledHost(1, true)}
	sps := []models.ServicePort{testServicePort(2)}

	m, err := NewManager(newWritableStubDB(t, hosts, sps), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	t.Cleanup(m.StopAllTunnels)

	result, err := m.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
	if result.Started != 1 || result.Stopped != 0 || result.Failed != 0 {
		t.Fatalf("Reconcile reported started=%d stopped=%d failed=%d, want 1/0/0",
			result.Started, result.Stopped, result.Failed)
	}

	running := runningKeys(m)
	if _, ok := running["1-2"]; !ok || len(running) != 1 {
		t.Fatalf("the tunnel of the desired state is not running: %v", running)
	}
}

func TestReconcileStopsWhatIsNotWanted(t *testing.T) {
	// No rows at all, so nothing is wanted and the running tunnel is left over.
	m, err := NewManager(newStubDB(t, nil, nil, nil), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	tun := registerStoppedTunnel(t, m, 1, 2)

	result, err := m.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
	if result.Started != 0 || result.Stopped != 1 || result.Failed != 0 {
		t.Fatalf("Reconcile reported started=%d stopped=%d failed=%d, want 0/1/0",
			result.Started, result.Stopped, result.Failed)
	}

	if len(runningKeys(m)) != 0 {
		t.Fatal("a tunnel that is not wanted is still registered")
	}

	tun.stopMu.Lock()
	stopped := tun.isStopped
	tun.stopMu.Unlock()

	if !stopped {
		t.Fatal("the tunnel that is not wanted was unregistered without being stopped")
	}
}

func TestReconcileLeavesRunningTunnelsAlone(t *testing.T) {
	hosts := []models.Host{enabledHost(1, true)}
	sps := []models.ServicePort{testServicePort(2)}

	m, err := NewManager(newStubDB(t, hosts, sps, nil), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	tun := registerStoppedTunnel(t, m, 1, 2)
	// The tunnel runs with the settings the desired state holds, which is what
	// the pass reads from its fingerprint. The stored password is plaintext, so
	// it is the password itself.
	tun.connFP = connectionFingerprint(&hosts[0], &sps[0], hostCreds{password: hosts[0].Password})

	result, err := m.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
	if result.Started != 0 || result.Stopped != 0 || result.Restarted != 0 || result.Failed != 0 {
		t.Fatalf("Reconcile reported started=%d stopped=%d restarted=%d failed=%d for a combination that is on both sides, want 0/0/0/0",
			result.Started, result.Stopped, result.Restarted, result.Failed)
	}

	running := runningKeys(m)
	if running["1-2"] != tun {
		t.Fatal("the tunnel that was already running was replaced")
	}

	tun.stopMu.Lock()
	stopped := tun.isStopped
	tun.stopMu.Unlock()

	if stopped {
		t.Fatal("the tunnel that was already running was stopped")
	}
}

func TestReconcileStopsTunnelsOfADisabledHost(t *testing.T) {
	hosts := []models.Host{enabledHost(1, false)}
	sps := []models.ServicePort{testServicePort(2)}

	m, err := NewManager(newStubDB(t, hosts, sps, nil), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	registerStoppedTunnel(t, m, 1, 2)

	result, err := m.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
	if result.Stopped != 1 || result.Started != 0 || result.Failed != 0 {
		t.Fatalf("Reconcile reported started=%d stopped=%d failed=%d for a disabled Host, want 0/1/0",
			result.Started, result.Stopped, result.Failed)
	}

	if len(runningKeys(m)) != 0 {
		t.Fatal("the tunnel of a disabled Host is still registered")
	}
}

func TestReconcileChangesNothingWhenTheDatabaseFails(t *testing.T) {
	m, err := NewManager(newFailingDB(t), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	tun := registerStoppedTunnel(t, m, 1, 2)

	result, err := m.Reconcile()
	if err == nil {
		t.Fatal("Reconcile with a failing database returned no error")
	}
	if result.Started != 0 || result.Stopped != 0 || result.Failed != 0 {
		t.Fatalf("Reconcile reported started=%d stopped=%d failed=%d although it could not read the desired state",
			result.Started, result.Stopped, result.Failed)
	}

	running := runningKeys(m)
	if running["1-2"] != tun || len(running) != 1 {
		t.Fatalf("the running tunnels were changed by a pass that could not read the desired state: %v", running)
	}
}

func TestWakeReconcileDoesNotBlock(t *testing.T) {
	// Nobody reads the wake-ups: the loop is not running.
	m, err := NewManager(newFailingDB(t), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		for i := 0; i < 100; i++ {
			m.WakeReconcile()
		}
	}()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("WakeReconcile did not return while no reconcile loop is running")
	}

	if len(m.reconcileWake) != 1 {
		t.Fatalf("%d wake-ups are queued, want 1", len(m.reconcileWake))
	}
}

// newPassCountingDB counts the reconcile passes that read the desired state.
// The desired state stays empty, so a pass starts and stops nothing.
func newPassCountingDB(t *testing.T, passes *atomic.Int64) *gorm.DB {
	t.Helper()

	db := newFailingDB(t)

	err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*[]models.Host); ok {
			passes.Add(1)
		}
	})
	if err != nil {
		t.Fatalf("failed to replace the query callback: %v", err)
	}

	return db
}

func TestRunReconcileLoopRunsOnAWakeAndEndsWithTheContext(t *testing.T) {
	var passes atomic.Int64

	m, err := NewManager(newPassCountingDB(t, &passes), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		// The interval is long enough that only a wake-up can run the second pass.
		m.RunReconcileLoop(ctx, 3600)
	}()

	waitFor(t, 5*time.Second, "the first pass", func() bool {
		return passes.Load() >= 1
	})

	m.WakeReconcile()

	waitFor(t, 5*time.Second, "the pass a wake-up asks for", func() bool {
		return passes.Load() >= 2
	})

	cancel()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("RunReconcileLoop did not return after the context was cancelled")
	}
}

func TestRunReconcileLoopRunsOnTheInterval(t *testing.T) {
	var passes atomic.Int64

	m, err := NewManager(newPassCountingDB(t, &passes), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		m.RunReconcileLoop(ctx, 1)
	}()

	// No wake-up is sent, so a second pass can only come from the ticker. The
	// database fails on every pass, which must not end the loop either.
	waitFor(t, 10*time.Second, "the pass the interval asks for", func() bool {
		return passes.Load() >= 2
	})

	cancel()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("RunReconcileLoop did not return after the context was cancelled")
	}
}

func TestReconcileIntervalFallsBackForAnUnusableInterval(t *testing.T) {
	if reconcileInterval(0) != defaultReconcileIntervalSec*time.Second {
		t.Fatalf("an interval of 0 became %s", reconcileInterval(0))
	}
	if reconcileInterval(-1) != defaultReconcileIntervalSec*time.Second {
		t.Fatalf("a negative interval became %s", reconcileInterval(-1))
	}
	if reconcileInterval(3) != 3*time.Second {
		t.Fatalf("an interval of 3 became %s", reconcileInterval(3))
	}
}

func TestParseTunnelKeyRoundTrip(t *testing.T) {
	hostID, spID, ok := parseTunnelKey(tunnelKey(12, 34))
	if !ok || hostID != 12 || spID != 34 {
		t.Fatalf("parseTunnelKey returned %d, %d, %v", hostID, spID, ok)
	}

	if _, _, ok := parseTunnelKey("12"); ok {
		t.Fatal("parseTunnelKey accepted a key without a separator")
	}
	if _, _, ok := parseTunnelKey("a-1"); ok {
		t.Fatal("parseTunnelKey accepted a key that is not made of numbers")
	}
}

// encryptedPassword returns what a Host row holds for plaintext.
func encryptedPassword(t *testing.T, c *crypto.Cipher, plaintext string) string {
	t.Helper()

	encrypted, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("failed to encrypt: %v", err)
	}

	return encrypted
}

// startedTunnelManager runs the first pass, so every combination of hosts and
// sps is running with the fingerprint of the settings it was built from. The
// stub answers from the slices the caller holds, so a value changed in them is
// what the next pass reads.
func startedTunnelManager(t *testing.T, hosts []models.Host, sps []models.ServicePort, c *crypto.Cipher, logger *zap.Logger) *Manager {
	t.Helper()

	m, err := NewManager(newWritableStubDB(t, hosts, sps), logger, c, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	t.Cleanup(m.StopAllTunnels)

	result, err := m.Reconcile()
	if err != nil {
		t.Fatalf("the first pass returned an error: %v", err)
	}
	if result.Started != len(hosts)*len(sps) {
		t.Fatalf("the first pass started %d tunnels, want %d", result.Started, len(hosts)*len(sps))
	}

	return m
}

func TestReconcileRestartsWhenTheConnectionSettingsChange(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, c *crypto.Cipher, host *models.Host, sp *models.ServicePort)
	}{
		{"the server IP", func(_ *testing.T, _ *crypto.Cipher, host *models.Host, _ *models.ServicePort) {
			host.IP = "127.0.0.2"
		}},
		{"the server port", func(_ *testing.T, _ *crypto.Cipher, host *models.Host, _ *models.ServicePort) {
			host.Port = 2
		}},
		{"the remote IP", func(_ *testing.T, _ *crypto.Cipher, _ *models.Host, sp *models.ServicePort) {
			sp.ServiceIP = "127.0.0.2"
		}},
		{"the remote port", func(_ *testing.T, _ *crypto.Cipher, _ *models.Host, sp *models.ServicePort) {
			sp.ServicePort = 2
		}},
		{"the local port", func(_ *testing.T, _ *crypto.Cipher, _ *models.Host, sp *models.ServicePort) {
			sp.LocalPort = 18099
		}},
		{"the user", func(_ *testing.T, _ *crypto.Cipher, host *models.Host, _ *models.ServicePort) {
			host.User = "other"
		}},
		{"the password", func(t *testing.T, c *crypto.Cipher, host *models.Host, _ *models.ServicePort) {
			host.Password = encryptedPassword(t, c, "fake-value-2") // hook:allow
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cipher := newTestCipher(t)

			hosts := []models.Host{enabledHost(1, true)}
			hosts[0].Password = encryptedPassword(t, cipher, "fake-value-1") // hook:allow
			sps := []models.ServicePort{testServicePort(2)}

			m := startedTunnelManager(t, hosts, sps, cipher, zap.NewNop())
			before := runningKeys(m)["1-2"]
			if before == nil {
				t.Fatal("the first pass registered no tunnel")
			}

			tc.change(t, cipher, &hosts[0], &sps[0])

			result, err := m.Reconcile()
			if err != nil {
				t.Fatalf("Reconcile returned an error: %v", err)
			}
			if result.Restarted != 1 || result.Started != 0 || result.Stopped != 0 || result.Failed != 0 {
				t.Fatalf("Reconcile reported started=%d stopped=%d restarted=%d failed=%d after %s changed, want 0/0/1/0",
					result.Started, result.Stopped, result.Restarted, result.Failed, tc.name)
			}

			after := runningKeys(m)["1-2"]
			if after == nil {
				t.Fatalf("no tunnel is registered after %s changed", tc.name)
			}
			if after == before {
				t.Fatalf("the tunnel kept running on the settings it was built with after %s changed", tc.name)
			}

			before.stopMu.Lock()
			stopped := before.isStopped
			before.stopMu.Unlock()

			if !stopped {
				t.Fatalf("the tunnel built with the old settings was replaced without being stopped after %s changed", tc.name)
			}
		})
	}
}

// reconcileIsQuiet runs passes and fails if any of them changes anything or
// replaces the tunnel that is running.
func reconcileIsQuiet(t *testing.T, m *Manager, passes int) {
	t.Helper()

	running := runningKeys(m)["1-2"]
	if running == nil {
		t.Fatal("no tunnel is registered")
	}

	for pass := 1; pass <= passes; pass++ {
		result, err := m.Reconcile()
		if err != nil {
			t.Fatalf("pass %d returned an error: %v", pass, err)
		}
		if result != (ReconcileResult{}) {
			t.Fatalf("pass %d reported started=%d stopped=%d restarted=%d failed=%d although nothing changed, want 0/0/0/0",
				pass, result.Started, result.Stopped, result.Restarted, result.Failed)
		}
		if runningKeys(m)["1-2"] != running {
			t.Fatalf("pass %d replaced a tunnel nothing changed for", pass)
		}
	}
}

func TestReconcileDoesNothingOnTwoPassesInARowWhenNothingChanged(t *testing.T) {
	cipher := newTestCipher(t)

	hosts := []models.Host{enabledHost(1, true)}
	hosts[0].Password = encryptedPassword(t, cipher, "fake-value-1") // hook:allow
	sps := []models.ServicePort{testServicePort(2)}

	m := startedTunnelManager(t, hosts, sps, cipher, zap.NewNop())

	reconcileIsQuiet(t, m, 2)
}

func TestReconcileDoesNothingAfterTheStoredPasswordIsEncryptedInPlace(t *testing.T) {
	// The stored password is plaintext here, so the first pass that reads it
	// writes it back encrypted. The password is sealed with a fresh nonce every
	// time, so a fingerprint taken from the stored value instead of the password
	// itself would differ on every pass and restart the tunnel on every pass.
	hosts := []models.Host{enabledHost(1, true)}
	sps := []models.ServicePort{testServicePort(2)}

	m := startedTunnelManager(t, hosts, sps, newTestCipher(t), zap.NewNop())

	if !crypto.IsEncrypted(hosts[0].Password) {
		t.Fatal("the plaintext password was not stored encrypted by the first pass")
	}

	reconcileIsQuiet(t, m, 2)
}

func TestReconcileKeepsTheTunnelWhoseStoredPasswordCannotBeRead(t *testing.T) {
	cipher := newTestCipher(t)

	hosts := []models.Host{enabledHost(1, true)}
	hosts[0].Password = encryptedPassword(t, cipher, "fake-value-1") // hook:allow
	sps := []models.ServicePort{testServicePort(2)}

	m := startedTunnelManager(t, hosts, sps, cipher, zap.NewNop())
	running := runningKeys(m)["1-2"]

	// Sealed with another key from here on, as if the key file was replaced.
	hosts[0].Password = encryptedPassword(t, newTestCipher(t), "fake-value-1") // hook:allow

	result, err := m.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
	if result.Failed != 1 || result.Started != 0 || result.Stopped != 0 || result.Restarted != 0 {
		t.Fatalf("Reconcile reported started=%d stopped=%d restarted=%d failed=%d for a password that does not decrypt, want 0/0/0/1",
			result.Started, result.Stopped, result.Restarted, result.Failed)
	}

	after := runningKeys(m)["1-2"]
	if after != running {
		t.Fatal("the tunnel was torn down although it could not be told whether its settings changed")
	}

	running.stopMu.Lock()
	stopped := running.isStopped
	running.stopMu.Unlock()

	if stopped {
		t.Fatal("the tunnel was stopped although it could not be told whether its settings changed")
	}
}

func TestReconcileKeepsTheFingerprintOutOfTheLogs(t *testing.T) {
	cipher := newTestCipher(t)

	hosts := []models.Host{enabledHost(1, true)}
	hosts[0].Password = encryptedPassword(t, cipher, "fake-value-1") // hook:allow
	sps := []models.ServicePort{testServicePort(2)}

	core, logs := observer.New(zapcore.DebugLevel)

	m := startedTunnelManager(t, hosts, sps, cipher, zap.New(core))

	hosts[0].Password = encryptedPassword(t, cipher, "fake-value-2") // hook:allow

	result, err := m.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
	if result.Restarted != 1 {
		t.Fatalf("the pass restarted %d tunnels, want 1", result.Restarted)
	}

	// Both fingerprints are derived from a password, so neither may show up
	// anywhere in what was logged, in any of the forms it can be written in.
	secrets := []string{"fake-value-1", "fake-value-2"}                 // hook:allow
	for _, password := range []string{"fake-value-1", "fake-value-2"} { // hook:allow
		fp := connectionFingerprint(&hosts[0], &sps[0], hostCreds{password: password})
		secrets = append(secrets,
			hex.EncodeToString(fp[:]),
			fmt.Sprint(fp),
			fmt.Sprintf("%v", fp[:]))
	}

	for _, entry := range logs.All() {
		logged := entry.Message + " " + fmt.Sprint(entry.ContextMap())
		for _, secret := range secrets {
			if strings.Contains(logged, secret) {
				t.Fatalf("a value derived from the password was logged in %q", entry.Message)
			}
		}
	}
}

// TestDesiredTunnelCountCountsWhatAPassWouldStart pins down that the count the
// status API is answered from is the size of the desired state of a reconcile
// pass: every service port on every enabled Host.
func TestDesiredTunnelCountCountsWhatAPassWouldStart(t *testing.T) {
	tests := []struct {
		name  string
		hosts []models.Host
		sps   []models.ServicePort
		want  int
	}{
		{
			name:  "every combination of the enabled hosts and the service ports",
			hosts: []models.Host{enabledHost(1, true), enabledHost(2, true)},
			sps:   []models.ServicePort{testServicePort(1), testServicePort(2), testServicePort(3)},
			want:  6,
		},
		{
			name:  "a host that is not enabled counts for nothing",
			hosts: []models.Host{enabledHost(1, true), enabledHost(2, false)},
			sps:   []models.ServicePort{testServicePort(1), testServicePort(2), testServicePort(3)},
			want:  3,
		},
		{
			name:  "no service port leaves nothing to run",
			hosts: []models.Host{enabledHost(1, true), enabledHost(2, true)},
			sps:   nil,
			want:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := NewManager(newStubDB(t, tt.hosts, tt.sps, nil), zap.NewNop(), newTestCipher(t), 1)
			if err != nil {
				t.Fatalf("failed to create manager: %v", err)
			}

			count, err := m.DesiredTunnelCount()
			if err != nil {
				t.Fatalf("DesiredTunnelCount returned error: %v", err)
			}
			if count != tt.want {
				t.Fatalf("DesiredTunnelCount = %d, want %d", count, tt.want)
			}

			// The same state a pass works from, read through the loop itself.
			desired, err := m.desiredTunnels()
			if err != nil {
				t.Fatalf("desiredTunnels returned error: %v", err)
			}
			if len(desired) != count {
				t.Fatalf("the count is %d while a pass desires %d tunnels", count, len(desired))
			}
		})
	}
}

// TestDesiredTunnelCountReportsAFailedRead pins down that rows that cannot be
// read are an error and not a count of zero, which would read as nothing being
// wanted.
func TestDesiredTunnelCountReportsAFailedRead(t *testing.T) {
	m, err := NewManager(newFailingDB(t), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	_, err = m.DesiredTunnelCount()
	if err == nil {
		t.Fatal("DesiredTunnelCount returned no error on a database that cannot be read")
	}
}

// TestReconcileRestartsATunnelWhoseKeyChanged pins down that replacing the
// private key of a Host is noticed by a pass, the same as replacing its
// password.
//
// It was not, once. The fingerprint a pass compares against was taken over the
// addresses, the user and the password alone, so a Host whose key was swapped
// kept its tunnel on the key it was built with. The save answered as though it
// had taken, and the new key was not tried until something else dropped the
// connection, which reads as the key failing rather than as the key never
// having been used.
func TestReconcileRestartsATunnelWhoseKeyChanged(t *testing.T) {
	cipher := newTestCipher(t)

	firstPEM, _ := testPrivateKey(t, "")
	secondPEM, _ := testPrivateKey(t, "")

	sealed := func(value string) string {
		t.Helper()

		out, err := cipher.Encrypt(value)
		if err != nil {
			t.Fatalf("failed to seal a test value: %v", err)
		}

		return out
	}

	hosts := []models.Host{enabledHost(1, true)}
	hosts[0].Password = ""
	hosts[0].PrivateKey = sealed(firstPEM)
	sps := []models.ServicePort{testServicePort(2)}

	m := startedTunnelManager(t, hosts, sps, cipher, zap.NewNop())

	// The same key again, sealed a second time. The stored form differs
	// because every seal carries its own nonce, and a pass must not read that
	// as a key that changed.
	hosts[0].PrivateKey = sealed(firstPEM)

	result, err := m.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
	if result.Restarted != 0 {
		t.Fatalf("resealing the same key restarted %d tunnels, want 0", result.Restarted)
	}

	hosts[0].PrivateKey = sealed(secondPEM)

	result, err = m.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
	if result.Restarted != 1 {
		t.Fatalf("replacing the key restarted %d tunnels, want 1", result.Restarted)
	}
}

// TestReconcileRestartsATunnelWhoseKeyPassphraseChanged is the same for the
// passphrase. A key that is opened with a different passphrase is a different
// credential, whatever the key bytes are.
func TestReconcileRestartsATunnelWhoseKeyPassphraseChanged(t *testing.T) {
	cipher := newTestCipher(t)

	keyPEM, _ := testPrivateKey(t, "first-phrase") // hook:allow

	sealed := func(value string) string {
		t.Helper()

		out, err := cipher.Encrypt(value)
		if err != nil {
			t.Fatalf("failed to seal a test value: %v", err)
		}

		return out
	}

	// The Host carries a password as well, so that the restart this is looking
	// for succeeds: a key that no longer opens leaves the password as the way
	// in, and a restart that failed would be counted as a failure rather than
	// as the restart this is about.
	hosts := []models.Host{enabledHost(1, true)}
	hosts[0].Password = sealed("fake-value-1") // hook:allow
	hosts[0].PrivateKey = sealed(keyPEM)
	hosts[0].KeyPassphrase = sealed("first-phrase") // hook:allow
	sps := []models.ServicePort{testServicePort(2)}

	m := startedTunnelManager(t, hosts, sps, cipher, zap.NewNop())

	hosts[0].KeyPassphrase = sealed("first-phrase") // hook:allow

	result, err := m.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
	if result.Restarted != 0 {
		t.Fatalf("resealing the same passphrase restarted %d tunnels, want 0", result.Restarted)
	}

	hosts[0].KeyPassphrase = sealed("second-phrase") // hook:allow

	result, err = m.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
	if result.Restarted != 1 {
		t.Fatalf("replacing the key passphrase restarted %d tunnels, want 1", result.Restarted)
	}
}

// assignment is one row of the assignment table, written short so that a test
// can list the pairs it means.
func assignment(hostID, spID uint) models.HostServicePort {
	return models.HostServicePort{HostID: hostID, SPID: spID}
}

// desiredKeys is the desired state as the sorted keys it holds, so a failure
// says which combinations a pass wanted.
func desiredKeys(t *testing.T, m *Manager) []string {
	t.Helper()

	desired, err := m.desiredTunnels()
	if err != nil {
		t.Fatalf("desiredTunnels returned an error: %v", err)
	}

	keys := make([]string, 0, len(desired))
	for key := range desired {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	return keys
}

// TestDesiredTunnelsTakesTheAssignedCombinationsOnly pins down what the
// assignment table is for: a pass wants the pairs it holds and not every Host
// against every service port. Two Hosts and three service ports used to be six
// tunnels whatever the operator wanted; with two assignments removed it is
// four, and which four is the point.
func TestDesiredTunnelsTakesTheAssignedCombinationsOnly(t *testing.T) {
	hosts := []models.Host{enabledHost(1, true), enabledHost(2, true)}
	hosts[1].IP = "127.0.0.2"
	sps := []models.ServicePort{testServicePort(1), testServicePort(2), testServicePort(3)}

	// The six combinations the table is filled with, less 1-2 and 2-3.
	assignments := []models.HostServicePort{
		assignment(1, 1), assignment(1, 3),
		assignment(2, 1), assignment(2, 2),
	}

	m, err := NewManager(newAssignedStubDB(t, hosts, sps, assignments, nil), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	got := desiredKeys(t, m)
	want := "1-1,1-3,2-1,2-2"
	if strings.Join(got, ",") != want {
		t.Fatalf("a pass wants %v, want %s", got, want)
	}

	count, err := m.DesiredTunnelCount()
	if err != nil {
		t.Fatalf("DesiredTunnelCount returned an error: %v", err)
	}
	if count != len(got) {
		t.Fatalf("the count is %d while a pass wants %d tunnels", count, len(got))
	}
}

// TestDesiredTunnelsKeepsTheAssignmentsOfADisabledHost pins down that the two
// questions stay apart: a Host that is not enabled runs no tunnel, and the
// assignments it keeps bring its tunnels back when it is enabled again.
func TestDesiredTunnelsKeepsTheAssignmentsOfADisabledHost(t *testing.T) {
	hosts := []models.Host{enabledHost(1, true), enabledHost(2, false)}
	hosts[1].IP = "127.0.0.2"
	sps := []models.ServicePort{testServicePort(1), testServicePort(2)}

	assignments := []models.HostServicePort{
		assignment(1, 1),
		assignment(2, 1), assignment(2, 2),
	}

	// The stub answers off the slice, so enabling the Host again is the same
	// write the API makes to the row.
	db := newAssignedStubDB(t, hosts, sps, assignments, nil)

	m, err := NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	got := desiredKeys(t, m)
	if strings.Join(got, ",") != "1-1" {
		t.Fatalf("a pass wants %v while a Host is disabled, want the tunnel of the enabled one alone", got)
	}

	hosts[1].Enabled = true

	got = desiredKeys(t, m)
	if strings.Join(got, ",") != "1-1,2-1,2-2" {
		t.Fatalf("a pass wants %v after the Host was enabled again, want its assignments back", got)
	}
}

// TestDesiredTunnelsPassesOverAnAssignmentWithNoRow pins down what happens to a
// row naming a Host or a service port that is not there. Nothing can be built
// from it, so it is left out, and the assignments beside it are unaffected: a
// leftover row must not take the tunnels of the rows that are still there down
// with it.
func TestDesiredTunnelsPassesOverAnAssignmentWithNoRow(t *testing.T) {
	hosts := []models.Host{enabledHost(1, true)}
	sps := []models.ServicePort{testServicePort(2)}

	assignments := []models.HostServicePort{
		assignment(1, 2),
		// A Host that was deleted, and a service port that was.
		assignment(9, 2),
		assignment(1, 9),
	}

	m, err := NewManager(newAssignedStubDB(t, hosts, sps, assignments, nil), zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	got := desiredKeys(t, m)
	if strings.Join(got, ",") != "1-2" {
		t.Fatalf("a pass wants %v beside two assignments whose rows are gone, want the one that has both", got)
	}
}

// TestReconcileIgnoresAnAssignmentWithNoRow is the same seen from a pass: the
// leftover row starts nothing, and the tunnel that is running for an assignment
// that is whole is neither stopped nor counted as failed.
func TestReconcileIgnoresAnAssignmentWithNoRow(t *testing.T) {
	hosts := []models.Host{enabledHost(1, true)}
	sps := []models.ServicePort{testServicePort(2)}

	assignments := []models.HostServicePort{assignment(1, 2), assignment(9, 2)}

	db := newAssignedStubDB(t, hosts, sps, assignments, nil)

	err := db.Callback().Create().Replace("gorm:create", func(tx *gorm.DB) {})
	if err != nil {
		t.Fatalf("failed to replace the create callback: %v", err)
	}

	err = db.Callback().Update().Replace("gorm:update", func(tx *gorm.DB) {})
	if err != nil {
		t.Fatalf("failed to replace the update callback: %v", err)
	}

	m, err := NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	t.Cleanup(m.StopAllTunnels)

	result, err := m.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
	if result.Started != 1 || result.Stopped != 0 || result.Failed != 0 {
		t.Fatalf("Reconcile reported started=%d stopped=%d failed=%d beside an assignment whose Host is gone, want 1/0/0",
			result.Started, result.Stopped, result.Failed)
	}

	running := runningKeys(m)
	if _, ok := running["1-2"]; !ok || len(running) != 1 {
		t.Fatalf("the tunnels running are %v, want the one of the assignment that is whole", running)
	}

	// A second pass leaves it alone rather than stopping it over the row it
	// could not build anything from.
	result, err = m.Reconcile()
	if err != nil {
		t.Fatalf("the second Reconcile returned an error: %v", err)
	}
	if result.Stopped != 0 || result.Failed != 0 {
		t.Fatalf("the second pass reported stopped=%d failed=%d, want 0/0", result.Stopped, result.Failed)
	}
}

// TestDesiredTunnelsReportsAssignmentsThatCannotBeRead pins down that a read of
// the assignment table that failed is an error and not an empty desired state.
// An empty one reads as nothing being wanted, and a pass would stop every
// tunnel of the installation over a database that was briefly away.
func TestDesiredTunnelsReportsAssignmentsThatCannotBeRead(t *testing.T) {
	hosts := []models.Host{enabledHost(1, true)}
	sps := []models.ServicePort{testServicePort(2)}

	db := newStubDB(t, hosts, sps, nil)

	err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		switch dest := tx.Statement.Dest.(type) {
		case *[]models.Host:
			*dest = hosts
		case *[]models.ServicePort:
			*dest = sps
		case *[]models.HostServicePort:
			_ = tx.AddError(errConnPoolClosed)
		}
	})
	if err != nil {
		t.Fatalf("failed to replace the query callback: %v", err)
	}

	m, err := NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	registerStoppedTunnel(t, m, 1, 2)

	_, err = m.Reconcile()
	if err == nil {
		t.Fatal("Reconcile returned no error when the assignments could not be read")
	}

	if len(runningKeys(m)) != 1 {
		t.Fatal("a pass that could not read the assignments stopped the tunnel that was running")
	}
}
