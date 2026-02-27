package aerospike_test

import (
	"testing"

	"github.com/aerospike/aerospike-client-go/v8"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/bsv-blockchain/teranode/util/uaerospike"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// go test -v -tags test_aerospike ./test/...

func TestStore_SpendMultiRecord(t *testing.T) {
	logger := ulogger.NewErrorTestLogger(t)
	tSettings := test.CreateBaseTestSettings(t)

	client, store, ctx, deferFn := initAerospike(t, tSettings, logger)

	t.Cleanup(func() {
		deferFn()
	})

	t.Run("Spent tx id", func(t *testing.T) {
		// clean up the externalStore, if needed
		_ = store.GetExternalStore().Del(ctx, tx.TxIDChainHash().CloneBytes(), fileformat.FileTypeTx)

		// create a tx
		_, err := store.Create(ctx, tx, 101)
		require.NoError(t, err)

		// spend the tx
		_, err = store.Spend(ctx, spendTx, store.GetBlockHeight()+1)
		require.NoError(t, err)

		// spend again, should not return an error
		_, err = store.Spend(ctx, spendTx, store.GetBlockHeight()+1)
		require.NoError(t, err)

		// try to spend the tx with a different tx, check the spending tx ID
		spends, err := store.Spend(ctx, spendTx2, store.GetBlockHeight()+1)
		require.Error(t, err)

		var tErr *errors.Error
		require.ErrorAs(t, err, &tErr)
		require.Equal(t, errors.ERR_UTXO_ERROR, tErr.Code())
		require.ErrorIs(t, spends[0].Err, errors.ErrSpent)
		require.Equal(t, spendTx.TxIDChainHash().String(), spends[0].ConflictingTxID.String())
	})

	t.Run("SpendMultiRecord LUA", func(t *testing.T) {
		cleanDB(t, client)

		store.SetUtxoBatchSize(1)

		// clean up the externalStore, if needed
		_ = store.GetExternalStore().Del(ctx, tx.TxIDChainHash().CloneBytes(), fileformat.FileTypeTx)

		// create a tx
		_, err := store.Create(ctx, tx, 101)
		require.NoError(t, err)

		keyTx, err := aerospike.NewKey(store.GetNamespace(), store.GetName(), tx.TxIDChainHash().CloneBytes())
		require.NoError(t, err)

		resp, err := client.Get(nil, keyTx)
		require.NoError(t, err)

		// Check the totalExtraRecs
		totalExtraRecs, ok := resp.Bins[fields.TotalExtraRecs.String()].(int)
		require.True(t, ok)
		assert.Equal(t, 4, totalExtraRecs) // parent is one, and there are 4 extra records

		// mine the tx
		blockIDsMap, err := store.SetMinedMulti(ctx, []*chainhash.Hash{tx.TxIDChainHash()}, utxo.MinedBlockInfo{BlockID: 101, BlockHeight: 101, SubtreeIdx: 101, OnLongestChain: true})
		require.NoError(t, err)
		assert.Len(t, blockIDsMap, 1)
		assert.Equal(t, uint32(101), blockIDsMap[*tx.TxIDChainHash()][0])

		utxoHashes := make([]*chainhash.Hash, len(tx.Outputs))
		for vOut, txOut := range tx.Outputs {
			//nolint:gosec
			utxoHashes[vOut], err = util.UTXOHashFromOutput(tx.TxIDChainHash(), txOut, uint32(vOut))
			require.NoError(t, err)

			//nolint:gosec
			keySource := uaerospike.CalculateKeySource(tx.TxIDChainHash(), uint32(vOut), store.GetUtxoBatchSize())
			key, err := aerospike.NewKey(store.GetNamespace(), store.GetName(), keySource)
			require.NoError(t, err)

			// check we created 5 records in aerospike properly
			resp, err := client.Get(nil, key)
			require.NoError(t, err)

			// We have a batch limit of 1 utxo per record.  Vout 0 is record 0 (the parent) and will have a totalUtxos of 5.
			// All other records do not have a totalUtxos field.
			if vOut == 0 {
				assert.Equal(t, 5, resp.Bins[fields.TotalUtxos.String()])
			} else {
				_, ok := resp.Bins[fields.TotalUtxos.String()]
				require.False(t, ok)
			}

			assert.Equal(t, 1, resp.Bins[fields.RecordUtxos.String()])

			if vOut == 0 {
				assert.Equal(t, true, resp.Bins[fields.External.String()])
				assert.Equal(t, 4, resp.Bins[fields.TotalExtraRecs.String()])
			} else {
				_, ok := resp.Bins[fields.External.String()]
				require.False(t, ok)
			}
		}

		// check we created the tx in the external store
		exists, err := store.GetExternalStore().Exists(ctx, tx.TxIDChainHash().CloneBytes(), fileformat.FileTypeTx)
		require.NoError(t, err)
		require.True(t, exists)

		// DAH is now managed centrally by pruner service, not by blob stores
		// External store DAH checks removed

		keySource := uaerospike.CalculateKeySource(tx.TxIDChainHash(), uint32(0), 1)
		mainRecordKey, err := aerospike.NewKey(store.GetNamespace(), store.GetName(), keySource)
		require.NoError(t, err)

		// spend 1,2,3,4
		_, err = store.Spend(ctx, spendTxRemaining, store.GetBlockHeight()+1)
		require.NoError(t, err)

		// get totalExtraRecs from main record
		resp, err = client.Get(nil, mainRecordKey)
		require.NoError(t, err)

		// assert that the record is not yet marked for DAH
		assert.Nil(t, resp.Bins[fields.DeleteAtHeight.String()])
		assert.Equal(t, 4, resp.Bins[fields.TotalExtraRecs.String()])

		// spend 0
		_, err = store.Spend(ctx, spendTx, store.GetBlockHeight()+1)
		require.NoError(t, err)

		resp, err = client.Get(nil, mainRecordKey)
		require.NoError(t, err)

		// main record check
		assert.Greater(t, resp.Bins[fields.DeleteAtHeight.String()], 0)
		assert.Equal(t, 4, resp.Bins[fields.TotalExtraRecs.String()])

		// DAH is now managed centrally by pruner service, not by blob stores
		// External file lifecycle is managed by the pruner service
	})
}

func TestStore_Unspend(t *testing.T) {
	logger := ulogger.NewErrorTestLogger(t)
	tSettings := test.CreateBaseTestSettings(t)

	client, store, ctx, deferFn := initAerospike(t, tSettings, logger)

	t.Cleanup(func() {
		deferFn()
	})

	t.Run("Successfully unspend a spent tx", func(t *testing.T) {
		// Clean up any existing data
		_ = store.GetExternalStore().Del(ctx, tx.TxIDChainHash().CloneBytes(), fileformat.FileTypeTx)

		// Create a tx
		_, err := store.Create(ctx, tx, 101)
		require.NoError(t, err)

		// Spend the tx
		spends, err := store.Spend(ctx, spendTx, store.GetBlockHeight()+1)
		require.NoError(t, err)
		require.Len(t, spends, 1)

		// Unspend the tx
		err = store.Unspend(ctx, spends)
		require.NoError(t, err)

		// Verify we can now spend it again with a different tx
		spends, err = store.Spend(ctx, spendTx2, store.GetBlockHeight()+1)
		require.NoError(t, err)
		require.Len(t, spends, 1)
	})

	t.Run("Unspend a non-spent tx", func(t *testing.T) {
		// Clean up the database
		cleanDB(t, client)

		// Clean up any existing data
		_ = store.GetExternalStore().Del(ctx, tx.TxIDChainHash().CloneBytes(), fileformat.FileTypeTx)

		// Create a tx
		_, err := store.Create(ctx, tx, 101)
		require.NoError(t, err)

		utxoHash, err := util.UTXOHashFromOutput(
			tx.TxIDChainHash(),
			tx.Outputs[0],
			0,
		)
		require.NoError(t, err)

		// Try to unspend a tx that hasn't been spent
		err = store.Unspend(ctx, []*utxo.Spend{
			{
				TxID:     tx.TxIDChainHash(),
				Vout:     0,
				UTXOHash: utxoHash,
			},
		})
		require.NoError(t, err)

		// Verify we can still spend it
		spends, err := store.Spend(ctx, spendTx, store.GetBlockHeight()+1)
		require.NoError(t, err)
		require.Len(t, spends, 1)
	})
}
