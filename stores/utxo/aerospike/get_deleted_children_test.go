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
