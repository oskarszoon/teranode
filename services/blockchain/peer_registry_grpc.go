package blockchain

import (
	"context"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// RegisterPeer adds or updates a peer in the centralized registry.
func (b *Blockchain) RegisterPeer(_ context.Context, req *blockchain_api.RegisterPeerRequest) (*emptypb.Empty, error) {
	if req.Peer == nil {
		return nil, status.Error(codes.InvalidArgument, "peer info is required")
	}
	info := protoToPeerInfo(req.Peer)
	b.peerRegistry.Register(info)
	return &emptypb.Empty{}, nil
}

// UpdatePeerMetrics updates runtime counters for an existing peer without a full re-registration.
func (b *Blockchain) UpdatePeerMetrics(_ context.Context, req *blockchain_api.UpdatePeerMetricsRequest) (*emptypb.Empty, error) {
	b.peerRegistry.UpdateMetrics(
		req.PeerId,
		req.Height,
		req.BytesSentDelta,
		req.BytesReceivedDelta,
		req.RecordSuccess,
		req.RecordFailure,
		req.RecordMalicious,
		req.ResponseTimeMs,
	)
	return &emptypb.Empty{}, nil
}

// RemovePeer removes a peer from the registry on disconnect or explicit eviction.
func (b *Blockchain) RemovePeer(_ context.Context, req *blockchain_api.RemovePeerRequest) (*emptypb.Empty, error) {
	b.peerRegistry.Remove(req.PeerId)
	return &emptypb.Empty{}, nil
}

// ListPeers returns all peers matching the given filters, sorted by reputation descending.
func (b *Blockchain) ListPeers(_ context.Context, req *blockchain_api.ListPeersRequest) (*blockchain_api.ListPeersResponse, error) {
	// Only apply the transport filter when the caller has explicitly set one; a zero value
	// for a proto enum is indistinguishable from "not set" without the boolean sentinel.
	var tf *blockchain_api.TransportType
	if req.FilterTransport {
		t := req.TransportFilter
		tf = &t
	}

	infos := b.peerRegistry.List(tf, req.MinReputation, req.MinHeight, req.ExcludeBanned)

	peers := make([]*blockchain_api.PeerRegistryInfo, 0, len(infos))
	for _, info := range infos {
		peers = append(peers, peerInfoToProto(info))
	}

	return &blockchain_api.ListPeersResponse{Peers: peers}, nil
}

// GetPeer retrieves a single peer by ID.
func (b *Blockchain) GetPeer(_ context.Context, req *blockchain_api.GetPeerRequest) (*blockchain_api.GetPeerResponse, error) {
	info, ok := b.peerRegistry.Get(req.PeerId)
	if !ok {
		return &blockchain_api.GetPeerResponse{NotFound: true}, nil
	}
	return &blockchain_api.GetPeerResponse{Peer: peerInfoToProto(info)}, nil
}

// peerInfoToProto converts the domain PeerInfo type to its protobuf representation.
func peerInfoToProto(info *PeerInfo) *blockchain_api.PeerRegistryInfo {
	return &blockchain_api.PeerRegistryInfo{
		PeerId:                 info.ID,
		TransportType:          info.TransportType,
		ClientName:             info.ClientName,
		Height:                 info.Height,
		DataHubUrl:             info.DataHubURL,
		NetworkAddress:         info.NetworkAddress,
		IsBanned:               info.IsBanned,
		BanScore:               info.BanScore,
		Storage:                info.Storage,
		BytesSent:              info.BytesSent,
		BytesReceived:          info.BytesReceived,
		InteractionAttempts:    info.InteractionAttempts,
		InteractionSuccesses:   info.InteractionSuccesses,
		InteractionFailures:    info.InteractionFailures,
		MaliciousCount:         info.MaliciousCount,
		ReputationScore:        info.ReputationScore,
		AvgResponseTimeMs:      info.AvgResponseTimeMs,
		ConnectedAt:            timestamppb.New(info.ConnectedAt),
		LastMessageTime:        timestamppb.New(info.LastMessageTime),
		LastInteractionAttempt: timestamppb.New(info.LastInteractionAttempt),
		LastInteractionSuccess: timestamppb.New(info.LastInteractionSuccess),
		LastInteractionFailure: timestamppb.New(info.LastInteractionFailure),
		LastSeen:               timestamppb.New(info.LastSeen),
		BlockHash:              blockHashToBytes(info.BlockHash),
	}
}

// protoToPeerInfo converts a protobuf PeerRegistryInfo to the domain PeerInfo type.
func protoToPeerInfo(p *blockchain_api.PeerRegistryInfo) *PeerInfo {
	return &PeerInfo{
		ID:                     p.PeerId,
		TransportType:          p.TransportType,
		ClientName:             p.ClientName,
		Height:                 p.Height,
		DataHubURL:             p.DataHubUrl,
		NetworkAddress:         p.NetworkAddress,
		IsBanned:               p.IsBanned,
		BanScore:               p.BanScore,
		Storage:                p.Storage,
		BytesSent:              p.BytesSent,
		BytesReceived:          p.BytesReceived,
		InteractionAttempts:    p.InteractionAttempts,
		InteractionSuccesses:   p.InteractionSuccesses,
		InteractionFailures:    p.InteractionFailures,
		MaliciousCount:         p.MaliciousCount,
		ReputationScore:        p.ReputationScore,
		AvgResponseTimeMs:      p.AvgResponseTimeMs,
		ConnectedAt:            protoTimeToTime(p.ConnectedAt),
		LastMessageTime:        protoTimeToTime(p.LastMessageTime),
		LastInteractionAttempt: protoTimeToTime(p.LastInteractionAttempt),
		LastInteractionSuccess: protoTimeToTime(p.LastInteractionSuccess),
		LastInteractionFailure: protoTimeToTime(p.LastInteractionFailure),
		LastSeen:               protoTimeToTime(p.LastSeen),
		BlockHash:              bytesToBlockHash(p.BlockHash),
	}
}

// protoTimeToTime converts a nullable proto timestamp to time.Time, returning the zero value when nil.
func protoTimeToTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

// blockHashToBytes converts a *chainhash.Hash to a byte slice, returning nil if hash is nil.
func blockHashToBytes(h *chainhash.Hash) []byte {
	if h == nil {
		return nil
	}
	return h[:]
}

// bytesToBlockHash converts a byte slice to *chainhash.Hash, returning nil if the slice is empty or invalid.
func bytesToBlockHash(b []byte) *chainhash.Hash {
	if len(b) == 0 {
		return nil
	}
	h, err := chainhash.NewHash(b)
	if err != nil {
		return nil
	}
	return h
}
