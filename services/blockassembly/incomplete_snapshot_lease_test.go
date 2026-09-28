package blockassembly

import (
	"context"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/services/blockassembly/subtreeprocessor"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/stretchr/testify/require"
)

type incompleteLeaseSource struct {
	subtreeprocessor.Interface
	data *subtreeprocessor.PrecomputedMiningData
}

func (*incompleteLeaseSource) GetPrecomputedMiningData() *subtreeprocessor.PrecomputedMiningData {
	return nil
}
func (s *incompleteLeaseSource) GetIncompleteSubtreeMiningData(context.Context) *subtreeprocessor.PrecomputedMiningData {
	return s.data
}

func TestMiningCandidateOwnsOptionalIncompleteSnapshotLease(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "rejected stale"}[stale], func(t *testing.T) {
			b, _ := newUnminedRecoveryTestAssembler(t, blockchain.FSMStateRUNNING, func(s *settings.Settings) {
				s.BlockAssembly.InitialMerkleItemsPerSubtree = 2
				s.BlockAssembly.SubtreeMmapDir = t.TempDir()
			})
			hashes := storeRecoverySelectionChain(t, b, 1)
			realProcessor := b.subtreeProcessor
			require.True(t, b.AddTxBatchIfRoom([]subtree.Node{{Hash: hashes[0], Fee: 1, SizeInBytes: 100}}, []*subtree.TxInpoints{{}}))
			var data *subtreeprocessor.PrecomputedMiningData
			require.Eventually(t, func() bool {
				data = realProcessor.GetPrecomputedMiningData()
				if data != nil && len(data.Subtrees) == 1 && data.Lease != nil {
					return true
				}
				if data != nil {
					data.Lease.Release()
				}
				return false
			}, time.Second, time.Millisecond)
			if stale {
				data.PreviousHeader = blockHeader1
			}
			b.subtreeProcessor = &incompleteLeaseSource{Interface: realProcessor, data: data}
			_, _, lease, err := b.GetMiningCandidate(t.Context())
			require.NoError(t, err)
			if stale {
				require.Nil(t, lease)
			} else {
				require.Same(t, data.Lease, lease)
			}
			lease.Release()
			retained, ok := data.Lease.Retain()
			retained.Release()
			require.False(t, ok, "caller or rejected-path must release the snapshot lease")
		})
	}
}
