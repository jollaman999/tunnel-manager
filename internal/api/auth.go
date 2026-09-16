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
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

// csrfRefusedMessage is the answer to a state changing request that did not
// bring its token back. It says what to send, because the client can fix it.
// It must not read like the setup refusal: the UI moves to the setup screen on
// a 403 that mentions one.
const csrfRefusedMessage = "The request carries no valid " + csrfHeaderName +
	" header. Send the token of the session, which the login answers with and " +
	"the " + csrfCookieName + " cookie holds, on every POST, PUT and DELETE"

// invalidCredentialsMessage is the answer to every failed login. It does not say
// whether the username or the password was the wrong one, because that tells an
// outsider which of the two they have already got right.
const invalidCredentialsMessage = "Invalid username or password"

// setupRequiredMessage is the answer to a request that a logged in client is
// not allowed to make yet. It names what is missing, since the client can fix it
// and the UI sends the operator to the setup screen on the strength of it.
const setupRequiredMessage = "The account setup is not finished. Set a username and a password through " + setupPath + " first"

// setupAlreadyDoneMessage is the answer to a setup that comes after the account
// has one. The setup is how the account is settled the first time, not how it is
// changed afterwards: it takes no current password, so letting it run twice
// would let anyone holding a session replace the credentials.
const setupAlreadyDoneMessage = "The account is already set up"

// minPasswordBytes is the shortest password the setup takes. The password it
// replaces is 52 characters of randomness, so a much shorter one would be a
// step down from what the account is opened with in the meantime.
const minPasswordBytes = 12

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
		sessions: make(map[string]session),
		lifetime: sessionLifetime,
		now:      time.Now,
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

	s.sessions[token] = session{
		userID:    userID,
		csrfToken: csrfToken,
		expiresAt: s.now().Add(s.lifetime),
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

	now := s.now()
	if !now.Before(found.expiresAt) {
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

// DeleteAllExcept drops every session but the one token stands for. The setup
// calls it: every session that is open at that point was got with the initial
// password, and that password was written to a file anyone with a look at the
// host could have read. The session that is doing the setup is kept, because
// throwing the operator out of the request they are in the middle of protects
// nothing: they are the one who just chose the new password.
func (s *SessionStore) DeleteAllExcept(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for other := range s.sessions {
		if other != token {
			delete(s.sessions, other)
		}
	}
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
}

func NewAuthHandler(db *gorm.DB, logger *zap.Logger, initialPasswordFile string) *AuthHandler {
	return &AuthHandler{
		db:                  db,
		logger:              logger,
		initialPasswordFile: initialPasswordFile,
		sessions:            NewSessionStore(),
	}
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
func sessionCookie(c echo.Context, token string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   c.IsTLS(),
	}
}

// csrfCookie returns the cookie the CSRF token of a session is handed out in.
// It is the session cookie in every way but one: HttpOnly is off, because the
// page has to read the value to send it back in a header. That costs nothing,
// since the value is not a credential on its own: it is only accepted next to
// the session it was made for.
func csrfCookie(c echo.Context, token string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
		Secure:   c.IsTLS(),
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
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid request body: " + err.Error(),
		})
	}

	user, err := h.readUser()
	if err != nil {
		h.logger.Error("failed to read the account", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to read the account",
		})
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
		return c.JSON(http.StatusUnauthorized, models.Response{
			Success: false,
			Error:   invalidCredentialsMessage,
		})
	}

	token, csrfToken, err := h.sessions.Create(user.ID)
	if err != nil {
		h.logger.Error("failed to create a session", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to create a session",
		})
	}

	c.SetCookie(sessionCookie(c, token, 0))
	c.SetCookie(csrfCookie(c, csrfToken, 0))

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

	c.SetCookie(sessionCookie(c, "", -1))
	c.SetCookie(csrfCookie(c, "", -1))

	return c.JSON(http.StatusOK, models.Response{Success: true})
}

// Setup settles the username and the password of the account and takes the setup
// gate down. Before it has run it is the only thing a session may do, and it
// runs once: it asks for no current password, so it is not a way to change the
// credentials later on.
func (h *AuthHandler) Setup(c echo.Context) error {
	var req setupRequest

	err := c.Bind(&req)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid request body: " + err.Error(),
		})
	}

	// The name is stored with the surrounding space taken off, because that is
	// how the login compares it. A name stored with a trailing space is one the
	// operator cannot type back in.
	username := strings.TrimSpace(req.Username)
	if username == "" {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Username must not be empty",
		})
	}

	// len on a string counts bytes, which is the unit bcrypt reads the password
	// in as well.
	switch {
	case len(req.Password) < minPasswordBytes:
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Password must be at least " + strconv.Itoa(minPasswordBytes) + " bytes long",
		})
	case len(req.Password) > maxPasswordBytes:
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error: "Password must be at most " + strconv.Itoa(maxPasswordBytes) +
				" bytes long, because that is as far as bcrypt reads",
		})
	}

	// The hash is made before the transaction is opened. bcrypt is slow on
	// purpose, and the row is locked for as long as the transaction is open.
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		h.logger.Error("failed to hash the password", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to hash the password",
		})
	}

	userID, ok := c.Get(contextUserIDKey).(uint)
	if !ok {
		// The middleware is what puts it there, so getting here means the route
		// was hung somewhere the middleware does not cover.
		h.logger.Error("the setup was reached with no account on the context")
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to read the account",
		})
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction",
		})
	}

	// The row is read inside the transaction and locked, and the flag is looked
	// at again once it is. The middleware checked it too, but two requests can
	// both get past the middleware, and the second one would otherwise write
	// over the credentials the first one had just settled.
	var user models.User
	err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, userID).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to read the account", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to read the account",
		})
	}

	if !user.SetupRequired {
		tx.Rollback()
		return c.JSON(http.StatusConflict, models.Response{
			Success: false,
			Error:   setupAlreadyDoneMessage,
		})
	}

	user.Username = username
	user.PasswordHash = hash
	user.SetupRequired = false

	err = tx.Save(&user).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to set up the account", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to set up the account",
		})
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the account setup", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction",
		})
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
			c.SetCookie(csrfCookie(c, csrfToken, 0))

			if !isSafeMethod(c.Request().Method) &&
				subtle.ConstantTimeCompare([]byte(c.Request().Header.Get(csrfHeaderName)), []byte(csrfToken)) != 1 {
				return c.JSON(http.StatusForbidden, models.Response{
					Success: false,
					Error:   csrfRefusedMessage,
				})
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
				h.logger.Error("failed to read the account", zap.Error(err))
				return c.JSON(http.StatusInternalServerError, models.Response{
					Success: false,
					Error:   "Failed to read the account",
				})
			}

			if user.SetupRequired && path != setupPath {
				return c.JSON(http.StatusForbidden, models.Response{
					Success: false,
					Error:   setupRequiredMessage,
				})
			}

			c.Set(contextUserIDKey, userID)

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

// unauthenticated answers a request that carries no usable session.
func unauthenticated(c echo.Context) error {
	return c.JSON(http.StatusUnauthorized, models.Response{
		Success: false,
		Error:   "Authentication required",
	})
}
