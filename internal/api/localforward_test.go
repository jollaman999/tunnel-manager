package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/go-playground/validator/v10"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/settings"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// localForwardAPIPort is the port the settings of these tests are stored with.
// It is not the default, so a check that read the default instead of the
// stored row would let it through.
const localForwardAPIPort = 19443

// newLocalForwardDB returns a database holding these Hosts and forwards, with
// the settings stored. The pool holds one connection, as the server's does, so
// a handler that reads through h.db while its transaction is open waits on
// itself here instead of going through.
func newLocalForwardDB(t *testing.T, hosts []models.Host, forwards []models.LocalForward) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "tunnel-manager.db")), &gorm.Config{
		Logger: gormlogger.Discard,
	})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to reach the connection pool: %v", err)
	}

	sqlDB.SetMaxOpenConns(1)

	err = db.AutoMigrate(&models.Host{}, &models.LocalForward{}, &settings.Settings{})
	if err != nil {
		t.Fatalf("failed to migrate the database: %v", err)
	}

	stored := settings.Defaults()
	stored.APIPort = localForwardAPIPort

	err = settings.Save(db, &stored)
	if err != nil {
		t.Fatalf("failed to store the settings: %v", err)
	}

	for _, host := range hosts {
		enabled := host.Enabled

		err = db.Create(&host).Error
		if err != nil {
			t.Fatalf("failed to store a Host: %v", err)
		}

		// Written by name for the reason newRowsDB gives.
		err = db.Model(&models.Host{}).Where("id = ?", host.ID).Update("enabled", enabled).Error
		if err != nil {
			t.Fatalf("failed to store whether a Host is enabled: %v", err)
		}
	}

	for _, lf := range forwards {
		err = db.Create(&lf).Error
		if err != nil {
			t.Fatalf("failed to store a local forward: %v", err)
		}
	}

	return db
}

func storedLocalForward(id, hostID uint, localPort int) models.LocalForward {
	return models.LocalForward{
		Number:     id,
		HostID:     hostID,
		BindScope:  models.BindScopeLoopback,
		LocalPort:  localPort,
		TargetIP:   "127.0.0.1",
		TargetPort: 5432,
		Enabled:    true,
	}
}

// localForwardPage is a success carrying one page of the list.
type localForwardPage struct {
	Success bool `json:"success"`
	Data    struct {
		Items []localForwardView `json:"items"`
		Total int64              `json:"total"`
		Page  int                `json:"page"`
		Size  int                `json:"size"`
	} `json:"data"`
}

func readLocalForwardPage(t *testing.T, rec *httptest.ResponseRecorder) localForwardPage {
	t.Helper()

	var answer localForwardPage

	err := json.Unmarshal(rec.Body.Bytes(), &answer)
	if err != nil {
		t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
	}

	if !answer.Success {
		t.Fatalf("success = false, body: %s", rec.Body.String())
	}

	return answer
}

// localForwardRequest builds a request with a JSON body and one path
// parameter, the way echo hands one to a handler.
func localForwardRequest(t *testing.T, method, target, body, value string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()

	e := echo.New()
	e.Validator = &testValidator{validator: validator.New()}

	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}

	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues(value)

	return c, rec
}

// localForwardAnswer is a success carrying one row.
type localForwardAnswer struct {
	Success bool             `json:"success"`
	Data    localForwardView `json:"data"`
}

func readLocalForwardAnswer(t *testing.T, rec *httptest.ResponseRecorder) localForwardView {
	t.Helper()

	var answer localForwardAnswer

	err := json.Unmarshal(rec.Body.Bytes(), &answer)
	if err != nil {
		t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
	}

	if !answer.Success {
		t.Fatalf("success = false, body: %s", rec.Body.String())
	}

	return answer.Data
}

// readRefusalCode reads the code a refusal went out under.
func readRefusalCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	var body errorBody

	err := json.Unmarshal(rec.Body.Bytes(), &body)
	if err != nil {
		t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
	}

	return string(body.Code)
}

func storedLocalForwards(t *testing.T, db *gorm.DB) []models.LocalForward {
	t.Helper()

	var rows []models.LocalForward

	err := db.Order("host_id, number").Find(&rows).Error
	if err != nil {
		t.Fatalf("failed to read the local forwards: %v", err)
	}

	return rows
}

// TestCreateHostLocalForwardStoresTheRow pins a creation: the row is stored
// under the Host of the path, an empty scope is stored and answered as the
// wildcard, the answer carries the status of a forward nothing runs for yet,
// and the loop is woken.
func TestCreateHostLocalForwardStoresTheRow(t *testing.T) {
	db := newLocalForwardDB(t, []models.Host{statusHost(1, true)}, nil)
	manager := &wakeRecorder{tx: &txConnPool{}}
	h := NewHandler(db, manager, zap.NewNop(), newTestCipher(t))

	c, rec := localForwardRequest(t, http.MethodPost, "/api/host/1/local-forward",
		`{"local_port":15432,"target_ip":"127.0.0.1","target_port":5432,"description":"db"}`, "1")

	err := h.CreateHostLocalForward(c)
	if err != nil {
		t.Fatalf("CreateHostLocalForward returned error: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	got := readLocalForwardAnswer(t, rec)
	if got.Number == 0 || got.HostID != 1 || got.LocalPort != 15432 || got.TargetIP != "127.0.0.1" ||
		got.TargetPort != 5432 || got.Description != "db" {
		t.Errorf("answer = %+v, want the row that was sent under Host 1", got)
	}
	if got.BindScope != models.BindScopeWildcard {
		t.Errorf("bind_scope = %q, want %q", got.BindScope, models.BindScopeWildcard)
	}
	if got.Status != localForwardStatusStopped {
		t.Errorf("status = %q, want %q", got.Status, localForwardStatusStopped)
	}

	rows := storedLocalForwards(t, db)
	if len(rows) != 1 || rows[0].BindScope != models.BindScopeWildcard || rows[0].HostID != 1 {
		t.Errorf("stored = %+v, want one wildcard row under Host 1", rows)
	}

	wakes, _ := manager.counts()
	if wakes != 1 {
		t.Errorf("reconcile wake-ups = %d, want 1", wakes)
	}
}

// TestListHostLocalForwardsCarriesTheStatus pins the list: only the rows of
// the Host of the path, each with what its running forward reports, "stopped"
// for one nothing runs for, and "disabled" for every row of a disabled Host
// whatever the manager says.
func TestListHostLocalForwardsCarriesTheStatus(t *testing.T) {
	connectedAt := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	db := newLocalForwardDB(t,
		[]models.Host{statusHost(1, true), statusHost(2, false)},
		[]models.LocalForward{
			storedLocalForward(1, 1, 15001),
			storedLocalForward(2, 1, 15002),
			storedLocalForward(3, 2, 15003),
		})

	manager := &wakeRecorder{tx: &txConnPool{}, localStates: map[uint]tunnel.LocalForwardState{
		15001: {Status: "connected", RetryCount: 2, LastConnectedAt: connectedAt},
		15003: {Status: "connected"},
	}}
	h := NewHandler(db, manager, zap.NewNop(), newTestCipher(t))

	c, rec := localForwardRequest(t, http.MethodGet, "/api/host/1/local-forward", "", "1")

	err := h.ListHostLocalForwards(c)
	if err != nil {
		t.Fatalf("ListHostLocalForwards returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	answer := readLocalForwardPage(t, rec)

	if len(answer.Data.Items) != 2 || answer.Data.Total != 2 {
		t.Fatalf("the answer carries %d rows of %d, want the 2 of Host 1, body: %s",
			len(answer.Data.Items), answer.Data.Total, rec.Body.String())
	}

	first := answer.Data.Items[0]
	if first.Number != 1 || first.Status != "connected" || first.RetryCount != 2 || !first.LastConnectedAt.Equal(connectedAt) {
		t.Errorf("row 1 = %+v, want the state the manager reports", first)
	}
	if first.BindScope != models.BindScopeLoopback {
		t.Errorf("row 1 bind_scope = %q, want %q", first.BindScope, models.BindScopeLoopback)
	}

	if answer.Data.Items[1].Number != 2 || answer.Data.Items[1].Status != localForwardStatusStopped {
		t.Errorf("row 2 = %+v, want status %q", answer.Data.Items[1], localForwardStatusStopped)
	}

	// The fields are there by name on the wire, which is what a screen reads.
	for _, field := range []string{`"status"`, `"last_error"`, `"retry_count"`, `"last_connected_at"`} {
		if !strings.Contains(rec.Body.String(), field) {
			t.Errorf("the answer carries no %s, body: %s", field, rec.Body.String())
		}
	}

	c, rec = localForwardRequest(t, http.MethodGet, "/api/host/2/local-forward", "", "2")

	err = h.ListHostLocalForwards(c)
	if err != nil {
		t.Fatalf("ListHostLocalForwards returned error: %v", err)
	}

	answer = readLocalForwardPage(t, rec)

	if len(answer.Data.Items) != 1 || answer.Data.Items[0].Status != localForwardStatusDisabled {
		t.Errorf("the rows of the disabled Host = %+v, want one with status %q",
			answer.Data.Items, localForwardStatusDisabled)
	}
}

// TestListHostLocalForwardsAnswersEmptyAsAnArray pins that a Host with no
// forwards is answered with an empty array of items and not a null.
func TestListHostLocalForwardsAnswersEmptyAsAnArray(t *testing.T) {
	db := newLocalForwardDB(t, []models.Host{statusHost(1, true)}, nil)
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	c, rec := localForwardRequest(t, http.MethodGet, "/api/host/1/local-forward", "", "1")

	err := h.ListHostLocalForwards(c)
	if err != nil {
		t.Fatalf("ListHostLocalForwards returned error: %v", err)
	}

	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Errorf("body = %s, want an empty array under items", rec.Body.String())
	}
}

// TestListHostLocalForwardsIsPaged pins the paging of the list: the page and
// the size are read the way every other list reads them, the rows are those of
// the Host of the path alone, in the order of their id, and a page past the
// last one is answered with the last page.
func TestListHostLocalForwardsIsPaged(t *testing.T) {
	forwards := make([]models.LocalForward, 0, 13)
	for i := uint(1); i <= 12; i++ {
		forwards = append(forwards, storedLocalForward(i, 1, 15000+int(i)))
	}
	forwards = append(forwards, storedLocalForward(13, 2, 16000))

	db := newLocalForwardDB(t, []models.Host{statusHost(1, true), statusHost(2, true)}, forwards)
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	cases := []struct {
		query string
		page  int
		size  int
		ids   []uint
	}{
		{query: "", page: 1, size: 10, ids: []uint{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}},
		{query: "?page=2", page: 2, size: 10, ids: []uint{11, 12}},
		{query: "?page=9&size=10", page: 2, size: 10, ids: []uint{11, 12}},
		{query: "?page=1&size=20", page: 1, size: 20, ids: []uint{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}},
	}

	for _, tc := range cases {
		c, rec := localForwardRequest(t, http.MethodGet, "/api/host/1/local-forward"+tc.query, "", "1")

		err := h.ListHostLocalForwards(c)
		if err != nil {
			t.Fatalf("%q: ListHostLocalForwards returned error: %v", tc.query, err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("%q: status = %d, want %d, body: %s", tc.query, rec.Code, http.StatusOK, rec.Body.String())
		}

		answer := readLocalForwardPage(t, rec)

		ids := make([]uint, 0, len(answer.Data.Items))
		for _, item := range answer.Data.Items {
			ids = append(ids, item.Number)
		}

		if answer.Data.Total != 12 || answer.Data.Page != tc.page || answer.Data.Size != tc.size ||
			!reflect.DeepEqual(ids, tc.ids) {
			t.Errorf("%q: total=%d page=%d size=%d ids=%v, want total=12 page=%d size=%d ids=%v",
				tc.query, answer.Data.Total, answer.Data.Page, answer.Data.Size, ids, tc.page, tc.size, tc.ids)
		}
	}

	for _, query := range []string{"?page=x", "?size=7"} {
		c, rec := localForwardRequest(t, http.MethodGet, "/api/host/1/local-forward"+query, "", "1")

		err := h.ListHostLocalForwards(c)
		if err != nil {
			t.Fatalf("%q: ListHostLocalForwards returned error: %v", query, err)
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want %d, body: %s", query, rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	}
}

// TestALocalForwardIsSwitchedOnUnlessAskedOtherwise pins the enabled flag of a
// creation: left out it is on, and false is stored as false and answered as
// "off" with nothing running for it.
func TestALocalForwardIsSwitchedOnUnlessAskedOtherwise(t *testing.T) {
	db := newLocalForwardDB(t, []models.Host{statusHost(1, true)}, nil)
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	off := false

	for _, tc := range []struct {
		localPort int
		enabled   *bool
		want      bool
		status    string
	}{
		{localPort: 15001, enabled: nil, want: true, status: localForwardStatusStopped},
		{localPort: 15002, enabled: &off, want: false, status: localForwardStatusOff},
	} {
		body, err := json.Marshal(models.LocalForwardRequest{LocalPort: tc.localPort, TargetIP: "127.0.0.1",
			TargetPort: 5432, Enabled: tc.enabled})
		if err != nil {
			t.Fatalf("failed to write the request: %v", err)
		}

		c, rec := localForwardRequest(t, http.MethodPost, "/api/host/1/local-forward", string(body), "1")

		err = h.CreateHostLocalForward(c)
		if err != nil {
			t.Fatalf("CreateHostLocalForward returned error: %v", err)
		}
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
		}

		got := readLocalForwardAnswer(t, rec)
		if got.Enabled != tc.want || got.Status != tc.status {
			t.Errorf("port %d: enabled=%v status=%q, want %v %q", tc.localPort, got.Enabled, got.Status,
				tc.want, tc.status)
		}
	}

	rows := storedLocalForwards(t, db)
	if len(rows) != 2 || !rows[0].Enabled || rows[1].Enabled {
		t.Errorf("stored = %+v, want the first on and the second off", rows)
	}
}

// TestAnUpdateKeepsWhetherALocalForwardIsOnUnlessItSays pins the enabled flag
// of a change: left out it keeps what is stored, and said it is stored.
func TestAnUpdateKeepsWhetherALocalForwardIsOnUnlessItSays(t *testing.T) {
	stored := storedLocalForward(1, 1, 15001)
	stored.Enabled = false

	db := newLocalForwardDB(t, []models.Host{statusHost(1, true)}, []models.LocalForward{stored})
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	on := true
	off := false

	for _, tc := range []struct {
		enabled *bool
		want    bool
		status  string
	}{
		{enabled: nil, want: false, status: localForwardStatusOff},
		{enabled: &on, want: true, status: localForwardStatusStopped},
		{enabled: nil, want: true, status: localForwardStatusStopped},
		{enabled: &off, want: false, status: localForwardStatusOff},
	} {
		body, err := json.Marshal(models.LocalForwardRequest{BindScope: models.BindScopeLoopback, LocalPort: 15001,
			TargetIP: "127.0.0.1", TargetPort: 5432, Enabled: tc.enabled})
		if err != nil {
			t.Fatalf("failed to write the request: %v", err)
		}

		c, rec := localForwardRequest(t, http.MethodPut, "/api/local-forward/1", string(body), "1")

		err = h.UpdateLocalForward(c)
		if err != nil {
			t.Fatalf("UpdateLocalForward returned error: %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
		}

		got := readLocalForwardAnswer(t, rec)
		rows := storedLocalForwards(t, db)

		if got.Enabled != tc.want || got.Status != tc.status || len(rows) != 1 || rows[0].Enabled != tc.want {
			t.Errorf("enabled sent %v: answered %v %q, stored %+v, want %v %q",
				tc.enabled, got.Enabled, got.Status, rows, tc.want, tc.status)
		}
	}
}

// TestTheStatusOfALocalForwardSaysWhatIsOff pins the order the three statuses
// that run nothing are given in: a disabled Host is named before a forward
// that is off, and a forward that is off is "off" whatever the manager says.
func TestTheStatusOfALocalForwardSaysWhatIsOff(t *testing.T) {
	on := storedLocalForward(1, 1, 15001)
	off := storedLocalForward(2, 1, 15002)
	off.Enabled = false

	states := map[uint]tunnel.LocalForwardState{
		15001: {Status: "connected"},
		15002: {Status: "connected"},
	}

	for _, tc := range []struct {
		lf          models.LocalForward
		hostEnabled bool
		want        string
	}{
		{lf: on, hostEnabled: true, want: "connected"},
		{lf: off, hostEnabled: true, want: localForwardStatusOff},
		{lf: on, hostEnabled: false, want: localForwardStatusDisabled},
		{lf: off, hostEnabled: false, want: localForwardStatusDisabled},
	} {
		got := localForwardViewOf(tc.lf, tc.hostEnabled, states)
		if got.Status != tc.want {
			t.Errorf("row %d with the Host enabled=%v: status = %q, want %q",
				tc.lf.Number, tc.hostEnabled, got.Status, tc.want)
		}
	}
}

// TestUpdateLocalForwardChangesTheRow pins an update, including one that keeps
// the local port the row already has: that is the row meeting itself and not
// another forward.
func TestUpdateLocalForwardChangesTheRow(t *testing.T) {
	db := newLocalForwardDB(t, []models.Host{statusHost(1, true)},
		[]models.LocalForward{storedLocalForward(1, 1, 15001)})
	manager := &wakeRecorder{tx: &txConnPool{}}
	h := NewHandler(db, manager, zap.NewNop(), newTestCipher(t))

	for _, body := range []string{
		`{"bind_scope":"wildcard","local_port":15001,"target_ip":"192.0.2.10","target_port":80}`,
		`{"bind_scope":"loopback","local_port":15009,"target_ip":"192.0.2.10","target_port":8080,"description":"web"}`,
	} {
		c, rec := localForwardRequest(t, http.MethodPut, "/api/local-forward/1", body, "1")

		err := h.UpdateLocalForward(c)
		if err != nil {
			t.Fatalf("UpdateLocalForward returned error: %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
		}
	}

	rows := storedLocalForwards(t, db)
	want := models.LocalForward{Number: 1, HostID: 1, BindScope: models.BindScopeLoopback, LocalPort: 15009,
		TargetIP: "192.0.2.10", TargetPort: 8080, Description: "web"}
	if len(rows) != 1 || rows[0].BindScope != want.BindScope || rows[0].LocalPort != want.LocalPort ||
		rows[0].TargetIP != want.TargetIP || rows[0].TargetPort != want.TargetPort ||
		rows[0].Description != want.Description || rows[0].HostID != want.HostID {
		t.Errorf("stored = %+v, want %+v", rows, want)
	}

	wakes, _ := manager.counts()
	if wakes != 2 {
		t.Errorf("reconcile wake-ups = %d, want 2", wakes)
	}
}

// TestDeleteLocalForwardRemovesTheRow pins a delete: the row is gone, the
// other rows are not, and the loop is woken.
func TestDeleteLocalForwardRemovesTheRow(t *testing.T) {
	db := newLocalForwardDB(t, []models.Host{statusHost(1, true)},
		[]models.LocalForward{storedLocalForward(1, 1, 15001), storedLocalForward(2, 1, 15002)})
	manager := &wakeRecorder{tx: &txConnPool{}}
	h := NewHandler(db, manager, zap.NewNop(), newTestCipher(t))

	c, rec := localForwardRequest(t, http.MethodDelete, "/api/local-forward/1", "", "1")

	err := h.DeleteLocalForward(c)
	if err != nil {
		t.Fatalf("DeleteLocalForward returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	rows := storedLocalForwards(t, db)
	if len(rows) != 1 || rows[0].Number != 2 {
		t.Errorf("stored = %+v, want row 2 alone", rows)
	}

	wakes, _ := manager.counts()
	if wakes != 1 {
		t.Errorf("reconcile wake-ups = %d, want 1", wakes)
	}
}

// TestGetLocalForwardAnswersTheRow pins a read of one row.
func TestGetLocalForwardAnswersTheRow(t *testing.T) {
	db := newLocalForwardDB(t, []models.Host{statusHost(1, true)},
		[]models.LocalForward{storedLocalForward(1, 1, 15001)})
	manager := &wakeRecorder{tx: &txConnPool{}, localStates: map[uint]tunnel.LocalForwardState{
		15001: {Status: "error", LastError: "connection refused"},
	}}
	h := NewHandler(db, manager, zap.NewNop(), newTestCipher(t))

	c, rec := localForwardRequest(t, http.MethodGet, "/api/local-forward/1", "", "1")

	err := h.GetLocalForward(c)
	if err != nil {
		t.Fatalf("GetLocalForward returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	got := readLocalForwardAnswer(t, rec)
	if got.Number != 1 || got.LocalPort != 15001 || got.Status != "error" || got.LastError != "connection refused" {
		t.Errorf("answer = %+v, want row 1 with the state the manager reports", got)
	}
}

// TestLocalForwardWritesAreRefused pins every refusal a write can meet, and
// that none of them writes a row or wakes the loop.
func TestLocalForwardWritesAreRefused(t *testing.T) {
	const valid = `"target_ip":"127.0.0.1","target_port":5432`

	tests := []struct {
		name   string
		method string
		value  string
		body   string
		status int
		code   errorCode
	}{
		{
			name:   "create on a local port another forward opens",
			method: http.MethodPost,
			value:  "1",
			body:   `{"local_port":15001,` + valid + `}`,
			status: http.StatusConflict,
			code:   errLocalForwardPortTaken,
		},
		{
			name:   "update onto a local port another forward opens",
			method: http.MethodPut,
			value:  "2",
			body:   `{"local_port":15001,` + valid + `}`,
			status: http.StatusConflict,
			code:   errLocalForwardPortTaken,
		},
		{
			name:   "create on the port of this server",
			method: http.MethodPost,
			value:  "1",
			body:   `{"local_port":19443,` + valid + `}`,
			status: http.StatusConflict,
			code:   errLocalForwardPortIsAPIPort,
		},
		{
			name:   "update onto the port of this server",
			method: http.MethodPut,
			value:  "1",
			body:   `{"local_port":19443,` + valid + `}`,
			status: http.StatusConflict,
			code:   errLocalForwardPortIsAPIPort,
		},
		{
			name:   "a scope that is neither word",
			method: http.MethodPost,
			value:  "1",
			body:   `{"bind_scope":"127.0.0.1","local_port":15100,` + valid + `}`,
			status: http.StatusBadRequest,
			code:   errRequestValidationFailed,
		},
		{
			name:   "a local port above the range",
			method: http.MethodPost,
			value:  "1",
			body:   `{"local_port":65536,` + valid + `}`,
			status: http.StatusBadRequest,
			code:   errRequestValidationFailed,
		},
		{
			name:   "a target port below the range",
			method: http.MethodPut,
			value:  "1",
			body:   `{"local_port":15100,"target_ip":"127.0.0.1","target_port":-1}`,
			status: http.StatusBadRequest,
			code:   errRequestValidationFailed,
		},
		{
			name:   "a target that is not an IP",
			method: http.MethodPost,
			value:  "1",
			body:   `{"local_port":15100,"target_ip":"db.example","target_port":5432}`,
			status: http.StatusBadRequest,
			code:   errRequestValidationFailed,
		},
		{
			name:   "create under a Host that is not there",
			method: http.MethodPost,
			value:  "9",
			body:   `{"local_port":15100,` + valid + `}`,
			status: http.StatusNotFound,
			code:   errHostNotFound,
		},
		{
			name:   "update of a forward that is not there",
			method: http.MethodPut,
			value:  "9",
			body:   `{"local_port":15100,` + valid + `}`,
			status: http.StatusNotFound,
			code:   errLocalForwardNotFound,
		},
		{
			name:   "delete of a forward that is not there",
			method: http.MethodDelete,
			value:  "9",
			status: http.StatusNotFound,
			code:   errLocalForwardNotFound,
		},
		{
			name:   "an id that is not a number",
			method: http.MethodPut,
			value:  "one",
			body:   `{"local_port":15100,` + valid + `}`,
			status: http.StatusBadRequest,
			code:   errLocalForwardIDInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := []models.LocalForward{storedLocalForward(1, 1, 15001), storedLocalForward(2, 1, 15002)}
			db := newLocalForwardDB(t, []models.Host{statusHost(1, true)}, before)
			manager := &wakeRecorder{tx: &txConnPool{}}
			h := NewHandler(db, manager, zap.NewNop(), newTestCipher(t))

			var call func(echo.Context) error
			target := "/api/local-forward/" + tt.value

			switch tt.method {
			case http.MethodPost:
				call = h.CreateHostLocalForward
				target = "/api/host/" + tt.value + "/local-forward"
			case http.MethodPut:
				call = h.UpdateLocalForward
			case http.MethodDelete:
				call = h.DeleteLocalForward
			}

			c, rec := localForwardRequest(t, tt.method, target, tt.body, tt.value)

			err := call(c)
			if err != nil {
				t.Fatalf("the handler returned error: %v", err)
			}
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, tt.status, rec.Body.String())
			}
			if code := readRefusalCode(t, rec); code != string(tt.code) {
				t.Errorf("error_code = %q, want %q", code, tt.code)
			}

			rows := storedLocalForwards(t, db)
			if len(rows) != len(before) {
				t.Fatalf("stored %d rows, want the %d there were", len(rows), len(before))
			}
			for i := range before {
				if rows[i].LocalPort != before[i].LocalPort || rows[i].TargetIP != before[i].TargetIP {
					t.Errorf("row %d = %+v, want it left as %+v", i, rows[i], before[i])
				}
			}

			wakes, _ := manager.counts()
			if wakes != 0 {
				t.Errorf("reconcile wake-ups = %d, want 0", wakes)
			}
		})
	}
}

// TestLocalForwardReadsAnswerNotFound pins the two reads against a row that is
// not there.
func TestLocalForwardReadsAnswerNotFound(t *testing.T) {
	db := newLocalForwardDB(t, []models.Host{statusHost(1, true)}, nil)
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	c, rec := localForwardRequest(t, http.MethodGet, "/api/host/9/local-forward", "", "9")

	err := h.ListHostLocalForwards(c)
	if err != nil {
		t.Fatalf("ListHostLocalForwards returned error: %v", err)
	}
	if rec.Code != http.StatusNotFound || readRefusalCode(t, rec) != string(errHostNotFound) {
		t.Errorf("list of a missing Host = %d %s, want %d %s", rec.Code, rec.Body.String(),
			http.StatusNotFound, errHostNotFound)
	}

	c, rec = localForwardRequest(t, http.MethodGet, "/api/local-forward/9", "", "9")

	err = h.GetLocalForward(c)
	if err != nil {
		t.Fatalf("GetLocalForward returned error: %v", err)
	}
	if rec.Code != http.StatusNotFound || readRefusalCode(t, rec) != string(errLocalForwardNotFound) {
		t.Errorf("read of a missing forward = %d %s, want %d %s", rec.Code, rec.Body.String(),
			http.StatusNotFound, errLocalForwardNotFound)
	}
}

// TestFreeLocalPortSkipsWhatIsHeld pins the port a refusal of api_port
// suggests: the first one after the port asked for that no forward opens and
// that is not the stored api_port, going on from 1024 past 65535.
func TestFreeLocalPortSkipsWhatIsHeld(t *testing.T) {
	tests := []struct {
		name    string
		ports   []int
		taken   int
		stored  int
		running int
		want    int
	}{
		{name: "the next port", ports: []int{15432}, taken: 15432, stored: 8888, want: 15433},
		{name: "a run of held ports", ports: []int{15432, 15433, 15434}, taken: 15432, stored: 8888, want: 15435},
		{name: "the stored api_port", ports: []int{15432, 15433}, taken: 15432, stored: 15434, want: 15435},
		{name: "past the top", ports: []int{65534, 65535}, taken: 65534, stored: 1024, want: 1025},
		{name: "the running port", ports: []int{15432}, taken: 15432, stored: 8888, running: 15433, want: 15434},
		{name: "the running port and the stored one", ports: []int{15432}, taken: 15432, stored: 15433,
			running: 15434, want: 15435},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			forwards := make([]models.LocalForward, 0, len(tt.ports))
			for i, port := range tt.ports {
				forwards = append(forwards, storedLocalForward(uint(i+1), 1, port))
			}

			db := newLocalForwardDB(t, []models.Host{statusHost(1, true)}, forwards)

			got, err := freeLocalPort(db, tt.taken, tt.stored, tt.running)
			if err != nil {
				t.Fatalf("freeLocalPort returned error: %v", err)
			}
			if got != tt.want {
				t.Errorf("freeLocalPort = %d, want %d", got, tt.want)
			}
		})
	}
}

// runningAPIPort is the port the tests below tell the handler this process
// listens on. It is apart from localForwardAPIPort, as it is when the stored
// port was taken at startup.
const runningAPIPort = 19500

// TestALocalPortOnTheRunningAPIPortIsRefused pins that a create or an update
// onto the port this process listens on is refused under the code the stored
// port is, with that port in the answer, and that with no running port told
// the same write goes through as it did before.
func TestALocalPortOnTheRunningAPIPortIsRefused(t *testing.T) {
	const body = `{"local_port":19500,"target_ip":"127.0.0.1","target_port":5432}`

	tests := []struct {
		name    string
		method  string
		running int
		status  int
	}{
		{name: "create on the running port", method: http.MethodPost, running: runningAPIPort,
			status: http.StatusConflict},
		{name: "update onto the running port", method: http.MethodPut, running: runningAPIPort,
			status: http.StatusConflict},
		{name: "create with no running port told", method: http.MethodPost, status: http.StatusCreated},
		{name: "update with no running port told", method: http.MethodPut, status: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newLocalForwardDB(t, []models.Host{statusHost(1, true)},
				[]models.LocalForward{storedLocalForward(1, 1, 15001)})
			h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))
			h.SetRunningAPIPort(tt.running)

			call := h.UpdateLocalForward
			target := "/api/local-forward/1"
			if tt.method == http.MethodPost {
				call = h.CreateHostLocalForward
				target = "/api/host/1/local-forward"
			}

			c, rec := localForwardRequest(t, tt.method, target, body, "1")

			err := call(c)
			if err != nil {
				t.Fatalf("the handler returned error: %v", err)
			}
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, tt.status, rec.Body.String())
			}

			if tt.status != http.StatusConflict {
				return
			}

			var answer struct {
				Code string    `json:"error_code"`
				Args errorArgs `json:"error_args"`
			}

			err = json.Unmarshal(rec.Body.Bytes(), &answer)
			if err != nil {
				t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
			}
			if answer.Code != string(errLocalForwardPortIsAPIPort) || answer.Args["local_port"] != "19500" {
				t.Errorf("error_code = %q, error_args = %v, want %q with local_port 19500", answer.Code,
					answer.Args, errLocalForwardPortIsAPIPort)
			}

			rows := storedLocalForwards(t, db)
			if len(rows) != 1 || rows[0].LocalPort != 15001 {
				t.Errorf("rows = %+v, want the one forward left on 15001", rows)
			}
		})
	}
}
