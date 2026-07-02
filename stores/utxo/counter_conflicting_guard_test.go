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
