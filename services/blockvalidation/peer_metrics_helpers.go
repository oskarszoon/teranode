package blockvalidation

import (
	"context"
	"time"
)

// reportCatchupAttempt reports a catchup attempt to the central registry.
func (u *Server) reportCatchupAttempt(ctx context.Context, peerID string) {
	if peerID == "" || u.centralPeerRegistry == nil {
		return
	}
	// Attempts are tracked via UpdatePeerMetrics with no success/failure flags.
	if err := u.centralPeerRegistry.UpdatePeerMetrics(ctx, peerID, 0, 0, 0, false, false, false, 0); err != nil {
		u.logger.Warnf("[peer_metrics] Failed to report catchup attempt for peer %s: %v", peerID, err)
	}
}

// reportCatchupSuccess reports a successful catchup to the central registry.
func (u *Server) reportCatchupSuccess(ctx context.Context, peerID string, duration time.Duration) {
	if peerID == "" || u.centralPeerRegistry == nil {
		return
	}
	durationMs := duration.Milliseconds()
	if err := u.centralPeerRegistry.UpdatePeerMetrics(ctx, peerID, 0, 0, 0, true, false, false, durationMs); err != nil {
		u.logger.Warnf("[peer_metrics] Failed to report catchup success for peer %s: %v", peerID, err)
	}
}

// reportCatchupFailure reports a failed catchup to the central registry.
func (u *Server) reportCatchupFailure(ctx context.Context, peerID string) {
	if peerID == "" || u.centralPeerRegistry == nil {
		return
	}
	if err := u.centralPeerRegistry.UpdatePeerMetrics(ctx, peerID, 0, 0, 0, false, true, false, 0); err != nil {
		u.logger.Warnf("[peer_metrics] Failed to report catchup failure for peer %s: %v", peerID, err)
	}
}

// reportCatchupError stores the catchup error message (best-effort logging only).
func (u *Server) reportCatchupError(_ context.Context, peerID string, errorMsg string) {
	if peerID == "" || errorMsg == "" {
		return
	}
	u.logger.Debugf("[peer_metrics] Catchup error for peer %s: %s", peerID, errorMsg)
}

// reportCatchupMalicious reports malicious behavior to the central registry.
func (u *Server) reportCatchupMalicious(ctx context.Context, peerID string, reason string) {
	if peerID == "" {
		return
	}
	u.logger.Warnf("[peer_metrics] Recording malicious attempt from peer %s: %s", peerID, reason)

	if u.centralPeerRegistry != nil {
		if err := u.centralPeerRegistry.UpdatePeerMetrics(ctx, peerID, 0, 0, 0, false, false, true, 0); err != nil {
			u.logger.Warnf("[peer_metrics] Failed to report malicious behavior for peer %s: %v", peerID, err)
		}
		// Also add ban score for malicious behavior
		if _, _, err := u.centralPeerRegistry.AddBanScore(ctx, peerID, "catchup_malicious", 50); err != nil {
			u.logger.Warnf("[peer_metrics] Failed to add ban score for malicious peer %s: %v", peerID, err)
		}
	}
}

// isPeerMalicious checks if a peer is banned in the central registry.
func (u *Server) isPeerMalicious(ctx context.Context, peerID string) bool {
	if peerID == "" || u.centralPeerRegistry == nil {
		return false
	}
	banned, err := u.centralPeerRegistry.IsPeerBanned(ctx, peerID)
	if err != nil {
		u.logger.Warnf("[isPeerMalicious] Failed to check ban status for peer %s: %v", peerID, err)
		return false
	}
	return banned
}

// isPeerBad checks if a peer has bad reputation in the central registry.
func (u *Server) isPeerBad(peerID string) bool {
	if peerID == "" || u.centralPeerRegistry == nil {
		return false
	}
	info, found, err := u.centralPeerRegistry.GetPeer(context.Background(), peerID)
	if err != nil || !found {
		return false
	}
	return info.ReputationScore < 20
}

// reportValidBlockForPeers credits reputation to all peers that contributed to a valid block.
func (u *Server) reportValidBlockForPeers(ctx context.Context, primaryPeerID string, blockHash string, contributingPeers map[string]struct{}) {
	if u.centralPeerRegistry == nil {
		return
	}

	// Credit the primary peer
	if primaryPeerID != "" {
		if err := u.centralPeerRegistry.UpdatePeerMetrics(ctx, primaryPeerID, 0, 0, 0, true, false, false, 0); err != nil {
			u.logger.Warnf("[peer_metrics] Failed to report valid block %s for primary peer %s: %v", blockHash, primaryPeerID, err)
		}
	}

	// Credit each secondary peer that contributed subtree data
	secondaryCount := 0
	for contributingPeerID := range contributingPeers {
		if contributingPeerID == primaryPeerID {
			continue
		}
		if err := u.centralPeerRegistry.UpdatePeerMetrics(ctx, contributingPeerID, 0, 0, 0, true, false, false, 0); err != nil {
			u.logger.Warnf("[peer_metrics] Failed to report valid block %s for contributing peer %s: %v", blockHash, contributingPeerID, err)
		} else {
			secondaryCount++
		}
	}
	if secondaryCount > 0 {
		u.logger.Infof("[peer_metrics] Credited %d contributing peers for valid block %s", secondaryCount, blockHash)
	}
}
