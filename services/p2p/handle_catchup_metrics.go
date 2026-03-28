package p2p

import (
	"context"
	"fmt"

	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/p2p/p2p_api"
)

// RecordCatchupAttempt records that a catchup attempt was made to a peer
func (s *Server) RecordCatchupAttempt(ctx context.Context, req *p2p_api.RecordCatchupAttemptRequest) (*p2p_api.RecordCatchupAttemptResponse, error) {
	if s.centralRegistry == nil {
		return &p2p_api.RecordCatchupAttemptResponse{Ok: false}, errors.WrapGRPC(errors.NewServiceError("central registry not initialized"))
	}

	// Record attempt as a metrics update (no success/failure yet)
	if err := s.centralRegistry.UpdatePeerMetrics(ctx, req.PeerId, 0, 0, 0, false, false, false, 0); err != nil {
		s.logger.Warnf("[RecordCatchupAttempt] failed to record attempt for peer %s: %v", req.PeerId, err)
	}

	return &p2p_api.RecordCatchupAttemptResponse{Ok: true}, nil
}

// RecordCatchupSuccess records a successful catchup from a peer
func (s *Server) RecordCatchupSuccess(ctx context.Context, req *p2p_api.RecordCatchupSuccessRequest) (*p2p_api.RecordCatchupSuccessResponse, error) {
	if s.centralRegistry == nil {
		return &p2p_api.RecordCatchupSuccessResponse{Ok: false}, errors.WrapGRPC(errors.NewServiceError("central registry not initialized"))
	}

	if err := s.centralRegistry.UpdatePeerMetrics(ctx, req.PeerId, 0, 0, 0, true, false, false, req.DurationMs); err != nil {
		s.logger.Warnf("[RecordCatchupSuccess] failed to record success for peer %s: %v", req.PeerId, err)
	}

	return &p2p_api.RecordCatchupSuccessResponse{Ok: true}, nil
}

// RecordCatchupFailure records a failed catchup attempt from a peer
func (s *Server) RecordCatchupFailure(ctx context.Context, req *p2p_api.RecordCatchupFailureRequest) (*p2p_api.RecordCatchupFailureResponse, error) {
	if s.centralRegistry == nil {
		return &p2p_api.RecordCatchupFailureResponse{Ok: false}, errors.WrapGRPC(errors.NewServiceError("central registry not initialized"))
	}

	if err := s.centralRegistry.UpdatePeerMetrics(ctx, req.PeerId, 0, 0, 0, false, true, false, 0); err != nil {
		s.logger.Warnf("[RecordCatchupFailure] failed to record failure for peer %s: %v", req.PeerId, err)
	}

	return &p2p_api.RecordCatchupFailureResponse{Ok: true}, nil
}

// RecordCatchupMalicious records malicious behavior detected during catchup
func (s *Server) RecordCatchupMalicious(ctx context.Context, req *p2p_api.RecordCatchupMaliciousRequest) (*p2p_api.RecordCatchupMaliciousResponse, error) {
	if s.centralRegistry == nil {
		return &p2p_api.RecordCatchupMaliciousResponse{Ok: false}, errors.WrapGRPC(errors.NewServiceError("central registry not initialized"))
	}

	if err := s.centralRegistry.UpdatePeerMetrics(ctx, req.PeerId, 0, 0, 0, false, false, true, 0); err != nil {
		s.logger.Warnf("[RecordCatchupMalicious] failed to record malicious behavior for peer %s: %v", req.PeerId, err)
	}

	return &p2p_api.RecordCatchupMaliciousResponse{Ok: true}, nil
}

// UpdateCatchupReputation updates the reputation score for a peer
func (s *Server) UpdateCatchupReputation(_ context.Context, req *p2p_api.UpdateCatchupReputationRequest) (*p2p_api.UpdateCatchupReputationResponse, error) {
	// NOTE: Reputation is computed automatically by the central registry based on interactions.
	// This explicit setter is a no-op but returns success for backward compatibility.
	s.logger.Infof("[P2P] UpdateCatchupReputation called but not yet implemented for central registry (peer=%s, score=%.2f)", req.PeerId, req.Score)
	return &p2p_api.UpdateCatchupReputationResponse{Ok: true}, nil
}

// UpdateCatchupError updates the last catchup error for a peer
func (s *Server) UpdateCatchupError(_ context.Context, req *p2p_api.UpdateCatchupErrorRequest) (*p2p_api.UpdateCatchupErrorResponse, error) {
	// NOTE: Catchup error tracking not yet implemented in central registry.
	// Log and return success for backward compatibility.
	s.logger.Infof("[P2P] UpdateCatchupError called but not yet implemented for central registry (peer=%s, error=%s)", req.PeerId, req.ErrorMsg)
	return &p2p_api.UpdateCatchupErrorResponse{Ok: true}, nil
}

// GetPeersForCatchup returns peers suitable for catchup operations
func (s *Server) GetPeersForCatchup(ctx context.Context, _ *p2p_api.GetPeersForCatchupRequest) (*p2p_api.GetPeersForCatchupResponse, error) {
	if s.centralRegistry == nil {
		return &p2p_api.GetPeersForCatchupResponse{Peers: []*p2p_api.PeerInfoForCatchup{}}, errors.WrapGRPC(errors.NewServiceError("central registry not initialized"))
	}

	// List peers with minimum reputation, excluding banned
	peers, err := s.centralRegistry.ListPeers(ctx, nil, 10.0, 0, true)
	if err != nil {
		s.logger.Errorf("[GetPeersForCatchup] failed to list peers from central registry: %v", err)
		return &p2p_api.GetPeersForCatchupResponse{Peers: []*p2p_api.PeerInfoForCatchup{}}, nil
	}

	protoPeers := make([]*p2p_api.PeerInfoForCatchup, 0, len(peers))
	for _, p := range peers {
		totalAttempts := p.InteractionSuccesses + p.InteractionFailures

		blockHashStr := ""
		if p.BlockHash != nil {
			blockHashStr = p.BlockHash.String()
		}

		protoPeers = append(protoPeers, &p2p_api.PeerInfoForCatchup{
			Id:                     p.ID,
			Height:                 p.Height,
			BlockHash:              blockHashStr,
			DataHubUrl:             p.DataHubURL,
			CatchupReputationScore: p.ReputationScore,
			CatchupAttempts:        totalAttempts,
			CatchupSuccesses:       p.InteractionSuccesses,
			CatchupFailures:        p.InteractionFailures,
		})
	}

	return &p2p_api.GetPeersForCatchupResponse{Peers: protoPeers}, nil
}

// ReportValidSubtree is a gRPC handler for reporting valid subtree reception
func (s *Server) ReportValidSubtree(ctx context.Context, req *p2p_api.ReportValidSubtreeRequest) (*p2p_api.ReportValidSubtreeResponse, error) {
	if s.centralRegistry == nil {
		return &p2p_api.ReportValidSubtreeResponse{
			Success: false,
			Message: "central registry not initialized",
		}, errors.WrapGRPC(errors.NewServiceError("central registry not initialized"))
	}

	if req.PeerId == "" {
		return &p2p_api.ReportValidSubtreeResponse{
			Success: false,
			Message: "peer ID is required",
		}, errors.WrapGRPC(errors.NewInvalidArgumentError("peer ID is required"))
	}

	if req.SubtreeHash == "" {
		return &p2p_api.ReportValidSubtreeResponse{
			Success: false,
			Message: "subtree hash is required",
		}, errors.WrapGRPC(errors.NewInvalidArgumentError("subtree hash is required"))
	}

	// Record successful subtree reception in central registry
	if err := s.centralRegistry.UpdatePeerMetrics(ctx, req.PeerId, 0, 0, 0, true, false, false, 0); err != nil {
		s.logger.Warnf("[ReportValidSubtree] failed to record success for peer %s: %v", req.PeerId, err)
	}
	s.logger.Debugf("[ReportValidSubtree] Recorded successful subtree %s from peer %s", req.SubtreeHash, req.PeerId)

	return &p2p_api.ReportValidSubtreeResponse{
		Success: true,
		Message: "subtree validation recorded",
	}, nil
}

// ReportValidBlock is a gRPC handler for reporting valid block reception
func (s *Server) ReportValidBlock(ctx context.Context, req *p2p_api.ReportValidBlockRequest) (*p2p_api.ReportValidBlockResponse, error) {
	if s.centralRegistry == nil {
		return &p2p_api.ReportValidBlockResponse{
			Success: false,
			Message: "central registry not initialized",
		}, errors.WrapGRPC(errors.NewServiceError("central registry not initialized"))
	}

	if req.PeerId == "" {
		return &p2p_api.ReportValidBlockResponse{
			Success: false,
			Message: "peer ID is required",
		}, errors.WrapGRPC(errors.NewInvalidArgumentError("peer ID is required"))
	}

	if req.BlockHash == "" {
		return &p2p_api.ReportValidBlockResponse{
			Success: false,
			Message: "block hash is required",
		}, errors.WrapGRPC(errors.NewInvalidArgumentError("block hash is required"))
	}

	// Record successful block reception in central registry
	if err := s.centralRegistry.UpdatePeerMetrics(ctx, req.PeerId, 0, 0, 0, true, false, false, 0); err != nil {
		s.logger.Warnf("[ReportValidBlock] failed to record success for peer %s: %v", req.PeerId, err)
	}
	s.logger.Debugf("[ReportValidBlock] Recorded successful block %s from peer %s", req.BlockHash, req.PeerId)

	return &p2p_api.ReportValidBlockResponse{
		Success: true,
		Message: "block validation recorded",
	}, nil
}

// IsPeerMalicious checks if a peer is considered malicious based on their behavior
func (s *Server) IsPeerMalicious(_ context.Context, req *p2p_api.IsPeerMaliciousRequest) (*p2p_api.IsPeerMaliciousResponse, error) {
	if req.PeerId == "" {
		return &p2p_api.IsPeerMaliciousResponse{
			IsMalicious: false,
			Reason:      "empty peer ID",
		}, nil
	}

	// Check if peer is banned via central registry
	if s.centralRegistry != nil {
		banned, err := s.centralRegistry.IsPeerBanned(context.Background(), req.PeerId)
		if err == nil && banned {
			return &p2p_api.IsPeerMaliciousResponse{
				IsMalicious: true,
				Reason:      "peer is banned",
			}, nil
		}
	}

	// A peer is ONLY considered malicious if they are explicitly banned.
	// Low reputation scores are handled by IsPeerUnhealthy(), not here.
	// This distinction is critical: malicious = serving invalid data, unhealthy = poor performance.
	// During catchup, we should still try unhealthy peers if they're our only option,
	// but never try truly malicious (banned) peers.

	return &p2p_api.IsPeerMaliciousResponse{
		IsMalicious: false,
		Reason:      "",
	}, nil
}

// IsPeerUnhealthy checks if a peer is considered unhealthy based on their performance
func (s *Server) IsPeerUnhealthy(_ context.Context, req *p2p_api.IsPeerUnhealthyRequest) (*p2p_api.IsPeerUnhealthyResponse, error) {
	if req.PeerId == "" {
		return &p2p_api.IsPeerUnhealthyResponse{
			IsUnhealthy:     true,
			Reason:          "empty peer ID",
			ReputationScore: 0,
		}, nil
	}

	// Check peer health via central registry
	if s.centralRegistry != nil {
		peerInfo, found, err := s.centralRegistry.GetPeer(context.Background(), req.PeerId)
		if err != nil || !found {
			return &p2p_api.IsPeerUnhealthyResponse{
				IsUnhealthy:     true,
				Reason:          "unknown peer",
				ReputationScore: 0,
			}, nil
		}

		if peerInfo.ReputationScore < 40 {
			return &p2p_api.IsPeerUnhealthyResponse{
				IsUnhealthy:     true,
				Reason:          fmt.Sprintf("low reputation score: %.2f", peerInfo.ReputationScore),
				ReputationScore: float32(peerInfo.ReputationScore),
			}, nil
		}

		totalInteractions := peerInfo.InteractionSuccesses + peerInfo.InteractionFailures
		if totalInteractions > 10 && peerInfo.InteractionSuccesses < totalInteractions/2 {
			successRate := float64(peerInfo.InteractionSuccesses) / float64(totalInteractions)
			return &p2p_api.IsPeerUnhealthyResponse{
				IsUnhealthy:     true,
				Reason:          fmt.Sprintf("low success rate: %.2f%%", successRate*100),
				ReputationScore: float32(peerInfo.ReputationScore),
			}, nil
		}

		return &p2p_api.IsPeerUnhealthyResponse{
			IsUnhealthy:     false,
			Reason:          "",
			ReputationScore: float32(peerInfo.ReputationScore),
		}, nil
	}

	return &p2p_api.IsPeerUnhealthyResponse{
		IsUnhealthy:     true,
		Reason:          "unable to determine peer health",
		ReputationScore: 0,
	}, nil
}
