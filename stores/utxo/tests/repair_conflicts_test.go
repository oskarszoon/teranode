// Package tests provides tests for the RepairConflictingChains function.
package tests

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// setupSQLiteFileStore creates a file-based SQLite store using t.TempDir().
// File SQLite uses WAL mode which allows concurrent reads and writes —
// required by RepairConflictingChains because SetConflicting issues writes
// while other queries are running on separate connections.
func setupSQLiteFileStore(ctx context.Context, t *testing.T) utxo.Store {
	t.Helper()
	logger := ulogger.TestLogger{}
	tSettings := test.CreateBaseTestSettings(t)
	tSettings.UtxoStore.DBTimeout = 30 * time.Second

	storeURL, err := url.Parse("sqlite:///repair-test")
	require.NoError(t, err)

	store, err := sql.New(ctx, logger, tSettings, storeURL)
	require.NoError(t, err)

	return store
}

// testBlockchainQuerier is a local implementation of utxo.BlockchainQuerier for testing.
type testBlockchainQuerier struct {
	bestBlockHash   *chainhash.Hash
	bestBlockHeight uint32
	blockHeaderIDs  []uint32
}

func (tbq *testBlockchainQuerier) GetBestBlockHeaderInfo(ctx context.Context) (utxo.BlockHeaderInfo, error) {
	return utxo.BlockHeaderInfo{Hash: tbq.bestBlockHash, Height: tbq.bestBlockHeight}, nil
}

func (tbq *testBlockchainQuerier) GetBlockHeaderIDs(ctx context.Context, blockHash *chainhash.Hash, numberOfHeaders uint64) ([]uint32, error) {
	return tbq.blockHeaderIDs, nil
}

// newQuerier creates a simple querier with no blocks on the best chain.
func newQuerier() *testBlockchainQuerier {
	h := &chainhash.Hash{}
	return &testBlockchainQuerier{
		bestBlockHash:   h,
		bestBlockHeight: 0,
		blockHeaderIDs:  []uint32{},
	}
}

// makeTxSpendingOutput builds a minimal transaction that spends parentTxHash[vout].
// satoshis is used to make the output unique (and thus the txid unique).
func makeTxSpendingOutput(t *testing.T, parentTx *bt.Tx, vout uint32, satoshis uint64) *bt.Tx {
	t.Helper()
	tx := bt.NewTx()
	err := tx.From(
		parentTx.TxIDChainHash().String(),
		vout,
		parentTx.Outputs[vout].LockingScript.String(),
		uint64(parentTx.Outputs[vout].Satoshis),
	)
	require.NoError(t, err)
	tx.Inputs[0].UnlockingScript = bscript.NewFromBytes([]byte{})
	err = tx.PayToAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", satoshis)
	require.NoError(t, err)
	return tx
}

// TestRepairConflictingChains_CleanState verifies that running repair on a clean store produces zeroed report.
func TestRepairConflictingChains_CleanState(t *testing.T) {
	ctx := context.Background()
	store := setupSQLiteFileStore(ctx, t)
	querier := newQuerier()

	report, err := utxo.RepairConflictingChains(ctx, store, querier, false)
	require.NoError(t, err)
	require.Equal(t, 0, report.CaseAFixed)
	require.Equal(t, 0, report.CaseCFixed)
	require.Equal(t, 0, report.UnminedSinceFixed)
	require.Empty(t, report.Errors)
}

// TestRepairConflictingChains_CaseA_SingleLoser sets up a double-spend scenario where
// TX_B is an unmined loser (not marked Conflicting) while TX_A is the winner per SpendingData.
// Repair must detect TX_B as Case A and mark it Conflicting=true.
func TestRepairConflictingChains_CaseA_SingleLoser(t *testing.T) {
	ctx := context.Background()
	store := setupSQLiteFileStore(ctx, t)

	// Create TX_PARENT with a real, unique output.
	parentTx, err := bt.NewTxFromString("010000000000000000ef01032e38e9c0a84c6046d687d10556dcacc41d275ec55fc00779ac88fdf357a18700000000" +
		"8c493046022100c352d3dd993a981beba4a63ad15c209275ca9470abfcd57da93b58e4eb5dce82022100840792bc1f456062819f15d33ee7055cf7b5" +
		"ee1af1ebcc6028d9cdb1c3af7748014104f46db5e9d61a9dc27b8d64ad23e7383a4e6ca164593c2527c038c0857eb67ee8e825dca65046b82c933158" +
		"6c82e0fd1f633f25f87c161bc6f8a630121df2b3d3ffffffff00f2052a010000001976a91471d7dd96d9edda09180fe9d57a477b5acc9cad1188ac02" +
		"00e32321000000001976a914c398efa9c392ba6013c5e04ee729755ef7f58b3288ac000fe208010000001976a914948c765a6914d43f2a7ac177da2c" +
		"2f6b52de3d7c88ac00000000")
	require.NoError(t, err)

	_, err = store.Create(ctx, parentTx, 100)
	require.NoError(t, err)

	// TX_A spends parentTx[0] — this sets SpendingData[0] = TX_A.
	txA := makeTxSpendingOutput(t, parentTx, 0, 10000)
	_, err = store.Create(ctx, txA, 100)
	require.NoError(t, err)
	_, err = store.Spend(ctx, txA, store.GetBlockHeight()+1, utxo.IgnoreFlags{})
	require.NoError(t, err)

	// TX_B also has an input pointing to parentTx[0], but we only Create() it (no Spend).
	// This means SpendingData[0] on parentTx still points to TX_A — TX_B is the loser but
	// was never marked Conflicting (simulating the bug).
	txB := makeTxSpendingOutput(t, parentTx, 0, 20000)
	// Create with unmined_since > 0 (blockHeight 100 means it is unmined)
	_, err = store.Create(ctx, txB, 100)
	require.NoError(t, err)

	querier := newQuerier()
	report, err := utxo.RepairConflictingChains(ctx, store, querier, false)
	require.NoError(t, err)
	// Non-fatal errors may occur when parentTx's input references an external tx not in the store.
	// The repair should still detect and fix TX_B.
	require.Equal(t, 1, report.CaseAFixed, "TX_B should be detected as Case A loser")

	// TX_B must now be marked Conflicting=true.
	txBHash := txB.TxIDChainHash()
	meta, err := store.Get(ctx, txBHash, fields.Conflicting)
	require.NoError(t, err)
	require.NotNil(t, meta)
	require.True(t, meta.Conflicting, "TX_B must be Conflicting after repair")
}

// TestRepairConflictingChains_CaseA_CascadeToChildren verifies that MarkConflictingRecursively
// propagates the Conflicting mark through TX_B → TX_B_CHILD → TX_B_GRANDCHILD.
func TestRepairConflictingChains_CaseA_CascadeToChildren(t *testing.T) {
	ctx := context.Background()
	store := setupSQLiteFileStore(ctx, t)

	// TX_PARENT
	parentTx, err := bt.NewTxFromString("010000000000000000ef01032e38e9c0a84c6046d687d10556dcacc41d275ec55fc00779ac88fdf357a18700000000" +
		"8c493046022100c352d3dd993a981beba4a63ad15c209275ca9470abfcd57da93b58e4eb5dce82022100840792bc1f456062819f15d33ee7055cf7b5" +
		"ee1af1ebcc6028d9cdb1c3af7748014104f46db5e9d61a9dc27b8d64ad23e7383a4e6ca164593c2527c038c0857eb67ee8e825dca65046b82c933158" +
		"6c82e0fd1f633f25f87c161bc6f8a630121df2b3d3ffffffff00f2052a010000001976a91471d7dd96d9edda09180fe9d57a477b5acc9cad1188ac02" +
		"00e32321000000001976a914c398efa9c392ba6013c5e04ee729755ef7f58b3288ac000fe208010000001976a914948c765a6914d43f2a7ac177da2c" +
		"2f6b52de3d7c88ac00000000")
	require.NoError(t, err)
	_, err = store.Create(ctx, parentTx, 100)
	require.NoError(t, err)

	// TX_A wins parentTx[0].
	txA := makeTxSpendingOutput(t, parentTx, 0, 10000)
	_, err = store.Create(ctx, txA, 100)
	require.NoError(t, err)
	_, err = store.Spend(ctx, txA, store.GetBlockHeight()+1, utxo.IgnoreFlags{})
	require.NoError(t, err)

	// TX_B is the loser — only Created, not Spent against parentTx[0].
	txB := makeTxSpendingOutput(t, parentTx, 0, 20000)
	_, err = store.Create(ctx, txB, 100)
	require.NoError(t, err)

	// TX_B_CHILD spends TX_B[0] — Created and Spent to establish the parent relationship.
	txBChild := makeTxSpendingOutput(t, txB, 0, 5000)
	_, err = store.Create(ctx, txBChild, 100)
	require.NoError(t, err)
	_, err = store.Spend(ctx, txBChild, store.GetBlockHeight()+1, utxo.IgnoreFlags{
		IgnoreConflicting: true,
		IgnoreLocked:      true,
	})
	require.NoError(t, err)

	// TX_B_GRANDCHILD spends TX_B_CHILD[0].
	txBGrandchild := makeTxSpendingOutput(t, txBChild, 0, 2000)
	_, err = store.Create(ctx, txBGrandchild, 100)
	require.NoError(t, err)
	_, err = store.Spend(ctx, txBGrandchild, store.GetBlockHeight()+1, utxo.IgnoreFlags{
		IgnoreConflicting: true,
		IgnoreLocked:      true,
	})
	require.NoError(t, err)

	querier := newQuerier()
	report, err := utxo.RepairConflictingChains(ctx, store, querier, false)
	require.NoError(t, err)
	// Non-fatal errors from external (unresolvable) parent inputs are expected; repair still runs.
	require.Equal(t, 1, report.CaseAFixed, "only the root loser TX_B is the Case A entry")

	// All three (TX_B, TX_B_CHILD, TX_B_GRANDCHILD) must be Conflicting.
	for _, tx := range []*bt.Tx{txB, txBChild, txBGrandchild} {
		h := tx.TxIDChainHash()
		m, gErr := store.Get(ctx, h, fields.Conflicting)
		require.NoError(t, gErr)
		require.NotNil(t, m)
		require.True(t, m.Conflicting, "tx %s must be Conflicting after cascade", h.String())
	}
}

// TestRepairConflictingChains_DryRun verifies that dryRun=true reports findings but writes nothing.
func TestRepairConflictingChains_DryRun(t *testing.T) {
	ctx := context.Background()
	store := setupSQLiteFileStore(ctx, t)

	// Same setup as CaseA_SingleLoser.
	parentTx, err := bt.NewTxFromString("010000000000000000ef01032e38e9c0a84c6046d687d10556dcacc41d275ec55fc00779ac88fdf357a18700000000" +
		"8c493046022100c352d3dd993a981beba4a63ad15c209275ca9470abfcd57da93b58e4eb5dce82022100840792bc1f456062819f15d33ee7055cf7b5" +
		"ee1af1ebcc6028d9cdb1c3af7748014104f46db5e9d61a9dc27b8d64ad23e7383a4e6ca164593c2527c038c0857eb67ee8e825dca65046b82c933158" +
		"6c82e0fd1f633f25f87c161bc6f8a630121df2b3d3ffffffff00f2052a010000001976a91471d7dd96d9edda09180fe9d57a477b5acc9cad1188ac02" +
		"00e32321000000001976a914c398efa9c392ba6013c5e04ee729755ef7f58b3288ac000fe208010000001976a914948c765a6914d43f2a7ac177da2c" +
		"2f6b52de3d7c88ac00000000")
	require.NoError(t, err)
	_, err = store.Create(ctx, parentTx, 100)
	require.NoError(t, err)

	txA := makeTxSpendingOutput(t, parentTx, 0, 10000)
	_, err = store.Create(ctx, txA, 100)
	require.NoError(t, err)
	_, err = store.Spend(ctx, txA, store.GetBlockHeight()+1, utxo.IgnoreFlags{})
	require.NoError(t, err)

	txB := makeTxSpendingOutput(t, parentTx, 0, 20000)
	_, err = store.Create(ctx, txB, 100)
	require.NoError(t, err)

	querier := newQuerier()

	// Dry run — must report CaseA but not fix.
	report, err := utxo.RepairConflictingChains(ctx, store, querier, true)
	require.NoError(t, err)
	require.Equal(t, 1, report.CaseAFixed, "dry run must still report CaseAFixed")

	// TX_B must NOT be marked Conflicting (nothing was written).
	txBHash := txB.TxIDChainHash()
	meta, err := store.Get(ctx, txBHash, fields.Conflicting)
	require.NoError(t, err)
	require.NotNil(t, meta)
	require.False(t, meta.Conflicting, "TX_B must remain non-conflicting during dry run")
}

// TestRepairConflictingChains_UnminedSinceFix verifies step 0.
// SQL store returns nil for ScanInconsistentUnminedTxs, so step 0 is a no-op.
// The test verifies no error is returned and report.UnminedSinceFixed == 0.
func TestRepairConflictingChains_UnminedSinceFix(t *testing.T) {
	ctx := context.Background()
	store := setupSQLiteMemoryStore(ctx, t)

	// Create a transaction that is mined (has block_ids) — under normal SQL operation,
	// ScanInconsistentUnminedTxs returns nil, so no unmined_since fix is attempted.
	parentTx, err := bt.NewTxFromString("010000000000000000ef01032e38e9c0a84c6046d687d10556dcacc41d275ec55fc00779ac88fdf357a18700000000" +
		"8c493046022100c352d3dd993a981beba4a63ad15c209275ca9470abfcd57da93b58e4eb5dce82022100840792bc1f456062819f15d33ee7055cf7b5" +
		"ee1af1ebcc6028d9cdb1c3af7748014104f46db5e9d61a9dc27b8d64ad23e7383a4e6ca164593c2527c038c0857eb67ee8e825dca65046b82c933158" +
		"6c82e0fd1f633f25f87c161bc6f8a630121df2b3d3ffffffff00f2052a010000001976a91471d7dd96d9edda09180fe9d57a477b5acc9cad1188ac02" +
		"00e32321000000001976a914c398efa9c392ba6013c5e04ee729755ef7f58b3288ac000fe208010000001976a914948c765a6914d43f2a7ac177da2c" +
		"2f6b52de3d7c88ac00000000")
	require.NoError(t, err)

	// Create with block info (mined).
	_, err = store.Create(ctx, parentTx, 0,
		utxo.WithMinedBlockInfo(utxo.MinedBlockInfo{BlockID: 1, BlockHeight: 1}),
	)
	require.NoError(t, err)

	querier := &testBlockchainQuerier{
		bestBlockHash:   &chainhash.Hash{},
		bestBlockHeight: 1,
		blockHeaderIDs:  []uint32{1},
	}

	report, err := utxo.RepairConflictingChains(ctx, store, querier, false)
	require.NoError(t, err)
	// SQL store returns nil from ScanInconsistentUnminedTxs — step 0 is skipped.
	require.Equal(t, 0, report.UnminedSinceFixed)
	require.Empty(t, report.Errors)
}
