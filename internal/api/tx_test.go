package api

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// newSingleConnDB opens a database whose pool holds one connection, as the
// server's does, with a table of one column to write to.
func newSingleConnDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "tx.db")), &gorm.Config{
		Logger: gormlogger.Discard,
	})
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

	err = db.Exec("CREATE TABLE rows_written (v INTEGER)").Error
	if err != nil {
		t.Fatalf("failed to create the table: %v", err)
	}

	return db
}

// panicInTransaction opens a transaction, writes a row and panics before the
// commit, the way a handler would if something between Begin and Commit went
// wrong. The panic is recovered here as the Recover middleware would. The
// transaction is handed back so that a caller that did not defer the rollback
// can still let it go.
func panicInTransaction(t *testing.T, db *gorm.DB, deferRollback bool) (tx *gorm.DB) {
	t.Helper()

	defer func() {
		if recover() == nil {
			t.Fatal("the transaction did not panic")
		}
	}()

	tx = db.Begin()
	if tx.Error != nil {
		t.Fatalf("failed to start the transaction: %v", tx.Error)
	}
	if deferRollback {
		defer rollbackUnlessDone(tx)
	}

	err := tx.Exec("INSERT INTO rows_written (v) VALUES (1)").Error
	if err != nil {
		t.Fatalf("failed to write the row: %v", err)
	}

	panic("between Begin and Commit")
}

// countRows reads how many rows the table holds, giving up after the timeout
// rather than waiting on a connection that is never given back.
func countRows(db *gorm.DB, timeout time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var n int64
	err := db.WithContext(ctx).Raw("SELECT COUNT(*) FROM rows_written").Scan(&n).Error

	return n, err
}

// A panic between Begin and Commit gives the one connection back when the
// rollback is deferred, and the row the transaction wrote is not kept. Without
// the deferred rollback the same panic leaves the connection held, which the
// first half shows, so that the second half is not passing on a pool that
// would have let the query through anyway.
func TestRollbackUnlessDoneReleasesConnectionAfterPanic(t *testing.T) {
	db := newSingleConnDB(t)

	held := panicInTransaction(t, db, false)

	_, err := countRows(db, 200*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a query after a panic with no deferred rollback should wait on the held connection, got %v", err)
	}

	held.Rollback()

	panicInTransaction(t, db, true)

	n, err := countRows(db, 5*time.Second)
	if err != nil {
		t.Fatalf("a query after a panic with the deferred rollback should go through, got %v", err)
	}
	if n != 0 {
		t.Fatalf("the rolled back transaction left %d rows, want 0", n)
	}
}

// The deferred rollback leaves alone a transaction that was committed, and the
// rows it wrote stay.
func TestRollbackUnlessDoneKeepsCommit(t *testing.T) {
	db := newSingleConnDB(t)

	func() {
		tx := db.Begin()
		if tx.Error != nil {
			t.Fatalf("failed to start the transaction: %v", tx.Error)
		}
		defer rollbackUnlessDone(tx)

		err := tx.Exec("INSERT INTO rows_written (v) VALUES (1)").Error
		if err != nil {
			t.Fatalf("failed to write the row: %v", err)
		}

		err = tx.Commit().Error
		if err != nil {
			t.Fatalf("failed to commit: %v", err)
		}
	}()

	n, err := countRows(db, 5*time.Second)
	if err != nil {
		t.Fatalf("failed to count the rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("the committed transaction left %d rows, want 1", n)
	}
}
