package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/auth"
	"github.com/jollaman999/tunnel-manager/internal/models"
)

// TestAccountKeepsBothOfTwoChangesThatMeet covers a rename and a change of
// the password sent at once, from two tabs of the same session. Each used to
// write back the whole row it had read, so the one that landed second put back
// the value the first had changed, and both answered that their change was
// made.
//
// What either order may end in is one of two things: both changes stored, or
// one of them stored and the other refused. A change that answers 200 and is
// not in the row is the failure. There are several rounds because which of the two
// reads first is left to the scheduler.
func TestAccountKeepsBothOfTwoChangesThatMeet(t *testing.T) {
	for round := 1; round <= 5; round++ {
		f := newAccountFixture(t)

		sqlDB, err := f.db.DB()
		if err != nil {
			t.Fatalf("failed to reach the connection pool: %v", err)
		}
		sqlDB.SetMaxOpenConns(1)

		cookies := f.signIn(t, testUsername, testPassword)

		var wg sync.WaitGroup

		codes := make([]int, 2)
		renamedTo := ""

		wg.Add(2)

		go func() {
			defer wg.Done()

			rec := f.changeAccount(t, cookies, testPassword, accountNewUsername, "")
			codes[0] = rec.Code

			if rec.Code == http.StatusOK {
				var answer struct {
					Data accountChanged `json:"data"`
				}

				if json.Unmarshal(rec.Body.Bytes(), &answer) == nil {
					renamedTo = answer.Data.Username
				}
			}
		}()

		go func() {
			defer wg.Done()

			codes[1] = f.changeAccount(t, cookies, testPassword, "", testNewPassword).Code
		}()

		wg.Wait()

		var user models.User

		err = f.db.First(&user).Error
		if err != nil {
			t.Fatalf("failed to read the account back: %v", err)
		}

		if codes[0] == http.StatusOK {
			if user.Username != accountNewUsername {
				t.Fatalf("round %d: the rename answered 200 and the row holds %q", round, user.Username)
			}

			if renamedTo != accountNewUsername {
				t.Fatalf("round %d: the rename answered with the name %q", round, renamedTo)
			}
		}

		if codes[1] == http.StatusOK && !auth.CheckPassword(user.PasswordHash, testNewPassword) {
			t.Fatalf("round %d: the change of the password answered 200 and the row holds another one", round)
		}

		if codes[0] != http.StatusOK && codes[1] != http.StatusOK {
			t.Fatalf("round %d: neither change went through: %d and %d", round, codes[0], codes[1])
		}
	}
}

// raceRounds is how many times each of the races below is run. Which of the
// two requests gets to the database first is left to the scheduler, and a
// single round lands on the side that does not fail often enough to hide the
// race; this many failed every time before the fix.
const raceRounds = 5

// TestALoginThatMeetsAPasswordChangeLeavesNoSession covers a login that
// compares the old password while the account is being changed. The change
// commits and ends every other session, and the login, whose bcrypt compare
// was on the hash read before the commit, then made a session that nothing
// was left to end: whoever held the old password kept the account after the
// operator changed it.
//
// Logins are sent one after another the whole time the change runs, so that
// one of them is in the middle of bcrypt when it lands. A login that has
// finished before the change is ended by it, and one that started after it
// is refused by the hash, so every session made with the old password has to
// be gone afterwards.
func TestALoginThatMeetsAPasswordChangeLeavesNoSession(t *testing.T) {
	for round := 1; round <= raceRounds; round++ {
		f := newAccountFixture(t)

		sqlDB, err := f.db.DB()
		if err != nil {
			t.Fatalf("failed to reach the connection pool: %v", err)
		}
		sqlDB.SetMaxOpenConns(1)

		owner := f.signIn(t, testUsername, testPassword)

		var stop atomic.Bool

		var mu sync.Mutex

		var made [][]*http.Cookie

		var wg sync.WaitGroup

		wg.Add(1)

		go func() {
			defer wg.Done()

			body := loginBody(t, testUsername, testPassword)
			for !stop.Load() {
				rec := doOverTLS(f.e, f.overTLS, http.MethodPost, "/api/login", body)

				session := sessionCookieOf(rec)
				if session != nil {
					mu.Lock()
					made = append(made, []*http.Cookie{session, csrfKeyOf(rec)})
					mu.Unlock()
				}
			}
		}()

		time.Sleep(150 * time.Millisecond)

		rec := f.changeAccount(t, owner, testPassword, "", testNewPassword)

		stop.Store(true)
		wg.Wait()

		if rec.Code != http.StatusOK {
			t.Fatalf("round %d: the change answered %d, body: %s", round, rec.Code, rec.Body.String())
		}

		alive := 0

		for _, cookies := range made {
			if f.reachable(cookies) == http.StatusOK {
				alive++
			}
		}

		if alive > 0 {
			t.Errorf("round %d: %d of the %d sessions made with the old password still open the account "+
				"after it was changed", round, alive, len(made))
		}
	}
}

// TestATokenThatMeetsAPasswordChangeIsNotStored is the same race on an API
// token. The creation checks the account password before its transaction is
// opened, so a change that committed in between, revoking every token, was
// followed by the insert of a token made with the password it had just
// replaced. The change answers that every token was revoked, and that has to
// be true of the table once both are done.
func TestATokenThatMeetsAPasswordChangeIsNotStored(t *testing.T) {
	for round := 1; round <= raceRounds; round++ {
		f := newAccountFixture(t)

		sqlDB, err := f.db.DB()
		if err != nil {
			t.Fatalf("failed to reach the connection pool: %v", err)
		}
		sqlDB.SetMaxOpenConns(1)

		g := f.e.Group("/api")
		g.Use(f.auth.RequireSession())
		g.POST("/token", f.auth.CreateToken)

		owner := f.signIn(t, testUsername, testPassword)
		other := f.signIn(t, testUsername, testPassword)

		var stop atomic.Bool

		var wg sync.WaitGroup

		wg.Add(1)

		go func() {
			defer wg.Done()

			days := 0
			for i := 0; !stop.Load(); i++ {
				body, err := json.Marshal(tokenRequest{
					Name:            fmt.Sprintf("raced-%d", i),
					Scopes:          []string{TokenScopeRead},
					ExpiresInDays:   &days,
					AccountPassword: testPassword,
				})
				if err != nil {
					t.Errorf("failed to build the token body: %v", err)
					return
				}

				doOverTLS(f.e, f.overTLS, http.MethodPost, tokenListPath, string(body), other...)
			}
		}()

		time.Sleep(150 * time.Millisecond)

		rec := f.changeAccount(t, owner, testPassword, "", testNewPassword)

		stop.Store(true)
		wg.Wait()

		if rec.Code != http.StatusOK {
			t.Fatalf("round %d: the change answered %d, body: %s", round, rec.Code, rec.Body.String())
		}

		var left int64

		err = f.db.Model(&models.APIToken{}).Count(&left).Error
		if err != nil {
			t.Fatalf("failed to count the API tokens: %v", err)
		}

		if left != 0 {
			t.Errorf("round %d: %d API tokens made with the old password are stored after the change "+
				"that revoked every token", round, left)
		}
	}
}
