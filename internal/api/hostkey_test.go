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
	"strconv"
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

	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

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
		leaveSessionOnContext(c, 1)
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

// newHostKeysDB opens the same database newHostKeyDB opens, holding several
// Hosts rather than one. The pool is held to a single connection there, and
// this is the one a bulk approval is run against: the whole point of the call
// is that it writes to several rows through one transaction, so it is the call
// with the most to hold while it reads.
func newHostKeysDB(t *testing.T, hosts ...models.Host) *gorm.DB {
	t.Helper()

	db := newHostKeyDB(t, hosts[0])

	for _, host := range hosts[1:] {
		err := db.Create(&host).Error
		if err != nil {
			t.Fatalf("failed to create the Host %d: %v", host.ID, err)
		}
	}

	return db
}

// waitingHost is a Host with an address of its own, carrying the trusted key
// and the key that is waiting it is given. Two Hosts cannot share an IP, so a
// list of them cannot be built out of hostKeyHost.
func waitingHost(id uint, trusted, pending string) models.Host {
	host := hostKeyHost(trusted, pending)
	host.ID = id
	host.Address = fmt.Sprintf("192.0.2.%d", id)

	return host
}

// approveHostKeys runs one bulk approval and hands back what it answered with,
// the manager it was given and the lines it wrote.
//
// It is run beside the test with a deadline for the reason approveHostKeyLogged
// states: on the pool of one connection these run against, a read through the
// default handle inside the transaction waits for the connection the same
// request is holding and never comes back, and a test that waits with it ends
// the whole package on the -timeout of the run with no word of which call
// stopped it.
func approveHostKeys(t *testing.T, db *gorm.DB,
	body string) (*httptest.ResponseRecorder, *wakeRecorder, *observer.ObservedLogs) {
	t.Helper()

	core, logs := observer.New(zap.DebugLevel)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/host-key", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	leaveSessionOnContext(c, 1)

	manager := &wakeRecorder{tx: &txConnPool{}}
	h := NewHandler(db, manager, zap.New(core), newTestCipher(t))

	done := make(chan error, 1)

	go func() {
		done <- h.ApproveHostKeys(c)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ApproveHostKeys returned error: %v", err)
		}
	case <-time.After(approvalDeadline):
		t.Fatalf("ApproveHostKeys did not come back within %v: it is waiting on the database, which "+
			"is what a read through the default handle inside the transaction does on the one "+
			"connection the server runs on", approvalDeadline)
	}

	return rec, manager, logs
}

// approvalBody writes the request out of pairs of a Host and the fingerprint
// the operator is saying yes to.
func approvalBody(password string, hosts ...[2]string) string {
	entries := make([]string, 0, len(hosts))
	for _, host := range hosts {
		entries = append(entries, `{"host_id":`+host[0]+`,"fingerprint":"`+host[1]+`"}`)
	}

	return `{"password":"` + password + `","hosts":[` + strings.Join(entries, ",") + `]}`
}

// approvalResults reads the answer back as what became of each Host, keyed by
// the Host it is about.
func approvalResults(t *testing.T, rec *httptest.ResponseRecorder) (map[string]map[string]interface{}, int, int) {
	t.Helper()

	var body struct {
		Data struct {
			Approved int                      `json:"approved"`
			Refused  int                      `json:"refused"`
			Hosts    []map[string]interface{} `json:"hosts"`
		} `json:"data"`
	}

	err := json.Unmarshal(rec.Body.Bytes(), &body)
	if err != nil {
		t.Fatalf("failed to read the answer %q: %v", rec.Body.String(), err)
	}

	byHost := make(map[string]map[string]interface{}, len(body.Data.Hosts))
	for _, host := range body.Data.Hosts {
		byHost[fmt.Sprint(host["host_id"])] = host
	}

	return byHost, body.Data.Approved, body.Data.Refused
}

// keysOf reads the two host key columns of one Host back out of the database.
func keysOf(t *testing.T, db *gorm.DB, id uint) (trusted, pending string) {
	t.Helper()

	var host models.Host

	err := db.First(&host, id).Error
	if err != nil {
		t.Fatalf("failed to read the Host %d back: %v", id, err)
	}

	return host.HostKey, host.PendingHostKey
}

// TestABulkApprovalOfFirstKeysNeedsNoPasswordAndApprovesEveryOne is the case an
// upgrade makes: every Host that was registered before the host key check
// existed is waiting for a first approval at the same moment, none of them has
// any trust to overturn, and the session is all that is asked for.
func TestABulkApprovalOfFirstKeysNeedsNoPasswordAndApprovesEveryOne(t *testing.T) {
	first, firstFingerprint := newHostKey(t)
	second, secondFingerprint := newHostKey(t)
	third, thirdFingerprint := newHostKey(t)

	db := newHostKeysDB(t,
		waitingHost(1, "", first),
		waitingHost(2, "", second),
		waitingHost(3, "", third))

	rec, manager, _ := approveHostKeys(t, db, approvalBody("",
		[2]string{"1", firstFingerprint},
		[2]string{"2", secondFingerprint},
		[2]string{"3", thirdFingerprint}))

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/host-key = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	_, approved, refused := approvalResults(t, rec)
	if approved != 3 || refused != 0 {
		t.Fatalf("the answer says %d approved and %d refused, want 3 and 0, body: %s",
			approved, refused, rec.Body.String())
	}

	for id, presented := range map[uint]string{1: first, 2: second, 3: third} {
		trusted, pending := keysOf(t, db, id)
		if trusted != presented {
			t.Errorf("the trusted key of Host %d is %q, want the key that was presented, %q",
				id, trusted, presented)
		}

		if pending != "" {
			t.Errorf("the key that was waiting on Host %d is still there: %q", id, pending)
		}
	}

	// Once for the whole request. The pass that follows sees every Host that
	// was approved, so a wake per Host would be the same pass asked for three
	// times.
	wakes, _ := manager.counts()
	if wakes != 1 {
		t.Errorf("the approval woke the reconcile loop %d times, want 1", wakes)
	}
}

// TestABulkApprovalWithAMismatchInItApprovesNothingWithoutThePassword is the
// check the panel hangs on. A list of first approvals is one press, and a Host
// whose trusted key would be replaced is not: dropping that key is what would
// otherwise have caught a server answering in place of this one.
//
// What it holds is that the whole request is refused rather than the one Host
// in it. An approval that landed in part would leave the operator to work out
// which of the Hosts went through before trying again, so the rows are read
// back afterwards and none of them may have moved.
func TestABulkApprovalWithAMismatchInItApprovesNothingWithoutThePassword(t *testing.T) {
	first, firstFingerprint := newHostKey(t)
	trusted, _ := newHostKey(t)
	presented, presentedFingerprint := newHostKey(t)

	passwords := map[string]string{
		"no password at all":             "",
		"a password that is not the one": "not the password", // hook:allow
	}

	for name, password := range passwords {
		t.Run(name, func(t *testing.T) {
			db := newHostKeysDB(t,
				waitingHost(1, "", first),
				waitingHost(2, trusted, presented))

			rec, manager, logs := approveHostKeys(t, db, approvalBody(password,
				[2]string{"1", firstFingerprint},
				[2]string{"2", presentedFingerprint}))

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("POST /api/host-key = %d, want %d, body: %s", rec.Code,
					http.StatusUnauthorized, rec.Body.String())
			}

			if code := refusalCode(t, rec); code != "host.host_key.password_wrong" {
				t.Errorf("error_code = %q, want %q", code, "host.host_key.password_wrong")
			}

			// The rows are read again rather than taken from the answer: what
			// is being held to is the database, and an answer that says nothing
			// was approved is the thing under test.
			stillFirst, waitingFirst := keysOf(t, db, 1)
			if stillFirst != "" || waitingFirst != first {
				t.Errorf("the first approval in the list went through on a refused password: "+
					"trusted %q, waiting %q", stillFirst, waitingFirst)
			}

			stillTrusted, stillWaiting := keysOf(t, db, 2)
			if stillTrusted != trusted || stillWaiting != presented {
				t.Errorf("the keys of the Host that carries a trusted key were changed: "+
					"trusted %q, waiting %q", stillTrusted, stillWaiting)
			}

			wakes, _ := manager.counts()
			if wakes != 0 {
				t.Errorf("an approval that was refused woke the reconcile loop %d times", wakes)
			}

			// The attempt is written down, against the Host whose trust it was
			// about to replace, and the password that was sent is on no line.
			fields := hostKeyLine(t, logs, logid.HostHostKeyApprovalPasswordWrong).ContextMap()
			if got := fmt.Sprint(fields["host_id"]); got != "2" {
				t.Errorf("the line carries host_id=%s, want 2", got)
			}

			if password != "" {
				for _, entry := range logs.All() {
					line := entry.Message
					for name, value := range entry.ContextMap() {
						line += " " + name + "=" + fmt.Sprint(value)
					}

					if strings.Contains(line, password) {
						t.Errorf("a log line carries the password that was sent: %s", line)
					}
				}
			}
		})
	}
}

// TestABulkApprovalWithAMismatchInItGoesThroughWithThePassword is the other
// half of that rule.
func TestABulkApprovalWithAMismatchInItGoesThroughWithThePassword(t *testing.T) {
	first, firstFingerprint := newHostKey(t)
	trusted, _ := newHostKey(t)
	presented, presentedFingerprint := newHostKey(t)

	db := newHostKeysDB(t,
		waitingHost(1, "", first),
		waitingHost(2, trusted, presented))

	rec, _, _ := approveHostKeys(t, db, approvalBody(hostKeyAccountPassword,
		[2]string{"1", firstFingerprint},
		[2]string{"2", presentedFingerprint}))

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/host-key = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	_, approved, refused := approvalResults(t, rec)
	if approved != 2 || refused != 0 {
		t.Fatalf("the answer says %d approved and %d refused, want 2 and 0, body: %s",
			approved, refused, rec.Body.String())
	}

	for id, want := range map[uint]string{1: first, 2: presented} {
		nowTrusted, pending := keysOf(t, db, id)
		if nowTrusted != want || pending != "" {
			t.Errorf("the keys of Host %d are trusted %q, waiting %q, want %q and nothing",
				id, nowTrusted, pending, want)
		}
	}
}

// TestABulkApprovalRefusesTheHostWhoseKeyChangedAndApprovesTheRest is what ties
// each Host of the request to the fingerprint that was on the screen for it.
// Between the answer the panel was drawn from and the press, an SSH server may
// have presented another key, and that says nothing about any other Host in the
// list: the one is refused and the others go through.
func TestABulkApprovalRefusesTheHostWhoseKeyChangedAndApprovesTheRest(t *testing.T) {
	first, firstFingerprint := newHostKey(t)
	second, _ := newHostKey(t)
	third, thirdFingerprint := newHostKey(t)
	_, staleFingerprint := newHostKey(t)

	db := newHostKeysDB(t,
		waitingHost(1, "", first),
		waitingHost(2, "", second),
		waitingHost(3, "", third))

	rec, manager, _ := approveHostKeys(t, db, approvalBody("",
		[2]string{"1", firstFingerprint},
		[2]string{"2", staleFingerprint},
		[2]string{"3", thirdFingerprint}))

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/host-key = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	results, approved, refused := approvalResults(t, rec)
	if approved != 2 || refused != 1 {
		t.Fatalf("the answer says %d approved and %d refused, want 2 and 1, body: %s",
			approved, refused, rec.Body.String())
	}

	if results["2"]["approved"] != false {
		t.Errorf("Host 2 was approved on a fingerprint that is not the one waiting: %v", results["2"])
	}

	if results["2"]["error_code"] != "host.host_key.fingerprint_changed" {
		t.Errorf("Host 2 was refused under %v, want %q", results["2"]["error_code"],
			"host.host_key.fingerprint_changed")
	}

	// The refusal says which fingerprint is the one waiting, so that the panel
	// can put the right one in front of the operator without asking again.
	args, ok := results["2"]["error_args"].(map[string]interface{})
	if !ok || args["waiting"] != tunnel.HostKeyFingerprint(second) {
		t.Errorf("the refusal of Host 2 carries %v, want the fingerprint that is waiting, %q",
			results["2"]["error_args"], tunnel.HostKeyFingerprint(second))
	}

	stillTrusted, stillWaiting := keysOf(t, db, 2)
	if stillTrusted != "" || stillWaiting != second {
		t.Errorf("the keys of the Host that was refused were changed: trusted %q, waiting %q",
			stillTrusted, stillWaiting)
	}

	for id, presented := range map[uint]string{1: first, 3: third} {
		trusted, pending := keysOf(t, db, id)
		if trusted != presented || pending != "" {
			t.Errorf("Host %d was not approved alongside the one that was refused: "+
				"trusted %q, waiting %q", id, trusted, pending)
		}
	}

	wakes, _ := manager.counts()
	if wakes != 1 {
		t.Errorf("the approval woke the reconcile loop %d times, want 1", wakes)
	}
}

// TestABulkApprovalRefusesAHostWithNothingWaitingAndApprovesTheRest covers the
// press that arrives after another client approved one of them, which is what
// two operators working through the same upgrade look like.
func TestABulkApprovalRefusesAHostWithNothingWaitingAndApprovesTheRest(t *testing.T) {
	first, firstFingerprint := newHostKey(t)
	settled, settledFingerprint := newHostKey(t)

	db := newHostKeysDB(t,
		waitingHost(1, "", first),
		waitingHost(2, settled, ""))

	rec, _, _ := approveHostKeys(t, db, approvalBody(hostKeyAccountPassword,
		[2]string{"1", firstFingerprint},
		[2]string{"2", settledFingerprint}))

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/host-key = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	results, approved, refused := approvalResults(t, rec)
	if approved != 1 || refused != 1 {
		t.Fatalf("the answer says %d approved and %d refused, want 1 and 1, body: %s",
			approved, refused, rec.Body.String())
	}

	if results["2"]["error_code"] != "host.host_key.nothing_to_approve" {
		t.Errorf("Host 2 was refused under %v, want %q", results["2"]["error_code"],
			"host.host_key.nothing_to_approve")
	}

	trusted, pending := keysOf(t, db, 1)
	if trusted != first || pending != "" {
		t.Errorf("the Host that was waiting was not approved: trusted %q, waiting %q", trusted, pending)
	}
}

// TestABulkApprovalLeavesTheOneConnectionToTheNextRequest is the regression
// test over what stopped a server, held against the call that writes to several
// rows through one transaction.
//
// The pool holds one connection (database.New), the transaction holds it for as
// long as it is open, and a read through the default handle inside it waits for
// the connection the same request is holding: the approval never answers, and
// every request after it that touches the database waits behind it. Checking
// the password is that read, which is why it is done before the transaction is
// opened, and this is what says it stayed there.
func TestABulkApprovalLeavesTheOneConnectionToTheNextRequest(t *testing.T) {
	trusted, _ := newHostKey(t)
	first, firstFingerprint := newHostKey(t)
	presented, presentedFingerprint := newHostKey(t)

	db := newHostKeysDB(t,
		waitingHost(1, "", first),
		waitingHost(2, trusted, presented))

	// What the whole test rests on. A pool that hands out a second connection
	// lets the read inside the transaction through, and the test would pass
	// over the fault it is here to catch.
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to reach the connection pool: %v", err)
	}

	if open := sqlDB.Stats().MaxOpenConnections; open != 1 {
		t.Fatalf("the test database hands out %d connections at once, want 1", open)
	}

	rec, _, _ := approveHostKeys(t, db, approvalBody(hostKeyAccountPassword,
		[2]string{"1", firstFingerprint},
		[2]string{"2", presentedFingerprint}))

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/host-key = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// The connection is with the next request. The approval answered, so its
	// transaction is over, and a read that is given the one connection within
	// the deadline is what says it was let go of rather than left held by
	// something the answer did not wait for.
	ctx, cancel := context.WithTimeout(context.Background(), approvalDeadline)
	defer cancel()

	var hosts []models.Host

	err = db.WithContext(ctx).Order("id").Find(&hosts).Error
	if err != nil {
		t.Fatalf("the read after the approval was not given a connection: %v", err)
	}

	for _, host := range hosts {
		if host.PendingHostKey != "" {
			t.Errorf("Host %d was not approved: %q is still waiting", host.ID, host.PendingHostKey)
		}
	}
}

// TestABulkApprovalIsWrittenToTheLogOncePerHost covers the record of it. Which
// SSH server a Host trusts is what keeps its password from being handed to
// somebody else, and a request over two hundred Hosts that wrote one line would
// answer that question for the first of them alone.
func TestABulkApprovalIsWrittenToTheLogOncePerHost(t *testing.T) {
	first, firstFingerprint := newHostKey(t)
	second, secondFingerprint := newHostKey(t)

	db := newHostKeysDB(t, waitingHost(1, "", first), waitingHost(2, "", second))

	_, _, logs := approveHostKeys(t, db, approvalBody("",
		[2]string{"1", firstFingerprint},
		[2]string{"2", secondFingerprint}))

	written := map[string]string{}

	for _, entry := range logs.All() {
		fields := entry.ContextMap()
		if fields[logid.FieldKey] != string(logid.HostHostKeyApproved) {
			continue
		}

		written[fmt.Sprint(fields["host_id"])] = fmt.Sprint(fields["fingerprint"])
	}

	want := map[string]string{"1": firstFingerprint, "2": secondFingerprint}
	if !reflect.DeepEqual(written, want) {
		t.Errorf("the approvals were written as %v, want %v", written, want)
	}
}

// TestABulkApprovalCarriesFingerprintsAndNotTheKeys holds the answer to the
// rule hostView states. What goes on a screen is the fingerprint, which is the
// one form of the key that can be compared against the server itself.
func TestABulkApprovalCarriesFingerprintsAndNotTheKeys(t *testing.T) {
	first, firstFingerprint := newHostKey(t)
	trusted, _ := newHostKey(t)
	presented, presentedFingerprint := newHostKey(t)

	db := newHostKeysDB(t,
		waitingHost(1, "", first),
		waitingHost(2, trusted, presented))

	rec, _, _ := approveHostKeys(t, db, approvalBody(hostKeyAccountPassword,
		[2]string{"1", firstFingerprint},
		[2]string{"2", presentedFingerprint}))

	for what, key := range map[string]string{"trusted": trusted, "waiting": presented, "first": first} {
		if strings.Contains(rec.Body.String(), key) {
			t.Errorf("the answer carries the %s host key itself: %s", what, rec.Body.String())
		}
	}

	results, _, _ := approvalResults(t, rec)
	if results["1"]["fingerprint"] != firstFingerprint {
		t.Errorf("Host 1 answers with fingerprint %v, want %q", results["1"]["fingerprint"],
			firstFingerprint)
	}

	if results["2"]["fingerprint"] != presentedFingerprint {
		t.Errorf("Host 2 answers with fingerprint %v, want %q", results["2"]["fingerprint"],
			presentedFingerprint)
	}
}

// TestTheBulkApprovalIsBehindTheSession pins where the route hangs, the way the
// single approval is pinned. It is on the /api group, so a client with no
// session is refused before the handler is reached at all.
func TestTheBulkApprovalIsBehindTheSession(t *testing.T) {
	presented, fingerprint := newHostKey(t)
	db := newHostKeysDB(t, waitingHost(1, "", presented))

	e := echo.New()
	g := e.Group("/api")
	g.Use(NewAuthHandler(db, zap.NewNop(), filepath.Join(t.TempDir(), "initial")).RequireSession())
	g.POST("/host-key",
		NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t)).ApproveHostKeys)

	req := httptest.NewRequest(http.MethodPost, "/api/host-key",
		strings.NewReader(approvalBody("", [2]string{"1", fingerprint})))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /api/host-key without a session = %d, want %d, body: %s", rec.Code,
			http.StatusUnauthorized, rec.Body.String())
	}

	trusted, pending := keysOf(t, db, 1)
	if trusted != "" || pending != presented {
		t.Errorf("a request that carried no session changed the keys: trusted %q, waiting %q",
			trusted, pending)
	}
}

// sqliteVariableLimit is how many values SQLite takes in one statement, which
// is what a list of Host ids written into an IN (...) is counted against. The
// number is the default a build of SQLite carries; it is written here rather
// than read from the driver because what the test is for is a request larger
// than any build takes, and a build that takes more would only make the test
// milder.
const sqliteVariableLimit = 32766

// TestABulkApprovalOfMoreHostsThanSQLiteTakesVariablesIsNotAServerError is the
// request that reaches past the panel. A press there is a page of a hundred at
// most, and this is a list handed straight to the API: the ids of it used to go
// into one IN (...), which is a statement SQLite refuses to prepare once the
// list is longer than the variables it takes, and what came back was a 500 over
// a request that is nothing but too large.
func TestABulkApprovalOfMoreHostsThanSQLiteTakesVariablesIsNotAServerError(t *testing.T) {
	presented, fingerprint := newHostKey(t)
	db := newHostKeysDB(t, waitingHost(1, "", presented))

	// One Host that is there and the rest that are not. The ids are what the
	// read is made of, so a row per id would say nothing more about the
	// statement being prepared than an id that matches nothing.
	hosts := make([][2]string, 0, sqliteVariableLimit+2)
	for id := 1; id <= sqliteVariableLimit+2; id++ {
		hosts = append(hosts, [2]string{strconv.Itoa(id), fingerprint})
	}

	rec, manager, _ := approveHostKeys(t, db, approvalBody("", hosts...))

	if rec.Code >= http.StatusInternalServerError {
		t.Fatalf("POST /api/host-key over %d Hosts = %d, and a request that is too large is not a "+
			"fault of this server, body: %s", len(hosts), rec.Code, rec.Body.String())
	}

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/host-key over %d Hosts = %d, want %d, body: %s", len(hosts), rec.Code,
			http.StatusBadRequest, rec.Body.String())
	}

	if code := refusalCode(t, rec); code != "request.validation.failed" {
		t.Errorf("error_code = %q, want %q", code, "request.validation.failed")
	}

	trusted, pending := keysOf(t, db, 1)
	if trusted != "" || pending != presented {
		t.Errorf("a request that was refused changed the keys: trusted %q, waiting %q", trusted, pending)
	}

	wakes, _ := manager.counts()
	if wakes != 0 {
		t.Errorf("a request that was refused woke the reconcile loop %d times, want 0", wakes)
	}
}

// TestTheHostsWaitingOnAChangedKeyAreReadInGroupsOfIdsThatSQLiteTakes is the
// read under that refusal, held on its own.
//
// The handler above never hands it more ids than a request may name, so what
// this covers is the read itself: a list longer than the variables SQLite takes
// comes back as rows and not as an error, whatever the caller above it lets
// through. It is run against the pool of one connection the server has, because
// a read made in several statements is several times the read that has to be
// given that connection, and it is run beside the test with a deadline for the
// reason every approval in this file is: a read that is waiting for a
// connection it will never be given would otherwise end the whole package on
// the -timeout of the run with no word of which call stopped it.
func TestTheHostsWaitingOnAChangedKeyAreReadInGroupsOfIdsThatSQLiteTakes(t *testing.T) {
	first, _ := newHostKey(t)
	trusted, _ := newHostKey(t)
	presented, _ := newHostKey(t)

	db := newHostKeysDB(t,
		waitingHost(1, "", first),
		waitingHost(2, trusted, presented),
		waitingHost(3, trusted, presented))

	// What the test rests on, the way the bulk approval above rests on it: a
	// pool that hands out a second connection says nothing about the one the
	// server runs on.
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to reach the connection pool: %v", err)
	}

	if open := sqlDB.Stats().MaxOpenConnections; open != 1 {
		t.Fatalf("the test database hands out %d connections at once, want 1", open)
	}

	// Three Hosts that are there and tens of thousands of ids that are not.
	// Rows are not what is being counted here: the ids are the values the
	// statement binds, so an id that matches nothing is bound exactly as one
	// that matches a row.
	ids := make([]uint, 0, sqliteVariableLimit+2)
	for id := 1; id <= sqliteVariableLimit+2; id++ {
		ids = append(ids, uint(id))
	}

	type read struct {
		changed []uint
		err     error
	}

	done := make(chan read, 1)

	go func() {
		changed, err := hostKeysChangedAmong(db, ids)
		done <- read{changed: changed, err: err}
	}()

	var got read

	select {
	case got = <-done:
	case <-time.After(approvalDeadline):
		t.Fatalf("the read over %d ids did not come back within %v", len(ids), approvalDeadline)
	}

	if got.err != nil {
		t.Fatalf("the read over %d ids failed: %v", len(ids), got.err)
	}

	// Hosts 2 and 3 alone: Host 1 is waiting for a first approval, which asks
	// for no password, and the rest of the ids are not rows at all.
	if !reflect.DeepEqual(got.changed, []uint{2, 3}) {
		t.Errorf("the read over %d ids found %v, want %v", len(ids), got.changed, []uint{2, 3})
	}
}

// TestABulkApprovalOfThePanelsLargestPageIsUnchanged is the request the screen
// actually makes. The panel approves what is on it, and the largest page a list
// is served in is a hundred (pageSizes), so this is the biggest press there is
// and nothing about the size of it may be answered with anything but the
// approvals.
func TestABulkApprovalOfThePanelsLargestPageIsUnchanged(t *testing.T) {
	const page = 100

	hosts := make([]models.Host, 0, page)
	asked := make([][2]string, 0, page)
	presented := make(map[uint]string, page)

	for id := 1; id <= page; id++ {
		key, fingerprint := newHostKey(t)

		hosts = append(hosts, waitingHost(uint(id), "", key))
		asked = append(asked, [2]string{strconv.Itoa(id), fingerprint})
		presented[uint(id)] = key
	}

	db := newHostKeysDB(t, hosts...)

	rec, manager, _ := approveHostKeys(t, db, approvalBody("", asked...))

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/host-key over %d Hosts = %d, want %d, body: %s", page, rec.Code,
			http.StatusOK, rec.Body.String())
	}

	_, approved, refused := approvalResults(t, rec)
	if approved != page || refused != 0 {
		t.Fatalf("the answer says %d approved and %d refused, want %d and 0", approved, refused, page)
	}

	for id, key := range presented {
		trusted, pending := keysOf(t, db, id)
		if trusted != key {
			t.Errorf("the trusted key of Host %d is %q, want the key that was presented, %q",
				id, trusted, key)
		}

		if pending != "" {
			t.Errorf("the key that was waiting on Host %d is still there: %q", id, pending)
		}
	}

	// Once for the whole request, however many Hosts were in it.
	wakes, _ := manager.counts()
	if wakes != 1 {
		t.Errorf("the approval woke the reconcile loop %d times, want 1", wakes)
	}
}

// TestABulkApprovalOfExactlyTheMostHostsItTakesGoesThrough pins which side of
// the line the number itself is on. A request of exactly hostKeyApprovalsMax is
// a request this API takes, and one Host more is the one that is refused, so
// the two tests together leave no room for the check to be read either way.
//
// The Hosts it names are one row and the rest ids that are not there, because
// what is under test is the length of the list rather than what the rows say:
// each of those comes back as that one Host's not found, which is the answer
// the request is carried out with.
func TestABulkApprovalOfExactlyTheMostHostsItTakesGoesThrough(t *testing.T) {
	presented, fingerprint := newHostKey(t)
	db := newHostKeysDB(t, waitingHost(1, "", presented))

	hosts := make([][2]string, 0, hostKeyApprovalsMax)
	for id := 1; id <= hostKeyApprovalsMax; id++ {
		hosts = append(hosts, [2]string{strconv.Itoa(id), fingerprint})
	}

	rec, manager, _ := approveHostKeys(t, db, approvalBody("", hosts...))

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/host-key over %d Hosts = %d, want %d, body: %s", len(hosts), rec.Code,
			http.StatusOK, rec.Body.String())
	}

	results, approved, refused := approvalResults(t, rec)
	if approved != 1 || refused != len(hosts)-1 {
		t.Fatalf("the answer says %d approved and %d refused, want 1 and %d", approved, refused,
			len(hosts)-1)
	}

	if results["2"]["error_code"] != "host.not_found" {
		t.Errorf("Host 2 was refused under %v, want %q", results["2"]["error_code"], "host.not_found")
	}

	trusted, pending := keysOf(t, db, 1)
	if trusted != presented || pending != "" {
		t.Errorf("Host 1 was not approved: trusted %q, waiting %q", trusted, pending)
	}

	wakes, _ := manager.counts()
	if wakes != 1 {
		t.Errorf("the approval woke the reconcile loop %d times, want 1", wakes)
	}
}

// listHostKeysWaiting reads one page of the list the panel is drawn from.
func listHostKeysWaiting(t *testing.T, db *gorm.DB, query string) (*httptest.ResponseRecorder, listedHostKeys) {
	t.Helper()

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/host-key?"+query, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	err := h.ListHostKeysWaiting(c)
	if err != nil {
		t.Fatalf("ListHostKeysWaiting returned error: %v", err)
	}

	var body struct {
		Data listedHostKeys `json:"data"`
	}

	err = json.Unmarshal(rec.Body.Bytes(), &body)
	if err != nil {
		t.Fatalf("failed to read the answer %q: %v", rec.Body.String(), err)
	}

	return rec, body.Data
}

// listedHostKeys is that page as a client reads it.
type listedHostKeys struct {
	Items []struct {
		HostID             uint   `json:"host_id"`
		Address            string `json:"address"`
		Mismatch           bool   `json:"mismatch"`
		Fingerprint        string `json:"fingerprint"`
		TrustedFingerprint string `json:"trusted_fingerprint"`
	} `json:"items"`
	Total int64 `json:"total"`
	Page  int   `json:"page"`
	Size  int   `json:"size"`
}

// TestTheHostKeysWaitingAreFilteredAndPagedByTheServer is what keeps the panel
// from being handed the whole list of Hosts to pick through. An upgrade leaves
// every Host waiting at once, so the list behind the line on the status screen
// is as long as the list of Hosts, and this holds both halves of what keeps it
// off the wire: the Hosts that are not waiting are not in it, and the ones that
// are come a page at a time.
func TestTheHostKeysWaitingAreFilteredAndPagedByTheServer(t *testing.T) {
	// Twenty-two Hosts, of which fifteen are waiting: ten for a first approval,
	// five on a key that is not the one they are trusted on. The smallest page
	// a list is served in is ten (pageSizes), so fifteen is what it takes for
	// the answer to be more than one page, and the seven that are settled are
	// what says the paging is over the ones that are waiting rather than over
	// the Hosts: read as Hosts, the second page would begin at 11 and carry
	// settled rows.
	hosts := make([]models.Host, 0, 22)
	waiting := make(map[uint]string, 15)
	trustedOn := make(map[uint]string, 5)

	for id := uint(1); id <= 22; id++ {
		switch {
		case id <= 10:
			presented, fingerprint := newHostKey(t)
			waiting[id] = fingerprint

			hosts = append(hosts, waitingHost(id, "", presented))
		case id <= 15:
			trusted, trustedFingerprint := newHostKey(t)
			presented, fingerprint := newHostKey(t)
			waiting[id] = fingerprint
			trustedOn[id] = trustedFingerprint

			hosts = append(hosts, waitingHost(id, trusted, presented))
		default:
			// Settled: trusted on a key with nothing waiting, which is what
			// every Host looks like once its key has been approved.
			trusted, _ := newHostKey(t)
			hosts = append(hosts, waitingHost(id, trusted, ""))
		}
	}

	db := newHostKeysDB(t, hosts...)

	pages := map[int][]uint{
		1: {1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
		2: {11, 12, 13, 14, 15},
	}

	for number, want := range pages {
		rec, page := listHostKeysWaiting(t, db, "page="+strconv.Itoa(number)+"&size=10")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/host-key = %d, want %d, body: %s", rec.Code, http.StatusOK,
				rec.Body.String())
		}

		if page.Total != 15 {
			t.Errorf("page %d says the list is %d long, want 15 (the Hosts that are waiting, and "+
				"not the twenty-two that are stored)", number, page.Total)
		}

		if page.Page != number || page.Size != 10 {
			t.Errorf("page %d came back as page %d of %d", number, page.Page, page.Size)
		}

		got := make([]uint, 0, len(page.Items))
		for _, item := range page.Items {
			got = append(got, item.HostID)
		}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("page %d carries %v, want %v", number, got, want)
		}
	}

	// And what a row carries: the Host as an operator names it, which of the
	// two states it is in, and the fingerprints the comparison is made of.
	for number := 1; number <= 2; number++ {
		_, page := listHostKeysWaiting(t, db, "page="+strconv.Itoa(number)+"&size=10")

		for _, item := range page.Items {
			if item.Fingerprint != waiting[item.HostID] {
				t.Errorf("Host %d carries the fingerprint %q, want %q", item.HostID,
					item.Fingerprint, waiting[item.HostID])
			}

			if item.Address != fmt.Sprintf("192.0.2.%d", item.HostID) {
				t.Errorf("Host %d carries the address %q", item.HostID, item.Address)
			}

			wantMismatch := trustedOn[item.HostID] != ""
			if item.Mismatch != wantMismatch {
				t.Errorf("Host %d says mismatch=%v, want %v", item.HostID, item.Mismatch,
					wantMismatch)
			}

			if item.TrustedFingerprint != trustedOn[item.HostID] {
				t.Errorf("Host %d says it is trusted on %q, want %q", item.HostID,
					item.TrustedFingerprint, trustedOn[item.HostID])
			}
		}
	}
}

// TestTheListOfHostKeysWaitingCarriesNoKey holds it to the rule hostView
// states, over the one answer that carries a key of every Host at once.
func TestTheListOfHostKeysWaitingCarriesNoKey(t *testing.T) {
	trusted, _ := newHostKey(t)
	presented, _ := newHostKey(t)

	db := newHostKeysDB(t, waitingHost(1, trusted, presented))

	rec, _ := listHostKeysWaiting(t, db, "page=1&size=10")

	for what, key := range map[string]string{"trusted": trusted, "waiting": presented} {
		if strings.Contains(rec.Body.String(), key) {
			t.Errorf("the answer carries the %s host key itself: %s", what, rec.Body.String())
		}
	}
}

// statusAnswer runs the status screen answer over a database and hands back
// what is in it.
//
// The manager is a real one over the same database, the way the other status
// tests build one: the three counts above these two are read from it, and an
// answer that could not be built is not one to read counts out of.
func statusAnswer(t *testing.T, db *gorm.DB, query string) map[string]interface{} {
	t.Helper()

	cipher := newTestCipher(t)

	manager, err := tunnel.NewManager(db, zap.NewNop(), cipher, 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/status?"+query, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err = NewHandler(db, manager, zap.NewNop(), cipher).GetStatus(c)
	if err != nil {
		t.Fatalf("GetStatus returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body struct {
		Data map[string]interface{} `json:"data"`
	}

	err = json.Unmarshal(rec.Body.Bytes(), &body)
	if err != nil {
		t.Fatalf("failed to read the answer %q: %v", rec.Body.String(), err)
	}

	return body.Data
}

// TestTheStatusCountsAWaitingHostOnceAndWhateverPageItIsOn is what the line
// over the table is drawn from, and it is the whole reason the counts are in
// the answer rather than read off the rows below them.
//
// A Host with four service ports has four tunnels, and every one of them was
// refused by the same key: counted over the page they would say the same thing
// four times, which is what an operator asked about. And the page carries ten
// rows of tunnels, so a Host whose tunnels are all on the second page would not
// be asked about at all while the first page is being read.
func TestTheStatusCountsAWaitingHostOnceAndWhateverPageItIsOn(t *testing.T) {
	presented, _ := newHostKey(t)
	trusted, _ := newHostKey(t)
	changed, _ := newHostKey(t)

	// Host 1 carries four service ports and is waiting for a first approval.
	// Host 2 is trusted on a key and was presented another. Host 3 is settled.
	hosts := []models.Host{
		waitingHost(1, "", presented),
		waitingHost(2, trusted, changed),
		waitingHost(3, "", ""),
	}

	sps := []models.ServicePort{
		statusServicePort(1), statusServicePort(2), statusServicePort(3), statusServicePort(4),
	}

	// Twelve tunnels over three Hosts, ordered by Host: a page of ten leaves
	// the last two of Host 3 for the second page, so the first page carries no
	// row of a Host that is waiting to be approved at all.
	var tunnels []models.Tunnel

	for host := uint(1); host <= 3; host++ {
		for sp := uint(1); sp <= 4; sp++ {
			status := "connected"
			if host != 3 {
				status = "host_key_unapproved"
			}

			tunnels = append(tunnels, statusTunnel(host, sp, status))
		}
	}

	db := newRowsDB(t, hosts, sps, tunnels)

	pages := []string{"page=1&size=10", "page=2&size=10"}
	for _, query := range pages {
		data := statusAnswer(t, db, query)

		if data["host_keys_unapproved"] != float64(1) {
			t.Errorf("%s says %v Hosts are waiting for a first approval, want 1 (the one Host, "+
				"and not one per tunnel of it)", query, data["host_keys_unapproved"])
		}

		if data["host_keys_mismatched"] != float64(1) {
			t.Errorf("%s says %v Hosts were presented another key, want 1", query,
				data["host_keys_mismatched"])
		}
	}
}

// TestTheStatusSaysNothingIsWaitingWhenNothingIs is the other end of it: a line
// over the table that appears when there is nothing to answer is a line that
// gets read past, and the counts are what the screen decides that by.
func TestTheStatusSaysNothingIsWaitingWhenNothingIs(t *testing.T) {
	trusted, _ := newHostKey(t)

	hosts := []models.Host{waitingHost(1, trusted, ""), waitingHost(2, "", "")}
	sps := []models.ServicePort{statusServicePort(1)}
	tunnels := []models.Tunnel{statusTunnel(1, 1, "connected"), statusTunnel(2, 1, "connected")}

	data := statusAnswer(t, newRowsDB(t, hosts, sps, tunnels), "page=1&size=10")

	for _, name := range []string{"host_keys_unapproved", "host_keys_mismatched"} {
		if data[name] != float64(0) {
			t.Errorf("%s = %v, want 0", name, data[name])
		}
	}
}
