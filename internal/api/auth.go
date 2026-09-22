package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/auth"
	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// The paths the session middleware lets through are matched against the request
// path rather than against the route, because /api/setup is served by a route
// that is added separately and a request for a path with no route at all still
// has to be answered the same way.
const (
	loginPath  = "/api/login"
	logoutPath = "/api/logout"
	setupPath  = "/api/setup"
)

// sessionCookieName is the cookie the session token is carried in.
const sessionCookieName = "tm_session"

// sessionLifetime is how long a session lives past the last request that used
// it. Every request pushes the deadline out, so a client that keeps working is
// not thrown out in the middle of it.
const sessionLifetime = 12 * time.Hour

// sessionAbsoluteLifetime is how long a session lives counted from the login,
// however much it is used. The sliding deadline above is pushed out by every
// request that finds the session, so a token that keeps being sent never runs
// out on its own: one that was stolen would go on working for as long as the
// process is up. This is the bound on that.
//
// It is fourteen sliding lifetimes, which is a week. A cap of one or two of
// them would fall due in the middle of an ordinary working day, which is the
// thing the sliding deadline exists to avoid, and it is not what the cap is
// for: what it is for is that a token cannot live forever. A week is only ever
// reached by a session that was kept alive by use for seven days straight, so
// the operator meets it about as often as they meet a restart, and a stolen
// token dies on a date that was fixed when it was made.
const sessionAbsoluteLifetime = 7 * 24 * time.Hour

// sessionTokenBytes is how much randomness a session token carries. The token
// is the whole credential, so it is read from crypto/rand and nothing else.
const sessionTokenBytes = 32

// csrfCookieName is the cookie the CSRF token of a session is carried in. It is
// a second cookie rather than the session one because the page has to read it
// to put it back in a header, and the session cookie stays HttpOnly so that a
// script can never get at the credential itself.
const csrfCookieName = "tm_csrf"

// csrfHeaderName is the header the token is sent back in. A header is what is
// checked rather than a form field, because a page on another origin cannot put
// one on a request to this one without a preflight, and no preflight is ever
// granted: there is no CORS middleware in front of this API.
const csrfHeaderName = "X-CSRF-Token"

// csrfTokenBytes is how much randomness a CSRF token carries. It is as large as
// a session token, because both are defended against guessing and nothing else.
const csrfTokenBytes = 32

// invalidCredentialsMessage is the answer to every failed login. It does not say
// whether the username or the password was the wrong one, because that tells an
// outsider which of the two they have already got right.
const invalidCredentialsMessage = "Invalid username or password"

// setupAlreadyDoneMessage is the answer to a setup that comes after the account
// has one. The setup is how the account is settled the first time, not how it is
// changed afterwards: it takes no current password, so letting it run twice
// would let anyone holding a session replace the credentials.
const setupAlreadyDoneMessage = "The account is already set up"

// minPasswordBytes is the shortest password the setup takes.
//
// The initial password it replaces is 52 characters of randomness, so anything
// a person types is a step down from what the account is opened with in the
// meantime. What this bound is for is the step down that goes further than
// that, and what stands behind it is the rate limit on the login rather than
// the length itself: see loginAddressBlockFor.
//
// The password that seals an exported configuration is held to this as well,
// and that one is guessed at under different conditions - the file carries the
// SSH credentials of every Host and is kept wherever it was put, with no rate
// limit in front of it.
const minPasswordBytes = 8

// maxPasswordBytes is the longest password the setup takes. bcrypt hashes the
// first 72 bytes of a password, and anything past that is not part of what is
// checked at the login, so a password that is longer is refused rather than
// silently shortened. The count is in bytes because that is what bcrypt counts:
// one Hangul syllable is three of them.
const maxPasswordBytes = 72

// contextUserIDKey is where the middleware leaves the account of the session,
// so a handler behind it does not read the row again.
const contextUserIDKey = "auth_user_id"

// session is one logged in client. Only the account and the deadline are kept:
// everything else about the account is read from the database when it is needed,
// so a session does not hold a copy that goes stale.
type session struct {
	userID uint
	// csrfToken is what a state changing request of this session has to send
	// back in a header. It is held next to the session rather than derived from
	// a cookie alone, so a token planted by something that can write cookies is
	// not a token the server accepts.
	csrfToken string
	// expiresAt is moved forward by every lookup that finds the session.
	expiresAt time.Time
	// createdAt is when the session was made and is never moved. It is what
	// the absolute deadline is counted from, so that the deadline cannot be
	// pushed out by the same requests that push expiresAt out.
	createdAt time.Time
}

// SessionStore holds the live sessions in memory. They are gone after a
// restart, which is what a single instance with one account can afford: the
// operator logs in again. A store of its own rather than a map on the handler
// keeps the locking in one place.
type SessionStore struct {
	// The lookup takes the write lock, because a lookup slides the deadline and
	// drops what has run out. Sessions are few enough that the contention does
	// not matter, and a read path that renewed nothing would leave every request
	// to a second locked section.
	mu       sync.RWMutex
	sessions map[string]session
	lifetime time.Duration
	// absoluteLifetime is the cap the sliding lifetime is measured against. It
	// sits next to it rather than being read from the constant at the lookup,
	// so that both deadlines of a store are named in one place.
	absoluteLifetime time.Duration
	// now is the clock the deadlines are measured against. It is a field so a
	// test can move time forward instead of waiting for it.
	now func() time.Time
}

// NewSessionStore returns an empty store with the default lifetime.
//
// No goroutine sweeps the map. A session that has run out is dropped by the
// lookup that finds it, and the map holds at most one entry per logged in
// client of the single account, so nothing piles up that a sweep would clear.
func NewSessionStore() *SessionStore {
	return &SessionStore{
		sessions:         make(map[string]session),
		lifetime:         sessionLifetime,
		absoluteLifetime: sessionAbsoluteLifetime,
		now:              time.Now,
	}
}

// Create makes a session for userID and returns the token it is found by
// together with the CSRF token that its state changing requests have to carry.
func (s *SessionStore) Create(userID uint) (string, string, error) {
	token, err := randomToken(sessionTokenBytes)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate a session token: %w", err)
	}

	csrfToken, err := randomToken(csrfTokenBytes)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate a CSRF token: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()

	s.sessions[token] = session{
		userID:    userID,
		csrfToken: csrfToken,
		expiresAt: now.Add(s.lifetime),
		createdAt: now,
	}

	return token, csrfToken, nil
}

// randomToken returns size bytes of randomness, encoded so it can sit in a
// cookie and in a header. The size is named by the caller rather than fixed
// here, so that the constant a token is described by is the one that decides
// how long it is.
func randomToken(size int) (string, error) {
	raw := make([]byte, size)

	_, err := io.ReadFull(rand.Reader, raw)
	if err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Lookup returns the account and the CSRF token of the session token stands for
// and reports whether there is one. A session that has run out is dropped here
// and answers as if it were never there.
func (s *SessionStore) Lookup(token string) (uint, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	found, ok := s.sessions[token]
	if !ok {
		return 0, "", false
	}

	// Two deadlines are checked and both end the session the same way, because
	// to the client there is no difference between the two: the session is not
	// there any more and the answer is the one a request with no session gets.
	// The first is the sliding one, which says the session has been left alone
	// for too long. The second is counted from the login and is not moved by
	// anything, which is what stops a token that is used often enough from
	// living for as long as the process does.
	now := s.now()
	if !now.Before(found.expiresAt) || !now.Before(found.createdAt.Add(s.absoluteLifetime)) {
		delete(s.sessions, token)
		return 0, "", false
	}

	found.expiresAt = now.Add(s.lifetime)
	s.sessions[token] = found

	return found.userID, found.csrfToken, true
}

// Delete drops the session token stands for. A token that is not there is not
// an error: the client asked for it to be gone and it is.
func (s *SessionStore) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.sessions, token)
}

// DeleteAllExcept drops every session but the one token stands for and returns
// how many went.
//
// The setup calls it: every session that is open at that point was got with the
// initial password, and that password was written to a file anyone with a look
// at the host could have read. So does the account change, for the same reason
// read forward: the credentials those sessions were opened with are not the
// credentials any more. The session that is making the change is kept, because
// throwing the operator out of the request they are in the middle of protects
// nothing: they are the one who just chose the new password.
//
// The count is returned because the change is answered with it. A caller that
// has nothing to say about it ignores it, and the operator being told how many
// other clients were signed out is how they find out that it happened at all.
func (s *SessionStore) DeleteAllExcept(token string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	dropped := 0

	for other := range s.sessions {
		if other != token {
			delete(s.sessions, other)
			dropped++
		}
	}

	return dropped
}

// AuthHandler serves the login and the logout and guards everything else. It is
// separate from Handler because it needs no tunnel manager and no cipher, and
// the sessions belong to it alone.
type AuthHandler struct {
	db     *gorm.DB
	logger *zap.Logger
	// initialPasswordFile is the file the startup wrote the initial password to.
	// It is handed in rather than worked out here, so the path the setup deletes
	// is the one the startup wrote and the two cannot drift apart.
	initialPasswordFile string
	sessions            *SessionStore
	// logins counts the failed sign ins and is what refuses one that has been
	// tried too often. It counts the calls that ask for the account password
	// again as well, which reach it through the session middleware: they are
	// guesses at the same secret and share these counters rather than holding
	// any of their own. It is made here rather than handed in, so that an
	// installation is limited by default and nothing has to be wired up at the
	// startup for it to be: see loginlimit.go for what it counts and why.
	logins *loginLimiter
	// trustProxyHeaders says whether the forwarding headers of whatever is in
	// front of this server are believed. It decides nothing but the Secure
	// flag of the cookies below, and it is off unless the operator turns it
	// on: a header anyone can send is only worth reading when the operator has
	// said that a proxy they run is the only thing that can send it.
	trustProxyHeaders bool
}

func NewAuthHandler(db *gorm.DB, logger *zap.Logger, initialPasswordFile string) *AuthHandler {
	return &AuthHandler{
		db:                  db,
		logger:              logger,
		initialPasswordFile: initialPasswordFile,
		sessions:            NewSessionStore(),
		logins:              newLoginLimiter(),
	}
}

// TrustProxyHeaders turns the reading of X-Forwarded-Proto on or off. It is a
// call of its own rather than an argument of the constructor, because every
// caller that builds a handler today passes three arguments and the default is
// the behaviour they already have.
//
// It is meant to be called at the startup, before anything is listening, so
// the flag is never written while a request is reading it.
func (h *AuthHandler) TrustProxyHeaders(trust bool) {
	h.trustProxyHeaders = trust
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// loginResponse tells the client which screen comes next. Before the setup is
// done the only thing a session may do is finish it.
//
// The CSRF token is in the body as well as in a cookie. The browser reads the
// cookie, which is what survives a reload of the page; a client that is not a
// browser reads the body and does not have to take a cookie jar apart to find
// the value it has to send back.
type loginResponse struct {
	SetupRequired bool   `json:"setup_required"`
	CSRFToken     string `json:"csrf_token"`
}

// setupRequest is the username and the password the operator settles on.
type setupRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// sessionCookie returns the cookie a session token is handed out in. Secure is
// set from the request rather than always, because the server is served over
// plain HTTP as well and a Secure cookie would never be sent back on it.
func (h *AuthHandler) sessionCookie(c echo.Context, token string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cookieIsSecure(c),
	}
}

// cookieIsSecure reports whether the cookies of this request are handed out
// with Secure on.
//
// echo's IsTLS is request.TLS != nil, which is what this process terminated
// itself. Behind a proxy that terminates the TLS and forwards plain HTTP, that
// is false while the browser is on https, and the session cookie would go out
// without Secure: the browser would then send it back over a plain request to
// the same name, which is the thing Secure exists to stop.
//
// X-Forwarded-Proto is what the proxy says the browser used, and it is read
// only when the operator has said there is a proxy. Unasked for, it is a
// header any client can put on a request, and believing it would let a client
// on plain HTTP ask for Secure cookies and lock itself out of its own session.
func (h *AuthHandler) cookieIsSecure(c echo.Context) bool {
	if c.IsTLS() {
		return true
	}

	if !h.trustProxyHeaders {
		return false
	}

	// A request that passed through more than one proxy carries them in a
	// list, oldest first, so the entry that says what the browser used is the
	// first one. The comparison ignores case because the value is a scheme,
	// and a scheme is case insensitive.
	proto := c.Request().Header.Get(echo.HeaderXForwardedProto)

	comma := strings.Index(proto, ",")
	if comma >= 0 {
		proto = proto[:comma]
	}

	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// csrfCookie returns the cookie the CSRF token of a session is handed out in.
// It is the session cookie in every way but one: HttpOnly is off, because the
// page has to read the value to send it back in a header. That costs nothing,
// since the value is not a credential on its own: it is only accepted next to
// the session it was made for.
func (h *AuthHandler) csrfCookie(c echo.Context, token string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cookieIsSecure(c),
	}
}

// readUser returns the account row. The table holds one row, so the login reads
// it without an identifier; a request behind a session names the account its
// session was made for.
func (h *AuthHandler) readUser(conds ...interface{}) (*models.User, error) {
	var user models.User

	err := h.db.First(&user, conds...).Error
	if err != nil {
		return nil, err
	}

	return &user, nil
}

// Login checks the credentials and hands out a session.
func (h *AuthHandler) Login(c echo.Context) error {
	var req loginRequest

	err := c.Bind(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestBodyInvalid, errorArgs{"reason": err.Error()})
	}

	user, err := h.readUser()
	if err != nil {
		h.logger.Error("failed to read the account", logid.AccountReadFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errAccountReadFailed)
	}

	// The failures are looked at before the password is, because checking the
	// password is the expensive half: bcrypt is what this refusal is protecting
	// as much as the account is. The account row is read first because the
	// counter of the account is named by the row, and that read is one indexed
	// row out of a table that holds one, which next to a bcrypt compare is
	// nothing.
	address := h.loginAddress(c)

	wait, held := h.logins.retryAfter(address, user.ID)
	if held {
		return h.refuseHeldLogin(c, wait)
	}

	// The password is hashed and compared whatever the username was, so that a
	// wrong username is not answered faster than a wrong password. Before the
	// setup there is no username to send, so only the password is looked at.
	ok := auth.CheckPassword(user.PasswordHash, req.Password)
	if !user.SetupRequired &&
		subtle.ConstantTimeCompare([]byte(req.Username), []byte(user.Username)) != 1 {
		ok = false
	}

	if !ok {
		h.logins.failed(address, user.ID)

		return failure(c, http.StatusUnauthorized, errAuthCredentialsInvalid)
	}

	// Everything counted against this address and this account is forgotten
	// here, before the session is made. The one who got the password right is
	// not the one the counters are for, and a failure that is still counted
	// after a successful sign in would hold the operator on their next slip.
	h.logins.succeeded(address, user.ID)

	token, csrfToken, err := h.sessions.Create(user.ID)
	if err != nil {
		h.logger.Error("failed to create a session", logid.AccountSessionCreateFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errAuthSessionCreateFailed)
	}

	c.SetCookie(h.sessionCookie(c, token, 0))
	c.SetCookie(h.csrfCookie(c, csrfToken, 0))

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: loginResponse{
			SetupRequired: user.SetupRequired,
			CSRFToken:     csrfToken,
		},
	})
}

// Logout drops the session and expires the cookie. A request that carries no
// session, or one that has already run out, is answered the same way: what it
// asked for is the state it is left in.
func (h *AuthHandler) Logout(c echo.Context) error {
	cookie, err := c.Cookie(sessionCookieName)
	if err == nil {
		h.sessions.Delete(cookie.Value)
	}

	c.SetCookie(h.sessionCookie(c, "", -1))
	c.SetCookie(h.csrfCookie(c, "", -1))

	return c.JSON(http.StatusOK, models.Response{Success: true})
}

// Setup settles the username and the password of the account and takes the setup
// gate down. Before it has run it is the only thing a session may do, and it
// runs once: it asks for no current password, so it is not a way to change the
// credentials later on.
// setupState is what GET /api/setup answers with.
type setupState struct {
	SetupRequired bool `json:"setup_required"`
}

// GetSetup says whether the account is still waiting for its username and
// password. It answers without a session: the login screen asks it before
// anyone has signed in, to decide whether the hint about the first sign in is
// still true.
func (h *AuthHandler) GetSetup(c echo.Context) error {
	user, err := h.readUser()
	if err != nil {
		h.logger.Error("failed to read the account", logid.AccountReadFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errAccountReadFailed)
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    setupState{SetupRequired: user.SetupRequired},
	})
}

func (h *AuthHandler) Setup(c echo.Context) error {
	var req setupRequest

	err := c.Bind(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestBodyInvalid, errorArgs{"reason": err.Error()})
	}

	// The name is stored with the surrounding space taken off, because that is
	// how the login compares it. A name stored with a trailing space is one the
	// operator cannot type back in.
	username := strings.TrimSpace(req.Username)
	if username == "" {
		return failure(c, http.StatusBadRequest, errAccountUsernameEmpty)
	}

	// len on a string counts bytes, which is the unit bcrypt reads the password
	// in as well.
	switch {
	case len(req.Password) < minPasswordBytes:
		return failure(c, http.StatusBadRequest, errAuthPasswordTooShort, errorArgs{"min": strconv.Itoa(minPasswordBytes)})
	case len(req.Password) > maxPasswordBytes:
		return failure(c, http.StatusBadRequest, errAuthPasswordTooLong, errorArgs{"max": strconv.Itoa(maxPasswordBytes)})
	}

	// The hash is made before the transaction is opened. bcrypt is slow on
	// purpose, and the row is locked for as long as the transaction is open.
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		h.logger.Error("failed to hash the password", logid.AccountPasswordHashFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errAccountHashFailed)
	}

	userID, ok := c.Get(contextUserIDKey).(uint)
	if !ok {
		// The middleware is what puts it there, so getting here means the route
		// was hung somewhere the middleware does not cover.
		h.logger.Error("the setup was reached with no account on the context", logid.AccountSetupNoAccountOnContext.Field())
		return failure(c, http.StatusInternalServerError, errAccountReadFailed)
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}

	// The row is read inside the transaction and the flag is looked at again
	// once it is. The middleware checked it too, but two requests can both get
	// past the middleware, and the second one would otherwise write over the
	// credentials the first one had just settled. The single database
	// connection holds the second transaction until the first has committed,
	// so this read sees the flag the first one cleared. See UpdateHost in
	// handlers.go for why the FOR UPDATE that stood here is gone.
	var user models.User
	err = tx.First(&user, userID).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to read the account", logid.AccountReadFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errAccountReadFailed)
	}

	if !user.SetupRequired {
		tx.Rollback()
		return failure(c, http.StatusConflict, errAuthSetupAlreadyDone)
	}

	user.Username = username
	user.PasswordHash = hash
	user.SetupRequired = false

	err = tx.Save(&user).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to set up the account", logid.AccountSetupFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errAuthSetupFailed)
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the account setup", logid.AccountSetupCommitFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionCommitFailed)
	}

	// Everything below this point runs only because the account has already
	// been written. Dropping the sessions or the file before the commit would
	// take away the initial password while it is still the one that opens the
	// account, and a commit that then failed would leave nobody able to log in.
	token := ""

	cookie, err := c.Cookie(sessionCookieName)
	if err == nil {
		token = cookie.Value
	}

	h.sessions.DeleteAllExcept(token)
	h.removeInitialPasswordFile()

	return c.JSON(http.StatusOK, models.Response{Success: true})
}

// removeInitialPasswordFile deletes the file the startup wrote the initial
// password to. The password in it stopped opening the account at the commit, so
// what is left is a readable copy of a credential with no reason to exist.
//
// A failure is logged and nothing more. The account has been set up by the time
// this runs, and answering the request with an error would send the operator
// back to a login that the initial password no longer opens. Only the path is
// logged: the log goes to the console as well as to a file, which is why the
// password was never written to it in the first place.
func (h *AuthHandler) removeInitialPasswordFile() {
	if h.initialPasswordFile == "" {
		return
	}

	err := os.Remove(h.initialPasswordFile)
	if err == nil || os.IsNotExist(err) {
		// A file that is not there is the state that was asked for. It is gone
		// because a setup already removed it or because the operator did.
		return
	}

	h.logger.Error("failed to remove the initial password file. The account is set up and the "+
		"password in the file no longer opens it, but the file is still there and has to be "+
		"removed by hand",
		logid.AccountInitialPasswordFileRemoveFailed.Field(),
		zap.Error(err),
		zap.String("initial_password_file", h.initialPasswordFile))
}

// RequireSession returns the middleware that keeps the API behind the login and
// behind the CSRF check. It is put on the /api group, so a path under it that
// nobody registered a route for is refused by it as well. The UI files are
// served outside it, because they are the same bytes for every client and carry
// no data: what they show is fetched from here, and that is what is guarded.
//
// The CSRF check lives here rather than in a middleware of its own because the
// token it compares against is the one held by the session, and both are read
// out of the same lookup. Two middlewares would either look the session up
// twice or depend on being ordered correctly to work at all.
func (h *AuthHandler) RequireSession() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			path := c.Request().URL.Path

			// The login is how a session and its CSRF token are got, so it
			// cannot be behind either check. Nothing is forged by it: a request
			// made from another origin still has to carry the password, and the
			// session it would hand out goes into a cookie the attacker cannot
			// read.
			if path == loginPath {
				return next(c)
			}

			// Whether the setup has happened is read by the login screen,
			// which has no session yet, so that it can stop telling a reader
			// to leave the username empty once there is a username to type.
			// It is the one bit a login attempt would reveal anyway, and only
			// the read is open: the setup itself stays behind the session.
			if path == setupPath && c.Request().Method == http.MethodGet {
				return next(c)
			}

			token := ""

			cookie, err := c.Cookie(sessionCookieName)
			if err == nil {
				token = cookie.Value
			}

			userID, csrfToken, ok := h.sessions.Lookup(token)
			if !ok {
				// The logout has to answer a client whose session is already
				// gone, and what it asked for is the state it is in. There is
				// no session to protect either, so there is nothing to check.
				if path == logoutPath {
					return next(c)
				}

				return unauthenticated(c)
			}

			// The cookie is written again on every request that carries a live
			// session, so that a page which lost its copy gets it back on the
			// next read instead of being unable to change anything until the
			// operator logs in again.
			c.SetCookie(h.csrfCookie(c, csrfToken, 0))

			if !isSafeMethod(c.Request().Method) &&
				subtle.ConstantTimeCompare([]byte(c.Request().Header.Get(csrfHeaderName)), []byte(csrfToken)) != 1 {
				return failure(c, http.StatusForbidden, errAuthCSRFRefused, errorArgs{"header": csrfHeaderName, "cookie": csrfCookieName})
			}

			// The logout needs nothing below this point: it ends the session it
			// was sent with, whatever the account behind it is allowed to reach.
			if path == logoutPath {
				return next(c)
			}

			// SetupRequired is read from the database on every request instead
			// of being carried in the session. A copy taken at login would keep
			// refusing the session that finished the setup, since the flag it
			// holds is the one from before.
			user, err := h.readUser(userID)
			if err != nil {
				h.logger.Error("failed to read the account", logid.AccountReadFailed.Field(), zap.Error(err))
				return failure(c, http.StatusInternalServerError, errAccountReadFailed)
			}

			if user.SetupRequired && path != setupPath {
				return failure(c, http.StatusForbidden, errAuthSetupRequired, errorArgs{"path": setupPath})
			}

			c.Set(contextUserIDKey, userID)
			// The limiter of the login goes on beside the account, because the
			// calls behind this middleware that ask for the password again are
			// guesses at the same secret and are counted on the same counters.
			// contextPasswordLimiterKey says why it travels on the request.
			c.Set(contextPasswordLimiterKey, accountPasswordLimiter(h))

			return next(c)
		}
	}
}

// isSafeMethod reports whether a method is one that RFC 9110 calls safe, which
// is the set that changes nothing and therefore needs no CSRF token. Everything
// this API changes state with is a POST, a PUT or a DELETE, and a method that
// is added later is checked unless it is named here.
func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}

	return false
}

// accountPasswordRefused checks the password of the account a session belongs
// to and returns what to answer with when it does not open it, or nil when it
// does.
//
// It reads the account behind the session rather than a username in the body,
// the way the uninstall does: what is being asked for is that the person at the
// screen is the one who logged in, and a request that named the account would
// let a stolen session name any of them.
//
// wrong is the code a password that does not open the account is refused under.
// It is handed in because the screen reads the code to tell a wrong password
// from a session that has ended, and the two calls that ask for a password have
// to be told apart there: a panel that shows the refusal where the operator is
// working is the point of the distinction.
//
// It reads through the handle it is given and is not to be called from inside a
// transaction. The pool holds one connection, so the read would wait for the
// connection the transaction is holding and never be given it. ApproveHostKey
// says the rest of it.
//
// The attempt is counted on the counters of the login and is refused once there
// have been too many of them, which is what keeps every call behind this one
// from being a way to guess at the password as fast as the network carries
// requests. loginlimit.go says what is counted and why it is the one set of
// counters.
func accountPasswordRefused(c echo.Context, db *gorm.DB, logger *zap.Logger, password string,
	wrong errorCode) *refusal {
	userID, limiter, ok := sessionOnContext(c)
	if !ok {
		// The middleware is what puts them there, so getting here means the
		// route was hung somewhere the middleware does not cover.
		logger.Error("a call that asks for the password of the account was reached with neither the "+
			"account nor the limiter of its failures on the context, so no password was checked",
			logid.AccountReadNoAccountOnContext.Field())

		return refuse(http.StatusInternalServerError, errAccountReadFailed)
	}

	// The failures are looked at before the account is read and long before
	// bcrypt runs, for the reason the login looks at them first: the compare is
	// the expensive half, and a refusal written ahead of it bounds both the
	// rate of the guessing and the CPU it spends.
	refused := limiter.passwordHeld(c, userID)
	if refused != nil {
		return refused
	}

	var user models.User

	err := db.First(&user, userID).Error
	if err != nil {
		logger.Error("failed to read the account", logid.AccountReadFailed.Field(), zap.Error(err))

		return refuse(http.StatusInternalServerError, errAccountReadFailed)
	}

	if !auth.CheckPassword(user.PasswordHash, password) {
		limiter.passwordFailed(c, userID)

		return refuse(http.StatusUnauthorized, wrong)
	}

	limiter.passwordSucceeded(c, userID)

	return nil
}
