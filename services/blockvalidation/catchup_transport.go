// Package blockvalidation contains the CatchupTransport interface for pluggable block/header fetching.
package blockvalidation

import (
	"context"
	"io"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/model"
)

// CatchupTransport abstracts the network transport used to fetch blocks and headers during catchup.
// Two implementations exist:
//   - HTTPTransport: fetches from the peer's DataHub HTTP endpoint (current behavior)
//   - WireTransport: delegates to Legacy service via gRPC using the Bitcoin wire protocol (future)
//
// The transport is responsible only for making network calls and parsing raw bytes into model types.
// Orchestration logic (circuit breakers, reputation, retry) lives in the Server, not in the transport.
//
// peerEndpoint is the peer's base URL for HTTP transport or network address (host:port) for wire
// protocol transport.
type CatchupTransport interface {
	// FetchHeaders retrieves block headers from a peer in a single request.
	// locatorHashes describes our current chain tip; the peer returns headers from the
	// common ancestor up to targetHash. maxHeaders caps the number of headers returned.
	FetchHeaders(ctx context.Context, peerEndpoint string, targetHash *chainhash.Hash, locatorHashes []*chainhash.Hash, maxHeaders int) ([]*model.BlockHeader, error)

	// FetchBlock fetches a single block by hash from the peer.
	FetchBlock(ctx context.Context, peerEndpoint string, hash *chainhash.Hash) (*model.Block, error)

	// FetchBlocks fetches a batch of n blocks starting at hash from the peer.
	FetchBlocks(ctx context.Context, peerEndpoint string, hash *chainhash.Hash, n uint32) ([]*model.Block, error)

	// FetchSubtree fetches the raw subtree bytes for the given hash.
	// The caller is responsible for parsing the returned bytes.
	FetchSubtree(ctx context.Context, peerEndpoint string, hash *chainhash.Hash) ([]byte, error)

	// FetchSubtreeData fetches the raw subtree data stream for the given hash.
	// The caller must close the returned ReadCloser.
	FetchSubtreeData(ctx context.Context, peerEndpoint string, hash *chainhash.Hash) (io.ReadCloser, error)
}
