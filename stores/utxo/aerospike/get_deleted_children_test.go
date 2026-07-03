package aerospike

// Unit coverage for processDeletedChildren — pure bins-map parsing, no Aerospike required.
// The pruner's addDeletedChildren UDF stores deletedChildren as a map keyed by the child tx
// hash in display hex (chainhash.String()) with a bool value; this discriminator tells the
// counter-conflicting walk a now-missing spender was reaped deliberately. Parsing is
// best-effort: an unparseable or wrong-typed key is skipped, never fatal.

import (
	"testing"

	aerospike "github.com/bsv-blockchain/aerospike-client-go/v8"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/stretchr/testify/require"
)

func deletedChildrenBin(entries map[interface{}]interface{}) aerospike.BinMap {
	return aerospike.BinMap{fields.DeletedChildren.String(): entries}
}

func TestProcessDeletedChildren_ValidHexKeys(t *testing.T) {
	h1 := chainhash.HashH([]byte("deleted-child-1"))
	h2 := chainhash.HashH([]byte("deleted-child-2"))

	res := processDeletedChildren(deletedChildrenBin(map[interface{}]interface{}{
		h1.String(): true,
		h2.String(): true,
	}))

	require.Len(t, res, 2)
	require.Contains(t, res, h1)
	require.Contains(t, res, h2)
}

func TestProcessDeletedChildren_InvalidHexKeySkipped(t *testing.T) {
	valid := chainhash.HashH([]byte("valid-child"))

	res := processDeletedChildren(deletedChildrenBin(map[interface{}]interface{}{
		valid.String():     true,
		"not-a-valid-hash": true, // unparseable hex → skipped, not fatal
	}))

	require.Len(t, res, 1)
	require.Contains(t, res, valid)
}

func TestProcessDeletedChildren_NonStringKeySkipped(t *testing.T) {
	valid := chainhash.HashH([]byte("valid-child-2"))

	res := processDeletedChildren(deletedChildrenBin(map[interface{}]interface{}{
		valid.String(): true,
		42:             true, // non-string key → skipped
	}))

	require.Len(t, res, 1)
	require.Contains(t, res, valid)
}

func TestProcessDeletedChildren_WrongBinTypeReturnsNil(t *testing.T) {
	// The bin exists but is not a map — the type assertion fails and the parse yields nil.
	res := processDeletedChildren(aerospike.BinMap{fields.DeletedChildren.String(): "not-a-map"})
	require.Nil(t, res)
}

func TestProcessDeletedChildren_AbsentBinReturnsNil(t *testing.T) {
	res := processDeletedChildren(aerospike.BinMap{"someOtherBin": 1})
	require.Nil(t, res)
}

func TestProcessDeletedChildren_EmptyMapReturnsNil(t *testing.T) {
	res := processDeletedChildren(deletedChildrenBin(map[interface{}]interface{}{}))
	require.Nil(t, res)
}

func TestProcessDeletedChildren_AllUnparseableReturnsNil(t *testing.T) {
	// A map with only unusable entries yields no hashes → nil (never an empty non-nil map).
	res := processDeletedChildren(deletedChildrenBin(map[interface{}]interface{}{
		"bad-key": true,
	}))
	require.Nil(t, res)
}

// The round-6 read makes fields.DeletedChildren page-aware: the pruner keeps the
// marker page-keyed (bounded per page record), and the counter-conflicting walk's
// Get unions the master record's map with every page record's map via
// unionDeletedChildren. mergePageDeletedChildren does the per-page Gets (needs a
// live client); unionDeletedChildren is the pure aggregation seam, exercised here
// on synthetic bins so the union logic is covered without Aerospike.

// TestUnionDeletedChildren_AggregatesMasterAndPages verifies the walk sees the full
// union across the master record and every page record, and that the master
// record's OWN map is not what grows — page-vout children live on the page bins.
func TestUnionDeletedChildren_AggregatesMasterAndPages(t *testing.T) {
	master1 := chainhash.HashH([]byte("master-child-1")) // spent at a low vout (master page)
	master2 := chainhash.HashH([]byte("master-child-2"))
	page1 := chainhash.HashH([]byte("page1-child")) // spent at vout >= utxoBatchSize
	page2 := chainhash.HashH([]byte("page2-child"))

	masterBins := deletedChildrenBin(map[interface{}]interface{}{
		master1.String(): true,
		master2.String(): true,
	})
	page1Bins := deletedChildrenBin(map[interface{}]interface{}{page1.String(): true})
	page2Bins := deletedChildrenBin(map[interface{}]interface{}{page2.String(): true})

	// The master record's own parsed set must NOT contain the page-vout children —
	// that no-concentration property is the whole point of page-keyed writes.
	masterOnly := processDeletedChildren(masterBins)
	require.Len(t, masterOnly, 2)
	require.NotContains(t, masterOnly, page1)
	require.NotContains(t, masterOnly, page2)

	// The walk's page-aggregating read folds every page into the master-derived set.
	union := processDeletedChildren(masterBins)
	union = unionDeletedChildren(union, processDeletedChildren(page1Bins))
	union = unionDeletedChildren(union, processDeletedChildren(page2Bins))

	require.Len(t, union, 4)
	require.Contains(t, union, master1)
	require.Contains(t, union, master2)
	require.Contains(t, union, page1)
	require.Contains(t, union, page2)
}

// TestUnionDeletedChildren_NilMasterAllocatesFromPages covers the empty-master
// parent: processDeletedChildren returns nil for the master record, but page
// records still carry markers, so the union must allocate and return them.
func TestUnionDeletedChildren_NilMasterAllocatesFromPages(t *testing.T) {
	page1 := chainhash.HashH([]byte("only-page-child"))

	union := unionDeletedChildren(nil, processDeletedChildren(deletedChildrenBin(map[interface{}]interface{}{
		page1.String(): true,
	})))

	require.Len(t, union, 1)
	require.Contains(t, union, page1)
}

// TestUnionDeletedChildren_DedupAcrossRecords verifies that a child hash appearing
// on more than one record collapses to a single entry — markers are keyed by the
// globally-unique child tx hash, so unioning is idempotent.
func TestUnionDeletedChildren_DedupAcrossRecords(t *testing.T) {
	shared := chainhash.HashH([]byte("shared-child"))

	union := unionDeletedChildren(
		processDeletedChildren(deletedChildrenBin(map[interface{}]interface{}{shared.String(): true})),
		processDeletedChildren(deletedChildrenBin(map[interface{}]interface{}{shared.String(): true})),
	)

	require.Len(t, union, 1)
	require.Contains(t, union, shared)
}

// TestUnionDeletedChildren_EmptyPageIsNoOp verifies an empty/absent page bin
// contributes nothing and does not allocate — a nil master stays nil (best-effort
// read: missing page markers never fabricate an empty non-nil set).
func TestUnionDeletedChildren_EmptyPageIsNoOp(t *testing.T) {
	require.Nil(t, unionDeletedChildren(nil, nil))

	master := chainhash.HashH([]byte("master-child"))
	into := processDeletedChildren(deletedChildrenBin(map[interface{}]interface{}{master.String(): true}))

	// Absent page bin → processDeletedChildren yields nil → union is a no-op.
	into = unionDeletedChildren(into, processDeletedChildren(aerospike.BinMap{"someOtherBin": 1}))
	require.Len(t, into, 1)
	require.Contains(t, into, master)
}
