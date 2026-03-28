package blockvalidation

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// FetchBlock — additional coverage
// ---------------------------------------------------------------------------

func TestWireTransport_FetchBlock_SuccessBlockContent(t *testing.T) {
	blockBytes := buildMinimalBlockBytes(t)
	mock := &mockLegacyFetchClient{blockBytes: blockBytes}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	block, err := tr.FetchBlock(context.Background(), "peer:8333", hash)
	require.NoError(t, err)
	require.NotNil(t, block)
	require.NotNil(t, block.Header)
}

func TestWireTransport_FetchBlock_EmptyBytes(t *testing.T) {
	mock := &mockLegacyFetchClient{blockBytes: []byte{}}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	_, err := tr.FetchBlock(context.Background(), "peer:8333", hash)
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to parse block")
}

func TestWireTransport_FetchBlock_NilBytes(t *testing.T) {
	mock := &mockLegacyFetchClient{blockBytes: nil}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	_, err := tr.FetchBlock(context.Background(), "peer:8333", hash)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// FetchHeaders — maxHeaders boundary cases
// ---------------------------------------------------------------------------

func TestWireTransport_FetchHeaders_MaxHeaders_ZeroMeansUnlimited(t *testing.T) {
	// Two valid headers, maxHeaders=0 means no cap.
	hdrBytes := genesisHeaderBytes(t)
	twoHeaders := append(hdrBytes, hdrBytes...)
	mock := &mockLegacyFetchClient{headerBytes: twoHeaders}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	headers, err := tr.FetchHeaders(context.Background(), "peer:8333", hash, []*chainhash.Hash{hash}, 0)
	require.NoError(t, err)
	require.Len(t, headers, 2)
}

func TestWireTransport_FetchHeaders_MaxHeaders_NegativeMeansUnlimited(t *testing.T) {
	hdrBytes := genesisHeaderBytes(t)
	twoHeaders := append(hdrBytes, hdrBytes...)
	mock := &mockLegacyFetchClient{headerBytes: twoHeaders}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	headers, err := tr.FetchHeaders(context.Background(), "peer:8333", hash, []*chainhash.Hash{hash}, -1)
	require.NoError(t, err)
	require.Len(t, headers, 2)
}

func TestWireTransport_FetchHeaders_MaxHeaders_ExactMatch(t *testing.T) {
	hdrBytes := genesisHeaderBytes(t)
	twoHeaders := append(hdrBytes, hdrBytes...)
	mock := &mockLegacyFetchClient{headerBytes: twoHeaders}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	headers, err := tr.FetchHeaders(context.Background(), "peer:8333", hash, []*chainhash.Hash{hash}, 2)
	require.NoError(t, err)
	require.Len(t, headers, 2)
}

func TestWireTransport_FetchHeaders_MaxHeaders_MoreThanAvailable(t *testing.T) {
	hdrBytes := genesisHeaderBytes(t)
	mock := &mockLegacyFetchClient{headerBytes: hdrBytes}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	headers, err := tr.FetchHeaders(context.Background(), "peer:8333", hash, []*chainhash.Hash{hash}, 100)
	require.NoError(t, err)
	require.Len(t, headers, 1)
}

// ---------------------------------------------------------------------------
// Unsupported methods — verify error messages contain context
// ---------------------------------------------------------------------------

func TestWireTransport_FetchBlocks_ErrorContainsHash(t *testing.T) {
	mock := &mockLegacyFetchClient{}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	_, err := tr.FetchBlocks(context.Background(), "peer:8333", hash, 5)
	require.Error(t, err)
	require.ErrorContains(t, err, "not supported over the wire protocol")
	require.ErrorContains(t, err, hash.String())
}

func TestWireTransport_FetchSubtree_ErrorContainsHash(t *testing.T) {
	mock := &mockLegacyFetchClient{}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	_, err := tr.FetchSubtree(context.Background(), "peer:8333", hash)
	require.Error(t, err)
	require.ErrorContains(t, err, "not supported over the wire protocol")
	require.ErrorContains(t, err, hash.String())
}

func TestWireTransport_FetchSubtreeData_ErrorContainsHash(t *testing.T) {
	mock := &mockLegacyFetchClient{}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	_, err := tr.FetchSubtreeData(context.Background(), "peer:8333", hash)
	require.Error(t, err)
	require.ErrorContains(t, err, "not supported over the wire protocol")
	require.ErrorContains(t, err, hash.String())
}

// ---------------------------------------------------------------------------
// Unsupported methods — verify error type is ServiceUnavailable
// ---------------------------------------------------------------------------

func TestWireTransport_UnsupportedMethods_ErrorType(t *testing.T) {
	mock := &mockLegacyFetchClient{}
	tr := NewWireTransport(mock)
	hash := makeHash(t)

	tests := []struct {
		name string
		fn   func() error
	}{
		{
			name: "FetchBlocks",
			fn: func() error {
				_, err := tr.FetchBlocks(context.Background(), "peer:8333", hash, 1)
				return err
			},
		},
		{
			name: "FetchSubtree",
			fn: func() error {
				_, err := tr.FetchSubtree(context.Background(), "peer:8333", hash)
				return err
			},
		},
		{
			name: "FetchSubtreeData",
			fn: func() error {
				_, err := tr.FetchSubtreeData(context.Background(), "peer:8333", hash)
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.fn()
			require.Error(t, err)

			var tnErr *errors.Error
			require.ErrorAs(t, err, &tnErr)
			require.Equal(t, errors.ERR_SERVICE_UNAVAILABLE, tnErr.Code())
		})
	}
}

// ---------------------------------------------------------------------------
// FetchBlock / FetchHeaders — error wrapping
// ---------------------------------------------------------------------------

func TestWireTransport_FetchBlock_ErrorMessage(t *testing.T) {
	clientErr := errors.NewNetworkError("timeout")
	mock := &mockLegacyFetchClient{blockErr: clientErr}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	_, err := tr.FetchBlock(context.Background(), "peer:8333", hash)
	require.Error(t, err)
	require.ErrorContains(t, err, hash.String())
	require.ErrorContains(t, err, "peer:8333")
}

func TestWireTransport_FetchHeaders_ErrorMessage(t *testing.T) {
	clientErr := errors.NewNetworkError("reset")
	mock := &mockLegacyFetchClient{headerErr: clientErr}
	tr := NewWireTransport(mock)

	hash := makeHash(t)
	_, err := tr.FetchHeaders(context.Background(), "peer:9999", hash, []*chainhash.Hash{hash}, 10)
	require.Error(t, err)
	require.ErrorContains(t, err, "peer:9999")
}

// ---------------------------------------------------------------------------
// Multiple locator hashes
// ---------------------------------------------------------------------------

func TestWireTransport_FetchHeaders_MultipleLocators(t *testing.T) {
	hdrBytes := genesisHeaderBytes(t)
	mock := &mockLegacyFetchClient{headerBytes: hdrBytes}
	tr := NewWireTransport(mock)

	hash1 := makeHash(t)
	hash2, err := chainhash.NewHashFromStr("0000000000000000000000000000000000000000000000000000000000000002")
	require.NoError(t, err)

	headers, err := tr.FetchHeaders(context.Background(), "peer:8333", hash1, []*chainhash.Hash{hash1, hash2}, 10000)
	require.NoError(t, err)
	require.Len(t, headers, 1)
}
