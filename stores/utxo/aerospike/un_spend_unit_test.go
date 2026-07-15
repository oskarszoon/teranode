package aerospike

import (
	"context"
	"testing"
	"time"

	"github.com/bsv-blockchain/aerospike-client-go/v8"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// These are container-free unit tests for the batched Unspend's non-I/O branches
// (chunking, context abort, nil-spend skip, and per-record response handling).
// The happy path is exercised by the container-backed tests in un_spend_batch_test.go.

// chainAllNotFound mirrors the rewind tool's allNotFound: every *Error node in
// the chain must be a NotFound-family code. Used here to prove aggregateUnspendErrors
// preserves the all-NotFound signal (no ERR_ERROR cap summary) for a benign storm.
func chainAllNotFound(err error) bool {
	for cur := err; cur != nil; {
		var e *errors.Error
		if !errors.As(cur, &e) {
			return false
		}

		if e.Code() != errors.ErrTxNotFound.Code() && e.Code() != errors.ErrNotFound.Code() {
			return false
		}

		cur = e.WrappedErr()
	}

	return true
}

func TestAggregateUnspendErrors(t *testing.T) {
	require.NoError(t, aggregateUnspendErrors(nil))
	require.NoError(t, aggregateUnspendErrors([]error{}))

	// All-NotFound, more than the cap: joined UNCAPPED so the all-NotFound signal
	// survives (a JoinCapped ERR_ERROR summary link would break allNotFound in the
	// rewind path). This is the consolidation-tx-all-gone case.
	nfs := make([]error, 15)
	for i := range nfs {
		nfs[i] = errors.NewNotFoundError("output gone %d", i)
	}

	allNF := aggregateUnspendErrors(nfs)
	require.Error(t, allNF)
	require.True(t, chainAllNotFound(allNF), "all-NotFound aggregate must stay all-NotFound")
	require.True(t, errors.Is(allNF, errors.ErrNotFound))

	// Mixed with more than the cap: capped, and the genuine error (or the cap
	// summary link) breaks the all-NotFound classification, so the rewind path
	// surfaces it instead of swallowing it.
	mixed := make([]error, 0, 13)
	for i := 0; i < 12; i++ {
		mixed = append(mixed, errors.NewNotFoundError("gone %d", i))
	}

	mixed = append(mixed, errors.NewStorageError("device overload"))

	gotMixed := aggregateUnspendErrors(mixed)
	require.Error(t, gotMixed)
	require.False(t, chainAllNotFound(gotMixed), "a genuine error must not be tolerated as all-NotFound")
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
