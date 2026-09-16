package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/auth"
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

// AuthHandler serves the login and the logout and guards everything else. It is
// separate from Handler because it needs no tunnel manager and no cipher, and
// the sessions belong to it alone.
type AuthHandler struct {
	db       *gorm.DB
	logger   *zap.Logger
	sessions *SessionStore
}

func NewAuthHandler(db *gorm.DB, logger *zap.Logger) *AuthHandler {
	return &AuthHandler{
		db:       db,
		logger:   logger,
		sessions: NewSessionStore(),
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

// RequireSession returns the middleware that keeps everything behind the login.
// It is put on the /api group and on the /ui group, so a path that nobody
// registered a route for is refused by it as well.
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
