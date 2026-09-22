package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/auth"
	"github.com/jollaman999/tunnel-manager/internal/install"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"path/filepath"
)

// updateAccountDB is a database with the one account the password is checked
// against.
func updateAccountDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "update.db")),
		&gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	err = db.AutoMigrate(&models.User{})
	if err != nil {
		t.Fatalf("failed to migrate the database: %v", err)
	}

	hash, err := auth.HashPassword(hostKeyAccountPassword)
	if err != nil {
		t.Fatalf("failed to hash the password: %v", err)
	}

	err = db.Create(&models.User{Username: "operator", PasswordHash: hash}).Error
	if err != nil {
		t.Fatalf("failed to create the account: %v", err)
	}

	return db
}

// updateCall is one press or one read, with what the handler would reach the
// network and the installer through standing in.
type updateCall struct {
	latest      install.Latest
	checkErr    error
	installable bool
	startErr    error

	started int
}

func (call *updateCall) handler(t *testing.T, core zap.Option) (*UpdateHandler, *observer.ObservedLogs) {
	t.Helper()

	recorded, logs := observer.New(zap.DebugLevel)

	h := NewUpdateHandler(zap.New(recorded), updateAccountDB(t), "3.7.4", call.installable,
		func(ctx context.Context, version string) (install.Latest, error) {
			if call.checkErr != nil {
				return install.Latest{}, call.checkErr
			}

			return call.latest, nil
		},
		func() error {
			call.started++

			return call.startErr
		})

	return h, logs
}

func updateRequest(t *testing.T, target string, body string, withAccount bool) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if withAccount {
		c.Set(contextUserIDKey, uint(1))
	}

	return c, rec
}

func decodeUpdate(t *testing.T, rec *httptest.ResponseRecorder) updateView {
	t.Helper()

	var resp struct {
		Success bool       `json:"success"`
		Data    updateView `json:"data"`
	}

	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil {
		t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
	}

	return resp.Data
}

// TestTheUpdateScreenSaysNothingHasLookedYet keeps a screen that has never
// looked apart from one that looked and found nothing newer.
func TestTheUpdateScreenSaysNothingHasLookedYet(t *testing.T) {
	call := &updateCall{}
	h, _ := call.handler(t, nil)

	c, rec := updateRequest(t, "/api/update", "", true)

	err := h.GetUpdate(c)
	if err != nil {
		t.Fatalf("GetUpdate returned an error: %v", err)
	}

	view := decodeUpdate(t, rec)

	if view.Tag != "" || view.Newer || view.Comparable || view.Problem != "" {
		t.Errorf("a screen that has never looked reads as %+v", view)
	}

	if view.Version != "3.7.4" {
		t.Errorf("version = %q, want 3.7.4", view.Version)
	}

	if !view.Checked.IsZero() {
		t.Errorf("checked_at = %v, want the zero time", view.Checked)
	}
}

// TestAFailedCheckIsNotUpToDate is the distinction the whole screen rests on.
func TestAFailedCheckIsNotUpToDate(t *testing.T) {
	call := &updateCall{checkErr: errors.New("the latest release could not be read: no route to host")}
	h, _ := call.handler(t, nil)

	c, rec := updateRequest(t, "/api/update/check", "", true)

	err := h.CheckUpdate(c)
	if err != nil {
		t.Fatalf("CheckUpdate returned an error: %v", err)
	}

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body: %s", rec.Code, rec.Body.String())
	}

	if h.NewerAvailable() {
		t.Error("a check that failed was read as a newer release being available")
	}

	// What is kept is the failure, so the screen draws it rather than the
	// answer of whatever looked before.
	view := h.view()
	if view.Problem == "" {
		t.Error("the failure was not kept")
	}
}

// TestOnlyAComparableNewerReleaseIsInstallable is the gate an install runs on
// when nobody is asking for it.
func TestOnlyAComparableNewerReleaseIsInstallable(t *testing.T) {
	tests := []struct {
		name  string
		found install.Latest
		err   error
		want  bool
	}{
		{"newer and comparable", install.Latest{Tag: "v3.7.5", Newer: true, Comparable: true}, nil, true},
		{"the same version", install.Latest{Tag: "v3.7.4", Newer: false, Comparable: true}, nil, false},
		{"older", install.Latest{Tag: "v3.7.3", Newer: false, Comparable: true}, nil, false},
		// The one that matters. A tag nothing could compare must not read as
		// newer, or an install runs on a release nobody can order.
		{"not comparable", install.Latest{Tag: "nightly", Newer: false, Comparable: false}, nil, false},
		{"the check failed", install.Latest{}, errors.New("no route to host"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			call := &updateCall{latest: tt.found, checkErr: tt.err}
			h, _ := call.handler(t, nil)

			h.Look(context.Background())

			if got := h.NewerAvailable(); got != tt.want {
				t.Errorf("NewerAvailable() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAnInstallTakesThePasswordOfTheAccount walks the refusals and then the
// press that goes through.
func TestAnInstallTakesThePasswordOfTheAccount(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		installable bool
		status      int
		started     int
	}{
		{"no password", `{"password":""}`, true, http.StatusBadRequest, 0},
		{"the wrong password", `{"password":"not it"}`, true, http.StatusUnauthorized, 0},
		{"not installable", `{"password":"` + hostKeyAccountPassword + `"}`, false, http.StatusConflict, 0},
		{"the right password", `{"password":"` + hostKeyAccountPassword + `"}`, true, http.StatusOK, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			call := &updateCall{
				latest:      install.Latest{Tag: "v3.7.5", Newer: true, Comparable: true},
				installable: tt.installable,
			}
			h, logs := call.handler(t, nil)

			c, rec := updateRequest(t, "/api/update/install", tt.body, true)

			err := h.InstallUpdate(c)
			if err != nil {
				t.Fatalf("InstallUpdate returned an error: %v", err)
			}

			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, tt.status, rec.Body.String())
			}

			if call.started != tt.started {
				t.Errorf("the install was started %d times, want %d", call.started, tt.started)
			}

			// What was typed never reaches the log, on any of these paths.
			for _, line := range logs.All() {
				if strings.Contains(line.Message, "not it") {
					t.Errorf("a log line carries what was typed: %s", line.Message)
				}
			}
		})
	}
}

// TestAWrongPasswordOnAnInstallIsAnsweredByItsOwnCode keeps the screen from
// reading the refusal as a session that has ended.
func TestAWrongPasswordOnAnInstallIsAnsweredByItsOwnCode(t *testing.T) {
	call := &updateCall{installable: true}
	h, logs := call.handler(t, nil)

	c, rec := updateRequest(t, "/api/update/install", `{"password":"not it"}`, true)

	err := h.InstallUpdate(c)
	if err != nil {
		t.Fatalf("InstallUpdate returned an error: %v", err)
	}

	var code struct {
		Code string `json:"error_code"`
	}

	err = json.Unmarshal(rec.Body.Bytes(), &code)
	if err != nil {
		t.Fatalf("failed to read the answer: %v", err)
	}

	if code.Code != string(errUpdatePasswordWrong) {
		t.Errorf("error_code = %q, want %q", code.Code, errUpdatePasswordWrong)
	}

	if logs.FilterMessageSnippet("password that does not open the account").Len() != 1 {
		t.Error("the refusal was not written down")
	}
}

// TestAnInstallOutsideTheSessionMiddlewareIsAnError is the route hung somewhere
// the middleware does not cover. It must not be a press that goes through with
// no account behind it.
func TestAnInstallOutsideTheSessionMiddlewareIsAnError(t *testing.T) {
	call := &updateCall{installable: true}
	h, _ := call.handler(t, nil)

	c, rec := updateRequest(t, "/api/update/install",
		`{"password":"`+hostKeyAccountPassword+`"}`, false)

	err := h.InstallUpdate(c)
	if err != nil {
		t.Fatalf("InstallUpdate returned an error: %v", err)
	}

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body: %s", rec.Code, rec.Body.String())
	}

	if call.started != 0 {
		t.Error("the install was started with no account on the context")
	}
}
