package aerospike

import (
	"context"
	"testing"
	"time"

	as "github.com/aerospike/aerospike-client-go/v8"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

func Test_ScanInconsistentUnminedTxs_NilClient(t *testing.T) {
	store := &Store{
		client: nil,
	}

	it, err := store.ScanInconsistentUnminedTxs()
	require.Error(t, err)
	require.Nil(t, it)
	require.Contains(t, err.Error(), "aerospike client not initialized")
}

func Test_consistencyScanIterator_Close(t *testing.T) {
	t.Run("close sets done flag", func(t *testing.T) {
		it := &consistencyScanIterator{
			done: false,
		}

		err := it.Close()
		require.NoError(t, err)
		require.True(t, it.done)
	})

	t.Run("close is idempotent", func(t *testing.T) {
		it := &consistencyScanIterator{
			done: false,
		}

		err := it.Close()
		require.NoError(t, err)
		require.True(t, it.done)

		// Second close should be safe
		err = it.Close()
		require.NoError(t, err)
		require.True(t, it.done)
	})

	t.Run("close with cancel function", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		it := &consistencyScanIterator{
			done:          false,
			cancelWorkers: cancel,
		}

		err := it.Close()
		require.NoError(t, err)
		require.True(t, it.done)
		// Verify context was cancelled
		require.Error(t, ctx.Err())
	})
}

func Test_consistencyScanIterator_Err(t *testing.T) {
	t.Run("no error", func(t *testing.T) {
		it := &consistencyScanIterator{}
		require.NoError(t, it.Err())
	})

	t.Run("with error", func(t *testing.T) {
		testErr := errors.NewError("test error")
		it := &consistencyScanIterator{
			err: testErr,
		}
		require.Equal(t, testErr, it.Err())
	})
}

func Test_consistencyScanIterator_TotalScanned(t *testing.T) {
	it := &consistencyScanIterator{}

	require.Equal(t, int64(0), it.TotalScanned())

	it.totalScanned.Add(42)
	require.Equal(t, int64(42), it.TotalScanned())

	it.totalScanned.Add(58)
	require.Equal(t, int64(100), it.TotalScanned())
}

func Test_consistencyScanIterator_Next_EdgeCases(t *testing.T) {
	ctx := context.Background()

	t.Run("already done", func(t *testing.T) {
		it := &consistencyScanIterator{
			done: true,
		}

		batch, err := it.Next(ctx)
		require.NoError(t, err)
		require.Nil(t, batch)
	})

	t.Run("has error", func(t *testing.T) {
		testErr := errors.NewError("test error")
		it := &consistencyScanIterator{
			err: testErr,
		}

		batch, err := it.Next(ctx)
		require.Equal(t, testErr, err)
		require.Nil(t, batch)
	})

	t.Run("channels closed — iteration complete", func(t *testing.T) {
		resultChan := make(chan []*utxo.InconsistentTxRecord)
		errorChan := make(chan error, 1)
		close(resultChan)
		close(errorChan)

		it := &consistencyScanIterator{
			resultChan: resultChan,
			errorChan:  errorChan,
		}

		batch, err := it.Next(ctx)
		require.NoError(t, err)
		require.Nil(t, batch)
		require.True(t, it.done)
	})

	t.Run("context cancelled", func(t *testing.T) {
		resultChan := make(chan []*utxo.InconsistentTxRecord)
		errorChan := make(chan error, 1)

		it := &consistencyScanIterator{
			resultChan: resultChan,
			errorChan:  errorChan,
		}

		cancelCtx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		batch, err := it.Next(cancelCtx)
		require.Error(t, err)
		require.Nil(t, batch)
		require.True(t, it.done)
	})

	t.Run("error on channel after result channel closes", func(t *testing.T) {
		resultChan := make(chan []*utxo.InconsistentTxRecord, 1)
		errorChan := make(chan error, 1)

		scanErr := errors.NewProcessingError("scan failed mid-way")
		errorChan <- scanErr
		close(resultChan)

		it := &consistencyScanIterator{
			resultChan: resultChan,
			errorChan:  errorChan,
		}

		batch, err := it.Next(ctx)
		require.Nil(t, batch)
		require.Error(t, err)
		require.Contains(t, err.Error(), "scan failed mid-way")
	})

	t.Run("receives batch from channel", func(t *testing.T) {
		resultChan := make(chan []*utxo.InconsistentTxRecord, 1)
		errorChan := make(chan error, 1)

		expectedBatch := []*utxo.InconsistentTxRecord{
			{UnminedSince: 5, BlockIDs: []uint32{1, 2}},
		}
		resultChan <- expectedBatch

		it := &consistencyScanIterator{
			resultChan: resultChan,
			errorChan:  errorChan,
		}

		batch, err := it.Next(ctx)
		require.NoError(t, err)
		require.Equal(t, expectedBatch, batch)
		require.False(t, it.done)
	})

	t.Run("error on errorChan before reading results", func(t *testing.T) {
		resultChan := make(chan []*utxo.InconsistentTxRecord, 1)
		errorChan := make(chan error, 1)

		workerErr := errors.NewProcessingError("partition query failed")
		errorChan <- workerErr

		it := &consistencyScanIterator{
			resultChan: resultChan,
			errorChan:  errorChan,
		}

		batch, err := it.Next(ctx)
		require.Nil(t, batch)
		require.Error(t, err)
		require.Contains(t, err.Error(), "partition query failed")
		require.True(t, it.done)
	})
}

func Test_parseConsistencyRecord(t *testing.T) {
	validHash := chainhash.HashH([]byte("test-tx"))

	t.Run("valid record with all fields", func(t *testing.T) {
		bins := map[string]interface{}{
			fields.TxID.String():         validHash[:],
			fields.UnminedSince.String(): int(42),
			fields.BlockIDs.String():     []interface{}{int(1), int(2), int(3)},
		}

		rec, ok := parseConsistencyRecord(bins)
		require.True(t, ok)
		require.Equal(t, validHash, rec.Hash)
		require.Equal(t, 42, rec.UnminedSince)
		require.Equal(t, []uint32{1, 2, 3}, rec.BlockIDs)
	})

	t.Run("valid record with no block IDs", func(t *testing.T) {
		bins := map[string]interface{}{
			fields.TxID.String():         validHash[:],
			fields.UnminedSince.String(): int(5),
		}

		rec, ok := parseConsistencyRecord(bins)
		require.True(t, ok)
		require.Equal(t, validHash, rec.Hash)
		require.Equal(t, 5, rec.UnminedSince)
		require.Equal(t, []uint32{}, rec.BlockIDs)
	})

	t.Run("valid record with unmined_since zero", func(t *testing.T) {
		bins := map[string]interface{}{
			fields.TxID.String():         validHash[:],
			fields.UnminedSince.String(): int(0),
			fields.BlockIDs.String():     []interface{}{int(10)},
		}

		rec, ok := parseConsistencyRecord(bins)
		require.True(t, ok)
		require.Equal(t, 0, rec.UnminedSince)
		require.Equal(t, []uint32{10}, rec.BlockIDs)
	})

	t.Run("missing unmined_since defaults to zero", func(t *testing.T) {
		bins := map[string]interface{}{
			fields.TxID.String(): validHash[:],
		}

		rec, ok := parseConsistencyRecord(bins)
		require.True(t, ok)
		require.Equal(t, 0, rec.UnminedSince)
	})

	t.Run("missing txid returns false", func(t *testing.T) {
		bins := map[string]interface{}{
			fields.UnminedSince.String(): int(5),
		}

		rec, ok := parseConsistencyRecord(bins)
		require.False(t, ok)
		require.Nil(t, rec)
	})

	t.Run("txid wrong type returns false", func(t *testing.T) {
		bins := map[string]interface{}{
			fields.TxID.String(): "not-bytes",
		}

		rec, ok := parseConsistencyRecord(bins)
		require.False(t, ok)
		require.Nil(t, rec)
	})

	t.Run("txid too short returns false", func(t *testing.T) {
		bins := map[string]interface{}{
			fields.TxID.String(): []byte{0x01, 0x02, 0x03}, // too short for a hash
		}

		rec, ok := parseConsistencyRecord(bins)
		require.False(t, ok)
		require.Nil(t, rec)
	})

	t.Run("empty bins returns false", func(t *testing.T) {
		bins := map[string]interface{}{}

		rec, ok := parseConsistencyRecord(bins)
		require.False(t, ok)
		require.Nil(t, rec)
	})
}

func Test_consistencyScanIterator_MultipleNextCalls(t *testing.T) {
	ctx := context.Background()

	t.Run("drains multiple batches then returns nil", func(t *testing.T) {
		resultChan := make(chan []*utxo.InconsistentTxRecord, 3)
		errorChan := make(chan error, 1)

		batch1 := []*utxo.InconsistentTxRecord{{UnminedSince: 1}}
		batch2 := []*utxo.InconsistentTxRecord{{UnminedSince: 2}}
		resultChan <- batch1
		resultChan <- batch2
		close(resultChan)
		close(errorChan)

		it := &consistencyScanIterator{
			resultChan: resultChan,
			errorChan:  errorChan,
		}

		got1, err := it.Next(ctx)
		require.NoError(t, err)
		require.Equal(t, batch1, got1)

		got2, err := it.Next(ctx)
		require.NoError(t, err)
		require.Equal(t, batch2, got2)

		got3, err := it.Next(ctx)
		require.NoError(t, err)
		require.Nil(t, got3)
		require.True(t, it.done)
	})
}

func Test_consistencyScanIterator_ProgressReporter(t *testing.T) {
	store := &Store{
		logger: ulogger.TestLogger{},
	}

	it := &consistencyScanIterator{
		store: store,
	}

	it.totalScanned.Add(1000)

	stop := it.ProgressReporter(50 * time.Millisecond)
	require.NotNil(t, stop)

	// Let it tick at least once
	time.Sleep(100 * time.Millisecond)

	// Stop should not panic
	stop()

	// Calling stop again should be safe
	stop()
}

func Test_consistencyScanIterator_processResults(t *testing.T) {
	validHash := chainhash.HashH([]byte("test-tx-1"))
	validHash2 := chainhash.HashH([]byte("test-tx-2"))

	makeResult := func(bins as.BinMap) *as.Result {
		return &as.Result{
			Record: &as.Record{Bins: bins},
		}
	}

	t.Run("processes valid records and flushes on channel close", func(t *testing.T) {
		resultChan := make(chan []*utxo.InconsistentTxRecord, 10)
		errorChan := make(chan error, 1)
		it := &consistencyScanIterator{
			resultChan: resultChan,
			errorChan:  errorChan,
		}

		results := make(chan *as.Result, 3)
		results <- makeResult(as.BinMap{
			fields.TxID.String():         validHash[:],
			fields.UnminedSince.String(): int(5),
			fields.BlockIDs.String():     []interface{}{int(1)},
		})
		results <- makeResult(as.BinMap{
			fields.TxID.String():         validHash2[:],
			fields.UnminedSince.String(): int(0),
		})
		close(results)

		it.processResults(context.Background(), results)

		batch := <-resultChan
		require.Len(t, batch, 2)
		require.Equal(t, validHash, batch[0].Hash)
		require.Equal(t, 5, batch[0].UnminedSince)
		require.Equal(t, []uint32{1}, batch[0].BlockIDs)
		require.Equal(t, validHash2, batch[1].Hash)
		require.Equal(t, int64(2), it.TotalScanned())
	})

	t.Run("skips records with invalid txid", func(t *testing.T) {
		resultChan := make(chan []*utxo.InconsistentTxRecord, 10)
		errorChan := make(chan error, 1)
		it := &consistencyScanIterator{
			resultChan: resultChan,
			errorChan:  errorChan,
		}

		results := make(chan *as.Result, 3)
		// Invalid: no txid
		results <- makeResult(as.BinMap{
			fields.UnminedSince.String(): int(5),
		})
		// Invalid: txid wrong type
		results <- makeResult(as.BinMap{
			fields.TxID.String(): "not-bytes",
		})
		// Valid
		results <- makeResult(as.BinMap{
			fields.TxID.String():         validHash[:],
			fields.UnminedSince.String(): int(3),
		})
		close(results)

		it.processResults(context.Background(), results)

		batch := <-resultChan
		require.Len(t, batch, 1)
		require.Equal(t, validHash, batch[0].Hash)
		require.Equal(t, int64(3), it.TotalScanned()) // all 3 counted, but only 1 parsed
	})

	t.Run("stops on context cancellation", func(t *testing.T) {
		resultChan := make(chan []*utxo.InconsistentTxRecord, 10)
		errorChan := make(chan error, 1)
		it := &consistencyScanIterator{
			resultChan: resultChan,
			errorChan:  errorChan,
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		results := make(chan *as.Result, 1)
		results <- makeResult(as.BinMap{
			fields.TxID.String():         validHash[:],
			fields.UnminedSince.String(): int(1),
		})

		it.processResults(ctx, results)

		// Should have returned early without sending to resultChan
		require.Equal(t, int64(0), it.TotalScanned())
	})

	t.Run("sends error on record error", func(t *testing.T) {
		resultChan := make(chan []*utxo.InconsistentTxRecord, 10)
		errorChan := make(chan error, 1)
		it := &consistencyScanIterator{
			resultChan: resultChan,
			errorChan:  errorChan,
		}

		results := make(chan *as.Result, 2)
		results <- &as.Result{
			Err: as.ErrTimeout,
		}
		close(results)

		it.processResults(context.Background(), results)

		err := <-errorChan
		require.Error(t, err)
		require.Contains(t, err.Error(), "Timeout")
	})

	t.Run("handles nil result from channel", func(t *testing.T) {
		resultChan := make(chan []*utxo.InconsistentTxRecord, 10)
		errorChan := make(chan error, 1)
		it := &consistencyScanIterator{
			resultChan: resultChan,
			errorChan:  errorChan,
		}

		results := make(chan *as.Result, 1)
		results <- nil
		close(results)

		it.processResults(context.Background(), results)

		require.Equal(t, int64(0), it.TotalScanned())
	})
}
