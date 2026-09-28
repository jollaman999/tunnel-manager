package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// searchPage is what a list answers with, read down to what the search tests
// look at: the ids on the page, and how many rows there are and which page
// this is.
type searchPage struct {
	ids   []uint
	total int
	page  int
}

// listSearch runs one of the two list handlers over db with the given query
// string and hands back the page.
func listSearch(t *testing.T, db *gorm.DB, list func(*Handler) func(echo.Context) error, query string) searchPage {
	t.Helper()

	c, rec := getRequest(t, "/api/list?"+query, "", "")
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	err := list(h)(c)
	if err != nil {
		t.Fatalf("the list returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Items []struct {
				ID uint `json:"id"`
			} `json:"items"`
			Total int `json:"total"`
			Page  int `json:"page"`
		} `json:"data"`
	}

	err = json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil {
		t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
	}
	if !resp.Success {
		t.Fatalf("success = false, body: %s", rec.Body.String())
	}

	got := searchPage{ids: []uint{}, total: resp.Data.Total, page: resp.Data.Page}
	for _, item := range resp.Data.Items {
		got.ids = append(got.ids, item.ID)
	}

	return got
}

func listHosts(h *Handler) func(echo.Context) error        { return h.ListHosts }
func listServicePorts(h *Handler) func(echo.Context) error { return h.ListServicePorts }

// searchCase is one search and the ids it is to find, all on the first page.
type searchCase struct {
	name string
	q    string
	want []uint
}

func runSearchCases(t *testing.T, db *gorm.DB, list func(*Handler) func(echo.Context) error, cases []searchCase) {
	t.Helper()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := listSearch(t, db, list, "q="+url.QueryEscape(tc.q))

			if !reflect.DeepEqual(got.ids, tc.want) {
				t.Errorf("q=%q finds %v, want %v", tc.q, got.ids, tc.want)
			}
			if got.total != len(tc.want) {
				t.Errorf("q=%q says total is %d, want %d", tc.q, got.total, len(tc.want))
			}
		})
	}
}

// TestListHostsNarrowsTheRowsToASearch pins q on the Hosts: a part of the
// address, the user, the description or the SSH port finds the Host whatever
// the case of its letters, and the characters LIKE would take as wildcards are
// taken as written.
func TestListHostsNarrowsTheRowsToASearch(t *testing.T) {
	hosts := []models.Host{
		{ID: 1, Address: "192.0.2.1", Port: 22, User: "root", Description: "Web Server", Enabled: true},
		{ID: 2, Address: "192.0.2.2", Port: 2222, User: "deploy", Description: "50% of the load", Enabled: true},
		{ID: 3, Address: "db.example.com", Port: 22, User: "root", Description: "db_1", Enabled: true},
		{ID: 4, Address: "192.0.2.4", Port: 22, User: "root", Description: "500 of them, dbx1", Enabled: true},
	}
	db := newRowsDB(t, hosts, nil, nil)

	runSearchCases(t, db, listHosts, []searchCase{
		{"the description in another case", "WEB", []uint{1}},
		{"a part of the address", "example", []uint{3}},
		{"the user", "deploy", []uint{2}},
		{"the port as text", "222", []uint{2}},
		{"nothing matches", "no-such-host", []uint{}},
		{"a percent sign is a percent sign", "50%", []uint{2}},
		{"an underscore is an underscore", "db_1", []uint{3}},
		{"a backslash is a backslash", `\`, []uint{}},
	})

	// No q and an empty one are the list as it was.
	for _, query := range []string{"", "q="} {
		got := listSearch(t, db, listHosts, query)
		if !reflect.DeepEqual(got.ids, []uint{1, 2, 3, 4}) || got.total != 4 {
			t.Errorf("%q answers %v of %d, want every Host", query, got.ids, got.total)
		}
	}
}

// TestListHostsPagesTheRowsThatMatch pins that a search is made before the
// page is cut: total is the rows that match, and the second page carries the
// rest of them and none of the rows that do not.
func TestListHostsPagesTheRowsThatMatch(t *testing.T) {
	var hosts []models.Host

	for id := uint(1); id <= 30; id++ {
		description := "other"
		if id%2 == 0 {
			description = "wanted"
		}

		hosts = append(hosts, models.Host{
			ID: id, Address: fmt.Sprintf("192.0.2.%d", id), Port: 22, User: "root",
			Description: description, Enabled: true,
		})
	}

	db := newRowsDB(t, hosts, nil, nil)

	got := listSearch(t, db, listHosts, "q=wanted&page=2&size=10")

	want := []uint{22, 24, 26, 28, 30}
	if !reflect.DeepEqual(got.ids, want) {
		t.Errorf("page 2 carries %v, want %v", got.ids, want)
	}
	if got.total != 15 || got.page != 2 {
		t.Errorf("total = %d and page = %d, want 15 and 2", got.total, got.page)
	}

	// A page past the last page of the rows that match is the last of them,
	// not the last of the table.
	got = listSearch(t, db, listHosts, "q=wanted&page=3&size=10")
	if got.page != 2 || !reflect.DeepEqual(got.ids, want) {
		t.Errorf("page 3 answers page %d with %v, want page 2 with %v", got.page, got.ids, want)
	}
}

// TestListServicePortsNarrowsTheRowsToASearch is the two Host tests above over
// the service ports: what a search finds, and the page cut from what it found.
func TestListServicePortsNarrowsTheRowsToASearch(t *testing.T) {
	sps := []models.ServicePort{
		{ID: 1, ServiceAddress: "198.51.100.10", ServicePort: 8080, LocalPort: 18080, Description: "Admin UI"},
		{ID: 2, ServiceAddress: "198.51.100.11", ServicePort: 5432, LocalPort: 15432, Description: "100% of reads"},
		{ID: 3, ServiceAddress: "db.example.com", ServicePort: 3306, LocalPort: 13306, Description: "db_main"},
		{ID: 4, ServiceAddress: "198.51.100.12", ServicePort: 6379, LocalPort: 16379, Description: "1000 of dbxmain"},
	}
	db := newRowsDB(t, nil, sps, nil)

	runSearchCases(t, db, listServicePorts, []searchCase{
		{"the description in another case", "admin ui", []uint{1}},
		{"a part of the service address", "EXAMPLE", []uint{3}},
		{"the service port as text", "5432", []uint{2}},
		{"the local port as text", "1637", []uint{4}},
		{"nothing matches", "no-such-port", []uint{}},
		{"a percent sign is a percent sign", "100%", []uint{2}},
		{"an underscore is an underscore", "db_main", []uint{3}},
	})

	var many []models.ServicePort
	for id := uint(1); id <= 30; id++ {
		address := "198.51.100.10"
		if id%2 == 0 {
			address = "db.example.com"
		}

		many = append(many, models.ServicePort{
			ID: id, ServiceAddress: address, ServicePort: 9000 + int(id), LocalPort: 19000 + int(id),
		})
	}

	db = newRowsDB(t, nil, many, nil)

	got := listSearch(t, db, listServicePorts, "q=example&page=2&size=10")

	want := []uint{22, 24, 26, 28, 30}
	if !reflect.DeepEqual(got.ids, want) || got.total != 15 || got.page != 2 {
		t.Errorf("page 2 answers %v of %d on page %d, want %v of 15 on page 2", got.ids, got.total, got.page, want)
	}
}

// searchStatusDB is two Hosts, each with two tunnels and two local forwards,
// with addresses and descriptions a search can tell apart.
func searchStatusDB(t *testing.T) *gorm.DB {
	t.Helper()

	hosts := []models.Host{statusHost(1, true), statusHost(2, true)}
	hosts[0].Description = "Seoul_office"
	hosts[1].Description = "Busan 100%"

	sps := []models.ServicePort{statusServicePort(1), statusServicePort(2)}

	tunnels := []models.Tunnel{
		statusTunnel(1, 1, "connected"),
		statusTunnel(1, 2, "error"),
		statusTunnel(2, 1, "connected"),
		statusTunnel(2, 2, "reconnecting"),
	}
	for i := range tunnels {
		tunnels[i].Local = fmt.Sprintf("127.0.0.1:%d", 18080+tunnels[i].SPID)
		tunnels[i].Remote = fmt.Sprintf("198.51.100.10:%d", 8080+tunnels[i].SPID)
	}

	db := newRowsDB(t, hosts, sps, tunnels)

	forwards := []models.LocalForward{
		statusLocalForward(1, 1, 19000, models.BindScopeWildcard, true),
		statusLocalForward(2, 1, 19001, models.BindScopeLoopback, true),
		statusLocalForward(3, 2, 19002, models.BindScopeWildcard, true),
		statusLocalForward(4, 2, 19003, models.BindScopeWildcard, true),
	}
	forwards[3].TargetAddress = "203.0.113.7"

	storeLocalForwards(t, db, forwards)

	return db
}

// statusNames is the rows of a status page as rowName names them.
func statusNames(t *testing.T, rows []map[string]interface{}) []string {
	t.Helper()

	names := []string{}
	for _, row := range rows {
		names = append(names, rowName(t, row))
	}

	return names
}

// TestGetStatusNarrowsTheRowsToASearch pins q on the status table: a Host
// found by its address or its description brings every row of both sorts it
// carries, a row is found by its own local or remote address whichever sort
// it is, and % and _ are taken as written.
func TestGetStatusNarrowsTheRowsToASearch(t *testing.T) {
	db := searchStatusDB(t)

	host1 := []string{
		"service_port host=1 sp=1",
		"service_port host=1 sp=2",
		"local_forward host=1 local=0.0.0.0:19000",
		"local_forward host=1 local=127.0.0.1:19001",
	}
	host2 := []string{
		"service_port host=2 sp=1",
		"service_port host=2 sp=2",
		"local_forward host=2 local=0.0.0.0:19002",
		"local_forward host=2 local=0.0.0.0:19003",
	}

	cases := []struct {
		name string
		q    string
		want []string
	}{
		{"the address of a Host", "192.0.2.2", host2},
		{"the description of a Host in another case", "seoul", host1},
		{"an underscore is an underscore", "seoul_", host1},
		{"an underscore matches no other character", "busan_100", []string{}},
		{"a percent sign is a percent sign", "100%", host2},
		{"a percent sign matches nothing else", "busan%100", []string{}},
		{"the local address of a tunnel", "127.0.0.1:18082", []string{
			"service_port host=1 sp=2",
			"service_port host=2 sp=2",
		}},
		{"where a local forward listens", "127.0.0.1:1900", []string{
			"local_forward host=1 local=127.0.0.1:19001",
		}},
		{"the target of a local forward", "203.0.113", []string{
			"local_forward host=2 local=0.0.0.0:19003",
		}},
		{"the remote address of a tunnel", "198.51.100.1", []string{
			"service_port host=1 sp=1",
			"service_port host=1 sp=2",
			"service_port host=2 sp=1",
			"service_port host=2 sp=2",
		}},
		{"nothing matches", "no-such-row", []string{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, data := statusRowsOf(t, db, "/api/status?q="+url.QueryEscape(tc.q))

			got := statusNames(t, rows)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("q=%q finds %v, want %v", tc.q, got, tc.want)
			}
			if statusCount(t, data, "total_rows") != len(tc.want) {
				t.Errorf("q=%q says total_rows is %d, want %d",
					tc.q, statusCount(t, data, "total_rows"), len(tc.want))
			}
		})
	}

	// No q and an empty one are the table as it was.
	for _, target := range []string{"/api/status", "/api/status?q="} {
		rows, data := statusRowsOf(t, db, target)

		want := append(append([]string{}, host1...), host2...)
		if got := statusNames(t, rows); !reflect.DeepEqual(got, want) {
			t.Errorf("%s answers %v, want %v", target, got, want)
		}
		if statusCount(t, data, "total_rows") != 8 {
			t.Errorf("%s says total_rows is %d, want 8", target, statusCount(t, data, "total_rows"))
		}
	}
}

// TestGetStatusPagesASearchAcrossPages pins paging over the rows that match
// with pages of the size the API serves: a search that matches more rows than
// a page holds is cut into pages of those rows alone, in the order of the
// table, and the last page is the rest of them.
func TestGetStatusPagesASearchAcrossPages(t *testing.T) {
	const hosts = 6

	var (
		hostRows   []models.Host
		tunnelRows []models.Tunnel
		wanted     []string
	)

	sps := []models.ServicePort{statusServicePort(1), statusServicePort(2)}

	for id := uint(1); id <= hosts; id++ {
		host := statusHost(id, true)
		if id%2 == 1 {
			host.Description = "wanted"
		}

		hostRows = append(hostRows, host)

		for sp := uint(1); sp <= 2; sp++ {
			tunnelRows = append(tunnelRows, statusTunnel(id, sp, "connected"))
		}
	}

	db := newRowsDB(t, hostRows, sps, tunnelRows)

	var forwards []models.LocalForward

	for id := uint(1); id <= hosts; id++ {
		for n := uint(0); n < 2; n++ {
			number := (id-1)*2 + n + 1
			port := 19000 + int(number)
			forwards = append(forwards, statusLocalForward(number, id, port, models.BindScopeWildcard, true))
		}
	}

	storeLocalForwards(t, db, forwards)

	for id := uint(1); id <= hosts; id += 2 {
		for sp := 1; sp <= 2; sp++ {
			wanted = append(wanted, fmt.Sprintf("service_port host=%d sp=%d", id, sp))
		}

		for n := uint(0); n < 2; n++ {
			wanted = append(wanted, fmt.Sprintf("local_forward host=%d local=0.0.0.0:%d", id, 19000+int((id-1)*2+n+1)))
		}
	}

	var got []string

	for number := 1; number <= 2; number++ {
		rows, data := statusRowsOf(t, db, fmt.Sprintf("/api/status?q=wanted&page=%d&size=10", number))

		wantRows := 10
		if number == 2 {
			wantRows = 2
		}

		if len(rows) != wantRows {
			t.Fatalf("page %d carries %d rows, want %d", number, len(rows), wantRows)
		}
		if statusCount(t, data, "total_rows") != 12 {
			t.Fatalf("page %d says total_rows is %d, want 12", number, statusCount(t, data, "total_rows"))
		}

		got = append(got, statusNames(t, rows)...)
	}

	if !reflect.DeepEqual(got, wanted) {
		t.Fatalf("the two pages carry\n%v\nwant\n%v", got, wanted)
	}
}

// TestGetStatusCountsEveryRowWhateverTheSearch pins that q leaves the four
// counts alone. They say what the installation is doing, and a search on the
// screen that found one row would otherwise say that one tunnel is connected
// while the rest carry traffic.
func TestGetStatusCountsEveryRowWhateverTheSearch(t *testing.T) {
	db := searchStatusDB(t)

	states := map[tunnel.LocalForwardKey]tunnel.LocalForwardState{
		{HostID: 1, Number: 1}: {Status: "connected"},
		{HostID: 2, Number: 3}: {Status: "error"},
	}

	_, all := statusRowsWithStates(t, db, "/api/status", states)

	for _, q := range []string{"seoul", "no-such-row", "203.0.113"} {
		_, narrowed := statusRowsWithStates(t, db, "/api/status?q="+url.QueryEscape(q), states)

		for _, key := range []string{
			"desired_tunnels", "connected_tunnels", "reconnecting_tunnels", "error_tunnels",
			"host_keys_unapproved", "host_keys_mismatched",
		} {
			if statusCount(t, narrowed, key) != statusCount(t, all, key) {
				t.Errorf("q=%q says %s is %d, want %d as without it",
					q, key, statusCount(t, narrowed, key), statusCount(t, all, key))
			}
		}
	}

	if statusCount(t, all, "connected_tunnels") != 3 {
		t.Errorf("connected_tunnels = %d, want 3", statusCount(t, all, "connected_tunnels"))
	}
}

// TestLikeContainingEscapesWhatLikeReads pins the pattern a search is bound
// as: the text between two wildcards, with the three characters LIKE reads
// escaped and nothing else touched.
func TestLikeContainingEscapesWhatLikeReads(t *testing.T) {
	cases := map[string]string{
		"web":  "%web%",
		"50%":  `%50\%%`,
		"db_1": `%db\_1%`,
		`a\b`:  `%a\\b%`,
		"it's": "%it's%",
		"서울":   "%서울%",
		`%_\`:  `%\%\_\\%`,
		"":     "%%",
	}

	for text, want := range cases {
		if got := likeContaining(text); got != want {
			t.Errorf("likeContaining(%q) = %q, want %q", text, got, want)
		}
	}
}

// listHostsByIDsDB is 30 Hosts, the even ones described as wanted, and a jump
// route on Host 4 that goes through 2 and then 3.
func listHostsByIDsDB(t *testing.T) *gorm.DB {
	t.Helper()

	var hosts []models.Host

	for id := uint(1); id <= 30; id++ {
		description := "other"
		if id%2 == 0 {
			description = "wanted"
		}

		hosts = append(hosts, models.Host{
			ID: id, Address: fmt.Sprintf("192.0.2.%d", id), Port: 22, User: "root",
			Description: description, Enabled: true,
		})
	}

	db := newRowsDB(t, hosts, nil, nil)

	for seq, jump := range []uint{2, 3} {
		err := db.Create(&models.HostJump{HostID: 4, Seq: uint(seq + 1), JumpHostID: jump}).Error
		if err != nil {
			t.Fatalf("failed to store the route: %v", err)
		}
	}

	return db
}

// TestListHostsByIDsAnswersThoseHostsOnOnePage pins ids on the Hosts: the
// Hosts named are answered in id order whatever order they are named in, one
// named twice is answered once, one that is not registered is left out, and
// page and size are not what cuts them.
func TestListHostsByIDsAnswersThoseHostsOnOnePage(t *testing.T) {
	db := listHostsByIDsDB(t)

	var many []string
	var every []uint
	for id := uint(1); id <= 30; id++ {
		many = append(many, fmt.Sprint(id))
		every = append(every, id)
	}

	tests := []struct {
		name  string
		query string
		want  []uint
	}{
		{"in id order", "ids=12,3,7", []uint{3, 7, 12}},
		{"one named twice is answered once", "ids=7,3,7,3", []uint{3, 7}},
		{"one not registered is left out", "ids=5,999,6", []uint{5, 6}},
		{"none registered is an empty list", "ids=999", []uint{}},
		{"page and size are not read", "ids=" + url.QueryEscape(strings.Join(many, ",")) + "&page=3&size=10", every},
		{"a page that is not a number is not read either", "ids=1&page=x", []uint{1}},
		{"q narrows them", "ids=3,4,5,6&q=wanted", []uint{4, 6}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := listSearch(t, db, listHosts, tc.query)

			if !reflect.DeepEqual(got.ids, tc.want) {
				t.Errorf("%s answers %v, want %v", tc.query, got.ids, tc.want)
			}
			if got.total != len(tc.want) || got.page != 1 {
				t.Errorf("%s says total %d on page %d, want %d on page 1", tc.query, got.total, got.page, len(tc.want))
			}
		})
	}

	// No ids and an empty one are the list as it was.
	for _, query := range []string{"", "ids="} {
		got := listSearch(t, db, listHosts, query)
		if !reflect.DeepEqual(got.ids, every[:defaultPageSize]) || got.total != 30 {
			t.Errorf("%q answers %v of %d, want the first page of every Host", query, got.ids, got.total)
		}
	}
}

// TestListHostsByIDsCarriesTheJumpRoute pins that a Host read by id is the
// hostView the list answers, jump_host_ids and all.
func TestListHostsByIDsCarriesTheJumpRoute(t *testing.T) {
	db := listHostsByIDsDB(t)

	c, rec := getRequest(t, "/api/host?ids=4,5", "", "")
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	err := h.ListHosts(c)
	if err != nil {
		t.Fatalf("the list returned error: %v", err)
	}

	var page struct {
		Data struct {
			Items []hostView `json:"items"`
		} `json:"data"`
	}

	err = json.Unmarshal(rec.Body.Bytes(), &page)
	if err != nil {
		t.Fatalf("failed to read the list %s: %v", rec.Body.String(), err)
	}

	want := map[uint][]uint{4: {2, 3}, 5: {}}
	if len(page.Data.Items) != len(want) {
		t.Fatalf("the list carries %d Hosts, want %d: %s", len(page.Data.Items), len(want), rec.Body.String())
	}

	for _, view := range page.Data.Items {
		if !reflect.DeepEqual(view.JumpHostIDs, want[view.ID]) {
			t.Errorf("Host %d carries %v, want %v", view.ID, view.JumpHostIDs, want[view.ID])
		}
	}
}

// TestListHostsByIDsRefusesWhatIsNotAListOfIDs pins the refusal of ids: a
// value that is not positive whole numbers separated by commas, and more of
// them than maxListHostIDs, are refused with the same code, and the ids that
// are just at the limit are taken.
func TestListHostsByIDsRefusesWhatIsNotAListOfIDs(t *testing.T) {
	db := listHostsByIDsDB(t)
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	idsOf := func(count int) string {
		ids := make([]string, 0, count)
		for id := 1; id <= count; id++ {
			ids = append(ids, fmt.Sprint(id))
		}

		return strings.Join(ids, ",")
	}

	tests := []struct {
		name   string
		ids    string
		status int
	}{
		{"not a number", "x", http.StatusBadRequest},
		{"zero", "0", http.StatusBadRequest},
		{"below zero", "-3", http.StatusBadRequest},
		{"an empty entry", "3,,7", http.StatusBadRequest},
		{"a trailing comma", "3,7,", http.StatusBadRequest},
		{"a space", "3, 7", http.StatusBadRequest},
		{"past 32 bits", "4294967296", http.StatusBadRequest},
		{"one past the limit", idsOf(maxListHostIDs + 1), http.StatusBadRequest},
		{"one past the limit, all the same id", strings.Repeat("1,", maxListHostIDs) + "1", http.StatusBadRequest},
		{"just at the limit", idsOf(maxListHostIDs), http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := getRequest(t, "/api/host?ids="+url.QueryEscape(tc.ids), "", "")

			err := h.ListHosts(c)
			if err != nil {
				t.Fatalf("the list returned error: %v", err)
			}
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, tc.status, rec.Body.String())
			}
			if tc.status == http.StatusOK {
				return
			}

			var refusal errorBody

			err = json.Unmarshal(rec.Body.Bytes(), &refusal)
			if err != nil {
				t.Fatalf("failed to read the refusal %s: %v", rec.Body.String(), err)
			}
			if refusal.Code != errHostListIDsInvalid {
				t.Errorf("code = %q, want %q", refusal.Code, errHostListIDsInvalid)
			}
		})
	}
}

// TestListHostsByIDsIsTwoReadsHoweverManyIDs pins what the parameter is for:
// the Hosts are one read and their routes another, so a screen that asks for
// many Hosts costs the database what it costs to ask for one.
func TestListHostsByIDsIsTwoReadsHoweverManyIDs(t *testing.T) {
	db := listHostsByIDsDB(t)

	var reads atomic.Int64

	err := db.Callback().Query().After("gorm:query").Register("test:count_reads", func(*gorm.DB) {
		reads.Add(1)
	})
	if err != nil {
		t.Fatalf("failed to register the read counter: %v", err)
	}

	for _, ids := range []string{"4", "1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20"} {
		reads.Store(0)

		got := listSearch(t, db, listHosts, "ids="+url.QueryEscape(ids))
		if len(got.ids) == 0 {
			t.Fatalf("ids=%s answers no Host", ids)
		}

		if n := reads.Load(); n != 2 {
			t.Errorf("ids=%s is %d reads, want 2", ids, n)
		}
	}
}
