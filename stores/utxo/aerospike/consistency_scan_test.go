package aerospike

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo"
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
