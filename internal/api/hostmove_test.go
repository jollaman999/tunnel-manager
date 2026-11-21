package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/settings"
	"github.com/labstack/echo/v5"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"
)

// hostMoveWrongPassword is a password that does not open the account
// newHostKeyDB stores.
const hostMoveWrongPassword = "not-the-password-of-the-account" // hook:allow

// newHostMoveDB opens the database newHostKeyDB opens, holding host as Host 1
// and a second Host to put on its jump route, with route as the route Host 1
// is stored with. The settings and the local forwards are built as well,
// because a change that switches the SOCKS5 proxy on reads both.
func newHostMoveDB(t *testing.T, host models.Host, route []uint) *gorm.DB {
	t.Helper()

	db := newHostKeyDB(t, host)

	err := db.AutoMigrate(&settings.Settings{}, &models.LocalForward{})
	if err != nil {
		t.Fatalf("failed to migrate the database: %v", err)
	}

	jump := storedHost()
	jump.ID = 2
	jump.Address = "192.0.2.2"

	err = db.Create(&jump).Error
	if err != nil {
		t.Fatalf("failed to create the jump Host: %v", err)
	}

	err = writeJumpRoute(db, host.ID, route)
	if err != nil {
		t.Fatalf("failed to store the jump route: %v", err)
	}

	return db
}

// updateHostOn sends one change of Host 1 as the account newHostKeyDB stores,
// on a logger whose lines can be read back. It is run beside the test with a
// deadline for the reason approveHostKeyLogged gives: the pool holds one
// connection, and a read through the default handle inside the transaction
// would wait for it forever.
func updateHostOn(t *testing.T, db *gorm.DB, body string) (*httptest.ResponseRecorder, *observer.ObservedLogs) {
	t.Helper()

	core, logs := observer.New(zap.DebugLevel)

	e := echo.New()
	e.Validator = &testValidator{validator: validator.New()}

	req := httptest.NewRequest(http.MethodPut, "/api/host/1", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPathValues(echo.PathValues{{Name: "id", Value: "1"}})
	leaveSessionOnContext(c, 1)

	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.New(core), newTestCipher(t))

	done := make(chan error, 1)

	go func() {
		done <- h.UpdateHost(c)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("UpdateHost returned error: %v", err)
		}
	case <-time.After(approvalDeadline):
		t.Fatalf("UpdateHost did not come back within %v: it is waiting on the database", approvalDeadline)
	}

	return rec, logs
}

// storedHostAndRoute reads Host 1 and its jump route back.
func storedHostAndRoute(t *testing.T, db *gorm.DB) (models.Host, []uint) {
	t.Helper()

	var host models.Host

	err := db.First(&host, 1).Error
	if err != nil {
		t.Fatalf("failed to read the Host back: %v", err)
	}

	route, err := jumpRouteOf(db, 1)
	if err != nil {
		t.Fatalf("failed to read the jump route back: %v", err)
	}

	return host, route
}

// hostMoves are the three changes that send the stored login of a Host
// somewhere else: its address, its SSH port and the jump hosts it is reached
// through. A jump host is on the path the login travels and can answer in
// place of the Host, which is the same as the address being somebody else's.
var hostMoves = []struct {
	name  string
	field string
	moved func(host models.Host, route []uint) bool
}{
	{
		name:  "address",
		field: `"address":"198.51.100.7"`,
		moved: func(host models.Host, _ []uint) bool { return host.Address == "198.51.100.7" },
	},
	{
		name:  "port",
		field: `"port":2222`,
		moved: func(host models.Host, _ []uint) bool { return host.Port == 2222 },
	},
	{
		name:  "jump route",
		field: `"jump_host_ids":[2]`,
		moved: func(_ models.Host, route []uint) bool { return slices.Equal(route, []uint{2}) },
	},
}

// TestUpdateHostAsksForTheAccountPasswordToMoveAnUntrustedHost covers a Host
// whose host key has never been approved and which carries a stored login.
// Moving it is what would let a session alone send that login to a server of
// its choosing: the first key that server presents is approved without the
// password of the account, and the tunnel then logs in to it with the stored
// password. So the move itself asks for that password.
func TestUpdateHostAsksForTheAccountPasswordToMoveAnUntrustedHost(t *testing.T) {
	for _, move := range hostMoves {
		t.Run(move.name, func(t *testing.T) {
			db := newHostMoveDB(t, storedHost(), nil)

			rec, _ := updateHostOn(t, db, `{`+move.field+`}`)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("a move without account_password = %d, want %d, body: %s", rec.Code,
					http.StatusBadRequest, rec.Body.String())
			}

			if code := refusalCode(t, rec); code != "host.update.account_password.required" {
				t.Errorf("error_code = %q, want %q", code, "host.update.account_password.required")
			}

			if host, route := storedHostAndRoute(t, db); move.moved(host, route) {
				t.Fatalf("a move without account_password was stored: %+v, route %v", host, route)
			}

			rec, logs := updateHostOn(t, db,
				`{`+move.field+`,"account_password":"`+hostMoveWrongPassword+`"}`)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("a move with the wrong account_password = %d, want %d, body: %s", rec.Code,
					http.StatusUnauthorized, rec.Body.String())
			}

			if code := refusalCode(t, rec); code != "host.update.account_password.wrong" {
				t.Errorf("error_code = %q, want %q", code, "host.update.account_password.wrong")
			}

			if host, route := storedHostAndRoute(t, db); move.moved(host, route) {
				t.Fatalf("a move with the wrong account_password was stored: %+v, route %v", host, route)
			}

			fields := hostKeyLine(t, logs, logid.HostUpdateAccountPasswordWrong).ContextMap()
			if got := fmt.Sprint(fields["host_id"]); got != "1" {
				t.Errorf("the line carries host_id=%s, want 1", got)
			}

			for _, entry := range logs.All() {
				line := entry.Message
				for name, value := range entry.ContextMap() {
					line += " " + name + "=" + fmt.Sprint(value)
				}

				if strings.Contains(line, hostMoveWrongPassword) {
					t.Errorf("a log line carries the password that was sent: %s", line)
				}
			}

			rec, _ = updateHostOn(t, db,
				`{`+move.field+`,"account_password":"`+hostKeyAccountPassword+`"}`)

			if rec.Code != http.StatusOK {
				t.Fatalf("a move with the account_password = %d, want %d, body: %s", rec.Code,
					http.StatusOK, rec.Body.String())
			}

			if host, route := storedHostAndRoute(t, db); !move.moved(host, route) {
				t.Fatalf("a move with the account_password was not stored: %+v, route %v", host, route)
			}
		})
	}
}

// TestUpdateHostTakesAChangeWithoutThePasswordWhereNoLoginIsMoved covers the
// changes the password is not asked for on. A Host that trusts a key proves
// itself at the end of whatever path it is reached by, a Host with no stored
// login has nothing to send, and a change that leaves the address, the port
// and the route where they are sends the login nowhere new.
func TestUpdateHostTakesAChangeWithoutThePasswordWhereNoLoginIsMoved(t *testing.T) {
	trusted, _ := newHostKey(t)

	withKey := storedHost()
	withKey.HostKey = trusted

	noLogin := storedHost()
	noLogin.Password = ""

	for _, tc := range []struct {
		name  string
		host  models.Host
		route []uint
		body  string
		check func(t *testing.T, host models.Host, route []uint)
	}{
		{
			name: "a Host that trusts a key, moved",
			host: withKey,
			body: `{"address":"198.51.100.7","port":2222,"jump_host_ids":[2]}`,
			check: func(t *testing.T, host models.Host, route []uint) {
				if host.Address != "198.51.100.7" || host.Port != 2222 || !slices.Equal(route, []uint{2}) {
					t.Errorf("the move was not stored: %s:%d, route %v", host.Address, host.Port, route)
				}
			},
		},
		{
			name: "a Host with no stored login, moved",
			host: noLogin,
			body: `{"address":"198.51.100.7","port":2222,"jump_host_ids":[2]}`,
			check: func(t *testing.T, host models.Host, route []uint) {
				if host.Address != "198.51.100.7" || host.Port != 2222 || !slices.Equal(route, []uint{2}) {
					t.Errorf("the move was not stored: %s:%d, route %v", host.Address, host.Port, route)
				}
			},
		},
		{
			name: "the description, the user, enabled and the SOCKS5 proxy",
			host: storedHost(),
			body: `{"description":"renamed","user":"admin","enabled":false,"socks_enabled":true,` +
				`"socks_port":1080}`,
			check: func(t *testing.T, host models.Host, _ []uint) {
				if host.Description != "renamed" || host.User != "admin" || host.Enabled ||
					!host.SocksEnabled || host.SocksPort != 1080 {
					t.Errorf("the change was not stored: %+v", host)
				}
			},
		},
		{
			name:  "the address, the port and the route sent back as they are",
			host:  storedHost(),
			route: []uint{2},
			body:  `{"address":"192.0.2.1","port":22,"jump_host_ids":[2],"description":"renamed"}`,
			check: func(t *testing.T, host models.Host, route []uint) {
				if host.Description != "renamed" {
					t.Errorf("the change was not stored: %+v", host)
				}
				if !slices.Equal(route, []uint{2}) {
					t.Errorf("the route is %v, want [2]", route)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newHostMoveDB(t, tc.host, tc.route)

			rec, _ := updateHostOn(t, db, tc.body)

			if rec.Code != http.StatusOK {
				t.Fatalf("PUT /api/host/1 = %d, want %d, body: %s", rec.Code, http.StatusOK,
					rec.Body.String())
			}

			host, route := storedHostAndRoute(t, db)
			tc.check(t, host, route)
		})
	}
}

// TestUpdateHostRefusesAMoveWhoseHostGainedALoginOnTheWay covers the gap
// between the read the password is decided on and the transaction the change
// is written in. A Host that had no login when the request was looked at and
// has one by the time it is written would otherwise be moved on a check that
// was never made.
func TestUpdateHostRefusesAMoveWhoseHostGainedALoginOnTheWay(t *testing.T) {
	noLogin := storedHost()
	noLogin.Password = ""

	db := newHostMoveDB(t, noLogin, nil)

	// The login is stored right after the first read of the Host that is made
	// outside a transaction, which is the read the password is decided on.
	// It is written through the default handle, which is free at that point:
	// the read has finished with the connection.
	gained := false

	err := db.Callback().Query().After("gorm:query").Register("test:gain_a_login", func(tx *gorm.DB) {
		if gained || tx.Statement.Table != "hosts" {
			return
		}
		if _, inside := tx.Statement.ConnPool.(gorm.TxCommitter); inside {
			return
		}

		gained = true

		err := db.Session(&gorm.Session{NewDB: true}).Model(&models.Host{}).Where("id = ?", 1).
			Update("password", storedHostPassword).Error
		if err != nil {
			t.Errorf("failed to store the login: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("failed to register the callback: %v", err)
	}

	rec, _ := updateHostOn(t, db, `{"address":"198.51.100.7"}`)

	if !gained {
		t.Fatal("the login was never stored, so the gap was not reached")
	}

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT /api/host/1 = %d, want %d, body: %s", rec.Code, http.StatusBadRequest,
			rec.Body.String())
	}

	if code := refusalCode(t, rec); code != "host.update.account_password.required" {
		t.Errorf("error_code = %q, want %q", code, "host.update.account_password.required")
	}

	host, _ := storedHostAndRoute(t, db)
	if host.Address != "192.0.2.1" {
		t.Errorf("the address is %q, want it left at 192.0.2.1", host.Address)
	}
}

// trustHostKeys marks every Host stored so far as trusting a host key, which is
// what lets a test move one without the password of the account. The tests
// that call it are about what a move stores or is refused for on other
// grounds, and hostFixture has no account to check a password against.
func trustHostKeys(t *testing.T, f *hostFixture) {
	t.Helper()

	trusted, _ := newHostKey(t)

	err := f.db.Model(&models.Host{}).Where("1 = 1").Update("host_key", trusted).Error
	if err != nil {
		t.Fatalf("failed to store the trusted keys: %v", err)
	}
}
