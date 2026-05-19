package model

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// makeSubtree builds a subtree with leafCount unique random transactions.
func makeSubtree(t *testing.T, leafCount int) *subtreepkg.Subtree {
	t.Helper()
	st, err := subtreepkg.NewTreeByLeafCount(leafCount)
	require.NoError(t, err)
	for i := 0; i < leafCount; i++ {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		h, err := chainhash.NewHash(b)
		require.NoError(t, err)
		require.NoError(t, st.AddNode(*h, 1, 0))
	}
	return st
}

// TestCheckDuplicateTransactions_LargeBlockNotQuadratic verifies that the O(1)
// base-index fix completes within budget for a block large enough to expose the
// O(N²) regression from PR #198 on any reasonable machine.
//
// N=20000 subtrees: old code executes ~200M Size() calls (each with RLock/RUnlock);
// fixed code executes 0 loop iterations. Wall-time budget is deliberately generous
// (30s) to avoid flakiness on slow CI while still catching a quadratic regression.
func TestCheckDuplicateTransactions_LargeBlockNotQuadratic(t *testing.T) {
	const numSubtrees = 20_000
	const leafCount = 4

	tSettings := test.CreateBaseTestSettings(t)

	subtrees := make([]*subtreepkg.Subtree, numSubtrees)
	for i := range subtrees {
		subtrees[i] = makeSubtree(t, leafCount)
	}

	// Use a txMap large enough for all transactions.
	totalTxCount := uint32(numSubtrees * leafCount) //nolint:gosec
	block := &Block{
		SubtreeSlices:    subtrees,
		TransactionCount: uint64(totalTxCount),
		txMap:            GetTxMap(totalTxCount),
	}

	start := time.Now()
	err := block.checkDuplicateTransactions(context.Background(), ulogger.TestLogger{}, tSettings.Block.CheckDuplicateTransactionsConcurrency, nil)
	elapsed := time.Since(start)

	require.NoError(t, err)
	// O(N²) at N=20000 with mutex contention takes tens of seconds; O(1) completes in milliseconds.
	require.Less(t, elapsed, 30*time.Second, "checkDuplicateTransactions took too long (%v); likely O(N²) regression", elapsed)

	t.Logf("checkDuplicateTransactions over %d subtrees completed in %v", numSubtrees, elapsed)
}

// TestCheckDuplicateTransactionsInSubtree_BaseIndexCorrectness verifies that the
// O(1) base-index formula produces correct global indices for all subtree positions,
// including when subIdx=0, a full middle subtree, and a smaller last subtree.
func TestCheckDuplicateTransactionsInSubtree_BaseIndexCorrectness(t *testing.T) {
	t.Run("single subtree subIdx=0 baseIdx=0", func(t *testing.T) {
		st := makeSubtree(t, 4)
		block := &Block{
			SubtreeSlices: []*subtreepkg.Subtree{st},
			txMap:         GetTxMap(4),
		}
		require.NoError(t, block.checkDuplicateTransactionsInSubtree(st, 0, st.Size()))

		// All 4 nodes should be recorded at indices 0–3.
		for i, node := range st.Nodes {
			idx, exists := block.txMap.Get(node.Hash)
			require.True(t, exists, "node %d missing from txMap", i)
			require.Equal(t, uint64(i), idx, "node %d has wrong index", i)
		}
	})

	t.Run("smaller last subtree gets correct base", func(t *testing.T) {
		// Two full subtrees (size=4) + one partial subtree (2 nodes in a size-4 tree).
		st1 := makeSubtree(t, 4)
		st2 := makeSubtree(t, 4)
		st3, err := subtreepkg.NewTreeByLeafCount(4)
		require.NoError(t, err)

		smallHashes := make([]chainhash.Hash, 2)
		for i := range smallHashes {
			b := make([]byte, 32)
			_, _ = rand.Read(b)
			h, err := chainhash.NewHash(b)
			require.NoError(t, err)
			smallHashes[i] = *h
			require.NoError(t, st3.AddNode(*h, 1, 0))
		}

		subtreeSize := st1.Size() // 4
		block := &Block{
			SubtreeSlices: []*subtreepkg.Subtree{st1, st2, st3},
			txMap:         GetTxMap(10),
		}

		require.NoError(t, block.checkDuplicateTransactionsInSubtree(st1, 0, subtreeSize))
		require.NoError(t, block.checkDuplicateTransactionsInSubtree(st2, 1, subtreeSize))
		require.NoError(t, block.checkDuplicateTransactionsInSubtree(st3, 2, subtreeSize))

		// st3 starts at baseIdx = 2*4 = 8.
		for i, h := range smallHashes {
			idx, exists := block.txMap.Get(h)
			require.True(t, exists, "small subtree node %d missing", i)
			require.Equal(t, uint64(8+i), idx, "small subtree node %d wrong index", i)
		}
	})

	t.Run("subtreeSize=1 degenerate", func(t *testing.T) {
		st1, err := subtreepkg.NewTreeByLeafCount(1)
		require.NoError(t, err)
		b1 := make([]byte, 32)
		_, _ = rand.Read(b1)
		h1, err := chainhash.NewHash(b1)
		require.NoError(t, err)
		require.NoError(t, st1.AddNode(*h1, 1, 0))

		st2, err := subtreepkg.NewTreeByLeafCount(1)
		require.NoError(t, err)
		b2 := make([]byte, 32)
		_, _ = rand.Read(b2)
		h2, err := chainhash.NewHash(b2)
		require.NoError(t, err)
		require.NoError(t, st2.AddNode(*h2, 1, 0))

		block := &Block{
			SubtreeSlices: []*subtreepkg.Subtree{st1, st2},
			txMap:         GetTxMap(2),
		}

		require.NoError(t, block.checkDuplicateTransactionsInSubtree(st1, 0, 1))
		require.NoError(t, block.checkDuplicateTransactionsInSubtree(st2, 1, 1))

		idx1, ok1 := block.txMap.Get(*h1)
		idx2, ok2 := block.txMap.Get(*h2)
		require.True(t, ok1)
		require.True(t, ok2)
		require.Equal(t, uint64(0), idx1)
		require.Equal(t, uint64(1), idx2)
	})

	t.Run("duplicate in second subtree detected", func(t *testing.T) {
		// Put the same hash in st1 and st2 — dedup must fire.
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		dupHash, err := chainhash.NewHash(b)
		require.NoError(t, err)

		// st1 contains dupHash at position 0 followed by 3 unique nodes.
		st1, err := subtreepkg.NewTreeByLeafCount(4)
		require.NoError(t, err)
		require.NoError(t, st1.AddNode(*dupHash, 1, 0))
		for i := 1; i < 4; i++ {
			rb := make([]byte, 32)
			_, _ = rand.Read(rb)
			h, _ := chainhash.NewHash(rb)
			require.NoError(t, st1.AddNode(*h, 1, 0))
		}

		// st2 opens with the same dupHash — should trigger the duplicate check.
		st2, err := subtreepkg.NewTreeByLeafCount(4)
		require.NoError(t, err)
		require.NoError(t, st2.AddNode(*dupHash, 1, 0))

		subtreeSize := st1.Size()
		block := &Block{
			SubtreeSlices: []*subtreepkg.Subtree{st1, st2},
			txMap:         GetTxMap(8),
		}

		require.NoError(t, block.checkDuplicateTransactionsInSubtree(st1, 0, subtreeSize))
		err = block.checkDuplicateTransactionsInSubtree(st2, 1, subtreeSize)
		require.Error(t, err, "duplicate transaction must be detected")
		require.Contains(t, err.Error(), "duplicate transaction")
	})
}
