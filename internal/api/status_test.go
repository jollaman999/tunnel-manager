package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// statusLocalForward is a local forward that no other one collides with: the
// local port is unique over the table.
func statusLocalForward(id, hostID uint, localPort int, scope string, enabled bool) models.LocalForward {
	return models.LocalForward{
		Number:        id,
		HostID:        hostID,
		BindScope:     scope,
		LocalPort:     localPort,
		TargetAddress: "198.51.100.20",
		TargetPort:    3306,
		Enabled:       enabled,
	}
}

// storeLocalForwards stores the rows newRowsDB knows nothing about. It takes
// the same database, so a test builds the Hosts, the service ports and the
// tunnels the way every other status test does and adds the forwards to it.
func storeLocalForwards(t *testing.T, db *gorm.DB, rows []models.LocalForward) {
	t.Helper()

	for i := range rows {
		err := db.Create(&rows[i]).Error
		if err != nil {
			t.Fatalf("failed to store a local forward: %v", err)
		}
	}
}

// runningLocalForwards is a manager that reports the local forwards a test has
// running. Everything else it answers is the real manager over the rows: the
// counts of what should be running are read from the tables, and only what the
// running forwards say is laid over, which is the one thing about a local
// forward that lives nowhere but in memory.
type runningLocalForwards struct {
	*tunnel.Manager
	states map[tunnel.LocalForwardKey]tunnel.LocalForwardState
}

func (m runningLocalForwards) LocalForwardStatuses() map[tunnel.LocalForwardKey]tunnel.LocalForwardState {
	return m.states
}

// statusRowsOf runs GetStatus over db and hands back the rows of the page as
// they were written, so that a field which is null in the answer can be told
// from one that carries a zero. Nothing runs over db, which is a manager whose
// local forwards all report nothing.
func statusRowsOf(t *testing.T, db *gorm.DB, target string) ([]map[string]interface{}, map[string]interface{}) {
	t.Helper()

	return statusRowsWithStates(t, db, target, nil)
}

// statusRowsWithStates is statusRowsOf with the local forwards states says are
// running.
func statusRowsWithStates(t *testing.T, db *gorm.DB, target string,
	states map[tunnel.LocalForwardKey]tunnel.LocalForwardState) ([]map[string]interface{}, map[string]interface{}) {
	t.Helper()

	base, err := tunnel.NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	manager := runningLocalForwards{Manager: base, states: states}

	c, rec := getRequest(t, target, "", "")
	h := NewHandler(db, manager, zap.NewNop(), newTestCipher(t))

	err = h.GetStatus(c)
	if err != nil {
		t.Fatalf("GetStatus returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var answer struct {
		Success bool `json:"success"`
		Data    struct {
			Tunnels []map[string]interface{} `json:"tunnels"`
		} `json:"data"`
	}

	err = json.Unmarshal(rec.Body.Bytes(), &answer)
	if err != nil {
		t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
	}
	if !answer.Success {
		t.Fatalf("success = false, body: %s", rec.Body.String())
	}

	_, data := decodeResponse(t, rec)

	return answer.Data.Tunnels, data
}

// statusCount is one of the counts off the status answer. It is answerNumber
// without the recorder, which the caller of these tests no longer holds.
func statusCount(t *testing.T, data map[string]interface{}, key string) int {
	t.Helper()

	value, ok := data[key].(float64)
	if !ok {
		t.Fatalf("%s is not a number in the answer: %v", key, data)
	}

	return int(value)
}

// rowName is a row of the status table as these tests name it: the sort, the
// Host, and what identifies it within the Host. A tunnel is identified by its
// service port and a local forward by the port it opens here, which is unique
// over the table and is the only thing on the row that tells two forwards of
// one Host apart.
func rowName(t *testing.T, row map[string]interface{}) string {
	t.Helper()

	kind, _ := row["kind"].(string)
	hostID, _ := row["host_id"].(float64)

	switch kind {
	case statusKindServicePort:
		spID, ok := row["sp_id"].(float64)
		if !ok {
			t.Fatalf("a service port row carries no sp_id: %v", row)
		}

		return fmt.Sprintf("service_port host=%d sp=%d", int(hostID), int(spID))
	case statusKindLocalForward:
		local, _ := row["local"].(string)

		return fmt.Sprintf("local_forward host=%d local=%s", int(hostID), local)
	}

	t.Fatalf("a row carries the kind %q, which is neither sort: %v", kind, row)

	return ""
}

// TestGetStatusCarriesBothSortsOfForward pins the shape the status table took
// on: the tunnels of a Host and its local forwards on the one list, each row
// saying which sort it is, and the local forward carrying no service port.
//
// The addresses are the point of the test beside the count of rows. They are
// the mirror of a tunnel row: local is what this machine opened and remote is
// what the Host reaches, where a tunnel row has the Host opening the port and
// this end reaching the service.
func TestGetStatusCarriesBothSortsOfForward(t *testing.T) {
	db := newRowsDB(t,
		[]models.Host{statusHost(1, true)},
		[]models.ServicePort{statusServicePort(1), statusServicePort(2)},
		[]models.Tunnel{statusTunnel(1, 1, "connected"), statusTunnel(1, 2, "error")})

	storeLocalForwards(t, db, []models.LocalForward{
		statusLocalForward(1, 1, 19000, models.BindScopeWildcard, true),
	})

	rows, data := statusRowsOf(t, db, "/api/status")

	if len(rows) != 3 {
		t.Fatalf("the page carries %d rows, want 3 (two tunnels and one local forward): %v", len(rows), rows)
	}

	forwards := 0

	for _, row := range rows {
		if row["kind"] != statusKindLocalForward {
			continue
		}

		forwards++

		spID, carried := row["sp_id"]
		if !carried {
			t.Errorf("the local forward row leaves sp_id out; it is to be there and empty: %v", row)
		}
		if spID != nil {
			t.Errorf("the local forward row carries sp_id %v, want nothing: a forward is carried by no service port", spID)
		}

		// The number is what the row is read and changed by, so the status
		// table has to carry it: a row seen here and no way to name it is a
		// row nothing can be done about.
		if number, ok := row["number"].(float64); !ok || int(number) != 1 {
			t.Errorf("the local forward row carries number %v, want the 1 it is numbered on its Host", row["number"])
		}

		addresses := []struct {
			field string
			want  string
		}{
			{"server", "192.0.2.1:22"},
			{"local", "0.0.0.0:19000"},
			{"remote", "198.51.100.20:3306"},
			{"forward_reach", ""},
		}
		for _, address := range addresses {
			if row[address.field] != address.want {
				t.Errorf("the local forward row carries %s %v, want %q", address.field, row[address.field], address.want)
			}
		}

		// Nothing runs over this database, so the forward reports what
		// localForwardViewOf says of a row that is on under a Host that is on
		// and is not running.
		if row["status"] != localForwardStatusStopped {
			t.Errorf("the local forward row carries the status %v, want %q", row["status"], localForwardStatusStopped)
		}
	}

	if forwards != 1 {
		t.Fatalf("the page carries %d local forward rows, want 1: %v", forwards, rows)
	}

	// The tunnel rows are unchanged by the forwards standing beside them.
	for _, row := range rows {
		if row["kind"] != statusKindServicePort {
			continue
		}

		if _, ok := row["sp_id"].(float64); !ok {
			t.Errorf("a service port row carries sp_id %v, want the number it always carried", row["sp_id"])
		}

		if _, carried := row["number"]; carried {
			t.Errorf("a service port row carries number %v, which is on no tunnel row", row["number"])
		}
	}

	counts := map[string]int{
		// Two tunnels and one local forward, and every count is over the two
		// sorts together.
		"total_rows":      3,
		"desired_tunnels": 3,
		// One of the two tunnel rows says connected and the other says error,
		// and nothing runs over this database, so the forward is in neither.
		"connected_tunnels":    1,
		"reconnecting_tunnels": 0,
		"error_tunnels":        1,
	}
	for key, want := range counts {
		got := statusCount(t, data, key)
		if got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}
}

// TestGetStatusCarriesWhereALocalForwardListens pins that the address on the
// row is the one the bind scope asks for, and that it is the IPv4 one of the
// pair, which is what a tunnel row carries too.
func TestGetStatusCarriesWhereALocalForwardListens(t *testing.T) {
	tests := []struct {
		name  string
		scope string
		want  string
	}{
		{"the wildcard", models.BindScopeWildcard, "0.0.0.0:19000"},
		{"loopback", models.BindScopeLoopback, "127.0.0.1:19000"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newRowsDB(t, []models.Host{statusHost(1, true)}, nil, nil)

			storeLocalForwards(t, db, []models.LocalForward{
				statusLocalForward(1, 1, 19000, tc.scope, true),
			})

			rows, _ := statusRowsOf(t, db, "/api/status")

			if len(rows) != 1 {
				t.Fatalf("the page carries %d rows, want 1: %v", len(rows), rows)
			}
			if rows[0]["local"] != tc.want {
				t.Errorf("the row says the forward listens on %v, want %q", rows[0]["local"], tc.want)
			}
		})
	}
}

// TestGetStatusReportsWhatALocalForwardRowSays pins that the status on the row
// is the one localForwardViewOf decides, so that the status table and the
// local forward screen say the same thing about the same row.
func TestGetStatusReportsWhatALocalForwardRowSays(t *testing.T) {
	tests := []struct {
		name        string
		hostEnabled bool
		enabled     bool
		want        string
	}{
		{"a forward under a disabled Host", false, true, localForwardStatusDisabled},
		{"a forward that is switched off", true, false, localForwardStatusOff},
		{"a forward that is on and not running", true, true, localForwardStatusStopped},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newRowsDB(t, []models.Host{statusHost(1, tc.hostEnabled)}, nil, nil)

			storeLocalForwards(t, db, []models.LocalForward{
				statusLocalForward(1, 1, 19000, models.BindScopeWildcard, tc.enabled),
			})

			rows, _ := statusRowsOf(t, db, "/api/status")

			if len(rows) != 1 {
				t.Fatalf("the page carries %d rows, want 1: %v", len(rows), rows)
			}
			if rows[0]["status"] != tc.want {
				t.Errorf("the row carries the status %v, want %q", rows[0]["status"], tc.want)
			}
		})
	}
}

// TestGetStatusPagesTheTwoSortsAsOneList is the test this work is for: with
// twenty-five rows of the two sorts over five Hosts and a page of ten, the
// three pages carry every row once, in the one order, and the rows of a Host
// stand together.
//
// A page that dropped a row or carried one twice is what the counting is
// about. Two tables paged apart and put together afterwards do exactly that at
// the boundary, and nothing on the screen says so: it looks like a page.
func TestGetStatusPagesTheTwoSortsAsOneList(t *testing.T) {
	const hosts = 5
	const tunnelsPerHost = 3
	const forwardsPerHost = 2

	var (
		hostRows    []models.Host
		serviceRows []models.ServicePort
		tunnelRows  []models.Tunnel
		wanted      []string
	)

	for sp := 1; sp <= tunnelsPerHost; sp++ {
		serviceRows = append(serviceRows, statusServicePort(uint(sp)))
	}

	for host := 1; host <= hosts; host++ {
		hostRows = append(hostRows, statusHost(uint(host), true))

		for sp := 1; sp <= tunnelsPerHost; sp++ {
			tunnelRows = append(tunnelRows, statusTunnel(uint(host), uint(sp), "connected"))
		}
	}

	db := newRowsDB(t, hostRows, serviceRows, tunnelRows)

	var forwardRows []models.LocalForward

	id := uint(1)

	for host := 1; host <= hosts; host++ {
		for n := range forwardsPerHost {
			port := 19000 + (host-1)*forwardsPerHost + n
			forwardRows = append(forwardRows, statusLocalForward(id, uint(host), port, models.BindScopeWildcard, true))
			id++
		}
	}

	storeLocalForwards(t, db, forwardRows)

	// The order the answer is to be in: the rows of a Host together, its
	// tunnels by service port and then its forwards by row id, which for these
	// rows runs with the local port.
	for host := 1; host <= hosts; host++ {
		for sp := 1; sp <= tunnelsPerHost; sp++ {
			wanted = append(wanted, fmt.Sprintf("service_port host=%d sp=%d", host, sp))
		}

		for n := range forwardsPerHost {
			port := 19000 + (host-1)*forwardsPerHost + n
			wanted = append(wanted, fmt.Sprintf("local_forward host=%d local=0.0.0.0:%d", host, port))
		}
	}

	const total = hosts * (tunnelsPerHost + forwardsPerHost)

	if len(wanted) != total {
		t.Fatalf("the test wants %d rows, want %d", len(wanted), total)
	}

	var got []string

	for number := 1; number <= 3; number++ {
		rows, data := statusRowsOf(t, db, fmt.Sprintf("/api/status?page=%d&size=10", number))

		want := 10
		if number == 3 {
			want = total - 20
		}

		if len(rows) != want {
			t.Fatalf("page %d carries %d rows, want %d: %v", number, len(rows), want, rows)
		}

		if statusCount(t, data, "total_rows") != total {
			t.Fatalf("page %d says total_rows is %d, want %d",
				number, statusCount(t, data, "total_rows"), total)
		}

		for _, row := range rows {
			got = append(got, rowName(t, row))
		}
	}

	if len(got) != total {
		t.Fatalf("the three pages carry %d rows in all, want %d: %v", len(got), total, got)
	}

	seen := make(map[string]int, len(got))
	for _, name := range got {
		seen[name]++
	}

	for name, count := range seen {
		if count != 1 {
			t.Errorf("%s is on the pages %d times, want once", name, count)
		}
	}

	for _, name := range wanted {
		if seen[name] == 0 {
			t.Errorf("%s is on none of the pages", name)
		}
	}

	for i := range wanted {
		if got[i] != wanted[i] {
			t.Fatalf("row %d of the three pages is %s, want %s\ngot:  %v\nwant: %v",
				i, got[i], wanted[i], got, wanted)
		}
	}
}

// TestGetStatusCountsBothSortsTogether pins the four counts to what they now
// count: the service port tunnels and the local forwards added together, each
// sort counted where its status lives.
//
// They used to be six, the three tunnel counts beside three of the forwards,
// and a reader had to add two numbers to learn how much of the installation
// was up. A local forward is a tunnel to whoever reads this answer, so the
// counts are over both sorts and the reader has the number.
func TestGetStatusCountsBothSortsTogether(t *testing.T) {
	hosts := []models.Host{statusHost(1, true), statusHost(2, true)}
	sps := []models.ServicePort{statusServicePort(1), statusServicePort(2)}
	tunnels := []models.Tunnel{
		statusTunnel(1, 1, "connected"),
		statusTunnel(1, 2, "error"),
		statusTunnel(2, 1, "connected"),
		statusTunnel(2, 2, "reconnecting"),
	}

	// What the forwards report. The one that is off runs nothing and says
	// nothing, and the one that is still starting is in none of the three
	// statuses that are counted.
	states := map[tunnel.LocalForwardKey]tunnel.LocalForwardState{
		{HostID: 1, Number: 1}: {Status: "connected"},
		{HostID: 2, Number: 3}: {Status: "error"},
		{HostID: 2, Number: 4}: {Status: "starting"},
	}

	without := newRowsDB(t, hosts, sps, tunnels)
	_, before := statusRowsWithStates(t, without, "/api/status", nil)

	with := newRowsDB(t, hosts, sps, tunnels)
	storeLocalForwards(t, with, []models.LocalForward{
		statusLocalForward(1, 1, 19000, models.BindScopeWildcard, true),
		statusLocalForward(2, 1, 19001, models.BindScopeWildcard, false),
		statusLocalForward(3, 2, 19002, models.BindScopeWildcard, true),
		statusLocalForward(4, 2, 19003, models.BindScopeWildcard, true),
	})

	_, after := statusRowsWithStates(t, with, "/api/status", states)

	counts := map[string]int{
		// Four tunnels, and three of the four forwards are on under a Host
		// that is on.
		"desired_tunnels": 7,
		// Two tunnels and one forward.
		"connected_tunnels": 3,
		// One tunnel and no forward.
		"reconnecting_tunnels": 1,
		// One tunnel and one forward.
		"error_tunnels": 2,
		// Every row of both tables, the ones that run nothing among them.
		"total_rows": 8,
	}
	for key, want := range counts {
		if statusCount(t, after, key) != want {
			t.Errorf("%s = %d, want %d", key, statusCount(t, after, key), want)
		}
	}

	// The forward that is starting is in none of the three, which is what
	// leaves the three short of what should be running.
	if counts["connected_tunnels"]+counts["reconnecting_tunnels"]+counts["error_tunnels"] >= counts["desired_tunnels"] {
		t.Error("the three statuses add up to what should be running; a row that is starting is counted in one of them")
	}

	// Without a local forward stored the counts are the tunnels alone, and
	// they are there and empty rather than left out: a screen reading a field
	// that is not there gets nothing and draws it as a zero anyway, and the
	// two cases would not be told apart.
	alone := map[string]int{
		"desired_tunnels":      4,
		"connected_tunnels":    2,
		"reconnecting_tunnels": 1,
		"error_tunnels":        1,
		"total_rows":           4,
	}
	for key, want := range alone {
		if statusCount(t, before, key) != want {
			t.Errorf("with no local forward stored %s = %d, want %d",
				key, statusCount(t, before, key), want)
		}
	}
}

// TestGetStatusCountsTheSortsUnderNoNamesOfTheirOwn pins that the four counts
// of the sorts apart are gone from the answer.
//
// They are not left in beside the four that replaced them. A count that says
// the service port tunnels alone, standing next to one of the same name that
// says both sorts, is read as the other by everyone who knew the old answer,
// and the screen would draw the same installation two ways.
func TestGetStatusCountsTheSortsUnderNoNamesOfTheirOwn(t *testing.T) {
	db := newRowsDB(t,
		[]models.Host{statusHost(1, true)},
		[]models.ServicePort{statusServicePort(1)},
		[]models.Tunnel{statusTunnel(1, 1, "connected")})

	storeLocalForwards(t, db, []models.LocalForward{
		statusLocalForward(1, 1, 19000, models.BindScopeWildcard, true),
	})

	_, data := statusRowsWithStates(t, db, "/api/status",
		map[tunnel.LocalForwardKey]tunnel.LocalForwardState{
			{HostID: 1, Number: 1}: {Status: "connected"},
		})

	for _, key := range []string{
		"total_tunnels", "total_local_forwards", "connected_local_forwards", "desired_local_forwards",
	} {
		if _, carried := data[key]; carried {
			t.Errorf("the answer still carries %s = %v", key, data[key])
		}
	}

	// The count the pages are cut from is the one that stays. Without it the
	// screen has no last page to walk to.
	if statusCount(t, data, "total_rows") != 2 {
		t.Errorf("total_rows = %d, want 2", statusCount(t, data, "total_rows"))
	}
}

// TestGetStatusCarriesWhatALocalForwardMeasured pins that the reading of the
// probe reaches the row, and that a row nothing is running for carries none.
func TestGetStatusCarriesWhatALocalForwardMeasured(t *testing.T) {
	tests := []struct {
		name   string
		states map[tunnel.LocalForwardKey]tunnel.LocalForwardState
		want   string
	}{
		{
			name: "a target that answered",
			states: map[tunnel.LocalForwardKey]tunnel.LocalForwardState{
				{HostID: 1, Number: 1}: {Status: "connected", ForwardReach: "reachable"},
			},
			want: "reachable",
		},
		{
			name: "a target that did not",
			states: map[tunnel.LocalForwardKey]tunnel.LocalForwardState{
				{HostID: 1, Number: 1}: {Status: "connected", ForwardReach: "unreachable"},
			},
			want: "unreachable",
		},
		{
			name: "a connection whose probe has not answered yet",
			states: map[tunnel.LocalForwardKey]tunnel.LocalForwardState{
				{HostID: 1, Number: 1}: {Status: "connected", ForwardReach: "unknown"},
			},
			want: "unknown",
		},
		{
			name:   "a forward that is not running",
			states: nil,
			want:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newRowsDB(t, []models.Host{statusHost(1, true)}, nil, nil)

			storeLocalForwards(t, db, []models.LocalForward{
				statusLocalForward(1, 1, 19000, models.BindScopeWildcard, true),
			})

			rows, _ := statusRowsWithStates(t, db, "/api/status", tc.states)

			if len(rows) != 1 {
				t.Fatalf("the page carries %d rows, want 1: %v", len(rows), rows)
			}
			if rows[0]["forward_reach"] != tc.want {
				t.Errorf("the local forward row carries forward_reach %v, want %q",
					rows[0]["forward_reach"], tc.want)
			}
		})
	}
}

// TestGetStatusLeavesATunnelRowAsItWas pins the regression: with no local
// forward stored, a row of the status table carries every field the tunnel row
// carried and nothing but kind beside them.
func TestGetStatusLeavesATunnelRowAsItWas(t *testing.T) {
	db := newRowsDB(t,
		[]models.Host{statusHost(1, true)},
		[]models.ServicePort{statusServicePort(1)},
		[]models.Tunnel{statusTunnel(1, 1, "connected")})

	rows, _ := statusRowsOf(t, db, "/api/status")

	if len(rows) != 1 {
		t.Fatalf("the page carries %d rows, want 1: %v", len(rows), rows)
	}

	written, err := json.Marshal(models.Tunnel{})
	if err != nil {
		t.Fatalf("failed to write a tunnel row: %v", err)
	}

	var fields map[string]interface{}

	err = json.Unmarshal(written, &fields)
	if err != nil {
		t.Fatalf("failed to read a tunnel row: %v", err)
	}

	for field := range fields {
		if _, carried := rows[0][field]; !carried {
			t.Errorf("the status row leaves out %s, which the tunnel row carries", field)
		}
	}

	for field := range rows[0] {
		if _, known := fields[field]; known || field == "kind" {
			continue
		}

		t.Errorf("the status row carries %s, which is on no tunnel row", field)
	}

	if rows[0]["kind"] != statusKindServicePort {
		t.Errorf("the tunnel row carries the kind %v, want %q", rows[0]["kind"], statusKindServicePort)
	}
}

// TestGetStatusAnswersAPagePastTheLastOne pins that the page is fitted to the
// rows of both tables together. A page number past the end is answered with
// the last page, the way every other list answers one.
func TestGetStatusAnswersAPagePastTheLastOne(t *testing.T) {
	db := newRowsDB(t,
		[]models.Host{statusHost(1, true)},
		[]models.ServicePort{statusServicePort(1)},
		[]models.Tunnel{statusTunnel(1, 1, "connected")})

	storeLocalForwards(t, db, []models.LocalForward{
		statusLocalForward(1, 1, 19000, models.BindScopeWildcard, true),
	})

	tests := []struct {
		name     string
		target   string
		wantPage int
		wantRows int
	}{
		{"a page past the last one", "/api/status?page=9&size=10", 1, 2},
		{"a page below the first", "/api/status?page=0&size=10", 1, 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows, data := statusRowsOf(t, db, tc.target)

			if statusCount(t, data, "page") != tc.wantPage {
				t.Errorf("the answer is page %d, want %d", statusCount(t, data, "page"), tc.wantPage)
			}
			if len(rows) != tc.wantRows {
				t.Errorf("the page carries %d rows, want %d: %v", len(rows), tc.wantRows, rows)
			}
		})
	}
}

// TestSplitStatusPageCutsBothRuns pins where the page falls on the two tables,
// which is what the two reads are given. The cases are the ones a boundary is
// got wrong at: a page that ends inside a Host, one that begins inside one,
// and a page holding rows of one sort alone.
func TestSplitStatusPageCutsBothRuns(t *testing.T) {
	// Two Hosts, each with two tunnels and two forwards, in the order the
	// table is paged in.
	var tunnels, forwards []statusRef

	for host := uint(1); host <= 2; host++ {
		for ref := uint(1); ref <= 2; ref++ {
			tunnels = append(tunnels, statusRef{HostID: host, Kind: statusKindServicePort, Ref: ref})
			forwards = append(forwards, statusRef{HostID: host, Kind: statusKindLocalForward, Ref: (host-1)*2 + ref})
		}
	}

	refs := mergeStatusRefs(tunnels, forwards)

	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, fmt.Sprintf("%s:%d:%d", ref.Kind, ref.HostID, ref.Ref))
	}

	want := []string{
		"service_port:1:1", "service_port:1:2", "local_forward:1:1", "local_forward:1:2",
		"service_port:2:1", "service_port:2:2", "local_forward:2:3", "local_forward:2:4",
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("the merged order is %v, want %v", names, want)
		}
	}

	tests := []struct {
		name   string
		offset int
		size   int
		want   statusPageSplit
	}{
		{"a page of three, ending inside the first Host", 0, 3,
			statusPageSplit{tunnelOffset: 0, tunnelCount: 2, forwardOffset: 0, forwardCount: 1}},
		{"the page after it, beginning inside the first Host", 3, 3,
			statusPageSplit{tunnelOffset: 2, tunnelCount: 2, forwardOffset: 1, forwardCount: 1}},
		{"the last page, shorter than the size", 6, 3,
			statusPageSplit{tunnelOffset: 4, tunnelCount: 0, forwardOffset: 2, forwardCount: 2}},
		{"a page of tunnels alone", 4, 2,
			statusPageSplit{tunnelOffset: 2, tunnelCount: 2, forwardOffset: 2, forwardCount: 0}},
		{"the whole list", 0, 10,
			statusPageSplit{tunnelOffset: 0, tunnelCount: 4, forwardOffset: 0, forwardCount: 4}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := splitStatusPage(refs, tc.offset, tc.size)
			if got != tc.want {
				t.Errorf("splitStatusPage(%d, %d) = %+v, want %+v", tc.offset, tc.size, got, tc.want)
			}
		})
	}
}

// TestLocalForwardStatusCountsCountEachStatus pins that the forwards are
// counted by what they report, each into its own number, and that a status
// which is none of the three is counted nowhere.
func TestLocalForwardStatusCountsCountEachStatus(t *testing.T) {
	states := map[tunnel.LocalForwardKey]tunnel.LocalForwardState{
		{HostID: 1, Number: 1}: {Status: "connected"},
		{HostID: 1, Number: 2}: {Status: "reconnecting"},
		{HostID: 2, Number: 1}: {Status: "connected"},
		{HostID: 2, Number: 2}: {Status: "error"},
		{HostID: 2, Number: 3}: {Status: "starting"},
	}

	got := localForwardStatusCounts(states)
	want := statusCounts{connected: 2, reconnecting: 1, errored: 1}

	if got != want {
		t.Errorf("localForwardStatusCounts = %+v, want %+v", got, want)
	}

	if localForwardStatusCounts(nil) != (statusCounts{}) {
		t.Errorf("with nothing running the counts are %+v, want every one of them empty",
			localForwardStatusCounts(nil))
	}
}
