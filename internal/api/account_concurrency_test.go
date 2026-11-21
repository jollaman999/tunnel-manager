package api

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

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
