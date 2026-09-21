package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/auth"
	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/crypto/ssh"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// hostKeyAccountPassword is what the account in these tests is opened with. It
// is the password an approval that replaces a trusted key has to carry.
const hostKeyAccountPassword = "the-correct-horse-battery" // hook:allow

// approvalDeadline is how long one approval is given before the test says it
// is not coming back. It is far longer than an approval takes - the slowest
// part of one is a single bcrypt comparison of some hundred milliseconds - so
// it measures nothing; it is there so that a call that waits forever is a
// failure of one test rather than a timeout of the run.
const approvalDeadline = 30 * time.Second

// newHostKey returns a host key in the form a Host row holds one, together
// with its fingerprint. A real key is made rather than a line written out by
// hand, because the fingerprint under test is a digest of the key and there is
// nothing to compare it against unless the key is one.
func newHostKey(t *testing.T) (stored, fingerprint string) {
	t.Helper()

	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to make a key: %v", err)
	}

	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatalf("failed to read the key as an SSH key: %v", err)
	}

	stored = tunnel.MarshalHostKey(key)

	return stored, tunnel.HostKeyFingerprint(stored)
}

// newHostKeyDB opens a real database holding the account and the one Host the
// approval is asked about. The rows are read back after the call, which is
// what says whether the trusted key moved, so the stubs the other handler
// tests answer reads from are no use here.
//
// The pool is held to one connection, the way database.New holds it. It is
// not a detail of this test: a handler that reads through the default handle
// while it holds a transaction open is waiting for the connection it is
// holding itself, and on a pool that hands out as many connections as are
// asked for it goes through and nothing is noticed. That is how such a read
// reached a server and stopped it. Every approval in this file runs against
// the pool the server has.
func newHostKeyDB(t *testing.T, host models.Host) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "hostkey.db")),
		&gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to reach the connection pool: %v", err)
	}

	sqlDB.SetMaxOpenConns(1)

	err = db.AutoMigrate(&models.Host{}, &models.User{})
	if err != nil {
		t.Fatalf("failed to migrate the database: %v", err)
	}

	hash, err := auth.HashPassword(hostKeyAccountPassword)
	if err != nil {
		t.Fatalf("failed to hash the password: %v", err)
	}

	err = db.Create(&models.User{Username: "operator", PasswordHash: hash}).Error
	if err != nil {
		t.Fatalf("failed to create the account: %v", err)
	}

	err = db.Create(&host).Error
	if err != nil {
		t.Fatalf("failed to create the Host: %v", err)
	}

	return db
}

// hostKeyHost is the Host the approval is asked about, carrying the trusted
// key and the key that is waiting it is given.
func hostKeyHost(trusted, pending string) models.Host {
	host := storedHost()
	host.HostKey = trusted
	host.PendingHostKey = pending

	return host
}

// approveHostKeyAs runs one approval. withAccount says whether the account the
// middleware leaves on the context is there, which is how a route hung outside
// that middleware is told apart.
func approveHostKeyAs(t *testing.T, db *gorm.DB, body string,
	withAccount bool) (*httptest.ResponseRecorder, *wakeRecorder) {
	t.Helper()

	rec, manager, _ := approveHostKeyLogged(t, db, body, withAccount)

	return rec, manager
}

// approveHostKeyLogged runs one on a logger whose lines can be read back. The
// approval writes what a change of trust is recorded by, and the only way to
// hold a line to what it says and to what it must not say is to read it.
func approveHostKeyLogged(t *testing.T, db *gorm.DB, body string,
	withAccount bool) (*httptest.ResponseRecorder, *wakeRecorder, *observer.ObservedLogs) {
	t.Helper()

	// Every level is kept. The approval is an Info and the refusal a Warn, and
	// one of the tests reads every line looking for the password in it.
	core, logs := observer.New(zap.DebugLevel)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/host/1/host-key", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("1")

	if withAccount {
		c.Set(contextUserIDKey, uint(1))
	}

	manager := &wakeRecorder{tx: &txConnPool{}}
	h := NewHandler(db, manager, zap.New(core), newTestCipher(t))

	// The call is run beside the test rather than in it. On the pool of one
	// connection newHostKeyDB opens, a call that reads through the default
	// handle while it holds the transaction open waits for the connection it
	// is holding itself and never comes back, and a test that waits with it
	// ends the whole package on the -timeout of the run, with a stack dump of
	// every test that was going at the time and no word of which call is the
	// one that stopped. Waiting here with a deadline turns that into a
	// failure that names it.
	done := make(chan error, 1)

	go func() {
		done <- h.ApproveHostKey(c)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ApproveHostKey returned error: %v", err)
		}
	case <-time.After(approvalDeadline):
		t.Fatalf("ApproveHostKey did not come back within %v: it is waiting on the database, which "+
			"is what a read through the default handle inside the transaction does on the one "+
			"connection the server runs on", approvalDeadline)
	}

	return rec, manager, logs
}

func approveHostKey(t *testing.T, db *gorm.DB, body string) (*httptest.ResponseRecorder, *wakeRecorder) {
	t.Helper()

	return approveHostKeyAs(t, db, body, true)
}

// storedKeys reads the two host key columns back out of the database.
func storedKeys(t *testing.T, db *gorm.DB) (trusted, pending string) {
	t.Helper()

	var host models.Host

	err := db.First(&host, 1).Error
	if err != nil {
		t.Fatalf("failed to read the Host back: %v", err)
	}

	return host.HostKey, host.PendingHostKey
}

// refusalCode reads the code a refusal went out under.
func refusalCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	var body struct {
		Code string `json:"error_code"`
	}

	err := json.Unmarshal(rec.Body.Bytes(), &body)
	if err != nil {
		t.Fatalf("failed to read the answer %q: %v", rec.Body.String(), err)
	}

	return body.Code
}

// TestAFirstHostKeyIsApprovedWithoutThePasswordOfTheAccount covers the
// approval that happens on every Host that is registered. There is no trust to
// overturn, so the session is all that is asked for; a password on this click
// would be a password on every registration.
func TestAFirstHostKeyIsApprovedWithoutThePasswordOfTheAccount(t *testing.T) {
	presented, fingerprint := newHostKey(t)
	db := newHostKeyDB(t, hostKeyHost("", presented))

	rec, manager := approveHostKey(t, db, `{"fingerprint":"`+fingerprint+`"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/host/1/host-key = %d, want %d, body: %s", rec.Code, http.StatusOK,
			rec.Body.String())
	}

	trusted, pending := storedKeys(t, db)
	if trusted != presented {
		t.Errorf("the trusted key is %q, want the key that was presented, %q", trusted, presented)
	}

	if pending != "" {
		t.Errorf("the key that was waiting is still there: %q", pending)
	}

	// The tunnels of this Host were refused against the key it carried before,
	// and a refused tunnel has no connection left whose ending would build it
	// again. The approval is what has to ask for the pass.
	wakes, _ := manager.counts()
	if wakes != 1 {
		t.Errorf("the approval woke the reconcile loop %d times, want 1", wakes)
	}
}

// TestAHostKeyThatReplacesTheTrustedOneIsRefusedWithTheWrongPassword is the
// check the whole screen hangs on. A session left open on an unattended screen
// must not be one click away from trusting whatever is answering in place of
// the server.
func TestAHostKeyThatReplacesTheTrustedOneIsRefusedWithTheWrongPassword(t *testing.T) {
	trusted, _ := newHostKey(t)
	presented, fingerprint := newHostKey(t)
	db := newHostKeyDB(t, hostKeyHost(trusted, presented))

	rec, manager := approveHostKey(t, db,
		`{"fingerprint":"`+fingerprint+`","password":"not the password"}`) // hook:allow

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /api/host/1/host-key = %d, want %d, body: %s", rec.Code,
			http.StatusUnauthorized, rec.Body.String())
	}

	if code := refusalCode(t, rec); code != "host.host_key.password_wrong" {
		t.Errorf("error_code = %q, want %q", code, "host.host_key.password_wrong")
	}

	stillTrusted, stillPending := storedKeys(t, db)
	if stillTrusted != trusted {
		t.Errorf("the trusted key was replaced by an approval that was refused: %q", stillTrusted)
	}

	if stillPending != presented {
		t.Errorf("the key that is waiting is %q, want it left where it was, %q", stillPending, presented)
	}

	wakes, _ := manager.counts()
	if wakes != 0 {
		t.Errorf("an approval that was refused woke the reconcile loop %d times", wakes)
	}
}

// TestAHostKeyThatReplacesTheTrustedOneIsApprovedWithThePassword is the other
// half of that rule: the password that opens the account lets the replacement
// through.
func TestAHostKeyThatReplacesTheTrustedOneIsApprovedWithThePassword(t *testing.T) {
	trusted, _ := newHostKey(t)
	presented, fingerprint := newHostKey(t)
	db := newHostKeyDB(t, hostKeyHost(trusted, presented))

	rec, _ := approveHostKey(t, db,
		`{"fingerprint":"`+fingerprint+`","password":"`+hostKeyAccountPassword+`"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/host/1/host-key = %d, want %d, body: %s", rec.Code, http.StatusOK,
			rec.Body.String())
	}

	nowTrusted, pending := storedKeys(t, db)
	if nowTrusted != presented {
		t.Errorf("the trusted key is %q, want the key that was presented, %q", nowTrusted, presented)
	}

	if pending != "" {
		t.Errorf("the key that was waiting is still there: %q", pending)
	}
}

// TestAnApprovalLeavesTheOneConnectionToTheNextRequest is the regression test
// over what stopped a server. The pool holds one connection (database.New),
// the approval holds it for as long as its transaction is open, and a read
// through the default handle inside that transaction waits for the connection
// the same request is holding: the approval never answers, and every request
// after it that touches the database waits behind it, so a login, a status
// screen and the reconcile loop all stop with it.
//
// It is checked on both approvals, because the two take different paths
// through the handler: the first key is approved on the session alone, and
// the key that replaces a trusted one reads the account to check the password
// of it, which is the read that was inside the transaction.
func TestAnApprovalLeavesTheOneConnectionToTheNextRequest(t *testing.T) {
	trustedKey, _ := newHostKey(t)

	approvals := map[string]struct {
		trusted  string
		password string
	}{
		"a first key, which is approved on the session alone": {},
		"a key that replaces the trusted one, which is approved on the password of the account": {
			trusted:  trustedKey,
			password: hostKeyAccountPassword,
		},
	}

	for name, approval := range approvals {
		t.Run(name, func(t *testing.T) {
			presented, fingerprint := newHostKey(t)
			db := newHostKeyDB(t, hostKeyHost(approval.trusted, presented))

			// What the whole test rests on. A pool that hands out a second
			// connection lets the read inside the transaction through, and
			// the test would pass over the fault it is here to catch.
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatalf("failed to reach the connection pool: %v", err)
			}

			if open := sqlDB.Stats().MaxOpenConnections; open != 1 {
				t.Fatalf("the test database hands out %d connections at once, want 1", open)
			}

			rec, _ := approveHostKey(t, db,
				`{"fingerprint":"`+fingerprint+`","password":"`+approval.password+`"}`)

			if rec.Code != http.StatusOK {
				t.Fatalf("POST /api/host/1/host-key = %d, want %d, body: %s", rec.Code,
					http.StatusOK, rec.Body.String())
			}

			// The connection is with the next request. The approval answered,
			// so its transaction is over, and a read that is given the one
			// connection within the deadline is what says it was let go of
			// rather than left held by something the answer did not wait for.
			ctx, cancel := context.WithTimeout(context.Background(), approvalDeadline)
			defer cancel()

			var host models.Host

			err = db.WithContext(ctx).First(&host, 1).Error
			if err != nil {
				t.Fatalf("the read after the approval was not given a connection: %v", err)
			}

			if host.HostKey != presented {
				t.Errorf("the trusted key is %q, want the key that was presented, %q",
					host.HostKey, presented)
			}
		})
	}
}

// TestAnApprovalIsRefusedWhenTheFingerprintIsNotTheOneWaiting is what ties the
// approval to the screen it was given on. Between the answer the screen was
// drawn from and the click, the server may have presented another key, and the
// click would otherwise approve a key nobody read.
func TestAnApprovalIsRefusedWhenTheFingerprintIsNotTheOneWaiting(t *testing.T) {
	presented, _ := newHostKey(t)
	_, otherFingerprint := newHostKey(t)
	db := newHostKeyDB(t, hostKeyHost("", presented))

	rec, manager := approveHostKey(t, db, `{"fingerprint":"`+otherFingerprint+`"}`)

	if rec.Code != http.StatusConflict {
		t.Fatalf("POST /api/host/1/host-key = %d, want %d, body: %s", rec.Code,
			http.StatusConflict, rec.Body.String())
	}

	if code := refusalCode(t, rec); code != "host.host_key.fingerprint_changed" {
		t.Errorf("error_code = %q, want %q", code, "host.host_key.fingerprint_changed")
	}

	trusted, pending := storedKeys(t, db)
	if trusted != "" {
		t.Errorf("a key was trusted by an approval that named another fingerprint: %q", trusted)
	}

	if pending != presented {
		t.Errorf("the key that is waiting is %q, want it left where it was, %q", pending, presented)
	}

	wakes, _ := manager.counts()
	if wakes != 0 {
		t.Errorf("an approval that was refused woke the reconcile loop %d times", wakes)
	}
}

// TestAnApprovalIsRefusedWhenNoKeyIsWaiting covers the click that arrives
// twice, or the one that arrives after the connection was made: there is
// nothing to say yes to, and the trusted key must not be touched by it.
func TestAnApprovalIsRefusedWhenNoKeyIsWaiting(t *testing.T) {
	trusted, fingerprint := newHostKey(t)
	db := newHostKeyDB(t, hostKeyHost(trusted, ""))

	rec, _ := approveHostKey(t, db,
		`{"fingerprint":"`+fingerprint+`","password":"`+hostKeyAccountPassword+`"}`)

	if rec.Code != http.StatusConflict {
		t.Fatalf("POST /api/host/1/host-key = %d, want %d, body: %s", rec.Code,
			http.StatusConflict, rec.Body.String())
	}

	if code := refusalCode(t, rec); code != "host.host_key.nothing_to_approve" {
		t.Errorf("error_code = %q, want %q", code, "host.host_key.nothing_to_approve")
	}

	stillTrusted, pending := storedKeys(t, db)
	if stillTrusted != trusted || pending != "" {
		t.Errorf("the keys of the Host were changed: trusted %q, waiting %q", stillTrusted, pending)
	}
}

// TestAnApprovalWithNoAccountOnTheContextChangesNothing is the case of a route
// hung outside the middleware that reads the session. The handler cannot ask
// for the password of an account it cannot name, and what it must not do is
// approve anything.
func TestAnApprovalWithNoAccountOnTheContextChangesNothing(t *testing.T) {
	trusted, _ := newHostKey(t)
	presented, fingerprint := newHostKey(t)
	db := newHostKeyDB(t, hostKeyHost(trusted, presented))

	rec, _ := approveHostKeyAs(t, db,
		`{"fingerprint":"`+fingerprint+`","password":"`+hostKeyAccountPassword+`"}`, false)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST /api/host/1/host-key = %d, want %d, body: %s", rec.Code,
			http.StatusInternalServerError, rec.Body.String())
	}

	stillTrusted, pending := storedKeys(t, db)
	if stillTrusted != trusted || pending != presented {
		t.Errorf("the keys of the Host were changed: trusted %q, waiting %q", stillTrusted, pending)
	}
}

// TestTheHostKeyApprovalIsBehindTheSession pins where the route hangs. It is
// on the /api group, so a client with no session is refused before the handler
// is reached at all.
func TestTheHostKeyApprovalIsBehindTheSession(t *testing.T) {
	presented, fingerprint := newHostKey(t)
	db := newHostKeyDB(t, hostKeyHost("", presented))

	e := echo.New()
	g := e.Group("/api")
	g.Use(NewAuthHandler(db, zap.NewNop(), filepath.Join(t.TempDir(), "initial")).RequireSession())
	g.POST("/host/:id/host-key",
		NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t)).ApproveHostKey)

	req := httptest.NewRequest(http.MethodPost, "/api/host/1/host-key",
		strings.NewReader(`{"fingerprint":"`+fingerprint+`"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /api/host/1/host-key without a session = %d, want %d, body: %s", rec.Code,
			http.StatusUnauthorized, rec.Body.String())
	}

	trusted, pending := storedKeys(t, db)
	if trusted != "" || pending != presented {
		t.Errorf("a request that carried no session changed the keys: trusted %q, waiting %q",
			trusted, pending)
	}
}

// TestAHostAnswerCarriesFingerprintsAndNotTheKeys is what keeps a screen
// showing something an operator can compare. The keys themselves are not
// secret, but a line of a few hundred characters is not one anybody reads to
// the end, and the fingerprint is what the server itself reports for its key.
func TestAHostAnswerCarriesFingerprintsAndNotTheKeys(t *testing.T) {
	trusted, trustedFingerprint := newHostKey(t)
	presented, presentedFingerprint := newHostKey(t)

	db := newHostStubDB(t, hostKeyHost(trusted, presented))
	h := NewHandler(db, &countFailingManager{}, zap.NewNop(), newTestCipher(t))

	answers := map[string]func() (*httptest.ResponseRecorder, map[string]interface{}){
		"GetHost": func() (*httptest.ResponseRecorder, map[string]interface{}) {
			c, rec := getRequest(t, "/api/host/1", "id", "1")

			err := h.GetHost(c)
			if err != nil {
				t.Fatalf("GetHost returned error: %v", err)
			}

			_, data := decodeResponse(t, rec)

			return rec, data
		},
		"GetHostStatus": func() (*httptest.ResponseRecorder, map[string]interface{}) {
			c, rec := getRequest(t, "/api/status/1", "hostId", "1")

			err := h.GetHostStatus(c)
			if err != nil {
				t.Fatalf("GetHostStatus returned error: %v", err)
			}

			_, data := decodeResponse(t, rec)

			host, ok := data["host"].(map[string]interface{})
			if !ok {
				t.Fatalf("the answer carries no host object, body: %s", rec.Body.String())
			}

			return rec, host
		},
	}

	for name, answer := range answers {
		rec, host := answer()

		for what, key := range map[string]string{"trusted": trusted, "waiting": presented} {
			if strings.Contains(rec.Body.String(), key) {
				t.Errorf("the answer of %s carries the %s host key itself: %s", name, what,
					rec.Body.String())
			}
		}

		for _, field := range []string{"host_key", "pending_host_key"} {
			_, there := host[field]
			if there {
				t.Errorf("the answer of %s carries a %s field: %s", name, field, rec.Body.String())
			}
		}

		fingerprints := map[string]string{
			"host_key_fingerprint":         trustedFingerprint,
			"pending_host_key_fingerprint": presentedFingerprint,
		}
		for field, want := range fingerprints {
			if host[field] != want {
				t.Errorf("the answer of %s has %s = %v, want %q", name, field, host[field], want)
			}
		}
	}
}

// TestTheApprovalAnswersWithTheFingerprintsAsTheyStandAfterIt holds the answer
// of the approval to the same rule, and is what a screen redraws itself from
// without asking again.
func TestTheApprovalAnswersWithTheFingerprintsAsTheyStandAfterIt(t *testing.T) {
	presented, fingerprint := newHostKey(t)
	db := newHostKeyDB(t, hostKeyHost("", presented))

	rec, _ := approveHostKey(t, db, `{"fingerprint":"`+fingerprint+`"}`)

	if strings.Contains(rec.Body.String(), presented) {
		t.Errorf("the answer carries the host key itself: %s", rec.Body.String())
	}

	_, data := decodeResponse(t, rec)

	if data["host_key_fingerprint"] != fingerprint {
		t.Errorf("host_key_fingerprint = %v, want %q", data["host_key_fingerprint"], fingerprint)
	}

	if data["pending_host_key_fingerprint"] != "" {
		t.Errorf("pending_host_key_fingerprint = %v, want it empty",
			data["pending_host_key_fingerprint"])
	}
}

// TestTheHostViewCarriesEveryFieldOfAHost does for an answer what
// TestTheHostContentCarriesEveryFieldOfAHost does for an export. A column
// added to the row and not to the view reaches no client, and the way to find
// that out must not be an operator missing it on a screen.
func TestTheHostViewCarriesEveryFieldOfAHost(t *testing.T) {
	// The three that are left out are the secrets of the Host: the sealed SSH
	// password, the PEM private key and the passphrase that opens it. A key
	// that leaves this process is a key into every machine that trusts it.
	left := map[string]bool{"Password": true, "PrivateKey": true, "KeyPassphrase": true}

	// The two that go out under another name. What is carried is the SHA256
	// fingerprint of each key rather than the key, which is the form that can
	// be compared against the server.
	renamed := map[string]string{
		"HostKey":        "HostKeyFingerprint",
		"PendingHostKey": "PendingHostKeyFingerprint",
	}

	stored := reflect.TypeOf(models.Host{})
	carried := reflect.TypeOf(hostView{})

	for i := 0; i < stored.NumField(); i++ {
		name := stored.Field(i).Name
		if left[name] {
			continue
		}

		if under, ok := renamed[name]; ok {
			name = under
		}

		_, found := carried.FieldByName(name)
		if !found {
			t.Errorf("models.Host has %s and hostView does not, so it reaches no client", name)
		}
	}
}

// hostKeyLine is the one line the call wrote under id.
//
// A line that is written twice is as much a fault as one that is not written
// at all: the log is read as a record of what was done, and two lines for one
// approval read as two approvals.
func hostKeyLine(t *testing.T, logs *observer.ObservedLogs, id logid.ID) observer.LoggedEntry {
	t.Helper()

	var found []observer.LoggedEntry

	for _, entry := range logs.All() {
		if entry.ContextMap()[logid.FieldKey] == string(id) {
			found = append(found, entry)
		}
	}

	if len(found) != 1 {
		t.Fatalf("the call wrote %d lines carrying %q, want 1: %v", len(found), id, logs.All())
	}

	return found[0]
}

// TestAnApprovalIsWrittenToTheLog covers the record of it. Which SSH server a
// Host trusts is what keeps its password from being handed to somebody else,
// so the answer to that question is one the log has to be able to be asked
// about afterwards.
func TestAnApprovalIsWrittenToTheLog(t *testing.T) {
	presented, fingerprint := newHostKey(t)
	db := newHostKeyDB(t, hostKeyHost("", presented))

	rec, _, logs := approveHostKeyLogged(t, db, `{"fingerprint":"`+fingerprint+`"}`, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/host/1/host-key = %d, want %d, body: %s", rec.Code, http.StatusOK,
			rec.Body.String())
	}

	fields := hostKeyLine(t, logs, logid.HostHostKeyApproved).ContextMap()

	if got := fmt.Sprint(fields["host_id"]); got != "1" {
		t.Errorf("the line carries host_id=%s, want 1", got)
	}

	// The fingerprint is on the line on purpose. It is the digest of a public
	// key, so it is no secret, and it is the one form of the key that can be
	// held against the server afterwards.
	if fields["fingerprint"] != fingerprint {
		t.Errorf("the line carries fingerprint=%v, want %q", fields["fingerprint"], fingerprint)
	}
}

// TestAnApprovalRefusedByThePasswordIsWrittenToTheLogWithoutIt covers the
// other line. Somebody working through a session that is not theirs leaves
// nothing else behind, so the attempt is recorded; what must not be recorded
// is the password that was tried, because the log file is read by whoever can
// read the Logs screen.
func TestAnApprovalRefusedByThePasswordIsWrittenToTheLogWithoutIt(t *testing.T) {
	const wrongPassword = "not the password" // hook:allow

	trusted, _ := newHostKey(t)
	presented, fingerprint := newHostKey(t)
	db := newHostKeyDB(t, hostKeyHost(trusted, presented))

	rec, _, logs := approveHostKeyLogged(t, db,
		`{"fingerprint":"`+fingerprint+`","password":"`+wrongPassword+`"}`, true)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /api/host/1/host-key = %d, want %d, body: %s", rec.Code,
			http.StatusUnauthorized, rec.Body.String())
	}

	fields := hostKeyLine(t, logs, logid.HostHostKeyApprovalPasswordWrong).ContextMap()

	if got := fmt.Sprint(fields["host_id"]); got != "1" {
		t.Errorf("the line carries host_id=%s, want 1", got)
	}

	// Nothing at all that was tried as a password is in the log, whichever
	// line it might have reached.
	for _, entry := range logs.All() {
		line := entry.Message
		for name, value := range entry.ContextMap() {
			line += " " + name + "=" + fmt.Sprint(value)
		}

		if strings.Contains(line, wrongPassword) {
			t.Errorf("a log line carries the password that was sent: %s", line)
		}
	}

	// The approval itself was not written, because it did not happen.
	for _, entry := range logs.All() {
		if entry.ContextMap()[logid.FieldKey] == string(logid.HostHostKeyApproved) {
			t.Errorf("an approval that was refused was logged as one: %v", entry)
		}
	}
}
