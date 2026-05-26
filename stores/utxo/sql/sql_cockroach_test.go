//go:build cockroach

package sql

import (
	"context"
	"net/url"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/teranode/stores/utxo/tests"
	tctc "github.com/bsv-blockchain/teranode/test/testcontainers/crdb"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/bsv-blockchain/teranode/util/usql"
	"github.com/stretchr/testify/require"
)

// TestCockroach_SchemaInit verifies that the UTXO store boots cleanly against
// a real CockroachDB instance: engine detection identifies CRDB, the Postgres
// schema-init path runs without the pg_advisory_lock / DO $$ statements, and
// post-init verifyUTXOSchema accepts every expected column.
func TestCockroach_SchemaInit(t *testing.T) {
	ctx := context.Background()
	connURL := tctc.StartCockroach(t, ctx, "teranode_utxo_test")

	logger := ulogger.TestLogger{}
	tSettings := test.CreateBaseTestSettings(t)
	parsed, err := url.Parse(connURL)
	require.NoError(t, err)

	db, err := util.InitSQLDB(logger, parsed, tSettings)
	require.NoError(t, err)
	defer db.Close()

	require.Equal(t, usql.EngineCockroach, db.Engine(), "expected Cockroach engine detection")

	store, err := New(ctx, logger, tSettings, parsed)
	require.NoError(t, err)
	require.NotNil(t, store)
}

// TestCockroach_VerifySchemaCatchesMissingColumn pre-creates a deliberately
// incomplete transactions table, then calls verifyUTXOSchema directly. The
// verification must report the missing columns rather than silently passing.
// This guards against schema drift where a future migration forgets to mirror
// a column add on CockroachDB's bare CREATE TABLE definition.
func TestCockroach_VerifySchemaCatchesMissingColumn(t *testing.T) {
	ctx := context.Background()
	connURL := tctc.StartCockroach(t, ctx, "teranode_utxo_verify_test")

	logger := ulogger.TestLogger{}
	tSettings := test.CreateBaseTestSettings(t)
	parsed, err := url.Parse(connURL)
	require.NoError(t, err)

	db, err := util.InitSQLDB(logger, parsed, tSettings)
	require.NoError(t, err)
	defer db.Close()

	// Pre-create a deliberately incomplete `transactions` table to simulate
	// drift. The store's New() is intentionally not called here — we want
	// verifyUTXOSchema to see this stub table, not the full schema.
	_, err = db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS transactions (id BIGSERIAL PRIMARY KEY)")
	require.NoError(t, err)

	err = verifyUTXOSchema(ctx, db, usql.EngineCockroach)
	require.Error(t, err, "expected verification to fail when columns are missing")
}

// TestCockroach_SpendRoundtrip exercises the Postgres-like fast paths
// (isPostgresLike branches) end-to-end against CRDB: Create a parent tx,
// Create a child tx that references the parent, Spend one of the child's
// outputs, then read back the outputs row to confirm spending_data is set.
func TestCockroach_SpendRoundtrip(t *testing.T) {
	ctx := context.Background()
	connURL := tctc.StartCockroach(t, ctx, "teranode_utxo_spend_test")

	logger := ulogger.TestLogger{}
	tSettings := test.CreateBaseTestSettings(t)

	parsed, err := url.Parse(connURL)
	require.NoError(t, err)

	store, err := New(ctx, logger, tSettings, parsed)
	require.NoError(t, err)

	err = store.SetBlockHeight(1000)
	require.NoError(t, err)

	// Parent first — tests.Tx references tests.ParentTx as input.
	_, err = store.Create(ctx, tests.ParentTx, 999)
	require.NoError(t, err)
	defer func() { _ = store.Delete(ctx, tests.ParentTx.TxIDChainHash()) }()

	_, err = store.Create(ctx, tests.Tx, 1000)
	require.NoError(t, err)
	defer func() { _ = store.Delete(ctx, tests.Tx.TxIDChainHash()) }()

	txHash := *tests.Tx.TxIDChainHash()

	// Build a spend tx that consumes output 0 of tests.Tx.
	spendTx := bt.NewTx()
	require.NoError(t, spendTx.From(
		txHash.String(), 0,
		tests.Tx.Outputs[0].LockingScript.String(),
		tests.Tx.Outputs[0].Satoshis,
	))
	require.NoError(t, spendTx.PayToAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", 1000))

	_, err = store.Spend(ctx, spendTx, 1001)
	require.NoError(t, err)

	// Verify the output row is marked spent. We don't decode spending_data —
	// non-NULL is sufficient to confirm the Postgres-like UPDATE path ran on
	// Cockroach.
	var spendingData []byte
	err = store.db.QueryRowContext(ctx,
		`SELECT spending_data FROM outputs o
		 JOIN transactions t ON o.transaction_id = t.id
		 WHERE t.hash = $1 AND o.idx = 0`,
		txHash[:]).Scan(&spendingData)
	require.NoError(t, err)
	require.NotNil(t, spendingData, "outputs.spending_data should be set after Spend")
	require.NotEmpty(t, spendingData, "outputs.spending_data should be non-empty after Spend")
}
