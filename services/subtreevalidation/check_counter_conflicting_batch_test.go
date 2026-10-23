package subtreevalidation

import (
	"context"
	"math/rand/v2"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/blob/memory"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// readCountingUtxoStore wraps a real utxo.Store and counts every record read the
// conflict check can issue: single Gets, GetMetas, and each item of a
// BatchDecorate.
type readCountingUtxoStore struct {
	utxo.Store
	reads atomic.Int64
}

func (c *readCountingUtxoStore) Get(ctx context.Context, hash *chainhash.Hash, f ...fields.FieldName) (*meta.Data, error) {
	c.reads.Add(1)
	return c.Store.Get(ctx, hash, f...)
}

func (c *readCountingUtxoStore) GetMeta(ctx context.Context, hash *chainhash.Hash, data *meta.Data) error {
	c.reads.Add(1)
	return c.Store.GetMeta(ctx, hash, data)
}

func (c *readCountingUtxoStore) BatchDecorate(ctx context.Context, items []*utxo.UnresolvedMetaData, f ...fields.FieldName) error {
	c.reads.Add(int64(len(items)))
	return c.Store.BatchDecorate(ctx, items, f...)
}

func newSQLUtxoStore(t *testing.T, ctx context.Context, tSettings *settings.Settings) utxo.Store {
	t.Helper()

	utxoStoreURL, err := url.Parse("sqlitememory:///test")
	require.NoError(t, err)

	utxoStore, err := sql.New(ctx, ulogger.NewErrorTestLogger(t), tSettings, utxoStoreURL)
	require.NoError(t, err)

	return utxoStore
}

// createConflicting stores tx flagged Conflicting without spending its inputs:
// the store records it as a conflicting child of each parent, not as a spender.
func createConflicting(t *testing.T, ctx context.Context, store utxo.Store, tx *bt.Tx) {
	t.Helper()

	_, _, err := store.SpendAndCreate(ctx, tx, 123, utxo.WithConflicting(true), utxo.WithCreateOnly())
	require.NoError(t, err)
}

// spendAndCreate stores tx as the recorded spender of its inputs, even where a
// parent is conflicting.
func spendAndCreate(t *testing.T, ctx context.Context, store utxo.Store, tx *bt.Tx, opts ...utxo.CreateOption) {
	t.Helper()

	_, _, err := store.SpendAndCreate(ctx, tx, 122, utxo.WithSpendOnly(), utxo.WithIgnoreConflicting(true))
	require.NoError(t, err)

	_, _, err = store.SpendAndCreate(ctx, tx, 122, append([]utxo.CreateOption{utxo.WithCreateOnly()}, opts...)...)
	require.NoError(t, err)
}

// buildConflictingChain stores the issue 2037 shape: counter is the recorded
// spender of the parentTx1 output that tx1 also spends, and tx1 heads a linear
// chain of n conflicting transactions, each a conflicting child of the one before.
// The chain is returned head first.
func buildConflictingChain(t *testing.T, ctx context.Context, store utxo.Store, n int, counterOpts ...utxo.CreateOption) (*bt.Tx, []chainhash.Hash) {
	t.Helper()

	_, _, err := store.SpendAndCreate(ctx, parentTx1, 122, utxo.WithCreateOnly())
	require.NoError(t, err)

	counter := tx1.Clone()
	counter.Version = 2
	spendAndCreate(t, ctx, store, counter, counterOpts...)

	chain := make([]chainhash.Hash, 0, n)

	prev := tx1
	createConflicting(t, ctx, store, prev)
	chain = append(chain, *prev.TxIDChainHash())

	for len(chain) < n {
		child := spendingChild(t, prev, 0)
		createConflicting(t, ctx, store, child)
		chain = append(chain, *child.TxIDChainHash())
		prev = child
	}

	return counter, chain
}

// newCachedCheckServer returns a server over store, and a block whose single
// cached subtree names conflictingTxs in its conflicting-node trailer. The block's
// candidate chain is blockIDs.
func newCachedCheckServer(t *testing.T, store utxo.Store, tSettings *settings.Settings, conflictingTxs []chainhash.Hash, blockIDs []uint32) (*Server, *model.Block, []chainhash.Hash) {
	t.Helper()

	st, err := subtreepkg.NewIncompleteTreeByLeafCount(len(conflictingTxs) + 1)
	require.NoError(t, err)
	require.NoError(t, st.AddCoinbaseNode())

	for _, txHash := range conflictingTxs {
		require.NoError(t, st.AddNode(txHash, 1, 1))
		require.NoError(t, st.AddConflictingNode(txHash))
	}

	serialized, err := st.Serialize()
	require.NoError(t, err)

	subtreeStore := memory.New()
	rootHash := st.RootHash()
	require.NoError(t, subtreeStore.Set(context.Background(), rootHash[:], fileformat.FileTypeSubtree, serialized))

	mockBlockchain := &blockchain.Mock{}
	mockBlockchain.On("GetBlockHeaderIDs", mock.Anything, mock.Anything, mock.Anything).Return(blockIDs, nil)

	prevHash := chainhash.HashH([]byte("candidate-parent"))
	block := &model.Block{
		Header: &model.BlockHeader{
			Version:        1,
			HashPrevBlock:  &prevHash,
			HashMerkleRoot: &chainhash.Hash{},
			Timestamp:      12345678,
			Bits:           model.NBit{},
			Nonce:          1,
		},
		Height:   200,
		Subtrees: []*chainhash.Hash{rootHash},
	}

	s := &Server{
		logger:           ulogger.NewErrorTestLogger(t),
		settings:         tSettings,
		utxoStore:        store,
		subtreeStore:     subtreeStore,
		blockchainClient: mockBlockchain,
	}

	return s, block, []chainhash.Hash{*rootHash}
}

// Test_checkCachedSubtreesAgainstCandidateChain_scaling reproduces the issue 2037
// wedge: a block whose cached subtrees name every member of a linear conflicting
// chain. Checked one transaction at a time, every member re-walks the rest of the
// chain, so store reads grow as N^2/2 (a 1,788-transaction chain took about five
// hours). Checked as one batch, each record is read a constant number of times.
//
// The bound is c = 5 reads per conflicting transaction. For this shape the
// batched check needs four: its own inpoints and its parent's spenders (phase 1),
// one read as a member of the walk (phase 2), and one GetMeta as a member of the
// counter-conflicting union (phase 3). The fifth leaves room for the reads that
// discover which cached transactions are conflicting.
func Test_checkCachedSubtreesAgainstCandidateChain_scaling(t *testing.T) {
	const chainLength = 200

	ctx := context.Background()
	tSettings := test.CreateBaseTestSettings(t)

	store := newSQLUtxoStore(t, ctx, tSettings)
	_, chain := buildConflictingChain(t, ctx, store, chainLength)

	countingStore := &readCountingUtxoStore{Store: store}
	s, block, cachedSubtrees := newCachedCheckServer(t, countingStore, tSettings, chain, []uint32{77})

	require.NoError(t, s.checkCachedSubtreesAgainstCandidateChain(ctx, block, cachedSubtrees))

	reads := countingStore.reads.Load()
	t.Logf("%d store reads for a %d-transaction conflicting chain", reads, chainLength)

	require.LessOrEqualf(t, reads, int64(5*chainLength),
		"the cached-subtree conflict check must read a conflicting chain O(N) times, got %d reads for N=%d (O(N^2) is the per-transaction re-walk of issue 2037)",
		reads, chainLength)
}

// verdict is the class of a conflict-check outcome: what block validation acts on.
type verdict string

const (
	verdictPass       verdict = "pass"
	verdictTxInvalid  verdict = "tx-invalid"
	verdictProcessing verdict = "processing"
)

func verdictOf(err error) verdict {
	switch {
	case err == nil:
		return verdictPass
	case errors.Is(err, errors.ErrTxInvalid):
		return verdictTxInvalid
	default:
		return verdictProcessing
	}
}

type outpoint struct {
	tx   *bt.Tx
	vout uint32
}

// buildTx returns a tx spending inputs into nOutputs outputs. salt keeps hashes
// distinct between txs that spend the same inputs.
func buildTx(t *testing.T, inputs []outpoint, nOutputs int, salt uint32) *bt.Tx {
	t.Helper()

	tx := bt.NewTx()
	tx.LockTime = salt

	var total uint64

	for _, in := range inputs {
		out := in.tx.Outputs[in.vout]
		require.NoError(t, tx.From(in.tx.TxIDChainHash().String(), in.vout, out.LockingScript.String(), out.Satoshis))
		total += out.Satoshis
	}

	for _, in := range tx.Inputs {
		in.UnlockingScript = bscript.NewFromBytes([]byte{})
	}

	for i := 0; i < nOutputs; i++ {
		require.NoError(t, tx.AddP2PKHOutputFromScript(parentTx1.Outputs[0].LockingScript, total/uint64(2*nOutputs)))
	}

	return tx
}

// Test_checkCounterConflictingOnCurrentChainBatch_equivalence pins the batched
// check to the per-transaction check it replaces. Each seed builds a random graph
// in a real SQL store: recorded spenders and conflicting txs over shared outputs,
// so conflicting txs have counter-spenders, counter-spender cones, own cones
// through both spends and conflicting children, and members of one tx's sets in
// another's. Some txs are mined on the candidate chain, some on another chain,
// and some outputs are frozen. Every record is present and the walk is unbounded.
//
// The reference is checkCounterConflictingOnCurrentChain run on every conflicting
// tx, reduced the way the batch defines its verdict: any processing error wins,
// then any tx-invalid, else pass. Where the per-transaction results do not mix
// classes, that is also exactly what the cached path's loop returned; where they
// do mix, the loop's answer depended on trailer order.
func Test_checkCounterConflictingOnCurrentChainBatch_equivalence(t *testing.T) {
	const (
		seeds          = 60
		txsPerSeed     = 20
		onChainBlockID = uint32(77)
		offChainBlock  = uint32(99)
	)

	seen := map[verdict]int{}

	tSettings := test.CreateBaseTestSettings(t)
	tSettings.UtxoStore.ConflictingChildrenMaxNodes = 0
	// the generator issues hundreds of sequential spends; don't wait out a
	// batcher window on each
	tSettings.UtxoStore.SpendBatcherDurationMillis = 1
	tSettings.UtxoStore.StoreBatcherDurationMillis = 1
	tSettings.UtxoStore.GetBatcherDurationMillis = 1

	for seed := uint64(1); seed <= seeds; seed++ {
		ctx := context.Background()
		rng := rand.New(rand.NewPCG(seed, 2037))

		store := newSQLUtxoStore(t, ctx, tSettings)

		var unspent, spent []outpoint

		for r := 0; r < 3; r++ {
			fake := bt.NewTx()
			require.NoError(t, fake.AddP2PKHOutputFromScript(parentTx1.Outputs[0].LockingScript, 1_000_000_000))
			fake.LockTime = uint32(seed)*1000 + uint32(r)

			root := buildTx(t, []outpoint{{tx: fake, vout: 0}}, 3, uint32(seed)*1000+uint32(r))
			_, _, err := store.SpendAndCreate(ctx, root, 122, utxo.WithCreateOnly())
			require.NoError(t, err)

			for v := range root.Outputs {
				unspent = append(unspent, outpoint{tx: root, vout: uint32(v)})
			}
		}

		var conflicting []chainhash.Hash

		for i := 0; i < txsPerSeed; i++ {
			isConflicting := rng.IntN(2) == 0

			pool := unspent
			if isConflicting && len(spent) > 0 && rng.IntN(10) < 6 {
				pool = spent
			}

			if len(pool) == 0 {
				continue
			}

			nIn := 1 + rng.IntN(2)
			picked := map[int]struct{}{}

			var inputs []outpoint

			for len(inputs) < nIn && len(picked) < len(pool) {
				idx := rng.IntN(len(pool))
				if _, ok := picked[idx]; ok {
					continue
				}

				picked[idx] = struct{}{}
				inputs = append(inputs, pool[idx])
			}

			tx := buildTx(t, inputs, 1+rng.IntN(3), uint32(seed)*1000+100+uint32(i))

			var opts []utxo.CreateOption

			switch roll := rng.IntN(40); {
			case roll < 3:
				opts = append(opts, utxo.WithMinedBlockInfo(utxo.MinedBlockInfo{BlockID: onChainBlockID, BlockHeight: 100}))
			case roll < 6:
				opts = append(opts, utxo.WithMinedBlockInfo(utxo.MinedBlockInfo{BlockID: offChainBlock, BlockHeight: 100}))
			}

			if isConflicting {
				_, _, err := store.SpendAndCreate(ctx, tx, 123, append(opts, utxo.WithConflicting(true), utxo.WithCreateOnly())...)
				require.NoError(t, err)

				conflicting = append(conflicting, *tx.TxIDChainHash())
			} else {
				spendAndCreate(t, ctx, store, tx, opts...)

				for idx := range picked {
					spent = append(spent, pool[idx])
				}

				remaining := unspent[:0:0]

				for idx, out := range unspent {
					if _, ok := picked[idx]; !ok {
						remaining = append(remaining, out)
					}
				}

				unspent = remaining
			}

			for v := range tx.Outputs {
				unspent = append(unspent, outpoint{tx: tx, vout: uint32(v)})
			}
		}

		// at most one frozen output: the graph is dense enough that a frozen
		// output anywhere tends to reach most cones
		if len(unspent) > 0 && rng.IntN(3) == 0 {
			out := unspent[rng.IntN(len(unspent))]
			freezeOutput(t, ctx, store, out.tx, out.vout, tSettings)
		}

		if len(conflicting) == 0 {
			continue
		}

		s, block, cachedSubtrees := newCachedCheckServer(t, store, tSettings, conflicting, []uint32{onChainBlockID})
		blockIds := map[uint32]bool{onChainBlockID: true}

		want := verdictPass

		for _, txHash := range conflicting {
			err := s.checkCounterConflictingOnCurrentChain(ctx, txHash, blockIds)
			require.NotErrorIs(t, err, errors.ErrTxNotFound, "seed %d: every record is present", seed)

			switch verdictOf(err) {
			case verdictProcessing:
				want = verdictProcessing
			case verdictTxInvalid:
				if want == verdictPass {
					want = verdictTxInvalid
				}
			}
		}

		got := verdictOf(s.checkCachedSubtreesAgainstCandidateChain(ctx, block, cachedSubtrees))
		require.Equalf(t, want, got, "seed %d: batched verdict differs from the per-transaction check", seed)

		seen[want]++
	}

	t.Logf("verdicts over %d seeds: %v", seeds, seen)

	// the generator must exercise every verdict, or the comparison proves little
	for _, v := range []verdict{verdictPass, verdictTxInvalid, verdictProcessing} {
		require.Positivef(t, seen[v], "no seed produced verdict %q; retune the generator", v)
	}
}

// The walk budget covers the union of every cone in the batch. Each
// per-transaction walk of this chain fits the budget; the batch's one walk visits
// the counter-spender plus every chain member, one more than the budget, and must
// fail closed with the retryable walk-limit error rather than pass or reject.
func Test_checkCounterConflictingOnCurrentChainBatch_budgetCoversUnion(t *testing.T) {
	const chainLength = 10

	ctx := context.Background()
	tSettings := test.CreateBaseTestSettings(t)
	tSettings.UtxoStore.ConflictingChildrenMaxNodes = chainLength

	store := newSQLUtxoStore(t, ctx, tSettings)
	_, chain := buildConflictingChain(t, ctx, store, chainLength)

	s, block, cachedSubtrees := newCachedCheckServer(t, store, tSettings, chain, []uint32{77})

	for _, txHash := range chain {
		require.NoError(t, s.checkCounterConflictingOnCurrentChain(ctx, txHash, map[uint32]bool{77: true}),
			"each per-transaction walk fits the budget on its own")
	}

	err := s.checkCachedSubtreesAgainstCandidateChain(ctx, block, cachedSubtrees)
	require.ErrorIs(t, err, errors.ErrUtxoWalkLimitExceeded)
	require.NotErrorIs(t, err, errors.ErrTxInvalid)
}

// A trailer can name a transaction whose record, or whose parent's record, has
// been pruned since the subtree was blessed. Its counter-spenders cannot be
// derived, so it is dropped from the check and the block passes on the rest, as
// the per-transaction loop's ErrTxNotFound skip did. Dropping one must not hide a
// real conflict on another.
func Test_checkCounterConflictingOnCurrentChainBatch_notFoundDropped(t *testing.T) {
	ctx := context.Background()
	tSettings := test.CreateBaseTestSettings(t)

	newFixture := func(t *testing.T, counterOpts ...utxo.CreateOption) (utxo.Store, []chainhash.Hash) {
		store := newSQLUtxoStore(t, ctx, tSettings)
		_, chain := buildConflictingChain(t, ctx, store, 3, counterOpts...)

		// a conflicting tx whose parent the store no longer holds
		orphanParent := buildTx(t, []outpoint{{tx: parentTx1, vout: 0}}, 1, 9001)
		_, _, err := store.SpendAndCreate(ctx, orphanParent, 122, utxo.WithCreateOnly())
		require.NoError(t, err)

		orphan := spendingChild(t, orphanParent, 0)
		createConflicting(t, ctx, store, orphan)
		require.NoError(t, store.Delete(ctx, orphanParent.TxIDChainHash()))

		// and one whose own record is gone
		pruned := chainhash.HashH([]byte("pruned-conflicting-tx"))

		return store, append([]chainhash.Hash{pruned, *orphan.TxIDChainHash()}, chain...)
	}

	t.Run("dropped and the rest passes", func(t *testing.T) {
		store, conflictingTxs := newFixture(t)
		s, block, cachedSubtrees := newCachedCheckServer(t, store, tSettings, conflictingTxs, []uint32{77})

		notFound, err := s.checkCounterConflictingOnCurrentChainBatch(ctx, conflictingTxs, map[uint32]bool{77: true})
		require.NoError(t, err)
		require.ElementsMatch(t, conflictingTxs[:2], notFound)

		require.NoError(t, s.checkCachedSubtreesAgainstCandidateChain(ctx, block, cachedSubtrees))
	})

	t.Run("a real conflict on another tx still rejects", func(t *testing.T) {
		store, conflictingTxs := newFixture(t, utxo.WithMinedBlockInfo(utxo.MinedBlockInfo{BlockID: 77, BlockHeight: 100}))
		s, block, cachedSubtrees := newCachedCheckServer(t, store, tSettings, conflictingTxs, []uint32{77})

		err := s.checkCachedSubtreesAgainstCandidateChain(ctx, block, cachedSubtrees)
		require.ErrorIs(t, err, errors.ErrTxInvalid)
	})
}

// Decision for records missing further out than a transaction and its parents:
// a cone member the store does not hold is a leaf of the walk, and a
// counter-conflicting member whose GetMeta is not found counts as not mined; the
// rest of the union is still checked. The per-transaction check abandoned the
// whole transaction on any missing member, so here it passes a block whose
// counter-spender is mined on the candidate chain, and the batch rejects it.
func Test_checkCounterConflictingOnCurrentChainBatch_missingMemberMidCone(t *testing.T) {
	ctx := context.Background()
	tSettings := test.CreateBaseTestSettings(t)

	store := newSQLUtxoStore(t, ctx, tSettings)
	counter, chain := buildConflictingChain(t, ctx, store, 3, utxo.WithMinedBlockInfo(utxo.MinedBlockInfo{BlockID: 77, BlockHeight: 100}))

	// the counter-spender's child, a member of the counter-conflicting cone, is gone
	counterChild := spendingChild(t, counter, 0)
	spendAndCreate(t, ctx, store, counterChild)
	require.NoError(t, store.Delete(ctx, counterChild.TxIDChainHash()))

	s, block, cachedSubtrees := newCachedCheckServer(t, store, tSettings, chain, []uint32{77})

	err := s.checkCounterConflictingOnCurrentChain(ctx, chain[0], map[uint32]bool{77: true})
	require.ErrorIs(t, err, errors.ErrTxNotFound, "the per-transaction check gives up on a missing member")

	err = s.checkCachedSubtreesAgainstCandidateChain(ctx, block, cachedSubtrees)
	require.ErrorIs(t, err, errors.ErrTxInvalid)
	require.Contains(t, err.Error(), counter.TxIDChainHash().String(), "the verdict names the mined counter-conflicting tx")

	t.Run("a store fault is still a processing error", func(t *testing.T) {
		faulty := &faultyGetMetaStore{Store: store, fail: *counterChild.TxIDChainHash()}
		s, block, cachedSubtrees := newCachedCheckServer(t, faulty, tSettings, chain, []uint32{77})

		err := s.checkCachedSubtreesAgainstCandidateChain(ctx, block, cachedSubtrees)
		require.ErrorIs(t, err, errors.ErrStorageError)
		require.NotErrorIs(t, err, errors.ErrTxInvalid, "a fault anywhere wins over a mined member, so the verdict is retryable")
	})
}

// faultyGetMetaStore fails GetMeta for one hash with a storage error.
type faultyGetMetaStore struct {
	utxo.Store
	fail chainhash.Hash
}

func (f *faultyGetMetaStore) GetMeta(ctx context.Context, hash *chainhash.Hash, data *meta.Data) error {
	if hash.Equal(f.fail) {
		return errors.NewStorageError("store down")
	}

	return f.Store.GetMeta(ctx, hash, data)
}

// A frozen output anywhere the per-transaction check would have looked is a
// processing error from the batch too: on a counter-spender's own input (the
// counter-spender is the frozen sentinel), deep in a counter-spender's cone, and
// in a conflicting transaction's own cone.
func Test_checkCounterConflictingOnCurrentChainBatch_frozen(t *testing.T) {
	ctx := context.Background()
	tSettings := test.CreateBaseTestSettings(t)

	check := func(t *testing.T, store utxo.Store, conflictingTxs []chainhash.Hash, wantMessage string) {
		t.Helper()

		s, block, cachedSubtrees := newCachedCheckServer(t, store, tSettings, conflictingTxs, []uint32{77})

		err := s.checkCachedSubtreesAgainstCandidateChain(ctx, block, cachedSubtrees)
		require.Error(t, err)
		require.NotErrorIs(t, err, errors.ErrTxInvalid)
		require.Contains(t, err.Error(), wantMessage)
	}

	t.Run("frozen direct counter", func(t *testing.T) {
		store := newSQLUtxoStore(t, ctx, tSettings)

		_, _, err := store.SpendAndCreate(ctx, parentTx1, 122, utxo.WithCreateOnly())
		require.NoError(t, err)

		freezeOutput(t, ctx, store, parentTx1, 2, tSettings)
		createConflicting(t, ctx, store, tx1)

		check(t, store, []chainhash.Hash{*tx1.TxIDChainHash()}, "counter conflicting tx is frozen")
	})

	t.Run("frozen deep in counter cone", func(t *testing.T) {
		store := newSQLUtxoStore(t, ctx, tSettings)
		counter, chain := buildConflictingChain(t, ctx, store, 3)

		counterChild := spendingChild(t, counter, 0)
		spendAndCreate(t, ctx, store, counterChild)
		freezeOutput(t, ctx, store, counterChild, 0, tSettings)

		check(t, store, chain, "a counter-conflicting tx has frozen child")
	})

	t.Run("frozen in own cone", func(t *testing.T) {
		store := newSQLUtxoStore(t, ctx, tSettings)
		_, chain := buildConflictingChain(t, ctx, store, 3)

		// the chain tip's output is unspent; freezing it puts the sentinel in
		// every chain member's own cone and in no counter-spender's
		tip, err := store.Get(ctx, &chain[len(chain)-1], fields.Tx)
		require.NoError(t, err)
		freezeOutput(t, ctx, store, tip.Tx, 0, tSettings)

		check(t, store, chain, "child transaction is frozen")
	})
}

// The fresh path asks the same question of every transaction the store flags
// Conflicting, from concurrent decorate batches. It must ask it once for the
// whole set after every batch has decorated, not once per transaction inside
// each batch, and a genuine conflict must still surface as tx-invalid through its
// wrapping. Unlike the cached path, a transaction whose counter-spenders cannot
// be derived is still a retryable failure there, as it was before.
func Test_processTxMetaUsingStore_batchedConflictCheck(t *testing.T) {
	const chainLength = 60

	ctx := context.Background()

	newFixture := func(t *testing.T, counterOpts ...utxo.CreateOption) (*readCountingUtxoStore, *Server, []chainhash.Hash) {
		tSettings := test.CreateBaseTestSettings(t)
		// several concurrent decorate batches, each seeing conflicting txs
		tSettings.BlockValidation.ProcessTxMetaUsingStoreBatchSize = 8
		tSettings.BlockValidation.ProcessTxMetaUsingStoreConcurrency = 4

		store := newSQLUtxoStore(t, ctx, tSettings)
		_, chain := buildConflictingChain(t, ctx, store, chainLength, counterOpts...)

		countingStore := &readCountingUtxoStore{Store: store}

		return countingStore, &Server{logger: ulogger.NewErrorTestLogger(t), settings: tSettings, utxoStore: countingStore}, chain
	}

	run := func(s *Server, txHashes []chainhash.Hash) error {
		_, err := s.processTxMetaUsingStore(ctx, txHashes, make([]metaSliceItem, len(txHashes)), map[uint32]bool{77: true}, true, false, true)
		return err
	}

	t.Run("one batched check", func(t *testing.T) {
		countingStore, s, chain := newFixture(t)

		require.NoError(t, run(s, chain))

		// c = 6: one decorate read per tx, the batched check's four (see the
		// cached-path scaling test), and one of headroom. The per-transaction
		// check needed ~N^2/2.
		reads := countingStore.reads.Load()
		t.Logf("%d store reads for a %d-transaction conflicting chain", reads, chainLength)
		require.LessOrEqual(t, reads, int64(6*chainLength))
	})

	t.Run("a genuine conflict is tx-invalid", func(t *testing.T) {
		_, s, chain := newFixture(t, utxo.WithMinedBlockInfo(utxo.MinedBlockInfo{BlockID: 77, BlockHeight: 100}))

		require.ErrorIs(t, run(s, chain), errors.ErrTxInvalid)
	})

	t.Run("an underivable tx is still a retryable failure", func(t *testing.T) {
		countingStore, s, chain := newFixture(t)

		// chain[1]'s parent, chain[0], is gone; chain[0] is not asked about
		require.NoError(t, countingStore.Delete(ctx, &chain[0]))

		err := run(s, chain[1:])
		require.ErrorIs(t, err, errors.ErrTxNotFound)
		require.NotErrorIs(t, err, errors.ErrTxInvalid)
	})
}
