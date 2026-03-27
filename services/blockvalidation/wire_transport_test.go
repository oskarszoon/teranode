package blockvalidation

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/stretchr/testify/require"
)

// mockLegacyFetchClient is a test double for legacyFetchClientI.
type mockLegacyFetchClient struct {
	headerBytes []byte
	headerErr   error
	blockBytes  []byte
	blockErr    error
}

func (m *mockLegacyFetchClient) FetchHeadersFromPeer(_ context.Context, _ string, _ []*chainhash.Hash, _ *chainhash.Hash) ([]byte, error) {
	return m.headerBytes, m.headerErr
}

func (m *mockLegacyFetchClient) FetchBlockFromPeer(_ context.Context, _ string, _ *chainhash.Hash) ([]byte, error) {
	return m.blockBytes, m.blockErr
}

func TestWireTransport_FetchHeaders_Success(t *testing.T) {
	hdrBytes := genesisHeaderBytes(t)
	mock := &mockLegacyFetchClient{headerBytes: hdrBytes}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	headers, err := tr.FetchHeaders(context.Background(), "peer:8333", hash, []*chainhash.Hash{hash}, 10000)
	require.NoError(t, err)
	require.Len(t, headers, 1)
}

func TestWireTransport_FetchHeaders_Empty(t *testing.T) {
	mock := &mockLegacyFetchClient{headerBytes: nil}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	headers, err := tr.FetchHeaders(context.Background(), "peer:8333", hash, []*chainhash.Hash{hash}, 10000)
	require.NoError(t, err)
	require.Empty(t, headers)
}

func TestWireTransport_FetchHeaders_ClientError(t *testing.T) {
	clientErr := errors.NewNetworkError("peer disconnected")
	mock := &mockLegacyFetchClient{headerErr: clientErr}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	_, err := tr.FetchHeaders(context.Background(), "peer:8333", hash, []*chainhash.Hash{hash}, 10000)
	require.Error(t, err)
	require.ErrorContains(t, err, "peer disconnected")
}

func TestWireTransport_FetchHeaders_InvalidBytes(t *testing.T) {
	// 81 bytes is not a multiple of 80 — should fail validation.
	mock := &mockLegacyFetchClient{headerBytes: make([]byte, 81)}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	_, err := tr.FetchHeaders(context.Background(), "peer:8333", hash, []*chainhash.Hash{hash}, 10000)
	require.Error(t, err)
}

func TestWireTransport_FetchHeaders_MaxHeadersCap(t *testing.T) {
	// Return 2 valid headers but cap at 1.
	hdrBytes := genesisHeaderBytes(t)
	twoHeaders := append(hdrBytes, hdrBytes...)
	mock := &mockLegacyFetchClient{headerBytes: twoHeaders}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	headers, err := tr.FetchHeaders(context.Background(), "peer:8333", hash, []*chainhash.Hash{hash}, 1)
	require.NoError(t, err)
	require.Len(t, headers, 1)
}

func TestWireTransport_FetchBlock_Success(t *testing.T) {
	// Build a minimal valid block bytes using model.
	blockBytes := buildMinimalBlockBytes(t)
	mock := &mockLegacyFetchClient{blockBytes: blockBytes}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	block, err := tr.FetchBlock(context.Background(), "peer:8333", hash)
	require.NoError(t, err)
	require.NotNil(t, block)
}

func TestWireTransport_FetchBlock_ClientError(t *testing.T) {
	clientErr := errors.NewNetworkError("connection refused")
	mock := &mockLegacyFetchClient{blockErr: clientErr}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	_, err := tr.FetchBlock(context.Background(), "peer:8333", hash)
	require.Error(t, err)
	require.ErrorContains(t, err, "connection refused")
}

func TestWireTransport_FetchBlock_MalformedBytes(t *testing.T) {
	mock := &mockLegacyFetchClient{blockBytes: []byte("not a block")}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	_, err := tr.FetchBlock(context.Background(), "peer:8333", hash)
	require.Error(t, err)
}

func TestWireTransport_FetchBlocks_Unsupported(t *testing.T) {
	mock := &mockLegacyFetchClient{}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	_, err := tr.FetchBlocks(context.Background(), "peer:8333", hash, 5)
	require.Error(t, err)
}

func TestWireTransport_FetchSubtree_Unsupported(t *testing.T) {
	mock := &mockLegacyFetchClient{}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	_, err := tr.FetchSubtree(context.Background(), "peer:8333", hash)
	require.Error(t, err)
}

func TestWireTransport_FetchSubtreeData_Unsupported(t *testing.T) {
	mock := &mockLegacyFetchClient{}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	_, err := tr.FetchSubtreeData(context.Background(), "peer:8333", hash)
	require.Error(t, err)
}

// buildMinimalBlockBytes returns raw bytes for a block with a coinbase transaction
// sufficient for model.NewBlockFromReader to parse successfully.
func buildMinimalBlockBytes(t *testing.T) []byte {
	t.Helper()

	// Minimal block: 80-byte header + varint(1 tx) + coinbase tx bytes.
	// We use the genesis header to avoid constructing a valid PoW header.
	hdrBytes := genesisHeaderBytes(t)

	// Minimal coinbase tx: version(4) + vin_count(varint 1) + vin + vout_count(varint 1) + vout + locktime(4)
	// vin: prevhash(32 zeros) + previndex(0xffffffff) + script_len(varint 1) + script(1 byte) + sequence(0xffffffff)
	// vout: value(0, 8 bytes) + script_len(varint 1) + script(1 byte OP_TRUE = 0x51)
	var tx bytes.Buffer
	// version
	_ = binary.Write(&tx, binary.LittleEndian, uint32(1))
	// vin count
	tx.WriteByte(1)
	// prevhash (32 zero bytes)
	tx.Write(make([]byte, 32))
	// previndex 0xffffffff
	_ = binary.Write(&tx, binary.LittleEndian, uint32(0xffffffff))
	// script length + coinbase data
	tx.WriteByte(1)
	tx.WriteByte(0x00)
	// sequence
	_ = binary.Write(&tx, binary.LittleEndian, uint32(0xffffffff))
	// vout count
	tx.WriteByte(1)
	// value (0 satoshis)
	_ = binary.Write(&tx, binary.LittleEndian, uint64(0))
	// script length + OP_TRUE
	tx.WriteByte(1)
	tx.WriteByte(0x51)
	// locktime
	_ = binary.Write(&tx, binary.LittleEndian, uint32(0))

	var block bytes.Buffer
	block.Write(hdrBytes)
	// tx count varint
	block.WriteByte(1)
	block.Write(tx.Bytes())

	return block.Bytes()
}

// Verify WireTransport satisfies CatchupTransport at compile time.
var _ CatchupTransport = (*WireTransport)(nil)

// Verify mockLegacyFetchClient satisfies legacyFetchClientI at compile time.
var _ legacyFetchClientI = (*mockLegacyFetchClient)(nil)
