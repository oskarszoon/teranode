// Package blockvalidation contains logic for BlockValidation-driven catchup using
// the centralized peer registry when CatchupUseCentralizedOrchestration is enabled.
package blockvalidation

import (
	"context"
	"sort"
	"time"

	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
)

const (
	// centralRegistryPollInterval is how often BlockValidation checks the centralized
	// peer registry for peers with higher height once syncing is underway.
	centralRegistryPollInterval = 30 * time.Second

	// centralRegistryInitialPollInterval is a shorter interval used at startup
	// while waiting for peers to register in the central registry.
	centralRegistryInitialPollInterval = 3 * time.Second

	// centralRegistryInitialPollAttempts is how many fast polls to attempt
	// before falling back to the normal interval.
	centralRegistryInitialPollAttempts = 10
)

// selectBestPeersFromCentralRegistry queries the centralized peer registry and returns
// peers suitable for catchup, sorted by reputation score (highest first).
// Only peers with height >= targetHeight and that are not banned are returned.
func (u *Server) selectBestPeersFromCentralRegistry(ctx context.Context, targetHeight uint32) ([]PeerForCatchup, error) {
	if u.centralPeerRegistry == nil {
		return nil, nil
	}

	peers, err := u.centralPeerRegistry.ListPeers(ctx, nil, 0, targetHeight, true)
	if err != nil {
		return nil, err
	}

	result := make([]PeerForCatchup, 0, len(peers))
	for _, p := range peers {
		// Determine the baseURL based on transport type.
		// For HTTP peers, use the DataHubURL. For wire-protocol peers, use the NetworkAddress.
		baseURL := p.DataHubURL
		if p.TransportType == blockchain_api.TransportType_TRANSPORT_WIRE_PROTOCOL {
			baseURL = p.NetworkAddress
		}

		if baseURL == "" {
			u.logger.Debugf("[central_registry] Skipping peer %s (no baseURL for transport type %v)", p.ID, p.TransportType)
			continue
		}

		result = append(result, PeerForCatchup{
			ID:                     p.ID,
			Storage:                p.Storage,
			DataHubURL:             baseURL,
			Height:                 p.Height,
			BlockHash:              p.BlockHash,
			CatchupReputationScore: p.ReputationScore,
			CatchupAttempts:        p.InteractionAttempts,
			CatchupSuccesses:       p.InteractionSuccesses,
			CatchupFailures:        p.InteractionFailures,
		})
	}

	// Sort: full nodes first, then by reputation descending.
	sort.Slice(result, func(i, j int) bool {
		isFull_i := result[i].Storage == "full"
		isFull_j := result[j].Storage == "full"
		if isFull_i != isFull_j {
			return isFull_i
		}
		return result[i].CatchupReputationScore > result[j].CatchupReputationScore
	})

	return result, nil
}

// selectTransport returns the appropriate CatchupTransport for the given peer.
// Wire-protocol peers use wireTransport; all others use httpTransport.
func (u *Server) selectTransport(peer *blockchain.PeerInfo) CatchupTransport {
	if peer.TransportType == blockchain_api.TransportType_TRANSPORT_WIRE_PROTOCOL && u.wireTransport != nil {
		return u.wireTransport
	}
	return u.httpTransport
}

// reportCatchupMetricsToCentralRegistry updates peer metrics in the centralized
// registry after a catchup attempt (success or failure).
func (u *Server) reportCatchupMetricsToCentralRegistry(ctx context.Context, peerID string, success, malicious bool, responseTimeMs int64) {
	if u.centralPeerRegistry == nil || peerID == "" {
		return
	}
	if err := u.centralPeerRegistry.UpdatePeerMetrics(ctx, peerID, 0, 0, 0, success, !success && !malicious, malicious, responseTimeMs); err != nil {
		u.logger.Warnf("[central_registry] Failed to update peer metrics for %s: %v", peerID, err)
	}
}

// runCentralRegistryPoller periodically polls the centralized peer registry and
// initiates catchup when a peer with height higher than our UTXO tip is found.
// This goroutine runs for the lifetime of the server context.
func (u *Server) runCentralRegistryPoller(ctx context.Context) {
	u.logger.Infof("[central_registry] Centralized orchestration poller started (fast interval %s for %d attempts, then %s)",
		centralRegistryInitialPollInterval, centralRegistryInitialPollAttempts, centralRegistryPollInterval)

	// Use a short initial interval — peers take a few seconds to register after startup.
	ticker := time.NewTicker(centralRegistryInitialPollInterval)
	defer ticker.Stop()

	attempts := 0
	for {
		select {
		case <-ctx.Done():
			u.logger.Infof("[central_registry] Centralized orchestration poller stopping")
			return
		case <-ticker.C:
			triggered := u.pollCentralRegistry(ctx)
			attempts++

			// Switch to normal interval once catchup has been triggered or
			// the initial burst of fast polls is exhausted.
			if triggered || attempts >= centralRegistryInitialPollAttempts {
				ticker.Reset(centralRegistryPollInterval)
			}
		}
	}
}

// pollCentralRegistry performs a single pass: checks if any registered peer has
// a height greater than our current UTXO height and, if so, triggers catchup.
// Returns true if catchup was triggered (successfully or not).
func (u *Server) pollCentralRegistry(ctx context.Context) bool {
	ourHeight := u.utxoStore.GetBlockHeight()

	peers, err := u.selectBestPeersFromCentralRegistry(ctx, ourHeight+1)
	if err != nil {
		u.logger.Warnf("[central_registry] Failed to query centralized registry: %v", err)
		return false
	}

	if len(peers) == 0 {
		u.logger.Debugf("[central_registry] No peers with height > %d in centralized registry", ourHeight)
		return false
	}

	// Don't attempt if catchup is already running.
	if u.isCatchingUp.Load() {
		return true // treat as triggered — catchup is active
	}

	// Find the best peer that isn't on cooldown from a recent failure.
	now := time.Now()
	var best *PeerForCatchup
	for i := range peers {
		cooldownUntil, onCooldown := u.catchupPeerCooldowns[peers[i].ID]
		if onCooldown && now.Before(cooldownUntil) {
			continue
		}
		best = &peers[i]
		break
	}

	if best == nil {
		u.logger.Debugf("[central_registry] All %d peers with height > %d are on cooldown", len(peers), ourHeight)
		return false
	}

	u.logger.Infof("[central_registry] Found peer %s at height %d (our height: %d) — triggering catchup", best.ID, best.Height, ourHeight)

	// Build a synthetic block representing the target we want to catch up to.
	if best.BlockHash == nil {
		u.logger.Warnf("[central_registry] Peer %s has no block hash in registry, skipping", best.ID)
		return false
	}

	targetBlock := model.NewSyntheticBlock(best.Height, best.BlockHash)

	startTime := now
	if err = u.catchup(ctx, targetBlock, best.ID, best.DataHubURL); err != nil {
		responseMs := time.Since(startTime).Milliseconds()
		u.logger.Warnf("[central_registry] Catchup from peer %s failed: %v", best.ID, err)
		u.reportCatchupMetricsToCentralRegistry(ctx, best.ID, false, false, responseMs)

		// Put the peer on cooldown (exponential: 30s, 60s, 120s, max 5min).
		cooldown := u.nextCooldownForPeer(best.ID)
		u.catchupPeerCooldowns[best.ID] = time.Now().Add(cooldown)
		u.logger.Infof("[central_registry] Peer %s on cooldown for %s after failure", best.ID, cooldown)

		return true
	}

	responseMs := time.Since(startTime).Milliseconds()
	u.logger.Infof("[central_registry] Catchup from peer %s succeeded in %dms", best.ID, responseMs)
	u.reportCatchupMetricsToCentralRegistry(ctx, best.ID, true, false, responseMs)

	// Clear cooldowns on success — all peers get a fresh start.
	u.catchupPeerCooldowns = make(map[string]time.Time)
	u.catchupPeerFailCounts = make(map[string]int)

	return true
}

// nextCooldownForPeer returns an exponentially increasing cooldown duration for a peer.
func (u *Server) nextCooldownForPeer(peerID string) time.Duration {
	if u.catchupPeerFailCounts == nil {
		u.catchupPeerFailCounts = make(map[string]int)
	}
	u.catchupPeerFailCounts[peerID]++
	count := u.catchupPeerFailCounts[peerID]

	cooldown := 30 * time.Second
	for i := 1; i < count && cooldown < 5*time.Minute; i++ {
		cooldown *= 2
	}
	if cooldown > 5*time.Minute {
		cooldown = 5 * time.Minute
	}
	return cooldown
}
