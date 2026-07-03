package utxo

// Coverage for the counter-conflicting walk's BFS tolerance branches and the fail-closed
// missing-parent guard that the ProcessConflicting-level guard tests don't reach directly.

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

// GetConflictingChildren BFS: a conflicting child whose own record is gone (NOT_FOUND) must
// be evicted from the visited set rather than propagate, while the frozen sentinel surfaced
// as a spending child stays IN the result but is never descended into.
func TestGetConflictingChildren_EvictsGhostChildAndKeepsFrozenSpendingChild(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	rootHash := createTestHash("gcc-bfs-root")
	ghostChild := createTestHash("gcc-bfs-ghost")
	frozenHash := subtree.FrozenBytesTxHash

	// The root has a conflicting child (ghost, to be evicted) and a spending child that is
	// the frozen sentinel (kept in the result, never descended).
	mockStore.On("Get", mock.Anything, &rootHash, mock.Anything).
		Return(&meta.Data{
			ConflictingChildren: []chainhash.Hash{ghostChild},
			SpendingDatas:       []*spend.SpendingData{{TxID: &frozenHash}},
		}, nil)

	// The ghost child's record is gone: NOT_FOUND → the goroutine leaves it nil and the
	// accumulation loop evicts it.
	mockStore.On("Get", mock.Anything, &ghostChild, mock.Anything).
		Return(nil, errors.NewTxNotFoundError("ghost child gone"))

	res, err := GetConflictingChildren(ctx, mockStore, rootHash)

	require.NoError(t, err)
	require.NotContains(t, res, ghostChild, "a NOT_FOUND child must be evicted, not propagate")
	require.NotContains(t, res, rootHash, "the root is always excluded from its own result")
	require.Contains(t, res, frozenHash, "the frozen sentinel spending child must stay visible in the result")
	require.Len(t, res, 1)
	// The frozen sentinel must never be descended into (no BFS Get on it).
	mockStore.AssertNotCalled(t, "Get", mock.Anything, &frozenHash, mock.Anything)
	mockStore.AssertExpectations(t)
}

// GetConflictingChildren BFS: the frozen sentinel surfaced as a CONFLICTING child (as
// opposed to a spending child) is likewise kept in the result but never descended into.
func TestGetConflictingChildren_KeepsFrozenConflictingChildNotDescended(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	rootHash := createTestHash("gcc-frozenchild-root")
	frozenHash := subtree.FrozenBytesTxHash

	mockStore.On("Get", mock.Anything, &rootHash, mock.Anything).
		Return(&meta.Data{ConflictingChildren: []chainhash.Hash{frozenHash}}, nil)

	res, err := GetConflictingChildren(ctx, mockStore, rootHash)

	require.NoError(t, err)
	require.Contains(t, res, frozenHash, "the frozen sentinel conflicting child must stay visible in the result")
	require.Len(t, res, 1)
	mockStore.AssertNotCalled(t, "Get", mock.Anything, &frozenHash, mock.Anything)
	mockStore.AssertExpectations(t)
}

// A non-not-found error while walking a recorded spender's existence must propagate — only
// a not-found is tolerated (and then discriminated by the deletedChildren marker).
func TestGetCounterConflictingTxHashes_SpenderGetErrorPropagates(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	txHash := createTestHash("spget-tx")
	parentTxHash := createTestHash("spget-parent")
	spenderHash := createTestHash("spget-spender")

	testTx := createTestTransactionWithInputs(parentTxHash, 0)

	mockStore.On("Get", mock.Anything, &txHash, mock.Anything).
		Return(&meta.Data{Tx: testTx}, nil)
	mockStore.On("Get", mock.Anything, &parentTxHash, mock.Anything).
		Return(&meta.Data{SpendingDatas: []*spend.SpendingData{{TxID: &spenderHash}}}, nil)
	// The spender existence check hits a transient (non-not-found) error.
	mockStore.On("Get", mock.Anything, &spenderHash, mock.Anything).
		Return(nil, errors.NewStorageError("transient spender read failure"))

	res, err := GetCounterConflictingTxHashes(ctx, mockStore, txHash)

	require.Nil(t, res)
	require.Error(t, err)
	require.False(t, errors.IsNotFound(err), "a transient spender error must propagate, not be tolerated as a ghost")
	mockStore.AssertExpectations(t)
}

// The queried tx may have been removed since it was flagged: the walk tolerates a not-found
// on the tx itself and returns an empty counter set rather than failing the caller.
func TestGetCounterConflictingTxHashes_QueriedTxGoneReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	txHash := createTestHash("gone-tx")

	mockStore.On("Get", mock.Anything, &txHash, mock.Anything).
		Return(nil, errors.NewTxNotFoundError("queried tx gone"))

	res, err := GetCounterConflictingTxHashes(ctx, mockStore, txHash)

	require.NoError(t, err)
	require.Empty(t, res)
	mockStore.AssertExpectations(t)
}

// GetConflictingChildren over a coinbase placeholder is a no-op that returns nil without any
// store read — pins the early placeholder short-circuit.
func TestGetConflictingChildren_CoinbasePlaceholderShortCircuits(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	res, err := GetConflictingChildren(ctx, mockStore, subtree.CoinbasePlaceholderHashValue)

	require.NoError(t, err)
	require.Nil(t, res)
	mockStore.AssertNotCalled(t, "Get", mock.Anything, mock.Anything, mock.Anything)
	mockStore.AssertExpectations(t)
}

// GetCounterConflictingTxHashes must fail closed when a PARENT record surfaces as (nil, nil):
// the spend graph for that input is lost, so the walk returns a not-found error rather than
// dereference nil SpendingDatas. This is distinct from the tolerated missing-spender case.
func TestGetCounterConflictingTxHashes_NilParentRecordFailsClosed(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	txHash := createTestHash("nilparent-tx")
	parentTxHash := createTestHash("nilparent-parent")

	testTx := createTestTransactionWithInputs(parentTxHash, 0)

	mockStore.On("Get", mock.Anything, &txHash, mock.Anything).
		Return(&meta.Data{Tx: testTx}, nil)

	// The parent record surfaces as (nil, nil): no error, no data.
	mockStore.On("Get", mock.Anything, &parentTxHash, mock.Anything).
		Return(nil, nil)

	res, err := GetCounterConflictingTxHashes(ctx, mockStore, txHash)

	require.Nil(t, res)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
	mockStore.AssertExpectations(t)
}
