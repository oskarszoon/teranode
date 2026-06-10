package netsync

import (
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	txmap "github.com/bsv-blockchain/go-tx-map"
	"github.com/stretchr/testify/require"
)

// TestCollectInBlockParentHashes covers the per-subtree in-block parent
// collection that feeds CheckSubtreeFromBlock's in_block_parent_hashes hint:
// only parents that are referenced by this subtree's transactions AND present
// in the block's txMap are returned, deduplicated.
func TestCollectInBlockParentHashes(t *testing.T) {
	newParent := func(seed string) *bt.Tx {
		tx := bt.NewTx()
		tx.Version = 1
		tx.LockTime = uint32(len(seed)) // differentiate txids
		require.NoError(t, tx.PayToAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", 1000))
		return tx
	}

	newChild := func(parent *bt.Tx, vout uint32) *bt.Tx {
		tx := bt.NewTx()
		tx.Version = 1
		require.NoError(t, tx.From(parent.TxID(), vout, parent.Outputs[vout].LockingScript.String(), parent.Outputs[vout].Satoshis))
		require.NoError(t, tx.PayToAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", 500))
		return tx
	}

	parentTx := newParent("parent-a")
	parentHash := *parentTx.TxIDChainHash()

	childTx := newChild(parentTx, 0)
	childHash := *childTx.TxIDChainHash()

	// externalSpender's parent is NOT in the txMap (prior-block parent)
	externalParent := newParent("external-parent-not-in-block")
	externalSpender := newChild(externalParent, 0)
	externalSpenderHash := *externalSpender.TxIDChainHash()

	buildTxMap := func(txs ...*bt.Tx) *txmap.SyncedMap[chainhash.Hash, *TxMapWrapper] {
		txMap := txmap.NewSyncedMap[chainhash.Hash, *TxMapWrapper](len(txs))
		for _, tx := range txs {
			txMap.Set(*tx.TxIDChainHash(), &TxMapWrapper{Tx: tx})
		}
		return txMap
	}

	buildSubtree := func(hashes ...chainhash.Hash) *subtreepkg.Subtree {
		st, err := subtreepkg.NewIncompleteTreeByLeafCount(len(hashes))
		require.NoError(t, err)
		for _, h := range hashes {
			require.NoError(t, st.AddNode(h, 1, 1))
		}
		return st
	}

	t.Run("in-block parent of subtree tx is collected", func(t *testing.T) {
		// parent lives in the block (txMap) but NOT in this subtree —
		// the cross-subtree case the hint exists for
		txMap := buildTxMap(parentTx, childTx, externalSpender)
		st := buildSubtree(childHash, externalSpenderHash)

		parents := collectInBlockParentHashes(st, txMap)
		require.Equal(t, []chainhash.Hash{parentHash}, parents)
	})

	t.Run("external parents are not collected", func(t *testing.T) {
		txMap := buildTxMap(externalSpender)
		st := buildSubtree(externalSpenderHash)

		parents := collectInBlockParentHashes(st, txMap)
		require.Nil(t, parents)
	})

	t.Run("duplicate parent references collapse to one entry", func(t *testing.T) {
		secondChild := newChild(parentTx, 0)
		secondChild.LockTime = 99 // differentiate from childTx

		txMap := buildTxMap(parentTx, childTx, secondChild)
		st := buildSubtree(childHash, *secondChild.TxIDChainHash())

		parents := collectInBlockParentHashes(st, txMap)
		require.Equal(t, []chainhash.Hash{parentHash}, parents)
	})

	t.Run("subtree nodes missing from txMap are skipped", func(t *testing.T) {
		// coinbase placeholder (and any tx not in the map) must not panic
		txMap := buildTxMap(childTx, parentTx)

		st, err := subtreepkg.NewIncompleteTreeByLeafCount(2)
		require.NoError(t, err)
		require.NoError(t, st.AddCoinbaseNode())
		require.NoError(t, st.AddNode(childHash, 1, 1))

		parents := collectInBlockParentHashes(st, txMap)
		require.Equal(t, []chainhash.Hash{parentHash}, parents)
	})

	t.Run("nil inputs return nil", func(t *testing.T) {
		require.Nil(t, collectInBlockParentHashes(nil, buildTxMap(childTx)))
		require.Nil(t, collectInBlockParentHashes(buildSubtree(childHash), nil))
	})
}
