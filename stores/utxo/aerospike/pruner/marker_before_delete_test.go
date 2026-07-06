package pruner

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/aerospike-client-go/v8"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/stretchr/testify/require"
)

// TestFlushCleanupBatches_MarkerErrorAbortsBeforeDelete fault-injects a parent-marker
// write failure via the executeBatchParentUpdatesFn seam and verifies the core ordering
// invariant of flushCleanupBatches: a failed marker write must abort BEFORE the child
// delete phase runs. This is the negative-path counterpart to the (already covered)
// happy path — without it, a regression that reordered or parallelized the two phases
// would slip through untested. A child deleted without its parent's deletedChildren
// marker is a marker-less ghost that the counter-conflicting walk fails OPEN on (see the
// ordering rationale on flushCleanupBatches).
func TestFlushCleanupBatches_MarkerErrorAbortsBeforeDelete(t *testing.T) {
	ctx := context.Background()
	svc := newTestServiceForSkip(t)
	svc.settings.Pruner.SkipDeletions = false // deletions must be reachable so the abort is meaningful

	markerErr := errors.NewProcessingError("simulated marker write failure")
	svc.executeBatchParentUpdatesFn = func(_ context.Context, _ map[string]*parentUpdateInfo) error {
		return markerErr
	}

	deletionsCalled := false
	svc.executeBatchDeletionsFn = func(_ context.Context, _ []*aerospike.Key) error {
		deletionsCalled = true
		return nil
	}

	var parent chainhash.Hash
	for i := range parent {
		parent[i] = 0xEE
	}
	parentKey, err := aerospike.NewKey(svc.namespace, svc.set, parent[:])
	require.NoError(t, err)

	var child chainhash.Hash
	for i := range child {
		child[i] = 0xFF
	}
	childKey, err := aerospike.NewKey(svc.namespace, svc.set, child[:])
	require.NoError(t, err)

	parentUpdates := map[string]*parentUpdateInfo{
		string(parentKey.Digest()): {key: parentKey, childHashes: []*chainhash.Hash{&child}},
	}
	deletions := []*aerospike.Key{childKey}

	flushErr := svc.flushCleanupBatches(ctx, parentUpdates, deletions, nil)

	require.Error(t, flushErr, "flushCleanupBatches must surface the marker-write error")
	require.Equal(t, markerErr, flushErr, "flushCleanupBatches must return the exact marker error, not swallow or wrap it away")
	require.False(t, deletionsCalled, "executeBatchDeletionsFn must NOT be invoked when the parent marker write fails — a child must never be deleted marker-less")
}

// TestFlushCleanupBatches_MarkerSuccessReachesDelete is the happy-path control for the
// fault-injection test above: when the marker write succeeds, flushCleanupBatches must
// proceed to the delete phase through the same executeBatchDeletionsFn seam. Without this,
// a broken seam wiring (e.g. one that never calls executeBatchDeletionsFn at all) could
// make the negative test above pass for the wrong reason.
func TestFlushCleanupBatches_MarkerSuccessReachesDelete(t *testing.T) {
	ctx := context.Background()
	svc := newTestServiceForSkip(t)
	svc.settings.Pruner.SkipDeletions = false

	markerCalled := false
	svc.executeBatchParentUpdatesFn = func(_ context.Context, _ map[string]*parentUpdateInfo) error {
		markerCalled = true
		return nil
	}

	deletionsCalled := false
	svc.executeBatchDeletionsFn = func(_ context.Context, _ []*aerospike.Key) error {
		deletionsCalled = true
		return nil
	}

	var parent chainhash.Hash
	for i := range parent {
		parent[i] = 0xEE
	}
	parentKey, err := aerospike.NewKey(svc.namespace, svc.set, parent[:])
	require.NoError(t, err)

	var child chainhash.Hash
	for i := range child {
		child[i] = 0xFF
	}
	childKey, err := aerospike.NewKey(svc.namespace, svc.set, child[:])
	require.NoError(t, err)

	parentUpdates := map[string]*parentUpdateInfo{
		string(parentKey.Digest()): {key: parentKey, childHashes: []*chainhash.Hash{&child}},
	}
	deletions := []*aerospike.Key{childKey}

	flushErr := svc.flushCleanupBatches(ctx, parentUpdates, deletions, nil)
	require.NoError(t, flushErr)
	require.True(t, markerCalled, "test premise: the marker seam must be invoked")
	require.True(t, deletionsCalled, "the delete phase must be reached once the marker write succeeds")
}
