//go:build windows

package tunnel

import (
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/models"
)

// portHolders are the ways another program holds a port that the wildcard
// scope has to see as taken. Only the one on [::] for both families is refused
// on every system; the other two are the ones Windows let a socket of the pair
// bind over.
var portHolders = []struct {
	name    string
	network string
	address string
}{
	{"dual-stack [::]", "tcp", "[::]:0"},
	{"0.0.0.0", "tcp4", "0.0.0.0:0"},
	{"127.0.0.1", "tcp4", "127.0.0.1:0"},
}

// holdPort takes a port the way another program would, with no option of its
// own, and returns it.
func holdPort(t *testing.T, network, address string) int {
	t.Helper()

	held, err := net.Listen(network, address)
	if err != nil {
		t.Fatalf("failed to take a port on %s: %v", address, err)
	}
	t.Cleanup(func() {
		_ = held.Close()
	})

	return held.Addr().(*net.TCPAddr).Port
}

// TestTheWildcardScopeOpensOneDualStackSocket is the wildcard scope on a free
// port: one socket, reaching both families.
func TestTheWildcardScopeOpensOneDualStackSocket(t *testing.T) {
	port := freeDualStackPort(t)
	pair := localPair{
		v4: &net.TCPAddr{IP: net.IPv4zero, Port: port},
		v6: &net.TCPAddr{IP: net.IPv6unspecified, Port: port},
	}

	opened, err := openLocalListeners(pair)
	if err != nil {
		t.Fatalf("failed to open the wildcard scope on a free port: %v", err)
	}
	defer opened.close()

	if opened.reach != openReachBoth || len(opened.listeners()) != 1 || opened.refused != nil {
		t.Fatalf("the wildcard scope opened %d listeners reaching %q (refused %v), want one reaching %q",
			len(opened.listeners()), opened.reach, opened.refused, openReachBoth)
	}

	for _, address := range []string{"127.0.0.1", "::1"} {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(address, strconv.Itoa(port)), localTestTimeout)
		if err != nil {
			t.Fatalf("the dual-stack socket did not answer on %s: %v", address, err)
		}
		_ = conn.Close()
	}
}

// TestALocalPortHeldAnyWayLeavesTheForwardInError is
// TestALocalPortInUseLeavesTheForwardInError for each of the ways another
// program holds the port.
func TestALocalPortHeldAnyWayLeavesTheForwardInError(t *testing.T) {
	for _, holder := range portHolders {
		t.Run(holder.name, func(t *testing.T) {
			targetIP, targetPort := startEchoService(t, "echo:")
			localPort := holdPort(t, holder.network, holder.address)

			f := newLocalForwardFixture(t, models.BindScopeWildcard, localPort, targetIP, targetPort)
			f.reconcile(t)

			state := waitLocalStatus(t, f.m, f.key(), "the local forward to report the port in use", func(state LocalForwardState) bool {
				return state.Status == localStatusError
			})

			if !strings.Contains(state.LastError, ":"+strconv.Itoa(localPort)) {
				t.Fatalf("the forward reports the error %q, want the port in use named", state.LastError)
			}
		})
	}
}

// TestASocksPortHeldAnyWayLeavesTheProxyInError is the same for the SOCKS5
// proxy of a Host.
func TestASocksPortHeldAnyWayLeavesTheProxyInError(t *testing.T) {
	for _, holder := range portHolders {
		t.Run(holder.name, func(t *testing.T) {
			port := holdPort(t, holder.network, holder.address)

			f := newSocksFixture(t, port, "")
			f.change(func(host *models.Host) {
				host.SocksBindScope = models.BindScopeWildcard
			})
			f.reconcile(t)

			var last SocksState
			waitFor(t, localTestTimeout, "the SOCKS5 proxy to report the port in use", func() bool {
				state, ok := f.m.SocksStatus(3)
				last = state
				return ok && state.Status == localStatusError
			})

			if !strings.Contains(last.LastError, ":"+strconv.Itoa(port)) {
				t.Fatalf("the proxy reports the error %q, want the port in use named", last.LastError)
			}
		})
	}
}
