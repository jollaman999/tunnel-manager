package tunnel

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/models"
)

// freeLocalPort is a port on this machine nothing listens on, for a forward the
// SSH test server confirms and nothing answers on. The probe of that forward
// gets a refusal at once, so a test does not wait out a timeout to see what the
// silence was written down as.
func freeLocalPort(t *testing.T) int {
	t.Helper()

	_, port, err := net.SplitHostPort(closedPort(t))
	if err != nil {
		t.Fatalf("failed to read the port that nothing listens on: %v", err)
	}

	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("failed to read %q as a port: %v", port, err)
	}

	return number
}

// waitProbeReading waits for the probe to write a reading other than unknown
// and returns it, and returns the empty string where the whole wait passed with
// the row still unknown.
//
// A reading of unknown is what the connection wrote before the probe ran and is
// also what a probe that measured nothing leaves, so the two cannot be told
// apart by the value. What tells them apart is time: the probe is refused by
// the loopback of this machine in under a millisecond, so a row still unknown
// after this wait is one the probe left that way.
func waitProbeReading(tun *SSHTunnel, tunnel *models.Tunnel, wait time.Duration) string {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if _, reach := readTunnelReach(tun, tunnel); reach != forwardReachUnknown {
			return reach
		}
		time.Sleep(10 * time.Millisecond)
	}

	return ""
}

// TestAForwardNothingHereCanDialIsLeftUnknown holds the probe to what it
// measured.
//
// There is one address of the Host here, so the probe carries one address
// family and cannot try the other at all. A silence from a family whose request
// was turned down is not a port that cannot be reached, and a row that says it
// is puts a failure on a tunnel that is doing what was asked of it. The loopback
// scope of a Host that is not this machine is the other such case, and it is
// held in TestTheProbeDialsLoopbackOnlyOnThisMachine, since the Host of every
// test server here is this machine.
func TestAForwardNothingHereCanDialIsLeftUnknown(t *testing.T) {
	cases := []struct {
		name   string
		scope  string
		refuse string
	}{
		{"the request for the family the Host is dialled over was turned down",
			models.BindScopeWildcard, "0.0.0.0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newSSHTestManager(t, 1)
			serverAddr, _ := startScopedForwardSSHServer(t, func(addr string) bool {
				return addr != tc.refuse
			})

			// The SSH test server confirms the port it was asked for, and the
			// Host of this tunnel is the loopback of this machine, so the
			// probe goes to a port here that nothing listens on.
			port := freeLocalPort(t)
			localV4 := net.JoinHostPort(loopbackBindAddressV4, strconv.Itoa(port))
			localV6 := net.JoinHostPort(loopbackBindAddressV6, strconv.Itoa(port))
			if tc.scope != models.BindScopeLoopback {
				localV4 = net.JoinHostPort(wildcardBindAddressV4, strconv.Itoa(port))
				localV6 = net.JoinHostPort(wildcardBindAddressV6, strconv.Itoa(port))
			}

			tun, tunnel := newScopedSSHTestTunnel(t, serverAddr, localV4, localV6)

			errc := make(chan error, 1)
			go func() {
				errc <- tun.establishConnection(m, tunnel)
			}()

			waitTunnelClient(t, tun, 10*time.Second)
			reading := waitProbeReading(tun, tunnel, 3*time.Second)

			closeTunnelClient(t, tun)

			select {
			case <-errc:
			case <-time.After(5 * time.Second):
				t.Fatal("establishConnection did not return after the connection was closed")
			}

			if reading != "" {
				t.Fatalf("forward reach = %q, want %q: the probe could not reach the addresses "+
					"this forward was asked for, so it measured nothing", reading, forwardReachUnknown)
			}
		})
	}
}

// TestASilentForwardTheProbeCouldDialReadsUnreachable is the other side of it,
// and is what keeps the reading above from being the answer to everything. The
// ports were asked for on the wildcard, the server answered yes to the family
// the Host is dialled over, and nothing came back: that is a measurement, and
// it is the one an operator has to be shown, because the row says connected
// either way.
func TestASilentForwardTheProbeCouldDialReadsUnreachable(t *testing.T) {
	m := newSSHTestManager(t, 1)
	serverAddr, _ := startScopedForwardSSHServer(t, func(string) bool { return true })

	port := freeLocalPort(t)
	tun, tunnel := newScopedSSHTestTunnel(t,
		serverAddr,
		net.JoinHostPort(wildcardBindAddressV4, strconv.Itoa(port)),
		net.JoinHostPort(wildcardBindAddressV6, strconv.Itoa(port)))

	errc := make(chan error, 1)
	go func() {
		errc <- tun.establishConnection(m, tunnel)
	}()

	waitTunnelClient(t, tun, 10*time.Second)
	reading := waitTunnelReach(t, tun, tunnel, 30*time.Second)

	closeTunnelClient(t, tun)

	select {
	case <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("establishConnection did not return after the connection was closed")
	}

	if reading != forwardUnreachable {
		t.Fatalf("forward reach = %q, want %q, nothing answered at an address the probe could dial",
			reading, forwardUnreachable)
	}
}

// TestALoopbackForwardOnThisMachineIsMeasured is the Host that is this machine:
// the ports were asked for on its loopback addresses, and those are this
// machine's too, so the probe dials them and what comes back is a reading. The
// test server confirms the port and opens nothing, so nothing answers until the
// test listens there itself.
func TestALoopbackForwardOnThisMachineIsMeasured(t *testing.T) {
	cases := []struct {
		name   string
		listen bool
		want   string
	}{
		{"nothing listens on the port", false, forwardUnreachable},
		{"something listens on the port", true, forwardReachable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newSSHTestManager(t, 1)
			serverAddr, _ := startScopedForwardSSHServer(t, func(string) bool { return true })

			port := freeLocalPort(t)
			if tc.listen {
				listener, err := net.Listen("tcp4", net.JoinHostPort(loopbackBindAddressV4, strconv.Itoa(port)))
				if err != nil {
					t.Fatalf("failed to listen on the port of the forward: %v", err)
				}
				t.Cleanup(func() { _ = listener.Close() })
			}

			tun, tunnel := newScopedSSHTestTunnel(t, serverAddr,
				net.JoinHostPort(loopbackBindAddressV4, strconv.Itoa(port)),
				net.JoinHostPort(loopbackBindAddressV6, strconv.Itoa(port)))

			errc := make(chan error, 1)
			go func() {
				errc <- tun.establishConnection(m, tunnel)
			}()

			waitTunnelClient(t, tun, 10*time.Second)
			reading := waitTunnelReach(t, tun, tunnel, 30*time.Second)

			closeTunnelClient(t, tun)

			select {
			case <-errc:
			case <-time.After(5 * time.Second):
				t.Fatal("establishConnection did not return after the connection was closed")
			}

			if reading != tc.want {
				t.Fatalf("forward reach = %q, want %q", reading, tc.want)
			}
		})
	}
}

// TestTheProbeDialsLoopbackOnlyOnThisMachine holds where the probe goes. A port
// asked for on loopback is dialled at the loopback address of the family that
// went up where the Host is this machine, and at the Host with a silence worth
// nothing where it is another. The wildcard is dialled at the Host either way.
func TestTheProbeDialsLoopbackOnlyOnThisMachine(t *testing.T) {
	held := interfaceAddrs
	t.Cleanup(func() { interfaceAddrs = held })

	interfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("198.51.100.100"), Mask: net.CIDRMask(32, 32)},
			&net.IPNet{IP: net.ParseIP("2001:db8::64"), Mask: net.CIDRMask(128, 128)},
		}, nil
	}

	tests := []struct {
		name    string
		localV4 string
		localV6 string
		server  string
		reach   string
		want    forwardProbe
	}{
		{"loopback, the Host is an address of this machine", "127.0.0.1:8086", "[::1]:8086",
			"198.51.100.100:22", openReachBoth, forwardProbe{"127.0.0.1:8086", forwardUnreachable, true}},
		{"loopback, the Host is the loopback of this machine", "127.0.0.1:8086", "[::1]:8086",
			"127.0.0.1:22", openReachBoth, forwardProbe{"127.0.0.1:8086", forwardUnreachable, true}},
		{"loopback, only IPv6 went up on this machine", "127.0.0.1:8086", "[::1]:8086",
			"[2001:db8::64]:22", openReachV6, forwardProbe{"[::1]:8086", forwardUnreachable, true}},
		{"loopback, the Host is another machine", "127.0.0.1:8086", "[::1]:8086",
			"192.0.2.10:22", openReachBoth, forwardProbe{"192.0.2.10:8086", forwardReachUnknown, false}},
		{"wildcard, the Host is this machine", "0.0.0.0:8086", "[::]:8086",
			"198.51.100.100:22", openReachBoth, forwardProbe{"198.51.100.100:8086", forwardUnreachable, false}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			local := localPair{v4: resolveTestTCPAddr(t, tt.localV4), v6: resolveTestTCPAddr(t, tt.localV6)}

			got := newForwardProbe(local, resolveTestTCPAddr(t, tt.server), tt.reach, 8086)
			if got != tt.want {
				t.Fatalf("newForwardProbe = %+v, want %+v", got, tt.want)
			}
		})
	}
}
