package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
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
	"github.com/labstack/echo/v5"
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

// sessionCookieName is the cookie the session token is carried in. It is the
// bare name: what goes out on the wire is what cookieName makes of it.
const sessionCookieName = "tm_session"

// hostCookiePrefix is the mark a browser guards a cookie name with. A cookie
// whose name carries it is only taken when it is Secure, has Path=/ and names
// no Domain. Without it, a sibling name under a shared domain can set a cookie
// of the same name for this host with Domain, and a page served over plain HTTP
// on this host name can set one without Secure, and the browser sends that one
// here instead. With it, only a page on this exact host name over HTTPS can.
//
// That is the whole of what it guards. Cookies carry no port (RFC 6265, section
// 8.5), and neither does the prefix: a service on another HTTPS port of this
// same host name is sent these cookies by the browser and can set ones of the
// same names, prefix and all. No attribute changes that, which is why the
// reference tells the operator to give this server a host name of its own when
// something they do not trust runs on the same one.
//
// It is also why the session cookie opens nothing on its own. A service on
// another port reads it off the requests the browser sends there, so every
// request it authenticates has to bring the key of the session as well, in a
// header, and the key is never put in a cookie: RequireSession says where it
// is kept instead. The device value that tells the operator's browser apart at
// the login stays out of cookies for the same reason, and handOutDevice says
// where it goes. Writing a cookie gives nothing away either: the key is
// compared against the one the session holds, and a device value is read from
// a header and nowhere else. What the prefix stops is the other half
// of it, an outsider writing over the cookies until the operator cannot use
// this server at all.
//
// The prefix is put on only where the cookie is really Secure, which
// cookieIsSecure decides for each request, because this server is served over
// plain HTTP as well. A browser throws away a prefixed cookie that arrives
// without Secure, so a name that carried the prefix there would hand out a
// session that never comes back and no login would ever finish.
const hostCookiePrefix = "__Host-"

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

// csrfCookieName is the cookie the key of a session was carried in before the
// key was taken out of cookies. Nothing hands it out any more: a cookie is sent
// to every port of the host name, and a key that went along with the session
// cookie would let whatever runs on another port use the session. A browser
// that still holds one is told to drop it, which dropRetiredCookies does. Like
// the session name it is the bare one, and cookieName says what it went out as.
const csrfCookieName = "tm_csrf"

// deviceCookieName is the cookie the device value of a browser was carried in
// before the value was taken out of cookies. Nothing hands it out any more and
// nothing reads it: a cookie is sent to every port of the host name, and a
// value that went there could be spent by whatever runs on another port, on
// wrong passwords that leave the operator's own browser untrusted. A browser
// that still holds one is told to drop it, which dropRetiredCookies does. Like
// the session name it is the bare one, and cookieName says what it went out as.
const deviceCookieName = "tm_device"

// csrfHeaderName is the header the key of a session is sent back in. A header
// is what is checked rather than a form field, because a page on another origin
// cannot put one on a request to this one without a preflight, and no preflight
// is ever granted: there is no CORS middleware in front of this API. The name
// says CSRF because that is what it was first there for; it is also what a
// session cookie read off another port of this host name comes without.
const csrfHeaderName = "X-CSRF-Token"

// deviceHeaderName is the header the device value of a browser is sent back in.
// loginlimit.go says what the value is for and deviceValue what is in it. It is
// a header of its own rather than the one the key of a session goes in, because
// it is sent where there is no session yet: the login is the request it is
// there for.
const deviceHeaderName = "X-TM-Device"

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
	// csrfToken is the key of the session: what every request the session
	// cookie authenticates has to send back in a header. It is held next to the
	// session rather than derived from a cookie, so a token planted by something
	// that can write cookies is not a token the server accepts, and it is handed
	// out in the body of the login and nowhere else.
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
	// generation counts the calls to DeleteAllExcept. A login takes it before
	// it reads the account and hands it back to Create, which is how a login
	// whose password was compared against credentials that have since been
	// replaced is told apart from one that was not. Generation says why the
	// counter is asked and not the row.
	generation uint64
}

// errSessionsEndedSince is what Create answers when DeleteAllExcept has run
// since the login took the generation it handed in. The credentials the login
// checked are then not the ones the account has, or are ones a setup or a
// change has just replaced, and the session is not made.
var errSessionsEndedSince = errors.New("every other session was ended after the credentials were checked")

// NewSessionStore returns an empty store with the default lifetime.
//
// No goroutine sweeps the map and no timer fires on it. What clears it is the
// two calls that hold the lock anyway: Lookup drops the session it was asked
// for once that one has run out, and Create drops every session that has.
//
// Create is there because Lookup only ever visits the token it was handed. A
// client that closes its window instead of logging out leaves a token that is
// never sent again, and a session nobody asks about is a session no lookup
// will ever throw out. Its two deadlines pass with nothing reading them.
//
// So between one login and the next the map holds the sessions that are still
// good, plus those that ran out since the last login. Both are bounded by how
// often the account password is sent correctly, and the login is what runs the
// sweep, so the map cannot grow without the thing that empties it running too.
func NewSessionStore() *SessionStore {
	return &SessionStore{
		sessions:         make(map[string]session),
		lifetime:         sessionLifetime,
		absoluteLifetime: sessionAbsoluteLifetime,
		now:              time.Now,
	}
}

// Generation returns the number of times DeleteAllExcept has run, which a
// login hands back to Create.
//
// A login compares the password against a row it read before bcrypt, and
// bcrypt takes long enough for a change of the account to commit and end every
// other session in the middle of it. A session made after that would be one
// made with credentials the operator has just replaced, and nothing would be
// left to end it. Reading the row again before Create would not close the gap
// either: the change could commit between that read and the session going into
// the map. The counter is moved under the same lock Create takes, after the
// change has committed, so a login that took it before reading the row and
// finds it unchanged inside Create was not crossed by any change at all. It
// covers a change of the username too, which leaves the hash as it was.
func (s *SessionStore) Generation() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.generation
}

// Create makes a session for userID and returns the token it is found by
// together with the key that every request of the session has to carry.
//
// generation is what Generation returned before the credentials were read.
// When DeleteAllExcept has run since, no session is made and the error is
// errSessionsEndedSince.
func (s *SessionStore) Create(userID uint, generation uint64) (string, string, error) {
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

	if s.generation != generation {
		return "", "", errSessionsEndedSince
	}

	now := s.now()

	// Every session that has run out goes here, not only the one this login
	// replaces. NewSessionStore says why this is the place: the write lock is
	// already held and a login is the one moment that is certain to come
	// before the map grows by an entry.
	//
	// The whole map is walked rather than a part of it. What is in it is the
	// sessions of the one account that were opened inside a sliding lifetime,
	// each of which cost a correct password and a bcrypt compare to make, so
	// the walk is over a handful of entries on a path that has just spent tens
	// of milliseconds in bcrypt. Walking a sample of the map instead would
	// trade a cost too small to measure for entries left behind.
	for other, found := range s.sessions {
		if s.expired(found, now) {
			delete(s.sessions, other)
		}
	}

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

	// Either deadline ends the session the same way, because to the client
	// there is no difference between the two: the session is not there any
	// more and the answer is the one a request with no session gets.
	now := s.now()
	if s.expired(found, now) {
		delete(s.sessions, token)
		return 0, "", false
	}

	found.expiresAt = now.Add(s.lifetime)
	s.sessions[token] = found

	return found.userID, found.csrfToken, true
}

// expired reports whether found has run out at now.
//
// The first deadline is the sliding one, which says the session has been left
// alone for too long. The second is counted from the login and is moved by
// nothing, which is what stops a token that is used often enough from living
// for as long as the process does. A session that is past either one is over.
//
// It is one function rather than the same test written out at both callers,
// because Lookup and the sweep in Create have to agree on what has run out. A
// sweep that dropped what Lookup still takes would sign a working client out
// of the session it is using, and one that kept what Lookup drops would leave
// behind exactly the entries it exists to clear.
func (s *SessionStore) expired(found session, now time.Time) bool {
	return !now.Before(found.expiresAt) || !now.Before(found.createdAt.Add(s.absoluteLifetime))
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

	s.generation++

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
// The key of the session, csrf_token, is in the body and only there. A cookie
// would be sent to every port of this host name along with the session cookie,
// and the two together are a session that works. The page keeps the key in its
// local storage, which a browser keeps apart for every port, and a client that
// is not a browser keeps it wherever it keeps the cookie jar.
//
// Device is the device value of this browser, which is in the body and only
// there as well. handOutDevice says why.
type loginResponse struct {
	SetupRequired bool   `json:"setup_required"`
	CSRFToken     string `json:"csrf_token"`
	Device        string `json:"device"`
}

// setupRequest is the username and the password the operator settles on.
type setupRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// setupDone is what the setup answers with: the device value of the browser
// under the credentials it just chose. The one the login handed out was signed
// under the initial password and holds no more.
type setupDone struct {
	Device string `json:"device"`
}

// sessionCookie returns the cookie a session token is handed out in. Secure is
// set from the request rather than always, because the server is served over
// plain HTTP as well and a Secure cookie would never be sent back on it.
//
// Path is / and Domain is left empty, which is what the guarded name asks for
// on top of Secure. The whole API and the page that calls it live under this
// one host, so neither one gives anything up.
func (h *AuthHandler) sessionCookie(c *echo.Context, token string, maxAge int) *http.Cookie {
	secure := h.cookieIsSecure(c)

	return &http.Cookie{
		Name:     cookieName(sessionCookieName, secure),
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	}
}

// cookieName returns the name a cookie goes out under on a request whose
// cookies are Secure or are not. hostCookiePrefix says why the name is not the
// same in both cases.
func cookieName(base string, secure bool) string {
	if !secure {
		return base
	}

	return hostCookiePrefix + base
}

// cookieValue returns what the request carries under base, or "" when it
// carries nothing under either name.
//
// Both names are looked for because which one was handed out follows the
// request the cookie was set on, and that is not decided here: an install
// behind TLS has the guarded name, one served over plain HTTP has the bare one,
// and an install that gains or loses TLS has clients holding either. The
// guarded name is read first, so where this server sets that one the bare name
// is only reached by a client that carries no guarded cookie at all. That is
// worth saying plainly: the bare name is the one an outsider can write, and a
// token from there opens nothing it did not already open, since it still has to
// name a session this server made.
func cookieValue(c *echo.Context, base string) string {
	cookie, err := c.Cookie(hostCookiePrefix + base)
	if err == nil {
		return cookie.Value
	}

	cookie, err = c.Cookie(base)
	if err == nil {
		return cookie.Value
	}

	return ""
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
func (h *AuthHandler) cookieIsSecure(c *echo.Context) bool {
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

// retiredCookieNames are the bare names of the cookies this server handed out
// before what they carried was taken out of cookies: the key of a session and
// the device value of a browser.
var retiredCookieNames = []string{csrfCookieName, deviceCookieName}

// dropRetiredCookies tells the browser to throw away the cookies the key of a
// session and the device value were handed out in before, under each name the
// request carries one under, and does nothing where it carries none.
//
// What is in them was handed out by a version that put it there, and the
// browser goes on sending it to every port of this host name for as long as it
// keeps it. Asking only where one arrived keeps a Set-Cookie off the answers of
// every other request.
//
// The guarded name is dropped with Secure on whatever the request came over,
// because a browser takes nothing under that name without it; the bare one
// follows the request, the way it was set.
func (h *AuthHandler) dropRetiredCookies(c *echo.Context) {
	for _, base := range retiredCookieNames {
		for _, name := range []string{hostCookiePrefix + base, base} {
			_, err := c.Cookie(name)
			if err != nil {
				continue
			}

			c.SetCookie(&http.Cookie{
				Name:     name,
				Value:    "",
				Path:     "/",
				MaxAge:   -1,
				SameSite: http.SameSiteLaxMode,
				Secure:   name != base || h.cookieIsSecure(c),
			})
		}
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
//
// @Summary      Log in
// @Description  Takes username and password, sets the session cookie, and answers with setup_required and csrf_token.
// @Description  csrf_token is the key of the session and is handed out here and in no cookie. Send it as X-CSRF-Token on every request the session cookie authenticates, GET included: the cookie without it is answered 401.
// @Description  A login that gets the password right also answers with device, the device value of this client, which is in no cookie either. Send it as X-TM-Device on every later login and on every call that asks for the password again: for 30 days it keeps a hold on the account from refusing this client.
// @Description  On the very first sign in the username is empty and the password is the one in the initial-password file.
// @Tags         account
// @Accept   json
// @Produce  json
// @Param   body  body  api.loginRequest  true  "The username and the password"
// @Success  200  {object}  models.Response{data=api.loginResponse}
// @Failure  400  {object}  api.errorBody  "The body could not be read"
// @Failure  401  {object}  api.errorBody  "Invalid username or password"
// @Failure  429  {object}  api.errorBody  "Too many failed attempts: the login is held"
// @Router       /login [post]
func (h *AuthHandler) Login(c *echo.Context) error {
	var req loginRequest

	err := c.Bind(&req)
	if err != nil {
		return unreadableBody(err).answer(c)
	}

	// Taken before the row is read, so that a change of the account that
	// commits after the read has ended the sessions after this was taken too.
	// SessionStore.Generation says why.
	generation := h.sessions.Generation()

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
	// nothing. The device value is checked against the same row, and it is an
	// HMAC, which is nothing next to bcrypt either.
	address := h.loginAddress(c)
	device := deviceOf(c.Request().Header.Get(deviceHeaderName), user, h.logins.now())

	attempt, wait, held := h.logins.begin(address, user.ID, device)
	if held {
		return h.refuseHeldLogin(c, wait)
	}
	defer attempt.release()

	// The password is hashed and compared whatever the username was, so that a
	// wrong username is not answered faster than a wrong password. Before the
	// setup there is no username to send, so only the password is looked at.
	ok := auth.CheckPassword(user.PasswordHash, req.Password)
	if !user.SetupRequired &&
		subtle.ConstantTimeCompare([]byte(req.Username), []byte(user.Username)) != 1 {
		ok = false
	}

	if !ok {
		attempt.failed()

		return failure(c, http.StatusUnauthorized, errAuthCredentialsInvalid)
	}

	// Everything counted against this address and this account is forgotten
	// here, before the session is made. The one who got the password right is
	// not the one the counters are for, and a failure that is still counted
	// after a successful sign in would hold the operator on their next slip.
	attempt.succeeded()

	token, csrfToken, err := h.sessions.Create(user.ID, generation)
	if errors.Is(err, errSessionsEndedSince) {
		// The answer is the one a wrong password gets. The password this
		// request carried is not the one the account has now, or is one that
		// was set as this login was being checked and that it is enough to
		// send again; saying more would tell whoever sent the old one that it
		// was right until a moment ago.
		return failure(c, http.StatusUnauthorized, errAuthCredentialsInvalid)
	}

	if err != nil {
		h.logger.Error("failed to create a session", logid.AccountSessionCreateFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errAuthSessionCreateFailed)
	}

	c.SetCookie(h.sessionCookie(c, token, 0))

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: loginResponse{
			SetupRequired: user.SetupRequired,
			CSRFToken:     csrfToken,
			// A new one on every login, whatever the browser already holds, so
			// that its thirty days are counted from the last time the password
			// was right.
			Device: h.handOutDevice(user),
		},
	})
}

// Logout drops the session and expires the cookie. A request that carries no
// session, or one that has already run out, is answered the same way: what it
// asked for is the state it is left in.
//
// @Summary      Log out
// @Description  Drops the session and expires the session cookie. The device value the client keeps goes on holding.
// @Description  Needs no X-CSRF-Token: it ends the session the cookie names and opens nothing.
// @Tags         account
// @Produce  json
// @Success  200  {object}  models.Response
// @Router       /logout [post]
func (h *AuthHandler) Logout(c *echo.Context) error {
	token := cookieValue(c, sessionCookieName)
	if token != "" {
		h.sessions.Delete(token)
	}

	c.SetCookie(h.sessionCookie(c, "", -1))

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
//
// @Summary      Whether the account still needs a username and a password
// @Description  Answers without a session, for the login screen.
// @Tags         account
// @Produce  json
// @Success  200  {object}  models.Response{data=api.setupState}
// @Router       /setup [get]
func (h *AuthHandler) GetSetup(c *echo.Context) error {
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

// @Summary      Finish the account setup
// @Description  Sets the username and the password once, on the account that still needs them.
// @Description  Answers with device, the device value of this client under the new credentials, which takes the place of the one the login answered with.
// @Tags         account
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   body  body  api.setupRequest  true  "The username and the password to set"
// @Success  200  {object}  models.Response{data=api.setupDone}
// @Failure  400  {object}  api.errorBody  "The password is too short or too long"
// @Failure  401  {object}  api.errorBody  "No session, or no valid X-CSRF-Token header"
// @Failure  409  {object}  api.errorBody  "The setup is already done"
// @Router       /setup [post]
func (h *AuthHandler) Setup(c *echo.Context) error {
	var req setupRequest

	err := c.Bind(&req)
	if err != nil {
		return unreadableBody(err).answer(c)
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
	defer rollbackUnlessDone(tx)

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
	token := cookieValue(c, sessionCookieName)

	h.sessions.DeleteAllExcept(token)
	h.removeInitialPasswordFile()

	// The device value the login handed out was signed under the initial
	// password, which stopped holding at the commit. The browser that did the
	// setup is the one browser known to be the operator's, so it is given one
	// under the credentials it just chose.
	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    setupDone{Device: h.handOutDevice(&user)},
	})
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
// behind the key of the session. It is put on the /api group, so a path under
// it that nobody registered a route for is refused by it as well. The UI files
// are served outside it, because they are the same bytes for every client and
// carry no data: what they show is fetched from here, and that is what is
// guarded.
//
// A session is the cookie and the key together, on every request and whatever
// the method. The browser sends the cookie to every port of this host name, so
// a service on another port is handed it by the operator's own browser. What
// that service is not handed is the key: the login answers with it in its
// body, and the page keeps it in local storage, which a browser keeps apart for
// every port. A request that brings the cookie without the key, or with a key
// that is not the one of that session, is answered the way one with no session
// at all is, which is what sends the page to the login.
//
// The same header is what keeps another site from making the browser send a
// request with the cookie attached, which is what it was first there for: a
// page on another origin cannot put it on a request to this one.
//
// The check lives here rather than in a middleware of its own because the key
// it compares against is the one held by the session, and both are read out of
// the same lookup. Two middlewares would either look the session up twice or
// depend on being ordered correctly to work at all.
func (h *AuthHandler) RequireSession() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			path := c.Request().URL.Path

			// A browser that signed in under a version that put the key or the
			// device value in a cookie still sends it, to this port and every
			// other one. It is told to drop it on whatever it asks for next, the
			// login included.
			h.dropRetiredCookies(c)

			// The login is how a session and its key are got, so it cannot be
			// behind either check. Nothing is forged by it: a request made from
			// another origin still has to carry the password, and the key it
			// would hand out is in an answer the sender of that request cannot
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

			// A request that brings an API token is served on the token alone,
			// whatever cookies it carries as well. The top of token.go says
			// why the key of a session is not asked of it.
			bearer, ok := bearerToken(c)
			if ok {
				return h.serveToken(c, next, bearer)
			}

			// The logout is let through on the cookie alone, with a session
			// behind it or without one. It ends the session the cookie names
			// and opens nothing, and what it asked for is the state the client
			// is left in either way, so a page that has lost the key can still
			// sign out.
			if path == logoutPath {
				return next(c)
			}

			token := cookieValue(c, sessionCookieName)

			userID, csrfToken, ok := h.sessions.Lookup(token)
			if !ok {
				return unauthenticated(c)
			}

			if subtle.ConstantTimeCompare([]byte(c.Request().Header.Get(csrfHeaderName)), []byte(csrfToken)) != 1 {
				return unauthenticated(c)
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
// is the set that changes nothing. Everything this API changes state with is a
// POST, a PUT or a DELETE, and a method that is added later is taken for one
// that changes something unless it is named here.
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
func accountPasswordRefused(c *echo.Context, db *gorm.DB, logger *zap.Logger, password string,
	wrong errorCode) *refusal {
	_, refused := accountPasswordChecked(c, db, logger, password, wrong)

	return refused
}

// accountPasswordChecked is accountPasswordRefused that also returns the row
// the password was compared against when it opened it.
//
// A call that stores something the credentials are meant to take away with
// them, an API token, has to know which credentials it was let through under.
// The compare runs ahead of its transaction, so a change of the account can
// commit between the two, and the row is what the transaction reads again to
// see whether that happened.
func accountPasswordChecked(c *echo.Context, db *gorm.DB, logger *zap.Logger, password string,
	wrong errorCode) (*models.User, *refusal) {
	userID, limiter, ok := sessionOnContext(c)
	if !ok {
		// The middleware is what puts them there, so getting here means the
		// route was hung somewhere the middleware does not cover.
		logger.Error("a call that asks for the password of the account was reached with neither the "+
			"account nor the limiter of its failures on the context, so no password was checked",
			logid.AccountReadNoAccountOnContext.Field())

		return nil, refuse(http.StatusInternalServerError, errAccountReadFailed)
	}

	// The failures are looked at before the account is read and long before
	// bcrypt runs, for the reason the login looks at them first: the compare is
	// the expensive half, and a refusal written ahead of it bounds both the
	// rate of the guessing and the CPU it spends.
	//
	// The place is reserved rather than only looked at, for the reason begin
	// gives: calls sent at once would otherwise all find the counters where
	// the first one found them and all reach bcrypt.
	attempt, refused := limiter.passwordBegin(c, userID)
	if refused != nil {
		return nil, refused
	}
	defer attempt.release()

	var user models.User

	err := db.First(&user, userID).Error
	if err != nil {
		logger.Error("failed to read the account", logid.AccountReadFailed.Field(), zap.Error(err))

		return nil, refuse(http.StatusInternalServerError, errAccountReadFailed)
	}

	if !auth.CheckPassword(user.PasswordHash, password) {
		attempt.failed()

		return nil, refuse(http.StatusUnauthorized, wrong)
	}

	attempt.succeeded()

	return &user, nil
}
