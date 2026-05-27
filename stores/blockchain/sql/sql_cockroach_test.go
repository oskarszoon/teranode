//go:build cockroach

package sql

import (
	"context"
	"net/url"
	"testing"

	tctc "github.com/bsv-blockchain/teranode/test/testcontainers/crdb"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/bsv-blockchain/teranode/util/usql"
	"github.com/stretchr/testify/require"
)

// TestCockroach_BlockchainSchemaInit verifies that the blockchain SQL store
// boots cleanly against a real CockroachDB instance: engine detection
// identifies CRDB, the Postgres schema-init path runs, and the post-init
// verifyBlockchainSchema (Task 8) accepts every expected column on the bare
// CREATE TABLE definitions emitted on Cockroach.
func TestCockroach_BlockchainSchemaInit(t *testing.T) {
	ctx := context.Background()
	connURL := tctc.StartCockroach(t, ctx, "teranode_blockchain_test")

	logger := ulogger.TestLogger{}
	tSettings := test.CreateBaseTestSettings(t)
	parsed, err := url.Parse(connURL)
	require.NoError(t, err)

	db, err := util.InitSQLDB(logger, parsed, tSettings)
	require.NoError(t, err)
	defer db.Close()

	require.Equal(t, usql.EngineCockroach, db.Engine(), "expected Cockroach engine detection")

	store, err := New(logger, parsed, tSettings)
	require.NoError(t, err)
	require.NotNil(t, store)
	defer store.Close()
}

// TestCockroach_BlockchainRoundtrip exercises the Postgres-like store/fetch
// paths end-to-end against CRDB: StoreBlock writes block1, then GetBlockByID
// reads it back and confirms the hash matches. This proves the Postgres
// branches of StoreBlock + GetBlockByID work on Cockroach.
func TestCockroach_BlockchainRoundtrip(t *testing.T) {
	ctx := context.Background()
	connURL := tctc.StartCockroach(t, ctx, "teranode_blockchain_rt_test")

	logger := ulogger.TestLogger{}
	tSettings := test.CreateBaseTestSettings(t)
	parsed, err := url.Parse(connURL)
	require.NoError(t, err)

	store, err := New(logger, parsed, tSettings)
	require.NoError(t, err)
	defer store.Close()

	id, height, err := store.StoreBlock(ctx, block1, "")
	require.NoError(t, err)
	require.Greater(t, id, uint64(0))
	require.Equal(t, uint32(1), height)

	retrieved, err := store.GetBlockByID(ctx, id)
	require.NoError(t, err)
	require.Equal(t, block1.Hash().String(), retrieved.Hash().String())

	// Exercise GetNextBlockID, which queries pg_get_serial_sequence — only
	// returns a non-NULL sequence on CRDB if serial_normalization='sql_sequence'
	// was active when the blocks table was created. Guards against future
	// reordering of the AfterConnect hook relative to schema init.
	nextID, err := store.GetNextBlockID(ctx)
	require.NoError(t, err)
	require.Greater(t, nextID, uint64(0))
}
