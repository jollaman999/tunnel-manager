package tunnel

import (
	"bytes"
	"runtime"
	"testing"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
)

// goroutineWaitsOnALock reports whether a goroutine sits in in, past during,
// and waiting on a sync.Mutex. It is how a test sees a reading that has been
// taken and is waiting for the lock it is written under, without the code
// under test having a place to stop at.
func goroutineWaitsOnALock(in, during string) bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]

			break
		}
		buf = make([]byte, 2*len(buf))
	}

	for _, stack := range bytes.Split(buf, []byte("\n\n")) {
		if bytes.Contains(stack, []byte(in)) && !bytes.Contains(stack, []byte(during)) &&
			bytes.Contains(stack, []byte("Mutex).Lock")) {
			return true
		}
	}

	return false
}

// TestRecordListenAddressesDropsAReadingTheNextConnectionOvertook is the
// connection being replaced after the ask came back and before its answer is
// written. The replacement empties the addresses when it writes its row, and
// the answer about the connection that is gone must not land on top of that.
//
// The row lock is held here while the ask comes back, which is where the ask
// waits to write, and the replacement is made current and writes its row
// before the lock is let go.
func TestRecordListenAddressesDropsAReadingTheNextConnectionOvertook(t *testing.T) {
	m := newSSHTestManager(t, 1)
	serverAddr, _ := startAskingSSHServer(t, 19601, map[string]sessionAnswer{
		"ss -ltnH sport = :19601": {output: ssOutput},
	})
	tun, tunnel := newSSHTestTunnel(t, serverAddr)

	measured, _, cleanup := dialTestSSHClient(t, serverAddr)
	defer cleanup()

	replacement, _, replacementCleanup := dialTestSSHClient(t, serverAddr)
	defer replacementCleanup()

	tun.clientMu.Lock()
	tun.client = measured
	tun.clientMu.Unlock()

	tun.tunnelMu.Lock()

	recorded := make(chan struct{})
	go func() {
		defer close(recorded)

		tun.recordListenAddresses(m, tunnel, measured, 19601)
	}()

	waitFor(t, localTestTimeout, "the ask to come back and wait for the row", func() bool {
		return goroutineWaitsOnALock("recordListenAddresses", "askListenAddresses")
	})

	tun.clientMu.Lock()
	tun.client = replacement
	tun.clientMu.Unlock()

	tunnel.ListenAddresses = ""
	tun.tunnelMu.Unlock()

	<-recorded

	if got := readTunnelListenAddresses(tun, tunnel); got != "" {
		t.Fatalf("listen addresses = %q, want empty: the answer about a connection that was replaced "+
			"was written over the row of the one that replaced it", got)
	}
}

// TestRecordForwardReachDropsAReadingTheNextConnectionOvertook is the same for
// the probe of a local forward: the replacement sets the reading back to
// unknown, and the reading of the connection that is gone must not land on top
// of it.
func TestRecordForwardReachDropsAReadingTheNextConnectionOvertook(t *testing.T) {
	// This server refuses every channel but a session, so the probe is
	// refused at once and comes back unreachable.
	serverAddr, _ := startAskingSSHServer(t, 0, nil)

	f, err := newLocalTunnel(&models.LocalForward{
		Number: 1, HostID: 1, BindScope: models.BindScopeLoopback, LocalPort: 1,
		TargetAddress: "127.0.0.1", TargetPort: 1,
	}, &models.Host{ID: 1, Address: "127.0.0.1", Port: 22}, nil, time.Hour, zap.NewNop())
	if err != nil {
		t.Fatalf("failed to build the forward: %v", err)
	}

	measured, _, cleanup := dialTestSSHClient(t, serverAddr)
	defer cleanup()

	replacement, _, replacementCleanup := dialTestSSHClient(t, serverAddr)
	defer replacementCleanup()

	f.clientMu.Lock()
	f.client = measured
	f.clientMu.Unlock()

	f.stateMu.Lock()

	recorded := make(chan struct{})
	go func() {
		defer close(recorded)

		f.recordForwardReach(measured)
	}()

	waitFor(t, localTestTimeout, "the probe to come back and wait for the state", func() bool {
		return goroutineWaitsOnALock("recordForwardReach", "probeLocalForwardReach")
	})

	f.clientMu.Lock()
	f.client = replacement
	f.clientMu.Unlock()

	f.state.ForwardReach = forwardReachUnknown
	f.stateMu.Unlock()

	<-recorded

	if got := f.snapshot().ForwardReach; got != forwardReachUnknown {
		t.Fatalf("forward_reach = %q, want %q: the reading of a connection that was replaced was "+
			"written over the state of the one that replaced it", got, forwardReachUnknown)
	}
}
