package aerospike

import (
	"context"
	"testing"
	"time"

	"github.com/bsv-blockchain/aerospike-client-go/v8"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// These are container-free unit tests for the batched Unspend's non-I/O branches
// (chunking, context abort, nil-spend skip, and per-record response handling).
// The happy path is exercised by the container-backed tests in un_spend_batch_test.go.

func TestChunkSpends_SizeZeroTreatsAsSingleChunk(t *testing.T) {
	spends := make([]*utxo.Spend, 5)
	// size <= 0 falls back to len(spends): the whole slice becomes one chunk.
	chunks := chunkSpends(spends, 0)
	require.Len(t, chunks, 1)
	require.Len(t, chunks[0], 5)

	require.Empty(t, chunkSpends(nil, 0))
}

func TestUnspend_ContextAbortReturnsError(t *testing.T) {
	s := &Store{
		settings: test.CreateBaseTestSettings(t),
		logger:   ulogger.NewErrorTestLogger(t),
	}
	spends := []*utxo.Spend{{TxID: &chainhash.Hash{}}} // non-empty so the loop reaches the ctx check

	// Cancelled context.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, s.unspend(cancelledCtx, spends), "cancelled context must abort before any batch op")

	// Deadline already exceeded.
	deadlineCtx, cancel2 := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancel2()
	require.Error(t, s.unspend(deadlineCtx, spends), "exceeded deadline must abort")
}

func TestUnspend_NilSpendSkippedNoError(t *testing.T) {
	s := &Store{
		settings: test.CreateBaseTestSettings(t),
		logger:   ulogger.NewErrorTestLogger(t),
	}
	// A nil entry is skipped; with no real records the batch is empty and the
	// call returns nil without touching the client.
	require.NoError(t, s.unspend(context.Background(), []*utxo.Spend{nil}))
}

func TestPostProcessUnspendRecord_Branches(t *testing.T) {
	InitPrometheusMetrics()
	s := &Store{}
	spend := &utxo.Spend{TxID: &chainhash.Hash{}, Vout: 0}
	ctx := context.Background()

	t.Run("nil record", func(t *testing.T) {
		require.Error(t, s.postProcessUnspendRecord(ctx, spend, nil))
	})

	t.Run("no lua bin", func(t *testing.T) {
		rec := &aerospike.Record{Bins: aerospike.BinMap{}}
		require.Error(t, s.postProcessUnspendRecord(ctx, spend, rec))
	})

	t.Run("unparseable response", func(t *testing.T) {
		rec := &aerospike.Record{Bins: aerospike.BinMap{LuaSuccess.String(): 12345}}
		require.Error(t, s.postProcessUnspendRecord(ctx, spend, rec))
	})

	t.Run("lua error status surfaces StorageError", func(t *testing.T) {
		rec := &aerospike.Record{Bins: aerospike.BinMap{LuaSuccess.String(): map[interface{}]interface{}{
			"status":    "ERROR",
			"errorCode": "SPENT",
			"message":   "boom",
		}}}
		require.Error(t, s.postProcessUnspendRecord(ctx, spend, rec))
	})

	t.Run("ok status with no signal succeeds", func(t *testing.T) {
		rec := &aerospike.Record{Bins: aerospike.BinMap{LuaSuccess.String(): map[interface{}]interface{}{
			"status": "OK",
		}}}
		require.NoError(t, s.postProcessUnspendRecord(ctx, spend, rec))
	})
}
