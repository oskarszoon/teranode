package blockassembly

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/services/blockassembly/subtreeprocessor"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/jellydator/ttlcache/v3"
	"github.com/stretchr/testify/require"
)

func TestMiningJobExplicitMutationAndRetainReleaseOwnership(t *testing.T) {
	b, _ := newUnminedRecoveryTestAssembler(t, blockchain.FSMStateRUNNING, func(s *settings.Settings) {
		s.BlockAssembly.InitialMerkleItemsPerSubtree = 2
		s.BlockAssembly.StoreTxInpointsForSubtreeMeta = false
		s.BlockAssembly.SubtreeMmapDir = t.TempDir()
	})
	hashes := storeRecoverySelectionChain(t, b, 1)
	require.True(t, b.AddTxBatchIfRoom([]subtree.Node{{Hash: hashes[0], Fee: 1, SizeInBytes: 100}}, []*subtree.TxInpoints{{}}))
	var snapshot *subtreeprocessor.PrecomputedMiningData
	require.Eventually(t, func() bool {
		snapshot = b.subtreeProcessor.GetPrecomputedMiningData()
		if snapshot != nil && snapshot.Lease != nil {
			return true
		}
		if snapshot != nil {
			snapshot.Lease.Release()
		}
		return false
	}, time.Second, time.Millisecond)
	defer snapshot.Lease.Release()

	server := &BlockAssembly{jobStore: ttlcache.New[chainhash.Hash, *subtreeprocessor.Job]()}
	server.jobStore.OnEviction(func(_ context.Context, _ ttlcache.EvictionReason, item *ttlcache.Item[chainhash.Hash, *subtreeprocessor.Job]) {
		item.Value().Lease.Release()
	})
	id := chainhash.HashH([]byte("concurrent-job"))
	const replacements = 64
	leases := make([]*subtreeprocessor.MiningSnapshotLease, 0, replacements)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < replacements; i++ {
			lease, ok := snapshot.Lease.Retain()
			if !ok {
				return
			}
			leases = append(leases, lease)
			server.replaceMiningJob(id, &subtreeprocessor.Job{ID: &id, Lease: lease})
			if i%3 == 0 {
				server.deleteMiningJob(id)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < replacements*3; i++ {
			_, lease, err := server.retainMiningJobForSubmission(&id, id.String())
			if err == nil {
				lease.Release()
			}
		}
	}()
	wg.Wait()
	server.deleteAllMiningJobs()
	require.Len(t, leases, replacements)
	for _, lease := range leases {
		retained, ok := lease.Retain()
		retained.Release()
		require.False(t, ok, "deleted/replaced cache ownership must be released")
	}
	retained, ok := snapshot.Lease.Retain()
	require.True(t, ok, "independent snapshot owner must survive cache retirement")
	retained.Release()
	snapshot.Lease.Release()
	retained, ok = snapshot.Lease.Retain()
	retained.Release()
	require.False(t, ok, "last owner release must retire the lease")
}
