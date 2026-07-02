package subtreevalidation

// End-to-end dangling-reference tolerance + regression guards for
// checkCounterConflictingOnCurrentChain, backed by the SQLite UTXO store.
//
// Tolerance: a conflicting tx whose counter (the recorded spender of the same
// parent output) has been deleted from the store must NOT fail the check with a
// TX_NOT_FOUND error — the counter simply no longer exists on our chain, which is
// not grounds to reject.
//
// Guard: when the counter DOES still exist and has been mined on our chain, the
// check must still reject the conflicting tx. The fix must tolerate genuinely
// missing counters without swallowing real mined-on-chain rejections.

import (
	"context"
	"net/url"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/stores/utxo/spend"
	"github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func newDanglingUtxoStore(ctx context.Context, t *testing.T, name string) *sql.Store {
	t.Helper()

	logger := ulogger.NewErrorTestLogger(t)
	tSettings := test.CreateBaseTestSettings(t)

	utxoStoreURL, err := url.Parse("sqlitememory:///" + name)
	require.NoError(t, err)

	utxoStore, err := sql.New(ctx, logger, tSettings, utxoStoreURL)
	require.NoError(t, err)

	return utxoStore
}

// TestCheckCounterConflictingOnCurrentChain_ToleratesDanglingCounter builds a real
// dangling reference (parent still records the counter as spender, but the counter
// record is deleted) and asserts the check passes. Pre-fix this FAILS: the counter
// walk fetches the deleted counter and returns TX_NOT_FOUND, which the check wraps.
func TestCheckCounterConflictingOnCurrentChain_ToleratesDanglingCounter(t *testing.T) {
	InitPrometheusMetrics()

	ctx := context.Background()
	utxoStore := newDanglingUtxoStore(ctx, t, "dangling_tolerate")

	s := &Server{utxoStore: utxoStore}

	_, err := s.utxoStore.Create(ctx, parentTx1, 122)
	require.NoError(t, err)

	// counter is a double-spend of the same parent output as tx1; it becomes the
	// recorded spender of the parent output.
	counter := tx1.Clone()
	counter.Version = 2

	_, err = s.utxoStore.Spend(ctx, counter, 122)
	require.NoError(t, err)

	_, err = s.utxoStore.Create(ctx, counter, 122)
	require.NoError(t, err)

	// tx1 is the conflicting loser also spending the parent output.
	_, err = s.utxoStore.Create(ctx, tx1, 123, utxo.WithConflicting(true))
	require.NoError(t, err)

	// Delete the counter → the parent's spending data now dangles.
	require.NoError(t, s.utxoStore.Delete(ctx, counter.TxIDChainHash()))

	err = s.checkCounterConflictingOnCurrentChain(ctx, *tx1.TxIDChainHash(), map[uint32]bool{})
	require.NoError(t, err, "a deleted counter must be tolerated, not rejected with TX_NOT_FOUND")
}

// TestCheckCounterConflictingOnCurrentChain_RejectsMinedCounterGuard is the
// regression guard: with the counter present and mined on our chain, the check must
// still reject. This passes both before and after the tolerance fix.
func TestCheckCounterConflictingOnCurrentChain_RejectsMinedCounterGuard(t *testing.T) {
	InitPrometheusMetrics()

	ctx := context.Background()
	utxoStore := newDanglingUtxoStore(ctx, t, "dangling_mined_guard")

	s := &Server{utxoStore: utxoStore}

	_, err := s.utxoStore.Create(ctx, parentTx1, 122)
	require.NoError(t, err)

	counter := tx1.Clone()
	counter.Version = 2

	_, err = s.utxoStore.Spend(ctx, counter, 122)
	require.NoError(t, err)

	_, err = s.utxoStore.Create(ctx, counter, 122)
	require.NoError(t, err)

	_, err = s.utxoStore.Create(ctx, tx1, 123, utxo.WithConflicting(true))
	require.NoError(t, err)

	// Counter not yet mined → check passes.
	err = s.checkCounterConflictingOnCurrentChain(ctx, *tx1.TxIDChainHash(), map[uint32]bool{})
	require.NoError(t, err)

	// Mine the counter on block 122.
	blockIDsMap, err := s.utxoStore.SetMinedMulti(ctx, []*chainhash.Hash{counter.TxIDChainHash()}, utxo.MinedBlockInfo{
		BlockID:     122,
		BlockHeight: 122,
		SubtreeIdx:  0,
	})
	require.NoError(t, err)
	require.Equal(t, []uint32{122}, blockIDsMap[*counter.TxIDChainHash()])

	// Counter now mined on our chain → the conflicting tx must be rejected.
	err = s.checkCounterConflictingOnCurrentChain(ctx, *tx1.TxIDChainHash(), map[uint32]bool{122: true})
	require.Error(t, err, "a counter mined on our chain must still reject the conflicting tx")
}

// TestCheckCounterConflictingOnCurrentChain_FrozenSentinelCounterRejected drives the
// counter-conflicting walk (via a mocked store) to keep the frozen sentinel
// (subtreepkg.CoinbasePlaceholderHashValue, the all-0xFF marker) in the counter set — the
// walk does this on purpose for a frozen parent slot. checkCounterConflictingOnCurrentChain
// must then reject the conflicting tx through its placeholder guard ("counter conflicting tx
// is frozen"), never treating the sentinel as an ordinary counter. Previously this path was
// covered only by the sequential integration test testFrozenTxInConflictResolutionPath; this
// is the unit-level guard.
func TestCheckCounterConflictingOnCurrentChain_FrozenSentinelCounterRejected(t *testing.T) {
	InitPrometheusMetrics()

	ctx := context.Background()
	mockStore := &utxo.MockUtxostore{}

	s := &Server{utxoStore: mockStore}

	txHash := chainhash.HashH([]byte("frozen-counter-tx"))
	parentHash := chainhash.HashH([]byte("frozen-counter-parent"))

	// The conflicting tx spends parentHash[0].
	conflictingTx := bt.NewTx()
	in := &bt.Input{PreviousTxOutIndex: 0}
	_ = in.PreviousTxIDAdd(&parentHash)
	conflictingTx.Inputs = append(conflictingTx.Inputs, in)
	conflictingTx.Outputs = append(conflictingTx.Outputs, &bt.Output{Satoshis: 1000})

	// Walk step 1: fetch the conflicting tx body.
	mockStore.On("Get", mock.Anything, &txHash, mock.Anything).
		Return(&meta.Data{Tx: conflictingTx}, nil)

	// Walk step 2: the parent output is spent by the frozen sentinel, so the walk keeps the
	// sentinel in the counter set (no existence Get on it).
	frozenHash := subtreepkg.FrozenBytesTxHash
	mockStore.On("Get", mock.Anything, &parentHash, mock.Anything).
		Return(&meta.Data{SpendingDatas: []*spend.SpendingData{{TxID: &frozenHash}}}, nil)

	// The non-sentinel counter (the tx itself) is looked up via GetMeta; return it alive so
	// only the sentinel drives the rejection.
	mockStore.On("GetMeta", mock.Anything, &txHash, mock.Anything).Return(&meta.Data{})

	err := s.checkCounterConflictingOnCurrentChain(ctx, txHash, map[uint32]bool{})

	require.Error(t, err)
	require.Contains(t, err.Error(), "counter conflicting tx is frozen",
		"a frozen sentinel in the counter set must reject via the placeholder guard")
}
