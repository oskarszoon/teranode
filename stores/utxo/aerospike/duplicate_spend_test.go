package aerospike_test

import (
	"testing"

	"github.com/aerospike/aerospike-client-go/v8"
	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDuplicateSpendLargeTx tests that duplicate spend attempts on a transaction
// with more than utxoBatchSize outputs don't cause errors or panics.
func TestDuplicateSpendLargeTx(t *testing.T) {
	batchSize := 2
	numOutputs := 10

	logger := ulogger.NewErrorTestLogger(t)
	tSettings := test.CreateBaseTestSettings(t)
	// Set batch size to 128 as in production
	tSettings.UtxoStore.UtxoBatchSize = batchSize

	client, store, ctx, deferFn := initAerospike(t, tSettings, logger)
	t.Cleanup(func() {
		deferFn()
	})

	// Build a transaction with many outputs
	largeTx := bt.NewTx()

	// Add a dummy input - use the From method which is the public API
	// Use a non-zero hash to avoid being treated as coinbase
	err := largeTx.From(
		"1111111111111111111111111111111111111111111111111111111111111111",
		0,
		"76a914000000000000000000000000000000000000000088ac", // dummy script
		uint64(numOutputs*1000+1000),                         // enough satoshis for all outputs plus fee
	)
	require.NoError(t, err)

	// Add many outputs using PayToAddress
	for i := 0; i < numOutputs; i++ {
		err = largeTx.PayToAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", 1000)
		require.NoError(t, err)
	}

	txID := largeTx.TxIDChainHash()

	// Store the transaction with UTXOs
	_, err = store.Create(ctx, largeTx, 1)
	require.NoError(t, err)

	// Verify the transaction was stored correctly
	txKey, err := aerospike.NewKey(store.GetNamespace(), store.GetName(), txID.CloneBytes())
	require.NoError(t, err)

	rec, err := client.Get(util.GetAerospikeReadPolicy(tSettings), txKey, "totalExtraRecs")
	require.NoError(t, err)
	require.NotNil(t, rec)

	totalExtraRecs, ok := rec.Bins["totalExtraRecs"].(int)
	require.True(t, ok)
	// With numOutputs outputs and batch size 2, we should have (numOutputs/batchSize)-1 extra records
	expectedExtraRecs := (numOutputs / batchSize) - 1
	assert.Equal(t, expectedExtraRecs, totalExtraRecs)

	// Create a spending transaction that spends all outputs
	spendingTx := bt.NewTx()

	// Add all outputs as inputs to the spending transaction
	for i := 0; i < numOutputs; i++ {
		err = spendingTx.From(
			txID.String(),
			uint32(i),
			largeTx.Outputs[i].LockingScript.String(),
			largeTx.Outputs[i].Satoshis,
		)
		require.NoError(t, err)
	}

	// Add a single output to the spending transaction
	err = spendingTx.PayToAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", uint64(numOutputs*1000-500))
	require.NoError(t, err)

	// First spend attempt - should succeed
	spends, err := store.Spend(ctx, spendingTx, store.GetBlockHeight()+1)
	require.NoError(t, err, "Failed on first spend attempt")
	require.NotNil(t, spends)
	require.Len(t, spends, numOutputs)

	// CRITICAL TEST: Attempt to spend the same outputs again with the same spending transaction
	// This should NOT cause errors or panics (idempotent behavior)
	spends2, err := store.Spend(ctx, spendingTx, store.GetBlockHeight()+1)
	require.NoError(t, err, "Failed on duplicate spend attempt")
	require.NotNil(t, spends2)
	require.Len(t, spends2, numOutputs)

	// Try a third time to be really sure
	spends3, err := store.Spend(ctx, spendingTx, store.GetBlockHeight()+1)
	require.NoError(t, err, "Failed on third spend attempt")
	require.NotNil(t, spends3)
	require.Len(t, spends3, numOutputs)
}

// TestSpendUnspendRespendLargeTx verifies that spend -> unspend -> re-spend cycles
// work correctly for transactions with outputs spanning multiple child records.
// This reproduces the production failure where partial block validation rollbacks
// (Spend at spend.go:381-385) trigger Unspend followed by re-spend on retry.
func TestSpendUnspendRespendLargeTx(t *testing.T) {
	batchSize := 2
	numOutputs := 10

	logger := ulogger.NewErrorTestLogger(t)
	tSettings := test.CreateBaseTestSettings(t)
	tSettings.UtxoStore.UtxoBatchSize = batchSize

	client, store, ctx, deferFn := initAerospike(t, tSettings, logger)
	t.Cleanup(func() {
		deferFn()
	})

	// Build a transaction with outputs spanning multiple child records
	largeTx := bt.NewTx()
	err := largeTx.From(
		"1111111111111111111111111111111111111111111111111111111111111111",
		0,
		"76a914000000000000000000000000000000000000000088ac",
		uint64(numOutputs*1000+1000),
	)
	require.NoError(t, err)

	for i := 0; i < numOutputs; i++ {
		err = largeTx.PayToAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", 1000)
		require.NoError(t, err)
	}

	txID := largeTx.TxIDChainHash()

	_, err = store.Create(ctx, largeTx, 1)
	require.NoError(t, err)

	// Verify initial state
	txKey, err := aerospike.NewKey(store.GetNamespace(), store.GetName(), txID.CloneBytes())
	require.NoError(t, err)

	rec, err := client.Get(util.GetAerospikeReadPolicy(tSettings), txKey, "totalExtraRecs")
	require.NoError(t, err)
	require.NotNil(t, rec)

	totalExtraRecs, ok := rec.Bins["totalExtraRecs"].(int)
	require.True(t, ok)
	expectedExtraRecs := (numOutputs / batchSize) - 1
	require.Equal(t, expectedExtraRecs, totalExtraRecs)

	// Build spending transaction
	spendingTx := bt.NewTx()
	for i := 0; i < numOutputs; i++ {
		err = spendingTx.From(
			txID.String(),
			uint32(i),
			largeTx.Outputs[i].LockingScript.String(),
			largeTx.Outputs[i].Satoshis,
		)
		require.NoError(t, err)
	}
	err = spendingTx.PayToAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", uint64(numOutputs*1000-500))
	require.NoError(t, err)

	// --- Step 1: Spend all UTXOs ---
	spends, err := store.Spend(ctx, spendingTx, store.GetBlockHeight()+1)
	require.NoError(t, err, "First spend failed")
	require.Len(t, spends, numOutputs)

	// --- Step 2: Unspend all UTXOs ---
	// Build the unspend list from the spend results (same as Spend rollback does)
	unspends := make([]*utxo.Spend, len(spends))
	copy(unspends, spends)

	err = store.Unspend(ctx, unspends)
	require.NoError(t, err, "Unspend failed")

	// --- Step 3: Re-spend all UTXOs ---
	// This is the critical step: verifies that spend -> unspend -> re-spend
	// cycles complete without errors or panics.
	spends2, err := store.Spend(ctx, spendingTx, store.GetBlockHeight()+1)
	require.NoError(t, err, "Re-spend failed")
	require.Len(t, spends2, numOutputs)
}
