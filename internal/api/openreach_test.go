package api

import (
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
)

// TestALocalForwardRowCarriesTheHalfItOpened holds the status answer to what
// a running local forward reports about the two addresses of its scope: the
// row of a forward that opened one of them says which and names that one as
// its address, the way a tunnel row does, and a row with no forward running
// says nothing.
func TestALocalForwardRowCarriesTheHalfItOpened(t *testing.T) {
	hosts := []models.Host{statusHost(1, true)}

	states := map[tunnel.LocalForwardKey]tunnel.LocalForwardState{
		{HostID: 1, Number: 1}: {Status: "connected", OpenReach: "ipv6"},
	}

	db := newRowsDB(t, hosts, nil, nil)
	storeLocalForwards(t, db, []models.LocalForward{
		statusLocalForward(1, 1, 19000, models.BindScopeLoopback, true),
		statusLocalForward(2, 1, 19001, models.BindScopeLoopback, true),
	})

	rows, _ := statusRowsWithStates(t, db, "/api/status", states)
	if len(rows) != 2 {
		t.Fatalf("the answer carries %d rows, want 2", len(rows))
	}

	want := map[float64]struct{ reach, local string }{
		// The row names the half that opened, which is the address that
		// answers, the way a tunnel row does.
		1: {"ipv6", "[::1]:19000"},
		2: {"", "127.0.0.1:19001"},
	}
	for _, row := range rows {
		number, _ := row["number"].(float64)

		if got := row["open_reach"]; got != want[number].reach {
			t.Fatalf("local forward %v carries open_reach %v, want %q", number, got, want[number].reach)
		}
		if got := row["local"]; got != want[number].local {
			t.Fatalf("local forward %v carries local %v, want %q", number, got, want[number].local)
		}
	}
}

// TestTheLocalForwardViewCarriesTheHalfItOpened is the same for the local
// forwards of a Host, which are read from the same view.
func TestTheLocalForwardViewCarriesTheHalfItOpened(t *testing.T) {
	lf := statusLocalForward(1, 1, 19000, models.BindScopeLoopback, true)
	states := map[tunnel.LocalForwardKey]tunnel.LocalForwardState{
		{HostID: 1, Number: 1}: {Status: "connected", OpenReach: "ipv4"},
	}

	if got := localForwardViewOf(lf, true, states).OpenReach; got != "ipv4" {
		t.Fatalf("a running forward that opened ipv4 alone is viewed with open_reach %q", got)
	}

	if got := localForwardViewOf(lf, false, states).OpenReach; got != "" {
		t.Fatalf("a forward of a disabled Host is viewed with open_reach %q, want it empty", got)
	}
}

// TestTheHostViewCarriesTheHalfTheProxyOpened is the same for the SOCKS5
// proxy of a Host.
func TestTheHostViewCarriesTheHalfTheProxyOpened(t *testing.T) {
	host := statusHost(1, true)
	host.SocksEnabled = true
	host.SocksPort = 1080

	socks := map[uint]tunnel.SocksState{1: {Status: "connected", OpenReach: "ipv6"}}

	if got := hostViewOf(host, nil, socks).SocksOpenReach; got != "ipv6" {
		t.Fatalf("a running proxy that opened ipv6 alone is viewed with socks_open_reach %q", got)
	}

	if got := hostViewOf(host, nil, nil).SocksOpenReach; got != "" {
		t.Fatalf("a proxy that does not run is viewed with socks_open_reach %q, want it empty", got)
	}
}
