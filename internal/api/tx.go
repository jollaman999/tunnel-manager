package api

import "gorm.io/gorm"

// rollbackUnlessDone rolls back a transaction that has not been committed or
// rolled back yet. Every handler that opens a transaction defers it straight
// after Begin, so that a panic between Begin and Commit gives the connection
// back. The pool holds a single connection (internal/database,
// SetMaxOpenConns(1)) and the Recover middleware keeps the process up after a
// panic, so a transaction left open by one would hold that connection for as
// long as the process runs, and every query after it would wait for good.
//
// On the paths that already committed or rolled back, the rollback here finds
// the transaction done and answers sql.ErrTxDone, which is let go: there is
// nothing left to undo.
func rollbackUnlessDone(tx *gorm.DB) {
	tx.Rollback()
}
