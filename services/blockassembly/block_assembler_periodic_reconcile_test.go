// Package blockassembly provides functionality for assembling Bitcoin blocks in Teranode.
package blockassembly

import (
	"context"
	"database/sql"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	utxostoresql "github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/ordishs/gocore"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// shutdownListenerGoroutine cancels ctx and gives the BlockAssembler's
// listener goroutine a brief grace window to exit before the test returns.
// BlockAssembler doesn't expose a Stop method (Stop lives on the BlockAssembly
// server wrapper), so context cancellation is the supported shutdown signal —
// see BlockAssembler.go:317 (`case <-ctx.Done(): ... return`). The grace
// sleep avoids leaving a goroutine alive in -race runs of subsequent tests.
func shutdownListenerGoroutine(t *testing.T, cancel context.CancelFunc) {
	t.Helper()
	t.Cleanup(func() {
		cancel()
		time.Sleep(50 * time.Millisecond)
	})
}

// TestBlockAssembler_PeriodicReconcile_FiresWhenIntervalSet verifies that when
// PeriodicReconcileInterval is configured to a positive duration, the listener
// goroutine fires a reconcile on the configured cadence. The reconcile path
// invokes processNewBlockAnnouncement, which calls GetBestBlockHeader; counting
// those calls past the startup baseline proves the periodic ticker is active.
//
// Regression test for #872: when the blockchain subscription stream went silent
// (e.g. after a gRPC EOF that some subscribers re-established but BA did not),
// BA's listener received no further block notifications and sat idle for 15+
// hours. The periodic reconcile guarantees BA pulls a fresh tip on a bounded
// cadence regardless of subscription state.
func TestBlockAssembler_PeriodicReconcile_FiresWhenIntervalSet(t *testing.T) {
	initPrometheusMetrics()

	ctx, cancel := context.WithCancel(t.Context())
	shutdownListenerGoroutine(t, cancel)

	tSettings := createTestSettings(t)
	tSettings.BlockAssembly.PeriodicReconcileInterval = 50 * time.Millisecond

	utxoStoreURL, err := url.Parse("sqlitememory:///test")
	require.NoError(t, err)

	uStore, err := utxostoresql.New(ctx, ulogger.TestLogger{}, tSettings, utxoStoreURL)
	require.NoError(t, err)

	stats := gocore.NewStat("test")

	var getBestBlockHeaderCalls atomic.Int32

	blockchainClient := &blockchain.Mock{}
	blockchainClient.On("GetState", mock.Anything, mock.Anything).Return([]byte{}, sql.ErrNoRows)
	blockchainClient.On("SetState", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	blockchainClient.
		On("GetBestBlockHeader", mock.Anything).
		Return(model.GenesisBlockHeader, &model.BlockHeaderMeta{Height: 0}, nil).
		Run(func(_ mock.Arguments) {
			getBestBlockHeaderCalls.Add(1)
		})
	blockchainClient.On("GetBlockHeaders", mock.Anything, mock.Anything, mock.Anything).Return([]*model.BlockHeader{model.GenesisBlockHeader}, []*model.BlockHeaderMeta{{Height: 0}}, nil)
	blockchainClient.On("GetBlockHeaderIDs", mock.Anything, mock.Anything, mock.Anything).Return([]uint32{0}, nil)
	blockchainClient.On("GetBlocksMinedNotSet", mock.Anything).Return([]*model.Block{}, nil)
	blockchainClient.On("GetNextWorkRequired", mock.Anything, mock.Anything, mock.Anything).Return(nil, errors.ErrNotFound)
	runningState := blockchain.FSMStateRUNNING
	blockchainClient.On("GetFSMCurrentState", mock.Anything).Return(&runningState, nil)

	// Subscription channel is intentionally empty. With no notifications flowing,
	// the only way GetBestBlockHeader fires post-startup is via the periodic
	// reconcile ticker we're verifying here.
	subChan := make(chan *blockchain_api.Notification, 1)
	blockchainClient.On("Subscribe", mock.Anything, mock.Anything).Return(subChan, nil)

	ba, err := NewBlockAssembler(ctx, ulogger.TestLogger{}, tSettings, stats, uStore, nil, blockchainClient, nil)
	require.NoError(t, err)

	require.NoError(t, ba.Start(ctx))

	// Let startup settle and capture baseline. Startup paths (initState,
	// loadUnminedTransactions, the initial triggerReconcile in
	// startChannelListeners) call GetBestBlockHeader a small fixed number of
	// times. We're not asserting against that absolute count — only that
	// further calls happen on the ticker cadence below.
	time.Sleep(150 * time.Millisecond)
	baseline := getBestBlockHeaderCalls.Load()

	// Wait long enough for ~4 ticker firings at 50ms.
	time.Sleep(220 * time.Millisecond)

	after := getBestBlockHeaderCalls.Load()

	require.GreaterOrEqual(t, after-baseline, int32(2),
		"expected periodic reconcile ticker to fire at least 2 times in 220ms at 50ms interval; baseline=%d after=%d",
		baseline, after)
}

// TestBlockAssembler_PeriodicReconcile_DisabledWhenIntervalZero verifies that
// setting PeriodicReconcileInterval=0 disables the periodic ticker entirely
// (escape hatch for tests / dev / environments that don't want the cadence).
func TestBlockAssembler_PeriodicReconcile_DisabledWhenIntervalZero(t *testing.T) {
	initPrometheusMetrics()

	ctx, cancel := context.WithCancel(t.Context())
	shutdownListenerGoroutine(t, cancel)

	tSettings := createTestSettings(t)
	tSettings.BlockAssembly.PeriodicReconcileInterval = 0

	utxoStoreURL, err := url.Parse("sqlitememory:///test")
	require.NoError(t, err)

	uStore, err := utxostoresql.New(ctx, ulogger.TestLogger{}, tSettings, utxoStoreURL)
	require.NoError(t, err)

	stats := gocore.NewStat("test")

	var getBestBlockHeaderCalls atomic.Int32

	blockchainClient := &blockchain.Mock{}
	blockchainClient.On("GetState", mock.Anything, mock.Anything).Return([]byte{}, sql.ErrNoRows)
	blockchainClient.On("SetState", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	blockchainClient.
		On("GetBestBlockHeader", mock.Anything).
		Return(model.GenesisBlockHeader, &model.BlockHeaderMeta{Height: 0}, nil).
		Run(func(_ mock.Arguments) {
			getBestBlockHeaderCalls.Add(1)
		})
	blockchainClient.On("GetBlockHeaders", mock.Anything, mock.Anything, mock.Anything).Return([]*model.BlockHeader{model.GenesisBlockHeader}, []*model.BlockHeaderMeta{{Height: 0}}, nil)
	blockchainClient.On("GetBlockHeaderIDs", mock.Anything, mock.Anything, mock.Anything).Return([]uint32{0}, nil)
	blockchainClient.On("GetBlocksMinedNotSet", mock.Anything).Return([]*model.Block{}, nil)
	blockchainClient.On("GetNextWorkRequired", mock.Anything, mock.Anything, mock.Anything).Return(nil, errors.ErrNotFound)
	runningState := blockchain.FSMStateRUNNING
	blockchainClient.On("GetFSMCurrentState", mock.Anything).Return(&runningState, nil)

	subChan := make(chan *blockchain_api.Notification, 1)
	blockchainClient.On("Subscribe", mock.Anything, mock.Anything).Return(subChan, nil)

	ba, err := NewBlockAssembler(ctx, ulogger.TestLogger{}, tSettings, stats, uStore, nil, blockchainClient, nil)
	require.NoError(t, err)
	require.NoError(t, ba.Start(ctx))

	// Let startup paths complete.
	time.Sleep(200 * time.Millisecond)
	baseline := getBestBlockHeaderCalls.Load()

	// Wait a window that would have produced multiple ticks at any small
	// positive interval. With interval=0 we expect zero additional calls.
	time.Sleep(300 * time.Millisecond)

	after := getBestBlockHeaderCalls.Load()

	require.Equal(t, baseline, after,
		"expected no additional GetBestBlockHeader calls when PeriodicReconcileInterval=0; baseline=%d after=%d",
		baseline, after)
}

// TestBlockAssembler_PeriodicReconcile_NotificationStillProcessed verifies that
// adding the periodic ticker doesn't break the existing notification path —
// a Block notification arriving on blockchainSubscriptionCh still drives
// processNewBlockAnnouncement on the notification cadence rather than waiting
// for the next ticker fire.
func TestBlockAssembler_PeriodicReconcile_NotificationStillProcessed(t *testing.T) {
	initPrometheusMetrics()

	ctx, cancel := context.WithCancel(t.Context())
	shutdownListenerGoroutine(t, cancel)

	tSettings := createTestSettings(t)
	// Long interval so any GetBestBlockHeader calls during the test window
	// can only be attributed to the notification arm (or startup), never the
	// periodic ticker.
	tSettings.BlockAssembly.PeriodicReconcileInterval = 1 * time.Hour

	utxoStoreURL, err := url.Parse("sqlitememory:///test")
	require.NoError(t, err)

	uStore, err := utxostoresql.New(ctx, ulogger.TestLogger{}, tSettings, utxoStoreURL)
	require.NoError(t, err)

	stats := gocore.NewStat("test")

	var getBestBlockHeaderCalls atomic.Int32

	blockchainClient := &blockchain.Mock{}
	blockchainClient.On("GetState", mock.Anything, mock.Anything).Return([]byte{}, sql.ErrNoRows)
	blockchainClient.On("SetState", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	blockchainClient.
		On("GetBestBlockHeader", mock.Anything).
		Return(model.GenesisBlockHeader, &model.BlockHeaderMeta{Height: 0}, nil).
		Run(func(_ mock.Arguments) {
			getBestBlockHeaderCalls.Add(1)
		})
	blockchainClient.On("GetBlockHeaders", mock.Anything, mock.Anything, mock.Anything).Return([]*model.BlockHeader{model.GenesisBlockHeader}, []*model.BlockHeaderMeta{{Height: 0}}, nil)
	blockchainClient.On("GetBlockHeaderIDs", mock.Anything, mock.Anything, mock.Anything).Return([]uint32{0}, nil)
	blockchainClient.On("GetBlocksMinedNotSet", mock.Anything).Return([]*model.Block{}, nil)
	blockchainClient.On("GetNextWorkRequired", mock.Anything, mock.Anything, mock.Anything).Return(nil, errors.ErrNotFound)
	runningState := blockchain.FSMStateRUNNING
	blockchainClient.On("GetFSMCurrentState", mock.Anything).Return(&runningState, nil)

	subChan := make(chan *blockchain_api.Notification, 4)
	blockchainClient.On("Subscribe", mock.Anything, mock.Anything).Return(subChan, nil)

	ba, err := NewBlockAssembler(ctx, ulogger.TestLogger{}, tSettings, stats, uStore, nil, blockchainClient, nil)
	require.NoError(t, err)
	require.NoError(t, ba.Start(ctx))

	// Let startup settle.
	time.Sleep(150 * time.Millisecond)
	baseline := getBestBlockHeaderCalls.Load()

	// Push a Block notification — should drive processNewBlockAnnouncement.
	subChan <- &blockchain_api.Notification{
		Type: model.NotificationType_Block,
		Hash: (&chainhash.Hash{}).CloneBytes(),
	}

	// Give the listener a chance to consume it.
	time.Sleep(100 * time.Millisecond)

	after := getBestBlockHeaderCalls.Load()
	require.Greater(t, after, baseline,
		"expected notification on blockchainSubscriptionCh to drive a reconcile pass; baseline=%d after=%d",
		baseline, after)
}
