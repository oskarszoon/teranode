package subtreeprocessor

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	utxostore "github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/stretchr/testify/require"
)

func TestUnminedRecoveryCancellationLeavesQueueUntouched(t *testing.T) {
	stp := newTestProcessorNoStart(t)
	stp.SetCurrentBlockHeader(prevBlockHeader)
	ctx, cancel := context.WithCancel(t.Context())
	row := recoveryRow("cancelled-selection")
	err := stp.recoverUnmined(ctx, prevBlockHeader, []chainhash.Hash{row.Hash}, func(context.Context, []chainhash.Hash, func([]chainhash.Hash) (map[chainhash.Hash]bool, error)) ([]*utxostore.UnminedTransaction, error) {
		cancel()
		return []*utxostore.UnminedTransaction{row}, nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, stp.queue.length())
}
