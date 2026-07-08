package aerospike_test

// These tests exercise the batched Unspend implementation (issue #1214 task
// 3: chunked BatchOperate of "unspend" BatchUDFs replacing the previous
// serial per-UTXO client.Execute loop) against a real Aerospike instance via
// TestContainers. They require Docker; initAerospike (container_helper_test.go)
// skips cleanly via t.Skipf when a container cannot be started, matching the
// rest of this package's Aerospike-backed tests (unspend_ownership_test.go,
// spend_rollback_test.go, duplicate_spend_test.go) — no build tag is needed
// to compile or to run under `go test ./stores/utxo/aerospike/...`.

import (
	"fmt"
	"sync"
	"testing"

	"github.com/bsv-blockchain/aerospike-client-go/v8"
	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	spendpkg "github.com/bsv-blockchain/teranode/stores/utxo/spend"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// buildDistinctParents builds n standalone, single-output transactions, each
// with its own unique dummy (unsigned, unvalidated) input so every parent
// lands on its own physical Aerospike key. This mirrors a real consolidation
// tx: its inputs are outputs of many distinct prior transactions, not
// thousands of outputs of one single parent — which is also the shape that
// avoids swamping a single Aerospike record with concurrent writes (a
// pre-existing, unrelated hot-key hazard in the forward Spend batcher when
// many outputs of the SAME heavily-paginated parent are all spent in one call).
func buildDistinctParents(t *testing.T, n int) []*bt.Tx {
	t.Helper()

	parents := make([]*bt.Tx, n)

	for i := 0; i < n; i++ {
		dummyPrevTxID := chainhash.HashH([]byte(fmt.Sprintf("dangling-spend-task3-parent-%d", i)))

		parentTx := bt.NewTx()
		require.NoError(t, parentTx.From(
			dummyPrevTxID.String(),
			0,
			"76a914000000000000000000000000000000000000000088ac",
			2000,
		))
		require.NoError(t, parentTx.PayToAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", 1000))

		parents[i] = parentTx
	}

	return parents
}

// buildManyOutputTx builds a standalone transaction with numOutputs outputs
// and a single dummy (unsigned, unvalidated) input — sufficient for the
// UTXO store, which does not run script validation on Create. Modeled on
// TestDuplicateSpendLargeTx's largeTx construction in duplicate_spend_test.go.
func buildManyOutputTx(t *testing.T, numOutputs int) *bt.Tx {
	t.Helper()

	largeTx := bt.NewTx()

	err := largeTx.From(
		"2222222222222222222222222222222222222222222222222222222222222222",
		0,
		"76a914000000000000000000000000000000000000000088ac",
		uint64(numOutputs)*1000+1000,
	)
	require.NoError(t, err)

	for i := 0; i < numOutputs; i++ {
		require.NoError(t, largeTx.PayToAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", 1000))
	}

	return largeTx
}

// buildSpendingAllOutputs builds a transaction that spends every output of
// parent (vouts 0..len(parent.Outputs)-1) in a single tx.
func buildSpendingAllOutputs(t *testing.T, parent *bt.Tx) *bt.Tx {
	t.Helper()

	txID := parent.TxIDChainHash()

	spendingTx := bt.NewTx()
	for i := range parent.Outputs {
		require.NoError(t, spendingTx.From(
			txID.String(),
			uint32(i), // nolint: gosec
			parent.Outputs[i].LockingScript.String(),
			parent.Outputs[i].Satoshis,
		))
	}

	require.NoError(t, spendingTx.PayToAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", uint64(len(parent.Outputs))*500))

	return spendingTx
}

// buildSpendingManyParents builds a single transaction spending vout 0 of
// every tx in parents — the "consolidation tx" shape.
func buildSpendingManyParents(t *testing.T, parents []*bt.Tx) *bt.Tx {
	t.Helper()

	childTx := bt.NewTx()
	for _, p := range parents {
		require.NoError(t, childTx.From(
			p.TxIDChainHash().String(),
			0,
			p.Outputs[0].LockingScript.String(),
			p.Outputs[0].Satoshis,
		))
	}

	require.NoError(t, childTx.PayToAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", uint64(len(parents))*500))

	return childTx
}

// TestUnspend_BatchedReversesAll_ManyDistinctKeys is the scale test matching
// the actual bug scenario (issue #1214): a consolidation-style tx spending
// > unspendBatchChunkSize (1024) UTXOs, each from a DIFFERENT prior
// transaction (a different physical Aerospike key). A single Unspend call
// covering all of them must drive at least two chunked BatchOperate
// round-trips (chunkSpends) and reverse every one of them.
func TestUnspend_BatchedReversesAll_ManyDistinctKeys(t *testing.T) {
	logger := ulogger.NewErrorTestLogger(t)
	tSettings := test.CreateBaseTestSettings(t)

	_, store, ctx, deferFn := initAerospike(t, tSettings, logger)
	t.Cleanup(deferFn)

	const numParents = 1200 // > unspendBatchChunkSize (1024): exercises chunking over 2 BatchOperate round-trips.

	parents := buildDistinctParents(t, numParents)

	const createConcurrency = 50
	sem := make(chan struct{}, createConcurrency)

	var wg sync.WaitGroup

	errCh := make(chan error, numParents)

	for _, p := range parents {
		wg.Add(1)
		sem <- struct{}{}

		go func(tx *bt.Tx) {
			defer wg.Done()
			defer func() { <-sem }()

			if _, err := store.Create(ctx, tx, 1); err != nil {
				errCh <- err
			}
		}(p)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		require.NoError(t, err)
	}

	child := buildSpendingManyParents(t, parents)

	spends, err := store.Spend(ctx, child, store.GetBlockHeight()+1)
	require.NoError(t, err)
	require.Len(t, spends, numParents)

	// The actual system under test: one Unspend call covering > 1 chunk,
	// spread across numParents distinct physical keys.
	require.NoError(t, store.Unspend(ctx, spends))

	// Spot-check a sample directly via Get (nil SpendingData == unspent)
	// before re-spending changes that state.
	for _, idx := range []int{0, 1, numParents / 2, numParents - 2, numParents - 1} {
		meta, getErr := store.Get(ctx, parents[idx].TxIDChainHash(), fields.Utxos)
		require.NoError(t, getErr)
		require.Len(t, meta.SpendingDatas, 1)
		require.Nil(t, meta.SpendingDatas[0], "parent %d's output must be unspent after batched Unspend", idx)
	}

	// Every parent's output must be freshly spendable again — if even one
	// spend had been dropped or mishandled by the batched Unspend, this
	// fresh Spend call would fail with ErrSpent for that specific input.
	respend := buildSpendingManyParents(t, parents)
	_, err = store.Spend(ctx, respend, store.GetBlockHeight()+1)
	require.NoError(t, err, "all parent outputs should be re-spendable after the batched Unspend")
}

// TestUnspend_BatchedReversesAll_Paginated verifies that a batched Unspend
// covering every output of a single, paginated (multi-record) parent still
// clears every output's SpendingData, and that the master record's
// spentExtraRecs counter is fully decremented back to zero — i.e.
// handleExtraRecords' NotAllSpent decrement still fires correctly for every
// pagination child record when driven through postProcessUnspendRecord
// instead of unspendLua's old single-item Execute loop. Kept intentionally
// small (well under any per-key concurrency contention threshold) since its
// purpose is pagination/signal parity, not scale.
func TestUnspend_BatchedReversesAll_Paginated(t *testing.T) {
	logger := ulogger.NewErrorTestLogger(t)
	tSettings := test.CreateBaseTestSettings(t)

	const utxoBatchSize = 6
	tSettings.UtxoStore.UtxoBatchSize = utxoBatchSize

	client, store, ctx, deferFn := initAerospike(t, tSettings, logger)
	t.Cleanup(deferFn)

	const numOutputs = 30 // 30/6 = 5 pagination records

	parent := buildManyOutputTx(t, numOutputs)
	parentTxID := parent.TxIDChainHash()

	_, err := store.Create(ctx, parent, 1)
	require.NoError(t, err)

	txKey, err := aerospike.NewKey(store.GetNamespace(), store.GetName(), parentTxID.CloneBytes())
	require.NoError(t, err)

	rec, err := client.Get(util.GetAerospikeReadPolicy(tSettings), txKey, "totalExtraRecs")
	require.NoError(t, err)
	require.NotNil(t, rec)

	totalExtraRecs, ok := rec.Bins["totalExtraRecs"].(int)
	require.True(t, ok)
	require.Positive(t, totalExtraRecs, "test is only meaningful with pagination child records")

	child := buildSpendingAllOutputs(t, parent)

	spends, err := store.Spend(ctx, child, store.GetBlockHeight()+1)
	require.NoError(t, err)
	require.Len(t, spends, numOutputs)

	// Sanity: every extra record should now be marked fully spent.
	rec, err = client.Get(util.GetAerospikeReadPolicy(tSettings), txKey, "totalExtraRecs", "spentExtraRecs")
	require.NoError(t, err)
	spentExtraRecsAfterSpend, ok := rec.Bins["spentExtraRecs"].(int)
	require.True(t, ok)
	require.Equal(t, totalExtraRecs, spentExtraRecsAfterSpend, "all extra records should be ALLSPENT after spending every output")

	// The actual system under test.
	require.NoError(t, store.Unspend(ctx, spends))

	meta, err := store.Get(ctx, parentTxID, fields.Utxos)
	require.NoError(t, err)
	require.Len(t, meta.SpendingDatas, numOutputs)

	for i, sd := range meta.SpendingDatas {
		require.Nil(t, sd, "output %d must be unspent after batched Unspend", i)
	}

	// The NotAllSpent decrement must have fired for every child record that
	// transitioned from ALLSPENT back to NOTALLSPENT — spentExtraRecs should
	// be back down to zero. Losing this decrement (e.g. if the batched
	// rewrite dropped the handleExtraRecords call in postProcessUnspendRecord)
	// would corrupt this counter and leave it stuck at totalExtraRecs.
	rec, err = client.Get(util.GetAerospikeReadPolicy(tSettings), txKey, "spentExtraRecs")
	require.NoError(t, err)
	spentExtraRecsAfterUnspend, ok := rec.Bins["spentExtraRecs"].(int)
	require.True(t, ok)
	require.Zero(t, spentExtraRecsAfterUnspend, "spentExtraRecs must be fully decremented after reversing every spend")

	// Every output must be freshly spendable again.
	respend := buildSpendingAllOutputs(t, parent)
	_, err = store.Spend(ctx, respend, store.GetBlockHeight()+1)
	require.NoError(t, err, "all outputs should be re-spendable after the batched Unspend")
}

// TestUnspend_OwnershipMismatchIsNoOp verifies that within a single batched
// Unspend call mixing legitimate reversals with an ownership mismatch, the
// mismatched entry is a silent no-op (per the Lua UDF's ownership check,
// unchanged by this rework) while the legitimate entries are still cleared —
// i.e. per-record post-processing is preserved independently for each
// record in the batch, not collapsed into an all-or-nothing outcome.
// Mirrors unspend_ownership_test.go's single-item assertions, extended to a
// multi-item batched call.
func TestUnspend_OwnershipMismatchIsNoOp(t *testing.T) {
	logger := ulogger.NewErrorTestLogger(t)
	tSettings := test.CreateBaseTestSettings(t)

	_, store, ctx, deferFn := initAerospike(t, tSettings, logger)
	t.Cleanup(deferFn)

	parent := buildManyOutputTx(t, 5)
	parentTxID := parent.TxIDChainHash()

	_, err := store.Create(ctx, parent, 1)
	require.NoError(t, err)

	child := buildSpendingAllOutputs(t, parent)

	spends, err := store.Spend(ctx, child, store.GetBlockHeight()+1)
	require.NoError(t, err)
	require.Len(t, spends, 5)

	// Corrupt vout 2's SpendingData so it no longer matches what's stored -
	// the batched call must treat this single record as a no-op while still
	// reversing the other four.
	wrongTxHash := chainhash.HashH([]byte("wrong-spender-in-batch"))
	mixed := make([]*utxo.Spend, len(spends))
	copy(mixed, spends)
	mixed[2] = &utxo.Spend{
		TxID:         spends[2].TxID,
		Vout:         spends[2].Vout,
		UTXOHash:     spends[2].UTXOHash,
		SpendingData: spendpkg.NewSpendingData(&wrongTxHash, 0),
	}

	err = store.Unspend(ctx, mixed)
	require.NoError(t, err, "a single ownership mismatch inside a batch must not fail the whole batch")

	meta, err := store.Get(ctx, parentTxID, fields.Utxos)
	require.NoError(t, err)
	require.Len(t, meta.SpendingDatas, 5)

	for i, sd := range meta.SpendingDatas {
		if i == 2 {
			require.NotNil(t, sd, "mismatched output must remain spent")
			require.True(t, sd.TxID.IsEqual(child.TxIDChainHash()), "the original spender's data must be untouched")

			continue
		}

		require.Nil(t, sd, "output %d must be unspent after batched Unspend", i)
	}
}
