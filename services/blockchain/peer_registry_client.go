package blockchain

import (
	"context"

	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/util"
	"google.golang.org/grpc"
)

// PeerRegistryClientI is the client interface for the centralized peer registry.
// Services that register or query peers use this instead of the full ClientI,
// keeping the dependency surface minimal.
type PeerRegistryClientI interface {
	// RegisterPeer adds or updates a peer in the centralized registry.
	RegisterPeer(ctx context.Context, info *PeerInfo) error

	// UpdatePeerMetrics reports metrics changes for an existing peer.
	UpdatePeerMetrics(ctx context.Context, peerID string, height uint32, bytesSentDelta, bytesRecvDelta uint64, recordSuccess, recordFailure, recordMalicious bool, responseTimeMs int64) error

	// RemovePeer removes a peer from the centralized registry.
	RemovePeer(ctx context.Context, peerID string) error

	// GetPeer retrieves a single peer by ID. Returns nil and false if not found.
	GetPeer(ctx context.Context, peerID string) (*PeerInfo, bool, error)

	// ListPeers returns peers matching the given filters, sorted by reputation.
	ListPeers(ctx context.Context, transportFilter *blockchain_api.TransportType, minReputation float64, minHeight uint32, excludeBanned bool) ([]*PeerInfo, error)

	// Close releases any resources held by the client.
	// For clients created with NewPeerRegistryClientFromConn, Close is a no-op
	// because the caller owns the underlying connection.
	Close() error
}

// PeerRegistryClient is the gRPC-backed implementation of PeerRegistryClientI.
type PeerRegistryClient struct {
	client   blockchain_api.PeerRegistryServiceClient
	conn     *grpc.ClientConn
	ownsConn bool // true when this client created the connection and is responsible for closing it
}

// NewPeerRegistryClient connects to the blockchain service and returns a PeerRegistryClientI.
// It reuses the same address as the blockchain service since PeerRegistryService is served on the same port.
func NewPeerRegistryClient(ctx context.Context, address string, tSettings *settings.Settings) (PeerRegistryClientI, error) {
	conn, err := util.GetGRPCClient(ctx, address, &util.ConnectionOptions{}, tSettings)
	if err != nil {
		return nil, err
	}

	return &PeerRegistryClient{
		client:   blockchain_api.NewPeerRegistryServiceClient(conn),
		conn:     conn,
		ownsConn: true,
	}, nil
}

// RegisterPeer implements PeerRegistryClientI.
func (c *PeerRegistryClient) RegisterPeer(ctx context.Context, info *PeerInfo) error {
	_, err := c.client.RegisterPeer(ctx, &blockchain_api.RegisterPeerRequest{
		Peer: peerInfoToProto(info),
	})
	return err
}

// UpdatePeerMetrics implements PeerRegistryClientI.
func (c *PeerRegistryClient) UpdatePeerMetrics(ctx context.Context, peerID string, height uint32, bytesSentDelta, bytesRecvDelta uint64, recordSuccess, recordFailure, recordMalicious bool, responseTimeMs int64) error {
	_, err := c.client.UpdatePeerMetrics(ctx, &blockchain_api.UpdatePeerMetricsRequest{
		PeerId:             peerID,
		Height:             height,
		BytesSentDelta:     bytesSentDelta,
		BytesReceivedDelta: bytesRecvDelta,
		RecordSuccess:      recordSuccess,
		RecordFailure:      recordFailure,
		RecordMalicious:    recordMalicious,
		ResponseTimeMs:     responseTimeMs,
	})
	return err
}

// RemovePeer implements PeerRegistryClientI.
func (c *PeerRegistryClient) RemovePeer(ctx context.Context, peerID string) error {
	_, err := c.client.RemovePeer(ctx, &blockchain_api.RemovePeerRequest{PeerId: peerID})
	return err
}

// GetPeer implements PeerRegistryClientI.
func (c *PeerRegistryClient) GetPeer(ctx context.Context, peerID string) (*PeerInfo, bool, error) {
	resp, err := c.client.GetPeer(ctx, &blockchain_api.GetPeerRequest{PeerId: peerID})
	if err != nil {
		return nil, false, err
	}
	if resp.NotFound {
		return nil, false, nil
	}
	return protoToPeerInfo(resp.Peer), true, nil
}

// ListPeers implements PeerRegistryClientI.
func (c *PeerRegistryClient) ListPeers(ctx context.Context, transportFilter *blockchain_api.TransportType, minReputation float64, minHeight uint32, excludeBanned bool) ([]*PeerInfo, error) {
	req := &blockchain_api.ListPeersRequest{
		MinReputation: minReputation,
		MinHeight:     minHeight,
		ExcludeBanned: excludeBanned,
	}
	if transportFilter != nil {
		req.FilterTransport = true
		req.TransportFilter = *transportFilter
	}

	resp, err := c.client.ListPeers(ctx, req)
	if err != nil {
		return nil, err
	}

	peers := make([]*PeerInfo, 0, len(resp.Peers))
	for _, p := range resp.Peers {
		peers = append(peers, protoToPeerInfo(p))
	}
	return peers, nil
}

// Close releases the underlying gRPC connection if this client owns it.
// Clients created via NewPeerRegistryClientFromConn do not own the connection
// and Close is a no-op for them.
func (c *PeerRegistryClient) Close() error {
	if !c.ownsConn || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// NewPeerRegistryClientFromConn creates a PeerRegistryClient using an existing gRPC connection.
// The caller retains ownership of the connection — Close() on this client is a no-op.
func NewPeerRegistryClientFromConn(conn *grpc.ClientConn) PeerRegistryClientI {
	return &PeerRegistryClient{
		client:   blockchain_api.NewPeerRegistryServiceClient(conn),
		conn:     conn,
		ownsConn: false,
	}
}
