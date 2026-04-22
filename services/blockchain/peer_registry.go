package blockchain

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
)

// PeerInfo holds transport-agnostic information about a peer known to the node.
// It is used across all transport types (HTTP DataHub, wire protocol, etc.).
type PeerInfo struct {
	ID                     string
	TransportType          blockchain_api.TransportType
	TransportTypeSet       bool // When true, TransportType was explicitly set by the caller
	ClientName             string
	Height                 uint32
	DataHubURL             string
	NetworkAddress         string
	IsBanned               bool
	BanScore               int32
	Storage                string
	BytesSent              uint64
	BytesReceived          uint64
	InteractionAttempts    int64
	InteractionSuccesses   int64
	InteractionFailures    int64
	MaliciousCount         int64
	ReputationScore        float64
	AvgResponseTimeMs      int64
	ConnectedAt            time.Time
	LastMessageTime        time.Time
	LastInteractionAttempt time.Time
	LastInteractionSuccess time.Time
	LastInteractionFailure time.Time
	LastSeen               time.Time
	BlockHash              *chainhash.Hash
}

// banEntry tracks ban scoring state for a single peer.
type banEntry struct {
	Score     int32
	Banned    bool
	BanUntil  time.Time
	LastDecay time.Time
	Reasons   []string
}

// BanConfig holds ban management configuration.
type BanConfig struct {
	Threshold     int32         // Score threshold that triggers a ban (default: 100)
	Duration      time.Duration // How long a ban lasts (default: 24h)
	DecayInterval time.Duration // How often scores decay (default: 1min)
	DecayAmount   int32         // Points removed per decay interval (default: 1)
	// Reason -> points mapping
	ReasonPoints map[string]int32
}

// DefaultBanConfig returns sensible defaults matching the existing P2P BanManager.
func DefaultBanConfig() BanConfig {
	return BanConfig{
		Threshold:     100,
		Duration:      24 * time.Hour,
		DecayInterval: time.Minute,
		DecayAmount:   1,
		ReasonPoints: map[string]int32{
			"invalid_subtree":    10,
			"protocol_violation": 20,
			"spam":               50,
			"invalid_block":      10,
			"catchup_failure":    30,
		},
	}
}

// CentralizedPeerRegistry is a thread-safe, in-memory store of peer information
// shared across all transport types in the blockchain service.
type CentralizedPeerRegistry struct {
	mu        sync.RWMutex
	peers     map[string]*PeerInfo
	banScores map[string]*banEntry
	banConfig BanConfig
}

// NewCentralizedPeerRegistry creates an empty peer registry with the given ban configuration.
func NewCentralizedPeerRegistry(banCfg BanConfig) *CentralizedPeerRegistry {
	return &CentralizedPeerRegistry{
		peers:     make(map[string]*PeerInfo),
		banScores: make(map[string]*banEntry),
		banConfig: banCfg,
	}
}

// Register adds a new peer or updates non-zero fields of an existing peer.
// For new peers, ConnectedAt and LastSeen are initialised and reputation starts
// at the neutral value of 50.
func (r *CentralizedPeerRegistry) Register(info *PeerInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()

	existing, exists := r.peers[info.ID]
	if !exists {
		entry := *info
		if info.BlockHash != nil {
			hashCopy := *info.BlockHash
			entry.BlockHash = &hashCopy
		}
		entry.ConnectedAt = now
		entry.LastSeen = now
		entry.LastMessageTime = now
		entry.ReputationScore = 50.0
		r.peers[info.ID] = &entry
		return
	}

	// Update only fields that carry meaningful new data.
	if info.ClientName != "" {
		existing.ClientName = info.ClientName
	}
	if info.Height > 0 {
		existing.Height = info.Height
	}
	if info.DataHubURL != "" {
		existing.DataHubURL = info.DataHubURL
	}
	if info.NetworkAddress != "" {
		existing.NetworkAddress = info.NetworkAddress
	}
	if info.Storage != "" {
		existing.Storage = info.Storage
	}
	if info.BlockHash != nil {
		hashCopy := *info.BlockHash
		existing.BlockHash = &hashCopy
	}
	// Only update TransportType when the caller explicitly set it.
	if info.TransportTypeSet {
		existing.TransportType = info.TransportType
	}
	existing.LastSeen = now
	existing.LastMessageTime = now
}

// UpdateMetrics atomically applies delta network counters and interaction outcome
// flags for a peer, then recalculates its reputation score.
func (r *CentralizedPeerRegistry) UpdateMetrics(
	peerID string,
	height uint32,
	bytesSentDelta, bytesRecvDelta uint64,
	recordSuccess, recordFailure, recordMalicious bool,
	responseTimeMs int64,
) {
	r.mu.Lock()
	defer r.mu.Unlock()

	info, exists := r.peers[peerID]
	if !exists {
		return
	}

	now := time.Now()

	if height > 0 {
		info.Height = height
	}

	info.BytesSent += bytesSentDelta
	info.BytesReceived += bytesRecvDelta
	info.LastSeen = now

	if recordMalicious {
		info.MaliciousCount++
		info.InteractionAttempts++
		info.InteractionFailures++
		info.LastInteractionAttempt = now
		info.LastInteractionFailure = now
	} else if recordSuccess {
		info.InteractionAttempts++
		info.InteractionSuccesses++
		info.LastInteractionAttempt = now
		info.LastInteractionSuccess = now

		// Running weighted average — weight recent observations at 20% to smooth spikes.
		if info.AvgResponseTimeMs == 0 {
			info.AvgResponseTimeMs = responseTimeMs
		} else {
			info.AvgResponseTimeMs = int64(float64(info.AvgResponseTimeMs)*0.8 + float64(responseTimeMs)*0.2)
		}
	} else if recordFailure {
		info.InteractionAttempts++
		info.InteractionFailures++
		info.LastInteractionAttempt = now
		info.LastInteractionFailure = now
	}

	r.calculateAndUpdateReputation(info)
}

// Remove deletes a peer and its ban score entry from the registry.
func (r *CentralizedPeerRegistry) Remove(peerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.peers, peerID)
	delete(r.banScores, peerID)
}

// Get returns a copy of the peer info for peerID, or false if not found.
// Returning a copy prevents callers from modifying registry state without locking.
func (r *CentralizedPeerRegistry) Get(peerID string) (*PeerInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	info, exists := r.peers[peerID]
	if !exists {
		return nil, false
	}

	peerCopy := *info
	if info.BlockHash != nil {
		hashCopy := *info.BlockHash
		peerCopy.BlockHash = &hashCopy
	}
	return &peerCopy, true
}

// List returns copies of peers that pass all active filters, sorted by reputation
// descending. Passing nil for transportFilter disables that filter.
func (r *CentralizedPeerRegistry) List(
	transportFilter *blockchain_api.TransportType,
	minReputation float64,
	minHeight uint32,
	excludeBanned bool,
) []*PeerInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*PeerInfo, 0, len(r.peers))

	for _, info := range r.peers {
		if excludeBanned && r.isPeerBannedLocked(info.ID) {
			continue
		}
		if transportFilter != nil && info.TransportType != *transportFilter {
			continue
		}
		if info.ReputationScore < minReputation {
			continue
		}
		if info.Height < minHeight {
			continue
		}

		peerCopy := *info
		if info.BlockHash != nil {
			hashCopy := *info.BlockHash
			peerCopy.BlockHash = &hashCopy
		}
		result = append(result, &peerCopy)
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].ReputationScore != result[j].ReputationScore {
			return result[i].ReputationScore > result[j].ReputationScore
		}
		// Stable secondary ordering by most recent successful interaction.
		return result[i].LastInteractionSuccess.After(result[j].LastInteractionSuccess)
	})

	return result
}

// Count returns the number of peers currently in the registry.
func (r *CentralizedPeerRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return len(r.peers)
}

// isPeerBannedLocked checks if a peer is currently banned, considering expiry.
// Must be called with r.mu held (read or write lock).
func (r *CentralizedPeerRegistry) isPeerBannedLocked(peerID string) bool {
	entry, ok := r.banScores[peerID]
	if !ok || !entry.Banned {
		return false
	}
	return time.Now().Before(entry.BanUntil)
}

// calculateAndUpdateReputation recalculates and stores the reputation score for
// info. Must be called with the write lock held.
//
// Algorithm mirrors the P2P registry's scoring so that peer quality signals are
// comparable regardless of which transport discovered the peer.
func (r *CentralizedPeerRegistry) calculateAndUpdateReputation(info *PeerInfo) {
	const (
		baseScore     = 50.0
		successWeight = 0.6
		recencyBonus  = 10.0
		recencyWindow = 1 * time.Hour
	)

	// Malicious peers are pinned at a near-zero score; no other factor redeems them.
	if info.MaliciousCount > 0 {
		info.ReputationScore = 5.0
		return
	}

	totalAttempts := info.InteractionSuccesses + info.InteractionFailures

	if totalAttempts == 0 {
		info.ReputationScore = baseScore
		return
	}

	successRate := (float64(info.InteractionSuccesses) / float64(totalAttempts)) * 100.0

	score := successRate*successWeight + baseScore*(1.0-successWeight)

	if !info.LastInteractionFailure.IsZero() && time.Since(info.LastInteractionFailure) < recencyWindow {
		score -= 15.0
	}

	if !info.LastInteractionSuccess.IsZero() && time.Since(info.LastInteractionSuccess) < recencyWindow {
		score += recencyBonus
	}

	score *= calculateSpeedFactorMs(info.AvgResponseTimeMs)

	if score < 0 {
		score = 0
	} else if score > 100 {
		score = 100
	}

	info.ReputationScore = score
}

// calculateSpeedFactorMs returns a reputation multiplier based on average response
// time in milliseconds. Fast peers are rewarded; slow peers are penalised.
func calculateSpeedFactorMs(avgMs int64) float64 {
	if avgMs == 0 {
		return 1.0
	}

	switch {
	case avgMs < 200:
		return 1.2
	case avgMs < 500:
		return 1.1
	case avgMs < 2000:
		return 1.0
	case avgMs < 5000:
		return 0.9
	case avgMs < 10000:
		return 0.8
	case avgMs < 30000:
		return 0.7
	default:
		return 0.6
	}
}

// ---------------------------------------------------------------------------
// Ban Management
// ---------------------------------------------------------------------------

// AddBanScore adds penalty points to a peer's ban score, applying decay first.
// Returns the updated score and whether the peer is now banned.
func (r *CentralizedPeerRegistry) AddBanScore(peerID string, reason string, points int32) (int32, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	entry, ok := r.banScores[peerID]
	if !ok {
		entry = &banEntry{LastDecay: now}
		r.banScores[peerID] = entry
	}

	// Apply decay (guard against zero interval to avoid division by zero)
	if r.banConfig.DecayInterval > 0 {
		elapsed := now.Sub(entry.LastDecay)
		decaySteps := int32(elapsed / r.banConfig.DecayInterval)
		if decaySteps > 0 {
			entry.Score -= decaySteps * r.banConfig.DecayAmount
			if entry.Score < 0 {
				entry.Score = 0
			}
			entry.LastDecay = now
		}
	}

	// Add reason to history (cap at 50 entries to prevent unbounded growth)
	const maxReasonHistory = 50
	entry.Reasons = append(entry.Reasons, reason)
	if len(entry.Reasons) > maxReasonHistory {
		entry.Reasons = entry.Reasons[len(entry.Reasons)-maxReasonHistory:]
	}

	// Look up points for this reason, default to provided points
	if configPoints, found := r.banConfig.ReasonPoints[reason]; found {
		points = configPoints
	}
	entry.Score += points

	// Check threshold
	wasBanned := entry.Banned
	if entry.Score >= r.banConfig.Threshold && !entry.Banned {
		entry.Banned = true
		entry.BanUntil = now.Add(r.banConfig.Duration)
	}

	// Sync ban status to peer info
	if peer, exists := r.peers[peerID]; exists {
		peer.BanScore = entry.Score
		peer.IsBanned = entry.Banned
	}

	return entry.Score, entry.Banned && !wasBanned // return true only on NEW ban
}

// IsBannedPeer checks if a peer is currently banned, auto-unbanning if expired.
func (r *CentralizedPeerRegistry) IsBannedPeer(peerID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.banScores[peerID]
	if !ok || !entry.Banned {
		return false
	}

	if time.Now().After(entry.BanUntil) {
		// Ban expired
		entry.Banned = false
		entry.Score = 0
		entry.Reasons = nil
		// Sync to peer info
		if peer, exists := r.peers[peerID]; exists {
			peer.BanScore = 0
			peer.IsBanned = false
		}
		return false
	}

	return true
}

// ListBannedPeers returns all currently banned peer IDs.
func (r *CentralizedPeerRegistry) ListBannedPeers() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := time.Now()
	var result []string
	for peerID, entry := range r.banScores {
		if entry.Banned && now.Before(entry.BanUntil) {
			result = append(result, peerID)
		}
	}
	return result
}

// ClearBannedPeers removes all ban entries and resets ban status on all peers.
func (r *CentralizedPeerRegistry) ClearBannedPeers() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.banScores = make(map[string]*banEntry)
	for _, peer := range r.peers {
		peer.IsBanned = false
		peer.BanScore = 0
	}
}

// StartBanDecay starts a background goroutine that decays ban scores periodically
// and removes zero-score entries. Call this once during service startup.
func (r *CentralizedPeerRegistry) StartBanDecay(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(r.banConfig.DecayInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.decayBanScores()
			}
		}
	}()
}

func (r *CentralizedPeerRegistry) decayBanScores() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	for peerID, entry := range r.banScores {
		elapsed := now.Sub(entry.LastDecay)
		decaySteps := int32(elapsed / r.banConfig.DecayInterval)
		if decaySteps > 0 {
			entry.Score -= decaySteps * r.banConfig.DecayAmount
			if entry.Score < 0 {
				entry.Score = 0
			}
			entry.LastDecay = now

			// Sync to peer info
			if peer, exists := r.peers[peerID]; exists {
				peer.BanScore = entry.Score
			}
		}

		// Cleanup: remove zero-score unbanned entries
		if entry.Score == 0 && !entry.Banned {
			delete(r.banScores, peerID)
		}
	}
}

// ---------------------------------------------------------------------------
// Additional Peer Methods
// ---------------------------------------------------------------------------

// ReconsiderBadPeers resets reputation for peers whose last failure is older than cooldown.
// Returns the number of peers reconsidered.
func (r *CentralizedPeerRegistry) ReconsiderBadPeers(cooldown time.Duration) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	count := 0
	cutoff := time.Now().Add(-cooldown)
	for _, info := range r.peers {
		if info.ReputationScore < 30 && !info.LastInteractionFailure.IsZero() && info.LastInteractionFailure.Before(cutoff) {
			info.InteractionAttempts = 0
			info.InteractionSuccesses = 0
			info.InteractionFailures = 0
			info.MaliciousCount = 0
			info.ReputationScore = 50.0
			count++
		}
	}
	return count
}
