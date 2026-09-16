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

// Create makes a session for userID and returns the token it is found by.
func (s *SessionStore) Create(userID uint) (string, error) {
	raw := make([]byte, sessionTokenBytes)

	_, err := io.ReadFull(rand.Reader, raw)
	if err != nil {
		return "", fmt.Errorf("failed to generate a session token: %w", err)
	}

	token := base64.RawURLEncoding.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.sessions[token] = session{userID: userID, expiresAt: s.now().Add(s.lifetime)}

	return token, nil
}

// Lookup returns the account of the session token stands for and reports
// whether there is one. A session that has run out is dropped here and answers
// as if it were never there.
func (s *SessionStore) Lookup(token string) (uint, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	found, ok := s.sessions[token]
	if !ok {
		return 0, false
	}

	now := s.now()
	if !now.Before(found.expiresAt) {
		delete(s.sessions, token)
		return 0, false
	}

	found.expiresAt = now.Add(s.lifetime)
	s.sessions[token] = found

	return found.userID, true
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
type loginResponse struct {
	SetupRequired bool `json:"setup_required"`
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

	token, err := h.sessions.Create(user.ID)
	if err != nil {
		h.logger.Error("failed to create a session", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to create a session",
		})
	}

	c.SetCookie(sessionCookie(c, token, 0))

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    loginResponse{SetupRequired: user.SetupRequired},
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
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction: " + err.Error(),
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
			Error:   "Failed to commit transaction: " + err.Error(),
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

// RequireSession returns the middleware that keeps the API behind the login.
// It is put on the /api group, so a path under it that nobody registered a
// route for is refused by it as well. The UI files are served outside it,
// because they are the same bytes for every client and carry no data: what
// they show is fetched from here, and that is what the session guards.
func (h *AuthHandler) RequireSession() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			path := c.Request().URL.Path

			// The login is how a session is got, and the logout has to answer a
			// client whose session is already gone, so neither can be behind
			// the session check.
			if path == loginPath || path == logoutPath {
				return next(c)
			}

			cookie, err := c.Cookie(sessionCookieName)
			if err != nil {
				return unauthenticated(c)
			}

			userID, ok := h.sessions.Lookup(cookie.Value)
			if !ok {
				return unauthenticated(c)
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

// unauthenticated answers a request that carries no usable session.
func unauthenticated(c echo.Context) error {
	return c.JSON(http.StatusUnauthorized, models.Response{
		Success: false,
		Error:   "Authentication required",
	})
}
