package pruner

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/bsv-blockchain/aerospike-client-go/v8"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/blob/memory"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/uaerospike"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// makeInputBytes builds a minimal Aerospike input-bin entry consisting of
// a 32-byte previous TXID plus a 4-byte little-endian previous output index.
// This matches the wire format consumed by extractInputReference.
func makeInputBytes(t *testing.T, parentTxID chainhash.Hash, prevIndex uint32) []byte {
	t.Helper()
	buf := make([]byte, 36)
	copy(buf[0:32], parentTxID[:])
	binary.LittleEndian.PutUint32(buf[32:36], prevIndex)
	return buf
}

// makeChildResult constructs an aerospike.Result that processRecordChunk will
// treat as a non-external, non-defensive, deletable record spending each parent
// at output index equal to its position in the slice. The child txid is
// synthesised from the index seed.
func makeChildResult(t *testing.T, s *Service, childSeed byte, parents []chainhash.Hash) *aerospike.Result {
	t.Helper()

	inputs := make([][]byte, 0, len(parents))
	for i, p := range parents {
		inputs = append(inputs, makeInputBytes(t, p, uint32(i)))
	}

	return makeChildResultWithInputs(t, s, childSeed, inputs)
}

// makeChildResultWithInputs is makeChildResult with explicit raw input bytes so
// a test can control the previous-output index (vout) of each spend.
func makeChildResultWithInputs(t *testing.T, s *Service, childSeed byte, rawInputs [][]byte) *aerospike.Result {
	t.Helper()

	var childTxID chainhash.Hash
	for i := range childTxID {
		childTxID[i] = childSeed
	}

	inputs := make([]interface{}, 0, len(rawInputs))
	for _, in := range rawInputs {
		inputs = append(inputs, in)
	}

	key, err := aerospike.NewKey(s.namespace, s.set, childTxID[:])
	require.NoError(t, err)

	bins := aerospike.BinMap{
		s.fieldTxID:     childTxID.CloneBytes(),
		s.fieldInputs:   inputs,
		s.fieldExternal: false,
	}

	return &aerospike.Result{
		Record: &aerospike.Record{
			Key:  key,
			Bins: bins,
		},
	}
}

// newTestServiceForSkip builds a Service configured for direct unit testing of
// processRecordChunk's parent-accumulation path. Defensive mode is off and
// SkipDeletions is on so the deletion path stays gated. flushCleanupBatchesFn
// defaults to the real method (a no-op for empty parentUpdates); tests that
// need to inspect the accumulated parent updates override it with a capturing
// closure so no real Aerospike call is ever attempted.
func newTestServiceForSkip(t *testing.T) *Service {
	t.Helper()
	ensurePrometheusMetrics()

	svc := &Service{
		logger: ulogger.NewVerboseTestLogger(t),
		settings: &settings.Settings{
			Pruner: settings.PrunerSettings{
				SkipDeletions: true,
			},
		},
		namespace:            "test",
		set:                  "test",
		utxoBatchSize:        128,
		defensiveEnabled:     false,
		fieldTxID:            fields.TxID.String(),
		fieldUtxos:           fields.Utxos.String(),
		fieldInputs:          fields.Inputs.String(),
		fieldDeletedChildren: fields.DeletedChildren.String(),
		fieldExternal:        fields.External.String(),
		fieldDeleteAtHeight:  fields.DeleteAtHeight.String(),
		fieldTotalExtraRecs:  fields.TotalExtraRecs.String(),
		fieldUnminedSince:    fields.UnminedSince.String(),
		fieldBlockHeights:    fields.BlockHeights.String(),
	}
	svc.flushCleanupBatchesFn = svc.flushCleanupBatches
	return svc
}

// captureFlush swaps in a flushCleanupBatchesFn that records the accumulated
// parent updates instead of hitting Aerospike, and returns a pointer the caller
// reads after processRecordChunk returns.
func captureFlush(svc *Service) *map[string]*parentUpdateInfo {
	captured := new(map[string]*parentUpdateInfo)
	svc.flushCleanupBatchesFn = func(_ context.Context, parentUpdates map[string]*parentUpdateInfo, _ []*aerospike.Key, _ []*externalFileInfo) error {
		*captured = parentUpdates
		return nil
	}
	return captured
}

// TestProcessRecordChunk_AccumulatesEveryParentMarker verifies the
// marker-reliability guarantee: processRecordChunk accumulates a deletedChildren
// marker for EVERY input's parent unconditionally, and never increments
// utxo_pruner_parents_skipped_pruned_total. The marker is a consensus
// discriminator for the counter-conflicting fail-closed guards, so it must never
// be suppressed by any pre-filter skip.
func TestProcessRecordChunk_AccumulatesEveryParentMarker(t *testing.T) {
	ctx := context.Background()
	svc := newTestServiceForSkip(t)
	captured := captureFlush(svc)

	// Two distinct parents; both must accumulate a marker.
	var parentA chainhash.Hash
	for i := range parentA {
		parentA[i] = 0xAA
	}
	var parentB chainhash.Hash
	for i := range parentB {
		parentB[i] = 0xBB
	}

	chunk := []*aerospike.Result{
		makeChildResult(t, svc, 0x11, []chainhash.Hash{parentA, parentB}),
	}

	before := testutil.ToFloat64(prometheusUtxoParentsSkippedPruned)

	processed, skipped, err := svc.processRecordChunk(ctx, 1000, chunk)
	require.NoError(t, err)
	require.Equal(t, 1, processed)
	require.Equal(t, 0, skipped, "no defensive skip when defensive mode is off")

	after := testutil.ToFloat64(prometheusUtxoParentsSkippedPruned)
	require.Equal(t, float64(0), after-before,
		"the skipped-pruned metric must never increment: the pre-filter skip was removed")

	require.Len(t, *captured, 2,
		"a marker must accumulate for every input's parent")
}

// TestProcessRecordChunk_MarkerPageKeyedOnly verifies the round-6 keying fix: the
// deletedChildren marker is written PAGE-keyed only — on the record that owns the
// spent output (CalculateKeySource(parent, vout)) — never dual-keyed onto the
// parent master record. Page-keying bounds the marker map per page so a
// high-fanout parent cannot concentrate one entry per distinct pruned child (across
// all vouts) onto a single record and wedge the pruner past the write-block-size.
// The spendMulti UDF reads its own page; the counter-conflicting walk reads a
// page-aggregating Get (get.go mergePageDeletedChildren), so a page-only marker is
// still visible to both readers.
func TestProcessRecordChunk_MarkerPageKeyedOnly(t *testing.T) {
	ctx := context.Background()
	svc := newTestServiceForSkip(t) // utxoBatchSize = 128
	captured := captureFlush(svc)

	var parent chainhash.Hash
	for i := range parent {
		parent[i] = 0xCC
	}

	// vout well beyond utxoBatchSize so CalculateKeySource(parent, 200) lands the
	// marker on pagination record num=1, distinct from the master record (vout 0).
	const vout = uint32(200)
	require.Greater(t, vout, uint32(svc.utxoBatchSize), "test premise: vout must exceed batch size")

	chunk := []*aerospike.Result{
		makeChildResultWithInputs(t, svc, 0x22, [][]byte{makeInputBytes(t, parent, vout)}),
	}

	processed, skipped, err := svc.processRecordChunk(ctx, 1000, chunk)
	require.NoError(t, err)
	require.Equal(t, 1, processed)
	require.Equal(t, 0, skipped)

	masterKeySrc := string(uaerospike.CalculateKeySource(&parent, 0, svc.utxoBatchSize))
	pageKeySrc := string(uaerospike.CalculateKeySource(&parent, vout, svc.utxoBatchSize))
	require.NotEqual(t, masterKeySrc, pageKeySrc,
		"test premise: vout must map to a distinct page record")

	require.Len(t, *captured, 1, "marker must accumulate on exactly one record (page-keyed only)")

	pageUpdate, okPage := (*captured)[pageKeySrc]
	require.True(t, okPage, "marker must be keyed on the PAGE record owning the spent vout")
	require.Len(t, pageUpdate.childHashes, 1, "page record carries the single child marker")

	_, okMaster := (*captured)[masterKeySrc]
	require.False(t, okMaster,
		"marker must NOT be dual-keyed onto the master record — that concentration is the prune-wedge this fix removes")
}

// TestProcessRecordChunk_MarkerOnMasterForLowVout verifies that for a spent output
// at vout < utxoBatchSize the page record IS the master record (page 0), so the
// single page-keyed marker lands on the master keySource. This is the common
// ≤128-output-parent case and is byte-identical to the pre-PR behaviour.
func TestProcessRecordChunk_MarkerOnMasterForLowVout(t *testing.T) {
	ctx := context.Background()
	svc := newTestServiceForSkip(t) // utxoBatchSize = 128
	captured := captureFlush(svc)

	var parent chainhash.Hash
	for i := range parent {
		parent[i] = 0xDD
	}

	const vout = uint32(5) // < utxoBatchSize: page 0 == master record
	require.Less(t, vout, uint32(svc.utxoBatchSize), "test premise: vout must be within the first page")

	chunk := []*aerospike.Result{
		makeChildResultWithInputs(t, svc, 0x44, [][]byte{makeInputBytes(t, parent, vout)}),
	}

	processed, skipped, err := svc.processRecordChunk(ctx, 1000, chunk)
	require.NoError(t, err)
	require.Equal(t, 1, processed)
	require.Equal(t, 0, skipped)

	masterKeySrc := string(uaerospike.CalculateKeySource(&parent, 0, svc.utxoBatchSize))
	pageKeySrc := string(uaerospike.CalculateKeySource(&parent, vout, svc.utxoBatchSize))
	require.Equal(t, masterKeySrc, pageKeySrc,
		"test premise: a vout within the first page must key on the master record")

	require.Len(t, *captured, 1, "single marker record for a low-vout spend")

	masterUpdate, okMaster := (*captured)[masterKeySrc]
	require.True(t, okMaster, "the page-keyed marker for a low vout lands on the master record (page 0)")
	require.Len(t, masterUpdate.childHashes, 1, "master record carries the single child marker")
}

// TestProcessRecordChunk_NoInputsIsNoOp verifies that a record with zero inputs
// (e.g. coinbase) accumulates no parent updates and causes no skipped-pruned
// increments. The flushCleanupBatches deletion path stays gated by SkipDeletions,
// so the default real flush is a no-op with no Aerospike call.
func TestProcessRecordChunk_NoInputsIsNoOp(t *testing.T) {
	ctx := context.Background()
	svc := newTestServiceForSkip(t)

	var childTxID chainhash.Hash
	for i := range childTxID {
		childTxID[i] = 0x77
	}

	key, keyErr := aerospike.NewKey(svc.namespace, svc.set, childTxID[:])
	require.NoError(t, keyErr)

	// Empty inputs (e.g. coinbase) — getTxInputsFromBins returns an empty
	// slice, so no parent loop runs.
	chunk := []*aerospike.Result{
		{
			Record: &aerospike.Record{
				Key: key,
				Bins: aerospike.BinMap{
					svc.fieldTxID:     childTxID.CloneBytes(),
					svc.fieldInputs:   []interface{}{},
					svc.fieldExternal: false,
				},
			},
		},
	}

	before := testutil.ToFloat64(prometheusUtxoParentsSkippedPruned)

	processed, skipped, err := svc.processRecordChunk(ctx, 1000, chunk)
	require.NoError(t, err)
	require.Equal(t, 1, processed)
	require.Equal(t, 0, skipped)

	after := testutil.ToFloat64(prometheusUtxoParentsSkippedPruned)
	require.Equal(t, float64(0), after-before)
}

// TestProcessRecordChunk_DefersExternalTxWithMissingBlob verifies the marker-invariant
// fix: when a non-coinbase external tx's blob is gone its inputs cannot be recovered, so
// the parent deletedChildren markers cannot be written. Below the finality horizon the
// record must therefore be DEFERRED (not deleted) — deleting it marker-less would make it
// an UNMARKED ghost that the counter-conflicting walk fails OPEN on. The record is counted
// as skipped and never appended to the deletion batch, and the missing blob must not abort
// the whole chunk. This record carries no delete_at_height bin, so the GC-horizon check
// cannot fire and the record stays deferred regardless of block height.
func TestProcessRecordChunk_DefersExternalTxWithMissingBlob(t *testing.T) {
	ctx := context.Background()
	svc := newTestServiceForSkip(t)
	svc.external = memory.New() // empty store: the tx's blob is absent

	// Capture both parent updates and deletions from the flush seam.
	var capturedUpdates map[string]*parentUpdateInfo
	var capturedDeletions []*aerospike.Key
	svc.flushCleanupBatchesFn = func(_ context.Context, parentUpdates map[string]*parentUpdateInfo, deletions []*aerospike.Key, _ []*externalFileInfo) error {
		capturedUpdates = parentUpdates
		capturedDeletions = deletions
		return nil
	}

	var txID chainhash.Hash
	for i := range txID {
		txID[i] = 0x33
	}

	key, keyErr := aerospike.NewKey(svc.namespace, svc.set, txID[:])
	require.NoError(t, keyErr)

	chunk := []*aerospike.Result{
		{
			Record: &aerospike.Record{
				Key: key,
				Bins: aerospike.BinMap{
					svc.fieldTxID:     txID.CloneBytes(),
					svc.fieldExternal: true, // external: inputs live in the (missing) blob
				},
			},
		},
	}

	processed, skipped, err := svc.processRecordChunk(ctx, 1000, chunk)
	require.NoError(t, err, "a missing external blob must not abort the chunk")
	require.Equal(t, 0, processed, "the record with an unrecoverable blob must not be processed for deletion")
	require.Equal(t, 1, skipped, "the record must be counted as skipped/deferred")

	require.Empty(t, capturedDeletions, "a marker-less external record must NOT be appended to the deletion batch")
	require.Empty(t, capturedUpdates, "no parent markers can be written without the tx's inputs")
}

// makeMissingBlobExternalResult builds an aerospike.Result for a non-coinbase external tx
// whose blob is absent, tagged with delete_at_height = dah. processRecordChunk will hit
// errExternalInputsUnrecoverable for it and then consult the DAH bin for the GC-horizon
// decision.
func makeMissingBlobExternalResult(t *testing.T, svc *Service, seed byte, dah int) *aerospike.Result {
	t.Helper()

	var txID chainhash.Hash
	for i := range txID {
		txID[i] = seed
	}

	key, keyErr := aerospike.NewKey(svc.namespace, svc.set, txID[:])
	require.NoError(t, keyErr)

	return &aerospike.Result{
		Record: &aerospike.Record{
			Key: key,
			Bins: aerospike.BinMap{
				svc.fieldTxID:           txID.CloneBytes(),
				svc.fieldExternal:       true, // inputs live in the (missing) blob
				svc.fieldDeleteAtHeight: dah,
			},
		},
	}
}

// TestProcessRecordChunk_DefersBlobMissingExternalTxBelowHorizon verifies that a
// blob-missing external tx whose delete_at_height is still within the finality horizon
// (blockHeight <= DAH + retention) stays DEFERRED: it must not be deleted, no marker is
// written, and it counts as skipped. The record is inside the counter-conflicting walk
// window so a marker-less deletion would still be a fail-OPEN ghost.
func TestProcessRecordChunk_DefersBlobMissingExternalTxBelowHorizon(t *testing.T) {
	ctx := context.Background()
	svc := newTestServiceForSkip(t)
	svc.external = memory.New()
	svc.blockHeightRetention = 288

	var capturedUpdates map[string]*parentUpdateInfo
	var capturedDeletions []*aerospike.Key
	svc.flushCleanupBatchesFn = func(_ context.Context, parentUpdates map[string]*parentUpdateInfo, deletions []*aerospike.Key, _ []*externalFileInfo) error {
		capturedUpdates = parentUpdates
		capturedDeletions = deletions
		return nil
	}

	// DAH = 100, retention = 288 → horizon = 388. blockHeight 388 is NOT strictly
	// greater than the horizon, so the record must stay deferred.
	const dah = 100
	chunk := []*aerospike.Result{makeMissingBlobExternalResult(t, svc, 0x55, dah)}

	beforeGC := testutil.ToFloat64(prometheusUtxoRecordsDeferredGCed)

	processed, skipped, err := svc.processRecordChunk(ctx, uint32(dah)+svc.blockHeightRetention, chunk)
	require.NoError(t, err)
	require.Equal(t, 0, processed, "below the horizon the record must not be deleted")
	require.Equal(t, 1, skipped, "below the horizon the record stays deferred/skipped")

	require.Empty(t, capturedDeletions, "below-horizon deferral must not append to the deletion batch")
	require.Empty(t, capturedUpdates, "no markers without recoverable inputs")
	require.Equal(t, beforeGC, testutil.ToFloat64(prometheusUtxoRecordsDeferredGCed),
		"the GC counter must not increment below the horizon")
}

// TestProcessRecordChunk_GCsBlobMissingExternalTxPastHorizon verifies the GC-on-horizon
// path: once a blob-missing external tx is past the finality horizon
// (blockHeight > DAH + retention, i.e. mined + 2x retention), it is beyond the
// counter-conflicting walk window and is deleted marker-less. The master record key is
// appended to the deletion batch, no parent marker is written, processedCount is bumped,
// and the GC counter increments.
func TestProcessRecordChunk_GCsBlobMissingExternalTxPastHorizon(t *testing.T) {
	ctx := context.Background()
	svc := newTestServiceForSkip(t)
	svc.external = memory.New()
	svc.blockHeightRetention = 288

	var capturedUpdates map[string]*parentUpdateInfo
	var capturedDeletions []*aerospike.Key
	svc.flushCleanupBatchesFn = func(_ context.Context, parentUpdates map[string]*parentUpdateInfo, deletions []*aerospike.Key, _ []*externalFileInfo) error {
		capturedUpdates = parentUpdates
		capturedDeletions = deletions
		return nil
	}

	// DAH = 100, retention = 288 → horizon = 388. blockHeight 389 is strictly past it.
	const dah = 100
	result := makeMissingBlobExternalResult(t, svc, 0x66, dah)
	chunk := []*aerospike.Result{result}

	beforeGC := testutil.ToFloat64(prometheusUtxoRecordsDeferredGCed)
	beforeNoMarker := testutil.ToFloat64(prometheusUtxoRecordsDeferredNoMarker)

	processed, skipped, err := svc.processRecordChunk(ctx, uint32(dah)+svc.blockHeightRetention+1, chunk)
	require.NoError(t, err)
	require.Equal(t, 1, processed, "past the horizon the record is reaped and counts as processed")
	require.Equal(t, 0, skipped, "past the horizon the record is not deferred/skipped")

	require.Len(t, capturedDeletions, 1, "the master record key must be appended to the deletion batch")
	require.Equal(t, result.Record.Key, capturedDeletions[0], "the deletion must target the record's own key")
	require.Empty(t, capturedUpdates, "GC delete-through is marker-less: no parent markers are written")

	require.Equal(t, beforeGC+1, testutil.ToFloat64(prometheusUtxoRecordsDeferredGCed),
		"the GC counter must increment past the horizon")
	require.Equal(t, beforeNoMarker, testutil.ToFloat64(prometheusUtxoRecordsDeferredNoMarker),
		"the deferral counter must not increment when the record is GC'd")
}
