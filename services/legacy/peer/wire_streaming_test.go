package peer

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/stretchr/testify/require"
)

// makeTestBlock builds a wire.MsgBlock with synthetic transactions whose
// scripts have a fixed size, useful for confirming the streaming decoder
// reconstructs the same structure that the buffered path produces.
func makeTestBlock(t *testing.T, numTxs, scriptLen int) *wire.MsgBlock {
	t.Helper()

	prev := chainhash.Hash{}
	merkle := chainhash.Hash{}
	header := wire.NewBlockHeader(1, &prev, &merkle, 0x1d00ffff, 0)

	block := wire.NewMsgBlock(header)
	for i := 0; i < numTxs; i++ {
		tx := wire.NewMsgTx(1)

		script := make([]byte, scriptLen)
		_, err := rand.Read(script)
		require.NoError(t, err)

		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{Hash: prev, Index: uint32(i)},
			SignatureScript:  script,
			Sequence:         0xffffffff,
		})
		tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: script})

		block.AddTransaction(tx)
	}

	return block
}

func TestStreamingBlockHandler_RoundTrip(t *testing.T) {
	wire.SetLimits(4000000000)

	want := makeTestBlock(t, 8, 256)

	var wire1 bytes.Buffer
	_, err := wire.WriteMessageN(&wire1, want, wire.ProtocolVersion, wire.MainNet)
	require.NoError(t, err)

	// Skip the 24-byte header so the handler receives the payload reader
	// it would get inside ReadMessageWithEncodingN.
	payloadLen := uint64(wire1.Len() - 24)
	_ = wire1.Next(24)

	n, msg, raw, err := streamingBlockHandler(&wire1, payloadLen, 24)
	require.NoError(t, err)
	require.Equal(t, int(payloadLen)+24, n)
	require.Nil(t, raw, "streaming handler must not retain the payload bytes")

	got, ok := msg.(*wire.MsgBlock)
	require.True(t, ok, "expected *wire.MsgBlock, got %T", msg)
	require.Equal(t, want.BlockHash(), got.BlockHash())
	require.Equal(t, len(want.Transactions), len(got.Transactions))

	for i := range want.Transactions {
		require.Equal(t,
			want.Transactions[i].TxHash(),
			got.Transactions[i].TxHash(),
			"tx %d hash mismatch", i)
	}
}

// TestStreamingBlockHandler_DrainsOnError verifies that a corrupted payload
// does not leave unread bytes on the underlying reader — otherwise the next
// ReadMessage call would parse those bytes as a fresh wire header and
// desync the stream.
func TestStreamingBlockHandler_DrainsOnError(t *testing.T) {
	const declared = 1024
	const truncated = 32

	// declared length 1024 but only 32 bytes of garbage followed by an
	// identifiable tail so we can detect whether the handler over-reads.
	payload := bytes.NewBuffer(make([]byte, truncated))
	tail := []byte("MARKER-AFTER-PAYLOAD")
	src := io.MultiReader(payload, bytes.NewReader(make([]byte, declared-truncated)), bytes.NewReader(tail))

	_, _, _, _ = streamingBlockHandler(src, declared, 0)

	got := make([]byte, len(tail))
	_, err := io.ReadFull(src, got)
	require.NoError(t, err)
	require.Equal(t, tail, got, "handler must drain exactly the declared payload, leaving the next message intact")
}

// TestRegisterStreamingBlockHandler_Idempotent guards against double
// registration causing the handler to be set repeatedly (cheap to test;
// the sync.Once contract is what we actually care about).
func TestRegisterStreamingBlockHandler_Idempotent(t *testing.T) {
	RegisterStreamingBlockHandler()
	RegisterStreamingBlockHandler()
	RegisterStreamingBlockHandler()
}
