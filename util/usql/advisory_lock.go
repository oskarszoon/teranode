package usql

import (
	"context"
	"fmt"

	"github.com/bsv-blockchain/teranode/errors"
)

// WithAdvisoryLock acquires a PostgreSQL session-level advisory lock, executes fn,
// then releases the lock. This serializes concurrent DDL operations (like CREATE TABLE
// IF NOT EXISTS) across multiple pods sharing the same database, preventing race
// conditions on PostgreSQL system catalog indexes.
//
// The lockID should be a unique constant per schema-creation context (e.g. one for
// blockchain store, one for UTXO store, one for banlist).
//
// This is a no-op wrapper for non-PostgreSQL databases — callers must check the
// engine type themselves before calling.
func WithAdvisoryLock(ctx context.Context, db *DB, lockID int64, fn func() error) error {
	if _, err := db.ExecContext(ctx, fmt.Sprintf("SELECT pg_advisory_lock(%d)", lockID)); err != nil {
		return errors.New(errors.ERR_ERROR, "failed to acquire advisory lock %d: %w", lockID, err)
	}

	defer func() {
		// Release on a background context — if the parent context was cancelled we
		// still need to release the lock to avoid blocking other pods.
		_, _ = db.Exec(fmt.Sprintf("SELECT pg_advisory_unlock(%d)", lockID))
	}()

	return fn()
}
