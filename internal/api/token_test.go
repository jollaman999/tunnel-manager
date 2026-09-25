package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/auth"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// tokenFixture is the server wired the way main wires it, against a real
// database file, with the token routes and one stand-in route per kind of
// thing a token is checked for: a read, a change a scope opens, and the
// account change no scope opens.
type tokenFixture struct {
	e     *echo.Echo
	db    *gorm.DB
	logs  *observer.ObservedLogs
	probe *csrfProbe

	// entered is sent on by every request that reaches the slow route, and
	// release lets all of them return at once.
	entered chan struct{}
	release chan struct{}
}

// The routes the stand-in answers on. They are real routes of the table, so
// what the middleware decides for them is what it decides in main.
const (
	tokenReadPath  = "/api/status"
	tokenHostsPath = "/api/host"
	tokenListPath  = "/api/token"
	// tokenSlowPath is a read route whose handler does not return until the
	// test lets it, so that requests can be held open inside the middleware.
	tokenSlowPath = "/api/settings"
)

// tokenOtherAddress is a second address to send from, for the tests that tell
// the counter of one address from the counter of another.
const tokenOtherAddress = "198.51.100.7:40000"

func newTokenFixture(t *testing.T) *tokenFixture {
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

	// One connection, the way internal/database opens it, so that the
	// transaction of the creation is what a second request waits behind.
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to reach the connection pool: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	err = db.AutoMigrate(&models.User{}, &models.APIToken{})
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

	core, logs := observer.New(zap.DebugLevel)

	e := echo.New()
	probe := &csrfProbe{}
	authHandler := NewAuthHandler(db, zap.New(core), "")

	g := e.Group("/api")
	g.Use(authHandler.RequireSession())
	g.POST("/login", authHandler.Login)
	g.POST("/logout", authHandler.Logout)
	g.GET("/setup", authHandler.GetSetup)
	g.POST("/setup", authHandler.Setup)
	g.GET("/account", authHandler.GetAccount)
	g.PUT("/account", authHandler.ChangeAccount)
	g.GET("/token", authHandler.ListTokens)
	g.POST("/token", authHandler.CreateToken)
	g.DELETE("/token/:id", authHandler.DeleteToken)
	g.GET("/status", probe.handle)
	g.POST("/host", probe.handle)

	entered := make(chan struct{}, 64)
	release := make(chan struct{})

	g.GET("/settings", func(c echo.Context) error {
		entered <- struct{}{}
		<-release

		return c.JSON(http.StatusOK, models.Response{Success: true})
	})
	// A route no table names, which is what a route added to main and not to
	// the table looks like to the middleware.
	g.GET("/unlisted", probe.handle)

	return &tokenFixture{e: e, db: db, logs: logs, probe: probe, entered: entered, release: release}
}

// tokenAnswer is an answer as a client of the token routes reads it.
type tokenAnswer struct {
	Success   bool            `json:"success"`
	ErrorCode string          `json:"error_code"`
	Data      json.RawMessage `json:"data"`
}

func decodeTokenAnswer(t *testing.T, rec *httptest.ResponseRecorder) tokenAnswer {
	t.Helper()

	var answer tokenAnswer

	err := json.Unmarshal(rec.Body.Bytes(), &answer)
	if err != nil {
		t.Fatalf("failed to decode the body %q: %v", rec.Body.String(), err)
	}

	return answer
}

// signIn logs in and returns the session cookies.
func (f *tokenFixture) signIn(t *testing.T) []*http.Cookie {
	t.Helper()

	return csrfLoginCookies(t, f.e, loginBody(t, testUsername, testPassword))
}

// create makes a token with the session given and returns what the answer
// carried.
func (f *tokenFixture) create(t *testing.T, cookies []*http.Cookie, body string) tokenCreated {
	t.Helper()

	rec := do(f.e, http.MethodPost, tokenListPath, body, cookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("the creation answered %d, want 201: %s", rec.Code, rec.Body.String())
	}

	var created tokenCreated

	err := json.Unmarshal(decodeTokenAnswer(t, rec).Data, &created)
	if err != nil {
		t.Fatalf("failed to read the created token: %v", err)
	}

	return created
}

// withBearer sends one request with the token in an Authorization header and
// no cookie, from remote.
func (f *tokenFixture) withBearer(method, target, body, token, remote string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)

	if remote != "" {
		req.RemoteAddr = remote
	}

	rec := httptest.NewRecorder()
	f.e.ServeHTTP(rec, req)

	return rec
}

// list reads the tokens back with the session given.
func (f *tokenFixture) list(t *testing.T, cookies []*http.Cookie) []tokenView {
	t.Helper()

	rec := do(f.e, http.MethodGet, tokenListPath, "", cookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("the list answered %d: %s", rec.Code, rec.Body.String())
	}

	var views []tokenView

	err := json.Unmarshal(decodeTokenAnswer(t, rec).Data, &views)
	if err != nil {
		t.Fatalf("failed to read the list: %v", err)
	}

	return views
}

// stored reads the row of a token straight out of the database.
func (f *tokenFixture) stored(t *testing.T, id uint) models.APIToken {
	t.Helper()

	var row models.APIToken

	err := f.db.First(&row, id).Error
	if err != nil {
		t.Fatalf("failed to read the token back: %v", err)
	}

	return row
}

// TestATokenIsMadeListedAndRevoked walks the three routes of the Settings card.
func TestATokenIsMadeListedAndRevoked(t *testing.T) {
	f := newTokenFixture(t)
	cookies := f.signIn(t)

	before := time.Now()
	created := f.create(t, cookies, `{"name":"monitoring","scopes":["read"]}`)

	if !strings.HasPrefix(created.Token, apiTokenPrefix) {
		t.Fatalf("the token is %q, want it to begin with %q", created.Token, apiTokenPrefix)
	}

	// 32 bytes in unpadded base64url is 43 characters.
	if len(created.Token) != len(apiTokenPrefix)+43 {
		t.Fatalf("the token is %d characters long, want %d", len(created.Token), len(apiTokenPrefix)+43)
	}

	if created.ExpiresAt == nil {
		t.Fatal("a token made without a lifetime never runs out, want the default of 90 days")
	}

	want := before.AddDate(0, 0, tokenDefaultExpiryDays)
	if created.ExpiresAt.Sub(want) > time.Minute || want.Sub(*created.ExpiresAt) > time.Minute {
		t.Fatalf("the token runs out at %v, want about %v", created.ExpiresAt, want)
	}

	rec := do(f.e, http.MethodGet, tokenListPath, "", cookies...)
	if strings.Contains(rec.Body.String(), created.Token) {
		t.Fatalf("the list carries the token itself: %s", rec.Body.String())
	}

	if strings.Contains(rec.Body.String(), hashAPIToken(created.Token)) {
		t.Fatalf("the list carries the hash of the token: %s", rec.Body.String())
	}

	views := f.list(t, cookies)
	if len(views) != 1 || views[0].Name != "monitoring" || strings.Join(views[0].Scopes, ",") != "read" ||
		views[0].LastUsedAt != nil || views[0].ExpiresAt == nil {
		t.Fatalf("the list is %+v", views)
	}

	rec = do(f.e, http.MethodDelete, tokenListPath+"/"+strconv.Itoa(int(created.ID)), "", cookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("the revocation answered %d: %s", rec.Code, rec.Body.String())
	}

	if views := f.list(t, cookies); len(views) != 0 {
		t.Fatalf("the list after the revocation is %+v", views)
	}

	rec = do(f.e, http.MethodDelete, tokenListPath+"/"+strconv.Itoa(int(created.ID)), "", cookies...)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a second revocation answered %d, want 404", rec.Code)
	}

	rec = f.withBearer(http.MethodGet, tokenReadPath, "", created.Token, "")
	if rec.Code != http.StatusUnauthorized || decodeTokenAnswer(t, rec).ErrorCode != string(errAuthTokenInvalid) {
		t.Fatalf("a revoked token answered %d %s, want 401 %s", rec.Code, rec.Body.String(), errAuthTokenInvalid)
	}
}

// TestTheCreationRefusesWhatItCannotStore covers the refusals of the creation,
// and the one lifetime that has to be asked for by name.
func TestTheCreationRefusesWhatItCannotStore(t *testing.T) {
	f := newTokenFixture(t)
	cookies := f.signIn(t)

	f.create(t, cookies, `{"name":"taken","scopes":["read"]}`)

	for _, tc := range []struct {
		body string
		code int
		want errorCode
	}{
		{`{"name":"  ","scopes":["read"]}`, http.StatusBadRequest, errTokenNameEmpty},
		{`{"name":"` + strings.Repeat("n", tokenNameMaxLength+1) + `","scopes":["read"]}`,
			http.StatusBadRequest, errTokenNameTooLong},
		{`{"name":"a","scopes":[]}`, http.StatusBadRequest, errTokenScopesEmpty},
		{`{"name":"a","scopes":["read","admin"]}`, http.StatusBadRequest, errTokenScopeUnknown},
		{`{"name":"a","scopes":["read"],"expires_in_days":7}`, http.StatusBadRequest, errTokenExpiryUnsupported},
		{`{"name":"taken","scopes":["read"]}`, http.StatusConflict, errTokenNameTaken},
		{`not json`, http.StatusBadRequest, errTokenRequestInvalid},
	} {
		rec := do(f.e, http.MethodPost, tokenListPath, tc.body, cookies...)
		if rec.Code != tc.code || decodeTokenAnswer(t, rec).ErrorCode != string(tc.want) {
			t.Errorf("%s answered %d %s, want %d %s", tc.body, rec.Code, rec.Body.String(), tc.code, tc.want)
		}
	}

	forever := f.create(t, cookies, `{"name":"forever","scopes":["hosts","read","hosts"],"expires_in_days":0}`)
	if forever.ExpiresAt != nil {
		t.Fatalf("a token asked to never run out runs out at %v", forever.ExpiresAt)
	}

	if strings.Join(forever.Scopes, ",") != "read,hosts" {
		t.Fatalf("the scopes were stored as %v, want read,hosts in the order of the table", forever.Scopes)
	}
}

// TestATokenIsNotStoredInPlainText reads the row the way someone holding a copy
// of the database file would.
func TestATokenIsNotStoredInPlainText(t *testing.T) {
	f := newTokenFixture(t)
	created := f.create(t, f.signIn(t), `{"name":"backup","scopes":["read"]}`)

	row := f.stored(t, created.ID)
	if row.Hash != hashAPIToken(created.Token) {
		t.Fatalf("the stored hash is %q, want the SHA-256 of the token", row.Hash)
	}

	var columns map[string]interface{}

	err := f.db.Raw("SELECT * FROM api_tokens WHERE id = ?", created.ID).Scan(&columns).Error
	if err != nil {
		t.Fatalf("failed to read the row: %v", err)
	}

	random := strings.TrimPrefix(created.Token, apiTokenPrefix)

	for name, value := range columns {
		text, ok := value.(string)
		if ok && strings.Contains(text, random) {
			t.Fatalf("the column %s holds the token: %q", name, text)
		}
	}
}

// TestATokenReachesARouteOfItsScope is the whole point: a script with a token
// and no cookie reads what the token was made for.
func TestATokenReachesARouteOfItsScope(t *testing.T) {
	f := newTokenFixture(t)
	created := f.create(t, f.signIn(t), `{"name":"monitoring","scopes":["read"]}`)

	rec := f.withBearer(http.MethodGet, tokenReadPath, "", created.Token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("a read with a read token answered %d: %s", rec.Code, rec.Body.String())
	}

	if f.probe.calls != 1 {
		t.Fatalf("the route was reached %d times, want 1", f.probe.calls)
	}

	// The scheme is read without regard to case.
	req := httptest.NewRequest(http.MethodGet, tokenReadPath, nil)
	req.Header.Set(echo.HeaderAuthorization, "bearer "+created.Token)

	lower := httptest.NewRecorder()
	f.e.ServeHTTP(lower, req)

	if lower.Code != http.StatusOK {
		t.Fatalf("a read with a lower case scheme answered %d: %s", lower.Code, lower.Body.String())
	}
}

// TestATokenIsRefusedAChangeItHasNoScopeFor covers the scope check, and that
// the token that does carry the scope makes the change with no CSRF token.
func TestATokenIsRefusedAChangeItHasNoScopeFor(t *testing.T) {
	f := newTokenFixture(t)
	cookies := f.signIn(t)
	reader := f.create(t, cookies, `{"name":"reader","scopes":["read"]}`)
	writer := f.create(t, cookies, `{"name":"writer","scopes":["hosts"]}`)

	rec := f.withBearer(http.MethodPost, tokenHostsPath, "{}", reader.Token, "")
	if rec.Code != http.StatusForbidden || decodeTokenAnswer(t, rec).ErrorCode != string(errAuthTokenScopeMissing) {
		t.Fatalf("a change with a read token answered %d %s, want 403 %s",
			rec.Code, rec.Body.String(), errAuthTokenScopeMissing)
	}

	if f.probe.calls != 0 {
		t.Fatalf("the route was reached %d times by a token without its scope", f.probe.calls)
	}

	rec = f.withBearer(http.MethodPost, tokenHostsPath, "{}", writer.Token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("a change with a hosts token answered %d: %s", rec.Code, rec.Body.String())
	}

	// And the other way round: hosts does not open the reads.
	rec = f.withBearer(http.MethodGet, tokenReadPath, "", writer.Token, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a read with a hosts token answered %d, want 403", rec.Code)
	}
}

// TestNoScopeOpensTheRoutesThatAreNeverGranted sends a token that carries every
// scope to each of the routes no scope opens, and to one no table names.
func TestNoScopeOpensTheRoutesThatAreNeverGranted(t *testing.T) {
	f := newTokenFixture(t)
	cookies := f.signIn(t)
	all := f.create(t, cookies, `{"name":"everything","scopes":["`+strings.Join(tokenScopes, `","`)+`"]}`)

	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/account"},
		{http.MethodPut, "/api/account"},
		{http.MethodGet, "/api/token"},
		{http.MethodPost, "/api/token"},
		{http.MethodDelete, "/api/token/" + strconv.Itoa(int(all.ID))},
		{http.MethodPost, "/api/logout"},
		{http.MethodPost, "/api/setup"},
		{http.MethodGet, "/api/unlisted"},
	} {
		rec := f.withBearer(route.method, route.path, `{"name":"more","scopes":["read"]}`, all.Token, "")
		if rec.Code != http.StatusForbidden || decodeTokenAnswer(t, rec).ErrorCode != string(errAuthTokenRouteRefused) {
			t.Errorf("%s %s with every scope answered %d %s, want 403 %s",
				route.method, route.path, rec.Code, rec.Body.String(), errAuthTokenRouteRefused)
		}
	}

	if views := f.list(t, cookies); len(views) != 1 {
		t.Fatalf("the tokens are %+v after the refusals, want the one that was made", views)
	}
}

// TestAReadTokenDoesNotReadTheAccount holds the account read to sessions: the
// read scope opens every other read, and not this one.
func TestAReadTokenDoesNotReadTheAccount(t *testing.T) {
	f := newTokenFixture(t)
	cookies := f.signIn(t)
	reader := f.create(t, cookies, `{"name":"reader","scopes":["read"]}`)

	rec := f.withBearer(http.MethodGet, "/api/account", "", reader.Token, "")
	if rec.Code != http.StatusForbidden || decodeTokenAnswer(t, rec).ErrorCode != string(errAuthTokenRouteRefused) {
		t.Fatalf("the account read with a read token answered %d %s, want 403 %s",
			rec.Code, rec.Body.String(), errAuthTokenRouteRefused)
	}

	if strings.Contains(rec.Body.String(), testUsername) {
		t.Fatalf("the refusal carries the account name: %s", rec.Body.String())
	}

	rec = do(f.e, http.MethodGet, "/api/account", "", cookies...)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), testUsername) {
		t.Fatalf("the account read with a session answered %d %s, want 200 with %q",
			rec.Code, rec.Body.String(), testUsername)
	}
}

// TestAnExpiredTokenIsRefused moves the deadline of a token into the past.
func TestAnExpiredTokenIsRefused(t *testing.T) {
	f := newTokenFixture(t)
	created := f.create(t, f.signIn(t), `{"name":"old","scopes":["read"]}`)

	err := f.db.Model(&models.APIToken{}).Where("id = ?", created.ID).
		UpdateColumn("expires_at", time.Now().Add(-time.Second)).Error
	if err != nil {
		t.Fatalf("failed to move the deadline: %v", err)
	}

	for i := 0; i < loginAddressFailureLimit+1; i++ {
		rec := f.withBearer(http.MethodGet, tokenReadPath, "", created.Token, "")
		if rec.Code != http.StatusUnauthorized || decodeTokenAnswer(t, rec).ErrorCode != string(errAuthTokenExpired) {
			t.Fatalf("try %d: an expired token answered %d %s, want 401 %s",
				i+1, rec.Code, rec.Body.String(), errAuthTokenExpired)
		}
	}
}

// TestAWrongTokenIsCountedAgainstTheAddress holds the wrong tokens to the
// counters of the login: the address that sent them is held, for tokens and
// for the login alike, and another address is not.
func TestAWrongTokenIsCountedAgainstTheAddress(t *testing.T) {
	f := newTokenFixture(t)
	created := f.create(t, f.signIn(t), `{"name":"good","scopes":["read"]}`)

	for i := 0; i < loginAddressFailureLimit; i++ {
		rec := f.withBearer(http.MethodGet, tokenReadPath, "", apiTokenPrefix+"guess"+strconv.Itoa(i), "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong token %d answered %d, want 401", i+1, rec.Code)
		}
	}

	rec := f.withBearer(http.MethodGet, tokenReadPath, "", apiTokenPrefix+"guess-again", "")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get(echo.HeaderRetryAfter) == "" {
		t.Fatalf("a wrong token from the held address answered %d with Retry-After %q, want 429",
			rec.Code, rec.Header().Get(echo.HeaderRetryAfter))
	}

	login, _ := login(t, f.e, loginBody(t, testUsername, testPassword))
	if login.Code != http.StatusTooManyRequests {
		t.Fatalf("a login from the held address answered %d, want 429", login.Code)
	}

	// A good token is not held by the address it comes from: the limiter is
	// not asked about a token that is found.
	rec = f.withBearer(http.MethodGet, tokenReadPath, "", created.Token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("a good token from the held address answered %d, want 200", rec.Code)
	}

	rec = f.withBearer(http.MethodGet, tokenReadPath, "", created.Token, tokenOtherAddress)
	if rec.Code != http.StatusOK {
		t.Fatalf("a good token from another address answered %d, want 200: the wrong tokens "+
			"were counted against more than the address", rec.Code)
	}
}

// TestWrongTokensNeverTouchTheAccountCounter sends more wrong tokens than the
// account counter takes, each from an address of its own so that no address
// counter is held, and then signs in from yet another address. Were the wrong
// tokens counted on the account as well, the account would be held and the
// operator with it.
func TestWrongTokensNeverTouchTheAccountCounter(t *testing.T) {
	f := newTokenFixture(t)

	for i := 0; i < loginAccountFailureLimit+5; i++ {
		address := "203.0.113." + strconv.Itoa(i+1) + ":40000"

		rec := f.withBearer(http.MethodGet, tokenReadPath, "", apiTokenPrefix+"guess"+strconv.Itoa(i), address)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong token %d answered %d, want 401", i+1, rec.Code)
		}
	}

	rec := loginFrom(f.e, "198.51.100.7", loginBody(t, testUsername, testPassword))
	if rec.Code != http.StatusOK {
		t.Fatalf("the login after %d wrong tokens from as many addresses answered %d, want 200: "+
			"the wrong tokens were counted on the account: %s",
			loginAccountFailureLimit+5, rec.Code, rec.Body.String())
	}
}

// TestGoodTokensInFlightDoNotHoldTheLogin keeps more good-token requests open
// at once than the address counter takes, on a handler that does not return
// until it is let go, and signs in from the same address while they are open.
// A good token that reserved a place on the counter for the length of its
// handler would hold that login.
func TestGoodTokensInFlightDoNotHoldTheLogin(t *testing.T) {
	f := newTokenFixture(t)
	created := f.create(t, f.signIn(t), `{"name":"slow","scopes":["read"]}`)

	const open = loginAddressFailureLimit + 2

	var wg sync.WaitGroup

	codes := make(chan int, open)

	for i := 0; i < open; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			codes <- f.withBearer(http.MethodGet, tokenSlowPath, "", created.Token, "").Code
		}()
	}

	// Every request is inside the handler before the login is sent. One that
	// was refused before it got there never arrives, so the wait is bounded
	// rather than left to hang the test.
	for i := 0; i < open; i++ {
		select {
		case <-f.entered:
		case <-time.After(5 * time.Second):
			close(f.release)
			wg.Wait()
			t.Fatalf("only %d of %d good-token requests reached the handler", i, open)
		}
	}

	rec, _ := login(t, f.e, loginBody(t, testUsername, testPassword))
	loginCode := rec.Code

	extra := f.withBearer(http.MethodGet, tokenReadPath, "", created.Token, "").Code

	close(f.release)
	wg.Wait()
	close(codes)

	if loginCode != http.StatusOK {
		t.Fatalf("a login with %d good-token requests open from the same address answered %d, want 200",
			open, loginCode)
	}

	if extra != http.StatusOK {
		t.Fatalf("a good token with %d others open answered %d, want 200", open, extra)
	}

	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("a held good-token request answered %d, want 200", code)
		}
	}
}

// TestTheCsrfCheckIsSkippedOnlyForAToken sends the same change twice without
// an X-CSRF-Token header: once with a session, which is refused, and once with
// a token, which is not. A request that carries both is served on the token.
func TestTheCsrfCheckIsSkippedOnlyForAToken(t *testing.T) {
	f := newTokenFixture(t)
	cookies := f.signIn(t)
	writer := f.create(t, cookies, `{"name":"writer","scopes":["hosts"]}`)

	var session *http.Cookie

	for _, cookie := range cookies {
		if isCookieFor(cookie, sessionCookieName) {
			session = cookie
		}
	}

	rec := do(f.e, http.MethodPost, tokenHostsPath, "{}", session)
	if rec.Code != http.StatusForbidden || decodeTokenAnswer(t, rec).ErrorCode != string(errAuthCSRFRefused) {
		t.Fatalf("a session change without the header answered %d %s, want 403 %s",
			rec.Code, rec.Body.String(), errAuthCSRFRefused)
	}

	rec = f.withBearer(http.MethodPost, tokenHostsPath, "{}", writer.Token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("a token change without the header answered %d: %s", rec.Code, rec.Body.String())
	}

	// A header of another scheme is not a token, so it is no way around the
	// check either.
	req := httptest.NewRequest(http.MethodPost, tokenHostsPath, strings.NewReader("{}"))
	req.Header.Set(echo.HeaderAuthorization, "Basic "+writer.Token)
	req.AddCookie(session)

	basic := httptest.NewRecorder()
	f.e.ServeHTTP(basic, req)

	if basic.Code != http.StatusForbidden || decodeTokenAnswer(t, basic).ErrorCode != string(errAuthCSRFRefused) {
		t.Fatalf("a session change with a Basic header answered %d %s, want 403 %s",
			basic.Code, basic.Body.String(), errAuthCSRFRefused)
	}
}

// TestATokenRecordsWhenItWasLastUsed covers the column the card shows, and that
// it is not written again inside the minute.
func TestATokenRecordsWhenItWasLastUsed(t *testing.T) {
	f := newTokenFixture(t)
	created := f.create(t, f.signIn(t), `{"name":"monitoring","scopes":["read"]}`)

	if f.stored(t, created.ID).LastUsedAt != nil {
		t.Fatal("a token that was never used has a last use")
	}

	before := time.Now()

	rec := f.withBearer(http.MethodGet, tokenReadPath, "", created.Token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("the read answered %d", rec.Code)
	}

	first := f.stored(t, created.ID).LastUsedAt
	if first == nil || first.Before(before.Add(-time.Second)) {
		t.Fatalf("the last use is %v after a use at %v", first, before)
	}

	f.withBearer(http.MethodGet, tokenReadPath, "", created.Token, "")

	second := f.stored(t, created.ID).LastUsedAt
	if second == nil || !second.Equal(*first) {
		t.Fatalf("the last use moved from %v to %v inside the minute", first, second)
	}
}

// TestAChangeWithATokenIsLoggedUnderItsName covers the line the log keeps of a
// change made with a token, and that a read leaves none.
func TestAChangeWithATokenIsLoggedUnderItsName(t *testing.T) {
	f := newTokenFixture(t)
	cookies := f.signIn(t)
	created := f.create(t, cookies, `{"name":"deploy-bot","scopes":["read","hosts"]}`)

	f.withBearer(http.MethodGet, tokenReadPath, "", created.Token, "")
	f.withBearer(http.MethodPost, tokenHostsPath, "{}", created.Token, "")

	lines := f.logs.FilterMessage("a change was requested with an API token").All()
	if len(lines) != 1 {
		t.Fatalf("%d lines were logged for one change and one read, want 1", len(lines))
	}

	fields := lines[0].ContextMap()
	if lines[0].Level != zap.InfoLevel || fields["token"] != "deploy-bot" ||
		fields["method"] != http.MethodPost || fields["path"] != tokenHostsPath {
		t.Fatalf("the line is %v %v", lines[0].Level, fields)
	}

	for _, line := range f.logs.All() {
		for _, value := range line.ContextMap() {
			text, ok := value.(string)
			if ok && strings.Contains(text, strings.TrimPrefix(created.Token, apiTokenPrefix)) {
				t.Fatalf("the line %q carries the token", line.Message)
			}
		}
	}
}

// TestAPasswordChangeLeavesTheTokensAlone covers the decision that the tokens
// outlive a change of the account password, which ends every other session.
func TestAPasswordChangeLeavesTheTokensAlone(t *testing.T) {
	f := newTokenFixture(t)
	cookies := f.signIn(t)
	created := f.create(t, cookies, `{"name":"monitoring","scopes":["read"]}`)

	rec := do(f.e, http.MethodPut, "/api/account", accountBody(t, testPassword, "", testNewPassword), cookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("the password change answered %d: %s", rec.Code, rec.Body.String())
	}

	rec = f.withBearer(http.MethodGet, tokenReadPath, "", created.Token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("the token answered %d after the password change, want 200", rec.Code)
	}
}

// TestTheTokenTableNamesOnlyScopesThatExist keeps a typo in the table from
// being a scope no token can ever be made with.
func TestTheTokenTableNamesOnlyScopesThatExist(t *testing.T) {
	for route, scope := range tokenRouteScopes {
		if !isTokenScope(scope) {
			t.Errorf("%s needs %q, which is not a scope", route, scope)
		}

		if tokenNeverRoutes[route] || tokenPublicRoutes[route] {
			t.Errorf("%s is given a scope and is named as a route no token reaches", route)
		}

		method, _, _ := strings.Cut(route, " ")
		if (method == http.MethodGet) != (scope == TokenScopeRead) {
			t.Errorf("%s needs %q: every read is the read scope and nothing else is", route, scope)
		}
	}
}
