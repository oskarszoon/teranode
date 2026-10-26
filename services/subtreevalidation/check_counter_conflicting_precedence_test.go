package subtreevalidation

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// faultyGetStore fails Get for one hash with a storage error.
type faultyGetStore struct {
	utxo.Store
	fail chainhash.Hash
}

func (f *faultyGetStore) Get(ctx context.Context, hash *chainhash.Hash, fs ...fields.FieldName) (*meta.Data, error) {
	if hash.Equal(f.fail) {
		return nil, errors.NewStorageError("store down")
	}

	return f.Store.Get(ctx, hash, fs...)
}

// A tx-invalid verdict proven from records that were read is final: the block
// is rejected whatever else in the batch could not be checked. A processing
// error is returned only when nothing is proven. Otherwise one tx whose parent
// was pruned for good, or one runaway cone, makes a block that also carries a
// provably invalid tx retry forever instead of being rejected.
func Test_checkCounterConflictingOnCurrentChainBatch_provenTxInvalidWins(t *testing.T) {
	ctx := context.Background()
	mined := utxo.WithMinedBlockInfo(utxo.MinedBlockInfo{BlockID: 77, BlockHeight: 100})
	blockIds := map[uint32]bool{77: true}

	// requireRejected checks the cached path end to end: the verdict is
	// tx-invalid through its wrapping.
	requireRejected := func(t *testing.T, store utxo.Store, tSettings *settings.Settings, conflictingTxs []chainhash.Hash) {
		t.Helper()

		s, block, cachedSubtrees := newCachedCheckServer(t, store, tSettings, conflictingTxs, []uint32{77})

		err := s.checkCachedSubtreesAgainstCandidateChain(ctx, block, cachedSubtrees)
		require.ErrorIs(t, err, errors.ErrTxInvalid)
	}

	t.Run("fresh path: a pruned parent does not hide a mined counter-spender", func(t *testing.T) {
		tSettings := test.CreateBaseTestSettings(t)

		store := newSQLUtxoStore(t, ctx, tSettings)
		_, chain := buildConflictingChain(t, ctx, store, 1, false, mined)

		// a conflicting tx whose parent the store no longer holds: on its own,
		// the fresh path's verdict is a retryable TX_NOT_FOUND
		orphanParent := buildTx(t, []outpoint{{tx: parentTx1, vout: 0}}, 1, 9001)
		_, _, err := store.SpendAndCreate(ctx, orphanParent, 122, utxo.WithCreateOnly())
		require.NoError(t, err)

		orphan := spendingChild(t, orphanParent, 0)
		createConflicting(t, ctx, store, orphan)
		require.NoError(t, store.Delete(ctx, orphanParent.TxIDChainHash()))

		s := &Server{logger: ulogger.NewErrorTestLogger(t), settings: tSettings, utxoStore: store}

		alone, err := s.checkCounterConflictingOnCurrentChainBatch(ctx, []chainhash.Hash{*orphan.TxIDChainHash()}, blockIds, failOnMissingRecords)
		require.Nil(t, alone)
		require.ErrorIs(t, err, errors.ErrTxNotFound, "alone, the orphan is a retryable failure")

		txHashes := []chainhash.Hash{*orphan.TxIDChainHash(), chain[0]}

		_, err = s.processTxMetaUsingStore(ctx, txHashes, make([]metaSliceItem, len(txHashes)), blockIds, true, false, true)
		require.ErrorIs(t, err, errors.ErrTxInvalid)
	})

	t.Run("cached path: a store fault on one tx does not hide a mined counter-spender", func(t *testing.T) {
		tSettings := test.CreateBaseTestSettings(t)
		tSettings.UtxoStore.SpendBatcherDurationMillis = 1

		store := newSQLUtxoStore(t, ctx, tSettings)
		faultedTx := buildIndependentConflicts(t, ctx, store, 1)[0]
		_, chain := buildConflictingChain(t, ctx, store, 1, false, mined)

		faulty := &faultyGetStore{Store: store, fail: faultedTx}

		s, block, cachedSubtrees := newCachedCheckServer(t, faulty, tSettings, []chainhash.Hash{faultedTx}, []uint32{77})
		err := s.checkCachedSubtreesAgainstCandidateChain(ctx, block, cachedSubtrees)
		require.ErrorIs(t, err, errors.ErrStorageError, "alone, the faulted tx is a retryable failure")
		require.NotErrorIs(t, err, errors.ErrTxInvalid)

		requireRejected(t, faulty, tSettings, []chainhash.Hash{faultedTx, chain[0]})
	})

	t.Run("a walk over its budget does not hide a mined direct counter-spender", func(t *testing.T) {
		const coneLength = 10

		tSettings := test.CreateBaseTestSettings(t)
		tSettings.UtxoStore.SpendBatcherDurationMillis = 1
		// four roots (two conflicting txs, two counter-spenders): a budget of 8,
		// against one counter-spender cone of 10
		tSettings.UtxoStore.ConflictingChildrenMaxNodes = 2

		store := newSQLUtxoStore(t, ctx, tSettings)
		counter, chain := buildConflictingChain(t, ctx, store, 1, false)

		prev := counter
		for i := 0; i < coneLength; i++ {
			child := spendingChild(t, prev, 0)
			spendAndCreate(t, ctx, store, child)
			prev = child
		}

		minedConflict := buildIndependentConflicts(t, ctx, store, 1, mined)[0]

		s, block, cachedSubtrees := newCachedCheckServer(t, store, tSettings, chain, []uint32{77})
		require.ErrorIs(t, s.checkCachedSubtreesAgainstCandidateChain(ctx, block, cachedSubtrees), errors.ErrUtxoWalkLimitExceeded,
			"alone, the oversized cone fails closed")

		requireRejected(t, store, tSettings, []chainhash.Hash{chain[0], minedConflict})
	})

	t.Run("a mined member the walk reached before its budget ran out is proof", func(t *testing.T) {
		const coneLength = 10

		tSettings := test.CreateBaseTestSettings(t)
		tSettings.UtxoStore.SpendBatcherDurationMillis = 1
		// two roots (tx1 and its counter-spender): a budget of 4, against a
		// counter-spender cone of 10 whose first member is mined
		tSettings.UtxoStore.ConflictingChildrenMaxNodes = 2

		store := newSQLUtxoStore(t, ctx, tSettings)
		counter, chain := buildConflictingChain(t, ctx, store, 1, false)

		prev := counter
		for i := 0; i < coneLength; i++ {
			child := spendingChild(t, prev, 0)

			if i == 0 {
				spendAndCreate(t, ctx, store, child, mined)
			} else {
				spendAndCreate(t, ctx, store, child)
			}

			prev = child
		}

		requireRejected(t, store, tSettings, chain)
	})

	t.Run("a frozen counter-spender does not hide a mined counter-spender", func(t *testing.T) {
		tSettings := test.CreateBaseTestSettings(t)
		tSettings.UtxoStore.SpendBatcherDurationMillis = 1

		store := newSQLUtxoStore(t, ctx, tSettings)

		_, _, err := store.SpendAndCreate(ctx, parentTx1, 122, utxo.WithCreateOnly())
		require.NoError(t, err)

		freezeOutput(t, ctx, store, parentTx1, 2, tSettings)
		createConflicting(t, ctx, store, tx1)

		minedConflict := buildIndependentConflicts(t, ctx, store, 1, mined)[0]

		requireRejected(t, store, tSettings, []chainhash.Hash{*tx1.TxIDChainHash(), minedConflict})
	})

	t.Run("a frozen own cone does not hide a mined counter-spender", func(t *testing.T) {
		tSettings := test.CreateBaseTestSettings(t)
		tSettings.UtxoStore.SpendBatcherDurationMillis = 1

		store := newSQLUtxoStore(t, ctx, tSettings)
		_, chain := buildConflictingChain(t, ctx, store, 1, false)

		tip, err := store.Get(ctx, &chain[0], fields.Tx)
		require.NoError(t, err)
		freezeOutput(t, ctx, store, tip.Tx, 0, tSettings)

		minedConflict := buildIndependentConflicts(t, ctx, store, 1, mined)[0]

		requireRejected(t, store, tSettings, []chainhash.Hash{chain[0], minedConflict})
	})
}

// On the fresh path an entry left set is never decorated again, so it must have
// passed the conflict check. When the check ran and passed, the collected
// entries have, and clearing them would only make every fail-fast retry re-check
// the whole batch: they stay set, and the batches' error is still returned.
func Test_processTxMetaUsingStore_passedConflictCheckKeepsEntries(t *testing.T) {
	ctx := context.Background()

	tSettings := test.CreateBaseTestSettings(t)
	// one tx per batch, run in order: the conflicting tx is collected first,
	// then the second missing tx trips the threshold
	tSettings.BlockValidation.ProcessTxMetaUsingStoreBatchSize = 1
	tSettings.BlockValidation.ProcessTxMetaUsingStoreConcurrency = 1
	tSettings.BlockValidation.ProcessTxMetaUsingStoreMissingTxThreshold = 1

	store := newSQLUtxoStore(t, ctx, tSettings)
	_, chain := buildConflictingChain(t, ctx, store, 1, false)

	s := &Server{logger: ulogger.NewErrorTestLogger(t), settings: tSettings, utxoStore: store}

	txHashes := []chainhash.Hash{chain[0], chainhash.HashH([]byte("missing-1")), chainhash.HashH([]byte("missing-2"))}
	txMetaSlice := make([]metaSliceItem, len(txHashes))

	_, err := s.processTxMetaUsingStore(ctx, txHashes, txMetaSlice, map[uint32]bool{77: true}, true, true, true)
	require.ErrorIs(t, err, errors.ErrThresholdExceeded)
	require.NotErrorIs(t, err, errors.ErrTxInvalid)

	require.True(t, txMetaSlice[0].isSet, "the conflicting tx passed the check, so the retry need not decorate and check it again")
	require.True(t, txMetaSlice[0].conflicting)
}
