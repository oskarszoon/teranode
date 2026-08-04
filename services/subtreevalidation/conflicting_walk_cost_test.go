// Package subtreevalidation provides functionality for validating subtrees in a blockchain context.
package subtreevalidation

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/stores/utxo/nullstore"
	spendpkg "github.com/bsv-blockchain/teranode/stores/utxo/spend"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// countingStore is a map-backed utxo.Store that counts Store.Get calls. The
// conflicting walks are real — GetConflictingChildren and GetCounterConflicting
// delegate to the production free functions — so the Get count is the true cost
// of the walk under test, not a stubbed approximation. NullStore is embedded only
// to satisfy the rest of the wide utxo.Store interface.
type countingStore struct {
	*nullstore.NullStore

	mu        sync.Mutex
	records   map[chainhash.Hash]*meta.Data
	getCount  atomic.Int64
	metaCount atomic.Int64
}

func newCountingStore(t *testing.T) *countingStore {
	t.Helper()

	null, err := nullstore.NewNullStore()
	require.NoError(t, err)

	return &countingStore{NullStore: null, records: make(map[chainhash.Hash]*meta.Data)}
}

func (s *countingStore) put(hash chainhash.Hash, data *meta.Data) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.records[hash] = data
}

func (s *countingStore) gets() int64     { return s.getCount.Load() }
func (s *countingStore) getMetas() int64 { return s.metaCount.Load() }

// lookup is the uncounted read. Get and GetMeta both go through it so the two
// counters stay independent — a GetMeta must not inflate the walk's Get count,
// which is the number the cost assertion is about.
func (s *countingStore) lookup(hash *chainhash.Hash) (*meta.Data, error) {
	// The frozen / coinbase-placeholder sentinel is a record-less pseudo-hash.
	// Aerospike returns (nil, nil) for it rather than an error — BatchDecorate's
	// per-record error handling special-cases CoinbasePlaceholderHashValue so no
	// Err is set for it even when the batch record itself errored — and the walk
	// relies on that, so the fake must reproduce it or frozen detection behaves
	// differently here than in production.
	if hash.Equal(subtree.CoinbasePlaceholderHashValue) {
		return nil, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data, ok := s.records[*hash]
	if !ok {
		return nil, errors.NewTxNotFoundError("%v not found", hash)
	}

	return data, nil
}

func (s *countingStore) Get(_ context.Context, hash *chainhash.Hash, _ ...fields.FieldName) (*meta.Data, error) {
	// Not counted for the sentinel: no round trip happens for it in the real store
	// either, so counting it would make the bound depend on frozen placement.
	if !hash.Equal(subtree.CoinbasePlaceholderHashValue) {
		s.getCount.Add(1)
	}

	return s.lookup(hash)
}

func (s *countingStore) GetMeta(_ context.Context, hash *chainhash.Hash, data *meta.Data) error {
	s.metaCount.Add(1)

	found, err := s.lookup(hash)
	if err != nil {
		return err
	}

	if found != nil {
		*data = *found
	}

	return nil
}

func (s *countingStore) GetConflictingChildren(ctx context.Context, hash chainhash.Hash) ([]chainhash.Hash, error) {
	return utxo.GetConflictingChildren(ctx, s, hash)
}

func (s *countingStore) GetCounterConflicting(ctx context.Context, hash chainhash.Hash) ([]chainhash.Hash, error) {
	return utxo.GetCounterConflictingTxHashes(ctx, s, hash)
}

// txSpending builds a transaction spending vout 0 of parentTxHash. Only the input
// list matters — GetCounterConflictingTxHashes reads Tx.Inputs to find the parents
// whose spender slots identify the counter-conflicting transactions.
func txSpending(t *testing.T, parentTxHash chainhash.Hash) *bt.Tx {
	t.Helper()

	tx := bt.NewTx()

	input := &bt.Input{PreviousTxOutIndex: 0}
	require.NoError(t, input.PreviousTxIDAdd(&parentTxHash))
	tx.Inputs = append(tx.Inputs, input)
	tx.Outputs = append(tx.Outputs, &bt.Output{Satoshis: 1000, LockingScript: &bscript.Script{bscript.OpTRUE}})

	return tx
}

func spentBy(txHash chainhash.Hash) []*spendpkg.SpendingData {
	return []*spendpkg.SpendingData{spendpkg.NewSpendingData(&txHash, 0)}
}

// coneChainLength is the length of the planted linear self-spend chain. It mirrors the
// production shape from #1391: the counter-conflicting transaction heads a chain
// that extends by roughly a hundred transactions per block, one transaction per
// BFS level, so each walk costs N serial store round trips.
const coneChainLength = 1000

// plantLinearChain builds the #1391 shape and returns (parent, txUnderCheck, counter).
//
//	parent P  — vout 0 recorded as spent by the counter Y0
//	txUnderCheck X — spends P:0 as well (the conflicting transaction), no descendants
//	counter Y0 → Y1 → ... → Y{coneChainLength-1}, a linear chain of ordinary spenders
func plantLinearChain(t *testing.T, store *countingStore) (chainhash.Hash, chainhash.Hash, chainhash.Hash) {
	t.Helper()

	parentHash := chainhash.HashH([]byte("cone-parent"))
	txUnderCheck := chainhash.HashH([]byte("cone-tx-under-check"))

	chain := make([]chainhash.Hash, coneChainLength)
	for i := range chain {
		chain[i] = chainhash.HashH([]byte("cone-chain-" + strconv.Itoa(i)))
	}

	store.put(parentHash, &meta.Data{
		Tx:            txSpending(t, chainhash.HashH([]byte("cone-grandparent"))),
		SpendingDatas: spentBy(chain[0]),
	})

	// X spends the same parent output and has no descendants of its own.
	store.put(txUnderCheck, &meta.Data{Tx: txSpending(t, parentHash)})

	for i := 0; i < coneChainLength-1; i++ {
		store.put(chain[i], &meta.Data{SpendingDatas: spentBy(chain[i+1])})
	}

	// tip of the chain: unspent
	store.put(chain[coneChainLength-1], &meta.Data{})

	return parentHash, txUnderCheck, chain[0]
}

// The counter-conflicting check must cost O(N) store reads, not O(N^2). Before the
// fix it walked the full descendant cone once per element of a slice that already
// IS the cone: 1002 reads to build the set (see GetCounterConflictingTxHashes) plus
// sum(1..1000) = 500500 for re-walking the chain elements plus 1 more for re-walking
// the tx-under-check itself, which is also a member of that slice — 501503 reads
// total, the 26-minute mainnet stall at 960,827.
func TestCheckCounterConflictingOnCurrentChain_CostIsLinear(t *testing.T) {
	ctx := context.Background()
	store := newCountingStore(t)

	_, txUnderCheck, _ := plantLinearChain(t, store)

	u := &Server{utxoStore: store, logger: ulogger.New("test")}

	require.NoError(t, u.checkCounterConflictingOnCurrentChain(ctx, txUnderCheck, map[uint32]bool{}))

	// Post-fix: 1 (X.Tx) + 1 (parent utxos) + coneChainLength (the single cone walk)
	// + 1 (the one walk rooted at X) = coneChainLength + 3.
	require.Equal(t, int64(coneChainLength+3), store.gets(),
		"descendant walk cost is not linear in the chain length — the per-element re-walk is back")

	// The mined-on-our-chain check still reads every counter's meta exactly once.
	require.Equal(t, int64(coneChainLength+1), store.getMetas())
}

// Equivalence guard: a frozen output directly on the counter-conflicting slot must
// still be rejected. Passes both before and after the fix — that is the point.
func TestCheckCounterConflictingOnCurrentChain_RejectsFrozenCounter(t *testing.T) {
	ctx := context.Background()
	store := newCountingStore(t)

	parentHash := chainhash.HashH([]byte("frozen-counter-parent"))
	txUnderCheck := chainhash.HashH([]byte("frozen-counter-tx"))

	store.put(parentHash, &meta.Data{SpendingDatas: spentBy(subtree.CoinbasePlaceholderHashValue)})
	store.put(txUnderCheck, &meta.Data{Tx: txSpending(t, parentHash)})

	u := &Server{utxoStore: store, logger: ulogger.New("test")}

	err := u.checkCounterConflictingOnCurrentChain(ctx, txUnderCheck, map[uint32]bool{})

	require.Error(t, err)
	require.Contains(t, err.Error(), "frozen")
}

// Equivalence guard: a frozen output deep inside the counter's descendant cone must
// still be rejected. This one is caught inside GetCounterConflictingTxHashes, which
// is why the per-element re-walk was redundant for it.
func TestCheckCounterConflictingOnCurrentChain_RejectsFrozenDeepInCounterCone(t *testing.T) {
	ctx := context.Background()
	store := newCountingStore(t)

	parentHash := chainhash.HashH([]byte("frozen-deep-parent"))
	txUnderCheck := chainhash.HashH([]byte("frozen-deep-tx"))
	counterHash := chainhash.HashH([]byte("frozen-deep-counter"))
	midHash := chainhash.HashH([]byte("frozen-deep-mid"))

	store.put(parentHash, &meta.Data{SpendingDatas: spentBy(counterHash)})
	store.put(txUnderCheck, &meta.Data{Tx: txSpending(t, parentHash)})
	store.put(counterHash, &meta.Data{SpendingDatas: spentBy(midHash)})
	store.put(midHash, &meta.Data{SpendingDatas: spentBy(subtree.CoinbasePlaceholderHashValue)})

	u := &Server{utxoStore: store, logger: ulogger.New("test")}

	err := u.checkCounterConflictingOnCurrentChain(ctx, txUnderCheck, map[uint32]bool{})

	require.Error(t, err)
	require.Contains(t, err.Error(), "frozen")
}

// Equivalence guard for the one term the deleted loop uniquely covered: a frozen
// output inside the descendant cone of the transaction under CHECK, which no
// earlier walk visits. This is why the fix keeps one walk instead of zero — delete
// the remaining GetConflictingChildren call and this test fails.
func TestCheckCounterConflictingOnCurrentChain_RejectsFrozenInOwnCone(t *testing.T) {
	ctx := context.Background()
	store := newCountingStore(t)

	parentHash := chainhash.HashH([]byte("frozen-own-parent"))
	txUnderCheck := chainhash.HashH([]byte("frozen-own-tx"))
	counterHash := chainhash.HashH([]byte("frozen-own-counter"))
	childHash := chainhash.HashH([]byte("frozen-own-child"))

	store.put(parentHash, &meta.Data{SpendingDatas: spentBy(counterHash)})
	store.put(txUnderCheck, &meta.Data{
		Tx:            txSpending(t, parentHash),
		SpendingDatas: spentBy(childHash),
	})
	store.put(counterHash, &meta.Data{})
	store.put(childHash, &meta.Data{SpendingDatas: spentBy(subtree.CoinbasePlaceholderHashValue)})

	u := &Server{utxoStore: store, logger: ulogger.New("test")}

	err := u.checkCounterConflictingOnCurrentChain(ctx, txUnderCheck, map[uint32]bool{})

	require.Error(t, err)
	require.Contains(t, err.Error(), "frozen")
}

// Equivalence guard: a counter mined in a block on our current chain is still an
// invalid-transaction rejection, not a processing error.
func TestCheckCounterConflictingOnCurrentChain_StillRejectsMinedCounter(t *testing.T) {
	ctx := context.Background()
	store := newCountingStore(t)

	parentHash := chainhash.HashH([]byte("mined-parent"))
	txUnderCheck := chainhash.HashH([]byte("mined-tx"))
	counterHash := chainhash.HashH([]byte("mined-counter"))

	store.put(parentHash, &meta.Data{SpendingDatas: spentBy(counterHash)})
	store.put(txUnderCheck, &meta.Data{Tx: txSpending(t, parentHash)})
	store.put(counterHash, &meta.Data{BlockIDs: []uint32{7}})

	u := &Server{utxoStore: store, logger: ulogger.New("test")}

	err := u.checkCounterConflictingOnCurrentChain(ctx, txUnderCheck, map[uint32]bool{7: true})

	require.Error(t, err)
	require.True(t, errors.Is(err, errors.ErrTxInvalid))
}
