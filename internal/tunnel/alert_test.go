package tunnel

import (
	"errors"
	"net"
	"reflect"
	"sort"
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/alert"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
)

// TestAlertConditionsNameWhatRuns holds the conditions to what the manager is
// running: a tunnel row whose tunnel does not run is left out, and so is a
// tunnel that runs and has no row yet.
func TestAlertConditionsNameWhatRuns(t *testing.T) {
	rows := []models.Tunnel{
		{HostID: 1, SPID: 1, Status: "connected"},
		{HostID: 1, SPID: 4, Status: "error", LastError: "ssh: unable to authenticate"},
		// Stopped already, so not running: the row is what a stop that has
		// not reached the database yet leaves behind.
		{HostID: 1, SPID: 2, Status: "reconnecting", LastError: "dial tcp: i/o timeout"},
	}

	m, err := NewManager(newTunnelStubDB(t, rows, nil), zap.NewNop(), nil, 5)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	for _, spID := range []uint{1, 4, 3} {
		hostID := uint(1)
		if spID == 3 {
			hostID = 2
		}

		tunnel, err := NewSSHTunnel(&hostID, &spID, "0.0.0.0:18080", "[::]:18080", "ssh.example.com:22",
			"127.0.0.1:3306", nil, zap.NewNop())
		if err != nil {
			t.Fatalf("NewSSHTunnel: %v", err)
		}

		m.tunnels[tunnelKey(hostID, spID)] = tunnel
	}

	m.localForwards[LocalForwardKey{HostID: 3, Number: 1}] = &localTunnel{
		server: "lf.example.com:22",
		listen: localPair{v4: &net.TCPAddr{Port: 15432}, v6: &net.TCPAddr{Port: 15432}},
		state:  LocalForwardState{Status: localStatusReconnecting, LastError: "EOF"},
	}

	m.socksProxies[4] = &socksTunnel{
		server: "[2001:db8::1]:22",
		listen: localPair{v4: &net.TCPAddr{Port: 1080}, v6: &net.TCPAddr{Port: 1080}},
		state:  SocksState{Status: localStatusConnected},
	}

	got, err := m.AlertConditions()
	if err != nil {
		t.Fatalf("AlertConditions: %v", err)
	}

	sort.Slice(got, func(i, j int) bool { return got[i].Key < got[j].Key })

	want := []alert.Condition{
		{Key: "local_forward:3-1", Kind: alert.KindLocalForward, Host: "lf.example.com", LocalPort: 15432,
			LastError: "EOF"},
		{Key: "service_port:1-1", Kind: alert.KindServicePort, Host: "ssh.example.com", LocalPort: 18080,
			Connected: true},
		{Key: "service_port:1-4", Kind: alert.KindServicePort, Host: "ssh.example.com", LocalPort: 18080,
			LastError: "ssh: unable to authenticate"},
		{Key: "socks:4", Kind: alert.KindSocks, Host: "2001:db8::1", LocalPort: 1080, Connected: true},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

// TestAlertConditionsReadNoRowWhenNoTunnelRuns keeps the scan off the
// database while nothing it would read there is running.
func TestAlertConditionsReadNoRowWhenNoTunnelRuns(t *testing.T) {
	m, err := NewManager(newTunnelStubDB(t, nil, errors.New("the table must not be read")), zap.NewNop(), nil, 5)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	got, err := m.AlertConditions()
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v, want nothing and no error", got, err)
	}
}

// TestAlertConditionsReportAFailedRead passes a database that cannot be read on
// rather than taking it for every tunnel having stopped.
func TestAlertConditionsReportAFailedRead(t *testing.T) {
	m, err := NewManager(newTunnelStubDB(t, nil, errors.New("database is locked")), zap.NewNop(), nil, 5)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	hostID, spID := uint(1), uint(1)

	tunnel, err := NewSSHTunnel(&hostID, &spID, "0.0.0.0:18080", "[::]:18080", "ssh.example.com:22",
		"127.0.0.1:3306", nil, zap.NewNop())
	if err != nil {
		t.Fatalf("NewSSHTunnel: %v", err)
	}

	m.tunnels[tunnelKey(hostID, spID)] = tunnel

	_, err = m.AlertConditions()
	if err == nil {
		t.Fatal("a tunnel table that could not be read was reported as read")
	}
}
