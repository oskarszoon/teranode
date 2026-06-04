package aerospike_test

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/aerospike-client-go/v8"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	teranode_aerospike "github.com/bsv-blockchain/teranode/stores/utxo/aerospike"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/bsv-blockchain/teranode/util/uaerospike"
	"github.com/stretchr/testify/require"
)

// recordLocked reads the `locked` flag of a single Aerospike record (master =
// index 0, pagination/extra records = 1..N) for the given transaction.
func recordLocked(t *testing.T, client *uaerospike.Client, store *teranode_aerospike.Store, txHash *chainhash.Hash, recordIdx uint32) bool {
	t.Helper()

	keySource := uaerospike.CalculateKeySourceInternal(txHash, recordIdx)

	key, err := aerospike.NewKey(store.GetNamespace(), store.GetName(), keySource)
	require.NoError(t, err)

	rec, err := client.Get(nil, key)
	require.NoError(t, err)
	require.NotNil(t, rec, "record %d for %s should exist", recordIdx, txHash.String())

	v, ok := rec.Bins[fields.Locked.String()]
	if !ok || v == nil {
		return false // absent ⇒ not locked
	}

	switch val := v.(type) {
	case bool:
		return val
	case int:
		return val != 0
	default:
		t.Fatalf("unexpected type %T for locked bin", v)
		return false
	}
}

// totalExtraRecs reads the master record's pagination-record count.
func totalExtraRecs(t *testing.T, client *uaerospike.Client, store *teranode_aerospike.Store, txHash *chainhash.Hash) int {
	t.Helper()

	key, err := aerospike.NewKey(store.GetNamespace(), store.GetName(), uaerospike.CalculateKeySourceInternal(txHash, 0))
	require.NoError(t, err)

	rec, err := client.Get(nil, key)
	require.NoError(t, err)
	require.NotNil(t, rec)

	v, ok := rec.Bins[fields.TotalExtraRecs.String()]
	if !ok || v == nil {
		return 0
	}

	n, ok := v.(int)
	require.True(t, ok, "totalExtraRecs should be an int, got %T", v)

	return n
}

// TestSetMinedMultiClearsLockOnPaginationRecords is the regression test for
// issue #1037: a fresh legacy IBD wedged forever with TX_LOCKED because the
// pagination/extra records of an external transaction kept the `locked` flag
// that was set at create time (WithLocked(true)). SetMinedMulti only ever cleared
// the lock on the MASTER record, so a child spending an output that lived on a
// pagination record (vout >= utxoBatchSize) failed permanently with TX_LOCKED.
//
// Mining a transaction makes ALL of its outputs spendable, so after SetMinedMulti
// every one of its records — master AND pagination — must be unlocked. The test
// runs for both the Lua UDF path and the filter-expression path.
func TestSetMinedMultiClearsLockOnPaginationRecords(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping Aerospike integration test in short mode")
	}

	for _, useExpressions := range []bool{false, true} {
		name := "lua"
		if useExpressions {
			name = "expressions"
		}

		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			logger := ulogger.NewErrorTestLogger(t)
			settings := test.CreateBaseTestSettings(t)

			// Small batch size so a modest tx spans multiple records (master + extras).
			settings.UtxoStore.UtxoBatchSize = 4
			settings.Aerospike.EnableSetMinedFilterExpressions = useExpressions

			client, store, _, cleanup := initAerospike(t, settings, logger)
			defer cleanup()

			cleanDB(t, client)

			const blockHeight = 604315

			// 10 outputs / batch 4 => master (4) + 2 pagination records (4, 2).
			tx := createTransactionWithOutputs(10)

			// Create the tx LOCKED — exactly how the quick-validate pipeline and the
			// validator 2PC create it (WithLocked(true) is applied to every record).
			_, err := store.Create(ctx, tx, blockHeight, utxo.WithLocked(true))
			require.NoError(t, err)

			txHash := tx.TxIDChainHash()

			extras := totalExtraRecs(t, client, store, txHash)
			require.GreaterOrEqual(t, extras, 1, "test requires a paginated tx (at least one extra record)")

			// Precondition: every record starts locked.
			require.True(t, recordLocked(t, client, store, txHash, 0), "master must start locked")
			for i := uint32(1); i <= uint32(extras); i++ {
				require.Truef(t, recordLocked(t, client, store, txHash, i), "extra record %d must start locked", i)
			}

			// Mine it. This is the documented "mined ⇒ spendable" safety net.
			_, err = store.SetMinedMulti(ctx, []*chainhash.Hash{txHash}, utxo.MinedBlockInfo{
				BlockID:        1,
				BlockHeight:    blockHeight,
				SubtreeIdx:     0,
				OnLongestChain: true,
			})
			require.NoError(t, err)

			// The master was already unlocked by the master-keyed setMined before
			// this fix; the pagination records are what regressed.
			require.False(t, recordLocked(t, client, store, txHash, 0), "master must be unlocked after mining")
			for i := uint32(1); i <= uint32(extras); i++ {
				require.Falsef(t, recordLocked(t, client, store, txHash, i),
					"pagination record %d must be unlocked after mining (issue #1037)", i)
			}
		})
	}
}

// TestSetMinedMultiWithExpressionsClearsLockWhenBlockIDFilteredOut covers the
// filter-gating bug in the expression path: SetMinedMultiWithExpressions gates the
// whole batch write (including the Locked=false op) behind a "blockID not already
// present" filter. When the blockID is already in the record's list the write is
// FILTERED_OUT and the lock-clear is skipped. A mined transaction must be
// spendable regardless, so the master lock must be cleared even on the filtered
// path.
func TestSetMinedMultiWithExpressionsClearsLockWhenBlockIDFilteredOut(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping Aerospike integration test in short mode")
	}

	ctx := context.Background()
	logger := ulogger.NewErrorTestLogger(t)
	settings := test.CreateBaseTestSettings(t)
	settings.Aerospike.EnableSetMinedFilterExpressions = true

	client, store, _, cleanup := initAerospike(t, settings, logger)
	defer cleanup()

	cleanDB(t, client)

	const blockHeight = 604315
	const blockID = 7

	tx := createTransactionWithOutputs(2) // single (master) record is enough here
	txHash := tx.TxIDChainHash()

	_, err := store.Create(ctx, tx, blockHeight, utxo.WithLocked(true))
	require.NoError(t, err)

	// First mine: blockID not yet present => filter passes => lock cleared, blockID added.
	_, err = store.SetMinedMulti(ctx, []*chainhash.Hash{txHash}, utxo.MinedBlockInfo{
		BlockID: blockID, BlockHeight: blockHeight, OnLongestChain: true,
	})
	require.NoError(t, err)
	require.False(t, recordLocked(t, client, store, txHash, 0), "master unlocked after first mine")

	// Re-lock just the master, simulating an orphaned lock that survived while the
	// record already carries this blockID.
	masterKey, err := aerospike.NewKey(store.GetNamespace(), store.GetName(), uaerospike.CalculateKeySourceInternal(txHash, 0))
	require.NoError(t, err)
	require.NoError(t, client.PutBins(nil, masterKey, aerospike.NewBin(fields.Locked.String(), true)))
	require.True(t, recordLocked(t, client, store, txHash, 0), "master re-locked")

	// Second mine with the SAME blockID: the durable list already contains it, so
	// the main write is FILTERED_OUT. The lock-clear must still happen.
	_, err = store.SetMinedMulti(ctx, []*chainhash.Hash{txHash}, utxo.MinedBlockInfo{
		BlockID: blockID, BlockHeight: blockHeight, OnLongestChain: true,
	})
	require.NoError(t, err)
	require.False(t, recordLocked(t, client, store, txHash, 0),
		"master must be unlocked even when the blockID filter excludes the write (issue #1037)")
}
