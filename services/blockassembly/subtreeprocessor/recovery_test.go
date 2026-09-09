package subtreeprocessor

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtree "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/stretchr/testify/require"
)

func TestRecoverySnapshotIncludesRemainder(t *testing.T) {
	stp := &SubtreeProcessor{queue: NewLockFreeQueueWithLimit(0)}
	first, err := subtree.NewTreeByLeafCount(4)
	require.NoError(t, err)
	require.NoError(t, first.AddCoinbaseNode())
	for i := byte(1); i < 4; i++ {
		require.NoError(t, first.AddNode(chainhash.Hash{i}, 0, 1))
	}
	current, err := subtree.NewTreeByLeafCount(4)
	require.NoError(t, err)
	require.NoError(t, current.AddNode(chainhash.Hash{4}, 0, 1))
	stp.chainedSubtrees = []*subtree.Subtree{first}
	stp.currentSubtree.Store(current)
	visited := 0
	err = stp.recoverySnapshot(t.Context(), func(_ *model.BlockHeader, trees []*subtree.Subtree) error {
		require.Len(t, trees, 2)
		for _, tree := range trees {
			visited += len(tree.Nodes)
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 5, visited)
}

func TestRecoverySnapshotCancellation(t *testing.T) {
	stp := &SubtreeProcessor{queue: NewLockFreeQueueWithLimit(0)}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, stp.RecoverySnapshot(ctx, func(*model.BlockHeader, []*subtree.Subtree) error { t.Fatal("cancelled snapshot executed"); return nil }), context.Canceled)
}

func TestRecoverySnapshotRejectsPendingIngress(t *testing.T) {
	stp := &SubtreeProcessor{queue: NewLockFreeQueueWithLimit(0)}
	stp.queue.queueLength.Store(1)
	called := false
	err := stp.recoverySnapshot(t.Context(), func(*model.BlockHeader, []*subtree.Subtree) error { called = true; return nil })
	require.ErrorContains(t, err, "empty ingest queue")
	require.False(t, called)
	stp.queue.queueLength.Store(0)
	err = stp.recoverySnapshot(t.Context(), func(*model.BlockHeader, []*subtree.Subtree) error { stp.queue.queueLength.Store(1); return nil })
	require.ErrorContains(t, err, "queue changed")
}

func TestRecoverySnapshotCancellationReleasesOwner(t *testing.T) {
	stp := setupTestSubtreeProcessor(t)
	ctx, cancel := context.WithCancel(t.Context())
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- stp.RecoverySnapshot(ctx, func(*model.BlockHeader, []*subtree.Subtree) error { close(entered); <-ctx.Done(); return ctx.Err() })
	}()
	<-entered
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.NoError(t, stp.RecoverySnapshot(t.Context(), func(*model.BlockHeader, []*subtree.Subtree) error { return nil }))
}
