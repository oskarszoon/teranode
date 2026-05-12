package utxo

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// createSpendableTestTransaction builds a tx whose input has the
// PreviousTxScript / PreviousTxSatoshis fields populated so that
// util.UTXOHashFromInput can compute a hash. createSpendableTestTransaction
// in process_conflicting_test.go leaves those nil, which is fine for the
// existing ProcessConflicting tests (which use mocks for Spend/Unspend) but
// trips ReverseProcessConflicting's spendsForTx helper.
func createSpendableTestTransaction(parentHash chainhash.Hash, vout uint32) *bt.Tx {
	tx := bt.NewTx()

	input := &bt.Input{
		PreviousTxOutIndex: vout,
		PreviousTxSatoshis: 1_000,
		PreviousTxScript:   bscript.NewFromBytes([]byte{0x51}), // OP_1 — anything non-nil works
	}
	_ = input.PreviousTxIDAdd(&parentHash)
	tx.Inputs = append(tx.Inputs, input)

	output := &bt.Output{
		Satoshis:      900,
		LockingScript: bscript.NewFromBytes([]byte{0x51}),
	}
	tx.Outputs = append(tx.Outputs, output)

	return tx
}

// TestReverseProcessConflicting_RestoresOriginalSpender exercises the
// production-incident scenario: a stale block's moveForward invoked
// ProcessConflicting with [demoted] as the promoted winner, swapping
// parent.SpendingDatas to point at demoted and marking the original
// mempool spender (counter) Conflicting=true. When that block is moved
// back, ReverseProcessConflicting must restore the prior state:
// demoted → Conflicting=true (loser), counter → Conflicting=false
// (winner), parent.SpendingDatas[vout] re-spent by counter.
func TestReverseProcessConflicting_RestoresOriginalSpender(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	parentHash := createTestHash("reverse-parent")
	demotedHash := createTestHash("reverse-demoted")
	counterHash := createTestHash("reverse-counter")

	const vout = uint32(1)

	demotedTx := createSpendableTestTransaction(parentHash, vout)
	counterTx := createSpendableTestTransaction(parentHash, vout)

	// Step 1 — load demoted tx; observed state is the "post-ProcessConflicting"
	// inversion: demoted is currently Conflicting=false (was the winner).
	mockStore.On("Get", mock.Anything, &demotedHash, mock.Anything).
		Return(&meta.Data{Tx: demotedTx, Conflicting: false}, nil).Once()

	// selectCountersForDemotedTx walks demoted's inputs and queries the
	// parent for its ConflictingChildren list.
	mockStore.On("Get", mock.Anything, &parentHash, mock.Anything).
		Return(&meta.Data{
			ConflictingChildren: []chainhash.Hash{demotedHash, counterHash},
		}, nil).Once()

	// For each non-demoted candidate, the implementation loads the tx and
	// verifies it spends the same (parentHash, vout). Returns Conflicting=true
	// matching the post-stale-block state for the demoted counter.
	mockStore.On("Get", mock.Anything, &counterHash, mock.Anything).
		Return(&meta.Data{Tx: counterTx, Conflicting: true}, nil).Once()

	// Step 2 — MarkConflictingRecursively([demoted]) cascades demoted +
	// descendants → Conflicting=true. The mock returns no further children,
	// terminating the BFS after one round.
	demotedSpends := []*Spend{{TxID: &demotedHash, Vout: 0}}
	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{demotedHash}, true).
		Return(demotedSpends, []chainhash.Hash{}, nil).Once()

	// Step 3 — Unspend the demoted tx's input spends so the parent UTXO
	// no longer claims demoted as its spender.
	mockStore.On("Unspend", mock.Anything, mock.AnythingOfType("[]*utxo.Spend"), mock.Anything).
		Return(nil).Once()

	// Step 4 — re-fetch counter tx for the Spend call.
	mockStore.On("Get", mock.Anything, &counterHash, mock.Anything).
		Return(&meta.Data{Tx: counterTx}, nil).Once()

	// Re-spend parent UTXO with counter.
	mockStore.On("Spend", mock.Anything, counterTx, mock.Anything, mock.Anything).
		Return([]*Spend{}, nil).Once()

	// Step 5 — UnmarkConflictingRecursively([counter]) flips counter +
	// descendants back to Conflicting=false.
	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{counterHash}, false).
		Return([]*Spend{}, []chainhash.Hash{}, nil).Once()

	touched, err := ReverseProcessConflicting(ctx, mockStore, 100,
		[]chainhash.Hash{demotedHash})

	require.NoError(t, err)
	require.NotNil(t, touched)
	require.Contains(t, touched, demotedHash, "demoted tx must be in touched set")
	require.Contains(t, touched, counterHash, "counter tx must be in touched set")
	mockStore.AssertExpectations(t)
}

// TestReverseProcessConflicting_SkipsAlreadyReversedDemoted handles
// idempotency: if a moveBack is replayed (e.g. during reset) on a block
// whose ConflictingNodes were already restored, the demoted tx is
// already Conflicting=true and we have nothing to undo.
func TestReverseProcessConflicting_SkipsAlreadyReversedDemoted(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	demotedHash := createTestHash("reverse-already-conflicting")
	demotedTx := createTestTransaction()

	// Demoted already Conflicting=true → skip.
	mockStore.On("Get", mock.Anything, &demotedHash, mock.Anything).
		Return(&meta.Data{Tx: demotedTx, Conflicting: true}, nil).Once()

	touched, err := ReverseProcessConflicting(ctx, mockStore, 100,
		[]chainhash.Hash{demotedHash})

	require.NoError(t, err)
	assert.Nil(t, touched, "no work done → no touched hashes")
	mockStore.AssertExpectations(t)
}

// TestReverseProcessConflicting_FiltersCounterWithMismatchedOutput
// covers the same-output check inside selectCountersForDemotedTx: a tx
// listed in parent.ConflictingChildren that does NOT actually spend the
// (parent, vout) the demoted tx spends must be skipped (it conflicts
// with a sibling output, not this one).
func TestReverseProcessConflicting_FiltersCounterWithMismatchedOutput(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	parentHash := createTestHash("reverse-parent-multi")
	demotedHash := createTestHash("reverse-demoted-multi")
	mismatchedHash := createTestHash("reverse-counter-mismatched-output")

	const demotedVout = uint32(3)
	const otherVout = uint32(7)

	demotedTx := createSpendableTestTransaction(parentHash, demotedVout)
	// mismatchedTx spends parentHash but a DIFFERENT vout — it's a
	// legitimate sibling-output spender, not a counter to our demoted tx.
	mismatchedTx := createSpendableTestTransaction(parentHash, otherVout)

	mockStore.On("Get", mock.Anything, &demotedHash, mock.Anything).
		Return(&meta.Data{Tx: demotedTx, Conflicting: false}, nil).Once()

	mockStore.On("Get", mock.Anything, &parentHash, mock.Anything).
		Return(&meta.Data{
			ConflictingChildren: []chainhash.Hash{demotedHash, mismatchedHash},
		}, nil).Once()

	mockStore.On("Get", mock.Anything, &mismatchedHash, mock.Anything).
		Return(&meta.Data{Tx: mismatchedTx, Conflicting: true}, nil).Once()

	// No Spend / Unmark on mismatched: same-output filter rejects it.
	demotedSpends := []*Spend{{TxID: &demotedHash, Vout: 0}}
	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{demotedHash}, true).
		Return(demotedSpends, []chainhash.Hash{}, nil).Once()
	mockStore.On("Unspend", mock.Anything, mock.AnythingOfType("[]*utxo.Spend"), mock.Anything).
		Return(nil).Once()

	touched, err := ReverseProcessConflicting(ctx, mockStore, 100,
		[]chainhash.Hash{demotedHash})

	require.NoError(t, err)
	require.Contains(t, touched, demotedHash)
	require.NotContains(t, touched, mismatchedHash,
		"mismatched-output candidate must not be touched")
	mockStore.AssertExpectations(t)
}

// TestReverseProcessConflicting_NoCounterToPromote covers the case where
// the demoted tx is currently the spender but no candidate in
// parent.ConflictingChildren is Conflicting=true (the counter was
// already promoted by some other process, or never existed). The
// function should still mark demoted Conflicting=true and unspend its
// inputs, but skip the Spend/Unmark steps.
func TestReverseProcessConflicting_NoCounterToPromote(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	parentHash := createTestHash("reverse-parent-lone")
	demotedHash := createTestHash("reverse-demoted-lone")

	demotedTx := createSpendableTestTransaction(parentHash, 0)

	mockStore.On("Get", mock.Anything, &demotedHash, mock.Anything).
		Return(&meta.Data{Tx: demotedTx, Conflicting: false}, nil).Once()

	// parent has only the demoted tx as a conflicting child — no counter.
	mockStore.On("Get", mock.Anything, &parentHash, mock.Anything).
		Return(&meta.Data{
			ConflictingChildren: []chainhash.Hash{demotedHash},
		}, nil).Once()

	demotedSpends := []*Spend{{TxID: &demotedHash, Vout: 0}}
	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{demotedHash}, true).
		Return(demotedSpends, []chainhash.Hash{}, nil).Once()
	mockStore.On("Unspend", mock.Anything, mock.AnythingOfType("[]*utxo.Spend"), mock.Anything).
		Return(nil).Once()

	touched, err := ReverseProcessConflicting(ctx, mockStore, 100,
		[]chainhash.Hash{demotedHash})

	require.NoError(t, err)
	require.Contains(t, touched, demotedHash)
	mockStore.AssertExpectations(t)
}

// TestReverseProcessConflicting_SkipsCounterAlreadyNonConflicting verifies
// the Conflicting=true filter inside selectCountersForDemotedTx: if the
// candidate counter is already Conflicting=false, it's not part of the
// state ProcessConflicting flipped — we must leave it alone.
func TestReverseProcessConflicting_SkipsCounterAlreadyNonConflicting(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	parentHash := createTestHash("reverse-parent-stale-counter")
	demotedHash := createTestHash("reverse-demoted-stale-counter")
	counterHash := createTestHash("reverse-counter-stale")

	const vout = uint32(2)

	demotedTx := createSpendableTestTransaction(parentHash, vout)
	counterTx := createSpendableTestTransaction(parentHash, vout)

	mockStore.On("Get", mock.Anything, &demotedHash, mock.Anything).
		Return(&meta.Data{Tx: demotedTx, Conflicting: false}, nil).Once()

	mockStore.On("Get", mock.Anything, &parentHash, mock.Anything).
		Return(&meta.Data{
			ConflictingChildren: []chainhash.Hash{demotedHash, counterHash},
		}, nil).Once()

	// Counter is Conflicting=false → skip.
	mockStore.On("Get", mock.Anything, &counterHash, mock.Anything).
		Return(&meta.Data{Tx: counterTx, Conflicting: false}, nil).Once()

	demotedSpends := []*Spend{{TxID: &demotedHash, Vout: 0}}
	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{demotedHash}, true).
		Return(demotedSpends, []chainhash.Hash{}, nil).Once()
	mockStore.On("Unspend", mock.Anything, mock.AnythingOfType("[]*utxo.Spend"), mock.Anything).
		Return(nil).Once()

	touched, err := ReverseProcessConflicting(ctx, mockStore, 100,
		[]chainhash.Hash{demotedHash})

	require.NoError(t, err)
	require.Contains(t, touched, demotedHash)
	require.NotContains(t, touched, counterHash)
	mockStore.AssertExpectations(t)
}

// TestReverseProcessConflicting_CoinbasePlaceholderSkipped mirrors
// ProcessConflicting's frozen-tx guard at the head of its loop.
func TestReverseProcessConflicting_CoinbasePlaceholderSkipped(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	// No expectations set — the placeholder branch should short-circuit
	// without touching the store.
	touched, err := ReverseProcessConflicting(ctx, mockStore, 1,
		[]chainhash.Hash{subtree.CoinbasePlaceholderHashValue})

	require.NoError(t, err)
	assert.Nil(t, touched)
	mockStore.AssertExpectations(t)
}

// TestReverseProcessConflicting_EmptyInput is a no-op guard for callers
// that pass a zero-length list (subtree had no ConflictingNodes).
func TestReverseProcessConflicting_EmptyInput(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	touched, err := ReverseProcessConflicting(ctx, mockStore, 1, nil)

	require.NoError(t, err)
	assert.Nil(t, touched)
	mockStore.AssertExpectations(t)
}

// TestReverseProcessConflicting_PropagatesGetError surfaces store-level
// errors instead of silently skipping the tx — a Get failure during
// reverse is non-recoverable for this block's moveBack.
func TestReverseProcessConflicting_PropagatesGetError(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	demotedHash := createTestHash("reverse-get-error")

	mockStore.On("Get", mock.Anything, &demotedHash, mock.Anything).
		Return((*meta.Data)(nil), errors.NewProcessingError("aerospike unavailable")).Once()

	touched, err := ReverseProcessConflicting(ctx, mockStore, 1,
		[]chainhash.Hash{demotedHash})

	require.Error(t, err)
	assert.Nil(t, touched)
	assert.Contains(t, err.Error(), "error getting demoted tx meta")
	mockStore.AssertExpectations(t)
}

// TestUnmarkConflictingRecursively_BFSCascade verifies that the BFS
// inversely mirrors MarkConflictingRecursively: input set + every
// descendant reached via SpendingDatas have Conflicting=false applied.
func TestUnmarkConflictingRecursively_BFSCascade(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	parentHash := createTestHash("unmark-root")
	childHash := createTestHash("unmark-child")
	grandchildHash := createTestHash("unmark-grandchild")

	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{parentHash}, false).
		Return([]*Spend{}, []chainhash.Hash{childHash}, nil).Once()
	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{childHash}, false).
		Return([]*Spend{}, []chainhash.Hash{grandchildHash}, nil).Once()
	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{grandchildHash}, false).
		Return([]*Spend{}, []chainhash.Hash{}, nil).Once()

	cleared, err := UnmarkConflictingRecursively(ctx, mockStore, []chainhash.Hash{parentHash})

	require.NoError(t, err)
	require.Equal(t, []chainhash.Hash{parentHash, childHash, grandchildHash}, cleared,
		"cleared set must be BFS order: input first, then each descendant level")
	mockStore.AssertExpectations(t)
}
