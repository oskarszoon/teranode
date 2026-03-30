package peer

import (
	"context"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/services/legacy/peer_api"
	"google.golang.org/protobuf/types/known/emptypb"
)

// ClientI defines the interface for legacy peer management operations.
// It provides methods for querying connected peers and managing the ban list
// through gRPC calls to the legacy service.
type ClientI interface {
	// GetPeers retrieves information about all currently connected legacy peers.
	GetPeers(ctx context.Context) (*peer_api.GetPeersResponse, error)

	// BanPeer adds a peer to the ban list, preventing future connections.
	BanPeer(ctx context.Context, peer *peer_api.BanPeerRequest) (*peer_api.BanPeerResponse, error)

	// UnbanPeer removes a peer from the ban list, allowing it to reconnect.
	UnbanPeer(ctx context.Context, peer *peer_api.UnbanPeerRequest) (*peer_api.UnbanPeerResponse, error)

	// IsBanned checks whether a specific peer is currently banned.
	IsBanned(ctx context.Context, peer *peer_api.IsBannedRequest) (*peer_api.IsBannedResponse, error)

	// ListBanned returns all currently banned peers.
	ListBanned(ctx context.Context, _ *emptypb.Empty) (*peer_api.ListBannedResponse, error)

	// ClearBanned removes all entries from the ban list.
	ClearBanned(ctx context.Context, _ *emptypb.Empty) (*peer_api.ClearBannedResponse, error)

	// FetchHeadersFromPeer sends a getheaders wire-protocol message to the specified connected peer
	// and returns the raw concatenated 80-byte block headers received in the response.
	// locatorHashes describes the caller's chain position; stopHash is the last hash the peer
	// should include (all-zeros means return up to chain tip).
	FetchHeadersFromPeer(ctx context.Context, peerAddr string, locatorHashes []*chainhash.Hash, stopHash *chainhash.Hash) ([]byte, error)

	// FetchBlockFromPeer sends a getdata wire-protocol message to the specified connected peer
	// and returns the raw serialized block bytes received in the response.
	FetchBlockFromPeer(ctx context.Context, peerAddr string, blockHash *chainhash.Hash) ([]byte, error)

	// DelegateCatchup asks the legacy service to run wire-protocol catchup using the
	// specified peer up to targetHeight. Progress messages are sent on progressCh.
	// The channel is closed when the operation completes. The method blocks until done.
	DelegateCatchup(ctx context.Context, peerAddr string, targetHeight uint32, progressCh chan<- *peer_api.CatchupProgress) error
}
