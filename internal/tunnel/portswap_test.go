package tunnel

import (
	"net"
	"strconv"
	"sync"
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// portSwapFixture is a manager with one Host that points at a direct-tcpip
// server, and whatever local forwards and proxy the test gives it, with every
// line it logs kept so a test can tell whether a port was refused on the way.
type portSwapFixture struct {
	m        *Manager
	logs     *observer.ObservedLogs
	mu       sync.Mutex
	hosts    []models.Host
	forwards []models.LocalForward
}

func newPortSwapFixture(t *testing.T) *portSwapFixture {
	t.Helper()

	server := startDirectSSHServer(t)

	serverHost, serverPort, err := net.SplitHostPort(server.addr)
	if err != nil {
		t.Fatalf("failed to split %s: %v", server.addr, err)
	}
	port, err := strconv.Atoi(serverPort)
	if err != nil {
		t.Fatalf("failed to read the port of %s: %v", server.addr, err)
	}

	f := &portSwapFixture{}
	f.hosts = []models.Host{{
		ID: 1, Address: serverHost, Port: port, User: "tester", Password: "pass", // hook:allow
		HostKey: MarshalHostKey(server.key), Enabled: true,
	}}

	core, logs := observer.New(zapcore.DebugLevel)
	f.logs = logs

	m, err := NewManager(newLocalForwardStubDB(t, &f.mu, &f.hosts, &f.forwards), zap.New(core), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	t.Cleanup(m.StopAllTunnels)

	f.m = m

	return f
}

func (f *portSwapFixture) change(edit func(host *models.Host, forwards []models.LocalForward)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	edit(&f.hosts[0], f.forwards)
}

func (f *portSwapFixture) reconcile(t *testing.T) ReconcileResult {
	t.Helper()

	result, err := f.m.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}

	return result
}

// refusedPorts returns the lines logged since line from on a forward or a
// proxy that could not open its port.
func (f *portSwapFixture) refusedPorts(from int) []map[string]interface{} {
	var refused []map[string]interface{}
	for _, line := range f.logs.All()[from:] {
		id, _ := line.ContextMap()[logid.FieldKey].(string)
		switch logid.ID(id) {
		case logid.TunnelLocalForwardListenFailed, logid.TunnelSocksListenFailed:
			refused = append(refused, line.ContextMap())
		}
	}

	return refused
}

// twoFreePorts returns two different ports nothing listens on at the moment.
func twoFreePorts(t *testing.T) (int, int) {
	t.Helper()

	first := freeDualStackPort(t)
	second := freeDualStackPort(t)
	if first == second {
		t.Skip("the two free ports found are the same one")
	}

	return first, second
}

// TestTwoLocalForwardsSwapTheirPortsInOnePass is two forwards trading local
// ports in one pass, the way an import or two saves read by one pass change
// them. Each new port is the one the other forward holds until it is stopped,
// so a pass that opens either before the other forward has let go of it has
// the port refused, and the forward reports an error until it is retried.
func TestTwoLocalForwardsSwapTheirPortsInOnePass(t *testing.T) {
	firstIP, firstTarget := startEchoService(t, "first:")
	_, secondTarget := startEchoService(t, "second:")
	firstPort, secondPort := twoFreePorts(t)

	f := newPortSwapFixture(t)
	f.forwards = []models.LocalForward{
		{Number: 7, HostID: 1, BindScope: models.BindScopeLoopback, LocalPort: firstPort,
			TargetAddress: firstIP, TargetPort: firstTarget, Enabled: true},
		{Number: 8, HostID: 1, BindScope: models.BindScopeLoopback, LocalPort: secondPort,
			TargetAddress: firstIP, TargetPort: secondTarget, Enabled: true},
	}
	first := LocalForwardKey{HostID: 1, Number: 7}
	second := LocalForwardKey{HostID: 1, Number: 8}

	if result := f.reconcile(t); result != (ReconcileResult{Started: 2}) {
		t.Fatalf("the first pass reported %+v, want two started", result)
	}
	waitLocalStatus(t, f.m, first, "the first forward to connect", isConnected)
	waitLocalStatus(t, f.m, second, "the second forward to connect", isConnected)

	before := f.logs.Len()

	f.change(func(_ *models.Host, forwards []models.LocalForward) {
		forwards[0].LocalPort = secondPort
		forwards[1].LocalPort = firstPort
	})

	if result := f.reconcile(t); result != (ReconcileResult{Restarted: 2}) {
		t.Fatalf("swapping the ports reported %+v, want two restarted", result)
	}

	firstState := waitLocalStatus(t, f.m, first, "the first forward to connect on its new port", isConnected)
	secondState := waitLocalStatus(t, f.m, second, "the second forward to connect on its new port", isConnected)

	if refused := f.refusedPorts(before); len(refused) != 0 {
		t.Fatalf("swapping the ports had a port refused, want none: %v", refused)
	}

	if got := exchange(t, openedLoopback(t, firstState.OpenReach, secondPort), "ping"); got != "first:ping" {
		t.Fatalf("the answer through the port the first forward moved to was %q, want %q", got, "first:ping")
	}
	if got := exchange(t, openedLoopback(t, secondState.OpenReach, firstPort), "ping"); got != "second:ping" {
		t.Fatalf("the answer through the port the second forward moved to was %q, want %q", got, "second:ping")
	}
}

// TestLocalForwardsPassTheirPortsRoundInOnePass is the trade of
// TestTwoLocalForwardsSwapTheirPortsInOnePass among more forwards, each moving
// to the port of the next. Every forward that is started before the next one
// is stopped has its port held for as long as the pass takes to reach that
// one, so a pass that starts and stops in one sweep has the port refused far
// more often than with two, where the window is one step of the sweep.
func TestLocalForwardsPassTheirPortsRoundInOnePass(t *testing.T) {
	const count = 12

	targetIP, targetPort := startEchoService(t, "echo:")

	held := make([]net.Listener, 0, count)
	ports := make([]int, 0, count)
	for len(ports) < count {
		ln, err := net.Listen("tcp", ":0")
		if err != nil {
			t.Fatalf("failed to find a free port: %v", err)
		}
		held = append(held, ln)
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
	}
	for _, ln := range held {
		_ = ln.Close()
	}

	f := newPortSwapFixture(t)
	keys := make([]LocalForwardKey, 0, count)
	for i, port := range ports {
		f.forwards = append(f.forwards, models.LocalForward{Number: uint(i + 1), HostID: 1,
			BindScope: models.BindScopeLoopback, LocalPort: port,
			TargetAddress: targetIP, TargetPort: targetPort, Enabled: true})
		keys = append(keys, LocalForwardKey{HostID: 1, Number: uint(i + 1)})
	}

	if result := f.reconcile(t); result != (ReconcileResult{Started: count}) {
		t.Fatalf("the first pass reported %+v, want %d started", result, count)
	}
	for _, key := range keys {
		waitLocalStatus(t, f.m, key, "every forward to connect", isConnected)
	}

	before := f.logs.Len()

	f.change(func(_ *models.Host, forwards []models.LocalForward) {
		for i := range forwards {
			forwards[i].LocalPort = ports[(i+1)%count]
		}
	})

	if result := f.reconcile(t); result != (ReconcileResult{Restarted: count}) {
		t.Fatalf("passing the ports round reported %+v, want %d restarted", result, count)
	}

	for _, key := range keys {
		waitLocalStatus(t, f.m, key, "every forward to connect on its new port", isConnected)
	}

	if refused := f.refusedPorts(before); len(refused) != 0 {
		t.Fatalf("passing the ports round had a port refused %d times, want none: %v", len(refused), refused[0])
	}
}

// TestALocalForwardAndAProxySwapTheirPortsInOnePass is the same trade between
// a local forward and the SOCKS5 proxy of its Host, which a pass reconciles
// one after the other.
func TestALocalForwardAndAProxySwapTheirPortsInOnePass(t *testing.T) {
	targetIP, targetPort := startEchoService(t, "echo:")
	forwardPort, socksPort := twoFreePorts(t)

	f := newPortSwapFixture(t)
	f.change(func(host *models.Host, _ []models.LocalForward) {
		host.SocksEnabled = true
		host.SocksPort = socksPort
		host.SocksBindScope = models.BindScopeLoopback
	})
	f.forwards = []models.LocalForward{
		{Number: 7, HostID: 1, BindScope: models.BindScopeLoopback, LocalPort: forwardPort,
			TargetAddress: targetIP, TargetPort: targetPort, Enabled: true},
	}
	forward := LocalForwardKey{HostID: 1, Number: 7}

	if result := f.reconcile(t); result != (ReconcileResult{Started: 2}) {
		t.Fatalf("the first pass reported %+v, want two started", result)
	}
	waitLocalStatus(t, f.m, forward, "the forward to connect", isConnected)
	waitSocksConnected(t, f.m, 1)

	before := f.logs.Len()

	f.change(func(host *models.Host, forwards []models.LocalForward) {
		forwards[0].LocalPort = socksPort
		host.SocksPort = forwardPort
	})

	if result := f.reconcile(t); result != (ReconcileResult{Restarted: 2}) {
		t.Fatalf("swapping the ports reported %+v, want two restarted", result)
	}

	forwardState := waitLocalStatus(t, f.m, forward, "the forward to connect on its new port", isConnected)
	waitSocksConnected(t, f.m, 1)

	if refused := f.refusedPorts(before); len(refused) != 0 {
		t.Fatalf("swapping the ports had a port refused, want none: %v", refused)
	}

	if got := exchange(t, openedLoopback(t, forwardState.OpenReach, socksPort), "ping"); got != "echo:ping" {
		t.Fatalf("the answer through the port the forward moved to was %q, want %q", got, "echo:ping")
	}
}
