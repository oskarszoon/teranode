package pruner

import (
	"context"
	"fmt"
	"time"

	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/utxo/pruner"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/usql"
)

// Ensure Store implements the Pruner Service interface
var _ pruner.Service = (*Service)(nil)

// Service implements the utxo.CleanupService interface for SQL-based UTXO stores
type Service struct {
	safetyWindow     uint32 // Block height retention for child stability verification
	defensiveEnabled bool   // Enable defensive checks before deleting UTXO transactions
	logger           ulogger.Logger
	settings         *settings.Settings
	db               *usql.DB
	ctx              context.Context
}

// Options contains configuration options for the cleanup service
type Options struct {
	// Logger is the logger to use
	Logger ulogger.Logger

	// DB is the SQL database connection
	DB *usql.DB

	// Ctx is the context to use to signal shutdown
	Ctx context.Context

	// SafetyWindow is the number of blocks a child must be stable before parent deletion
	// If not specified, defaults to global_blockHeightRetention (288 blocks)
	SafetyWindow uint32
}

// NewService creates a new cleanup service for the SQL store
func NewService(tSettings *settings.Settings, opts Options) (*Service, error) {
	if opts.Logger == nil {
		return nil, errors.NewProcessingError("logger is required")
	}

	if tSettings == nil {
		return nil, errors.NewProcessingError("settings is required")
	}

	if opts.DB == nil {
		return nil, errors.NewProcessingError("db is required")
	}

	safetyWindow := opts.SafetyWindow
	if safetyWindow == 0 {
		// Default to global retention setting (288 blocks)
		safetyWindow = tSettings.GlobalBlockHeightRetention
	}

	service := &Service{
		safetyWindow:     safetyWindow,
		defensiveEnabled: tSettings.Pruner.UTXODefensiveEnabled,
		logger:           opts.Logger,
		settings:         tSettings,
		db:               opts.DB,
		ctx:              opts.Ctx,
	}

	return service, nil
}

// Start starts the cleanup service
func (s *Service) Start(ctx context.Context) {
	s.logger.Infof("[SQLCleanupService] service ready")
}

// AddObserver adds an observer to be notified when pruning completes.
// This is a no-op for the SQL pruner service as it doesn't support observers yet.
func (s *Service) AddObserver(observer pruner.Observer) {
	// No-op: SQL pruner doesn't support observers yet
}

// Prune removes transactions marked for deletion at or before the specified height.
// Returns the number of records processed and any error encountered.
// This method is synchronous and blocks until pruning completes or context is cancelled.
func (s *Service) Prune(ctx context.Context, blockHeight uint32, blockHashStr string) (int64, error) {
	if blockHeight == 0 {
		return 0, errors.NewProcessingError("Cannot prune at block height 0")
	}

	startTime := time.Now()

	// Log start of cleanup
	s.logger.Infof("[pruner][%s:%d] phase 2: starting cleanup scan (delete_at_height <= %d)",
		blockHashStr, blockHeight, blockHeight)

	// Execute the cleanup
	deletedCount, err := s.deleteTombstoned(ctx, blockHeight)
	if err != nil {
		s.logger.Errorf("[pruner][%s:%d] phase 2: cleanup failed: %v", blockHashStr, blockHeight, err)
		return 0, err
	}

	// Calculate throughput
	elapsed := time.Since(startTime)
	tps := float64(deletedCount) / elapsed.Seconds()

	// Format TPS for readability
	var tpsStr string
	if tps >= 1_000_000 {
		tpsStr = fmt.Sprintf("%.1fM records/sec", tps/1_000_000)
	} else if tps >= 1_000 {
		tpsStr = fmt.Sprintf("%.1fK records/sec", tps/1_000)
	} else {
		tpsStr = fmt.Sprintf("%.2f records/sec", tps)
	}

	s.logger.Infof("[pruner][%s:%d] phase 2: completed cleanup in %v: deleted %s records (%s)",
		blockHashStr, blockHeight, elapsed, util.FormatComma(deletedCount), tpsStr)

	return deletedCount, nil
}

// deleteTombstoned removes transactions that have passed their expiration time.
// Only deletes parent transactions if their last spending child is mined and stable.
//
// Before the DELETE — in the same database transaction — it records a
// deleted_children marker row (parent_hash, child_hash) on each deleted tx's input
// parents, mirroring the aerospike deletedChildren bin. The counter-conflicting
// fail-closed guards consume these markers to discriminate a deliberately reaped
// spender from a tolerable ghost. Markers whose parent is itself deleted in the same
// pass are removed again (a missing parent already fails the walk closed), and a marker
// is never written for a parent whose record is already gone (an earlier pass reaped
// it) — together bounding the table's growth to (surviving parent, reaped child) pairs.
// The deletable set is materialized once into a temporary table that every statement
// reads from, so the marker INSERT and the DELETE always operate on the exact same
// rows: the old postgres READ COMMITTED window (a row that starts qualifying mid-
// transaction, deleted unmarked, failing the walk open) is closed.
func (s *Service) deleteTombstoned(ctx context.Context, blockHeight uint32) (int64, error) {
	// Use configured safety window from settings
	safetyWindow := s.safetyWindow

	// Defensive child verification is conditional on the UTXODefensiveEnabled setting
	// When disabled, parents are deleted without verifying children are stable
	var (
		deletableIDs string
		args         []interface{}
	)

	if !s.defensiveEnabled {
		// Defensive mode disabled - delete all transactions past their expiration
		deletableIDs = `
			SELECT t.id
			FROM transactions t
			WHERE t.delete_at_height IS NOT NULL
			  AND t.delete_at_height <= $1
		`
		args = []interface{}{blockHeight}
	} else {
		// Defensive mode enabled - verify ALL spending children are stable before deletion
		// This prevents orphaning any child transaction
		deletableIDs = `
			SELECT t.id
			FROM transactions t
			WHERE t.delete_at_height IS NOT NULL
			  AND t.delete_at_height <= $1
			  AND NOT EXISTS (
			    -- Find ANY unstable child - if found, parent cannot be deleted
			    -- This ensures ALL children must be stable before parent deletion
			    SELECT 1
			    FROM outputs o
			    WHERE o.transaction_id = t.id
			      AND o.spending_data IS NOT NULL
			      AND (
			        -- Extract child TX hash from spending_data (first 32 bytes)
			        -- Check if this child is NOT stable
			        NOT EXISTS (
			          SELECT 1
			          FROM transactions child
			          INNER JOIN block_ids child_blocks ON child.id = child_blocks.transaction_id
			          WHERE child.hash = substr(o.spending_data, 1, 32)
			            AND child.unmined_since IS NULL  -- Child must be mined
			            AND child_blocks.block_height <= ($1 - $2)  -- Child must be stable
			        )
			      )
			  )
		`
		args = []interface{}{blockHeight, safetyWindow}
	}

	txn, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, errors.NewStorageError("failed to begin prune transaction", err)
	}

	defer func() {
		_ = txn.Rollback() // no-op after Commit
	}()

	// Materialize the deletable set ONCE into a temporary table that all three
	// statements below read from. Two whys:
	//   1. Correctness: a single evaluation of the (possibly correlated) predicate kills
	//      the READ COMMITTED drift. Re-running <deletableIDs> per statement let a row
	//      become deletable between the marker INSERT and the DELETE (postgres READ
	//      COMMITTED) — the DELETE would then reap it WITHOUT a marker, and the
	//      counter-conflicting walk fails OPEN on that unmarked ghost. Binding every
	//      statement to the same frozen id set makes "deleted" and "marked" agree.
	//   2. Cost: the predicate — especially the defensive-mode NOT EXISTS correlated
	//      subquery — evaluated 3x per pass; now it runs once.
	// CREATE TEMPORARY TABLE ... AS SELECT is portable across sqlite and postgres. The
	// table is dropped as the last statement before COMMIT: sqlite temp tables live for
	// the connection, not the transaction, so a surviving one would leak back to the
	// pool and make the next pass's CREATE collide. Error paths roll back the txn, which
	// also discards the temp table.
	createBatchQuery := `CREATE TEMPORARY TABLE prune_batch_ids AS ` + deletableIDs
	if _, err = txn.ExecContext(ctx, createBatchQuery, args...); err != nil {
		return 0, errors.NewStorageError("failed to materialize prune batch", err)
	}

	// 1: mark each deleted tx on its input parents. The INNER JOIN to transactions p
	// (ChiR2) refuses to write a marker whose parent record is already gone — an earlier
	// pass reaped it, and a missing parent already fails the walk closed, so the marker
	// would carry no information and only leak. Same-pass parents still match (they die
	// in statement 3 below) and are removed by the cleanup in statement 2, keeping the
	// table to (surviving parent, reaped child) pairs.
	insertMarkersQuery := `
		INSERT INTO deleted_children (parent_hash, child_hash)
		SELECT DISTINCT i.previous_transaction_hash, tx.hash
		FROM transactions tx
		INNER JOIN inputs i ON i.transaction_id = tx.id
		INNER JOIN transactions p ON p.hash = i.previous_transaction_hash
		WHERE tx.id IN (SELECT id FROM prune_batch_ids)
		ON CONFLICT (parent_hash, child_hash) DO NOTHING
	`
	// A marker-INSERT failure aborts the whole pass deliberately: deleting a spender
	// without recording its marker is the one outcome this table must never allow — it
	// would leave the walk unable to tell a reaped spender from a tolerable ghost.
	if _, err = txn.ExecContext(ctx, insertMarkersQuery); err != nil {
		return 0, errors.NewStorageError("failed to insert deleted_children markers", err)
	}

	// 2: drop markers whose parent is deleted in this pass (including ones just
	// inserted) — the walk already fails closed on a missing parent, and this keeps
	// the marker table from growing past the surviving parents.
	cleanupMarkersQuery := `
		DELETE FROM deleted_children
		WHERE parent_hash IN (
			SELECT tx.hash FROM transactions tx WHERE tx.id IN (SELECT id FROM prune_batch_ids)
		)
	`
	if _, err = txn.ExecContext(ctx, cleanupMarkersQuery); err != nil {
		return 0, errors.NewStorageError("failed to clean up deleted_children markers", err)
	}

	// 3: the delete itself.
	deleteQuery := `DELETE FROM transactions WHERE id IN (SELECT id FROM prune_batch_ids)`

	result, err := txn.ExecContext(ctx, deleteQuery)
	if err != nil {
		return 0, errors.NewStorageError("failed to delete transactions", err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return 0, errors.NewStorageError("failed to get rows affected", err)
	}

	// Drop the temp table before COMMIT so it does not leak back to the pooled
	// connection (see the createBatchQuery comment above).
	if _, err = txn.ExecContext(ctx, `DROP TABLE prune_batch_ids`); err != nil {
		return 0, errors.NewStorageError("failed to drop prune batch table", err)
	}

	if err = txn.Commit(); err != nil {
		return 0, errors.NewStorageError("failed to commit prune transaction", err)
	}

	return count, nil
}
