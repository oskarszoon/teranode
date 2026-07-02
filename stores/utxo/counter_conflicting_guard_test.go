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

// Regression guard (P0, freeze bypass): a frozen output records subtree.FrozenBytesTxHash
// directly in the parent's spend slot. The sentinel has no store record, so the spender
// existence Get would return NOT_FOUND and the tolerance fix would misclassify it as a
// benign ghost. The walk must reject it with an explicit frozen-sentinel error BEFORE that
// Get, and the error must NOT be a TX_NOT_FOUND the tolerance path would swallow.
func TestGetCounterConflictingTxHashes_FrozenSentinelSpenderRejected(t *testing.T) {
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

	require.Nil(t, res)
	require.Error(t, err)
	require.Contains(t, err.Error(), "spent by frozen sentinel")
	require.False(t, errors.Is(err, errors.ErrTxNotFound),
		"frozen-sentinel rejection must be a distinct error, not a TX_NOT_FOUND the tolerance fix would swallow")
	// The sentinel must be rejected before any existence Get on it.
	mockStore.AssertNotCalled(t, "Get", mock.Anything, &frozenHash, mock.Anything)
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

// Regression guard (P0, freeze bypass at the ProcessConflicting repair path): a winner
// input whose parent slot holds the frozen sentinel must abort ProcessConflicting before
// step 2, and must NEVER queue an Unspend for that slot — clearing it would unfreeze the
// output on aerospike.
func TestProcessConflicting_FrozenSentinelParentSlot_NoUnspend(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	winnerHash := createTestHash("frozen-slot-winner")
	winnerHashes := []chainhash.Hash{winnerHash}
	parentTxHash := createTestHash("frozen-slot-winner-parent")

	winnerTx := createTestTransactionWithInputs(parentTxHash, 0)

	mockStore.On("Get", mock.Anything, &winnerHash, mock.Anything).
		Return(&meta.Data{Tx: winnerTx, Conflicting: true}, nil)

	// No losers — keep the cascade empty so the run reaches the dangling-slot check cleanly.
	mockStore.On("GetCounterConflicting", mock.Anything, winnerHash).
		Return([]chainhash.Hash{}, nil)

	// The winner's parent output is spent by the frozen sentinel.
	frozenHash := subtree.FrozenBytesTxHash
	mockStore.On("Get", mock.Anything, &parentTxHash, mock.Anything).
		Return(&meta.Data{SpendingDatas: []*spend.SpendingData{{TxID: &frozenHash}}}, nil)

	result, _, err := ProcessConflicting(ctx, mockStore, 1, chainhash.Hash{}, winnerHashes, map[chainhash.Hash]struct{}{})

	require.Nil(t, result)
	require.Error(t, err)
	require.Contains(t, err.Error(), "frozen sentinel")
	// The frozen slot must never be unspent — that would unfreeze the output.
	mockStore.AssertNotCalled(t, "Unspend", mock.Anything, mock.Anything, mock.Anything)
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

	// The winner's parent records the ghost as spender AND carries a deletedChildren marker.
	mockStore.On("Get", mock.Anything, &parentTxHash, mock.Anything).
		Return(&meta.Data{
			SpendingDatas:   []*spend.SpendingData{{TxID: &ghostTxHash}},
			DeletedChildren: map[chainhash.Hash]struct{}{ghostTxHash: {}},
		}, nil)

	// The ghost's own record is gone.
	mockStore.On("Get", mock.Anything, &ghostTxHash, mock.Anything).
		Return(nil, errors.NewTxNotFoundError("not found"))

	result, _, err := ProcessConflicting(ctx, mockStore, 1, chainhash.Hash{}, winnerHashes, map[chainhash.Hash]struct{}{})

	require.Nil(t, result)
	require.Error(t, err)
	require.Contains(t, err.Error(), "deleted by the pruner")
	// The marked slot must never be unspent — the spender was reaped deliberately.
	mockStore.AssertNotCalled(t, "Unspend", mock.Anything, mock.Anything, mock.Anything)
	mockStore.AssertExpectations(t)
}
