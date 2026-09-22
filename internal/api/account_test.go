package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/auth"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// accountPath is the route the credentials are changed through.
const accountPath = "/api/account"

// accountProbePath is a route behind the session check that answers 200 and
// does nothing else. It is how a session that is still open is told from one
// that was ended: the status of a request it makes is the whole of what a
// client can see about it.
const accountProbePath = "/api/probe"

// accountNewUsername is the name an account is renamed to in these tests.
const accountNewUsername = "second-operator"

// accountFixture is the server wired the way main wires it, against a real
// database file.
//
// A file rather than one of the stubs in this package, the way the settings
// tests do it: what is under test is the row that is left behind, and the
// answer to that is the login that comes after the change rather than the
// statement the write went out as.
type accountFixture struct {
	e    *echo.Echo
	auth *AuthHandler
	db   *gorm.DB
	logs *observer.ObservedLogs

	// overTLS makes every request the fixture sends arrive over TLS this
	// process terminated itself. It is off by default, which is the install
	// served over plain HTTP, and a test that turns it on gets the cookie
	// names a browser guards: the session this endpoint has to find is then
	// under __Host-tm_session and not under tm_session.
	overTLS bool
}

func newAccountFixture(t *testing.T) *accountFixture {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "tunnel-manager.db")), &gorm.Config{
		Logger: gormlogger.Discard,
	})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	err = db.AutoMigrate(&models.User{})
	if err != nil {
		t.Fatalf("failed to migrate the database: %v", err)
	}

	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("failed to hash the password: %v", err)
	}

	err = db.Create(&models.User{Username: testUsername, PasswordHash: hash}).Error
	if err != nil {
		t.Fatalf("failed to write the account: %v", err)
	}

	// Every level is kept, because one of the tests reads the lines back
	// looking for a password in them and the line the change writes is an Info.
	core, logs := observer.New(zap.DebugLevel)

	e := echo.New()
	probe := &csrfProbe{}
	authHandler := NewAuthHandler(db, zap.New(core), "")

	g := e.Group("/api")
	g.Use(authHandler.RequireSession())
	g.POST("/login", authHandler.Login)
	g.POST("/logout", authHandler.Logout)
	g.GET("/account", authHandler.GetAccount)
	g.PUT("/account", authHandler.ChangeAccount)
	g.GET("/probe", probe.handle)

	return &accountFixture{e: e, auth: authHandler, db: db, logs: logs}
}

// signIn logs in with the credentials given and returns the cookies of the
// session, which is what a client holds afterwards.
func (f *accountFixture) signIn(t *testing.T, username, password string) []*http.Cookie {
	t.Helper()

	return csrfLoginCookiesOverTLS(t, f.e, f.overTLS, loginBody(t, username, password))
}

// signInFails sends a login that is expected to be refused and says what the
// status was, so a test can tell a refusal from a login that went through.
func (f *accountFixture) signInFails(t *testing.T, username, password string) int {
	t.Helper()

	rec, _ := login(t, f.e, loginBody(t, username, password))

	return rec.Code
}

// reachable reports the status a session gets on a route behind the session
// check. 200 is a session that is still open and 401 is one that is gone.
func (f *accountFixture) reachable(cookies []*http.Cookie) int {
	return doOverTLS(f.e, f.overTLS, http.MethodGet, accountProbePath, "", cookies...).Code
}

// storedUsername is the name in the row, read back out of the database rather
// than out of the answer.
func (f *accountFixture) storedUsername(t *testing.T) string {
	t.Helper()

	var user models.User

	err := f.db.First(&user).Error
	if err != nil {
		t.Fatalf("failed to read the account back: %v", err)
	}

	return user.Username
}

// loginBody is the body of a login, built the way a client builds it.
func loginBody(t *testing.T, username, password string) string {
	t.Helper()

	body, err := json.Marshal(loginRequest{Username: username, Password: password})
	if err != nil {
		t.Fatalf("failed to build the login body: %v", err)
	}

	return string(body)
}

// accountBody is the body of a change. A field left empty is one the request
// does not name, which is how a change of one value alone is sent.
func accountBody(t *testing.T, current, username, newPassword string) string {
	t.Helper()

	body, err := json.Marshal(accountRequest{
		CurrentPassword: current,
		Username:        username,
		NewPassword:     newPassword,
	})
	if err != nil {
		t.Fatalf("failed to build the account body: %v", err)
	}

	return string(body)
}

// changeAccount sends one change with the cookies of the session given.
func (f *accountFixture) changeAccount(t *testing.T, cookies []*http.Cookie,
	current, username, newPassword string) *httptest.ResponseRecorder {
	t.Helper()

	return doOverTLS(f.e, f.overTLS, http.MethodPut, accountPath,
		accountBody(t, current, username, newPassword), cookies...)
}

// TestGetAccountAnswersWithTheUsername covers what the screen draws the card
// from. The name is not written down anywhere the page can read after a reload,
// so it is asked for.
func TestGetAccountAnswersWithTheUsername(t *testing.T) {
	f := newAccountFixture(t)
	cookies := f.signIn(t, testUsername, testPassword)

	rec := do(f.e, http.MethodGet, accountPath, "", cookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Username string `json:"username"`
		} `json:"data"`
	}

	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil {
		t.Fatalf("failed to decode the body %q: %v", rec.Body.String(), err)
	}

	if !resp.Success {
		t.Errorf("success = false, body: %s", rec.Body.String())
	}
	if resp.Data.Username != testUsername {
		t.Errorf("username = %q, want %q", resp.Data.Username, testUsername)
	}
}

// TestAccountChangeNeedsTheCurrentPassword is the whole reason this is not the
// setup: a session that is open must not be enough to replace the credentials
// with. The username-only change is in the table because that is the case it
// would be easiest to leave the question out of.
func TestAccountChangeNeedsTheCurrentPassword(t *testing.T) {
	tests := []struct {
		name        string
		current     string
		username    string
		newPassword string
	}{
		{name: "no current password, renaming", username: accountNewUsername},
		{name: "no current password, new password", newPassword: testNewPassword},
		{
			name:     "wrong current password, renaming",
			current:  testOtherPassword,
			username: accountNewUsername,
		},
		{
			name:        "wrong current password, new password",
			current:     testOtherPassword,
			newPassword: testNewPassword,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAccountFixture(t)
			cookies := f.signIn(t, testUsername, testPassword)

			rec := f.changeAccount(t, cookies, tt.current, tt.username, tt.newPassword)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d, body: %s",
					rec.Code, http.StatusUnauthorized, rec.Body.String())
			}

			resp := decode(t, rec)
			if resp.Error != accountWrongPasswordMessage {
				t.Errorf("error = %q, want %q", resp.Error, accountWrongPasswordMessage)
			}

			// Nothing was changed, so the credentials that opened the account
			// still open it.
			if f.storedUsername(t) != testUsername {
				t.Errorf("username = %q, want %q", f.storedUsername(t), testUsername)
			}

			f.signIn(t, testUsername, testPassword)
		})
	}
}

// TestAccountChangesTheUsername covers the rename end to end: the new name
// opens the account afterwards and the old one does not.
func TestAccountChangesTheUsername(t *testing.T) {
	f := newAccountFixture(t)
	cookies := f.signIn(t, testUsername, testPassword)

	rec := f.changeAccount(t, cookies, testPassword, accountNewUsername, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if f.storedUsername(t) != accountNewUsername {
		t.Errorf("username = %q, want %q", f.storedUsername(t), accountNewUsername)
	}

	f.signIn(t, accountNewUsername, testPassword)

	code := f.signInFails(t, testUsername, testPassword)
	if code != http.StatusUnauthorized {
		t.Errorf("the old username: status = %d, want %d", code, http.StatusUnauthorized)
	}
}

// TestAccountChangesThePassword covers the other half: the new password opens
// the account afterwards and the old one does not.
func TestAccountChangesThePassword(t *testing.T) {
	f := newAccountFixture(t)
	cookies := f.signIn(t, testUsername, testPassword)

	rec := f.changeAccount(t, cookies, testPassword, "", testNewPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// The name was not named, so it is the one it was.
	if f.storedUsername(t) != testUsername {
		t.Errorf("username = %q, want %q", f.storedUsername(t), testUsername)
	}

	f.signIn(t, testUsername, testNewPassword)

	code := f.signInFails(t, testUsername, testPassword)
	if code != http.StatusUnauthorized {
		t.Errorf("the old password: status = %d, want %d", code, http.StatusUnauthorized)
	}
}

// TestAccountChangeEndsEveryOtherSession is the decision this endpoint was
// written around. The session that made the change goes on, every other one is
// gone, and the rename is in the table because it is the case the rule would
// most easily be left out of: the sessions point at the account and not at its
// name, so nothing about a rename ends them on its own.
func TestAccountChangeEndsEveryOtherSession(t *testing.T) {
	tests := []struct {
		name        string
		username    string
		newPassword string
	}{
		{name: "the username alone", username: accountNewUsername},
		{name: "the password alone", newPassword: testNewPassword},
		{name: "both", username: accountNewUsername, newPassword: testNewPassword},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAccountFixture(t)

			mine := f.signIn(t, testUsername, testPassword)
			other := f.signIn(t, testUsername, testPassword)

			rec := f.changeAccount(t, mine, testPassword, tt.username, tt.newPassword)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d, body: %s",
					rec.Code, http.StatusOK, rec.Body.String())
			}

			code := f.reachable(mine)
			if code != http.StatusOK {
				t.Errorf("the session that made the change: status = %d, want %d",
					code, http.StatusOK)
			}

			code = f.reachable(other)
			if code != http.StatusUnauthorized {
				t.Errorf("the other session: status = %d, want %d",
					code, http.StatusUnauthorized)
			}

			f.auth.sessions.mu.RLock()
			left := len(f.auth.sessions.sessions)
			f.auth.sessions.mu.RUnlock()

			if left != 1 {
				t.Errorf("%d sessions are left, want 1", left)
			}

			// How many went is in the answer, because it is the half of this
			// call that happens away from the screen it was made on.
			var resp struct {
				Data struct {
					Username        string `json:"username"`
					UsernameChanged bool   `json:"username_changed"`
					PasswordChanged bool   `json:"password_changed"`
					SessionsEnded   int    `json:"sessions_ended"`
				} `json:"data"`
			}

			err := json.Unmarshal(rec.Body.Bytes(), &resp)
			if err != nil {
				t.Fatalf("failed to decode the body %q: %v", rec.Body.String(), err)
			}

			if resp.Data.SessionsEnded != 1 {
				t.Errorf("sessions_ended = %d, want 1, body: %s",
					resp.Data.SessionsEnded, rec.Body.String())
			}
			if resp.Data.UsernameChanged != (tt.username != "") {
				t.Errorf("username_changed = %v, want %v",
					resp.Data.UsernameChanged, tt.username != "")
			}
			if resp.Data.PasswordChanged != (tt.newPassword != "") {
				t.Errorf("password_changed = %v, want %v",
					resp.Data.PasswordChanged, tt.newPassword != "")
			}
		})
	}
}

// TestAccountChangeKeepsTheSessionItWasMadeWith pins down that the session that
// is kept is kept whole: it goes on changing things with the token it already
// had, which is what lets the page that made the change draw what happened.
func TestAccountChangeKeepsTheSessionItWasMadeWith(t *testing.T) {
	f := newAccountFixture(t)
	cookies := f.signIn(t, testUsername, testPassword)

	rec := f.changeAccount(t, cookies, testPassword, accountNewUsername, testNewPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// The same cookies, the CSRF one among them, on a second state changing
	// request. A token that had been replaced would be refused with a 403 here.
	rec = f.changeAccount(t, cookies, testNewPassword, testUsername, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("the second change: status = %d, want %d, body: %s",
			rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestAccountChangeOverTLSKeepsTheSessionItWasMadeWith is the same rule as the
// two tests above on an install served over HTTPS, where the session cookie
// arrives under the guarded name.
//
// It is a test of its own because every other test here logs in over plain
// HTTP, where the name is the bare one. This endpoint reads the session out of
// the request to decide which session to keep, and a read that missed it would
// leave an empty token: DeleteAllExcept takes that as a session none of the
// open ones is, so the operator who just chose a new password would be logged
// out by the change they made, on the installs that are set up the safer way.
func TestAccountChangeOverTLSKeepsTheSessionItWasMadeWith(t *testing.T) {
	f := newAccountFixture(t)
	f.overTLS = true

	mine := f.signIn(t, testUsername, testPassword)
	other := f.signIn(t, testUsername, testPassword)

	// What makes this test the one it is. A login that handed out the bare
	// names would be testing the same thing as the tests above it.
	for _, cookie := range mine {
		if !strings.HasPrefix(cookie.Name, hostCookiePrefix) {
			t.Fatalf("the login over TLS set %q, want a name starting with %q",
				cookie.Name, hostCookiePrefix)
		}
	}

	rec := f.changeAccount(t, mine, testPassword, accountNewUsername, testNewPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	code := f.reachable(mine)
	if code != http.StatusOK {
		t.Errorf("the session that made the change: status = %d, want %d", code, http.StatusOK)
	}

	code = f.reachable(other)
	if code != http.StatusUnauthorized {
		t.Errorf("the other session: status = %d, want %d", code, http.StatusUnauthorized)
	}

	var resp struct {
		Data struct {
			SessionsEnded int `json:"sessions_ended"`
		} `json:"data"`
	}

	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil {
		t.Fatalf("failed to decode the body %q: %v", rec.Body.String(), err)
	}

	if resp.Data.SessionsEnded != 1 {
		t.Errorf("sessions_ended = %d, want 1, body: %s", resp.Data.SessionsEnded, rec.Body.String())
	}

	// The change went through with the credentials it was sent with, so the new
	// ones are what opens the account now.
	if f.signInFails(t, accountNewUsername, testNewPassword) != http.StatusOK {
		t.Errorf("the new credentials do not open the account")
	}
}

// TestAccountChangeNeedsTheCsrfToken covers the route being behind the check
// the middleware puts on everything that is not a GET.
func TestAccountChangeNeedsTheCsrfToken(t *testing.T) {
	f := newAccountFixture(t)
	cookies := f.signIn(t, testUsername, testPassword)

	// The session cookie alone. do puts the header on only when the CSRF cookie
	// is among the ones it is handed, which is what the page does with it.
	rec := do(f.e, http.MethodPut, accountPath,
		accountBody(t, testPassword, accountNewUsername, ""), cookies[0])
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d, body: %s",
			rec.Code, http.StatusForbidden, rec.Body.String())
	}

	if f.storedUsername(t) != testUsername {
		t.Errorf("username = %q, want %q", f.storedUsername(t), testUsername)
	}
}

// TestAccountRefusesAPasswordOutsideTheRange covers both ends of the range the
// setup holds. The counts are in bytes, which is what bcrypt reads.
func TestAccountRefusesAPasswordOutsideTheRange(t *testing.T) {
	tests := []struct {
		name        string
		newPassword string
	}{
		{name: "one byte short", newPassword: strings.Repeat("a", minPasswordBytes-1)},
		{name: "one byte long", newPassword: strings.Repeat("a", maxPasswordBytes+1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAccountFixture(t)
			cookies := f.signIn(t, testUsername, testPassword)

			rec := f.changeAccount(t, cookies, testPassword, "", tt.newPassword)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d, body: %s",
					rec.Code, http.StatusBadRequest, rec.Body.String())
			}

			t.Logf("%d bytes: %s", len(tt.newPassword), decode(t, rec).Error)

			// The password that was there still opens the account, and the
			// other session is still open: a refusal ends nothing.
			f.signIn(t, testUsername, testPassword)
		})
	}
}

// TestAccountRefusesARequestThatChangesNothing covers the request that names
// neither value. There is nothing to do for it, and answering it as a success
// would report a change that did not happen and sign the other clients out.
func TestAccountRefusesARequestThatChangesNothing(t *testing.T) {
	f := newAccountFixture(t)

	mine := f.signIn(t, testUsername, testPassword)
	other := f.signIn(t, testUsername, testPassword)

	rec := f.changeAccount(t, mine, testPassword, "", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s",
			rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	resp := decode(t, rec)
	if resp.Error != accountNothingToChangeMessage {
		t.Errorf("error = %q, want %q", resp.Error, accountNothingToChangeMessage)
	}

	code := f.reachable(other)
	if code != http.StatusOK {
		t.Errorf("the other session: status = %d, want %d", code, http.StatusOK)
	}
}

// TestAccountRefusesAnEmptyUsername covers the name that is sent and is nothing
// but space. It is a name being cleared rather than one being left alone, and
// the account cannot be left without one.
func TestAccountRefusesAnEmptyUsername(t *testing.T) {
	f := newAccountFixture(t)
	cookies := f.signIn(t, testUsername, testPassword)

	rec := f.changeAccount(t, cookies, testPassword, "   ", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body: %s",
			rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	t.Logf("the answer: %s", decode(t, rec).Error)

	if f.storedUsername(t) != testUsername {
		t.Errorf("username = %q, want %q", f.storedUsername(t), testUsername)
	}
}

// TestAccountStoresTheUsernameTrimmed covers the name that arrives with space
// around it. The login compares what was stored, so a name stored with a
// trailing space is one the operator cannot type back in.
func TestAccountStoresTheUsernameTrimmed(t *testing.T) {
	f := newAccountFixture(t)
	cookies := f.signIn(t, testUsername, testPassword)

	rec := f.changeAccount(t, cookies, testPassword, "  "+accountNewUsername+"\t", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if f.storedUsername(t) != accountNewUsername {
		t.Errorf("username = %q, want %q", f.storedUsername(t), accountNewUsername)
	}

	f.signIn(t, accountNewUsername, testPassword)
}

// TestAccountRefusesWhatTheAccountAlreadyHas covers the value that is already
// the stored one. Storing it again would change nothing while the answer said
// otherwise, and every other client would be signed out over it.
func TestAccountRefusesWhatTheAccountAlreadyHas(t *testing.T) {
	tests := []struct {
		name        string
		username    string
		newPassword string
	}{
		{name: "the username it has", username: testUsername},
		{name: "the password it has", newPassword: testPassword},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAccountFixture(t)

			mine := f.signIn(t, testUsername, testPassword)
			other := f.signIn(t, testUsername, testPassword)

			rec := f.changeAccount(t, mine, testPassword, tt.username, tt.newPassword)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d, body: %s",
					rec.Code, http.StatusBadRequest, rec.Body.String())
			}

			t.Logf("the answer: %s", decode(t, rec).Error)

			code := f.reachable(other)
			if code != http.StatusOK {
				t.Errorf("the other session: status = %d, want %d", code, http.StatusOK)
			}
		})
	}
}

// TestAccountKeepsThePasswordsOutOfTheAnswerAndTheLog pins down that neither
// password turns up in what is written. The log goes to the console as well as
// to a file that is kept and rotated, so a credential written there outlives
// every screen it was typed on, and an answer carrying one back is one a proxy
// or a browser cache may keep.
func TestAccountKeepsThePasswordsOutOfTheAnswerAndTheLog(t *testing.T) {
	tests := []struct {
		name    string
		current string
		status  int
	}{
		{name: "the change that was stored", current: testPassword, status: http.StatusOK},
		{
			name:    "the change that was refused",
			current: testOtherPassword,
			status:  http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAccountFixture(t)
			cookies := f.signIn(t, testUsername, testPassword)

			rec := f.changeAccount(t, cookies, tt.current, accountNewUsername, testNewPassword)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, tt.status, rec.Body.String())
			}

			t.Logf("the answer: %s", rec.Body.String())

			secrets := map[string]string{
				"the current password":                          testPassword,
				"the password that was sent as the current one": tt.current,
				"the new password":                              testNewPassword,
			}

			for what, secret := range secrets {
				if secret == "" {
					continue
				}

				if strings.Contains(rec.Body.String(), secret) {
					t.Errorf("the answer carries %s: %s", what, rec.Body.String())
				}

				for _, entry := range f.logs.All() {
					line := entry.Message

					for _, field := range entry.Context {
						line += " " + field.String
					}

					if strings.Contains(line, secret) {
						t.Errorf("a log line carries %s: %s", what, line)
					}
				}
			}

			// The hash is not in the answer either. It is what a guess at the
			// password would be checked against offline.
			var user models.User

			err := f.db.First(&user).Error
			if err != nil {
				t.Fatalf("failed to read the account back: %v", err)
			}

			if strings.Contains(rec.Body.String(), user.PasswordHash) {
				t.Errorf("the answer carries the password hash: %s", rec.Body.String())
			}
		})
	}
}

// TestAccountChangeIsRefusedWithoutASession covers the route being behind the
// login like everything else under /api.
func TestAccountChangeIsRefusedWithoutASession(t *testing.T) {
	f := newAccountFixture(t)

	rec := do(f.e, http.MethodPut, accountPath,
		accountBody(t, testPassword, accountNewUsername, ""), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d, body: %s",
			rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}
