package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/auth"
	"github.com/jollaman999/tunnel-manager/internal/install"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v5"
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

	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

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

func updateRequest(t *testing.T, target string, body string, withAccount bool) (*echo.Context, *httptest.ResponseRecorder) {
	t.Helper()

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if withAccount {
		leaveSessionOnContext(c, 1)
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

	// The screen learns it is on the development build from this field alone,
	// the version being the same string either way.
	if view.DevBuild != install.DevBuild {
		t.Errorf("dev_build = %v in a build where install.DevBuild is %v", view.DevBuild, install.DevBuild)
	}

	if !strings.Contains(rec.Body.String(), `"dev_build":`) {
		t.Errorf("the answer carries no dev_build for the screen to read: %s", rec.Body.String())
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
			h.Look(context.Background())

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
	call := &updateCall{
		latest:      install.Latest{Tag: "v3.7.5", Newer: true, Comparable: true},
		installable: true,
	}
	h, logs := call.handler(t, nil)
	h.Look(context.Background())

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
	call := &updateCall{
		latest:      install.Latest{Tag: "v3.7.5", Newer: true, Comparable: true},
		installable: true,
	}
	h, _ := call.handler(t, nil)
	h.Look(context.Background())

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

// TestAnInstallWithNothingNewerIsRefused holds a press to what the screen
// offers it on. The install takes whatever the newest release is, so a press
// that went through here would put the running release in place again or an
// older one over it, and take every tunnel down to do so.
func TestAnInstallWithNothingNewerIsRefused(t *testing.T) {
	tests := []struct {
		name  string
		found install.Latest
		err   error
		look  bool
	}{
		{"nothing has looked yet", install.Latest{Tag: "v3.7.5", Newer: true, Comparable: true}, nil, false},
		{"the same version", install.Latest{Tag: "v3.7.4", Newer: false, Comparable: true}, nil, true},
		{"older", install.Latest{Tag: "v3.7.3", Newer: false, Comparable: true}, nil, true},
		{"not comparable", install.Latest{Tag: "nightly", Newer: false, Comparable: false}, nil, true},
		{"the check failed", install.Latest{}, errors.New("no route to host"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			call := &updateCall{latest: tt.found, checkErr: tt.err, installable: true}
			h, _ := call.handler(t, nil)

			if tt.look {
				h.Look(context.Background())
			}

			c, rec := updateRequest(t, "/api/update/install",
				`{"password":"`+hostKeyAccountPassword+`"}`, true)

			err := h.InstallUpdate(c)
			if err != nil {
				t.Fatalf("InstallUpdate returned an error: %v", err)
			}

			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409, body: %s", rec.Code, rec.Body.String())
			}

			var code struct {
				Code string `json:"error_code"`
			}

			err = json.Unmarshal(rec.Body.Bytes(), &code)
			if err != nil {
				t.Fatalf("failed to read the answer: %v", err)
			}

			if code.Code != string(errUpdateNothingNewer) {
				t.Errorf("error_code = %q, want %q", code.Code, errUpdateNothingNewer)
			}

			if call.started != 0 {
				t.Errorf("the install was started %d times with nothing newer, want 0", call.started)
			}
		})
	}
}

// TestASecondInstallIsRefusedWhileTheFirstStarts sends two presses that meet.
// The first is held inside the start, which is where the install is handed
// over; the second arrives then. Two installs handed over at once each stop
// the service and copy over the other, and on a systemd machine the second
// collided with the unit of the first and failed while the press answered 200.
func TestASecondInstallIsRefusedWhileTheFirstStarts(t *testing.T) {
	var started atomic.Int32

	entered := make(chan struct{}, 2)
	release := make(chan struct{})

	recorded, _ := observer.New(zap.DebugLevel)
	h := NewUpdateHandler(zap.New(recorded), updateAccountDB(t), "3.7.4", true,
		func(ctx context.Context, version string) (install.Latest, error) {
			return install.Latest{Tag: "v3.7.5", Newer: true, Comparable: true}, nil
		},
		func() error {
			started.Add(1)
			entered <- struct{}{}
			<-release

			return nil
		})
	h.Look(context.Background())

	press := func() chan *httptest.ResponseRecorder {
		done := make(chan *httptest.ResponseRecorder, 1)

		go func() {
			e := echo.New()
			req := httptest.NewRequest(http.MethodPost, "/api/update/install",
				strings.NewReader(`{"password":"`+hostKeyAccountPassword+`"}`))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			leaveSessionOnContext(c, 1)

			err := h.InstallUpdate(c)
			if err != nil {
				rec.Code = -1
			}

			done <- rec
		}()

		return done
	}

	first := press()
	<-entered

	second := press()

	var secondRec *httptest.ResponseRecorder

	select {
	case secondRec = <-second:
	case <-entered:
		t.Error("the second press started an install while the first was being handed over")
	case <-time.After(10 * time.Second):
		t.Fatal("the second press neither answered nor started an install")
	}

	close(release)

	firstRec := <-first
	if secondRec == nil {
		secondRec = <-second
	}

	if firstRec.Code != http.StatusOK {
		t.Errorf("the first press answered %d, want 200. Body: %s", firstRec.Code, firstRec.Body.String())
	}

	if secondRec.Code != http.StatusConflict {
		t.Errorf("the second press answered %d, want 409. Body: %s", secondRec.Code, secondRec.Body.String())
	}

	if !strings.Contains(secondRec.Body.String(), `"error_code":"`+string(errUpdateInstallRunning)+`"`) {
		t.Errorf("the second press is not refused as an install running: %s", secondRec.Body.String())
	}

	if got := started.Load(); got != 1 {
		t.Errorf("the install was started %d times, want once", got)
	}
}

// pressInstall sends one press with the right password and answers the status
// and the error code it came back with.
func pressInstall(t *testing.T, h *UpdateHandler) (int, string) {
	t.Helper()

	c, rec := updateRequest(t, "/api/update/install", `{"password":"`+hostKeyAccountPassword+`"}`, true)

	err := h.InstallUpdate(c)
	if err != nil {
		t.Fatalf("InstallUpdate returned an error: %v", err)
	}

	var body struct {
		Code string `json:"error_code"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &body)

	return rec.Code, body.Code
}

// TestAnInstallThatEndedMakesWayForTheNext walks the flag through what brings
// it down. An install that went well ends this process and the flag with it;
// the ones that did not are what would otherwise leave every press after them
// refused until the service was restarted by hand.
func TestAnInstallThatEndedMakesWayForTheNext(t *testing.T) {
	call := &updateCall{
		latest:      install.Latest{Tag: "v3.7.5", Newer: true, Comparable: true},
		installable: true,
	}
	h, _ := call.handler(t, nil)
	h.Look(context.Background())

	status, _ := pressInstall(t, h)
	if status != http.StatusOK || call.started != 1 {
		t.Fatalf("the first press answered %d and started %d installs, want 200 and one", status, call.started)
	}

	// Handed over and not yet ended: the press and the loop are both refused,
	// and neither reaches start.
	status, code := pressInstall(t, h)
	if status != http.StatusConflict || code != string(errUpdateInstallRunning) {
		t.Errorf("a press while an install is out answered %d %q, want 409 %q", status, code, errUpdateInstallRunning)
	}

	err := h.StartInstall()
	if !errors.Is(err, install.ErrInstallRunning) {
		t.Errorf("the loop was not refused while an install is out: %v", err)
	}

	if call.started != 1 {
		t.Fatalf("the install was started %d times while one was out, want once", call.started)
	}

	// The install ended without the restart.
	h.InstallEnded("")

	// A start that fails brings the flag down on its own.
	call.startErr = errors.New("systemd-run is not where it was")

	status, code = pressInstall(t, h)
	if status != http.StatusInternalServerError || code != string(errUpdateInstallFailed) {
		t.Errorf("a start that failed answered %d %q, want 500 %q", status, code, errUpdateInstallFailed)
	}

	err = h.StartInstall()
	if err == nil {
		t.Error("the loop was told a failed start worked")
	}

	// An install that start found running, which this process did not start,
	// is answered as one that it did.
	call.startErr = install.ErrInstallRunning

	status, code = pressInstall(t, h)
	if status != http.StatusConflict || code != string(errUpdateInstallRunning) {
		t.Errorf("an install found running answered %d %q, want 409 %q", status, code, errUpdateInstallRunning)
	}

	call.startErr = nil

	status, _ = pressInstall(t, h)
	if status != http.StatusOK {
		t.Errorf("a press after the failed starts answered %d, want 200", status)
	}

	if call.started != 5 {
		t.Errorf("the install was started %d times, want 5: the first, the two that failed, the one found running and the last", call.started)
	}
}

// TestWhyAnInstallEndedIsAnsweredUntilTheNextStarts is the reason an install
// left, from the moment main reports it to the moment the next install is
// begun. Before the reason was kept, all the Update screen had to go on was a
// version that did not change, three minutes later.
func TestWhyAnInstallEndedIsAnsweredUntilTheNextStarts(t *testing.T) {
	call := &updateCall{
		latest:      install.Latest{Tag: "v3.7.5", Newer: true, Comparable: true},
		installable: true,
	}
	h, _ := call.handler(t, nil)
	h.Look(context.Background())

	read := func() (updateView, string) {
		t.Helper()

		c, rec := updateRequest(t, "/api/update", "", true)

		err := h.GetUpdate(c)
		if err != nil {
			t.Fatalf("GetUpdate returned an error: %v", err)
		}

		return decodeUpdate(t, rec), rec.Body.String()
	}

	why := "the update was not installed and nothing was stopped or replaced: the release was not used " +
		"(release v3.7.5 carries no SHA256SUMS.sig), and an update installs a release or nothing"

	status, _ := pressInstall(t, h)
	if status != http.StatusOK {
		t.Fatalf("the first press answered %d, want 200", status)
	}

	h.InstallEnded(why)

	view, _ := read()
	if view.InstallProblem != why {
		t.Errorf("install_problem = %q once the install ended, want %q", view.InstallProblem, why)
	}

	// Read again, it is still there: the screen may be opened after the press.
	view, _ = read()
	if view.InstallProblem != why {
		t.Errorf("install_problem = %q on a second read, want %q", view.InstallProblem, why)
	}

	status, _ = pressInstall(t, h)
	if status != http.StatusOK {
		t.Fatalf("the second press answered %d, want 200", status)
	}

	view, body := read()
	if view.InstallProblem != "" || strings.Contains(body, "install_problem") {
		t.Errorf("the reason of the install before stayed once the next began: %s", body)
	}

	// An install that left nothing says nothing.
	h.InstallEnded("")

	_, body = read()
	if strings.Contains(body, "install_problem") {
		t.Errorf("an install that left no reason is answered with one: %s", body)
	}

	// The loop begins an install the same way the press does.
	h.InstallEnded(why)

	err := h.StartInstall()
	if err != nil {
		t.Fatalf("the loop failed to start an install: %v", err)
	}

	view, _ = read()
	if view.InstallProblem != "" {
		t.Errorf("install_problem = %q once the loop began an install, want none", view.InstallProblem)
	}
}

// TestWhyAnInstallBeforeThisProcessEndedGivesWayToOneBegunHere is the reason
// read off the disk at startup, which may arrive once the lock of the install
// that left it is let go. An install begun from this process before then is the
// last one, and its own end is what the screen says.
func TestWhyAnInstallBeforeThisProcessEndedGivesWayToOneBegunHere(t *testing.T) {
	call := &updateCall{
		latest:      install.Latest{Tag: "v3.7.5", Newer: true, Comparable: true},
		installable: true,
	}

	read := func(h *UpdateHandler) string {
		t.Helper()

		c, rec := updateRequest(t, "/api/update", "", true)

		err := h.GetUpdate(c)
		if err != nil {
			t.Fatalf("GetUpdate returned an error: %v", err)
		}

		return decodeUpdate(t, rec).InstallProblem
	}

	before := "failed to register the service: systemctl daemon-reload: exit status 1"

	h, _ := call.handler(t, nil)
	h.InstallEndedBefore(before)

	if got := read(h); got != before {
		t.Errorf("install_problem = %q at startup, want %q", got, before)
	}

	h, _ = call.handler(t, nil)

	err := h.StartInstall()
	if err != nil {
		t.Fatalf("the loop failed to start an install: %v", err)
	}

	h.InstallEndedBefore(before)

	if got := read(h); got != "" {
		t.Errorf("install_problem = %q while an install begun here runs, want none", got)
	}

	h.InstallEnded("")
	h.InstallEndedBefore(before)

	if got := read(h); got != "" {
		t.Errorf("install_problem = %q once an install begun here ended with none, want none", got)
	}
}

// TestAStartThatPanickedMakesWayForTheNext is a start that never returned. The
// panic is the server's to answer, but the flag it left up would refuse every
// install after it, the press and the loop alike, until the service restarted.
func TestAStartThatPanickedMakesWayForTheNext(t *testing.T) {
	panics := true
	started := 0

	recorded, _ := observer.New(zap.DebugLevel)
	h := NewUpdateHandler(zap.New(recorded), updateAccountDB(t), "3.7.4", true,
		func(ctx context.Context, version string) (install.Latest, error) {
			return install.Latest{Tag: "v3.7.5", Newer: true, Comparable: true}, nil
		},
		func() error {
			started++

			if panics {
				panic("the start fell over")
			}

			return nil
		})
	h.Look(context.Background())

	mustPanic := func(name string, call func()) {
		t.Helper()

		defer func() {
			if recover() == nil {
				t.Errorf("%s did not pass the panic of the start on", name)
			}
		}()

		call()
	}

	mustPanic("the press", func() { _, _ = pressInstall(t, h) })
	mustPanic("the loop", func() { _ = h.StartInstall() })

	panics = false

	status, code := pressInstall(t, h)
	if status != http.StatusOK {
		t.Errorf("a press after the start panicked answered %d %q, want 200", status, code)
	}

	if started != 3 {
		t.Errorf("the start was reached %d times, want 3: both that panicked and the press after", started)
	}
}
