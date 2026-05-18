package peer

import (
	"io"
	"sync"

	"github.com/bsv-blockchain/go-wire"
)

// streamingBlockHandler is a wire.SetExternalHandler implementation for the
// "block" message that decodes the block payload directly from the network
// reader, avoiding the default ReadMessageWithEncodingN behaviour of
// allocating the full payload as a []byte before calling Bsvdecode. On fat
// blocks (multi-GB testnet stress blocks) that buffer alone reached
// ~2.86 GB of legacy heap inuse during sync, the second-largest contributor
// to RSS after the per-tx scratch buffer.
//
// Note on the wire-level DoubleHash checksum: the default path verifies the
// peer-supplied checksum over the payload bytes. This handler skips it, but
// integrity is preserved by the existing downstream validation in
// netsync.HandleBlockDirect — PoW via HasMetTargetDifficulty, merkle root
// reconstruction during subtree preparation, and per-tx parse + validate.
// Any wire-corruption that a checksum would have caught also fails one of
// those downstream checks. TCP framing CRC covers the remaining
// transit-corruption surface. The default path only retains the checksum
// because it must hold the full payload in []byte anyway; preserving that
// property under streaming (a TeeReader → SHA-256 pass over multi-GB
// payloads) is not justified given the downstream guarantees.
func streamingBlockHandler(r io.Reader, length uint64, totalBytes int) (int, wire.Message, []byte, error) {
	// Cap the inner decoder so a malformed varint cannot read past the
	// declared payload boundary and desync the next ReadMessage call.
	lr := &io.LimitedReader{R: r, N: int64(length)}

	msg := &wire.MsgBlock{}
	decodeErr := msg.Bsvdecode(lr, wire.ProtocolVersion, wire.BaseEncoding)

	// Always drain any unread payload bytes — both on error (so the next
	// ReadMessage sees a fresh header) and on success (in case the encoded
	// length included trailing bytes the decoder did not consume).
	if lr.N > 0 {
		_, _ = io.Copy(io.Discard, lr)
	}

	// totalBytes accounts for the header already read by
	// ReadMessageWithEncodingN; add the full declared payload length so
	// the caller's bytesReceived counter stays consistent with the
	// non-streaming path regardless of how many bytes Bsvdecode actually
	// consumed before erroring.
	return totalBytes + int(length), msg, nil, decodeErr
}

var registerStreamingBlockHandlerOnce sync.Once

// RegisterStreamingBlockHandler installs the streaming "block" handler with
// go-wire globally. Safe to call multiple times; the registration runs at
// most once. Call this once during legacy service startup, after any other
// wire-level configuration (e.g. wire.SetLimits).
func RegisterStreamingBlockHandler() {
	registerStreamingBlockHandlerOnce.Do(func() {
		wire.SetExternalHandler(wire.CmdBlock, streamingBlockHandler)
	})
}
