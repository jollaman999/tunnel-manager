package api

import (
	"crypto/tls"
	"encoding/json"
	"net"
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
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v5"
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
	return loginCarrying(e, address, body)
}

// loginCarrying is loginFrom for a client that holds something, which is how a
// login carries the device value of a browser that signed in before, in the
// header carry puts it in. The host and the port are joined the way a socket
// address is written, so an IPv6 address arrives in brackets as it would from
// a real connection.
func loginCarrying(e *echo.Echo, address, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, loginPath, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.RemoteAddr = net.JoinHostPort(address, "54321")

	carry(req, cookies)

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
		attempt, _, held := limiter.begin(guesser, 1, "")
		if held {
			t.Fatalf("attempt %d of %d was held with nothing yet recorded", i+1, loginAddressFailureLimit)
		}

		attempts = append(attempts, attempt)
	}

	_, wait, held := limiter.begin(guesser, 1, "")
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
	other, _, held := limiter.begin("203.0.113.22", 1, "")
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

	again, _, held := limiter.begin(guesser, 1, "")
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

	_, wait, held = limiter.begin(guesser, 1, "")
	if !held || wait != loginAddressBlockFor {
		t.Errorf("after %d failures: held = %v, wait = %v, want true and %v",
			loginAddressFailureLimit, held, wait, loginAddressBlockFor)
	}

	if len(limiter.inFlight) != 0 {
		t.Errorf("the limiter still counts %d keys in flight, want 0", len(limiter.inFlight))
	}

	// A success forgets both counters and gives the place back.
	at = at.Add(loginAddressBlockFor)

	attempt, _, held := limiter.begin(guesser, 1, "")
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
// itself, is one of the two that reserve their place through passwordBegin
// where they stand; the uninstall, tested beside itself, is the other.
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

	key := csrfKeyOf(rec)
	if key == nil {
		t.Fatalf("the login answered with no csrf_token, body: %s", rec.Body.String())
	}

	return []*http.Cookie{session, key}
}

// passwordCheckFrom sends one call that asks for the account password. It is
// do with an address of its own, for the reason loginFrom has one.
func passwordCheckFrom(e *echo.Echo, address, method, target, body string,
	cookies []*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.RemoteAddr = address + ":54321"

	carry(req, cookies)

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

// sendAtOnce runs send sent times, every one of them let go at the same
// moment, and returns the status each was answered with.
func sendAtOnce(sent int, send func() int) []int {
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

			codes[i] = send()
		}(i)
	}

	ready.Wait()
	close(start)
	done.Wait()

	return codes
}

// checkedAtOnce fails the test when more of the wrong passwords answered with
// codes reached the compare than the limit of one address allows, or when
// none did, or when the limiter still holds a place for any of them. A 401 is
// a password that was compared and a 429 one that was not.
func checkedAtOnce(t *testing.T, limiter *loginLimiter, call string, codes []int) {
	t.Helper()

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

	t.Logf("%d wrong passwords sent at once at %s: %d had the password checked, %d were refused as held",
		len(codes), call, checked, refused)

	if checked > loginAddressFailureLimit {
		t.Errorf("%d of %d wrong passwords sent at once had the password checked, want no more than %d",
			checked, len(codes), loginAddressFailureLimit)
	}

	if checked == 0 {
		t.Errorf("none of %d wrong passwords had the password checked, want at least one", len(codes))
	}

	limiter.mu.Lock()
	inFlight := len(limiter.inFlight)
	limiter.mu.Unlock()

	if inFlight != 0 {
		t.Errorf("the limiter still counts %d keys in flight after every call came back, want 0", inFlight)
	}
}

// TestWrongPasswordsSentAtOnceOnACallThatAsksAgainAreCountedAsTheyAreLetThrough
// is TestWrongLoginsSentAtOnceAreCountedAsTheyAreLetThrough for the calls
// behind a session. Whoever holds a session can send the log's button as many
// times at once as the network carries, and were the failures counted only
// once bcrypt has answered, every one of those would find the counter where
// the first did and every one would reach the compare.
func TestWrongPasswordsSentAtOnceOnACallThatAsksAgainAreCountedAsTheyAreLetThrough(t *testing.T) {
	e, h, logPath := newPasswordCheckServer(t)

	const guesser = "198.51.100.36"

	cookies := signInFrom(t, e, guesser)

	codes := sendAtOnce(8*loginAddressFailureLimit, func() int {
		return clearLogsWith(e, guesser, mistypedPassword, cookies).Code
	})

	checkedAtOnce(t, h.logins, clearLogsPath, codes)

	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("failed to stat the log file: %v", err)
	}

	if info.Size() == 0 {
		t.Error("the log was emptied by a wrong password")
	}
}

// TestWrongPasswordsSentAtOnceOnTheAccountChangeAreCountedAsTheyAreLetThrough
// is the same for the account change, which compares the password where it
// stands rather than through accountPasswordRefused and so reserves its place
// on its own.
func TestWrongPasswordsSentAtOnceOnTheAccountChangeAreCountedAsTheyAreLetThrough(t *testing.T) {
	e, h, _ := newPasswordCheckServer(t)

	const guesser = "198.51.100.37"

	cookies := signInFrom(t, e, guesser)

	codes := sendAtOnce(8*loginAddressFailureLimit, func() int {
		return changeAccountWith(e, guesser, mistypedPassword, cookies).Code
	})

	checkedAtOnce(t, h.logins, accountPath, codes)

	user, err := h.readUser()
	if err != nil {
		t.Fatalf("failed to read the account: %v", err)
	}

	if user.Username == "someone-else" {
		t.Error("the account was renamed by a wrong password")
	}
}

// TestTheAccountChangeIsHeldByTheSameCounter covers the other half of the six
// calls: the ones that do not go through accountPasswordRefused but reserve
// their place and compare the password where they stand. The account change is
// one of those, and the uninstall is the other.
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

// The tests below are the device value: what lets the operator's browser past
// a hold on the account that somebody else brought about.

// deviceKey is a device value in the shape carry takes it in. It is named after
// the header it goes out in, which no cookie is, so carry can tell it from the
// cookies it travels among and put it where the page puts it.
func deviceKey(value string) *http.Cookie {
	return &http.Cookie{Name: deviceHeaderName, Value: value}
}

// deviceKeyOf returns the device value the answer carried in its body, as
// deviceKey makes it, or nil where the answer carries none. The body is the one
// place the server hands it out.
func deviceKeyOf(rec *httptest.ResponseRecorder) *http.Cookie {
	var resp testResponse

	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil || resp.Data.Device == "" {
		return nil
	}

	return deviceKey(resp.Data.Device)
}

// deviceCookieOf returns the cookie the answer set under the name the device
// value went out in before it was taken out of cookies, or nil.
func deviceCookieOf(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, cookie := range rec.Result().Cookies() {
		if isCookieFor(cookie, deviceCookieName) {
			return cookie
		}
	}

	return nil
}

// holdTheAccount is the attack the device value is for, as it was measured
// against a running server: five wrong passwords from each of 127.0.0.2 to
// 127.0.0.7. No address goes past its own limit and the thirty together hold
// the account, which no address the operator could come from is spared.
func holdTheAccount(t *testing.T, e *echo.Echo) {
	t.Helper()

	for i := 0; i < loginAccountFailureLimit/loginAddressFailureLimit; i++ {
		failLogins(t, e, "127.0.0."+strconv.Itoa(2+i), loginAddressFailureLimit)
	}
}

// deviceFrom logs in from address with the right password and returns the
// device value the answer handed out, failing the test where there is none.
func deviceFrom(t *testing.T, e *echo.Echo, address string) *http.Cookie {
	t.Helper()

	rec := loginFrom(e, address, goodLogin)
	if rec.Code != http.StatusOK {
		t.Fatalf("the login that hands out the device value: status = %d, want %d, body: %s",
			rec.Code, http.StatusOK, rec.Body.String())
	}

	device := deviceKeyOf(rec)
	if device == nil {
		t.Fatalf("a login with the right password answered with no device, body: %s", rec.Body.String())
	}

	return device
}

// TestADeviceValueGetsPastAHoldOnTheAccount is the lockout the value exists
// to end. Once the account is held, the right password from an address that has
// never sent anything is refused, and the same password from a browser that
// signed in before, sending the value in its header, is let in.
func TestADeviceValueGetsPastAHoldOnTheAccount(t *testing.T) {
	e, _ := newTestServer(t, newTestAccount(t, false))

	signedIn := loginFrom(e, "127.0.0.1", goodLogin)
	if signedIn.Code != http.StatusOK {
		t.Fatalf("the first login: status = %d, want %d, body: %s",
			signedIn.Code, http.StatusOK, signedIn.Body.String())
	}

	device := deviceKeyOf(signedIn)
	if device == nil {
		t.Fatalf("a login with the right password answered with no device, body: %s", signedIn.Body.String())
	}

	holdTheAccount(t, e)

	// Without the value it is still the hold it was: what the account counter
	// is for has not changed.
	stranger := loginFrom(e, "127.0.0.8", goodLogin)
	if stranger.Code != http.StatusTooManyRequests {
		t.Errorf("the right password without a device value: status = %d, want %d, body: %s",
			stranger.Code, http.StatusTooManyRequests, stranger.Body.String())
	}

	if got, want := stranger.Result().Header.Get(echo.HeaderRetryAfter),
		strconv.Itoa(int(loginAccountBlockFor/time.Second)); got != want {
		t.Errorf("Retry-After without a device value = %q, want %q", got, want)
	}

	operator := loginCarrying(e, "127.0.0.9", goodLogin, device)
	if operator.Code != http.StatusOK {
		t.Fatalf("the right password with the device value: status = %d, want %d, body: %s",
			operator.Code, http.StatusOK, operator.Body.String())
	}

	if sessionCookieOf(operator) == nil {
		t.Error("the login with the device value set no session cookie")
	}

	t.Logf("account held: without the value %d %s, with it %d",
		stranger.Code, stranger.Body.String(), operator.Code)
}

// TestTheDeviceValueIsHandedOutInTheBodyAndInNoCookie is where the value goes.
// A cookie is sent to every port of the host name, so a cookie that carried it
// would hand it to whatever runs on another port; the session cookie is the one
// cookie a login sets, and the value is in the body, signed for this account.
func TestTheDeviceValueIsHandedOutInTheBodyAndInNoCookie(t *testing.T) {
	user := newTestAccount(t, false)
	e, _ := newTestServer(t, user)

	for _, overTLS := range []bool{false, true} {
		rec, _ := loginOverTLS(t, e, overTLS, goodLogin)
		if rec.Code != http.StatusOK {
			t.Fatalf("over TLS %v: status = %d, want %d, body: %s", overTLS, rec.Code, http.StatusOK, rec.Body.String())
		}

		names := []string{}
		for _, cookie := range rec.Result().Cookies() {
			names = append(names, cookie.Name)
		}

		if want := cookieName(sessionCookieName, overTLS); len(names) != 1 || names[0] != want {
			t.Errorf("over TLS %v: the login set the cookies %v, want %s alone", overTLS, names, want)
		}

		device := deviceKeyOf(rec)
		if device == nil {
			t.Fatalf("over TLS %v: the login answered with no device, body: %s", overTLS, rec.Body.String())
		}

		if deviceOf(device.Value, &user, time.Now()) == "" {
			t.Errorf("over TLS %v: the device the login answered with does not hold: %q", overTLS, device.Value)
		}
	}
}

// TestAnOldDeviceCookieCountsForNothingAndIsDropped is a browser that signed in
// under a version that handed the value out in a cookie. The cookie is not
// read: a login that carries a value that holds in it, and nothing in the
// header, is counted the way one with no value is and meets the hold on the
// account. The same value in the header is let through, so what refused it is
// where it came from. Every answer to a request that carries the cookie drops
// it, under the name it came under.
func TestAnOldDeviceCookieCountsForNothingAndIsDropped(t *testing.T) {
	tests := []struct {
		name    string
		overTLS bool
		cookie  string
	}{
		{name: "the bare name over plain HTTP", cookie: deviceCookieName},
		{name: "the guarded name over TLS", overTLS: true, cookie: hostCookiePrefix + deviceCookieName},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _ := newTestServer(t, newTestAccount(t, false))

			device := deviceFrom(t, e, "127.0.0.1")
			old := &http.Cookie{Name: tt.cookie, Value: device.Value}

			holdTheAccount(t, e)

			req := httptest.NewRequest(http.MethodPost, loginPath, strings.NewReader(goodLogin))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			req.RemoteAddr = net.JoinHostPort("127.0.0.9", "54321")

			if tt.overTLS {
				req.TLS = &tls.ConnectionState{}
			}

			carry(req, []*http.Cookie{old})

			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)

			if rec.Code != http.StatusTooManyRequests {
				t.Errorf("the right password with the value in an old cookie: status = %d, want %d, body: %s",
					rec.Code, http.StatusTooManyRequests, rec.Body.String())
			}

			for _, step := range []struct {
				what string
				rec  *httptest.ResponseRecorder
			}{
				{"the login", rec},
				{"a read", doOverTLS(e, tt.overTLS, http.MethodGet, "/api/host", "", old)},
			} {
				dropped := deviceCookieOf(step.rec)
				if dropped == nil {
					t.Errorf("%s: nothing was set under the %s name", step.what, tt.cookie)

					continue
				}

				if dropped.Name != tt.cookie || dropped.MaxAge >= 0 || dropped.Value != "" || dropped.Path != "/" {
					t.Errorf("%s: the %s cookie was not dropped: %s set with MaxAge %d, Path %q and a value of %d bytes",
						step.what, tt.cookie, dropped.Name, dropped.MaxAge, dropped.Path, len(dropped.Value))
				}

				if tt.overTLS && !dropped.Secure {
					t.Errorf("%s: the guarded name was dropped without Secure, which a browser ignores", step.what)
				}
			}

			rec = loginCarrying(e, "127.0.0.10", goodLogin, device)
			if rec.Code != http.StatusOK {
				t.Errorf("the right password with the same value in the header: status = %d, want %d, body: %s",
					rec.Code, http.StatusOK, rec.Body.String())
			}

			if cookie := deviceCookieOf(rec); cookie != nil {
				t.Errorf("a login that carried no old cookie set %s, MaxAge %d", cookie.Name, cookie.MaxAge)
			}
		})
	}
}

// TestADeviceValueThatDoesNotHoldIsIgnored is the other side: anything but a
// value this server signed for these credentials less than its lifetime ago
// leaves the attempt counted against the account, and the hold reaches it.
func TestADeviceValueThatDoesNotHoldIsIgnored(t *testing.T) {
	user := newTestAccount(t, false)

	otherHash := newTestAccount(t, false)

	renamed := user
	renamed.Username = "someone-else"

	tampered := func(c *http.Cookie) *http.Cookie {
		forged := *c
		last := forged.Value[len(forged.Value)-1]

		replacement := byte('A')
		if last == 'A' {
			replacement = 'B'
		}

		forged.Value = forged.Value[:len(forged.Value)-1] + string(replacement)

		return &forged
	}

	signed := func(c *http.Cookie, by models.User) *http.Cookie {
		forged := *c
		forged.Value = deviceValue(&by, time.Now())

		return &forged
	}

	cases := []struct {
		name  string
		forge func(*http.Cookie) *http.Cookie
		wait  time.Duration
	}{
		{name: "a signature that was changed", forge: tampered},
		{name: "signed under another password hash", forge: func(c *http.Cookie) *http.Cookie { return signed(c, otherHash) }},
		{name: "signed under another username", forge: func(c *http.Cookie) *http.Cookie { return signed(c, renamed) }},
		{name: "not a device value at all", forge: func(c *http.Cookie) *http.Cookie {
			forged := *c
			forged.Value = "anything"

			return &forged
		}},
		{name: "past its lifetime", forge: func(c *http.Cookie) *http.Cookie { return c }, wait: deviceLifetime},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, h := newTestServer(t, user)
			advance, _ := heldClock(h)

			device := tc.forge(deviceFrom(t, e, "127.0.0.1"))

			advance(tc.wait)
			holdTheAccount(t, e)

			rec := loginCarrying(e, "127.0.0.9", goodLogin, device)
			if rec.Code != http.StatusTooManyRequests {
				t.Errorf("status = %d, want %d, body: %s", rec.Code, http.StatusTooManyRequests, rec.Body.String())
			}
		})
	}

	// One second short of the lifetime the same value still holds, so what
	// refused it above is the date and nothing else.
	e, h := newTestServer(t, user)
	advance, _ := heldClock(h)

	device := deviceFrom(t, e, "127.0.0.1")

	advance(deviceLifetime - time.Second)
	holdTheAccount(t, e)

	rec := loginCarrying(e, "127.0.0.9", goodLogin, device)
	if rec.Code != http.StatusOK {
		t.Errorf("a second before the lifetime ends: status = %d, want %d, body: %s",
			rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestDeviceOfReadsOnlyAValueThatHolds is deviceOf on its own, for the cases
// a login cannot reach: another account id, and an account with no hash, which
// would otherwise be a key anyone can sign with.
func TestDeviceOfReadsOnlyAValueThatHolds(t *testing.T) {
	user := newTestAccount(t, false)
	at := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)

	value := deviceValue(&user, at)

	nonce := deviceOf(value, &user, at)
	if nonce == "" {
		t.Fatalf("a value handed out a moment ago does not hold: %q", value)
	}

	if again := deviceOf(deviceValue(&user, at), &user, at); again == nonce {
		t.Errorf("two values handed out at the same moment share the nonce %q", nonce)
	}

	otherID := user
	otherID.ID = 2

	noHash := user
	noHash.PasswordHash = ""

	unsigned := deviceValue(&noHash, at)

	cases := []struct {
		name  string
		value string
		user  models.User
		now   time.Time
	}{
		{name: "another account id", value: value, user: otherID, now: at},
		{name: "an account with no hash", value: unsigned, user: noHash, now: at},
		{name: "the end of its lifetime", value: value, user: user, now: at.Add(deviceLifetime)},
		{name: "empty", value: "", user: user, now: at},
		{name: "no signature", value: value[:strings.LastIndexByte(value, '.')], user: user, now: at},
	}

	for _, tc := range cases {
		got := deviceOf(tc.value, &tc.user, tc.now)
		if got != "" {
			t.Errorf("%s: deviceOf = %q, want nothing", tc.name, got)
		}
	}
}

// TestADeviceValueHasALimitOfItsOwn is what keeps a stolen value from being a
// way to guess with the account counter switched off. Its wrong passwords are
// not counted on the account, they are counted on the value, and once the
// value has met its limit it is trusted no more than no value at all.
func TestADeviceValueHasALimitOfItsOwn(t *testing.T) {
	e, h := newTestServer(t, newTestAccount(t, false))

	device := deviceFrom(t, e, "127.0.0.1")

	// Each from an address of its own, so that no address counter is what
	// holds any of them.
	for i := 0; i < loginDeviceFailureLimit-1; i++ {
		rec := loginCarrying(e, "198.51.100."+strconv.Itoa(100+i), badLogin, device)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong password %d with the value: status = %d, want %d, body: %s",
				i+1, rec.Code, http.StatusUnauthorized, rec.Body.String())
		}
	}

	h.logins.mu.Lock()
	_, accountCounted := h.logins.failures[loginKey{kind: loginKeyAccount, name: "1"}]
	devices := 0

	for key, entry := range h.logins.failures {
		if key.kind == loginKeyDevice && entry.count == loginDeviceFailureLimit-1 {
			devices++
		}
	}
	h.logins.mu.Unlock()

	if accountCounted {
		t.Error("wrong passwords sent with a device value were counted against the account")
	}

	if devices != 1 {
		t.Errorf("%d device counters hold %d failures, want 1", devices, loginDeviceFailureLimit-1)
	}

	holdTheAccount(t, e)

	// The account is held now and the value still has one failure to go, so
	// it is let through and counted.
	rec := loginCarrying(e, "198.51.100.150", badLogin, device)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password %d with the value while the account is held: status = %d, want %d, body: %s",
			loginDeviceFailureLimit, rec.Code, http.StatusUnauthorized, rec.Body.String())
	}

	// That was its limit. The right password with it is now counted the way it
	// would be without it, and meets the hold on the account.
	rec = loginCarrying(e, "198.51.100.151", goodLogin, device)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("the right password with a value past its limit: status = %d, want %d, body: %s",
			rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}
}

// TestACallThatAsksAgainHonoursTheDeviceValue is the same lockout one step
// later: the operator signed in before the hold, and a call that asks for the
// password again would otherwise be refused because of what somebody else sent
// at the login.
func TestACallThatAsksAgainHonoursTheDeviceValue(t *testing.T) {
	e, _, logPath := newPasswordCheckServer(t)

	signedIn := loginFrom(e, "127.0.0.1", goodLogin)
	cookies := []*http.Cookie{sessionCookieOf(signedIn), csrfKeyOf(signedIn)}
	device := deviceKeyOf(signedIn)

	if cookies[0] == nil || cookies[1] == nil || device == nil {
		t.Fatalf("the login set session %v, CSRF %v and device %v", cookies[0], cookies[1], device)
	}

	holdTheAccount(t, e)

	rec := clearLogsWith(e, "127.0.0.1", testPassword, cookies)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the right password without the device value: status = %d, want %d, body: %s",
			rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}

	rec = clearLogsWith(e, "127.0.0.1", testPassword, append(cookies, device))
	if rec.Code != http.StatusOK {
		t.Fatalf("the right password with the device value: status = %d, want %d, body: %s",
			rec.Code, http.StatusOK, rec.Body.String())
	}

	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("failed to stat the log file: %v", err)
	}

	if info.Size() != 0 {
		t.Error("the log was not emptied by the call that was let through")
	}
}

// TestChangingTheCredentialsEndsTheDeviceValuesButOne is the account change.
// Every value handed out before it was signed under the credentials it
// replaced and stops holding; the browser that made the change is handed one
// under the new ones.
func TestChangingTheCredentialsEndsTheDeviceValuesButOne(t *testing.T) {
	for _, change := range []struct {
		name, username, newPassword, loginAs, password string
	}{
		{name: "the password", newPassword: testNewPassword, loginAs: testUsername, password: testNewPassword},
		{name: "the username", username: accountNewUsername, loginAs: accountNewUsername, password: testPassword},
	} {
		t.Run(change.name, func(t *testing.T) {
			f := newAccountFixture(t)

			other := deviceFrom(t, f.e, "127.0.0.1")

			signedIn := loginFrom(f.e, "127.0.0.2", goodLogin)
			changer := deviceKeyOf(signedIn)
			cookies := []*http.Cookie{sessionCookieOf(signedIn), csrfKeyOf(signedIn), changer}

			rec := f.changeAccount(t, cookies, testPassword, change.username, change.newPassword)
			if rec.Code != http.StatusOK {
				t.Fatalf("the change: status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
			}

			renewed := deviceKeyOf(rec)
			if renewed == nil {
				t.Fatalf("the change answered with no device, body: %s", rec.Body.String())
			}

			if cookie := deviceCookieOf(rec); cookie != nil {
				t.Errorf("the change set the %s cookie", cookie.Name)
			}

			holdTheAccount(t, f.e)

			body := loginBody(t, change.loginAs, change.password)

			for _, stale := range []*http.Cookie{other, changer} {
				rec = loginCarrying(f.e, "127.0.0.9", body, stale)
				if rec.Code != http.StatusTooManyRequests {
					t.Errorf("a value handed out before the change: status = %d, want %d, body: %s",
						rec.Code, http.StatusTooManyRequests, rec.Body.String())
				}
			}

			rec = loginCarrying(f.e, "127.0.0.9", body, renewed)
			if rec.Code != http.StatusOK {
				t.Errorf("the value the change handed out: status = %d, want %d, body: %s",
					rec.Code, http.StatusOK, rec.Body.String())
			}
		})
	}
}

// TestTheSetupHandsOutADeviceValueUnderTheNewCredentials is the same for the
// setup, which replaces the initial password the first login was signed under.
func TestTheSetupHandsOutADeviceValueUnderTheNewCredentials(t *testing.T) {
	f := newAccountFixture(t)
	f.e.POST(setupPath, f.auth.Setup, f.auth.RequireSession())

	err := f.db.Model(&models.User{}).Where("1 = 1").
		Updates(map[string]interface{}{"setup_required": true, "username": ""}).Error
	if err != nil {
		t.Fatalf("failed to put the account back before its setup: %v", err)
	}

	first := loginFrom(f.e, "127.0.0.1", loginBody(t, "", testPassword))
	initial := deviceKeyOf(first)
	cookies := []*http.Cookie{sessionCookieOf(first), csrfKeyOf(first), initial}

	if cookies[0] == nil || initial == nil {
		t.Fatalf("the first login set session %v and device %v, body: %s", cookies[0], initial, first.Body.String())
	}

	rec := do(f.e, http.MethodPost, setupPath, loginBody(t, testUsername, testNewPassword), cookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("the setup: status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	renewed := deviceKeyOf(rec)
	if renewed == nil {
		t.Fatalf("the setup answered with no device, body: %s", rec.Body.String())
	}

	if cookie := deviceCookieOf(rec); cookie != nil {
		t.Errorf("the setup set the %s cookie", cookie.Name)
	}

	holdTheAccount(t, f.e)

	body := loginBody(t, testUsername, testNewPassword)

	rec = loginCarrying(f.e, "127.0.0.9", body, initial)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("the value of the initial password: status = %d, want %d, body: %s",
			rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}

	rec = loginCarrying(f.e, "127.0.0.9", body, renewed)
	if rec.Code != http.StatusOK {
		t.Errorf("the value the setup handed out: status = %d, want %d, body: %s",
			rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestTheLogoutLeavesTheDeviceValue holds that signing out does not make the
// browser a stranger: the logout sets nothing under the old device name, and
// the value the browser kept still gets it past a hold on the account after it.
func TestTheLogoutLeavesTheDeviceValue(t *testing.T) {
	e, _ := newTestServer(t, newTestAccount(t, false))

	signedIn := loginFrom(e, "127.0.0.1", goodLogin)
	device := deviceKeyOf(signedIn)
	cookies := []*http.Cookie{sessionCookieOf(signedIn), csrfKeyOf(signedIn), device}

	if cookies[0] == nil || cookies[1] == nil || device == nil {
		t.Fatalf("the login set session %v, CSRF %v and device %v", cookies[0], cookies[1], device)
	}

	rec := do(e, http.MethodPost, logoutPath, "", cookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("the logout: status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if cookie := deviceCookieOf(rec); cookie != nil {
		t.Errorf("the logout set the %s cookie: %+v", cookie.Name, cookie)
	}

	holdTheAccount(t, e)

	rec = loginCarrying(e, "127.0.0.9", goodLogin, device)
	if rec.Code != http.StatusOK {
		t.Errorf("the right password with the value kept over the logout: status = %d, want %d, body: %s",
			rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestAnIPv6AddressIsCountedByItsSlash64 is the address counter over IPv6.
// Every address of a /64 is one sender's to choose, so they are one counter;
// the next /64 over is somebody else.
func TestAnIPv6AddressIsCountedByItsSlash64(t *testing.T) {
	e, _ := newTestServer(t, newTestAccount(t, false))

	for i := 1; i <= loginAddressFailureLimit; i++ {
		failLogins(t, e, "2001:db8::"+strconv.Itoa(i), 1)
	}

	rec := loginFrom(e, "2001:db8::ffff:1", goodLogin)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("another address in the same /64: status = %d, want %d, body: %s",
			rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}

	rec = loginFrom(e, "2001:db8:0:1::1", badLogin)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("an address in the next /64: status = %d, want %d, body: %s",
			rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}

// TestLoginAddressKey covers the names the address counter goes by, the IPv4
// address written in its IPv6 form among them: grouped by /64 as it stands,
// every client that came in on IPv4 through a dual stack socket would share one
// counter.
func TestLoginAddressKey(t *testing.T) {
	cases := map[string]string{
		"192.0.2.1":           "192.0.2.1",
		"::ffff:192.0.2.1":    "192.0.2.1",
		"2001:db8::1":         "2001:db8::/64",
		"2001:db8::ffff:ffff": "2001:db8::/64",
		"2001:db8:0:1::1":     "2001:db8:0:1::/64",
		"fe80::1%eth0":        "fe80::/64",
		"not-an-address":      "not-an-address",
	}

	for address, want := range cases {
		got := loginAddressKey(address)
		if got != want {
			t.Errorf("loginAddressKey(%q) = %q, want %q", address, got, want)
		}
	}
}
