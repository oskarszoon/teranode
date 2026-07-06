package sql

// Dangling-reference tolerance tests for the counter-conflicting traversal.
//
// A "dangling reference" is a parent UTXO whose SpendingDatas still points at a
// spender transaction whose own record has been removed from the store (pruned,
// reorged, or otherwise deleted) while the parent survived. GetConflictingChildren
// (BFS) and GetCounterConflictingTxHashes must tolerate this: a spender that no
// longer exists is not a reason to fail the whole counter-conflicting check with a
// TX_NOT_FOUND error.
//
// These are integration tests against the SQLite-backed store so the dangling ref
// is real (the parent row genuinely references a deleted child), not a mock of the
// error. On current (pre-fix) code they FAIL: the deleted spender's Get surfaces
// NewTxNotFoundError, which the traversal propagates.

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	sqlpruner "github.com/bsv-blockchain/teranode/stores/utxo/sql/pruner"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// danglingUnlockingScript is a minimal unlocking script so the fixture tx is
// storable (SQL store has NOT NULL on unlocking_script).
var danglingUnlockingScript = bscript.NewFromBytes([]byte{0x00, 0x48, 0x30, 0x45})

// danglingParentTx builds a standalone parent tx with one spendable output. The
// seed makes the txid unique so multiple fixtures can share one store.
func danglingParentTx(seed byte) *bt.Tx {
	parent := bt.NewTx()
	in := &bt.Input{
		PreviousTxOutIndex: 0,
		PreviousTxSatoshis: 200000,
		PreviousTxScript:   bscript.NewFromBytes([]byte{0x51}),
		SequenceNumber:     0xFFFFFFFF,
		UnlockingScript:    danglingUnlockingScript,
	}
	_ = in.PreviousTxIDAdd(&chainhash.Hash{seed, seed, seed, 0xCC})
	parent.Inputs = []*bt.Input{in}
	parent.Outputs = []*bt.Output{{Satoshis: 100000, LockingScript: bscript.NewFromBytes([]byte{0x52})}}

	return parent
}

// markTxMined flags a stored tx as mined on our longest chain — unmined_since NULL plus a
// block_ids row — so the F1 pruner marker gate (mined-only) writes a deleted_children
// marker when the tx is reaped. Without it a Create'd tx stays never-mined (Create leaves
// unmined_since set and adds no block_ids row), and the pruner intentionally leaves such a
// reaped child UNMARKED (marker ⟹ mined-on-our-chain). block_id 900001 is arbitrary; the
// block_ids PK is (transaction_id, block_id) so distinct txs may share it.
func markTxMined(ctx context.Context, t *testing.T, store *Store, tx *bt.Tx, blockHeight int64) {
	t.Helper()
	_, err := store.db.ExecContext(ctx, `UPDATE transactions SET unmined_since = NULL WHERE hash = $1`, tx.TxIDChainHash()[:])
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx,
		`INSERT INTO block_ids (transaction_id, block_id, block_height, subtree_idx) SELECT id, 900001, $1, 0 FROM transactions WHERE hash = $2`,
		blockHeight, tx.TxIDChainHash()[:])
	require.NoError(t, err)
}

// danglingSpendTx builds a tx spending parent[0]; outSats makes its txid unique.
func danglingSpendTx(t *testing.T, parent *bt.Tx, outSats uint64) *bt.Tx {
	t.Helper()
	tx := bt.NewTx()
	require.NoError(t, tx.From(parent.TxIDChainHash().String(), 0, parent.Outputs[0].LockingScript.String(), parent.Outputs[0].Satoshis))
	tx.Inputs[0].UnlockingScript = danglingUnlockingScript
	tx.Outputs = []*bt.Output{{Satoshis: outSats, LockingScript: bscript.NewFromBytes([]byte{0x52})}}

	return tx
}

// TestGetConflictingChildren_ToleratesDanglingSpendingRef verifies the BFS in
// GetConflictingChildren does not error when a parent's SpendingDatas points at a
// deleted spender. Pre-fix: BFS fetches the deleted child and returns TX_NOT_FOUND.
func TestGetConflictingChildren_ToleratesDanglingSpendingRef(t *testing.T) {
	ctx := context.Background()
	store, _ := setup(ctx, t)

	parent := danglingParentTx(0x71)
	_, err := store.Create(ctx, parent, 100)
	require.NoError(t, err)

	child := danglingSpendTx(t, parent, 90000)
	_, err = store.Create(ctx, child, 100)
	require.NoError(t, err)

	// Record the spend so parent.SpendingDatas[0] -> child.
	_, err = store.Spend(ctx, child, 101)
	require.NoError(t, err)

	// Delete the child's own record, leaving parent's SpendingDatas dangling.
	require.NoError(t, store.Delete(ctx, child.TxIDChainHash()))

	res, err := utxo.GetConflictingChildren(ctx, store, *parent.TxIDChainHash())
	require.NoError(t, err, "BFS must tolerate a parent that references a deleted spender, not fail with TX_NOT_FOUND")
	require.NotNil(t, res)
}

// TestGetCounterConflictingTxHashes_ToleratesDanglingCounter verifies the counter
// walk does not error when the counter spender itself has been deleted while the
// parent still records it as the spender. Pre-fix: GetConflictingChildren(counter)
// fetches the deleted counter and returns TX_NOT_FOUND.
func TestGetCounterConflictingTxHashes_ToleratesDanglingCounter(t *testing.T) {
	ctx := context.Background()
	store, _ := setup(ctx, t)

	parent := danglingParentTx(0x72)
	_, err := store.Create(ctx, parent, 100)
	require.NoError(t, err)

	// counter is the recorded spender of parent[0].
	counter := danglingSpendTx(t, parent, 80000)
	_, err = store.Create(ctx, counter, 100)
	require.NoError(t, err)

	_, err = store.Spend(ctx, counter, 101)
	require.NoError(t, err)

	// queryTx is the conflicting tx that ALSO spends parent[0]; it is the tx whose
	// counter-conflicting set we walk. Not spent (it is the loser), just stored.
	queryTx := danglingSpendTx(t, parent, 90000)
	_, err = store.Create(ctx, queryTx, 100, utxo.WithConflicting(true))
	require.NoError(t, err)

	// Delete the counter's record → parent.SpendingDatas[0] is now dangling.
	require.NoError(t, store.Delete(ctx, counter.TxIDChainHash()))

	res, err := utxo.GetCounterConflictingTxHashes(ctx, store, *queryTx.TxIDChainHash())
	require.NoError(t, err, "counter walk must tolerate a deleted counter, not fail with TX_NOT_FOUND")
	require.Contains(t, res, *queryTx.TxIDChainHash(), "result must include the queried tx")
	require.NotContains(t, res, *counter.TxIDChainHash(),
		"a ghost counter must be EXCLUDED from the set: callers feed it into SetConflicting/GetMeta, which fail on missing records")
}

// TestGetCounterConflictingTxHashes_ToleratesDanglingChildOfCounter verifies the
// walk survives when the counter exists but ITS spending child has been deleted —
// exercises the BFS one level deeper than the counter itself. Pre-fix: the internal
// GetConflictingChildren(counter) fetches the deleted grandchild → TX_NOT_FOUND.
func TestGetCounterConflictingTxHashes_ToleratesDanglingChildOfCounter(t *testing.T) {
	ctx := context.Background()
	store, _ := setup(ctx, t)

	parent := danglingParentTx(0x73)
	_, err := store.Create(ctx, parent, 100)
	require.NoError(t, err)

	// counter spends parent[0] and survives.
	counter := danglingSpendTx(t, parent, 80000)
	_, err = store.Create(ctx, counter, 100)
	require.NoError(t, err)

	_, err = store.Spend(ctx, counter, 101)
	require.NoError(t, err)

	// grandchild spends counter[0]; recorded then deleted → counter.SpendingDatas
	// dangles.
	grandchild := danglingSpendTx(t, counter, 70000)
	_, err = store.Create(ctx, grandchild, 100)
	require.NoError(t, err)

	_, err = store.Spend(ctx, grandchild, 102)
	require.NoError(t, err)

	require.NoError(t, store.Delete(ctx, grandchild.TxIDChainHash()))

	// queryTx is the conflicting loser also spending parent[0].
	queryTx := danglingSpendTx(t, parent, 90000)
	_, err = store.Create(ctx, queryTx, 100, utxo.WithConflicting(true))
	require.NoError(t, err)

	res, err := utxo.GetCounterConflictingTxHashes(ctx, store, *queryTx.TxIDChainHash())
	require.NoError(t, err, "counter walk must tolerate a deleted grandchild of the counter, not fail with TX_NOT_FOUND")
	require.Contains(t, res, *counter.TxIDChainHash(), "the surviving counter must still be reported")
	require.NotContains(t, res, *grandchild.TxIDChainHash(),
		"a ghost grandchild must be EXCLUDED from the set: callers feed it into SetConflicting/GetMeta, which fail on missing records")
}

// TestProcessConflicting_SelfHealsDanglingLoserSlot is the end-to-end self-heal proof
// for issue #1213: a parent output is still held by a ghost loser (record deleted,
// spend slot intact) while the network mined the rival. ProcessConflicting must
// (a) exclude the ghost from the losing set, (b) clear the ghost's stale slot,
// (c) spend the winner into the slot, and (d) unmark the winner — repairing the store
// on the forward path with no manual intervention.
func TestProcessConflicting_SelfHealsDanglingLoserSlot(t *testing.T) {
	ctx := context.Background()
	store, _ := setup(ctx, t)

	parent := danglingParentTx(0x75)
	_, err := store.Create(ctx, parent, 100)
	require.NoError(t, err)

	// ghost is the recorded spender of parent[0]; its record is then deleted.
	ghost := danglingSpendTx(t, parent, 80000)
	_, err = store.Create(ctx, ghost, 100)
	require.NoError(t, err)

	_, err = store.Spend(ctx, ghost, 101)
	require.NoError(t, err)

	require.NoError(t, store.Delete(ctx, ghost.TxIDChainHash()))

	// winner is the rival the network mined; stored flagged conflicting (the state the
	// validator leaves it in when its spend loses to the recorded spender).
	winner := danglingSpendTx(t, parent, 90000)
	_, err = store.Create(ctx, winner, 100, utxo.WithConflicting(true))
	require.NoError(t, err)

	_, _, err = utxo.ProcessConflicting(ctx, store, 102, chainhash.Hash{}, []chainhash.Hash{*winner.TxIDChainHash()}, map[chainhash.Hash]struct{}{})
	require.NoError(t, err, "promotion must tolerate a ghost loser, not wedge on its missing record")

	// The stale slot must now belong to the winner.
	parentMeta, err := store.Get(ctx, parent.TxIDChainHash(), fields.Utxos)
	require.NoError(t, err)
	require.NotNil(t, parentMeta.SpendingDatas[0], "parent slot must be spent after promotion")
	require.True(t, parentMeta.SpendingDatas[0].TxID.Equal(*winner.TxIDChainHash()),
		"parent slot must be overwritten from the ghost to the winner")

	// And the winner must no longer be conflicting.
	winnerMeta, err := store.Get(ctx, winner.TxIDChainHash(), fields.Conflicting)
	require.NoError(t, err)
	require.False(t, winnerMeta.Conflicting, "promoted winner must be unmarked")

	// And the parent must be spendable again — step 2 locked it, step 5 must have
	// unlocked it. A leak here would leave the repaired parent stuck Locked.
	parentMeta, err = store.Get(ctx, parent.TxIDChainHash(), fields.Locked)
	require.NoError(t, err)
	require.False(t, parentMeta.Locked, "self-heal must leave the parent unlocked")
}

// TestGetCounterConflictingTxHashes_FailsClosedOnMissingParent verifies that a
// missing PARENT record is NOT tolerated the way a missing spender/child record is.
// A missing spender is safe to tolerate because the surviving parent's SpendingDatas
// still identifies the current spender; a missing parent erases the whole spend graph
// for that input, so we can no longer tell whether a counter mined on our chain (and
// later pruned by retention) spends it. The walk must fail closed, not silently drop
// the input — otherwise the counter-conflicting guard becomes fail-open.
func TestGetCounterConflictingTxHashes_FailsClosedOnMissingParent(t *testing.T) {
	ctx := context.Background()
	store, _ := setup(ctx, t)

	parent := danglingParentTx(0x74)
	_, err := store.Create(ctx, parent, 100)
	require.NoError(t, err)

	// queryTx is the conflicting tx spending parent[0].
	queryTx := danglingSpendTx(t, parent, 90000)
	_, err = store.Create(ctx, queryTx, 100, utxo.WithConflicting(true))
	require.NoError(t, err)

	// Delete the PARENT record → the spend graph for queryTx's input is gone.
	require.NoError(t, store.Delete(ctx, parent.TxIDChainHash()))

	_, err = utxo.GetCounterConflictingTxHashes(ctx, store, *queryTx.TxIDChainHash())
	require.Error(t, err, "a missing parent record must fail closed, not be silently tolerated")
	require.True(t, errors.Is(err, errors.ErrTxNotFound) || errors.Is(err, errors.ErrNotFound),
		"the missing-parent failure must propagate the underlying not-found error, not be swallowed")
}

// newDanglingTestPruner builds the SQL pruner service against the test store's own
// database handle so pruner-driven deletions hit the same schema the walk reads.
func newDanglingTestPruner(ctx context.Context, t *testing.T, store *Store) *sqlpruner.Service {
	t.Helper()

	tSettings := test.CreateBaseTestSettings(t)
	tSettings.Pruner.UTXODefensiveEnabled = false // pin the plain-delete branch; the marker logic is branch-independent

	svc, err := sqlpruner.NewService(tSettings, sqlpruner.Options{
		Logger: ulogger.TestLogger{},
		DB:     store.db,
		Ctx:    ctx,
	})
	require.NoError(t, err)

	return svc
}

// TestSQLPruner_DeletedChildLeavesMarker_WalkFailsClosed verifies the SQL mirror of the
// aerospike deletedChildren bin: when the PRUNER (not an ad-hoc Delete) reaps a spender
// whose parent survives, it must leave a deleted_children marker row, the store must
// surface it via fields.DeletedChildren, and the counter walk must fail closed on the
// marked ghost instead of tolerating it. This closes the (retention, retention*2] band
// on postgres/sqlite where a mined-spent counter is pruned while still inside the
// mined-on-chain comparison window.
func TestSQLPruner_DeletedChildLeavesMarker_WalkFailsClosed(t *testing.T) {
	ctx := context.Background()
	store, _ := setup(ctx, t)

	parent := danglingParentTx(0x76)
	_, err := store.Create(ctx, parent, 100)
	require.NoError(t, err)

	// counter is the recorded spender of parent[0]; the pruner will reap it. It is MINED on
	// our chain (mined-then-pruned counter in the retention band), so the F1 gate writes its
	// marker and the walk must fail closed.
	counter := danglingSpendTx(t, parent, 80000)
	_, err = store.Create(ctx, counter, 100)
	require.NoError(t, err)

	_, err = store.Spend(ctx, counter, 101)
	require.NoError(t, err)

	markTxMined(ctx, t, store, counter, 140)

	// queryTx is the conflicting tx that ALSO spends parent[0].
	queryTx := danglingSpendTx(t, parent, 90000)
	_, err = store.Create(ctx, queryTx, 100, utxo.WithConflicting(true))
	require.NoError(t, err)

	// Tombstone the counter and run the pruner.
	_, err = store.db.ExecContext(ctx, `UPDATE transactions SET delete_at_height = $1 WHERE hash = $2`, 150, counter.TxIDChainHash()[:])
	require.NoError(t, err)

	pruned, err := newDanglingTestPruner(ctx, t, store).Prune(ctx, 150, "test")
	require.NoError(t, err)
	require.EqualValues(t, 1, pruned, "the tombstoned counter must be reaped")

	// The counter's record is gone...
	_, err = store.Get(ctx, counter.TxIDChainHash(), fields.Conflicting)
	require.Error(t, err)
	require.True(t, errors.Is(err, errors.ErrTxNotFound))

	// ...but the surviving parent carries the marker.
	parentMeta, err := store.Get(ctx, parent.TxIDChainHash(), fields.DeletedChildren)
	require.NoError(t, err)
	require.Contains(t, parentMeta.DeletedChildren, *counter.TxIDChainHash(),
		"the pruner must leave a deleted_children marker on the surviving parent")

	// The walk must fail closed on the marked ghost, not tolerate it.
	_, err = utxo.GetCounterConflictingTxHashes(ctx, store, *queryTx.TxIDChainHash())
	require.Error(t, err, "a pruner-marked ghost must fail the walk closed")
	require.Contains(t, err.Error(), "deleted by the pruner")
	require.True(t, errors.Is(err, errors.ErrTxInvalid),
		"a marked ghost is a mined-then-pruned counter → INVALID, not a transient ProcessingError")
}

// TestSQLPruner_NeverMinedGhostToleratedByWalk is the F1 regression guard and the
// never-mined counterpart to TestSQLPruner_DeletedChildLeavesMarker_WalkFailsClosed: a
// CONFLICTING/never-mined loser is given a delete_at_height (as SetConflicting stamps a
// loser, with NO mined gate) and reaped by the pruner, but because it was never mined on
// our chain the F1 marker gate leaves it UNMARKED. The counter walk must therefore
// TOLERATE its ghost (exclude it, no error), NOT return TxInvalidError — permanently
// rejecting a block the honest network accepted would be a consensus split.
func TestSQLPruner_NeverMinedGhostToleratedByWalk(t *testing.T) {
	ctx := context.Background()
	store, _ := setup(ctx, t)

	parent := danglingParentTx(0x78)
	_, err := store.Create(ctx, parent, 100)
	require.NoError(t, err)

	// counter is the recorded spender of parent[0]; it stays NEVER-MINED (Create leaves
	// unmined_since set and adds no block_ids row — no markTxMined call).
	counter := danglingSpendTx(t, parent, 80000)
	_, err = store.Create(ctx, counter, 100)
	require.NoError(t, err)

	_, err = store.Spend(ctx, counter, 101)
	require.NoError(t, err)

	// queryTx is the conflicting tx that ALSO spends parent[0] — the tx we walk.
	queryTx := danglingSpendTx(t, parent, 90000)
	_, err = store.Create(ctx, queryTx, 100, utxo.WithConflicting(true))
	require.NoError(t, err)

	// Tombstone the never-mined counter (as SetConflicting does for a loser) and reap it.
	_, err = store.db.ExecContext(ctx, `UPDATE transactions SET delete_at_height = $1 WHERE hash = $2`, 150, counter.TxIDChainHash()[:])
	require.NoError(t, err)

	pruned, err := newDanglingTestPruner(ctx, t, store).Prune(ctx, 150, "test")
	require.NoError(t, err)
	require.EqualValues(t, 1, pruned, "the never-mined counter is still reaped")

	// F1: the reaped never-mined counter leaves NO marker on the surviving parent.
	var markerCount int
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM deleted_children WHERE parent_hash = $1`, parent.TxIDChainHash()[:]).Scan(&markerCount))
	require.Equal(t, 0, markerCount,
		"a never-mined reaped counter must NOT leave a deleted_children marker (marker ⟹ mined-on-our-chain)")

	// ...so the walk TOLERATES the unmarked ghost instead of rejecting the block as INVALID.
	res, err := utxo.GetCounterConflictingTxHashes(ctx, store, *queryTx.TxIDChainHash())
	require.NoError(t, err,
		"an unmarked never-mined ghost must be tolerated, not fail the walk with TxInvalidError")
	require.NotContains(t, res, *counter.TxIDChainHash(), "the ghost counter is excluded from the set")
}

// TestSQLPruner_MarkerRemovedWhenParentPruned verifies marker growth stays bounded:
// once the parent itself is reaped, its deleted_children rows go with it (a missing
// parent already fails the walk closed, so the markers carry no information).
func TestSQLPruner_MarkerRemovedWhenParentPruned(t *testing.T) {
	ctx := context.Background()
	store, _ := setup(ctx, t)

	parent := danglingParentTx(0x77)
	_, err := store.Create(ctx, parent, 100)
	require.NoError(t, err)

	child := danglingSpendTx(t, parent, 80000)
	_, err = store.Create(ctx, child, 100)
	require.NoError(t, err)

	_, err = store.Spend(ctx, child, 101)
	require.NoError(t, err)

	// Mined-then-pruned child: the F1 gate writes its marker when reaped.
	markTxMined(ctx, t, store, child, 140)

	prunerSvc := newDanglingTestPruner(ctx, t, store)

	// Pass 1: reap the child → marker row on the parent.
	_, err = store.db.ExecContext(ctx, `UPDATE transactions SET delete_at_height = $1 WHERE hash = $2`, 150, child.TxIDChainHash()[:])
	require.NoError(t, err)

	pruned, err := prunerSvc.Prune(ctx, 150, "test")
	require.NoError(t, err)
	require.EqualValues(t, 1, pruned)

	var markerCount int
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM deleted_children WHERE parent_hash = $1`, parent.TxIDChainHash()[:]).Scan(&markerCount))
	require.Equal(t, 1, markerCount, "pass 1 must have left a marker on the surviving parent")

	// Pass 2: reap the parent → its marker rows must be removed in the same pass.
	_, err = store.db.ExecContext(ctx, `UPDATE transactions SET delete_at_height = $1 WHERE hash = $2`, 200, parent.TxIDChainHash()[:])
	require.NoError(t, err)

	pruned, err = prunerSvc.Prune(ctx, 200, "test")
	require.NoError(t, err)
	require.EqualValues(t, 1, pruned)

	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM deleted_children WHERE parent_hash = $1`, parent.TxIDChainHash()[:]).Scan(&markerCount))
	require.Equal(t, 0, markerCount, "reaping the parent must remove its deleted_children rows")
}

// TestSetConflicting_SkipsFrozenSentinel verifies SQL SetConflicting handles the frozen
// sentinel (subtree.CoinbasePlaceholderHashValue, the all-0xFF marker) symmetrically with
// aerospike: it is skipped, not treated as a real tx. The counter-conflicting walk can now
// keep the sentinel in the losing set, so SetConflicting must tolerate it in the batch
// without erroring, mark the accompanying real tx, and never create a row for the sentinel.
// Pre-fix this call fails: s.Get(sentinel) returns TX_NOT_FOUND and aborts the whole batch.
func TestSetConflicting_SkipsFrozenSentinel(t *testing.T) {
	ctx := context.Background()
	store, _ := setup(ctx, t)

	parent := danglingParentTx(0x7F)
	_, err := store.Create(ctx, parent, 100)
	require.NoError(t, err)

	// A real, storable tx that WILL be marked conflicting.
	realTx := danglingSpendTx(t, parent, 90000)
	_, err = store.Create(ctx, realTx, 100)
	require.NoError(t, err)

	sentinel := subtree.CoinbasePlaceholderHashValue

	// SetConflicting over [sentinel, realTx]: the sentinel is skipped, not errored.
	_, _, err = store.SetConflicting(ctx, []chainhash.Hash{sentinel, *realTx.TxIDChainHash()}, true)
	require.NoError(t, err, "the frozen sentinel must be skipped, not abort the whole SetConflicting batch")

	// The real tx is now flagged conflicting.
	realMeta, err := store.Get(ctx, realTx.TxIDChainHash(), fields.Conflicting)
	require.NoError(t, err)
	require.True(t, realMeta.Conflicting, "the real tx in the batch must still be marked conflicting")

	// The sentinel was never turned into a row.
	_, err = store.Get(ctx, &sentinel, fields.Conflicting)
	require.Error(t, err, "no row must be created for the sentinel — it is a marker, not a tx")
	require.True(t, errors.Is(err, errors.ErrTxNotFound))
}
