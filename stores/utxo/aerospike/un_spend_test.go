package aerospike

import (
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	spendpkg "github.com/bsv-blockchain/teranode/stores/utxo/spend"
	"github.com/stretchr/testify/require"
)

// makeSpends builds n minimal, distinct *utxo.Spend values for exercising
// chunkSpends. Only Vout needs to vary per element for the "distinct
// contents survive chunking" assertions below; no real Aerospike I/O is
// involved so the TxID/SpendingData/UTXOHash values don't need to be
// cryptographically meaningful.
func makeSpends(t *testing.T, n int) []*utxo.Spend {
	t.Helper()

	txID := chainhash.HashH([]byte("chunk-spends-test-tx"))
	spendingTxID := chainhash.HashH([]byte("chunk-spends-test-spender"))

	spends := make([]*utxo.Spend, n)
	for i := 0; i < n; i++ {
		spends[i] = &utxo.Spend{
			TxID:         &txID,
			Vout:         uint32(i), // nolint: gosec
			UTXOHash:     &chainhash.Hash{},
			SpendingData: spendpkg.NewSpendingData(&spendingTxID, i),
		}
	}

	return spends
}

// TestUnspendChunks_CountAndContents is the RED->GREEN TDD driver for the
// batched Unspend rework (issue #1214 task 3): chunkSpends is the pure
// helper the batched unspend uses to bound each BatchOperate round-trip to
// unspendBatchChunkSize records. This runs with no build tag and no
// Aerospike container.
func TestUnspendChunks_CountAndContents(t *testing.T) {
	spends := makeSpends(t, 2100)

	chunks := chunkSpends(spends, 1024)
	require.Len(t, chunks, 3) // ceil(2100/1024)
	require.Len(t, chunks[0], 1024)
	require.Len(t, chunks[1], 1024)
	require.Len(t, chunks[2], 2100-2048)

	// Contents must be preserved in order, with no drops or duplicates.
	reassembled := make([]*utxo.Spend, 0, len(spends))
	for _, c := range chunks {
		reassembled = append(reassembled, c...)
	}
	require.Equal(t, spends, reassembled)
}

func TestUnspendChunks_ExactMultiple(t *testing.T) {
	spends := makeSpends(t, 2048)

	chunks := chunkSpends(spends, 1024)
	require.Len(t, chunks, 2)
	require.Len(t, chunks[0], 1024)
	require.Len(t, chunks[1], 1024)
}

func TestUnspendChunks_SmallerThanChunkSize(t *testing.T) {
	spends := makeSpends(t, 5)

	chunks := chunkSpends(spends, 1024)
	require.Len(t, chunks, 1)
	require.Len(t, chunks[0], 5)
}

func TestUnspendChunks_Empty(t *testing.T) {
	chunks := chunkSpends(nil, 1024)
	require.Empty(t, chunks)
}

func TestUnspendChunks_SingleElement(t *testing.T) {
	spends := makeSpends(t, 1)

	chunks := chunkSpends(spends, 1024)
	require.Len(t, chunks, 1)
	require.Len(t, chunks[0], 1)
	require.Same(t, spends[0], chunks[0][0])
}
