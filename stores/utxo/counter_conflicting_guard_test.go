package utxo

// Regression guard: the counter walk must keep rejecting a frozen sentinel child.
//
// The dangling-reference fix tolerates TX_NOT_FOUND from a deleted spender, but it
// must NOT weaken the frozen-child rejection: when GetConflictingChildren reports a
// child equal to the frozen sentinel (subtree.FrozenBytesTxHash), the counter walk
// must still fail with an explicit "tx has frozen child" error — a distinct
// rejection, not a TX_NOT_FOUND that the fix would swallow.
//
// Mock-based on purpose: a real store BFS over a frozen output would itself surface
// TX_NOT_FOUND on current code, so mocking GetConflictingChildren isolates the
// sentinel-rejection branch we are guarding.

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/stores/utxo/spend"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestGetCounterConflictingTxHashes_FrozenSentinelStillRejected(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	txHash := createTestHash("frozen-guard-tx")
	parentTxHash := createTestHash("frozen-guard-parent")
	counterTxHash := createTestHash("frozen-guard-counter")

	testTx := createTestTransactionWithInputs(parentTxHash, 0)

	mockStore.On("Get", mock.Anything, &txHash, mock.Anything).
		Return(&meta.Data{Tx: testTx}, nil)

	spendingData := &spend.SpendingData{TxID: &counterTxHash}
	mockStore.On("Get", mock.Anything, &parentTxHash, mock.Anything).
		Return(&meta.Data{SpendingDatas: []*spend.SpendingData{spendingData}}, nil)

	// The counter exists (the walk existence-checks a recorded spender before
	// descending into its children).
	mockStore.On("Get", mock.Anything, &counterTxHash, mock.Anything).
		Return(&meta.Data{}, nil)

	// The counter's conflicting children include the frozen sentinel.
	mockStore.On("GetConflictingChildren", mock.Anything, counterTxHash).
		Return([]chainhash.Hash{subtree.FrozenBytesTxHash}, nil)

	res, err := GetCounterConflictingTxHashes(ctx, mockStore, txHash)

	require.Nil(t, res)
	require.Error(t, err)
	require.Contains(t, err.Error(), "tx has frozen child")
	require.False(t, errors.Is(err, errors.ErrTxNotFound),
		"frozen-child rejection must be a distinct error, not a TX_NOT_FOUND the tolerance fix would swallow")
	mockStore.AssertExpectations(t)
}

// Regression guard (freeze handling, walk): a frozen output records
// subtree.FrozenBytesTxHash (== subtree.CoinbasePlaceholderHashValue — both are the
// all-0xFF hash, so ONE comparison covers both spellings) directly in the parent's
// spend slot. The sentinel has no store record, so the spender existence Get would
// return NOT_FOUND (or (nil, nil) on aerospike) and the tolerance fix would
// misclassify it as a benign ghost. Pre-tolerance parity instead: the sentinel is
// INCLUDED in the counter set without any existence Get or child BFS on it, and the
// walk succeeds — downstream consumers stay fail-safe on the placeholder
// (checkCounterConflictingOnCurrentChain rejects it as frozen; SetConflicting skips it).
func TestGetCounterConflictingTxHashes_FrozenSentinelSpenderIncludedWithoutGet(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	txHash := createTestHash("frozen-slot-tx")
	parentTxHash := createTestHash("frozen-slot-parent")

	testTx := createTestTransactionWithInputs(parentTxHash, 0)

	mockStore.On("Get", mock.Anything, &txHash, mock.Anything).
		Return(&meta.Data{Tx: testTx}, nil)

	// The parent output is spent by the frozen sentinel (no store record).
	frozenHash := subtree.FrozenBytesTxHash
	spendingData := &spend.SpendingData{TxID: &frozenHash}
	mockStore.On("Get", mock.Anything, &parentTxHash, mock.Anything).
		Return(&meta.Data{SpendingDatas: []*spend.SpendingData{spendingData}}, nil)

	res, err := GetCounterConflictingTxHashes(ctx, mockStore, txHash)

	require.NoError(t, err)
	require.Contains(t, res, txHash)
	require.Contains(t, res, frozenHash,
		"the sentinel must stay in the counter set so downstream placeholder guards fire")
	require.Len(t, res, 2)
	// The sentinel must never be existence-checked or descended into.
	mockStore.AssertNotCalled(t, "Get", mock.Anything, &frozenHash, mock.Anything)
	mockStore.AssertNotCalled(t, "GetConflictingChildren", mock.Anything, frozenHash)
	mockStore.AssertExpectations(t)
}

// Regression guard (deletedChildren discriminator): a ghost spender whose own record is
// gone but which the pruner recorded in the parent's deletedChildren set was deleted
// deliberately (e.g. a mined tx reaped after retention). The walk must fail closed rather
// than tolerate it as a benign dangling ref.
func TestGetCounterConflictingTxHashes_DeletedChildrenMarkerFailsClosed(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	txHash := createTestHash("marker-tx")
	parentTxHash := createTestHash("marker-parent")
	ghostTxHash := createTestHash("marker-ghost-spender")

	testTx := createTestTransactionWithInputs(parentTxHash, 0)

	mockStore.On("Get", mock.Anything, &txHash, mock.Anything).
		Return(&meta.Data{Tx: testTx}, nil)

	// Parent records the ghost as spender AND carries a deletedChildren marker for it.
	spendingData := &spend.SpendingData{TxID: &ghostTxHash}
	mockStore.On("Get", mock.Anything, &parentTxHash, mock.Anything).
		Return(&meta.Data{
			SpendingDatas:   []*spend.SpendingData{spendingData},
			DeletedChildren: map[chainhash.Hash]struct{}{ghostTxHash: {}},
		}, nil)

	// The ghost's own record is gone.
	mockStore.On("Get", mock.Anything, &ghostTxHash, mock.Anything).
		Return(nil, errors.NewTxNotFoundError("not found"))

	res, err := GetCounterConflictingTxHashes(ctx, mockStore, txHash)

	require.Nil(t, res)
	require.Error(t, err)
	require.Contains(t, err.Error(), "deleted by the pruner")
	require.True(t, errors.Is(err, errors.ErrTxInvalid),
		"a marked ghost is a mined-then-pruned counter → INVALID, not a transient ProcessingError")
	mockStore.AssertExpectations(t)
}

// Counterpart to the marker test: a ghost spender with NO deletedChildren marker is still
// tolerated — excluded from the counter set — so the walk succeeds and returns only the
// queried tx. This is the behaviour the discriminator must preserve for the unmarked case.
func TestGetCounterConflictingTxHashes_GhostWithoutMarkerTolerated(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	txHash := createTestHash("nomarker-tx")
	parentTxHash := createTestHash("nomarker-parent")
	ghostTxHash := createTestHash("nomarker-ghost-spender")

	testTx := createTestTransactionWithInputs(parentTxHash, 0)

	mockStore.On("Get", mock.Anything, &txHash, mock.Anything).
		Return(&meta.Data{Tx: testTx}, nil)

	// Parent records the ghost as spender but has NO deletedChildren marker for it.
	spendingData := &spend.SpendingData{TxID: &ghostTxHash}
	mockStore.On("Get", mock.Anything, &parentTxHash, mock.Anything).
		Return(&meta.Data{SpendingDatas: []*spend.SpendingData{spendingData}}, nil)

	// The ghost's own record is gone.
	mockStore.On("Get", mock.Anything, &ghostTxHash, mock.Anything).
		Return(nil, errors.NewTxNotFoundError("not found"))

	res, err := GetCounterConflictingTxHashes(ctx, mockStore, txHash)

	require.NoError(t, err)
	require.Contains(t, res, txHash)
	require.NotContains(t, res, ghostTxHash)
	require.Len(t, res, 1)
	mockStore.AssertExpectations(t)
}

// Walk (nil, nil) missing-spender branch, marked: some backends surface a deleted record
// as (nil, nil) instead of a NOT_FOUND error. When the parent carries a deletedChildren
// marker for that ghost the walk must fail closed, identically to the NOT_FOUND path —
// the shared discriminateGhostSpender helper must treat both variants the same.
func TestGetCounterConflictingTxHashes_NilSpenderWithMarkerFailsClosed(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	txHash := createTestHash("nilspender-marker-tx")
	parentTxHash := createTestHash("nilspender-marker-parent")
	ghostTxHash := createTestHash("nilspender-marker-ghost")

	testTx := createTestTransactionWithInputs(parentTxHash, 0)

	mockStore.On("Get", mock.Anything, &txHash, mock.Anything).
		Return(&meta.Data{Tx: testTx}, nil)

	// Parent records the ghost as spender AND carries a deletedChildren marker for it.
	spendingData := &spend.SpendingData{TxID: &ghostTxHash}
	mockStore.On("Get", mock.Anything, &parentTxHash, mock.Anything).
		Return(&meta.Data{
			SpendingDatas:   []*spend.SpendingData{spendingData},
			DeletedChildren: map[chainhash.Hash]struct{}{ghostTxHash: {}},
		}, nil)

	// The ghost's own record surfaces as (nil, nil): no error, no data.
	mockStore.On("Get", mock.Anything, &ghostTxHash, mock.Anything).
		Return(nil, nil)

	res, err := GetCounterConflictingTxHashes(ctx, mockStore, txHash)

	require.Nil(t, res)
	require.Error(t, err)
	require.Contains(t, err.Error(), "deleted by the pruner")
	require.True(t, errors.Is(err, errors.ErrTxInvalid),
		"a marked ghost is a mined-then-pruned counter → INVALID, not a transient ProcessingError")
	mockStore.AssertExpectations(t)
}

// Walk (nil, nil) missing-spender branch, unmarked: the (nil, nil) variant of a ghost with
// no deletedChildren marker must be tolerated — excluded from the counter set — exactly
// like the NOT_FOUND variant.
func TestGetCounterConflictingTxHashes_NilSpenderWithoutMarkerTolerated(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	txHash := createTestHash("nilspender-nomarker-tx")
	parentTxHash := createTestHash("nilspender-nomarker-parent")
	ghostTxHash := createTestHash("nilspender-nomarker-ghost")

	testTx := createTestTransactionWithInputs(parentTxHash, 0)

	mockStore.On("Get", mock.Anything, &txHash, mock.Anything).
		Return(&meta.Data{Tx: testTx}, nil)

	// Parent records the ghost as spender but has NO deletedChildren marker for it.
	spendingData := &spend.SpendingData{TxID: &ghostTxHash}
	mockStore.On("Get", mock.Anything, &parentTxHash, mock.Anything).
		Return(&meta.Data{SpendingDatas: []*spend.SpendingData{spendingData}}, nil)

	// The ghost's own record surfaces as (nil, nil).
	mockStore.On("Get", mock.Anything, &ghostTxHash, mock.Anything).
		Return(nil, nil)

	res, err := GetCounterConflictingTxHashes(ctx, mockStore, txHash)

	require.NoError(t, err)
	require.Contains(t, res, txHash)
	require.NotContains(t, res, ghostTxHash)
	require.Len(t, res, 1)
	mockStore.AssertExpectations(t)
}

// Walk GetConflictingChildren NOT_FOUND fallback: a counter that exists at the existence
// check but whose record is deleted before its child BFS runs surfaces NOT_FOUND from
// GetConflictingChildren. The walk must evict the counter from the set (it was already
// added) rather than propagate — callers feed the set into SetConflicting/GetMeta, which
// fail on a missing record.
func TestGetCounterConflictingTxHashes_GetConflictingChildrenNotFoundEvicts(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	txHash := createTestHash("gcc-notfound-tx")
	parentTxHash := createTestHash("gcc-notfound-parent")
	counterTxHash := createTestHash("gcc-notfound-counter")

	testTx := createTestTransactionWithInputs(parentTxHash, 0)

	mockStore.On("Get", mock.Anything, &txHash, mock.Anything).
		Return(&meta.Data{Tx: testTx}, nil)

	spendingData := &spend.SpendingData{TxID: &counterTxHash}
	mockStore.On("Get", mock.Anything, &parentTxHash, mock.Anything).
		Return(&meta.Data{SpendingDatas: []*spend.SpendingData{spendingData}}, nil)

	// The counter exists at the existence check...
	mockStore.On("Get", mock.Anything, &counterTxHash, mock.Anything).
		Return(&meta.Data{}, nil)

	// ...but its record is gone by the time we walk its conflicting children.
	mockStore.On("GetConflictingChildren", mock.Anything, counterTxHash).
		Return([]chainhash.Hash{}, errors.NewTxNotFoundError("counter deleted mid-walk"))

	res, err := GetCounterConflictingTxHashes(ctx, mockStore, txHash)

	require.NoError(t, err, "a counter whose child walk returns NOT_FOUND must be evicted, not propagate")
	require.Contains(t, res, txHash)
	require.NotContains(t, res, counterTxHash,
		"the evicted counter must not remain in the set — callers feed it into SetConflicting/GetMeta")
	require.Len(t, res, 1)
	mockStore.AssertExpectations(t)
}

// Regression guard (frozen sentinel at the ProcessConflicting pre-flight): a winner
// input whose parent slot holds the frozen sentinel (subtree.FrozenBytesTxHash, ==
// subtree.CoinbasePlaceholderHashValue — one comparison covers both spellings) makes step
// 3 deterministically doomed (the aerospike lua refuses frozen slots; SQL frozen state
// lives in a column Unspend never touches). collectDanglingWinnerInputSpends must fail
// closed BEFORE step 2 ever runs: no Unspend queued (clearing the slot would unfreeze the
// output; and a doomed resolution reaching step 2 could strand another input's cleared
// ghost with no tx body for rollback to re-Spend). The error must not be a not-found the
// tolerance path would swallow, and the sentinel must never be existence-checked.
func TestProcessConflicting_FrozenSentinelParentSlot_FailsClosedBeforeUnspend(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	winnerHash := createTestHash("frozen-slot-winner")
	winnerHashes := []chainhash.Hash{winnerHash}
	parentTxHash := createTestHash("frozen-slot-winner-parent")

	winnerTx := createTestTransactionWithInputs(parentTxHash, 0)

	mockStore.On("Get", mock.Anything, &winnerHash, mock.Anything).
		Return(&meta.Data{Tx: winnerTx, Conflicting: true}, nil)

	// No losers — keep the cascade empty so the run reaches the pre-flight check cleanly.
	mockStore.On("GetCounterConflicting", mock.Anything, winnerHash).
		Return([]chainhash.Hash{}, nil)

	// The winner's parent output slot is held by the frozen sentinel: GetSpend reports it
	// as the recorded spender (both backends set it explicitly for a frozen slot).
	frozenHash := subtree.FrozenBytesTxHash
	mockStore.On("GetSpend", mock.Anything, mock.MatchedBy(func(sp *Spend) bool {
		return sp.TxID != nil && sp.TxID.Equal(parentTxHash)
	})).Return(&SpendResponse{SpendingData: &spend.SpendingData{TxID: &frozenHash}}, nil)

	result, _, err := ProcessConflicting(ctx, mockStore, 1, chainhash.Hash{}, winnerHashes, map[chainhash.Hash]struct{}{})

	require.Nil(t, result)
	require.Error(t, err, "a frozen winner-input slot must fail the resolution closed")
	require.Contains(t, err.Error(), "is frozen")
	require.False(t, errors.Is(err, errors.ErrTxNotFound),
		"the frozen pre-flight rejection must be a distinct error, not a not-found the tolerance path swallows")
	// Fail-closed means no mutation past step 1: Unspend must never run.
	mockStore.AssertNotCalled(t, "Unspend", mock.Anything, mock.Anything, mock.Anything)
	// The sentinel must never be existence-checked as a spender.
	mockStore.AssertNotCalled(t, "Get", mock.Anything, &frozenHash, mock.Anything)
	mockStore.AssertExpectations(t)
}

// Regression guard (deletedChildren discriminator at the ProcessConflicting repair path):
// a winner input whose parent slot records a ghost spender the pruner marked in
// deletedChildren must abort ProcessConflicting before any Unspend — the slot must not be
// cleared, because the spender was reaped deliberately.
func TestProcessConflicting_DeletedChildrenMarker_FailsClosedNoUnspend(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	winnerHash := createTestHash("marker-repair-winner")
	winnerHashes := []chainhash.Hash{winnerHash}
	parentTxHash := createTestHash("marker-repair-parent")
	ghostTxHash := createTestHash("marker-repair-ghost")

	winnerTx := createTestTransactionWithInputs(parentTxHash, 0)

	mockStore.On("Get", mock.Anything, &winnerHash, mock.Anything).
		Return(&meta.Data{Tx: winnerTx, Conflicting: true}, nil)

	mockStore.On("GetCounterConflicting", mock.Anything, winnerHash).
		Return([]chainhash.Hash{}, nil)

	// The winner's parent slot records the ghost as spender (targeted GetSpend read).
	mockStore.On("GetSpend", mock.Anything, mock.MatchedBy(func(sp *Spend) bool {
		return sp.TxID != nil && sp.TxID.Equal(parentTxHash)
	})).Return(&SpendResponse{SpendingData: &spend.SpendingData{TxID: &ghostTxHash}}, nil)

	// The ghost's own record is gone.
	mockStore.On("Get", mock.Anything, &ghostTxHash, mock.Anything).
		Return(nil, errors.NewTxNotFoundError("not found"))

	// On the ghost path the pre-flight fetches the parent's deletedChildren marker (a
	// single-bin Get). The pruner recorded the ghost there, so the deletion was deliberate.
	mockStore.On("Get", mock.Anything, &parentTxHash, mock.Anything).
		Return(&meta.Data{DeletedChildren: map[chainhash.Hash]struct{}{ghostTxHash: {}}}, nil)

	result, _, err := ProcessConflicting(ctx, mockStore, 1, chainhash.Hash{}, winnerHashes, map[chainhash.Hash]struct{}{})

	require.Nil(t, result)
	require.Error(t, err)
	require.Contains(t, err.Error(), "deleted by the pruner")
	require.True(t, errors.Is(err, errors.ErrTxInvalid),
		"a marked ghost is a mined-then-pruned counter → INVALID, not a transient ProcessingError")
	// The marked slot must never be unspent — the spender was reaped deliberately.
	mockStore.AssertNotCalled(t, "Unspend", mock.Anything, mock.Anything, mock.Anything)
	mockStore.AssertExpectations(t)
}

// Regression guard (live third-party spender at the ProcessConflicting pre-flight): a
// winner input whose parent slot records a LIVE spender outside the marked-conflicting
// set makes step 3 a real double-spend against a live tx. collectDanglingWinnerInputSpends
// must fail closed BEFORE step 2 — never clear the slot (which would steal the live tx's
// spend) and never let step 2 commit (a doomed resolution could strand another input's
// cleared ghost). The error must name the live spender. Pins the
// `err == nil && spenderMeta != nil → fail closed` branch in collectDanglingWinnerInputSpends.
func TestProcessConflicting_LiveThirdPartySpenderSlot_FailsClosedBeforeUnspend(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	winnerHash := createTestHash("live-spender-winner")
	winnerHashes := []chainhash.Hash{winnerHash}
	parentTxHash := createTestHash("live-spender-parent")
	liveHash := createTestHash("live-third-party-spender")

	winnerTx := createTestTransactionWithInputs(parentTxHash, 0)

	mockStore.On("Get", mock.Anything, &winnerHash, mock.Anything).
		Return(&meta.Data{Tx: winnerTx, Conflicting: true}, nil)

	// No losers — the live spender is NOT in the counter set (e.g. a reorg edge case),
	// so it also ends up outside the marked set.
	mockStore.On("GetCounterConflicting", mock.Anything, winnerHash).
		Return([]chainhash.Hash{}, nil)

	// The winner's parent output slot is held by the live third party (targeted GetSpend).
	mockStore.On("GetSpend", mock.Anything, mock.MatchedBy(func(sp *Spend) bool {
		return sp.TxID != nil && sp.TxID.Equal(parentTxHash)
	})).Return(&SpendResponse{SpendingData: &spend.SpendingData{TxID: &liveHash}}, nil)

	// The spender's record EXISTS — the pre-flight existence check sees it live.
	mockStore.On("Get", mock.Anything, &liveHash, mock.Anything).
		Return(&meta.Data{}, nil)

	result, _, err := ProcessConflicting(ctx, mockStore, 1, chainhash.Hash{}, winnerHashes, map[chainhash.Hash]struct{}{})

	require.Nil(t, result)
	require.Error(t, err, "a live third-party spender must fail the resolution closed before any mutation")
	require.Contains(t, err.Error(), "spent by live non-conflicting tx")
	require.Contains(t, err.Error(), liveHash.String(), "the error must name the live spender")
	// Fail-closed before step 2: neither Unspend nor Spend may run.
	mockStore.AssertNotCalled(t, "Unspend", mock.Anything, mock.Anything, mock.Anything)
	mockStore.AssertNotCalled(t, "Spend", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	// The existence check on the live spender must have run.
	mockStore.AssertCalled(t, "Get", mock.Anything, &liveHash, mock.Anything)
	mockStore.AssertExpectations(t)
}

// Regression guard (frozen sentinel filtered before SetConflicting): the frozen sentinel
// (subtree.FrozenBytesTxHash == subtree.CoinbasePlaceholderHashValue, the all-0xFF marker)
// can reach MarkConflictingRecursively both in the input hashes (the counter walk keeps it
// in the counter set) and as a returned spending child (a frozen parent slot surfaces it).
// It is a marker, not a transaction: it must never be handed to SetConflicting, which would
// nil-deref on aerospike and NOT_FOUND-error on SQL. The precise per-batch matchers below
// (each rejecting any batch that contains the sentinel) prove the filter runs on BOTH the
// initial batch and the descendant batch.
func TestMarkConflictingRecursively_FiltersFrozenSentinel(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	realTx := createTestHash("mcr-real-tx")
	childTx := createTestHash("mcr-child-tx")
	sentinel := subtree.FrozenBytesTxHash

	// First batch: the input [realTx, sentinel] with the sentinel filtered → exactly
	// [realTx]. The returned spending-child set deliberately includes the sentinel, as a
	// frozen slot would surface it.
	firstBatch := mock.MatchedBy(func(hashes []chainhash.Hash) bool {
		return len(hashes) == 1 && hashes[0].Equal(realTx)
	})
	mockStore.On("SetConflicting", mock.Anything, firstBatch, true).
		Return([]*Spend{}, []chainhash.Hash{childTx, sentinel}, nil).Once()

	// Second batch: the descendant set [childTx, sentinel] with the sentinel filtered →
	// exactly [childTx]. No more children.
	secondBatch := mock.MatchedBy(func(hashes []chainhash.Hash) bool {
		return len(hashes) == 1 && hashes[0].Equal(childTx)
	})
	mockStore.On("SetConflicting", mock.Anything, secondBatch, true).
		Return([]*Spend{}, []chainhash.Hash{}, nil).Once()

	_, marked, err := MarkConflictingRecursively(ctx, mockStore, []chainhash.Hash{realTx, sentinel})

	require.NoError(t, err)
	require.Contains(t, marked, realTx)
	require.Contains(t, marked, childTx)
	require.NotContains(t, marked, sentinel,
		"the sentinel is a marker, never itself marked conflicting")
	// A SetConflicting call carrying the sentinel would fail the per-batch matchers and
	// surface as an unexpected-call panic; AssertExpectations confirms only the two
	// sentinel-free batches ran.
	mockStore.AssertExpectations(t)
}

// Rollback transient-path: the pre-flight queues a genuine dangling ghost spend, step 2
// commits it (clearing the stale slot), then step 3 fails with a TRANSIENT storage error
// (not a double-spend). The deferred rollback must run — re-spend the cascade, then unlock
// the parents — and when a rollback sub-step (here SetLocked) also fails, the returned
// error must aggregate the original failure with the rollback failure under the MANUAL
// INTERVENTION REQUIRED tag. Exercises the collect→step2→step3-fail→rollback interaction
// that the new pre-flight introduces.
func TestProcessConflicting_RollbackAfterDanglingUnspendThenTransientStep3(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	winnerHash := createTestHash("transient-winner")
	winnerHashes := []chainhash.Hash{winnerHash}
	parentTxHash := createTestHash("transient-winner-parent")
	ghostHash := createTestHash("transient-ghost-spender")
	loserHash := createTestHash("transient-loser")
	loserParentHash := createTestHash("transient-loser-parent")

	// Extended input so the pre-flight's UTXOHashFromInput succeeds and the dangling spend
	// is actually queued.
	winnerTx := extendedInputTx(parentTxHash, 0)
	loserTx := createTestTransaction()

	mockStore.On("Get", mock.Anything, &winnerHash, mock.Anything).
		Return(&meta.Data{Tx: winnerTx, Conflicting: true}, nil)

	mockStore.On("GetCounterConflicting", mock.Anything, winnerHash).
		Return([]chainhash.Hash{loserHash}, nil)

	// step 1: mark the loser conflicting; its parent slot is the affected spend.
	loserSpends := []*Spend{{TxID: &loserParentHash, Vout: 0}}
	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{loserHash}, true).
		Return(loserSpends, []chainhash.Hash{}, nil)

	// pre-flight: the winner's parent slot is held by a ghost whose record is gone and
	// which carries NO deletedChildren marker → a benign dangling ref to be cleared.
	mockStore.On("GetSpend", mock.Anything, mock.MatchedBy(func(sp *Spend) bool {
		return sp.TxID != nil && sp.TxID.Equal(parentTxHash)
	})).Return(&SpendResponse{SpendingData: &spend.SpendingData{TxID: &ghostHash}}, nil)
	mockStore.On("Get", mock.Anything, &ghostHash, mock.Anything).
		Return(nil, errors.NewTxNotFoundError("ghost gone"))
	mockStore.On("Get", mock.Anything, &parentTxHash, mock.Anything).
		Return(&meta.Data{}, nil)

	// step 2: Unspend the combined set — the loser slot AND the queued dangling ghost slot.
	unspendSawDangling := false
	mockStore.On("Unspend", mock.Anything, mock.MatchedBy(func(spends []*Spend) bool {
		for _, sp := range spends {
			if sp != nil && sp.TxID != nil && sp.TxID.Equal(parentTxHash) {
				unspendSawDangling = true
			}
		}
		return len(spends) == 2
	}), mock.Anything).Return(nil)

	// step 3: winner spend fails transiently (a storage error, not a double-spend).
	mockStore.On("Spend", mock.Anything, winnerTx, mock.Anything, mock.Anything).
		Return([]*Spend{}, errors.NewStorageError("transient backend write failure"))

	// rollback: re-fetch + re-spend the loser (undo step 2 for the cascade).
	mockStore.On("Get", mock.Anything, &loserHash, mock.Anything).
		Return(&meta.Data{Tx: loserTx}, nil)
	mockStore.On("Spend", mock.Anything, loserTx, mock.Anything, mock.Anything).
		Return([]*Spend{}, nil)
	// rollback: clear the conflicting flag on the cascade (succeeds).
	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{loserHash}, false).
		Return([]*Spend{}, []chainhash.Hash{}, nil)
	// rollback: unlock the parents — this sub-step FAILS, forcing the aggregated error.
	mockStore.On("SetLocked", mock.Anything, mock.MatchedBy(func(hs []chainhash.Hash) bool {
		if len(hs) != 2 {
			return false
		}
		seen := map[chainhash.Hash]bool{hs[0]: true, hs[1]: true}
		return seen[loserParentHash] && seen[parentTxHash]
	}), false).Return(errors.NewProcessingError("transient unlock failure"))

	result, _, err := ProcessConflicting(ctx, mockStore, 1, chainhash.Hash{}, winnerHashes, map[chainhash.Hash]struct{}{})

	require.Nil(t, result)
	require.Error(t, err)
	require.True(t, unspendSawDangling, "step 2 must have unspent the queued dangling ghost slot")
	require.Contains(t, err.Error(), "MANUAL INTERVENTION REQUIRED")
	require.Contains(t, err.Error(), "transient backend write failure", "the original step-3 failure must surface")
	require.Contains(t, err.Error(), "SetLocked false", "the rollback sub-step failure must be aggregated in")
	mockStore.AssertExpectations(t)
}
