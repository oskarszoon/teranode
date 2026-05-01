package httpimpl

import (
	"context"
	"net/http"
	"time"

	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/p2p"
	"github.com/labstack/echo/v4"
	"github.com/libp2p/go-libp2p/core/peer"
)

// PeerInfoResponse represents the JSON response for a single peer.
//
// swagger:model PeerInfoResponse
type PeerInfoResponse struct {
	ID              string `json:"id"`
	ClientName      string `json:"client_name"`
	Height          uint32 `json:"height"`
	BlockHash       string `json:"block_hash"`
	DataHubURL      string `json:"data_hub_url"`
	BanScore        int    `json:"ban_score"`
	IsBanned        bool   `json:"is_banned"`
	IsConnected     bool   `json:"is_connected"`
	ConnectedAt     int64  `json:"connected_at"`
	BytesReceived   uint64 `json:"bytes_received"`
	LastBlockTime   int64  `json:"last_block_time"`
	LastMessageTime int64  `json:"last_message_time"`

	// Catchup metrics
	CatchupAttempts        int64   `json:"catchup_attempts"`
	CatchupSuccesses       int64   `json:"catchup_successes"`
	CatchupFailures        int64   `json:"catchup_failures"`
	CatchupLastAttempt     int64   `json:"catchup_last_attempt"`
	CatchupLastSuccess     int64   `json:"catchup_last_success"`
	CatchupLastFailure     int64   `json:"catchup_last_failure"`
	CatchupReputationScore float64 `json:"catchup_reputation_score"`
	CatchupMaliciousCount  int64   `json:"catchup_malicious_count"`
	CatchupAvgResponseTime int64   `json:"catchup_avg_response_ms"`
	LastCatchupError       string  `json:"last_catchup_error"`
	LastCatchupErrorTime   int64   `json:"last_catchup_error_time"`
}

// PeersResponse represents the JSON response containing all peers.
//
// swagger:model PeersResponse
type PeersResponse struct {
	Peers []PeerInfoResponse `json:"peers"`
	Count int                `json:"count"`
}

// GetPeers returns the current peer registry data from the centralized registry
// in the blockchain service. All peers (P2P and legacy) are stored there.
func (h *HTTP) GetPeers(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), 5*time.Second)
	defer cancel()

	// Query the central registry directly — it's the single source of truth
	// for all peers regardless of transport (HTTP/P2P and wire/legacy).
	peers, err := h.getPeersFromCentralRegistry(ctx)
	if err != nil {
		h.logger.Errorf("[GetPeers] Failed to get peers from central registry: %v", err)
		return c.JSON(http.StatusServiceUnavailable, PeersResponse{
			Peers: []PeerInfoResponse{},
			Count: 0,
		})
	}

	return c.JSON(http.StatusOK, peersToResponse(peers))
}

// getPeersFromCentralRegistry queries the blockchain service's PeerRegistryService directly.
// Used as a fallback when P2P is not running (e.g., legacy-only mode).
// peersToResponse converts native p2p.PeerInfo values into the dashboard JSON
// response shape. Pure function; extracted from GetPeers for unit testing.
func peersToResponse(peers []*p2p.PeerInfo) PeersResponse {
	peerResponses := make([]PeerInfoResponse, 0, len(peers))
	for _, peerPtr := range peers {
		peer := (*p2p.PeerInfo)(peerPtr) // Explicit type assertion to satisfy import checker

		blockHashStr := ""
		if peer.BlockHash != nil {
			blockHashStr = peer.BlockHash.String()
		}

		peerResponses = append(peerResponses, PeerInfoResponse{
			ID:              peer.ID.String(),
			ClientName:      peer.ClientName,
			Height:          peer.Height,
			BlockHash:       blockHashStr,
			DataHubURL:      peer.DataHubURL,
			BanScore:        peer.BanScore,
			IsBanned:        peer.IsBanned,
			IsConnected:     peer.IsConnected,
			ConnectedAt:     peer.ConnectedAt.Unix(),
			BytesReceived:   peer.BytesReceived,
			LastBlockTime:   peer.LastBlockTime.Unix(),
			LastMessageTime: peer.LastMessageTime.Unix(),

			// Interaction/catchup metrics (using the original field names for backward compatibility)
			CatchupAttempts:        peer.InteractionAttempts,
			CatchupSuccesses:       peer.InteractionSuccesses,
			CatchupFailures:        peer.InteractionFailures,
			CatchupLastAttempt:     peer.LastInteractionAttempt.Unix(),
			CatchupLastSuccess:     peer.LastInteractionSuccess.Unix(),
			CatchupLastFailure:     peer.LastInteractionFailure.Unix(),
			CatchupReputationScore: peer.ReputationScore,
			CatchupMaliciousCount:  peer.MaliciousCount,
			CatchupAvgResponseTime: peer.AvgResponseTime.Milliseconds(),
			LastCatchupError:       peer.LastCatchupError,
			LastCatchupErrorTime:   peer.LastCatchupErrorTime.Unix(),
		})
	}

	return PeersResponse{
		Peers: peerResponses,
		Count: len(peerResponses),
	}
}

func (h *HTTP) getPeersFromCentralRegistry(ctx context.Context) ([]*p2p.PeerInfo, error) {
	blockchainClient := h.repository.GetBlockchainClient()
	if blockchainClient == nil {
		// Intentional: returns empty list when blockchain client is not configured
		// (e.g. legacy-only or partial deployment modes). Caller returns 200 with count=0.
		return nil, nil
	}

	// Use the blockchain gRPC address to create a peer registry client.
	// The PeerRegistryService is served on the same gRPC port as BlockchainAPI.
	registryClient, err := blockchain.NewPeerRegistryClient(ctx, h.settings.BlockChain.GRPCAddress, h.settings)
	if err != nil {
		return nil, err
	}
	defer registryClient.Close()

	centralPeers, err := registryClient.ListPeers(ctx, nil, 0, 0, false)
	if err != nil {
		return nil, err
	}

	peers := make([]*p2p.PeerInfo, 0, len(centralPeers))
	for _, cp := range centralPeers {
		peerInfo := &p2p.PeerInfo{
			ClientName:             cp.ClientName,
			Height:                 cp.Height,
			DataHubURL:             cp.DataHubURL,
			BanScore:               int(cp.BanScore),
			IsBanned:               cp.IsBanned,
			ConnectedAt:            cp.ConnectedAt,
			BytesReceived:          cp.BytesReceived,
			LastMessageTime:        cp.LastMessageTime,
			Storage:                cp.Storage,
			InteractionAttempts:    cp.InteractionAttempts,
			InteractionSuccesses:   cp.InteractionSuccesses,
			InteractionFailures:    cp.InteractionFailures,
			LastInteractionAttempt: cp.LastInteractionAttempt,
			LastInteractionSuccess: cp.LastInteractionSuccess,
			LastInteractionFailure: cp.LastInteractionFailure,
			ReputationScore:        cp.ReputationScore,
			MaliciousCount:         cp.MaliciousCount,
			AvgResponseTime:        time.Duration(cp.AvgResponseTimeMs) * time.Millisecond,
		}
		// For legacy peers, ID is the network address (host:port) — not a valid libp2p peer ID.
		// Set it as raw bytes so .String() returns the address.
		peerInfo.ID = peer.ID(cp.ID)
		if cp.BlockHash != nil {
			peerInfo.BlockHash = cp.BlockHash
		}
		if cp.NetworkAddress != "" {
			peerInfo.DataHubURL = cp.NetworkAddress
		}
		peers = append(peers, peerInfo)
	}

	return peers, nil
}
