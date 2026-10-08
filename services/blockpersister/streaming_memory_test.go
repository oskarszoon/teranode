package blockpersister

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/url"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/services/utxopersister"
	"github.com/bsv-blockchain/teranode/stores/blob/file"
	"github.com/bsv-blockchain/teranode/stores/blob/options"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// Large-subtree fixture: every tx carries one big previous locking script (extended
// input) and one big output, the shape that made testnet block 1681787's single
// subtree 1.5x the size of the block.
const (
	largeSubtreeTxCount    = 32
	largeSubtreeScriptSize = 1 << 20 // 1 MiB per script, 2 MiB of scripts per tx
)

// largeExtendedTx returns a 1-in-1-out transaction whose input carries a previous
// locking script of scriptSize bytes and whose output has a locking script of
// scriptSize bytes. seed makes every transaction distinct.
func largeExtendedTx(t *testing.T, seed uint32, scriptSize int) *bt.Tx {
	t.Helper()

	prevScript := make([]byte, scriptSize)
	lockScript := make([]byte, scriptSize)

	for i := range prevScript {
		prevScript[i] = byte(i) ^ byte(seed)
		lockScript[i] = byte(i>>3) ^ byte(seed>>1)
	}

	// OP_1 first, so the output is never mistaken for an OP_FALSE OP_RETURN data output.
	lockScript[0] = bscript.Op1
	binary.LittleEndian.PutUint32(lockScript[1:5], seed)

	var prevHashBytes [32]byte
	binary.LittleEndian.PutUint32(prevHashBytes[:4], seed+1)
	prevHash := chainhash.Hash(prevHashBytes)

	input := &bt.Input{
		PreviousTxOutIndex: 0,
		PreviousTxSatoshis: 2000,
		PreviousTxScript:   bscript.NewFromBytes(prevScript),
		UnlockingScript:    bscript.NewFromBytes([]byte{bscript.Op1}),
		SequenceNumber:     0xffffffff,
	}
	require.NoError(t, input.PreviousTxIDAdd(&prevHash))

	tx := bt.NewTx()
	tx.Inputs = append(tx.Inputs, input)
	tx.AddOutput(&bt.Output{Satoshis: 1000, LockingScript: bscript.NewFromBytes(lockScript)})

	return tx
}

// storeLargeSubtree writes a subtree of txCount large extended transactions, and its
// subtreeData file, to a file-backed blob store. A file store keeps the data off the
// heap, so a heap measurement taken while the persister reads it shows only what the
// persister itself holds.
func storeLargeSubtree(t *testing.T, txCount, scriptSize int) (*file.File, *subtreepkg.Subtree, int) {
	t.Helper()

	storeURL, err := url.Parse("file://" + t.TempDir())
	require.NoError(t, err)

	subtreeStore, err := file.New(ulogger.TestLogger{}, storeURL, options.WithBlobDeletionScheduler(&mockBlobDeletionScheduler{}))
	require.NoError(t, err)

	subtree, err := subtreepkg.NewTreeByLeafCount(txCount)
	require.NoError(t, err)

	data := make([]byte, 0, txCount*(2*scriptSize+256))

	for i := 0; i < txCount; i++ {
		tx := largeExtendedTx(t, uint32(i), scriptSize)
		require.NoError(t, subtree.AddNode(*tx.TxIDChainHash(), 1, uint64(tx.Size())))
		data = append(data, tx.ExtendedBytes()...)
	}

	subtreeBytes, err := subtree.Serialize()
	require.NoError(t, err)
	require.NoError(t, subtreeStore.Set(t.Context(), subtree.RootHash()[:], fileformat.FileTypeSubtree, subtreeBytes))
	require.NoError(t, subtreeStore.Set(t.Context(), subtree.RootHash()[:], fileformat.FileTypeSubtreeData, data))

	return subtreeStore, subtree, len(data)
}

// peakHeapGrowth runs fn and returns the highest HeapAlloc seen above the
// pre-call baseline. GOGC is lowered for the duration so uncollected garbage
// stays small and the figure tracks what fn keeps live.
func peakHeapGrowth(t *testing.T, fn func()) uint64 {
	t.Helper()

	defer debug.SetGCPercent(debug.SetGCPercent(10))

	runtime.GC()

	var ms runtime.MemStats

	runtime.ReadMemStats(&ms)
	baseline := ms.HeapAlloc

	var peak atomic.Uint64

	sample := func() {
		var m runtime.MemStats

		runtime.ReadMemStats(&m)

		if m.HeapAlloc > peak.Load() {
			peak.Store(m.HeapAlloc)
		}
	}

	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)

		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()

		for {
			sample()

			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()

	fn()
	sample()

	close(stop)
	<-done

	if peak.Load() <= baseline {
		return 0
	}

	return peak.Load() - baseline
}

// TestProcessSubtreeUTXOStreaming_MemoryDoesNotScaleWithSubtree reproduces cause 1 of
// issue 1914: the decode arena is never reset inside the per-tx loop, so every script
// of every tx in the subtree accumulates in one doubling slab.
func TestProcessSubtreeUTXOStreaming_MemoryDoesNotScaleWithSubtree(t *testing.T) {
	ctx := context.Background()
	logger := ulogger.TestLogger{}
	tSettings := test.CreateBaseTestSettings(t)

	subtreeStore, subtree, dataSize := storeLargeSubtree(t, largeSubtreeTxCount, largeSubtreeScriptSize)

	blockStoreURL, err := url.Parse("file://" + t.TempDir())
	require.NoError(t, err)

	blockStore, err := file.New(logger, blockStoreURL)
	require.NoError(t, err)

	persister := New(ctx, logger, tSettings, blockStore, subtreeStore, nil, nil)

	blockHash := chainhash.DoubleHashH([]byte("issue-1914-phase-2"))
	utxoDiff, err := utxopersister.NewUTXOSet(ctx, logger, tSettings, blockStore, &blockHash, 2000)
	require.NoError(t, err)

	defer func() { _ = utxoDiff.Close() }()

	growth := peakHeapGrowth(t, func() {
		err = persister.ProcessSubtreeUTXOStreaming(ctx, *subtree.RootHash(), utxoDiff)
	})
	require.NoError(t, err)

	t.Logf("subtreeData %d MiB, peak heap growth %d MiB", dataSize>>20, growth>>20)

	require.Less(t, growth, uint64(dataSize/2),
		"peak heap growth must not scale with the subtreeData size (%d bytes)", dataSize)
}

// TestCreateSubtreeDataFileStreaming_ExistingFileMemoryDoesNotScaleWithSubtree
// reproduces cause 2 of issue 1914: validating an existing subtreeData file decodes
// the whole subtree into memory just to check that it parses.
func TestCreateSubtreeDataFileStreaming_ExistingFileMemoryDoesNotScaleWithSubtree(t *testing.T) {
	ctx := context.Background()
	logger := ulogger.TestLogger{}
	tSettings := test.CreateBaseTestSettings(t)

	subtreeStore, subtree, dataSize := storeLargeSubtree(t, largeSubtreeTxCount, largeSubtreeScriptSize)

	persister := New(ctx, logger, tSettings, nil, subtreeStore, nil, nil)

	// The existing-file path only uses the block for logging; it never reads its coinbase.
	block := &model.Block{
		Header: &model.BlockHeader{
			HashPrevBlock:  &chainhash.Hash{},
			HashMerkleRoot: &chainhash.Hash{},
		},
		Subtrees: []*chainhash.Hash{subtree.RootHash()},
	}

	var err error

	growth := peakHeapGrowth(t, func() {
		err = persister.CreateSubtreeDataFileStreaming(ctx, *subtree.RootHash(), block, 1)
	})
	require.NoError(t, err)

	// the existing file was accepted, not recreated
	exists, err := subtreeStore.Exists(ctx, subtree.RootHash()[:], fileformat.FileTypeSubtreeData)
	require.NoError(t, err)
	require.True(t, exists)

	t.Logf("subtreeData %d MiB, peak heap growth %d MiB", dataSize>>20, growth>>20)

	require.Less(t, growth, uint64(dataSize/2),
		"peak heap growth must not scale with the subtreeData size (%d bytes)", dataSize)
}

// smallTx returns a distinct 1-in-1-out transaction. A zero prevHash with index 0xffffffff
// makes it a coinbase.
func smallTx(t *testing.T, prevHash chainhash.Hash, prevIndex uint32, seed byte) *bt.Tx {
	t.Helper()

	input := &bt.Input{
		PreviousTxOutIndex: prevIndex,
		PreviousTxSatoshis: 2000,
		PreviousTxScript:   bscript.NewFromBytes([]byte{bscript.Op1, seed}),
		UnlockingScript:    bscript.NewFromBytes([]byte{bscript.Op1, seed}),
		SequenceNumber:     0xffffffff,
	}
	require.NoError(t, input.PreviousTxIDAdd(&prevHash))

	tx := bt.NewTx()
	tx.Inputs = append(tx.Inputs, input)
	tx.AddOutput(&bt.Output{Satoshis: 1000, LockingScript: bscript.NewFromBytes([]byte{bscript.Op1, seed})})

	return tx
}

// TestValidateSubtreeData_MatchesGoSubtree pins validateSubtreeData to the accept/reject
// behaviour of subtreepkg.NewSubtreeDataFromReader, which it replaces on the existing-file
// path. A divergence either way changes which subtreeData files the persister deletes and
// recreates.
func TestValidateSubtreeData_MatchesGoSubtree(t *testing.T) {
	coinbase := smallTx(t, chainhash.Hash{}, 0xffffffff, 0xcb)

	txs := make([]*bt.Tx, 4)
	for i := range txs {
		txs[i] = smallTx(t, chainhash.HashH([]byte{byte(i)}), uint32(i), byte(i))
	}

	newSubtree := func(t *testing.T, placeholder bool, nodes []*bt.Tx) *subtreepkg.Subtree {
		t.Helper()

		st, err := subtreepkg.NewTreeByLeafCount(4)
		require.NoError(t, err)

		if placeholder {
			require.NoError(t, st.AddCoinbaseNode())
		}

		for _, tx := range nodes {
			require.NoError(t, st.AddNode(*tx.TxIDChainHash(), 1, uint64(tx.Size())))
		}

		return st
	}

	concat := func(extended bool, list ...*bt.Tx) []byte {
		var out []byte

		for _, tx := range list {
			if extended {
				out = append(out, tx.ExtendedBytes()...)
			} else {
				out = append(out, tx.Bytes()...)
			}
		}

		return out
	}

	plain := newSubtree(t, false, txs)
	withPlaceholder := newSubtree(t, true, txs[1:])
	full := concat(true, txs...)

	cases := []struct {
		name    string
		subtree *subtreepkg.Subtree
		data    []byte
		valid   bool
	}{
		{"valid extended", plain, full, true},
		{"valid non-extended", plain, concat(false, txs...), true},
		{"placeholder with coinbase in file", withPlaceholder, concat(true, coinbase, txs[1], txs[2], txs[3]), true},
		{"placeholder without coinbase in file", withPlaceholder, concat(true, txs[1], txs[2], txs[3]), true},
		{"hash mismatch", plain, concat(true, txs[1], txs[0], txs[2], txs[3]), false},
		{"trailing extra transaction", plain, concat(true, txs[0], txs[1], txs[2], txs[3], txs[0]), false},
		{"truncated inside a transaction", plain, full[:len(full)-3], false},
		{"truncated on a transaction boundary", plain, concat(true, txs[0], txs[1]), true},
		{"empty file", plain, nil, true},
		// Both accept a file that stops inside a transaction exactly where a field
		// starts: the varint read sees a clean io.EOF, which the loop takes as end of
		// file. Pinned here as shared behaviour, not endorsed.
		{"ends inside a transaction on a field boundary", plain, []byte{0x01, 0x00, 0x00, 0x00, 0xff}, true},
		// go-bt wraps this EOF (script length read, no script bytes) with pkg/errors.
		{"ends after a script length", plain, append(append([]byte{0x01, 0x00, 0x00, 0x00, 0x01}, make([]byte, 36)...), 0x05), true},
		{"ends inside a field", plain, []byte{0x01, 0x00, 0x00, 0x00, 0xff, 0x01}, false},
		{"wrong transaction after a valid prefix", plain, concat(true, txs[0], txs[2]), false},
		// Both skip a coinbase in second position even with no placeholder node, because
		// the check is txIndex == 1 rather than "node 0 is the placeholder". Also pinned
		// as shared behaviour, not endorsed.
		{"coinbase second without a placeholder", plain, concat(true, txs[0], coinbase), true},
		{"no nodes", &subtreepkg.Subtree{}, full, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, refErr := subtreepkg.NewSubtreeDataFromReader(tc.subtree, bytes.NewReader(tc.data))
			err := validateSubtreeData(tc.subtree, bytes.NewReader(tc.data))

			require.Equal(t, tc.valid, refErr == nil, "go-subtree reference: %v", refErr)
			require.Equal(t, tc.valid, err == nil, "validateSubtreeData: %v", err)
		})
	}
}
