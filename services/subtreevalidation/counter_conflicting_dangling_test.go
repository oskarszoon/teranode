package subtreevalidation

// Fail-closed behaviour + regression guards for
// checkCounterConflictingOnCurrentChain, backed by the SQLite UTXO store and a mock
// store for the TOCTOU race.
//
// Fail closed: if a counter the walk saw alive is deleted before the mined-on-chain
// re-read (GetMeta returns NOT_FOUND), the check must reject with an error naming the
// counter rather than tolerate it — a tolerated NOT_FOUND could hide a counter that is
// mined on our chain (TOCTOU fail-open on a consensus gate). A counter genuinely absent
// at walk time is instead excluded earlier by the walk (GetCounterConflictingTxHashes).
//
// Guard: when the counter DOES still exist and has been mined on our chain, the
// check must still reject the conflicting tx.

import (
	"context"
	"net/url"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
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

// TestCheckCounterConflictingOnCurrentChain_FailsClosedOnDanglingCounter models the
// TOCTOU race the fail-closed policy defends against: the counter-conflicting walk sees a
// counter alive, but the counter is deleted (pruner DAH) before
// checkCounterConflictingOnCurrentChain re-reads it via GetMeta. A tolerated NOT_FOUND at
// that point could hide a counter that IS mined on our chain, so the check must fail closed
// — return an error naming the missing counter — instead of accepting the block.
//
// This requires a mock store: with a consistent real store the walk and GetMeta observe the
// same state, so a counter absent at GetMeta time is already excluded by the walk and never
// re-read here. The walk-side exclusion of a genuinely deleted counter is covered by the
// store-backed walk tests in stores/utxo.
func TestCheckCounterConflictingOnCurrentChain_FailsClosedOnDanglingCounter(t *testing.T) {
	InitPrometheusMetrics()

	ctx := context.Background()
	mockStore := &utxo.MockUtxostore{}

	s := &Server{utxoStore: mockStore}

	txHash := chainhash.HashH([]byte("dangling-counter-tx"))
	parentHash := chainhash.HashH([]byte("dangling-counter-parent"))
	counterHash := chainhash.HashH([]byte("dangling-counter-spender"))

	// The conflicting tx spends parentHash[0].
	conflictingTx := bt.NewTx()
	in := &bt.Input{PreviousTxOutIndex: 0}
	_ = in.PreviousTxIDAdd(&parentHash)
	conflictingTx.Inputs = append(conflictingTx.Inputs, in)
	conflictingTx.Outputs = append(conflictingTx.Outputs, &bt.Output{Satoshis: 1000})

	// Walk step 1: fetch the conflicting tx body.
	mockStore.On("Get", mock.Anything, &txHash, mock.Anything).
		Return(&meta.Data{Tx: conflictingTx}, nil)

	// Walk step 2: the parent output is spent by counterHash, with no deletedChildren marker.
	mockStore.On("Get", mock.Anything, &parentHash, mock.Anything).
		Return(&meta.Data{SpendingDatas: []*spend.SpendingData{{TxID: &counterHash}}}, nil)

	// Walk step 3: the counter still exists at walk time, so the walk keeps it in the counter
	// set (existence Get succeeds, no conflicting children).
	mockStore.On("Get", mock.Anything, &counterHash, mock.Anything).
		Return(&meta.Data{}, nil)
	mockStore.On("GetConflictingChildren", mock.Anything, counterHash).
		Return([]chainhash.Hash{}, nil)

	// The queried tx itself is always in the counter set; keep it alive so only the racing
	// counter drives the outcome.
	mockStore.On("GetMeta", mock.Anything, &txHash, mock.Anything).Return(&meta.Data{})

	// But the counter is deleted before the mined-on-chain re-read: GetMeta returns NOT_FOUND.
	// The mock's message deliberately does NOT contain the counter hash, so the assertion
	// below pins the CODE's own "...for %s" enrichment rather than echoing the mock's text.
	mockStore.On("GetMeta", mock.Anything, &counterHash, mock.Anything).
		Return(errors.NewTxNotFoundError("record vanished"))

	err := s.checkCounterConflictingOnCurrentChain(ctx, txHash, map[uint32]bool{})

	require.Error(t, err, "a counter that vanishes between the walk and GetMeta must fail closed")
	require.Contains(t, err.Error(), counterHash.String(),
		"the fail-closed error must name the missing counter hash (from the code's enrichment, not the mock message)")
	require.False(t, errors.Is(err, errors.ErrTxInvalid),
		"a racing-delete fail-close is a Processing error, not the marked-ghost TxInvalid path")
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
