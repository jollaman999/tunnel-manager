package api

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/settings"
)

// TestUpdateSettingsKeepsBothOfTwoSavesThatMeet covers two saves of different
// settings that arrive at once, the screen saving one card while a client with
// a token saves another. The row is written whole, so a save that read the set
// before the other one committed would write the other setting back to what
// it was, and both saves would still answer that they stored what they were
// sent.
//
// The pool is held to one connection, as database.NewDatabase holds it. The
// two requests are run many times over because which of them reads first is
// left to the scheduler: a save that reads outside its transaction loses the
// other one on some rounds and not on others.
func TestUpdateSettingsKeepsBothOfTwoSavesThatMeet(t *testing.T) {
	db := newSettingsDB(t)

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to reach the connection pool: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	h, _, _, _ := newSettingsHandler(t, db)

	for round := 1; round <= 50; round++ {
		monitoring := 10 + round
		reconcile := 100 + round

		var wg sync.WaitGroup

		codes := make([]int, 2)
		bodies := []string{
			fmt.Sprintf(`{"monitoring_interval_sec":%d}`, monitoring),
			fmt.Sprintf(`{"reconcile_interval_sec":%d}`, reconcile),
		}

		for i, body := range bodies {
			wg.Add(1)

			go func() {
				defer wg.Done()

				codes[i] = settingsRequest(t, h, body).Code
			}()
		}

		wg.Wait()

		for i, code := range codes {
			if code != http.StatusOK {
				t.Fatalf("round %d: the save of %s answered %d", round, bodies[i], code)
			}
		}

		stored, err := settings.Load(db)
		if err != nil {
			t.Fatalf("failed to read the settings: %v", err)
		}

		if stored.MonitoringIntervalSec != monitoring || stored.ReconcileIntervalSec != reconcile {
			t.Fatalf("round %d: stored monitoring %d and reconcile %d, want %d and %d: one save put back what the other stored",
				round, stored.MonitoringIntervalSec, stored.ReconcileIntervalSec, monitoring, reconcile)
		}
	}
}
