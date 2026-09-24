package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
		ID:         id,
		HostID:     hostID,
		BindScope:  models.BindScopeLoopback,
		LocalPort:  localPort,
		TargetIP:   "127.0.0.1",
		TargetPort: 5432,
	}
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

	err := db.Order("id").Find(&rows).Error
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
	if got.ID == 0 || got.HostID != 1 || got.LocalPort != 15432 || got.TargetIP != "127.0.0.1" ||
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
		1: {Status: "connected", RetryCount: 2, LastConnectedAt: connectedAt},
		3: {Status: "connected"},
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

	var answer struct {
		Success bool               `json:"success"`
		Data    []localForwardView `json:"data"`
	}

	err = json.Unmarshal(rec.Body.Bytes(), &answer)
	if err != nil {
		t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
	}

	if len(answer.Data) != 2 {
		t.Fatalf("the answer carries %d rows, want the 2 of Host 1, body: %s", len(answer.Data), rec.Body.String())
	}

	first := answer.Data[0]
	if first.ID != 1 || first.Status != "connected" || first.RetryCount != 2 || !first.LastConnectedAt.Equal(connectedAt) {
		t.Errorf("row 1 = %+v, want the state the manager reports", first)
	}
	if first.BindScope != models.BindScopeLoopback {
		t.Errorf("row 1 bind_scope = %q, want %q", first.BindScope, models.BindScopeLoopback)
	}

	if answer.Data[1].ID != 2 || answer.Data[1].Status != localForwardStatusStopped {
		t.Errorf("row 2 = %+v, want status %q", answer.Data[1], localForwardStatusStopped)
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

	answer.Data = nil

	err = json.Unmarshal(rec.Body.Bytes(), &answer)
	if err != nil {
		t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
	}

	if len(answer.Data) != 1 || answer.Data[0].Status != localForwardStatusDisabled {
		t.Errorf("the rows of the disabled Host = %+v, want one with status %q",
			answer.Data, localForwardStatusDisabled)
	}
}

// TestListHostLocalForwardsAnswersEmptyAsAnArray pins that a Host with no
// forwards is answered with an empty array and not a null.
func TestListHostLocalForwardsAnswersEmptyAsAnArray(t *testing.T) {
	db := newLocalForwardDB(t, []models.Host{statusHost(1, true)}, nil)
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	c, rec := localForwardRequest(t, http.MethodGet, "/api/host/1/local-forward", "", "1")

	err := h.ListHostLocalForwards(c)
	if err != nil {
		t.Fatalf("ListHostLocalForwards returned error: %v", err)
	}

	if !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Errorf("body = %s, want an empty array under data", rec.Body.String())
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
	want := models.LocalForward{ID: 1, HostID: 1, BindScope: models.BindScopeLoopback, LocalPort: 15009,
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
	if len(rows) != 1 || rows[0].ID != 2 {
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
		1: {Status: "error", LastError: "connection refused"},
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
	if got.ID != 1 || got.LocalPort != 15001 || got.Status != "error" || got.LastError != "connection refused" {
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
