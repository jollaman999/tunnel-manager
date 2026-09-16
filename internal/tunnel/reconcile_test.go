package tunnel

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
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

	result, err := m.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
	if result.Started != 0 || result.Stopped != 0 || result.Failed != 0 {
		t.Fatalf("Reconcile reported started=%d stopped=%d failed=%d for a combination that is on both sides, want 0/0/0",
			result.Started, result.Stopped, result.Failed)
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
