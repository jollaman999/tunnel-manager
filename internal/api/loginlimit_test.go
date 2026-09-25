package api

import (
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
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// The wrong password every attempt below sends. It is not the account's, and
// it is not sent anywhere.
const mistypedPassword = "not-a-real-one-9" // hook:allow

// tooManyAttemptsCode is the code a held login is refused under, and
// passwordAttemptsCode the one a held call that asks for the account password
// again is refused under. Both are written out here so that the tests read the
// name a screen keys its phrase book by rather than the identifier of the
// constant, and they are two names because the sentence behind them is two
// sentences: one counter holds both, but only one of the two paths is a login.
const (
	tooManyAttemptsCode  = "auth.attempts.too_many"
	passwordAttemptsCode = "auth.password_attempts.too_many"
)

// goodLogin and badLogin are the bodies of a login that has the password right
// and one that has it wrong.
var (
	goodLogin = `{"username":"` + testUsername + `","password":"` + testPassword + `"}`
	badLogin  = `{"username":"` + testUsername + `","password":"` + mistypedPassword + `"}`
)

// loginFrom sends one login that arrives from address.
//
// httptest gives every request the same RemoteAddr, which is exactly what the
// per address counter is not allowed to be tested with: a test that let every
// attempt come from one place could not tell the address counter from the
// account one. The port is fixed and made up; only the host half is what the
// limiter counts by.
func loginFrom(e *echo.Echo, address, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, loginPath, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.RemoteAddr = address + ":54321"

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	return rec
}

// failLogins sends count wrong passwords from address and fails the test if any
// of them is answered with anything but the ordinary refusal. It is what a test
// uses to walk a counter up to just under its limit.
func failLogins(t *testing.T, e *echo.Echo, address string, count int) {
	t.Helper()

	for i := 0; i < count; i++ {
		rec := loginFrom(e, address, badLogin)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d from %s: status = %d, want %d, body: %s",
				i+1, address, rec.Code, http.StatusUnauthorized, rec.Body.String())
		}
	}
}

// heldClock puts the limiter of a handler on a clock the test moves, and
// returns the function that moves it. It is the SessionStore trick: the field
// is there so that a deadline can be met without waiting for it.
//
// Nothing runs in another goroutine here. Every request a test sends is served
// on the goroutine that sent it, so the clock is read and written in one place.
func heldClock(h *AuthHandler) (func(time.Duration), time.Time) {
	start := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	at := start

	h.logins.now = func() time.Time { return at }

	return func(d time.Duration) { at = at.Add(d) }, start
}

// TestTooManyFailedLoginsFromOneAddressAreRefused is the per address half of
// the limit. The password of the sixth attempt is never looked at, which is
// shown by sending the right one and being refused all the same.
func TestTooManyFailedLoginsFromOneAddressAreRefused(t *testing.T) {
	e, _ := newTestServer(t, newTestAccount(t, false))

	const guesser = "198.51.100.7"

	failLogins(t, e, guesser, loginAddressFailureLimit)

	rec := loginFrom(e, guesser, badLogin)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d: status = %d, want %d, body: %s",
			loginAddressFailureLimit+1, rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}

	code := refusalCode(t, rec)
	if code != tooManyAttemptsCode {
		t.Errorf("error_code = %q, want %q", code, tooManyAttemptsCode)
	}

	t.Logf("the answer to attempt %d: %s", loginAddressFailureLimit+1, rec.Body.String())

	// The right password is refused as well, which is the only way from out
	// here to show that no password is being checked at all.
	rec = loginFrom(e, guesser, goodLogin)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("the right password from a held address: status = %d, want %d, body: %s",
			rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}

	if sessionCookieOf(rec) != nil {
		t.Errorf("a held login handed out a session cookie")
	}

	// Another address is still checked. The account counter is at six failures
	// here and its limit is much higher, so what refuses the address above is
	// the address counter and nothing else.
	rec = loginFrom(e, "203.0.113.4", badLogin)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("another address: status = %d, want %d, body: %s",
			rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}

// TestAHeldLoginSaysHowLongToWait covers the header and the sentence. Both
// carry the same number, so a browser and a script are told the same thing.
func TestAHeldLoginSaysHowLongToWait(t *testing.T) {
	e, h := newTestServer(t, newTestAccount(t, false))
	advance, _ := heldClock(h)

	const guesser = "198.51.100.8"

	failLogins(t, e, guesser, loginAddressFailureLimit)

	rec := loginFrom(e, guesser, badLogin)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}

	want := strconv.Itoa(int(loginAddressBlockFor / time.Second))

	got := rec.Result().Header.Get(echo.HeaderRetryAfter)
	if got != want {
		t.Errorf("Retry-After = %q, want %q", got, want)
	}

	if !strings.Contains(rec.Body.String(), want+" seconds") {
		t.Errorf("the sentence does not carry the same number as the header: %s", rec.Body.String())
	}

	t.Logf("Retry-After: %s", got)

	// It counts down with the clock rather than being the same number for as
	// long as the hold lasts.
	advance(loginAddressBlockFor - time.Minute)

	rec = loginFrom(e, guesser, badLogin)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}

	got = rec.Result().Header.Get(echo.HeaderRetryAfter)
	if got != "60" {
		t.Errorf("a minute before the hold ends, Retry-After = %q, want %q", got, "60")
	}
}

// TestTooManyFailedLoginsAgainstTheAccountAreRefused is the other half. Every
// attempt comes from an address of its own, so no address counter ever reaches
// its limit and what refuses the last one is the account counter.
func TestTooManyFailedLoginsAgainstTheAccountAreRefused(t *testing.T) {
	e, _ := newTestServer(t, newTestAccount(t, false))

	for i := 0; i < loginAccountFailureLimit; i++ {
		failLogins(t, e, "203.0.113."+strconv.Itoa(i), 1)
	}

	// An address that has never sent anything, so nothing about it is counted.
	rec := loginFrom(e, "198.51.100.200", badLogin)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}

	code := refusalCode(t, rec)
	if code != tooManyAttemptsCode {
		t.Errorf("error_code = %q, want %q", code, tooManyAttemptsCode)
	}

	t.Logf("%d failures from %d addresses held the account: %s",
		loginAccountFailureLimit, loginAccountFailureLimit, rec.Body.String())

	// The right password from a fresh address is held too. The account is what
	// is being protected, so nothing gets in while it is held.
	rec = loginFrom(e, "198.51.100.201", goodLogin)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("the right password while the account is held: status = %d, want %d, body: %s",
			rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}
}

// TestAHeldLoginIsCheckedAgainOnceTheHoldRunsOut moves the clock past the hold
// and finds the login working. Without it a mistyped password would be an
// outage that only a restart ends.
func TestAHeldLoginIsCheckedAgainOnceTheHoldRunsOut(t *testing.T) {
	e, h := newTestServer(t, newTestAccount(t, false))
	advance, start := heldClock(h)

	const operator = "198.51.100.9"

	failLogins(t, e, operator, loginAddressFailureLimit)

	rec := loginFrom(e, operator, goodLogin)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}

	// One second short of the hold it is still a hold.
	advance(loginAddressBlockFor - time.Second)

	rec = loginFrom(e, operator, goodLogin)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a second before the hold ends: status = %d, want %d, body: %s",
			rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}

	advance(time.Second)

	rec = loginFrom(e, operator, goodLogin)
	if rec.Code != http.StatusOK {
		t.Fatalf("after the hold: status = %d, want %d, body: %s",
			rec.Code, http.StatusOK, rec.Body.String())
	}

	if sessionCookieOf(rec) == nil {
		t.Errorf("the login after the hold set no session cookie")
	}

	t.Logf("held at %s, signed in at %s", start.Format(time.TimeOnly),
		start.Add(loginAddressBlockFor).Format(time.TimeOnly))

	// The count went with the hold, so the address has its whole set of tries
	// again rather than one. This is the last of them.
	failLogins(t, e, operator, loginAddressFailureLimit-1)
}

// TestASuccessfulLoginForgetsTheFailuresBeforeIt is the reset. Four wrong
// passwords and then the right one leaves nothing counted, so the four after it
// are the first four and not the fifth to the eighth.
func TestASuccessfulLoginForgetsTheFailuresBeforeIt(t *testing.T) {
	e, h := newTestServer(t, newTestAccount(t, false))

	const operator = "198.51.100.10"

	failLogins(t, e, operator, loginAddressFailureLimit-1)

	rec := loginFrom(e, operator, goodLogin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	h.logins.mu.Lock()
	left := len(h.logins.failures)
	h.logins.mu.Unlock()

	if left != 0 {
		t.Errorf("the limiter holds %d counters after a successful login, want 0", left)
	}

	// Had the four before the success been kept, the first of these would have
	// been the fifth and would have been held.
	failLogins(t, e, operator, loginAddressFailureLimit-1)

	t.Logf("%d failures, a successful login, and %d more failures, none of them held",
		loginAddressFailureLimit-1, loginAddressFailureLimit-1)
}

// TestWrongLoginsSentAtOnceAreCountedAsTheyAreLetThrough is the limit against
// guesses that arrive together rather than one after another. Every one of
// them is let go at the same moment, so they all ask the limiter while the
// first few are still in bcrypt. Were the failures counted only once the
// compare has answered, every one would find the counter where the first did
// and all of them would be checked; counted as they are let through, no more
// than the limit reach the password and the rest are refused as held.
func TestWrongLoginsSentAtOnceAreCountedAsTheyAreLetThrough(t *testing.T) {
	e, h := newTestServer(t, newTestAccount(t, false))

	const (
		guesser = "198.51.100.21"
		sent    = 8 * loginAddressFailureLimit
	)

	codes := make([]int, sent)

	var (
		ready sync.WaitGroup
		done  sync.WaitGroup
	)

	start := make(chan struct{})

	for i := 0; i < sent; i++ {
		ready.Add(1)
		done.Add(1)

		go func(i int) {
			defer done.Done()

			ready.Done()
			<-start

			codes[i] = loginFrom(e, guesser, badLogin).Code
		}(i)
	}

	ready.Wait()
	close(start)
	done.Wait()

	checked, refused := 0, 0

	for i, code := range codes {
		switch code {
		case http.StatusUnauthorized:
			checked++
		case http.StatusTooManyRequests:
			refused++
		default:
			t.Errorf("attempt %d: status = %d, want %d or %d",
				i+1, code, http.StatusUnauthorized, http.StatusTooManyRequests)
		}
	}

	t.Logf("%d wrong logins sent at once: %d had the password checked, %d were refused as held",
		sent, checked, refused)

	if checked > loginAddressFailureLimit {
		t.Errorf("%d of %d wrong logins sent at once had the password checked, want no more than %d",
			checked, sent, loginAddressFailureLimit)
	}

	if checked == 0 {
		t.Errorf("none of %d wrong logins had the password checked, want at least one", sent)
	}

	// Every reservation was given back, whichever way the attempt ended.
	h.logins.mu.Lock()
	inFlight := len(h.logins.inFlight)
	h.logins.mu.Unlock()

	if inFlight != 0 {
		t.Errorf("the limiter still counts %d keys in flight after every login came back, want 0", inFlight)
	}
}

// TestALoginInFlightHoldsTheOnesAfterIt is the same thing on the limiter
// alone, where the attempts can be held open for as long as the test likes
// rather than for as long as a bcrypt compare happens to take. It also covers
// the three ways an attempt ends: a release gives the place back without
// counting anything, a failure turns it into a failure, and a success forgets
// the lot.
func TestALoginInFlightHoldsTheOnesAfterIt(t *testing.T) {
	limiter := newLoginLimiter()

	at := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	limiter.now = func() time.Time { return at }

	const guesser = "198.51.100.22"

	attempts := make([]*loginAttempt, 0, loginAddressFailureLimit)

	for i := 0; i < loginAddressFailureLimit; i++ {
		attempt, _, held := limiter.begin(guesser, 1)
		if held {
			t.Fatalf("attempt %d of %d was held with nothing yet recorded", i+1, loginAddressFailureLimit)
		}

		attempts = append(attempts, attempt)
	}

	_, wait, held := limiter.begin(guesser, 1)
	if !held {
		t.Fatalf("attempt %d was let through with %d already in flight",
			loginAddressFailureLimit+1, loginAddressFailureLimit)
	}

	if retryAfterSeconds(wait) != 1 {
		t.Errorf("a login held only for what is in flight is told to wait %d seconds, want 1",
			retryAfterSeconds(wait))
	}

	// Another address is not held by these: the account counter is far below
	// its own limit.
	other, _, held := limiter.begin("203.0.113.22", 1)
	if held {
		t.Errorf("another address was held by the logins in flight from %s", guesser)
	} else {
		other.release()
	}

	// A release gives one place back and records nothing, and a second one
	// after it does nothing.
	attempts[0].release()
	attempts[0].release()
	attempts[0].failed()

	again, _, held := limiter.begin(guesser, 1)
	if held {
		t.Fatalf("a released attempt did not give its place back")
	}

	attempts[0] = again

	// Every attempt failing turns the reservations into failures, which is
	// the ordinary hold, deadline and all.
	for _, attempt := range attempts {
		attempt.failed()
		attempt.release()
	}

	_, wait, held = limiter.begin(guesser, 1)
	if !held || wait != loginAddressBlockFor {
		t.Errorf("after %d failures: held = %v, wait = %v, want true and %v",
			loginAddressFailureLimit, held, wait, loginAddressBlockFor)
	}

	if len(limiter.inFlight) != 0 {
		t.Errorf("the limiter still counts %d keys in flight, want 0", len(limiter.inFlight))
	}

	// A success forgets both counters and gives the place back.
	at = at.Add(loginAddressBlockFor)

	attempt, _, held := limiter.begin(guesser, 1)
	if held {
		t.Fatalf("the hold did not end when it ran out")
	}

	attempt.succeeded()

	if len(limiter.failures) != 0 || len(limiter.inFlight) != 0 {
		t.Errorf("after a success the limiter holds %d counters and %d keys in flight, want 0 and 0",
			len(limiter.failures), len(limiter.inFlight))
	}
}

// TestAnOrdinaryLoginIsUntouchedByTheLimiter is the evidence that none of this
// broke the thing it guards: the first login of a fresh installation, and one
// that comes after a couple of mistyped passwords, both work.
func TestAnOrdinaryLoginIsUntouchedByTheLimiter(t *testing.T) {
	e, _ := newTestServer(t, newTestAccount(t, false))

	rec := loginFrom(e, "198.51.100.11", goodLogin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if sessionCookieOf(rec) == nil {
		t.Fatalf("the login set no session cookie")
	}

	if rec.Result().Header.Get(echo.HeaderRetryAfter) != "" {
		t.Errorf("an ordinary login was answered with Retry-After")
	}

	failLogins(t, e, "198.51.100.12", 2)

	rec = loginFrom(e, "198.51.100.12", goodLogin)
	if rec.Code != http.StatusOK {
		t.Fatalf("after two mistyped passwords: status = %d, want %d, body: %s",
			rec.Code, http.StatusOK, rec.Body.String())
	}

	// The login before the setup sends the password alone, and it is held by
	// the same counters. What matters here is that it still works.
	e, _ = newTestServer(t, newTestAccount(t, true))

	rec = loginFrom(e, "198.51.100.13", `{"password":"`+testPassword+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the login before the setup: status = %d, want %d, body: %s",
			rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestAHeldLoginTellsNothingAboutTheAccount holds the refusal to the rule every
// failed login is already held to: the answer may not say which half of the
// credentials was right. A held login is refused before either half is looked
// at, so the two answers have to be the same bytes.
func TestAHeldLoginTellsNothingAboutTheAccount(t *testing.T) {
	e, _ := newTestServer(t, newTestAccount(t, false))

	const guesser = "198.51.100.14"

	failLogins(t, e, guesser, loginAddressFailureLimit)

	// The account's own username with the right password, and a username no
	// account has with a wrong one. Nothing about the two is alike except that
	// both are held.
	known := loginFrom(e, guesser, goodLogin)
	unknown := loginFrom(e, guesser, `{"username":"nobody-of-that-name","password":"`+mistypedPassword+`"}`)

	if known.Code != unknown.Code {
		t.Errorf("status = %d for the account's username and %d for one no account has",
			known.Code, unknown.Code)
	}

	if known.Body.String() != unknown.Body.String() {
		t.Errorf("the two answers differ:\n%s\n%s", known.Body.String(), unknown.Body.String())
	}

	if known.Result().Header.Get(echo.HeaderRetryAfter) != unknown.Result().Header.Get(echo.HeaderRetryAfter) {
		t.Errorf("Retry-After = %q and %q",
			known.Result().Header.Get(echo.HeaderRetryAfter),
			unknown.Result().Header.Get(echo.HeaderRetryAfter))
	}

	if strings.Contains(known.Body.String(), testUsername) {
		t.Errorf("the refusal carries the username of the account: %s", known.Body.String())
	}

	t.Logf("both answers: %s", known.Body.String())
}

// TestACounterThatRanOutIsDroppedWhenItIsMet is the cheap half of the cleanup.
// Nothing sweeps in the background, so an entry that stopped meaning anything
// goes when the next attempt touches it.
func TestACounterThatRanOutIsDroppedWhenItIsMet(t *testing.T) {
	limiter := newLoginLimiter()

	at := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	limiter.now = func() time.Time { return at }

	limiter.failed("198.51.100.15", 1)

	if len(limiter.failures) != 2 {
		t.Fatalf("one failure left %d counters, want 2: the address and the account",
			len(limiter.failures))
	}

	at = at.Add(loginFailureWindow)

	_, held := limiter.retryAfter("198.51.100.15", 1)
	if held {
		t.Errorf("a failure from a window that has closed is still holding")
	}

	if len(limiter.failures) != 0 {
		t.Errorf("the limiter holds %d counters after the window closed, want 0", len(limiter.failures))
	}
}

// TestTheLimiterDoesNotGrowWithoutBound is the other half, and it is the one
// that matters: the map is a thing an outsider writes to, one entry per address
// they send a failed login from, so without a ceiling the memory is the attack.
//
// The account counter is held while the flood runs, which is the second thing
// checked here. If a flood of addresses could push it out, a flood of addresses
// would be how an attacker lifts a hold off the account.
func TestTheLimiterDoesNotGrowWithoutBound(t *testing.T) {
	limiter := newLoginLimiter()

	at := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	limiter.now = func() time.Time { return at }

	for i := 0; i < loginAccountFailureLimit; i++ {
		limiter.failed("203.0.113."+strconv.Itoa(i), 1)
	}

	_, held := limiter.retryAfter("198.51.100.16", 1)
	if !held {
		t.Fatalf("%d failures did not hold the account", loginAccountFailureLimit)
	}

	// A failed login every millisecond from an address nothing has been seen
	// from before. This is the shape of the attack the ceiling is for, and the
	// whole flood runs inside the hold that is already on the account.
	const flood = 3 * loginLimiterMaxAddresses

	widest := 0

	for i := 0; i < flood; i++ {
		at = at.Add(time.Millisecond)

		limiter.failed("192.0.2."+strconv.Itoa(i), 1)

		if len(limiter.failures) > widest {
			widest = len(limiter.failures)
		}
	}

	t.Logf("%d addresses left %d counters, the most held at once was %d",
		flood, len(limiter.failures), widest)

	// One over the ceiling is the account counter, which is not evicted and is
	// one entry per row of a table that holds one row.
	if widest > loginLimiterMaxAddresses+1 {
		t.Errorf("the limiter held %d counters at once, which is over the ceiling of %d",
			widest, loginLimiterMaxAddresses)
	}

	_, held = limiter.retryAfter("198.51.100.16", 1)
	if !held {
		t.Errorf("a flood of %d addresses lifted the hold off the account", flood)
	}

	// What is left over from the flood is counters that have run out once the
	// window has passed, and a second flood meets them. They go before anything
	// that is still counting does, so what is left at the end is the second
	// flood and not a mixture of the two.
	left := len(limiter.failures)

	at = at.Add(loginFailureWindow + time.Minute)

	const secondFlood = loginLimiterMaxAddresses - loginLimiterKeepAddresses + 200

	for i := 0; i < secondFlood; i++ {
		at = at.Add(time.Millisecond)

		limiter.failed("198.51.100."+strconv.Itoa(i), 1)
	}

	t.Logf("%d counters that had run out met %d new ones and left %d",
		left, secondFlood, len(limiter.failures))

	if len(limiter.failures) > secondFlood+1 {
		t.Errorf("the limiter holds %d counters, want no more than the %d of the second flood "+
			"and the account: the ones that had run out were not swept",
			len(limiter.failures), secondFlood)
	}
}

// The tests below are the other side of the same counters: the calls that ask
// for the account password again once a session is open. They are sent at a
// server wired the way main wires it, with the session middleware on the group
// and the login beside them, because what they are holding is that an attempt
// sent through one of those doors is counted on the counters the login counts
// on and not on a set of its own.

// clearLogsPath is the one of the six that is exercised here through
// accountPasswordRefused, the shared function the log, the update and the two
// host key approvals go through. accountPath, next to the tests of the change
// itself, is the one that compares the password where it stands.
const clearLogsPath = "/api/logs/clear"

// newPasswordCheckServer returns the server, the handler whose counters the
// calls land on, and the log file the emptying is pointed at.
func newPasswordCheckServer(t *testing.T) (*echo.Echo, *AuthHandler, string) {
	t.Helper()

	db := newAccountStubDB(t, newTestAccount(t, false))
	logPath := writeLog(t, `{"level":"info","msg":"something happened"}`)

	e := echo.New()
	e.Validator = &testValidator{validator: validator.New()}

	authHandler := NewAuthHandler(db, zap.NewNop(), filepath.Join(t.TempDir(), "initial-password"))
	logsHandler := NewLogsHandler(zap.NewNop(), logPath, db, func() error {
		return os.Truncate(logPath, 0)
	})

	g := e.Group("/api")
	g.Use(authHandler.RequireSession())
	g.POST("/login", authHandler.Login)
	g.POST("/logs/clear", logsHandler.ClearLogs)
	g.PUT("/account", authHandler.ChangeAccount)

	return e, authHandler, logPath
}

// signInFrom logs in from address and returns the cookies a client holds
// afterwards. The login is what the tests below get a session from, and it is
// sent from the address they go on to use: a login that opens the account
// forgets both counters, so everything counted below is what the call under
// test put there.
func signInFrom(t *testing.T, e *echo.Echo, address string) []*http.Cookie {
	t.Helper()

	rec := loginFrom(e, address, goodLogin)

	session := sessionCookieOf(rec)
	if session == nil {
		t.Fatalf("the login set no session cookie, body: %s", rec.Body.String())
	}

	csrf := csrfCookieOf(rec)
	if csrf == nil {
		t.Fatalf("the login set no %s cookie, body: %s", csrfCookieName, rec.Body.String())
	}

	return []*http.Cookie{session, csrf}
}

// passwordCheckFrom sends one call that asks for the account password. It is
// do with an address of its own, for the reason loginFrom has one.
func passwordCheckFrom(e *echo.Echo, address, method, target, body string,
	cookies []*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.RemoteAddr = address + ":54321"

	for _, cookie := range cookies {
		req.AddCookie(cookie)

		if cookie.Name == csrfCookieName {
			req.Header.Set(csrfHeaderName, cookie.Value)
		}
	}

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	return rec
}

// clearLogsWith sends one press of the button that empties the log, carrying
// password.
func clearLogsWith(e *echo.Echo, address, password string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	return passwordCheckFrom(e, address, http.MethodPost, clearLogsPath,
		`{"password":"`+password+`"}`, cookies)
}

// changeAccountWith sends one account change that renames the account, carrying
// password as the current one.
func changeAccountWith(e *echo.Echo, address, password string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	return passwordCheckFrom(e, address, http.MethodPut, accountPath,
		`{"current_password":"`+password+`","username":"someone-else"}`, cookies)
}

// failPasswordChecks sends count wrong passwords at the log and fails the test
// if any of them is answered with anything but the ordinary refusal.
func failPasswordChecks(t *testing.T, e *echo.Echo, address string, cookies []*http.Cookie, count int) {
	t.Helper()

	for i := 0; i < count; i++ {
		rec := clearLogsWith(e, address, mistypedPassword, cookies)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d from %s: status = %d, want %d, body: %s",
				i+1, address, rec.Code, http.StatusUnauthorized, rec.Body.String())
		}
	}
}

// TestTooManyWrongPasswordsOnACallThatAsksAgainAreRefused is the limit on the
// calls that ask for the account password behind a session. The password of the
// sixth attempt is never looked at, which is shown by sending the right one and
// being refused all the same, with the file it would have emptied still there.
func TestTooManyWrongPasswordsOnACallThatAsksAgainAreRefused(t *testing.T) {
	e, _, logPath := newPasswordCheckServer(t)

	const guesser = "198.51.100.30"

	cookies := signInFrom(t, e, guesser)

	failPasswordChecks(t, e, guesser, cookies, loginAddressFailureLimit)

	rec := clearLogsWith(e, guesser, mistypedPassword, cookies)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d: status = %d, want %d, body: %s",
			loginAddressFailureLimit+1, rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}

	code := refusalCode(t, rec)
	if code != passwordAttemptsCode {
		t.Errorf("error_code = %q, want %q", code, passwordAttemptsCode)
	}

	want := strconv.Itoa(int(loginAddressBlockFor / time.Second))

	got := rec.Result().Header.Get(echo.HeaderRetryAfter)
	if got != want {
		t.Errorf("Retry-After = %q, want %q", got, want)
	}

	t.Logf("the answer to attempt %d: %s, Retry-After: %s",
		loginAddressFailureLimit+1, rec.Body.String(), got)

	// The right password is refused as well, which is the only way from out
	// here to show that no password is being checked at all.
	rec = clearLogsWith(e, guesser, testPassword, cookies)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("the right password from a held address: status = %d, want %d, body: %s",
			rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}

	// And that the call itself never ran: the hold is reached before the work
	// the password stands in front of.
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("failed to stat the log file: %v", err)
	}

	if info.Size() == 0 {
		t.Error("the log was emptied by a call that was held")
	}
}

// TestTheLoginAndTheCallsThatAskAgainShareOneCounter is what the whole of this
// rests on. Were the two counted apart, the five tries the login allows would
// be five more on every call behind it, and the limit would be worth a
// fraction of what it reads as.
func TestTheLoginAndTheCallsThatAskAgainShareOneCounter(t *testing.T) {
	// The failures at the login hold the call behind it.
	e, _, _ := newPasswordCheckServer(t)

	const guesser = "198.51.100.31"

	cookies := signInFrom(t, e, guesser)

	failLogins(t, e, guesser, loginAddressFailureLimit)

	// With the right password, so that what refuses it is the hold and nothing
	// else.
	rec := clearLogsWith(e, guesser, testPassword, cookies)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after %d failed logins, the log was emptied with status = %d, want %d, body: %s",
			loginAddressFailureLimit, rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}

	t.Logf("%d failed logins held the call that asks for the password again: %s",
		loginAddressFailureLimit, rec.Body.String())

	// And the other way about: the failures on that call hold the login.
	e, _, _ = newPasswordCheckServer(t)

	const other = "198.51.100.32"

	cookies = signInFrom(t, e, other)

	failPasswordChecks(t, e, other, cookies, loginAddressFailureLimit)

	rec = loginFrom(e, other, goodLogin)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after %d wrong passwords on the log, the login answered %d, want %d, body: %s",
			loginAddressFailureLimit, rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}

	if sessionCookieOf(rec) != nil {
		t.Error("a held login handed out a session cookie")
	}
}

// TestAHeldPasswordCheckAndAHeldLoginAreWordedApart is the other side of the
// one counter: the hold is shared, and the sentence is not.
//
// A call that asks for the account password again is made by somebody who is
// logged in and was asked for their password on the way to something else. Were
// it refused under the login's code, the screen they are on would read the
// login's phrase and tell them that signing in was blocked, which is a thing
// they are not doing and, on the Logs screen, a thing they have no way to make
// sense of. So the code is checked here on both paths at once, from the one set
// of failures, which is the only place the two can be told apart.
func TestAHeldPasswordCheckAndAHeldLoginAreWordedApart(t *testing.T) {
	e, _, _ := newPasswordCheckServer(t)

	const guesser = "198.51.100.35"

	cookies := signInFrom(t, e, guesser)

	failPasswordChecks(t, e, guesser, cookies, loginAddressFailureLimit)

	held := clearLogsWith(e, guesser, mistypedPassword, cookies)
	if held.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d at the log: status = %d, want %d, body: %s",
			loginAddressFailureLimit+1, held.Code, http.StatusTooManyRequests, held.Body.String())
	}

	if code := refusalCode(t, held); code != passwordAttemptsCode {
		t.Errorf("the held password check: error_code = %q, want %q", code, passwordAttemptsCode)
	}

	// The sentence the answer carries is the one a client that reads no catalog
	// is left with, and it is the one the English catalog holds for that code.
	if strings.Contains(held.Body.String(), "sign in") {
		t.Errorf("the held password check speaks of signing in: %s", held.Body.String())
	}

	if want := strconv.Itoa(int(loginAddressBlockFor / time.Second)); !strings.Contains(held.Body.String(), want) {
		t.Errorf("the held password check does not say how long to wait (%s): %s", want, held.Body.String())
	}

	// The same failures, met at the login, which is a login and says so.
	login := loginFrom(e, guesser, goodLogin)
	if login.Code != http.StatusTooManyRequests {
		t.Fatalf("the login: status = %d, want %d, body: %s",
			login.Code, http.StatusTooManyRequests, login.Body.String())
	}

	if code := refusalCode(t, login); code != tooManyAttemptsCode {
		t.Errorf("the held login: error_code = %q, want %q", code, tooManyAttemptsCode)
	}

	t.Logf("one hold, two sentences: %s / %s", held.Body.String(), login.Body.String())
}

// TestAPasswordThatOpensTheAccountForgetsTheFailuresBeforeIt is what keeps the
// limit off the operator who mistyped on their way to the right password.
func TestAPasswordThatOpensTheAccountForgetsTheFailuresBeforeIt(t *testing.T) {
	e, h, _ := newPasswordCheckServer(t)

	const operator = "198.51.100.33"

	cookies := signInFrom(t, e, operator)

	failPasswordChecks(t, e, operator, cookies, loginAddressFailureLimit-1)

	// The address and the account, which is what four failures on one address
	// leave behind and what the success below has to clear.
	h.logins.mu.Lock()
	counted := len(h.logins.failures)
	h.logins.mu.Unlock()

	if counted != 2 {
		t.Fatalf("%d wrong passwords left %d counters, want 2",
			loginAddressFailureLimit-1, counted)
	}

	rec := clearLogsWith(e, operator, testPassword, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	h.logins.mu.Lock()
	left := len(h.logins.failures)
	h.logins.mu.Unlock()

	if left != 0 {
		t.Errorf("the limiter holds %d counters after a password that opened the account, want 0", left)
	}

	// Had the four before it been kept, the first of these would have been the
	// fifth and would have been held.
	failPasswordChecks(t, e, operator, cookies, loginAddressFailureLimit-1)

	t.Logf("%d wrong passwords, one that opened the account, and %d more, none of them held",
		loginAddressFailureLimit-1, loginAddressFailureLimit-1)
}

// TestTheAccountChangeIsHeldByTheSameCounter covers the other half of the six
// calls: the ones that do not go through accountPasswordRefused but compare the
// password where they stand. The account change is one of those, and the
// uninstall is the other.
func TestTheAccountChangeIsHeldByTheSameCounter(t *testing.T) {
	e, _, _ := newPasswordCheckServer(t)

	const guesser = "198.51.100.34"

	cookies := signInFrom(t, e, guesser)

	for i := 0; i < loginAddressFailureLimit; i++ {
		rec := changeAccountWith(e, guesser, mistypedPassword, cookies)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want %d, body: %s",
				i+1, rec.Code, http.StatusUnauthorized, rec.Body.String())
		}
	}

	rec := changeAccountWith(e, guesser, mistypedPassword, cookies)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d: status = %d, want %d, body: %s",
			loginAddressFailureLimit+1, rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}

	code := refusalCode(t, rec)
	if code != passwordAttemptsCode {
		t.Errorf("error_code = %q, want %q", code, passwordAttemptsCode)
	}

	if rec.Result().Header.Get(echo.HeaderRetryAfter) == "" {
		t.Error("the held account change was answered without Retry-After")
	}

	// The failures are on the counters the login is on, so the login is held
	// as well.
	rec = loginFrom(e, guesser, goodLogin)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("the login after %d wrong passwords on the account change: status = %d, want %d",
			loginAddressFailureLimit, rec.Code, http.StatusTooManyRequests)
	}
}
