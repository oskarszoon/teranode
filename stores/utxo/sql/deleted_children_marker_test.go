package sql

// deleted_children marker tests for the SQL pruner's single-evaluation rewrite.
//
// These cover the two invariants the pruner rewrite added on top of the marker
// behaviour already exercised by counter_conflicting_dangling_test.go:
//
//   - ChiR2: a marker is never written for a parent whose record was already reaped
//     in an earlier pass (the INNER JOIN transactions p in deleteTombstoned). The
//     walk already fails closed on a missing parent, so such a row would carry no
//     information and only leak into the marker table.
//   - The defensive branch (pruner_service.go deleteTombstoned, defensive-mode
//     predicate): a tombstoned parent is protected while ANY spending child is
//     unstable, and the deletable ones still leave markers on their surviving input
//     parents.
//
// Fixtures mirror counter_conflicting_dangling_test.go: real sqlite-backed store,
// danglingParentTx / danglingSpendTx builders, delete_at_height driven by raw SQL.

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	sqlpruner "github.com/bsv-blockchain/teranode/stores/utxo/sql/pruner"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// newDefensiveTestPruner builds the SQL pruner in DEFENSIVE mode against the test
// store's own database handle, with an explicit (small) safety window so a child can
// be made "stable" at a low block height without needing a 288-block fixture.
func newDefensiveTestPruner(ctx context.Context, t *testing.T, store *Store, safetyWindow uint32) *sqlpruner.Service {
	t.Helper()

	tSettings := test.CreateBaseTestSettings(t)
	tSettings.Pruner.UTXODefensiveEnabled = true // exercise the child-stability predicate

	svc, err := sqlpruner.NewService(tSettings, sqlpruner.Options{
		Logger:       ulogger.TestLogger{},
		DB:           store.db,
		Ctx:          ctx,
		SafetyWindow: safetyWindow,
	})
	require.NoError(t, err)

	return svc
}

// TestSQLPruner_NoMarkerLeakWhenParentPrunedEarlier verifies the ChiR2 join: once a
// parent P has been reaped, a later pass that reaps its child C must NOT resurrect a
// (P, C) marker. The marker points at an already-gone parent (the walk fails closed on
// a missing parent regardless), so writing it would only leak an orphaned row.
func TestSQLPruner_NoMarkerLeakWhenParentPrunedEarlier(t *testing.T) {
	ctx := context.Background()
	store, _ := setup(ctx, t)

	// P is the parent; C spends P[0].
	parent := danglingParentTx(0x81)
	_, err := store.Create(ctx, parent, 100)
	require.NoError(t, err)

	child := danglingSpendTx(t, parent, 80000)
	_, err = store.Create(ctx, child, 100)
	require.NoError(t, err)

	// Record C spending P so C's input genuinely references P.
	_, err = store.Spend(ctx, child, 101)
	require.NoError(t, err)

	prunerSvc := newDanglingTestPruner(ctx, t, store)

	// Pass 1: tombstone and reap the PARENT only (the child is not yet deletable).
	_, err = store.db.ExecContext(ctx, `UPDATE transactions SET delete_at_height = $1 WHERE hash = $2`, 150, parent.TxIDChainHash()[:])
	require.NoError(t, err)

	pruned, err := prunerSvc.Prune(ctx, 150, "test")
	require.NoError(t, err)
	require.EqualValues(t, 1, pruned, "pass 1 must reap the parent only")

	// Pass 2: tombstone and reap the CHILD; its parent is already gone.
	_, err = store.db.ExecContext(ctx, `UPDATE transactions SET delete_at_height = $1 WHERE hash = $2`, 200, child.TxIDChainHash()[:])
	require.NoError(t, err)

	pruned, err = prunerSvc.Prune(ctx, 200, "test")
	require.NoError(t, err)
	require.EqualValues(t, 1, pruned, "pass 2 must reap the child")

	// No deleted_children row may reference the already-reaped parent.
	var markerCount int
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM deleted_children WHERE parent_hash = $1`, parent.TxIDChainHash()[:]).Scan(&markerCount))
	require.Equal(t, 0, markerCount, "the ChiR2 join must not write a marker for a parent reaped in an earlier pass")

	// Specifically, no (reaped-parent, reaped-child) pair leaked.
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM deleted_children WHERE parent_hash = $1 AND child_hash = $2`,
		parent.TxIDChainHash()[:], child.TxIDChainHash()[:]).Scan(&markerCount))
	require.Equal(t, 0, markerCount, "no (pruned-parent, reaped-child) marker may leak")
}

// TestSQLPruner_DefensiveModeProtectsUnstableChildAndMarksDeletable exercises the
// defensive branch of deleteTombstoned (the child-stability predicate that plain-delete
// mode skips). Two tombstoned parents are prepared:
//
//   - D spends the surviving parent P1 and its own output is spent by a mined, stable
//     child DC → D is deletable and must leave a (P1, D) marker.
//   - Q spends the surviving parent P2 but its own output is spent by an UNMINED child
//     QC → Q is unstable and must be protected (not reaped, no marker).
func TestSQLPruner_DefensiveModeProtectsUnstableChildAndMarksDeletable(t *testing.T) {
	ctx := context.Background()
	store, _ := setup(ctx, t)

	const (
		safetyWindow = uint32(5)
		pruneHeight  = uint32(200)
		stableHeight = int64(190) // <= pruneHeight - safetyWindow (195) ⇒ stable
	)

	// --- Group A: deletable via a stable, mined child ⇒ marker on P1 ---
	p1 := danglingParentTx(0x81)
	_, err := store.Create(ctx, p1, 100)
	require.NoError(t, err)

	d := danglingSpendTx(t, p1, 90000) // spends P1[0]
	_, err = store.Create(ctx, d, 100)
	require.NoError(t, err)

	dc := danglingSpendTx(t, d, 80000) // spends D[0]
	_, err = store.Create(ctx, dc, 100)
	require.NoError(t, err)

	// Record DC spending D so D[0].spending_data points at DC.
	_, err = store.Spend(ctx, dc, 190)
	require.NoError(t, err)

	// Make DC mined and stable: unmined_since NULL + a block_ids row at a stable height.
	_, err = store.db.ExecContext(ctx, `UPDATE transactions SET unmined_since = NULL WHERE hash = $1`, dc.TxIDChainHash()[:])
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx,
		`INSERT INTO block_ids (transaction_id, block_id, block_height, subtree_idx) SELECT id, 900001, $1, 0 FROM transactions WHERE hash = $2`,
		stableHeight, dc.TxIDChainHash()[:])
	require.NoError(t, err)

	// --- Group B: protected by an unstable (unmined) child ⇒ no marker on P2 ---
	p2 := danglingParentTx(0x82)
	_, err = store.Create(ctx, p2, 100)
	require.NoError(t, err)

	q := danglingSpendTx(t, p2, 90000) // spends P2[0]
	_, err = store.Create(ctx, q, 100)
	require.NoError(t, err)

	qc := danglingSpendTx(t, q, 80000) // spends Q[0]
	_, err = store.Create(ctx, qc, 100)
	require.NoError(t, err)

	// Record QC spending Q; QC stays unmined (Create leaves unmined_since set, and we
	// add no block_ids row) ⇒ Q has an unstable child.
	_, err = store.Spend(ctx, qc, 190)
	require.NoError(t, err)

	// Tombstone both parents at the prune height.
	_, err = store.db.ExecContext(ctx, `UPDATE transactions SET delete_at_height = $1 WHERE hash IN ($2, $3)`,
		pruneHeight, d.TxIDChainHash()[:], q.TxIDChainHash()[:])
	require.NoError(t, err)

	pruned, err := newDefensiveTestPruner(ctx, t, store, safetyWindow).Prune(ctx, pruneHeight, "test")
	require.NoError(t, err)
	require.EqualValues(t, 1, pruned, "only D (stable child) is deletable; Q (unstable child) must be protected")

	// D was reaped...
	_, err = store.Get(ctx, d.TxIDChainHash(), fields.Conflicting)
	require.Error(t, err)
	require.True(t, errors.IsNotFound(err), "the deletable parent D must be gone")

	// ...and its surviving input parent P1 carries the marker.
	p1Meta, err := store.Get(ctx, p1.TxIDChainHash(), fields.DeletedChildren)
	require.NoError(t, err)
	require.Contains(t, p1Meta.DeletedChildren, *d.TxIDChainHash(),
		"the reaped deletable parent must leave a deleted_children marker on its surviving parent")

	// Q was protected by its unstable child — still present, no marker on P2.
	_, err = store.Get(ctx, q.TxIDChainHash(), fields.Conflicting)
	require.NoError(t, err, "the parent with an unstable (unmined) child must NOT be reaped")

	var markerCount int
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM deleted_children WHERE parent_hash = $1`, p2.TxIDChainHash()[:]).Scan(&markerCount))
	require.Equal(t, 0, markerCount, "a protected (never-reaped) parent must leave no marker")

	// The stable child DC survives (only D was tombstoned).
	_, err = store.Get(ctx, dc.TxIDChainHash(), fields.Conflicting)
	require.NoError(t, err, "the stable child DC must survive")
}
