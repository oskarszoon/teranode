package blockassembly

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/stores/blob"
	blob_memory "github.com/bsv-blockchain/teranode/stores/blob/memory"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// TestHasConflictingNodesInBlocks_DetectsStaleConflicts is a regression test
// for the mainnet incident on teranode-mainnet-eu-1 (2026-05-10) where, after
// a reorg, block assembly kept producing mining candidates rejected by SVNode
// with bad-txns-inputs-missingorspent. Root cause:
//
//  1. Stale block N_stale arrived first with a tx TX_L that double-spent an
//     existing mempool tx TX_W. Validator marked TX_L conflicting, and
//     subtree validation persisted TX_L in the subtree file's
//     ConflictingNodes list.
//
//  2. moveForwardBlock(N_stale) called ProcessConflicting([TX_L]), which
//     swapped the UTXO spender (TX_W → TX_L) and flipped the Conflicting
//     flags accordingly (TX_W=true, TX_L=false).
//
//  3. A reorg arrived: moveBackBlock(N_stale) then moveForwardBlock(N_winner)
//     containing TX_W. moveBackBlock does NOT reverse ProcessConflicting's
//     UTXO side effects. N_winner's subtree was produced upstream before the
//     conflict materialized, so its ConflictingNodes list is empty —
//     ProcessConflicting is not called during moveForward. The inverted state
//     persists: TX_L stays Conflicting=false (along with any unmined
//     descendants) and the candidate keeps including it.
//
// Detection: any moveBackBlock whose subtree carries ConflictingNodes is a
// block whose original moveForward already invoked ProcessConflicting in the
// UTXO store. After such a reorg the unmined side of the UTXO state needs
// validation via validateUnminedTxInputs (Case 2 — counter-conflicting tx
// confirmed on chain → mark conflicting, cascade to descendants).
//
// This test pins the detection behavior: hasConflictingNodesInBlocks must
// return true whenever any referenced subtree on disk has a non-empty
// ConflictingNodes list, and false otherwise.
func TestHasConflictingNodesInBlocks_DetectsStaleConflicts(t *testing.T) {
	tx1 := chainhash.HashH([]byte("tx1-stale-conflict-test"))
	tx2 := chainhash.HashH([]byte("tx2-stale-conflict-test"))
	tx3 := chainhash.HashH([]byte("tx3-stale-conflict-test"))

	cleanSubtree := buildSubtreeWithConflictingNodes(t, []chainhash.Hash{tx1, tx2}, nil)
	conflictingSubtree := buildSubtreeWithConflictingNodes(t, []chainhash.Hash{tx1, tx2, tx3}, []chainhash.Hash{tx3})

	tests := []struct {
		name        string
		setup       func(t *testing.T, ba *BlockAssembler) []*model.Block
		expectFound bool
	}{
		{
			name: "empty block list returns false",
			setup: func(t *testing.T, ba *BlockAssembler) []*model.Block {
				return nil
			},
			expectFound: false,
		},
		{
			name: "block with no subtrees returns false",
			setup: func(t *testing.T, ba *BlockAssembler) []*model.Block {
				return []*model.Block{{Subtrees: nil}}
			},
			expectFound: false,
		},
		{
			name: "block with subtree that has empty ConflictingNodes returns false",
			setup: func(t *testing.T, ba *BlockAssembler) []*model.Block {
				storeSubtreeInBlob(t, ba.subtreeStore, cleanSubtree)
				return []*model.Block{
					{Subtrees: []*chainhash.Hash{cleanSubtree.RootHash()}},
				}
			},
			expectFound: false,
		},
		{
			name: "block with subtree that carries ConflictingNodes returns true",
			setup: func(t *testing.T, ba *BlockAssembler) []*model.Block {
				storeSubtreeInBlob(t, ba.subtreeStore, conflictingSubtree)
				return []*model.Block{
					{Subtrees: []*chainhash.Hash{conflictingSubtree.RootHash()}},
				}
			},
			expectFound: true,
		},
		{
			name: "any block having a conflicting subtree wins (clean + conflicting mixed)",
			setup: func(t *testing.T, ba *BlockAssembler) []*model.Block {
				storeSubtreeInBlob(t, ba.subtreeStore, cleanSubtree)
				storeSubtreeInBlob(t, ba.subtreeStore, conflictingSubtree)
				return []*model.Block{
					{Subtrees: []*chainhash.Hash{cleanSubtree.RootHash()}},
					{Subtrees: []*chainhash.Hash{conflictingSubtree.RootHash()}},
				}
			},
			expectFound: true,
		},
		{
			name: "missing subtree on disk is tolerated (treated as no conflicts)",
			setup: func(t *testing.T, ba *BlockAssembler) []*model.Block {
				phantom := chainhash.HashH([]byte("phantom-subtree"))
				return []*model.Block{
					{Subtrees: []*chainhash.Hash{&phantom}},
				}
			},
			expectFound: false,
		},
		{
			name: "nil block entry is skipped",
			setup: func(t *testing.T, ba *BlockAssembler) []*model.Block {
				storeSubtreeInBlob(t, ba.subtreeStore, conflictingSubtree)
				return []*model.Block{
					nil,
					{Subtrees: []*chainhash.Hash{conflictingSubtree.RootHash()}},
				}
			},
			expectFound: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ba := &BlockAssembler{
				logger:       ulogger.TestLogger{},
				subtreeStore: blob_memory.New(),
			}

			blocks := tc.setup(t, ba)

			got := ba.hasConflictingNodesInBlocks(context.Background(), blocks)
			require.Equal(t, tc.expectFound, got)
		})
	}
}

func buildSubtreeWithConflictingNodes(t *testing.T, nodes []chainhash.Hash, conflicting []chainhash.Hash) *subtreepkg.Subtree {
	t.Helper()

	// Nearest power-of-two leaf count ≥ len(nodes), minimum 2 because
	// NewTreeByLeafCount rejects single-leaf trees.
	leafCount := 2
	for leafCount < len(nodes) {
		leafCount *= 2
	}

	s, err := subtreepkg.NewTreeByLeafCount(leafCount)
	require.NoError(t, err)

	for _, h := range nodes {
		require.NoError(t, s.AddNode(h, 1, 100))
	}

	for _, h := range conflicting {
		require.NoError(t, s.AddConflictingNode(h))
	}

	return s
}

func storeSubtreeInBlob(t *testing.T, store blob.Store, s *subtreepkg.Subtree) {
	t.Helper()

	bytes, err := s.Serialize()
	require.NoError(t, err)

	require.NoError(t, store.Set(context.Background(), s.RootHash()[:], fileformat.FileTypeSubtree, bytes))
}
