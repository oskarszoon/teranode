// Package blockvalidation provides the WireTransport implementation of CatchupTransport.
package blockvalidation

import (
	"bytes"
	"context"
	"io"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockvalidation/catchup"
)

// legacyFetchClientI is the narrow interface required by WireTransport.
// It is satisfied by services/legacy/peer.ClientI.
type legacyFetchClientI interface {
	FetchHeadersFromPeer(ctx context.Context, peerAddr string, locatorHashes []*chainhash.Hash, stopHash *chainhash.Hash) ([]byte, error)
	FetchBlockFromPeer(ctx context.Context, peerAddr string, blockHash *chainhash.Hash) ([]byte, error)
}

// WireTransport implements CatchupTransport using the Bitcoin wire protocol via
// the Legacy service's gRPC endpoints. The baseURL parameter used in each method
// is the legacy peer address ("host:port") rather than an HTTP base URL.
//
// FetchSubtree and FetchSubtreeData are not supported over the wire protocol and
// return errors.ErrServiceUnavailable.
type WireTransport struct {
	legacyClient legacyFetchClientI
}

// NewWireTransport returns a WireTransport backed by the given legacy fetch client.
func NewWireTransport(legacyClient legacyFetchClientI) *WireTransport {
	return &WireTransport{legacyClient: legacyClient}
}

// FetchHeaders implements CatchupTransport.
// peerAddr is the legacy peer address ("host:port").
// targetHash is used as the wire-protocol stop hash (the hash the peer should stop at).
func (t *WireTransport) FetchHeaders(ctx context.Context, peerAddr string, targetHash *chainhash.Hash, locatorHashes []*chainhash.Hash, maxHeaders int) ([]*model.BlockHeader, error) {
	headerBytes, err := t.legacyClient.FetchHeadersFromPeer(ctx, peerAddr, locatorHashes, targetHash)
	if err != nil {
		return nil, errors.NewProcessingError("[WireTransport.FetchHeaders] failed to fetch headers from peer %s", peerAddr, err)
	}

	if err = catchup.ValidateBlockHeaderBytes(headerBytes); err != nil {
		return nil, err
	}

	headers, err := catchup.ParseBlockHeaders(headerBytes)
	if err != nil {
		return nil, errors.NewProcessingError("[WireTransport.FetchHeaders] failed to parse headers from peer %s", peerAddr, err)
	}

	if maxHeaders > 0 && len(headers) > maxHeaders {
		headers = headers[:maxHeaders]
	}

	return headers, nil
}

// FetchBlock implements CatchupTransport.
// peerAddr is the legacy peer address ("host:port").
func (t *WireTransport) FetchBlock(ctx context.Context, peerAddr string, hash *chainhash.Hash) (*model.Block, error) {
	blockBytes, err := t.legacyClient.FetchBlockFromPeer(ctx, peerAddr, hash)
	if err != nil {
		return nil, errors.NewProcessingError("[WireTransport.FetchBlock] failed to fetch block %s from peer %s", hash.String(), peerAddr, err)
	}

	block, err := model.NewBlockFromReader(bytes.NewReader(blockBytes))
	if err != nil {
		return nil, errors.NewProcessingError("[WireTransport.FetchBlock] failed to parse block %s from peer %s", hash.String(), peerAddr, err)
	}

	return block, nil
}

// FetchBlocks is not supported over the wire protocol.
// Wire protocol requires a separate hash for each block; callers must use FetchBlock individually.
func (t *WireTransport) FetchBlocks(_ context.Context, _ string, hash *chainhash.Hash, _ uint32) ([]*model.Block, error) {
	return nil, errors.NewServiceUnavailableError("WireTransport does not support FetchBlocks (use FetchBlock per hash; starting hash %s)", hash.String())
}

// FetchSubtree is not supported over the wire protocol.
func (t *WireTransport) FetchSubtree(_ context.Context, _ string, hash *chainhash.Hash) ([]byte, error) {
	return nil, errors.NewServiceUnavailableError("WireTransport does not support FetchSubtree (hash %s)", hash.String())
}

// FetchSubtreeData is not supported over the wire protocol.
func (t *WireTransport) FetchSubtreeData(_ context.Context, _ string, hash *chainhash.Hash) (io.ReadCloser, error) {
	return nil, errors.NewServiceUnavailableError("WireTransport does not support FetchSubtreeData (hash %s)", hash.String())
}
