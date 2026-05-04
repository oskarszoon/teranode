package subtreeprocessor

import (
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDequeue_NoParentConflictingCheck_BUG demonstrates the production bug
// observed on teranode-mainnet-eu-1 (v0.15.0-beta-3): a child transaction whose
// parent is already marked Conflicting=true in the UTXO store still lands in the
// block-assembly subtree. The resulting mining candidate fails ValidateBlock with
// "parent transaction X of tx Y has no block IDs" and the block is rejected with
// "bad-txns-inputs-missingorspent" by SVNode.
//
// Race in production:
//
//	T0  parent P added to UTXO store via validator.
//	T1  ProcessConflicting (during moveForwardBlock with ConflictingNodes) flags
//	    P.Conflicting=true. Cascade walks P.outputs -> recorded spenders, finds
//	    none for child C: C's Spend has not been committed yet (C is mid-flight
//	    in the BA queue). Cascade misses C; C.Conflicting stays false in store.
//	T2  Event loop returns to default case, drains queue. Phase 2 filter
//	    (SubtreeProcessor.go:861-886) only checks removeMap and currentTxMap
//	    dedup. No Conflicting check on self or any parent in TxInpoints.
//	T3  C lands in subtree. Mining candidate built and submitted. REJECTED.
//
// Even the alternative drain path used during block movement,
// dequeueDuringBlockMovement at SubtreeProcessor.go:3715, only filters by
// `losingTxHashesMap.Exists(node.Hash)` — its own hash, never parents. And the
// losing map itself is built in ProcessConflicting (process_conflicting.go:109)
// from GetCounterConflicting hashes only; the cascaded descendants returned by
// MarkConflictingRecursively (line 122) are discarded. So even when the cascade
// does discover a descendant, that hash never reaches the dequeue filter.
//
// This test proves the queue-drain side of the gap: the subtree-processor
// dequeue path has no mechanism to reject a child whose parent the system
// considers conflicting — because there is no map/store lookup at all on the
// parent inpoints during dequeue.
//
// Fix shape (per design discussion):
//   - Introduce conflictingMap on SubtreeProcessor (separate from removeMap).
//   - ProcessConflicting / MarkConflictingRecursively populate it recursively
//     with all known-conflicting hashes (the cascaded descendants currently
//     thrown away at process_conflicting.go:122 must be captured into it).
//   - Phase 2 dequeue filter (SubtreeProcessor.go:861-886) and
//     dequeueDuringBlockMovement (3731) reject any node whose Hash is in the
//     map OR whose TxInpoints.ParentTxHashes contains a hash in the map.
//   - When rejected at dequeue, the rejected child Hash is itself added to the
//     map so any later-arriving descendants are also caught.
//
// Status: this test FAILS under current code (child is admitted) and is
// expected to PASS once the conflictingMap fix is in place.
func TestDequeue_NoParentConflictingCheck_BUG(t *testing.T) {
	stp := setupTestSubtreeProcessor(t)

	// We do not need the parent to actually be in the UTXO store to prove the
	// bug. Production has the parent marked Conflicting=true in the store, but
	// the dequeue path never reads the store for parent inpoints, so the test
	// is just as faithful with a synthetic parent hash. (Avoiding real
	// SetConflicting also avoids sqlitememory single-writer lock contention
	// during this test.)
	parentHash := chainhash.HashH([]byte("conflicting-parent"))
	childHash := chainhash.HashH([]byte("child-of-conflicting-parent"))

	// Populate the SubtreeProcessor's conflictingMap to reflect what
	// processConflictingTransactions / BlockAssembler.markAsConflicting would
	// have published when the parent was flagged Conflicting=true in the
	// store. The dequeue path must consult this and reject the child.
	stp.MarkConflicting([]chainhash.Hash{parentHash})

	childNode := subtreepkg.Node{Hash: childHash, Fee: 1, SizeInBytes: 250}
	childInpoints := &subtreepkg.TxInpoints{
		ParentTxHashes: []chainhash.Hash{parentHash},
		Idxs:           [][]uint32{{0}},
	}

	stp.AddBatch([]subtreepkg.Node{childNode}, []*subtreepkg.TxInpoints{childInpoints})

	require.Eventually(t, func() bool { return stp.QueueLength() == 0 },
		2*time.Second, 5*time.Millisecond, "queue did not drain")

	// Allow the goroutine one more iteration to complete the insert path.
	time.Sleep(50 * time.Millisecond)

	found := false
	for _, h := range stp.GetTransactionHashes() {
		if h.Equal(childHash) {
			found = true
			break
		}
	}

	assert.False(t, found,
		"BUG: child of conflicting parent admitted to subtree.\n"+
			"  parent: %s (treated as Conflicting=true)\n"+
			"  child:  %s (admitted into BA subtree)\n"+
			"Cause: SubtreeProcessor.go:861-886 (Phase 2 dequeue filter) and\n"+
			"SubtreeProcessor.go:3731 (dequeueDuringBlockMovement) consult only\n"+
			"removeMap, currentTxMap dedup, and losingTxHashesMap by self-hash.\n"+
			"Neither path checks TxInpoints.ParentTxHashes against any\n"+
			"conflicting-state source. Cascade hashes from\n"+
			"MarkConflictingRecursively are also discarded at\n"+
			"process_conflicting.go:122, so they never reach the filter.\n"+
			"Fix: add conflictingMap, populated recursively, consulted in\n"+
			"Phase 2 filter for both self-hash and parent-inpoints.",
		parentHash.String(), childHash.String())
}
