package utxo

// Direct unit coverage for collectDanglingWinnerInputSpends. The ProcessConflicting-level
// guard tests exercise the success and fail-closed outcomes end-to-end; these drive the
// helper in isolation to pin the early-skip and error branches that are awkward to reach
// through the full resolution flow (nil tx / index mismatch, nil parent ref, GetSpend
// error variants, the self-spend and marked-spender skips, and the UTXO-hash failure on the
// benign-ghost path).

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/stores/utxo/spend"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// nilPrevTxIDTx builds a tx whose single input never had PreviousTxIDAdd called, so
// input.PreviousTxIDChainHash() returns nil — the guard's nil-parent-ref skip.
func nilPrevTxIDTx() *bt.Tx {
	tx := bt.NewTx()
	tx.Inputs = append(tx.Inputs, &bt.Input{PreviousTxOutIndex: 0})
	tx.Outputs = append(tx.Outputs, &bt.Output{Satoshis: 1000})

	return tx
}

// A nil tx entry and a hash-slice shorter than the tx slice must both be skipped without
// touching the store.
func TestCollectDanglingWinnerInputSpends_NilTxAndIndexMismatch(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	parentTxHash := createTestHash("collect-nil-parent")
	// winningTxs has two entries; winningTxHashes has one, so index 1 is out of range.
	winningTxs := []*bt.Tx{nil, createTestTransactionWithInputs(parentTxHash, 0)}
	winningTxHashes := []chainhash.Hash{createTestHash("collect-nil-h0")}

	res, err := collectDanglingWinnerInputSpends(ctx, mockStore, winningTxs, winningTxHashes, map[chainhash.Hash]struct{}{})

	require.NoError(t, err)
	require.Empty(t, res)
	// Neither the nil tx nor the out-of-range tx may reach a store read.
	mockStore.AssertNotCalled(t, "GetSpend", mock.Anything, mock.Anything)
	mockStore.AssertExpectations(t)
}

// An input whose previous-tx-id was never set (nil chainhash) is skipped before any read.
func TestCollectDanglingWinnerInputSpends_NilPrevTxIDRefSkipped(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	winningTxs := []*bt.Tx{nilPrevTxIDTx()}
	winningTxHashes := []chainhash.Hash{createTestHash("collect-nilref-tx")}

	res, err := collectDanglingWinnerInputSpends(ctx, mockStore, winningTxs, winningTxHashes, map[chainhash.Hash]struct{}{})

	require.NoError(t, err)
	require.Empty(t, res)
	mockStore.AssertNotCalled(t, "GetSpend", mock.Anything, mock.Anything)
	mockStore.AssertExpectations(t)
}

// GetSpend reporting the slot as NOT_FOUND (missing parent normalised on both backends) is
// a no-op: there is no slot to repair.
func TestCollectDanglingWinnerInputSpends_GetSpendNotFoundSkipped(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	parentTxHash := createTestHash("collect-notfound-parent")
	winningTxs := []*bt.Tx{createTestTransactionWithInputs(parentTxHash, 0)}
	winningTxHashes := []chainhash.Hash{createTestHash("collect-notfound-tx")}

	// Error variant: GetSpend surfaces a not-found error.
	mockStore.On("GetSpend", mock.Anything, mock.MatchedBy(func(sp *Spend) bool {
		return sp.TxID != nil && sp.TxID.Equal(parentTxHash)
	})).Return((*SpendResponse)(nil), errors.NewTxNotFoundError("parent gone"))

	res, err := collectDanglingWinnerInputSpends(ctx, mockStore, winningTxs, winningTxHashes, map[chainhash.Hash]struct{}{})

	require.NoError(t, err)
	require.Empty(t, res)
	mockStore.AssertExpectations(t)
}

// A non-not-found GetSpend error aborts the whole collect with a wrapped processing error.
func TestCollectDanglingWinnerInputSpends_GetSpendErrorPropagates(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	parentTxHash := createTestHash("collect-gserr-parent")
	winningTxs := []*bt.Tx{createTestTransactionWithInputs(parentTxHash, 0)}
	winningTxHashes := []chainhash.Hash{createTestHash("collect-gserr-tx")}

	mockStore.On("GetSpend", mock.Anything, mock.MatchedBy(func(sp *Spend) bool {
		return sp.TxID != nil && sp.TxID.Equal(parentTxHash)
	})).Return((*SpendResponse)(nil), errors.NewStorageError("transient read failure"))

	res, err := collectDanglingWinnerInputSpends(ctx, mockStore, winningTxs, winningTxHashes, map[chainhash.Hash]struct{}{})

	require.Nil(t, res)
	require.Error(t, err)
	require.Contains(t, err.Error(), "error getting parent")
	require.False(t, errors.IsNotFound(err), "a transient error must propagate, not be swallowed as not-found")
	mockStore.AssertExpectations(t)
}

// A slot whose SpendingData (or its TxID) is nil carries no spender to repair: skipped.
func TestCollectDanglingWinnerInputSpends_NilSpendingDataSkipped(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	parentTxHash := createTestHash("collect-nilsd-parent")
	winningTxs := []*bt.Tx{createTestTransactionWithInputs(parentTxHash, 0)}
	winningTxHashes := []chainhash.Hash{createTestHash("collect-nilsd-tx")}

	// Status OK (0), but no SpendingData → nothing recorded in the slot.
	mockStore.On("GetSpend", mock.Anything, mock.MatchedBy(func(sp *Spend) bool {
		return sp.TxID != nil && sp.TxID.Equal(parentTxHash)
	})).Return(&SpendResponse{Status: int(Status_OK), SpendingData: nil}, nil)

	res, err := collectDanglingWinnerInputSpends(ctx, mockStore, winningTxs, winningTxHashes, map[chainhash.Hash]struct{}{})

	require.NoError(t, err)
	require.Empty(t, res)
	mockStore.AssertExpectations(t)
}

// The slot recorded as spent by the winner itself is already correct: skipped.
func TestCollectDanglingWinnerInputSpends_SelfSpendSkipped(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	parentTxHash := createTestHash("collect-self-parent")
	winnerHash := createTestHash("collect-self-winner")
	winningTxs := []*bt.Tx{createTestTransactionWithInputs(parentTxHash, 0)}
	winningTxHashes := []chainhash.Hash{winnerHash}

	// Slot's recorded spender is the winner itself.
	selfHash := winnerHash
	mockStore.On("GetSpend", mock.Anything, mock.MatchedBy(func(sp *Spend) bool {
		return sp.TxID != nil && sp.TxID.Equal(parentTxHash)
	})).Return(&SpendResponse{SpendingData: &spend.SpendingData{TxID: &selfHash}}, nil)

	res, err := collectDanglingWinnerInputSpends(ctx, mockStore, winningTxs, winningTxHashes, map[chainhash.Hash]struct{}{})

	require.NoError(t, err)
	require.Empty(t, res)
	// No existence check on the winner as a foreign spender.
	mockStore.AssertNotCalled(t, "Get", mock.Anything, mock.Anything, mock.Anything)
	mockStore.AssertExpectations(t)
}

// A spender already in the marked-conflicting set has its slot cleared by step 1's cascade:
// the collect skips it without an existence check.
func TestCollectDanglingWinnerInputSpends_MarkedSpenderSkipped(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	parentTxHash := createTestHash("collect-marked-parent")
	spenderHash := createTestHash("collect-marked-spender")
	winningTxs := []*bt.Tx{createTestTransactionWithInputs(parentTxHash, 0)}
	winningTxHashes := []chainhash.Hash{createTestHash("collect-marked-tx")}
	markedSet := map[chainhash.Hash]struct{}{spenderHash: {}}

	mockStore.On("GetSpend", mock.Anything, mock.MatchedBy(func(sp *Spend) bool {
		return sp.TxID != nil && sp.TxID.Equal(parentTxHash)
	})).Return(&SpendResponse{SpendingData: &spend.SpendingData{TxID: &spenderHash}}, nil)

	res, err := collectDanglingWinnerInputSpends(ctx, mockStore, winningTxs, winningTxHashes, markedSet)

	require.NoError(t, err)
	require.Empty(t, res)
	mockStore.AssertNotCalled(t, "Get", mock.Anything, &spenderHash, mock.Anything)
	mockStore.AssertExpectations(t)
}

// A non-not-found error on the spender existence Get aborts the collect with a wrapped error.
func TestCollectDanglingWinnerInputSpends_SpenderGetErrorPropagates(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	parentTxHash := createTestHash("collect-spget-parent")
	spenderHash := createTestHash("collect-spget-spender")
	winningTxs := []*bt.Tx{createTestTransactionWithInputs(parentTxHash, 0)}
	winningTxHashes := []chainhash.Hash{createTestHash("collect-spget-tx")}

	mockStore.On("GetSpend", mock.Anything, mock.MatchedBy(func(sp *Spend) bool {
		return sp.TxID != nil && sp.TxID.Equal(parentTxHash)
	})).Return(&SpendResponse{SpendingData: &spend.SpendingData{TxID: &spenderHash}}, nil)

	// The spender existence check hits a transient (non-not-found) storage error.
	mockStore.On("Get", mock.Anything, &spenderHash, mock.Anything).
		Return(nil, errors.NewStorageError("transient spender read failure"))

	res, err := collectDanglingWinnerInputSpends(ctx, mockStore, winningTxs, winningTxHashes, map[chainhash.Hash]struct{}{})

	require.Nil(t, res)
	require.Error(t, err)
	require.Contains(t, err.Error(), "error getting spender")
	mockStore.AssertExpectations(t)
}

// Benign-ghost path (spender record gone, no deletedChildren marker) that cannot be repaired
// because the input carries no PreviousTxScript, so util.UTXOHashFromInput fails: the collect
// aborts with a wrapped hashing error rather than queueing an unhashable spend.
func TestCollectDanglingWinnerInputSpends_UTXOHashErrorOnGhost(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	parentTxHash := createTestHash("collect-hash-parent")
	ghostHash := createTestHash("collect-hash-ghost")
	// createTestTransactionWithInputs leaves PreviousTxScript nil, so UTXOHashFromInput errors.
	winningTxs := []*bt.Tx{createTestTransactionWithInputs(parentTxHash, 0)}
	winningTxHashes := []chainhash.Hash{createTestHash("collect-hash-tx")}

	mockStore.On("GetSpend", mock.Anything, mock.MatchedBy(func(sp *Spend) bool {
		return sp.TxID != nil && sp.TxID.Equal(parentTxHash)
	})).Return(&SpendResponse{SpendingData: &spend.SpendingData{TxID: &ghostHash}}, nil)

	// Ghost's own record is gone (not-found → tolerated, not an abort).
	mockStore.On("Get", mock.Anything, &ghostHash, mock.Anything).
		Return(nil, errors.NewTxNotFoundError("ghost gone"))

	// Parent carries no deletedChildren marker for the ghost → benign dangling ref.
	mockStore.On("Get", mock.Anything, &parentTxHash, mock.Anything).
		Return(&meta.Data{}, nil)

	res, err := collectDanglingWinnerInputSpends(ctx, mockStore, winningTxs, winningTxHashes, map[chainhash.Hash]struct{}{})

	require.Nil(t, res)
	require.Error(t, err)
	require.Contains(t, err.Error(), "error hashing input")
	mockStore.AssertExpectations(t)
}

// A non-not-found error while fetching the parent's deletedChildren marker aborts the
// collect: the marker discriminator can't run, so fail rather than guess.
func TestCollectDanglingWinnerInputSpends_MarkerGetErrorPropagates(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	parentTxHash := createTestHash("collect-marker-parent")
	ghostHash := createTestHash("collect-marker-ghost")
	winningTxs := []*bt.Tx{createTestTransactionWithInputs(parentTxHash, 0)}
	winningTxHashes := []chainhash.Hash{createTestHash("collect-marker-tx")}

	mockStore.On("GetSpend", mock.Anything, mock.MatchedBy(func(sp *Spend) bool {
		return sp.TxID != nil && sp.TxID.Equal(parentTxHash)
	})).Return(&SpendResponse{SpendingData: &spend.SpendingData{TxID: &ghostHash}}, nil)

	// Ghost record gone (tolerated), but the marker fetch on the parent errors transiently.
	mockStore.On("Get", mock.Anything, &ghostHash, mock.Anything).
		Return(nil, errors.NewTxNotFoundError("ghost gone"))
	mockStore.On("Get", mock.Anything, &parentTxHash, mock.Anything).
		Return(nil, errors.NewStorageError("transient marker read failure"))

	res, err := collectDanglingWinnerInputSpends(ctx, mockStore, winningTxs, winningTxHashes, map[chainhash.Hash]struct{}{})

	require.Nil(t, res)
	require.Error(t, err)
	require.Contains(t, err.Error(), "error getting deletedChildren marker")
	mockStore.AssertExpectations(t)
}

// Benign-ghost happy path: a repairable dangling ref (input carries PreviousTxScript) is
// queued as a Spend carrying the stored spending data so step 2 can clear the stale slot.
func TestCollectDanglingWinnerInputSpends_QueuesBenignGhost(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	parentTxHash := createTestHash("collect-ok-parent")
	ghostHash := createTestHash("collect-ok-ghost")
	winningTxs := []*bt.Tx{extendedInputTx(parentTxHash, 0)}
	winningTxHashes := []chainhash.Hash{createTestHash("collect-ok-tx")}

	spendingData := &spend.SpendingData{TxID: &ghostHash}
	mockStore.On("GetSpend", mock.Anything, mock.MatchedBy(func(sp *Spend) bool {
		return sp.TxID != nil && sp.TxID.Equal(parentTxHash)
	})).Return(&SpendResponse{SpendingData: spendingData}, nil)

	mockStore.On("Get", mock.Anything, &ghostHash, mock.Anything).
		Return(nil, errors.NewTxNotFoundError("ghost gone"))
	mockStore.On("Get", mock.Anything, &parentTxHash, mock.Anything).
		Return(&meta.Data{}, nil)

	res, err := collectDanglingWinnerInputSpends(ctx, mockStore, winningTxs, winningTxHashes, map[chainhash.Hash]struct{}{})

	require.NoError(t, err)
	require.Len(t, res, 1)
	require.NotNil(t, res[0])
	require.True(t, res[0].TxID.Equal(parentTxHash), "the queued spend must target the parent slot")
	require.Equal(t, spendingData, res[0].SpendingData, "the queued spend must carry the stored spending data")
	require.NotNil(t, res[0].UTXOHash)
	mockStore.AssertExpectations(t)
}
