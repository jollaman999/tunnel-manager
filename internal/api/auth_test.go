package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/jollaman999/tunnel-manager/internal/auth"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// The passwords below are made up. They are never sent anywhere and no account
// is served with them.
const (
	testPassword      = "not-a-real-one-1" // hook:allow
	testOtherPassword = "not-a-real-one-2" // hook:allow
	testNewPassword   = "not-a-real-one-3" // hook:allow
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

// newTestServer wires the routes against an account that is only read, and an
// initial password file that is not there, which is every test but the ones
// that watch the setup write.
func newTestServer(t *testing.T, user models.User) (*echo.Echo, *AuthHandler) {
	t.Helper()

	return newServer(t, newAccountStubDB(t, user), zap.NewNop(),
		filepath.Join(t.TempDir(), "initial-password"))
}

// newServer wires the routes the way main does: the session middleware on the
// /api group, then the login, the logout, the setup and one of the existing
// handlers.
func newServer(t *testing.T, db *gorm.DB, logger *zap.Logger, passwordFile string) (*echo.Echo, *AuthHandler) {
	t.Helper()

	e := echo.New()
	e.Validator = &testValidator{validator: validator.New()}

	authHandler := NewAuthHandler(db, logger, passwordFile)
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	g := e.Group("/api")
	g.Use(authHandler.RequireSession())
	g.POST("/login", authHandler.Login)
	g.POST("/logout", authHandler.Logout)
	g.POST("/setup", authHandler.Setup)
	g.GET("/host", h.ListHosts)

	return e, authHandler
}

// testResponse is models.Response as a client reads it back.
type testResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Data    struct {
		SetupRequired bool   `json:"setup_required"`
		CSRFToken     string `json:"csrf_token"`
	} `json:"data"`
}

// do sends one request, with the cookies attached that were handed in, and
// returns the recorder. A CSRF cookie among them is put in the header as well,
// which is what the page does with it: a test that wants a request to arrive
// without a token leaves that cookie out and sends the session one alone.
func do(e *echo.Echo, method, target, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	for _, cookie := range cookies {
		if cookie == nil {
			continue
		}

		req.AddCookie(cookie)

		if cookie.Name == csrfCookieName {
			req.Header.Set(csrfHeaderName, cookie.Value)
		}
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

// csrfCookieOf returns the CSRF cookie the answer set, or nil.
func csrfCookieOf(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == csrfCookieName {
			return cookie
		}
	}

	return nil
}

// csrfLoginCookies logs in and returns both cookies the answer set: the session
// and the CSRF token that belongs to it. That pair is what a client holds after
// a login, and a request that changes something has to carry both.
func csrfLoginCookies(t *testing.T, e *echo.Echo, body string) []*http.Cookie {
	t.Helper()

	rec, session := login(t, e, body)
	if session == nil {
		t.Fatalf("the login set no session cookie, body: %s", rec.Body.String())
	}

	csrf := csrfCookieOf(rec)
	if csrf == nil {
		t.Fatalf("the login set no %s cookie, Set-Cookie: %v", csrfCookieName, rec.Result().Header["Set-Cookie"])
	}

	return []*http.Cookie{session, csrf}
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

	cookies := csrfLoginCookies(t, e, `{"password":"`+testPassword+`"}`)

	rec := do(e, http.MethodGet, "/api/host", "", cookies...)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}

	resp := decode(t, rec)
	if !strings.Contains(resp.Error, "setup") {
		t.Errorf("error = %q, want it to say that the setup is not finished", resp.Error)
	}

	// The setup is the one path the gate lets through. The body is empty, so
	// the handler behind it answers 400, and what is observed here is that the
	// answer came from the handler rather than from the gate.
	rec = do(e, http.MethodPost, setupPath, `{}`, cookies...)
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

	cookies := csrfLoginCookies(t, e, `{"username":"`+testUsername+`","password":"`+testPassword+`"}`)

	rec := do(e, http.MethodPost, "/api/logout", "", cookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	expired := sessionCookieOf(rec)
	if expired == nil || expired.MaxAge >= 0 {
		t.Errorf("the logout did not expire the cookie: %v", expired)
	}

	// The CSRF cookie is set twice on this answer: the middleware writes it
	// again on the way in, and the handler expires it. The last one is the one
	// the browser is left holding.
	var lastCSRF *http.Cookie

	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == csrfCookieName {
			lastCSRF = cookie
		}
	}

	if lastCSRF == nil || lastCSRF.MaxAge >= 0 {
		t.Errorf("the logout did not expire the %s cookie: %v", csrfCookieName, lastCSRF)
	}

	rec = do(e, http.MethodGet, "/api/host", "", cookies...)
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

	token, _, err := store.Create(1)
	if err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}

	// Two lookups, each an hour before the deadline of the one before it.
	for i := 0; i < 2; i++ {
		now = now.Add(sessionLifetime - time.Hour)

		_, _, ok := store.Lookup(token)
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
				token, _, err := store.Create(1)
				if err != nil {
					t.Errorf("Create returned an error: %v", err)
					return
				}

				_, _, _ = store.Lookup(token)
				store.Delete(token)
			}
		}()
	}

	wg.Wait()
}

// accountRead is one read of the account row: whether it was issued on the
// transaction rather than on the root connection, and whether it locked the row.
type accountRead struct {
	inTransaction   bool
	lockedForUpdate bool
}

// setupStub is the account the setup runs against together with what the
// transaction did to it. Reads are served from user, the row the handler writes
// is held in pending, and the commit is what moves one to the other, so a login
// that comes after a commit that failed still finds the account that was there
// before.
type setupStub struct {
	mu         sync.Mutex
	user       models.User
	pending    *models.User
	reads      []accountRead
	statements []string
	commits    int
	rollbacks  int
	commitErr  error
	// txPool is the connection the transaction is served on. It is set once,
	// before the first request, and only compared against afterwards.
	txPool gorm.ConnPool
}

func (s *setupStub) account() models.User {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.user
}

func (s *setupStub) accountReads() []accountRead {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]accountRead(nil), s.reads...)
}

func (s *setupStub) recordRead(read accountRead) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.reads = append(s.reads, read)
}

func (s *setupStub) statementsRun() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.statements...)
}

func (s *setupStub) recordStatement(query string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.statements = append(s.statements, query)
}

func (s *setupStub) recordWrite(user models.User) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pending = &user
}

// failCommit makes every commit from here on report err.
func (s *setupStub) failCommit(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.commitErr = err
}

func (s *setupStub) commit() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.commitErr != nil {
		return s.commitErr
	}

	s.commits++

	if s.pending != nil {
		s.user = *s.pending
		s.pending = nil
	}

	return nil
}

func (s *setupStub) rollback() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.rollbacks++
	s.pending = nil

	return nil
}

// counts reports how often the transaction was committed and rolled back.
func (s *setupStub) counts() (commits, rollbacks int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.commits, s.rollbacks
}

// setupTxPool stands in for the transaction the setup writes in. It keeps the
// statements it is handed, so a test can read off which columns the write
// touched, and its commit fails when the stub is told to fail it.
type setupTxPool struct {
	stub *setupStub
}

func (p *setupTxPool) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return nil, errQueryFailed
}

func (p *setupTxPool) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	p.stub.recordStatement(query)

	return fakeResult{}, nil
}

func (p *setupTxPool) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return nil, errQueryFailed
}

func (p *setupTxPool) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return &sql.Row{}
}

func (p *setupTxPool) Commit() error {
	return p.stub.commit()
}

func (p *setupTxPool) Rollback() error {
	return p.stub.rollback()
}

// setupRootPool hands out the transaction on Begin and fails every statement of
// its own, so a write that was issued outside the transaction cannot pass for
// one that was issued inside it.
type setupRootPool struct {
	tx *setupTxPool
}

func (p *setupRootPool) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return nil, errQueryFailed
}

func (p *setupRootPool) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return nil, errQueryFailed
}

func (p *setupRootPool) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return nil, errQueryFailed
}

func (p *setupRootPool) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return &sql.Row{}
}

func (p *setupRootPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	return p.tx, nil
}

// setupFixture is a server wired the way main wires it, with the account row in
// memory, an initial password file that a test can put where it wants it, and
// the log the handler wrote to.
type setupFixture struct {
	e            *echo.Echo
	auth         *AuthHandler
	stub         *setupStub
	passwordFile string
	logs         *observer.ObservedLogs
}

// newSetupFixture returns a fixture whose account is in the given state. The
// initial password file is not created: a test that wants one calls
// writeInitialPassword, which is how the file being there and not being there
// are both covered.
func newSetupFixture(t *testing.T, setupRequired bool) *setupFixture {
	t.Helper()

	return newSetupFixtureAt(t, setupRequired, filepath.Join(t.TempDir(), "initial-password"))
}

func newSetupFixtureAt(t *testing.T, setupRequired bool, passwordFile string) *setupFixture {
	t.Helper()

	stub := &setupStub{user: newTestAccount(t, setupRequired)}
	txPool := &setupTxPool{stub: stub}
	stub.txPool = txPool

	db, err := gorm.Open(mysql.New(mysql.Config{
		Conn:                      &setupRootPool{tx: txPool},
		SkipInitializeWithVersion: true,
	}), &gorm.Config{
		Logger:               gormlogger.Discard,
		DisableAutomaticPing: true,
	})
	if err != nil {
		t.Fatalf("failed to open gorm with the setup conn pool: %v", err)
	}

	err = db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		switch dest := tx.Statement.Dest.(type) {
		case *models.User:
			_, locked := tx.Statement.Clauses["FOR"]
			stub.recordRead(accountRead{
				inTransaction:   tx.Statement.ConnPool == stub.txPool,
				lockedForUpdate: locked,
			})
			*dest = stub.account()
		case *[]models.Host:
			*dest = []models.Host{{ID: 1, IP: "192.0.2.1", Port: 22, User: "root", Enabled: true}}
		}
		tx.RowsAffected = 1
	})
	if err != nil {
		t.Fatalf("failed to replace the query callback: %v", err)
	}

	// The row the update carried is taken after the statement has been built and
	// run, so the statement the pool recorded and the values it carried are both
	// observed.
	err = db.Callback().Update().After("gorm:update").Register("test:record_account", func(tx *gorm.DB) {
		user, ok := tx.Statement.Dest.(*models.User)
		if ok {
			stub.recordWrite(*user)
		}
	})
	if err != nil {
		t.Fatalf("failed to register the update callback: %v", err)
	}

	core, logs := observer.New(zap.ErrorLevel)

	e, authHandler := newServer(t, db, zap.New(core), passwordFile)

	return &setupFixture{e: e, auth: authHandler, stub: stub, passwordFile: passwordFile, logs: logs}
}

// writeInitialPassword puts a file where the startup would have left the initial
// password, so that its removal can be watched.
func writeInitialPassword(t *testing.T, path string) {
	t.Helper()

	err := os.WriteFile(path, []byte(testPassword+"\n"), 0600)
	if err != nil {
		t.Fatalf("failed to write the initial password file: %v", err)
	}
}

// initialPasswordFileIsGone reports whether the file is no longer there.
func initialPasswordFileIsGone(t *testing.T, path string) bool {
	t.Helper()

	_, err := os.Stat(path)
	if err == nil {
		return false
	}
	if !os.IsNotExist(err) {
		t.Fatalf("failed to stat the initial password file: %v", err)
	}

	return true
}

// setupBody is the request body of the setup, built the way a client builds it.
func setupBody(t *testing.T, username, password string) string {
	t.Helper()

	body, err := json.Marshal(setupRequest{Username: username, Password: password})
	if err != nil {
		t.Fatalf("failed to build the setup body: %v", err)
	}

	return string(body)
}

// loginBeforeTheSetup logs in with the initial password and returns the cookies
// of the session it got.
func loginBeforeTheSetup(t *testing.T, f *setupFixture) []*http.Cookie {
	t.Helper()

	return csrfLoginCookies(t, f.e, `{"password":"`+testPassword+`"}`)
}

// TestSetupWritesTheAccountInOneTransaction covers the whole of the good case:
// the row is read locked inside the transaction, the three columns go out in one
// statement that is committed once, and the initial password file is gone
// afterwards.
func TestSetupWritesTheAccountInOneTransaction(t *testing.T) {
	f := newSetupFixture(t, true)
	writeInitialPassword(t, f.passwordFile)

	cookies := loginBeforeTheSetup(t, f)

	rec := do(f.e, http.MethodPost, setupPath, setupBody(t, testUsername, testNewPassword), cookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	resp := decode(t, rec)
	if !resp.Success {
		t.Errorf("success = false, body: %s", rec.Body.String())
	}

	account := f.stub.account()
	if account.Username != testUsername {
		t.Errorf("username = %q, want %q", account.Username, testUsername)
	}
	if account.SetupRequired {
		t.Errorf("setup_required = true after the setup")
	}
	if !auth.CheckPassword(account.PasswordHash, testNewPassword) {
		t.Errorf("the stored hash is not the one of the password that was sent")
	}

	commits, rollbacks := f.stub.counts()
	if commits != 1 || rollbacks != 0 {
		t.Errorf("commits = %d, rollbacks = %d, want 1 and 0", commits, rollbacks)
	}

	// The middleware reads the account outside the transaction on every request,
	// so the read the setup made is the last one.
	reads := f.stub.accountReads()
	if len(reads) == 0 {
		t.Fatalf("the account was never read")
	}

	last := reads[len(reads)-1]
	if !last.inTransaction || !last.lockedForUpdate {
		t.Errorf("the setup read the account with in_transaction = %v, locked = %v, want both true",
			last.inTransaction, last.lockedForUpdate)
	}

	statements := f.stub.statementsRun()
	if len(statements) != 1 {
		t.Fatalf("the transaction ran %d statements, want 1: %v", len(statements), statements)
	}

	for _, column := range []string{"username", "password_hash", "setup_required"} {
		if !strings.Contains(statements[0], column) {
			t.Errorf("the write does not carry %s: %s", column, statements[0])
		}
	}

	t.Logf("the setup ran %q and committed %d time(s)", statements[0], commits)

	if !initialPasswordFileIsGone(t, f.passwordFile) {
		t.Errorf("the initial password file %s is still there after the setup", f.passwordFile)
	}
}

// TestLoginAfterTheSetupTakesTheNewCredentials pins down what the setup was for:
// the password that was chosen opens the account and the one it replaced does
// not.
func TestLoginAfterTheSetupTakesTheNewCredentials(t *testing.T) {
	f := newSetupFixture(t, true)
	writeInitialPassword(t, f.passwordFile)

	cookies := loginBeforeTheSetup(t, f)

	rec := do(f.e, http.MethodPost, setupPath, setupBody(t, testUsername, testNewPassword), cookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	rec, newCookie := login(t, f.e, `{"username":"`+testUsername+`","password":"`+testNewPassword+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login with the new password: status = %d, want %d, body: %s",
			rec.Code, http.StatusOK, rec.Body.String())
	}
	if newCookie == nil {
		t.Errorf("the login with the new password set no session cookie")
	}

	resp := decode(t, rec)
	if resp.Data.SetupRequired {
		t.Errorf("data.setup_required = true after the setup")
	}

	rec, staleCookie := login(t, f.e, `{"username":"`+testUsername+`","password":"`+testPassword+`"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login with the initial password: status = %d, want %d, body: %s",
			rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
	if staleCookie != nil {
		t.Errorf("the login with the initial password handed out a session")
	}
}

// TestSetupIsRefusedOnceItIsDone pins down that the setup is not a way to change
// the credentials afterwards. It asks for no current password, so a session that
// was left open would otherwise be enough to take the account over.
func TestSetupIsRefusedOnceItIsDone(t *testing.T) {
	f := newSetupFixture(t, false)
	writeInitialPassword(t, f.passwordFile)

	cookies := csrfLoginCookies(t, f.e, `{"username":"`+testUsername+`","password":"`+testPassword+`"}`)

	rec := do(f.e, http.MethodPost, setupPath, setupBody(t, "someone-else", testNewPassword), cookies...)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}

	resp := decode(t, rec)
	if resp.Error != setupAlreadyDoneMessage {
		t.Errorf("error = %q, want %q", resp.Error, setupAlreadyDoneMessage)
	}

	account := f.stub.account()
	if account.Username != testUsername || !auth.CheckPassword(account.PasswordHash, testPassword) {
		t.Errorf("the refused setup changed the account: username = %q", account.Username)
	}

	commits, rollbacks := f.stub.counts()
	if commits != 0 || rollbacks != 1 {
		t.Errorf("commits = %d, rollbacks = %d, want 0 and 1", commits, rollbacks)
	}
}

// TestSetupRefusesWhatItCannotStore covers the input the setup turns away, and
// pins down that a refused setup leaves the initial password file where it is:
// that password is still the only one that opens the account.
func TestSetupRefusesWhatItCannotStore(t *testing.T) {
	tests := []struct {
		name     string
		username string
		password string
		want     string
	}{
		{
			name:     "no username",
			username: "",
			password: testNewPassword,
			want:     "empty",
		},
		{
			name:     "username of spaces alone",
			username: "   ",
			password: testNewPassword,
			want:     "empty",
		},
		{
			name:     "password one byte short",
			username: testUsername,
			password: strings.Repeat("x", minPasswordBytes-1),
			want:     "at least",
		},
		{
			name:     "password one byte long",
			username: testUsername,
			password: strings.Repeat("x", maxPasswordBytes+1),
			want:     "at most",
		},
		{
			// 25 Hangul syllables are 75 bytes, which bcrypt would cut short
			// even though it is well under 72 characters.
			name:     "password over the limit in bytes but not in characters",
			username: testUsername,
			password: strings.Repeat("가", 25),
			want:     "at most",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSetupFixture(t, true)
			writeInitialPassword(t, f.passwordFile)

			cookies := loginBeforeTheSetup(t, f)

			rec := do(f.e, http.MethodPost, setupPath, setupBody(t, tt.username, tt.password), cookies...)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}

			resp := decode(t, rec)
			if !strings.Contains(resp.Error, tt.want) {
				t.Errorf("error = %q, want it to say %q", resp.Error, tt.want)
			}
			t.Logf("%d bytes of password, %d of username: %s", len(tt.password), len(tt.username), resp.Error)

			if initialPasswordFileIsGone(t, f.passwordFile) {
				t.Errorf("the refused setup removed the initial password file")
			}

			commits, _ := f.stub.counts()
			if commits != 0 {
				t.Errorf("the refused setup committed %d time(s)", commits)
			}

			if !f.stub.account().SetupRequired {
				t.Errorf("the refused setup took the setup gate down")
			}
		})
	}
}

// TestSetupTakesThePasswordAtEitherEndOfTheRange pins down that the limits
// themselves are allowed, so the message a client is given names the length it
// can actually send.
func TestSetupTakesThePasswordAtEitherEndOfTheRange(t *testing.T) {
	for _, length := range []int{minPasswordBytes, maxPasswordBytes} {
		t.Run(strconv.Itoa(length)+" bytes", func(t *testing.T) {
			f := newSetupFixture(t, true)
			writeInitialPassword(t, f.passwordFile)

			cookies := loginBeforeTheSetup(t, f)
			password := strings.Repeat("x", length)

			rec := do(f.e, http.MethodPost, setupPath, setupBody(t, testUsername, password), cookies...)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
			}
		})
	}
}

// TestSetupStoresTheUsernameTrimmed pins down that the name is stored the way
// the login compares it.
func TestSetupStoresTheUsernameTrimmed(t *testing.T) {
	f := newSetupFixture(t, true)
	writeInitialPassword(t, f.passwordFile)

	cookies := loginBeforeTheSetup(t, f)

	rec := do(f.e, http.MethodPost, setupPath, setupBody(t, "  "+testUsername+"  ", testNewPassword), cookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	account := f.stub.account()
	if account.Username != testUsername {
		t.Errorf("username = %q, want %q", account.Username, testUsername)
	}

	rec, _ = login(t, f.e, `{"username":"`+testUsername+`","password":"`+testNewPassword+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestSetupWithoutASessionIsRefused pins down that the setup is behind the
// login like everything else. The gate lets the path through, the session check
// does not.
func TestSetupWithoutASessionIsRefused(t *testing.T) {
	f := newSetupFixture(t, true)
	writeInitialPassword(t, f.passwordFile)

	rec := do(f.e, http.MethodPost, setupPath, setupBody(t, testUsername, testNewPassword), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}

	if initialPasswordFileIsGone(t, f.passwordFile) {
		t.Errorf("a setup that was never authenticated removed the initial password file")
	}
}

// TestSetupThatCannotCommitKeepsTheInitialPassword is why the file is removed
// after the commit and not before it. The account still holds the initial
// password, so the file that carries it has to stay or nobody can log in.
func TestSetupThatCannotCommitKeepsTheInitialPassword(t *testing.T) {
	f := newSetupFixture(t, true)
	writeInitialPassword(t, f.passwordFile)

	cookies := loginBeforeTheSetup(t, f)
	f.stub.failCommit(errors.New("commit failed"))

	rec := do(f.e, http.MethodPost, setupPath, setupBody(t, testUsername, testNewPassword), cookies...)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}

	if initialPasswordFileIsGone(t, f.passwordFile) {
		t.Fatalf("the setup removed the initial password file although the commit failed")
	}

	account := f.stub.account()
	if account.Username != "" || !account.SetupRequired {
		t.Errorf("the account changed although the commit failed: %+v", account.Username)
	}

	// The initial password is still the one that opens the account, and the
	// session that was open is still the way back in.
	rec, again := login(t, f.e, `{"password":"`+testPassword+`"}`)
	if rec.Code != http.StatusOK || again == nil {
		t.Errorf("the initial password no longer logs in: status = %d, body: %s", rec.Code, rec.Body.String())
	}
}

// TestSetupSucceedsWithNoInitialPasswordFile covers the file being gone already,
// which is what a second run of the setup or an operator who deleted it leaves
// behind.
func TestSetupSucceedsWithNoInitialPasswordFile(t *testing.T) {
	f := newSetupFixture(t, true)

	if !initialPasswordFileIsGone(t, f.passwordFile) {
		t.Fatalf("the fixture created the initial password file")
	}

	cookies := loginBeforeTheSetup(t, f)

	rec := do(f.e, http.MethodPost, setupPath, setupBody(t, testUsername, testNewPassword), cookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if f.logs.Len() != 0 {
		t.Errorf("a file that was not there was logged as a failure: %v", f.logs.All())
	}
}

// TestSetupSucceedsWhenTheFileCannotBeRemoved pins down the answer the operator
// gets when only the deletion fails. The account has been set up by then, so an
// error would send them back to a login the initial password no longer opens.
// The directory is made unwritable to get the deletion to fail.
func TestSetupSucceedsWhenTheFileCannotBeRemoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes files from a directory it has no write permission on")
	}

	dir := filepath.Join(t.TempDir(), "locked")

	err := os.Mkdir(dir, 0700)
	if err != nil {
		t.Fatalf("failed to create the directory: %v", err)
	}

	f := newSetupFixtureAt(t, true, filepath.Join(dir, "initial-password"))
	writeInitialPassword(t, f.passwordFile)

	err = os.Chmod(dir, 0500)
	if err != nil {
		t.Fatalf("failed to take the write permission off the directory: %v", err)
	}

	// The cleanup of t.TempDir has to be able to remove the file again.
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0700)
	})

	cookies := loginBeforeTheSetup(t, f)

	rec := do(f.e, http.MethodPost, setupPath, setupBody(t, testUsername, testNewPassword), cookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if initialPasswordFileIsGone(t, f.passwordFile) {
		t.Fatalf("the file was removed, so the failure this test is about did not happen")
	}

	entries := f.logs.All()
	if len(entries) != 1 {
		t.Fatalf("the failed removal was logged %d times, want 1: %v", len(entries), entries)
	}

	logged := entries[0].ContextMap()["initial_password_file"]
	if logged != f.passwordFile {
		t.Errorf("initial_password_file = %v, want %s", logged, f.passwordFile)
	}

	// The log goes to the console as well as to a file, so the password must not
	// be anywhere in it.
	line := entries[0].Message + entries[0].Stack + fmt.Sprint(entries[0].ContextMap())
	if strings.Contains(line, testPassword) || strings.Contains(line, testNewPassword) {
		t.Errorf("the log line carries a password: %s", line)
	}

	t.Logf("%s %v", entries[0].Message, entries[0].ContextMap())
}

// TestSetupDropsTheOtherSessions pins down that the sessions the initial
// password opened are gone. Whoever read the file it was written to could have
// one of them, and the setup is the point at which that password stops counting.
func TestSetupDropsTheOtherSessions(t *testing.T) {
	f := newSetupFixture(t, true)
	writeInitialPassword(t, f.passwordFile)

	other := loginBeforeTheSetup(t, f)
	mine := loginBeforeTheSetup(t, f)

	rec := do(f.e, http.MethodPost, setupPath, setupBody(t, testUsername, testNewPassword), mine...)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	rec = do(f.e, http.MethodGet, "/api/host", "", other...)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("the other session: status = %d, want %d, body: %s",
			rec.Code, http.StatusUnauthorized, rec.Body.String())
	}

	// The session that did the setup goes on: the gate is down for it now that
	// the account has a username and a password.
	rec = do(f.e, http.MethodGet, "/api/host", "", mine...)
	if rec.Code != http.StatusOK {
		t.Fatalf("the session that did the setup: status = %d, want %d, body: %s",
			rec.Code, http.StatusOK, rec.Body.String())
	}

	f.auth.sessions.mu.RLock()
	left := len(f.auth.sessions.sessions)
	f.auth.sessions.mu.RUnlock()

	if left != 1 {
		t.Errorf("%d sessions are left, want 1", left)
	}
}

// csrfGoodLogin is the body that logs in against an account that is set up.
const csrfGoodLogin = `{"username":"` + testUsername + `","password":"` + testPassword + `"}`

// csrfStateChangingMethods is every method this API changes something with, and
// therefore every method the token is asked for on.
var csrfStateChangingMethods = []string{http.MethodPost, http.MethodPut, http.MethodDelete}

// csrfProbe stands in for the handlers behind the middleware. It counts the
// calls that reached it, so a request the middleware refused can be told apart
// from one that was answered further in.
type csrfProbe struct {
	calls int
}

func (p *csrfProbe) handle(c echo.Context) error {
	p.calls++

	return c.JSON(http.StatusOK, models.Response{Success: true})
}

// csrfProbeServer wires the middleware the way main does and hangs the probe
// behind it on one route per method, the readable one included. The login is
// registered as well, because it is where the token comes from.
func csrfProbeServer(t *testing.T) (*echo.Echo, *csrfProbe) {
	t.Helper()

	e := echo.New()
	probe := &csrfProbe{}

	authHandler := NewAuthHandler(newAccountStubDB(t, newTestAccount(t, false)), zap.NewNop(),
		filepath.Join(t.TempDir(), "initial-password"))

	g := e.Group("/api")
	g.Use(authHandler.RequireSession())
	g.POST("/login", authHandler.Login)
	g.GET("/probe", probe.handle)

	for _, method := range csrfStateChangingMethods {
		g.Add(method, "/probe", probe.handle)
	}

	return e, probe
}

// TestCsrfStateChangesWithoutATokenAreRefused is the forged request: another
// site made the browser send it, so the session cookie rides along and the
// token does not, because the page that made it cannot read the cookie of this
// origin. The refusal has to come before the handler.
func TestCsrfStateChangesWithoutATokenAreRefused(t *testing.T) {
	for _, method := range csrfStateChangingMethods {
		t.Run(method, func(t *testing.T) {
			e, probe := csrfProbeServer(t)

			cookies := csrfLoginCookies(t, e, csrfGoodLogin)
			session := cookies[0]

			rec := do(e, method, "/api/probe", "", session)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusForbidden, rec.Body.String())
			}

			if probe.calls != 0 {
				t.Errorf("the handler ran %d times on a request with no token", probe.calls)
			}

			resp := decode(t, rec)
			if !strings.Contains(resp.Error, csrfHeaderName) {
				t.Errorf("error = %q, want it to name the %s header", resp.Error, csrfHeaderName)
			}

			// The page moves to the setup screen on a 403 that mentions one, so
			// this refusal must not read like that one.
			if strings.Contains(strings.ToLower(resp.Error), "setup") {
				t.Errorf("error = %q, which the page would read as the setup refusal", resp.Error)
			}
		})
	}
}

// TestCsrfATokenFromSomewhereElseIsRefused covers what a cookie on its own
// cannot: something that can write cookies for this host sets both the cookie
// and the header to a value of its own. They agree with each other and still do
// not open anything, because the token that counts is the one the session
// holds.
func TestCsrfATokenFromSomewhereElseIsRefused(t *testing.T) {
	for _, method := range csrfStateChangingMethods {
		t.Run(method, func(t *testing.T) {
			e, probe := csrfProbeServer(t)

			cookies := csrfLoginCookies(t, e, csrfGoodLogin)
			planted := &http.Cookie{Name: csrfCookieName, Value: "a-token-the-session-never-had"}

			rec := do(e, method, "/api/probe", "", cookies[0], planted)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusForbidden, rec.Body.String())
			}

			if probe.calls != 0 {
				t.Errorf("the handler ran %d times on a planted token", probe.calls)
			}
		})
	}
}

// TestCsrfStateChangesWithTheTokenPass is the flow the page walks: log in, keep
// what the answer set, and send the token back on everything that changes
// something.
func TestCsrfStateChangesWithTheTokenPass(t *testing.T) {
	for _, method := range csrfStateChangingMethods {
		t.Run(method, func(t *testing.T) {
			e, probe := csrfProbeServer(t)

			cookies := csrfLoginCookies(t, e, csrfGoodLogin)

			rec := do(e, method, "/api/probe", "", cookies...)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
			}

			if probe.calls != 1 {
				t.Errorf("the handler ran %d times, want 1", probe.calls)
			}
		})
	}
}

// TestCsrfTheLoginNeedsNoToken pins down the one call that cannot have a token
// yet, and that it is where the token comes from.
func TestCsrfTheLoginNeedsNoToken(t *testing.T) {
	e, _ := csrfProbeServer(t)

	rec := do(e, http.MethodPost, "/api/login", csrfGoodLogin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	cookie := csrfCookieOf(rec)
	if cookie == nil {
		t.Fatalf("the login set no %s cookie, Set-Cookie: %v", csrfCookieName, rec.Result().Header["Set-Cookie"])
	}

	if cookie.Value == "" {
		t.Errorf("the %s cookie carries no token", csrfCookieName)
	}

	// The page has to read it to send it back, which is the one way this cookie
	// differs from the session one.
	if cookie.HttpOnly {
		t.Errorf("the %s cookie is HttpOnly, so the page cannot send it back", csrfCookieName)
	}

	if cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
		t.Errorf("SameSite = %v and path = %q, want %v and /", cookie.SameSite, cookie.Path, http.SameSiteLaxMode)
	}

	// The test request is plain HTTP, and a Secure cookie would never come back.
	if cookie.Secure {
		t.Errorf("the %s cookie is Secure on a plain HTTP request", csrfCookieName)
	}

	resp := decode(t, rec)
	if resp.Data.CSRFToken != cookie.Value {
		t.Errorf("data.csrf_token = %q, want the cookie value %q", resp.Data.CSRFToken, cookie.Value)
	}
}

// TestCsrfReadsNeedNoToken pins down that the check is only on the methods that
// change something. A read that asked for a token would make every screen of
// the UI wait for a login it already has.
func TestCsrfReadsNeedNoToken(t *testing.T) {
	e, probe := csrfProbeServer(t)

	cookies := csrfLoginCookies(t, e, csrfGoodLogin)

	rec := do(e, http.MethodGet, "/api/probe", "", cookies[0])
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if probe.calls != 1 {
		t.Errorf("the handler ran %d times, want 1", probe.calls)
	}

	// Every answer to a live session carries the cookie again, so a page that
	// lost its copy gets it back on the next read.
	again := csrfCookieOf(rec)
	if again == nil || again.Value != cookies[1].Value {
		t.Errorf("the read did not hand the token back: %v", again)
	}
}

// causeOnlySetupRequest builds the setup request the handler is called with
// directly, carrying the account that the session middleware would have left on
// the context.
func causeOnlySetupRequest(t *testing.T) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()

	e := echo.New()
	e.Validator = &testValidator{validator: validator.New()}

	req := httptest.NewRequest(http.MethodPost, setupPath,
		strings.NewReader(setupBody(t, testUsername, testNewPassword)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(contextUserIDKey, uint(1))

	return c, rec
}

// TestSetupKeepsTheCauseOfATransactionThatFailedOutOfTheAnswer covers the two
// failures of the setup that carried the text of the database: the transaction
// it could not start and the one it could not commit. The setup is reached
// before anyone has logged in with a password of their own, so what it answers
// is read by whoever can reach the port.
func TestSetupKeepsTheCauseOfATransactionThatFailedOutOfTheAnswer(t *testing.T) {
	tests := []struct {
		name string
		pool func() gorm.ConnPool
	}{
		{
			name: "the transaction cannot be started",
			pool: func() gorm.ConnPool { return &causeOnlyBeginFailingPool{} },
		},
		{
			name: "the transaction cannot be committed",
			pool: func() gorm.ConnPool {
				return &causeOnlyCommitFailingRoot{failing: &causeOnlyCommitFailingTx{}}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, logs := errPathLogger()
			h := NewAuthHandler(causeOnlyDB(t, tt.pool()), logger, "")

			c, rec := causeOnlySetupRequest(t)

			err := h.Setup(c)
			if err != nil {
				t.Fatalf("Setup returned an error: %v", err)
			}

			causeOnlyCheckAnswer(t, rec, logs)
		})
	}
}
