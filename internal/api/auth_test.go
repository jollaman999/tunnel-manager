package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/jollaman999/tunnel-manager/internal/auth"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// The passwords below are made up. They are never sent anywhere and no account
// is served with them.
const (
	testPassword      = "not-a-real-one-1" // hook:allow
	testOtherPassword = "not-a-real-one-2" // hook:allow
	testUsername      = "operator"
)

// newAccountStubDB answers every read of the account with user, and the host
// listing with one row, so a request that gets past the middleware can be told
// apart from one that does not by its status alone.
func newAccountStubDB(t *testing.T, user models.User) *gorm.DB {
	t.Helper()

	db, _ := newTxRecordingDB(t)

	err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		switch dest := tx.Statement.Dest.(type) {
		case *models.User:
			*dest = user
		case *[]models.Host:
			*dest = []models.Host{{ID: 1, IP: "192.0.2.1", Port: 22, User: "root", Enabled: true}}
		}
		tx.RowsAffected = 1
	})
	if err != nil {
		t.Fatalf("failed to replace the query callback: %v", err)
	}

	return db
}

// newTestAccount returns the account row a test logs in against, with the
// password hashed the way the startup hashes it.
func newTestAccount(t *testing.T, setupRequired bool) models.User {
	t.Helper()

	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("failed to hash the password: %v", err)
	}

	user := models.User{ID: 1, PasswordHash: hash, SetupRequired: setupRequired}
	if !setupRequired {
		user.Username = testUsername
	}

	return user
}

// newTestServer wires the routes the way main does: the session middleware on
// the /api group, then the login, the logout and one of the existing handlers.
func newTestServer(t *testing.T, user models.User) (*echo.Echo, *AuthHandler) {
	t.Helper()

	db := newAccountStubDB(t, user)

	e := echo.New()
	e.Validator = &testValidator{validator: validator.New()}

	authHandler := NewAuthHandler(db, zap.NewNop())
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	g := e.Group("/api")
	g.Use(authHandler.RequireSession())
	g.POST("/login", authHandler.Login)
	g.POST("/logout", authHandler.Logout)
	g.GET("/host", h.ListHosts)

	return e, authHandler
}

// testResponse is models.Response as a client reads it back.
type testResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Data    struct {
		SetupRequired bool `json:"setup_required"`
	} `json:"data"`
}

// do sends one request, with cookie attached when there is one, and returns the
// recorder.
func do(e *echo.Echo, method, target, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	if cookie != nil {
		req.AddCookie(cookie)
	}

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	return rec
}

// sessionCookieOf returns the session cookie the answer set, or nil.
func sessionCookieOf(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			return cookie
		}
	}

	return nil
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) testResponse {
	t.Helper()

	var resp testResponse

	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil {
		t.Fatalf("failed to decode the body %q: %v", rec.Body.String(), err)
	}

	return resp
}

// login logs in and returns the answer together with the session cookie it set.
func login(t *testing.T, e *echo.Echo, body string) (*httptest.ResponseRecorder, *http.Cookie) {
	t.Helper()

	rec := do(e, http.MethodPost, "/api/login", body, nil)

	return rec, sessionCookieOf(rec)
}

// TestApiIsRefusedWithoutASession pins down that the middleware covers the
// routes that were registered before it, which is every route the API had
// before there was a login.
func TestApiIsRefusedWithoutASession(t *testing.T) {
	e, _ := newTestServer(t, newTestAccount(t, true))

	rec := do(e, http.MethodGet, "/api/host", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}

// TestLoginBeforeTheSetupTakesThePasswordAlone covers the first login: the
// account has no username yet, so the password is all there is to send, and the
// answer has to say that the setup is still ahead.
func TestLoginBeforeTheSetupTakesThePasswordAlone(t *testing.T) {
	e, _ := newTestServer(t, newTestAccount(t, true))

	rec, cookie := login(t, e, `{"password":"`+testPassword+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if cookie == nil {
		t.Fatalf("no %s cookie was set, Set-Cookie: %v", sessionCookieName, rec.Result().Header["Set-Cookie"])
	}
	if cookie.Value == "" {
		t.Errorf("the session cookie carries no token")
	}
	if !cookie.HttpOnly {
		t.Errorf("the session cookie is not HttpOnly: %s", rec.Result().Header.Get("Set-Cookie"))
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want %v", cookie.SameSite, http.SameSiteLaxMode)
	}
	if cookie.Path != "/" {
		t.Errorf("path = %q, want /", cookie.Path)
	}
	// The test request is plain HTTP, and a Secure cookie would never come back
	// on it.
	if cookie.Secure {
		t.Errorf("the session cookie is Secure on a plain HTTP request")
	}

	// The token itself is a credential, so only the attributes are logged.
	_, attributes, _ := strings.Cut(rec.Result().Header.Get("Set-Cookie"), ";")
	t.Logf("Set-Cookie: %s=<token>;%s", sessionCookieName, attributes)

	resp := decode(t, rec)
	if !resp.Success {
		t.Errorf("success = false, body: %s", rec.Body.String())
	}
	if !resp.Data.SetupRequired {
		t.Errorf("data.setup_required = false, want true, body: %s", rec.Body.String())
	}
}

// TestFailedLoginsAreAnsweredTheSameWay pins down that the answer does not say
// which half of the credentials was wrong, and that no session is handed out.
func TestFailedLoginsAreAnsweredTheSameWay(t *testing.T) {
	tests := []struct {
		name          string
		setupRequired bool
		body          string
	}{
		{
			name:          "wrong password before the setup",
			setupRequired: true,
			body:          `{"password":"` + testOtherPassword + `"}`,
		},
		{
			name:          "wrong password",
			setupRequired: false,
			body:          `{"username":"` + testUsername + `","password":"` + testOtherPassword + `"}`,
		},
		{
			name:          "wrong username",
			setupRequired: false,
			body:          `{"username":"someone-else","password":"` + testPassword + `"}`,
		},
	}

	bodies := make(map[string]string)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _ := newTestServer(t, newTestAccount(t, tt.setupRequired))

			rec, cookie := login(t, e, tt.body)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
			}
			if cookie != nil {
				t.Errorf("a session cookie was set on a failed login")
			}

			resp := decode(t, rec)
			if resp.Success {
				t.Errorf("success = true on a failed login")
			}
			if resp.Error != invalidCredentialsMessage {
				t.Errorf("error = %q, want %q", resp.Error, invalidCredentialsMessage)
			}

			bodies[tt.name] = rec.Body.String()
		})
	}

	// The two failures that differ only in which half was wrong have to read
	// the same, or the answer tells which half was right.
	if bodies["wrong password"] != bodies["wrong username"] {
		t.Errorf("a wrong password and a wrong username are answered differently: %q and %q",
			bodies["wrong password"], bodies["wrong username"])
	}
}

// TestSessionBeforeTheSetupReachesOnlyTheSetup pins down the gate: the session
// is good, and everything but the setup and the logout is still refused.
func TestSessionBeforeTheSetupReachesOnlyTheSetup(t *testing.T) {
	e, _ := newTestServer(t, newTestAccount(t, true))

	_, cookie := login(t, e, `{"password":"`+testPassword+`"}`)
	if cookie == nil {
		t.Fatalf("the login set no session cookie")
	}

	rec := do(e, http.MethodGet, "/api/host", "", cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}

	resp := decode(t, rec)
	if !strings.Contains(resp.Error, "setup") {
		t.Errorf("error = %q, want it to say that the setup is not finished", resp.Error)
	}

	// /api/setup has no route yet, so what is observed is that the middleware
	// let the request through to the router, which answers 404 rather than 403.
	rec = do(e, http.MethodPost, "/api/setup", `{}`, cookie)
	if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
		t.Errorf("the middleware refused %s with %d", setupPath, rec.Code)
	}
	t.Logf("POST %s with a session from before the setup answered %d", setupPath, rec.Code)
}

// TestSessionAfterTheSetupReachesTheApi covers the ordinary case: a username
// and a password, and the session gets at the API.
func TestSessionAfterTheSetupReachesTheApi(t *testing.T) {
	e, _ := newTestServer(t, newTestAccount(t, false))

	rec, cookie := login(t, e, `{"username":"`+testUsername+`","password":"`+testPassword+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if cookie == nil {
		t.Fatalf("the login set no session cookie")
	}

	resp := decode(t, rec)
	if resp.Data.SetupRequired {
		t.Errorf("data.setup_required = true on an account that is set up")
	}

	rec = do(e, http.MethodGet, "/api/host", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestLogoutEndsTheSession pins down that the cookie a client keeps is worth
// nothing after the logout, and that a logout without a session is not an error.
func TestLogoutEndsTheSession(t *testing.T) {
	e, _ := newTestServer(t, newTestAccount(t, false))

	_, cookie := login(t, e, `{"username":"`+testUsername+`","password":"`+testPassword+`"}`)
	if cookie == nil {
		t.Fatalf("the login set no session cookie")
	}

	rec := do(e, http.MethodPost, "/api/logout", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	expired := sessionCookieOf(rec)
	if expired == nil || expired.MaxAge >= 0 {
		t.Errorf("the logout did not expire the cookie: %v", expired)
	}

	rec = do(e, http.MethodGet, "/api/host", "", cookie)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}

	// A client that has already lost its session still gets an answer it can
	// act on.
	rec = do(e, http.MethodPost, "/api/logout", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout without a session: status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestLoginIsNotBehindTheSessionCheck pins down that the middleware does not
// lock out the one request that exists to get past it.
func TestLoginIsNotBehindTheSessionCheck(t *testing.T) {
	e, _ := newTestServer(t, newTestAccount(t, false))

	rec := do(e, http.MethodPost, "/api/login", `{"username":"`+testUsername+`","password":"`+testPassword+`"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestAnExpiredSessionIsRefused moves the clock of the store past the lifetime
// rather than waiting for it, and pins down that the session is gone from the
// map afterwards instead of only being refused.
func TestAnExpiredSessionIsRefused(t *testing.T) {
	e, authHandler := newTestServer(t, newTestAccount(t, false))

	_, cookie := login(t, e, `{"username":"`+testUsername+`","password":"`+testPassword+`"}`)
	if cookie == nil {
		t.Fatalf("the login set no session cookie")
	}

	authHandler.sessions.now = func() time.Time {
		return time.Now().Add(sessionLifetime + time.Minute)
	}

	rec := do(e, http.MethodGet, "/api/host", "", cookie)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}

	authHandler.sessions.mu.RLock()
	left := len(authHandler.sessions.sessions)
	authHandler.sessions.mu.RUnlock()

	if left != 0 {
		t.Errorf("the expired session is still in the store: %d left", left)
	}
}

// TestTheSessionDeadlineSlides pins down that a session that keeps being used
// outlives the lifetime counted from the login.
func TestTheSessionDeadlineSlides(t *testing.T) {
	store := NewSessionStore()

	now := time.Now()
	store.now = func() time.Time { return now }

	token, err := store.Create(1)
	if err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}

	// Two lookups, each an hour before the deadline of the one before it.
	for i := 0; i < 2; i++ {
		now = now.Add(sessionLifetime - time.Hour)

		_, ok := store.Lookup(token)
		if !ok {
			t.Fatalf("the session was refused %v after the login", time.Duration(i+1)*(sessionLifetime-time.Hour))
		}
	}
}

// TestTheSessionStoreIsUsedFromManyGoroutines is here for the race detector.
func TestTheSessionStoreIsUsedFromManyGoroutines(t *testing.T) {
	store := NewSessionStore()

	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for j := 0; j < 50; j++ {
				token, err := store.Create(1)
				if err != nil {
					t.Errorf("Create returned an error: %v", err)
					return
				}

				_, _ = store.Lookup(token)
				store.Delete(token)
			}
		}()
	}

	wg.Wait()
}
