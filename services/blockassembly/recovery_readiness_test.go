package blockassembly

import (
	"context"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/services/blockassembly/blockassembly_api"
	"github.com/stretchr/testify/require"
)

func TestRecoveryResetRejectsLoadingBeforeEnqueue(t *testing.T) {
	initPrometheusMetrics()
	items := setupBlockAssemblyTest(t)
	b := items.blockAssembler
	setupBlockchainClient(t, items)
	b.SetSkipWaitForPendingBlocks(true)
	service := &BlockAssembly{blockAssembler: b, logger: b.logger}

	// Startup serves gRPC while loading, before the reset owner starts. A
	// rejected request must not remain queued to execute after startup finishes.
	b.unminedTransactionsLoading.Store(true)
	loadingCtx, cancelLoading := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancelLoading()
	state, err := service.RecoveryReset(loadingCtx, &blockassembly_api.EmptyMessage{})
	require.ErrorContains(t, err, errServiceNotReadyUnminedLoading)
	require.Nil(t, state)
	require.Empty(t, b.resetCh)
	require.Zero(t, b.recoveryResetID.Load())

	b.unminedTransactionsLoading.Store(false)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, b.startChannelListeners(ctx))
	t.Cleanup(func() { cancel(); b.wg.Wait() })
	state, err = service.RecoveryReset(ctx, &blockassembly_api.EmptyMessage{})
	require.NoError(t, err)
	require.Equal(t, uint64(1), state.ResetId)
}
