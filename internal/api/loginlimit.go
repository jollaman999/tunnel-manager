package api

import (
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
)

// The login is the one place in this API where a request that carries no
// session is answered with work. The password it sends is checked with bcrypt
// at cost 10 (internal/auth/auth.go), which is tens of milliseconds of CPU
// every time and is meant to be. With nothing counting the failures that is two
// holes at once: a password that can be guessed at as fast as the network
// carries requests, against an installation that has exactly one account to aim
// at, and a way for anyone who can reach the port to spend the CPU of the host
// without ever logging in.
//
// What is below counts the failures and stops checking passwords at all once
// there have been too many of them.
//
// It refuses rather than delays. A delay holds the request inside the handler
// and costs the sender nothing but a socket: fifty requests sent at once are
// delayed alongside each other and every one of them still reaches bcrypt, so
// a delay bounds neither the rate of the guessing nor the CPU it burns. A
// refusal written before the password is looked at bounds both, and it is the
// same answer whether the requests arrive one at a time or all together.
//
// Two things are counted, because either one alone is walked around. An
// address alone is walked around by sending each guess from another address,
// which a botnet has by the thousand. An account alone would be the only
// counter there is, and since this installation has one account, anybody who
// can reach the port could hold the operator out of it forever by guessing
// wrong on purpose. Counting both means the cheap attack meets the address
// limit and the distributed one meets the account limit, and the second is set
// far enough above ordinary mistyping that the operator never reaches it by
// hand.

// loginFailureWindow is how long a failed sign in goes on counting. Failures
// further apart than this never add up, so an operator who gets it wrong once
// this morning and once this afternoon is at one failure and not at two.
const loginFailureWindow = 15 * time.Minute

// loginAddressFailureLimit is how many failures one address may pile up inside
// that window before it is held. Five, because an operator typing a password
// they know gets it wrong once or twice - caps lock, the wrong keyboard layout
// - and a third and fourth mistake is still a person rather than a program.
const loginAddressFailureLimit = 5

// loginAddressBlockFor is how long that address is then held for.
//
// Five minutes, and the number is a trade between two costs that pull opposite
// ways. Long enough is what makes guessing pointless: five tries per five
// minutes is sixty an hour from one address, which against the shortest
// password the setup takes (minPasswordBytes, eight bytes) is not a number
// that ever finishes. Short enough matters because the operator who typed it
// wrong five times is held by this as well, with nothing but the clock to get
// them back in: five minutes is a wait, fifteen would be an outage of the thing
// they came to the screen to fix.
const loginAddressBlockFor = 5 * time.Minute

// loginAccountFailureLimit is how many failures the account may pile up inside
// the window, counted wherever they came from. Thirty, which is six times the
// per address limit on purpose: this counter exists for the attack that changes
// address on every request, so it has to sit far enough above the other one
// that an operator at one keyboard cannot walk into it. One person typing
// thirty wrong passwords in a quarter of an hour has met the address limit five
// times over already.
const loginAccountFailureLimit = 30

// loginAccountBlockFor is how long the account is then held for.
//
// Five minutes as well, and here the short end of the trade is what decides it.
// Unlike the address counter, this one can be driven by anyone who can reach
// the port: thirty requests from thirty addresses hold the account, and holding
// the operator out is the whole of what the sender gets. Five minutes bounds
// what that buys them, and it still cuts guessing down to three hundred and
// sixty tries an hour across every address there is, which is around twenty
// seconds of CPU an hour rather than a core held flat.
const loginAccountBlockFor = 5 * time.Minute

// loginLimiterMaxAddresses is how many addresses are counted at once.
//
// The map is the other thing an outsider can spend: a request from a new
// address adds an entry, so without a ceiling, sending one failed login each
// from many addresses grows it without bound and the memory is the attack.
// Four thousand entries is a few hundred kilobytes and is far more than any
// installation of this has operators, so the ceiling is only ever met by
// something that is doing it on purpose.
const loginLimiterMaxAddresses = 4096

// loginLimiterKeepAddresses is how far the map is cut back to when the ceiling
// is met. It is three quarters of it rather than the ceiling itself, so that
// the sweep runs once per thousand new addresses instead of once per request
// for as long as the flood lasts.
const loginLimiterKeepAddresses = loginLimiterMaxAddresses * 3 / 4

// loginKeyKind says which of the two counters a key belongs to. The two live in
// one map so that a single sweep covers both.
type loginKeyKind uint8

const (
	loginKeyAddress loginKeyKind = iota
	loginKeyAccount
)

// loginKey names one counter.
type loginKey struct {
	kind loginKeyKind
	name string
}

// limits returns how many failures a key of this kind takes before it is held,
// and how long it is held for.
func (k loginKey) limits() (int, time.Duration) {
	if k.kind == loginKeyAccount {
		return loginAccountFailureLimit, loginAccountBlockFor
	}

	return loginAddressFailureLimit, loginAddressBlockFor
}

// loginKeysOf returns the two counters one login attempt is counted against.
//
// The account is named by the row the password is checked against and not by
// the username the request sent. The username arrives from outside, so keying
// on it would be keying on a string the sender chooses, which is both a way to
// grow the map a name at a time and a counter that counts nothing: a guess
// against a username that does not exist is not a guess at this account. There
// is one account row, so there is one account key.
func loginKeysOf(address string, accountID uint) [2]loginKey {
	return [2]loginKey{
		{kind: loginKeyAddress, name: address},
		{kind: loginKeyAccount, name: strconv.FormatUint(uint64(accountID), 10)},
	}
}

// loginFailures is what one counter holds.
type loginFailures struct {
	count int
	// blocked says the limit has been reached and no password is being checked
	// for this key until the entry runs out.
	blocked bool
	// expiresAt is when the entry stops meaning anything. While the count is
	// under the limit it is the end of the window the failures are counted in;
	// once the limit is reached it is the end of the block, so that the block
	// ending and the count being forgotten are the same event and whoever comes
	// back after it has the whole set of tries again rather than one.
	expiresAt time.Time
}

// loginLimiter counts the failed logins. It is memory and nothing else: a
// restart forgets every block, which is the same trade the sessions make
// (SessionStore) and is affordable for the same reason. A restart of this
// process is not something an outsider can ask for.
type loginLimiter struct {
	// A plain mutex rather than an RWMutex: every path that reads also drops
	// what has run out, so there is no read-only path to take a cheaper lock
	// on, and the contention on a login is not what costs anything here.
	mu       sync.Mutex
	failures map[loginKey]loginFailures
	// inFlight is how many logins per key have been let through to the
	// password check and have not come back from it yet. It is apart from
	// failures so that neither live nor prune can drop a count that is still
	// owed a release, and it is bounded by the requests being served at once
	// rather than by anything a sender piles up over time.
	inFlight map[loginKey]int
	// now is the clock the deadlines are measured against. It is a field so a
	// test can move time forward instead of waiting for it, which is what
	// SessionStore does with the same name.
	now func() time.Time
}

// newLoginLimiter returns an empty limiter on the real clock.
//
// No goroutine sweeps the map. What has run out is dropped by the attempt that
// meets it, and the ceiling is enforced by the attempt that would push the map
// past it, so a limiter that nobody is talking to costs nothing and needs
// nothing shut down.
func newLoginLimiter() *loginLimiter {
	return &loginLimiter{
		failures: make(map[loginKey]loginFailures),
		inFlight: make(map[loginKey]int),
		now:      time.Now,
	}
}

// live returns the entry key stands for and reports whether it still counts. An
// entry that has run out is dropped here and answers as if it had never been
// made, which is how a block ends and how a window closes: both are the same
// deadline.
func (l *loginLimiter) live(key loginKey, now time.Time) (loginFailures, bool) {
	entry, ok := l.failures[key]
	if !ok {
		return loginFailures{}, false
	}

	if !now.Before(entry.expiresAt) {
		delete(l.failures, key)

		return loginFailures{}, false
	}

	return entry, true
}

// retryAfter reports whether this attempt is held, and for how much longer.
//
// Where both counters are holding, the longer of the two is what is answered:
// the sender is not free to try again until both have let go, and an answer
// that named the shorter one would send them back to another refusal.
func (l *loginLimiter) retryAfter(address string, accountID uint) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	longest := time.Duration(0)

	for _, key := range loginKeysOf(address, accountID) {
		entry, ok := l.live(key, now)
		if !ok || !entry.blocked {
			continue
		}

		left := entry.expiresAt.Sub(now)
		if left > longest {
			longest = left
		}
	}

	return longest, longest > 0
}

// failed records one wrong password against both counters.
func (l *loginLimiter) failed(address string, accountID uint) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.recordFailure(loginKeysOf(address, accountID))
}

// recordFailure is failed with the lock already held.
func (l *loginLimiter) recordFailure(keys [2]loginKey) {
	now := l.now()

	for _, key := range keys {
		limit, blockFor := key.limits()

		entry, ok := l.live(key, now)
		if !ok {
			entry = loginFailures{expiresAt: now.Add(loginFailureWindow)}
		}

		entry.count++

		if entry.count >= limit {
			entry.blocked = true
			entry.expiresAt = now.Add(blockFor)
		}

		l.failures[key] = entry
	}

	l.prune(now)
}

// succeeded forgets both counters of an attempt that got the password right.
//
// It is what keeps the limit off the operator: somebody who mistypes four times
// and then signs in is back at nothing, rather than carrying those four
// failures into the next quarter of an hour where one more slip would hold
// them. It costs an attacker nothing to reach, because reaching it means they
// already have the password.
func (l *loginLimiter) succeeded(address string, accountID uint) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.forget(loginKeysOf(address, accountID))
}

// forget is succeeded with the lock already held.
func (l *loginLimiter) forget(keys [2]loginKey) {
	for _, key := range keys {
		delete(l.failures, key)
	}
}

// begin is what the login asks before it checks a password, and unlike
// retryAfter it is a reservation and not only a question.
//
// retryAfter looks at the failures that have been recorded, and a failure is
// recorded only once bcrypt has answered. Between the two there is the whole
// of the compare, and every request that arrives in that time finds the
// counters where the first one found them: fifty wrong guesses sent at once
// all pass the check before any of them is counted, and all fifty reach
// bcrypt. That is the very pair of holes the limit is here to close, reached
// by sending in parallel what would have been refused in sequence.
//
// So the attempt is counted as it is let through. A key is held when the
// failures it has recorded and the attempts it has in the compare right now
// add up to its limit, which is the most it could be at once those attempts
// come back. For requests that arrive one at a time nothing is in flight when
// the next one asks, and the answer is exactly the one retryAfter gives.
//
// An attempt refused only for what is in flight has no deadline to name yet:
// whether a hold follows depends on how those attempts come out, which is a
// matter of the length of a bcrypt compare. The wait answered is then zero,
// which retryAfterSeconds turns into one second.
//
// The attempt returned is nil when held is true. Otherwise it must be ended
// exactly once, by failed, succeeded or release, and the caller defers
// release so that a path that returns without an outcome gives the
// reservation back rather than holding the key forever.
func (l *loginLimiter) begin(address string, accountID uint) (*loginAttempt, time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	keys := loginKeysOf(address, accountID)
	longest := time.Duration(0)
	held := false

	for _, key := range keys {
		limit, _ := key.limits()

		entry, ok := l.live(key, now)
		if ok && entry.blocked {
			held = true

			left := entry.expiresAt.Sub(now)
			if left > longest {
				longest = left
			}

			continue
		}

		if entry.count+l.inFlight[key] >= limit {
			held = true
		}
	}

	if held {
		return nil, longest, true
	}

	for _, key := range keys {
		l.inFlight[key]++
	}

	return &loginAttempt{limiter: l, keys: keys}, 0, false
}

// loginAttempt is one login that begin let through to the password check.
type loginAttempt struct {
	limiter *loginLimiter
	keys    [2]loginKey
	// ended is set by whichever of failed, succeeded or release comes first,
	// under the limiter's lock, so that the ones after it do nothing.
	ended bool
}

// end gives the reservation back and reports whether it was still held. The
// lock is held by the caller.
func (a *loginAttempt) end() bool {
	if a.ended {
		return false
	}

	a.ended = true

	for _, key := range a.keys {
		a.limiter.inFlight[key]--
		if a.limiter.inFlight[key] <= 0 {
			delete(a.limiter.inFlight, key)
		}
	}

	return true
}

// failed ends the attempt as a wrong password, which turns the reservation
// into a recorded failure in one step, so that no other request can find the
// key between the two and see neither.
func (a *loginAttempt) failed() {
	a.limiter.mu.Lock()
	defer a.limiter.mu.Unlock()

	if a.end() {
		a.limiter.recordFailure(a.keys)
	}
}

// succeeded ends the attempt as the right password and forgets both counters,
// for the reason loginLimiter.succeeded gives.
func (a *loginAttempt) succeeded() {
	a.limiter.mu.Lock()
	defer a.limiter.mu.Unlock()

	if a.end() {
		a.limiter.forget(a.keys)
	}
}

// release ends the attempt without an outcome. It is what the caller defers,
// and after failed or succeeded it does nothing.
func (a *loginAttempt) release() {
	a.limiter.mu.Lock()
	defer a.limiter.mu.Unlock()

	a.end()
}

// prune keeps the map from being the attack.
//
// It runs only when the map is over its ceiling, which no ordinary use reaches.
// First what has run out goes, which under a flood of single failures from
// changing addresses is most of it. If that is not enough, what is left is all
// still counting, and what goes then is whatever was closest to going anyway:
// the entries are cut back to loginLimiterKeepAddresses by deadline, oldest
// first.
//
// Only address entries are ever evicted. There is one account entry per row of
// the account table and the table holds one row, so account entries are not a
// way to grow anything - and if they could be evicted, a flood of addresses
// would be a way to lift the hold off the account, which is the opposite of
// what the ceiling is for.
func (l *loginLimiter) prune(now time.Time) {
	if len(l.failures) <= loginLimiterMaxAddresses {
		return
	}

	for key, entry := range l.failures {
		if !now.Before(entry.expiresAt) {
			delete(l.failures, key)
		}
	}

	addresses := make([]loginKey, 0, len(l.failures))

	for key := range l.failures {
		if key.kind == loginKeyAddress {
			addresses = append(addresses, key)
		}
	}

	if len(addresses) <= loginLimiterKeepAddresses {
		return
	}

	sort.Slice(addresses, func(i, j int) bool {
		return l.failures[addresses[i]].expiresAt.Before(l.failures[addresses[j]].expiresAt)
	})

	for _, key := range addresses[:len(addresses)-loginLimiterKeepAddresses] {
		delete(l.failures, key)
	}
}

// loginAddress returns what the login attempt is counted against.
//
// It is the address the connection came from, and it is read from the socket
// rather than from a header. echo's RealIP believes X-Forwarded-For, which is a
// header any client can put anything in: counting by it unasked would make the
// per address limit a line an attacker steps over by writing a different number
// on every request, and it would also let them pin the failures on somebody
// else's address. The forwarding headers are read only where the operator has
// said that a proxy they run is the only thing that can set them, which is the
// same switch and the same reasoning as cookieIsSecure in auth.go.
func (h *AuthHandler) loginAddress(c echo.Context) string {
	if h.trustProxyHeaders {
		return c.RealIP()
	}

	// RemoteAddr is host:port and the port is a different one on every
	// connection, so only the host half is the counter's name.
	host, _, err := net.SplitHostPort(c.Request().RemoteAddr)
	if err != nil {
		return c.Request().RemoteAddr
	}

	return host
}

// refuseHeldLogin is the answer to a login that is not being checked.
//
// It says nothing about the account. It cannot say which of the two counters is
// holding, whether the username sent exists, or whether the password was right,
// because all three are things the sender is here to find out and the refusal
// is reached before any of them is looked at. That is the same reason every
// failed login is answered with one sentence (invalidCredentialsMessage).
//
// Retry-After is sent. It tells the sender how long the hold they are already
// in has left to run, which is something they find out anyway by trying again,
// and against that it is what lets an operator who is locked out know whether
// to wait or to go and restart the service. A client that is not a browser gets
// the same number in the sentence, so neither has to guess at a number by
// retrying, which is the traffic this is here to stop.
func (h *AuthHandler) refuseHeldLogin(c echo.Context, wait time.Duration) error {
	seconds := retryAfterSeconds(wait)

	c.Response().Header().Set(echo.HeaderRetryAfter, strconv.Itoa(seconds))

	return failure(c, http.StatusTooManyRequests, errAuthTooManyAttempts,
		errorArgs{"retry_after": strconv.Itoa(seconds)})
}

// retryAfterSeconds is the wait as a client is told it.
//
// Rounded up, so that a client that comes back exactly when it was told to is
// past the deadline rather than one tick short of it, and never below one, so
// that a hold with a fraction of a second left is not answered with a nought
// that reads as no wait at all.
func retryAfterSeconds(wait time.Duration) int {
	seconds := int((wait + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}

	return seconds
}

// The login is not the only place a password reaches bcrypt. Six calls ask for
// the password of the account again before they do something that cannot be
// taken back - the account change, the uninstall, emptying the log, installing
// an update and the two host key approvals - and every one of them is a guess
// at the same secret of the same one account. What is below is how they are
// counted, and they are counted on the counters above rather than on ones of
// their own.
//
// One set of counters and not two, because two would be a limit of half the
// strength it reads as: five tries at the login plus five at each of those
// calls is a sender who gets far more than five, and the number that was argued
// for above (loginAddressFailureLimit) would hold nothing down. A guess is a
// guess whichever door it is sent through.
//
// These calls are behind a session, which is what keeps the limit from being a
// way to lock the operator out: reaching them at all means already holding one.
// What it stops is the screen left unattended, where a session is there for the
// taking and the password is the only thing between it and an uninstall.

// accountPasswordLimiter is what such a call counts an attempt against.
//
// It is an interface, and the handler that serves the login is what implements
// it, because the address a failure is counted against is not read the same way
// everywhere: loginAddress reads it from the socket unless the operator has
// said a proxy of theirs is in front. The one place that knows which it is is
// that handler, so the counters and the flag that names an address stay in a
// single object and no call can end up counting against an address the login
// does not count against.
type accountPasswordLimiter interface {
	// passwordHeld returns what to answer a call whose password is not being
	// checked, or nil when it is to be checked.
	passwordHeld(c echo.Context, accountID uint) *refusal
	// passwordFailed records one password that did not open the account.
	passwordFailed(c echo.Context, accountID uint)
	// passwordSucceeded forgets what was counted against one that did.
	passwordSucceeded(c echo.Context, accountID uint)
}

// contextPasswordLimiterKey is where the session middleware leaves the limiter,
// beside the account it leaves under contextUserIDKey.
//
// It is carried on the request rather than held by each of the handlers that
// serve those six calls, because the session middleware is the one thing all
// six are behind and the account those counters are keyed by is what it leaves
// there anyway. A call that arrives with neither is a call served from outside
// that middleware, and it checks no password at all: the counter that bounds
// the guessing is not there to count it.
const contextPasswordLimiterKey = "auth_password_limiter"

// sessionOnContext returns what the session middleware leaves on the context:
// the account of the session, and the limiter the failed password checks of
// that session are counted on. ok is false where the route was hung outside
// that middleware, which is a bug in the wiring rather than anything the
// request did.
func sessionOnContext(c echo.Context) (uint, accountPasswordLimiter, bool) {
	userID, haveUser := c.Get(contextUserIDKey).(uint)
	limiter, haveLimiter := c.Get(contextPasswordLimiterKey).(accountPasswordLimiter)

	return userID, limiter, haveUser && haveLimiter
}

// passwordHeld answers a call that asks for the account password again while
// the address it came from, or the account itself, is already held.
//
// It carries Retry-After for the reason refuseHeldLogin does, and it is a
// refusal rather than an answer written out here because the callers hold one
// until they have rolled back whatever they had open.
//
// The code is its own, and not the login's. The hold is the same hold and the
// number is the same number, but whoever meets this is logged in already and
// was asked for their password on the way to something else; a sentence that
// told them signing in was blocked would name a thing they are not doing.
func (h *AuthHandler) passwordHeld(c echo.Context, accountID uint) *refusal {
	wait, held := h.logins.retryAfter(h.loginAddress(c), accountID)
	if !held {
		return nil
	}

	seconds := retryAfterSeconds(wait)

	c.Response().Header().Set(echo.HeaderRetryAfter, strconv.Itoa(seconds))

	return refuse(http.StatusTooManyRequests, errAuthPasswordTooManyAttempts,
		errorArgs{"retry_after": strconv.Itoa(seconds)})
}

func (h *AuthHandler) passwordFailed(c echo.Context, accountID uint) {
	h.logins.failed(h.loginAddress(c), accountID)
}

// passwordSucceeded forgets both counters of a password that opened the
// account, the way a successful login does and for the same reason: an operator
// who mistyped on the way to the right password is back at nothing, and an
// attacker who reaches this already has the password.
func (h *AuthHandler) passwordSucceeded(c echo.Context, accountID uint) {
	h.logins.succeeded(h.loginAddress(c), accountID)
}
