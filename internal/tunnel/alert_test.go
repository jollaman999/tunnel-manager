package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
	"time"

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
		server: "[2001:db8::1]:2222",
		listen: localPair{v4: &net.TCPAddr{Port: 1080}, v6: &net.TCPAddr{Port: 1080}},
		state:  SocksState{Status: localStatusConnected},
	}

	got, err := m.AlertConditions()
	if err != nil {
		t.Fatalf("AlertConditions: %v", err)
	}

	sort.Slice(got, func(i, j int) bool { return got[i].Key < got[j].Key })

	want := []alert.Condition{
		{Key: "local_forward:3-1@lf.example.com:22/15432", Kind: alert.KindLocalForward, Host: "lf.example.com",
			HostID: 3, HostSSHPort: 22, LocalPort: 15432, LastError: "EOF"},
		{Key: "service_port:1-1@ssh.example.com:22/18080", Kind: alert.KindServicePort, Host: "ssh.example.com",
			HostID: 1, HostSSHPort: 22, LocalPort: 18080, Connected: true},
		{Key: "service_port:1-4@ssh.example.com:22/18080", Kind: alert.KindServicePort, Host: "ssh.example.com",
			HostID: 1, HostSSHPort: 22, LocalPort: 18080, LastError: "ssh: unable to authenticate"},
		{Key: "socks:4@[2001:db8::1]:2222/1080", Kind: alert.KindSocks, Host: "2001:db8::1", HostID: 4,
			HostSSHPort: 2222, LocalPort: 1080, Connected: true},
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

// TestAnAlertKeyChangesWithTheHostItGoesThrough gives each kind of forward
// the same id on two different Hosts, as a Host whose address or port was
// changed, or one removed and its id given to another, leaves them. The key
// has to change with it, or the watcher answers the down of the Host that was
// there with an up that names the Host that is there now.
func TestAnAlertKeyChangesWithTheHostItGoesThrough(t *testing.T) {
	keysOn := func(server string) map[string]string {
		t.Helper()

		m, err := NewManager(newTunnelStubDB(t, []models.Tunnel{{HostID: 1, SPID: 1, Status: "error"}}, nil),
			zap.NewNop(), nil, 5)
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}

		hostID, spID := uint(1), uint(1)

		tunnel, err := NewSSHTunnel(&hostID, &spID, "0.0.0.0:18080", "[::]:18080", server,
			"127.0.0.1:3306", nil, zap.NewNop())
		if err != nil {
			t.Fatalf("NewSSHTunnel: %v", err)
		}
		m.tunnels[tunnelKey(hostID, spID)] = tunnel

		m.localForwards[LocalForwardKey{HostID: 1, Number: 1}] = &localTunnel{
			server: server,
			listen: localPair{v4: &net.TCPAddr{Port: 15432}, v6: &net.TCPAddr{Port: 15432}},
			state:  LocalForwardState{Status: localStatusError},
		}
		m.socksProxies[1] = &socksTunnel{
			server: server,
			listen: localPair{v4: &net.TCPAddr{Port: 1080}, v6: &net.TCPAddr{Port: 1080}},
			state:  SocksState{Status: localStatusError},
		}

		conditions, err := m.AlertConditions()
		if err != nil {
			t.Fatalf("AlertConditions: %v", err)
		}

		keys := make(map[string]string, len(conditions))
		for _, condition := range conditions {
			keys[condition.Kind] = condition.Key
		}

		return keys
	}

	before := keysOn("old.example.com:22")
	again := keysOn("old.example.com:22")
	moved := keysOn("new.example.com:22")
	portMoved := keysOn("old.example.com:2222")

	for _, kind := range []string{alert.KindServicePort, alert.KindLocalForward, alert.KindSocks} {
		if before[kind] == "" {
			t.Fatalf("no %s condition was reported", kind)
		}
		if again[kind] != before[kind] {
			t.Errorf("the %s key of the same Host changed: %q, then %q", kind, before[kind], again[kind])
		}
		if moved[kind] == before[kind] {
			t.Errorf("the %s key %q stayed the same when the Host address changed", kind, before[kind])
		}
		if portMoved[kind] == before[kind] {
			t.Errorf("the %s key %q stayed the same when the Host port changed", kind, before[kind])
		}
	}
}

// TestAnUpNamesWhichOfTwoHostsOnOneAddressCameBack runs two Hosts that share
// an address and differ only in their SSH port, as two systems behind one NAT
// do, both carrying the same service port. Both go down and one comes back.
// The up that is posted has to say which one, or whoever reads it closes the
// outage of the Host that is still down. The body is read as plain JSON, so
// that what is checked is what a receiver gets.
func TestAnUpNamesWhichOfTwoHostsOnOneAddressCameBack(t *testing.T) {
	rows := []models.Tunnel{
		{HostID: 1, SPID: 1, Status: "error", LastError: "dial tcp: connection refused"},
		{HostID: 2, SPID: 1, Status: "error", LastError: "dial tcp: connection refused"},
	}

	m, err := NewManager(newTunnelStubDB(t, rows, nil), zap.NewNop(), nil, 5)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	servers := map[uint]string{1: "192.0.2.10:22", 2: "192.0.2.10:2222"}
	for hostID, server := range servers {
		hostID, spID := hostID, uint(1)

		tunnel, err := NewSSHTunnel(&hostID, &spID, "0.0.0.0:18080", "[::]:18080", server,
			"127.0.0.1:3306", nil, zap.NewNop())
		if err != nil {
			t.Fatalf("NewSSHTunnel: %v", err)
		}

		m.tunnels[tunnelKey(hostID, spID)] = tunnel
	}

	bodies := make(chan map[string]interface{}, 8)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)

		var body map[string]interface{}
		_ = json.Unmarshal(raw, &body)

		bodies <- body

		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	config := alert.Config{After: time.Minute, WebhookURL: server.URL}
	watcher := alert.NewWatcher(zap.NewNop(), m.AlertConditions, func() (alert.Config, error) {
		return config, nil
	}, alert.NewSender(zap.NewNop(), nil), "tm.example.com")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The interval is never reached, so the scans below are the only ones and
	// Run is there to send what they queue.
	go watcher.Run(ctx, time.Hour)

	start := time.Date(2026, 9, 26, 1, 0, 0, 0, time.UTC)
	watcher.Scan(start)
	watcher.Scan(start.Add(2 * time.Minute))

	rows[1].Status = "connected"
	rows[1].LastError = ""
	watcher.Scan(start.Add(3 * time.Minute))

	var ups []map[string]interface{}

	for i := 0; i < 3; i++ {
		select {
		case body := <-bodies:
			if body["event"] == alert.EventUp {
				ups = append(ups, body)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of the 3 alerts reached the webhook", i)
		}
	}

	if len(ups) != 1 {
		t.Fatalf("%d ups were posted, want 1: %v", len(ups), ups)
	}

	up := ups[0]

	if up["host"] != "192.0.2.10" || up["local_port"] != float64(18080) {
		t.Errorf("the up names %v port %v, want 192.0.2.10 port 18080", up["host"], up["local_port"])
	}

	if up["host_id"] != float64(2) || up["host_ssh_port"] != float64(2222) {
		t.Fatalf("the up carries host_id %v and host_ssh_port %v, want 2 and 2222, which is the Host that "+
			"came back: %v", up["host_id"], up["host_ssh_port"], up)
	}
}
