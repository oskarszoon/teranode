// Package tests — TestGetCounterConflicting_CountsDanglingRef.
//
// This lives in the store-agnostic "tests" package (alongside
// getandlockchildren_test.go) rather than as an internal test in package
// utxo, because the counter it exercises must be checked against a REAL
// backing store (sqlitememory), not a mock — but any real Store
// implementation (stores/utxo/sql, stores/utxo/aerospike, ...) imports
// package utxo itself. An internal utxo test file importing such a package
// back would be a hard import cycle ("import cycle not allowed in test"),
// which Go's toolchain cannot break for same-package (internal) test
// augmentation — only for external test packages like this one.
//
// Because prometheusDanglingSpenderRef (stores/utxo/metrics_dangling.go) is
// unexported, this test reads its current value straight off the default
// Prometheus registry (promauto registers there) instead of via
// testutil.ToFloat64 on the collector directly.
package tests

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

// danglingRefFundingTx returns a real historical raw tx with multiple
// outputs, reused verbatim from getandlockchildren_test.go's fixtures so it
// is already known to satisfy the sqlitememory store's constraints.
func danglingRefFundingTx(t *testing.T) *bt.Tx {
	tx, err := bt.NewTxFromString("010000000000000000ef01032e38e9c0a84c6046d687d10556dcacc41d275ec55fc00779ac88fdf357a18700000000" +
		"8c493046022100c352d3dd993a981beba4a63ad15c209275ca9470abfcd57da93b58e4eb5dce82022100840792bc1f456062819f15d33ee7055cf7b5" +
		"ee1af1ebcc6028d9cdb1c3af7748014104f46db5e9d61a9dc27b8d64ad23e7383a4e6ca164593c2527c038c0857eb67ee8e825dca65046b82c933158" +
		"6c82e0fd1f633f25f87c161bc6f8a630121df2b3d3ffffffff00f2052a010000001976a91471d7dd96d9edda09180fe9d57a477b5acc9cad1188ac02" +
		"00e32321000000001976a914c398efa9c392ba6013c5e04ee729755ef7f58b3288ac000fe208010000001976a914948c765a6914d43f2a7ac177da2c" +
		"2f6b52de3d7c88ac00000000")
	require.NoError(t, err)

	return tx
}

// danglingRefSpendingTx builds a tx spending output vout of parent, paying a
// distinct amount to addr so that two spending txs built from the same
// parent output never collide on txid.
func danglingRefSpendingTx(t *testing.T, parent *bt.Tx, vout uint32, addr string, amount uint64) *bt.Tx {
	spendTx := bt.NewTx()

	err := spendTx.From(parent.TxIDChainHash().String(), vout, parent.Outputs[vout].LockingScript.String(), uint64(parent.Outputs[vout].Satoshis))
	require.NoError(t, err)

	err = spendTx.AddP2PKHOutputFromAddress(addr, amount)
	require.NoError(t, err)

	// A basic (empty) unlocking script satisfies the store's constraints; the
	// real validator's signature checks are not exercised by this test.
	spendTx.Inputs[0].UnlockingScript = bscript.NewFromBytes([]byte{})

	return spendTx
}

// danglingSpenderRefCounterValue reads the current value of
// teranode_utxo_dangling_spender_ref_total{site=siteLabel} straight off the
// default Prometheus registry. See package doc above for why this can't use
// testutil.ToFloat64 on the (unexported) collector directly.
func danglingSpenderRefCounterValue(t *testing.T, siteLabel string) float64 {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	for _, mf := range families {
		if mf.GetName() != "teranode_utxo_dangling_spender_ref_total" {
			continue
		}

		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "site" && lp.GetValue() == siteLabel {
					return m.GetCounter().GetValue()
				}
			}
		}
	}

	return 0
}

// TestGetCounterConflicting_CountsDanglingRef exercises the production
// dangling-spender-ref condition end to end against a real store: parent's
// output 0 is spent by childC, childC's own record is then deleted (the
// record a follow-up conflict-resolution read would need is now missing),
// and GetCounterConflictingTxHashes is asked to resolve counters for a rival
// spend of the same output. It must still surface the underlying error (no
// read-side tolerance) while bumping the detection counter for the site that
// followed the dangling reference (get_conflicting_children, reached via
// GetConflictingChildren's root Get on childC's now-missing record).
func TestGetCounterConflicting_CountsDanglingRef(t *testing.T) {
	ctx := context.Background()
	store := setupSQLiteMemoryStore(ctx, t)

	parent := danglingRefFundingTx(t)
	_, err := store.Create(ctx, parent, 1)
	require.NoError(t, err)

	childC := danglingRefSpendingTx(t, parent, 0, "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", 40000)
	_, err = store.Create(ctx, childC, 1)
	require.NoError(t, err)

	_, err = store.Spend(ctx, childC, store.GetBlockHeight()+1, utxo.IgnoreFlags{}) // parent.SpendingDatas[0] -> childC
	require.NoError(t, err)

	require.NoError(t, store.Delete(ctx, childC.TxIDChainHash())) // childC's record gone -> dangling ref

	rival := danglingRefSpendingTx(t, parent, 0, "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2", 30000) // the tx we resolve counters for
	_, err = store.Create(ctx, rival, 1)
	require.NoError(t, err)

	before := danglingSpenderRefCounterValue(t, "get_conflicting_children")

	_, gErr := utxo.GetCounterConflictingTxHashes(ctx, store, *rival.TxIDChainHash())
	require.Error(t, gErr, "reader still returns the error (no tolerance added)")

	after := danglingSpenderRefCounterValue(t, "get_conflicting_children")
	require.Equal(t, before+1, after)
}
