package blockassembly

import (
	"context"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockassembly/subtreeprocessor"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/stretchr/testify/require"
)

// Pause only snapshot acquisition, retaining the real dispatcher, storage,
// publication lock and mmap lease lifecycle underneath it.
type recoveryCandidateSnapshotBarrier struct {
	subtreeprocessor.Interface
	beforePrecomputed func()
	beforeIncomplete  func()
	acquiredLease     *subtreeprocessor.MiningSnapshotLease
}

func (s *recoveryCandidateSnapshotBarrier) GetPrecomputedMiningData() *subtreeprocessor.PrecomputedMiningData {
	if s.beforePrecomputed != nil {
		s.beforePrecomputed()
	}
	data := s.Interface.GetPrecomputedMiningData()
	if data != nil {
		s.acquiredLease = data.Lease
	}
	return data
}

func (s *recoveryCandidateSnapshotBarrier) GetIncompleteSubtreeMiningData(ctx context.Context) *subtreeprocessor.PrecomputedMiningData {
	if s.beforeIncomplete != nil {
		s.beforeIncomplete()
	}
	return s.Interface.GetIncompleteSubtreeMiningData(ctx)
}

func TestMiningCandidateRejectsSnapshotAcquiredAcrossRecovery(t *testing.T) {
	for _, complete := range []bool{false, true} {
		name := "incomplete"
		if complete {
			name = "completed mmap"
		}
		t.Run(name, func(t *testing.T) {
			b, _ := newUnminedRecoveryTestAssembler(t, blockchain.FSMStateRUNNING, func(s *settings.Settings) {
				s.BlockAssembly.InitialMerkleItemsPerSubtree = 8
				if complete {
					s.BlockAssembly.InitialMerkleItemsPerSubtree = 2
					s.BlockAssembly.StoreTxInpointsForSubtreeMeta = false
				}
				s.BlockAssembly.SubtreeMmapDir = t.TempDir()
			})
			hashes := storeRecoverySelectionChain(t, b, 1)
			processor := b.subtreeProcessor
			if complete {
				require.True(t, b.AddTxBatchIfRoom([]subtree.Node{{Hash: hashes[0], Fee: 1, SizeInBytes: 100}}, []*subtree.TxInpoints{{}}))
				require.Eventually(t, func() bool {
					data := processor.GetPrecomputedMiningData()
					if data == nil {
						return false
					}
					defer data.Lease.Release()
					return len(data.Subtrees) == 1 && data.Subtrees[0].IsMmapBacked()
				}, time.Second, time.Millisecond, "fixture must complete a real mmap subtree before snapshot acquisition")
			}
			entered, release := make(chan struct{}), make(chan struct{})
			barrier := &recoveryCandidateSnapshotBarrier{Interface: processor}
			wait := func() { close(entered); <-release }
			if complete {
				barrier.beforePrecomputed = wait
			} else {
				barrier.beforeIncomplete = wait
			}
			b.subtreeProcessor = barrier
			type result struct {
				candidate *model.MiningCandidate
				trees     []*subtree.Subtree
				lease     *subtreeprocessor.MiningSnapshotLease
				err       error
			}
			finished := make(chan result, 1)
			go func() {
				candidate, trees, lease, err := b.GetMiningCandidate(t.Context())
				finished <- result{candidate: candidate, trees: trees, lease: lease, err: err}
			}()
			// Always release the blocked reader before the real processor's cleanup.
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("candidate did not reach snapshot acquisition")
			}
			header, _ := b.CurrentBlock()
			require.NoError(t, processor.RecoverUnmined(t.Context(), header, hashes,
				func(ctx context.Context, candidates []chainhash.Hash, accepted func(chainhash.Hash) bool) ([]*utxo.UnminedTransaction, error) {
					selected, err := b.prepareUnminedRecovery(ctx, candidates, accepted)
					if err == nil {
						b.recoveryMiningBlocked.Store(true)
					}
					return selected, err
				}))
			require.False(t, processor.RecoveryPending(), "publication completed, but BA has not reconciled its anchor")
			close(release)
			released = true
			var got result
			select {
			case got = <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("candidate did not finish after snapshot acquisition")
			}
			defer got.lease.Release()
			require.Error(t, got.err, "a call admitted before recovery must not serve the newly repaired anchor before reconciliation")
			require.Nil(t, got.candidate)
			require.Nil(t, got.trees)
			require.Nil(t, got.lease)
			if complete {
				require.NotNil(t, barrier.acquiredLease, "fixture must exercise real mmap ownership")
				retained, ok := barrier.acquiredLease.Retain()
				defer retained.Release()
				require.False(t, ok, "rejected candidate must release its acquired mmap lease")
			}
		})
	}
}

// A real authoritative read is the last asynchronous dependency while building
// a candidate. Close recovery admission as that read completes, after the
// initial checks and any snapshot acquisition have already happened.
type recoveryCandidateChainRead struct {
	blockchain.ClientI
	afterRead func()
}

func (c *recoveryCandidateChainRead) GetBlockHeaders(ctx context.Context, hash *chainhash.Hash, count uint64) ([]*model.BlockHeader, []*model.BlockHeaderMeta, error) {
	headers, metadata, err := c.ClientI.GetBlockHeaders(ctx, hash, count)
	c.afterRead()
	return headers, metadata, err
}

func TestMiningCandidateRechecksRecoveryBeforeReturning(t *testing.T) {
	for _, mode := range []string{"populated", "empty", "moving block"} {
		t.Run(mode, func(t *testing.T) {
			b, _ := newUnminedRecoveryTestAssembler(t, blockchain.FSMStateRUNNING)
			if mode == "populated" {
				hashes := storeRecoverySelectionChain(t, b, 1)
				header, _ := b.CurrentBlock()
				require.NoError(t, b.subtreeProcessor.RecoverUnmined(t.Context(), header, hashes, b.prepareUnminedRecovery))
			}
			if mode == "moving block" {
				b.setCurrentRunningState(StateMovingUp)
			}
			b.blockchainClient = &recoveryCandidateChainRead{ClientI: b.blockchainClient, afterRead: func() { b.recoveryMiningBlocked.Store(true) }}
			candidate, trees, lease, err := b.GetMiningCandidate(t.Context())
			defer lease.Release()
			require.True(t, b.recoveryMiningBlocked.Load(), "fixture must cross recovery admission while constructing the candidate")
			require.Error(t, err)
			require.Nil(t, candidate)
			require.Nil(t, trees)
			require.Nil(t, lease)
		})
	}
}
