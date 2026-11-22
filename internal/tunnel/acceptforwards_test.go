package tunnel

import (
	"errors"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// errAcceptReset is what failingListener ends with. It stands for the error of
// writing the confirmation of a forwarded channel on a connection the far side
// reset, which is not io.EOF.
var errAcceptReset = errors.New("write tcp: connection reset by peer")

// failingListener is a forward whose Accept fails with err at once.
type failingListener struct {
	err error
}

func (l failingListener) Accept() (net.Conn, error) { return nil, l.err }
func (l failingListener) Close() error              { return nil }
func (l failingListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 18081}
}

// clientClosed reports whether the client was closed, which ends its Wait.
func clientClosed(client *ssh.Client) bool {
	waited := make(chan struct{})
	go func() {
		_ = client.Wait()
		close(waited)
	}()

	select {
	case <-waited:
		return true
	case <-time.After(time.Second):
		return false
	}
}

// TestAcceptForwardsDropsTheClientOnAnAcceptErrorOtherThanEOF covers a forward
// whose accept ends with an error other than io.EOF, which a connection reset
// by the far side gives. The connection is gone just as for io.EOF, so the
// client is dropped and the tunnel reported as reconnecting for the wait
// before the next attempt, rather than left connected on a dead client the
// next connection only replaces once it is made.
func TestAcceptForwardsDropsTheClientOnAnAcceptErrorOtherThanEOF(t *testing.T) {
	m := newSSHTestManager(t, 60)
	tun, row := newSSHTestTunnel(t, "127.0.0.1:1")
	row.Status = "connected"

	client, cleanup := newLoopbackSSHClient(t)
	defer cleanup()

	tun.clientMu.Lock()
	tun.client = client
	tun.clientMu.Unlock()

	err := tun.acceptForwards(m, row, client, &openForwards{
		v4:    failingListener{err: errAcceptReset},
		reach: openReachV4,
	})

	if !errors.Is(err, errAcceptReset) {
		t.Fatalf("acceptForwards returned %v, want the accept error", err)
	}
	if errors.Is(err, errConnectionClosed) {
		t.Fatalf("acceptForwards returned %v, which the Start loop takes for the far side closing", err)
	}

	tun.clientMu.RLock()
	kept := tun.client
	tun.clientMu.RUnlock()
	if kept != nil {
		t.Error("the client of a connection whose forward failed is still the tunnel's")
	}

	tun.tunnelMu.Lock()
	status, retries := row.Status, row.RetryCount
	tun.tunnelMu.Unlock()
	if status != "reconnecting" {
		t.Errorf("Status = %q, want %q", status, "reconnecting")
	}
	if retries != 1 {
		t.Errorf("RetryCount = %d, want 1", retries)
	}

	if !clientClosed(client) {
		t.Fatal("the client of a connection whose forward failed was not closed")
	}
}

// TestAcceptForwardsLeavesAReplacedClientOnAnAcceptError covers the same error
// on a connection that is no longer the tunnel's. reconnect dropped it and
// reported the tunnel as reconnecting already, so nothing is reported again.
func TestAcceptForwardsLeavesAReplacedClientOnAnAcceptError(t *testing.T) {
	m := newSSHTestManager(t, 60)
	tun, row := newSSHTestTunnel(t, "127.0.0.1:1")
	row.Status = "reconnecting"
	row.RetryCount = 1

	client, cleanup := newLoopbackSSHClient(t)
	defer cleanup()

	err := tun.acceptForwards(m, row, client, &openForwards{
		v4:    failingListener{err: errAcceptReset},
		reach: openReachV4,
	})
	if !errors.Is(err, errAcceptReset) {
		t.Fatalf("acceptForwards returned %v, want the accept error", err)
	}

	tun.tunnelMu.Lock()
	status, retries := row.Status, row.RetryCount
	tun.tunnelMu.Unlock()
	if status != "reconnecting" || retries != 1 {
		t.Fatalf("Status = %q, RetryCount = %d, want %q and 1: one drop was reported twice",
			status, retries, "reconnecting")
	}
}
