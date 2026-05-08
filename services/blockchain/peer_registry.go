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

	// P2P-domain fields. Migrated from services/p2p so the blockchain registry is
	// the single source of truth for peer state across libp2p connections.
	IsConnected          bool
	LastBlockTime        time.Time
	BlocksReceived       int64
	SubtreesReceived     int64
	TransactionsReceived int64
	CatchupBlocks        int64
	LastSyncAttempt      time.Time
	SyncAttemptCount     int32
	LastReputationReset  time.Time
	ReputationResetCount int32
	LastCatchupError     string
	LastCatchupErrorTime time.Time
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

	// Background-goroutine lifecycle. stopCh is closed once by Close() to signal
	// the ban-decay loop (and any future loops) to exit; wg waits for those
	// loops to finish so Close() can give callers a synchronous shutdown.
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewCentralizedPeerRegistry creates an empty peer registry with the given ban configuration.
func NewCentralizedPeerRegistry(banCfg BanConfig) *CentralizedPeerRegistry {
	return &CentralizedPeerRegistry{
		peers:     make(map[string]*PeerInfo),
		banScores: make(map[string]*banEntry),
		banConfig: banCfg,
		stopCh:    make(chan struct{}),
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
		// A response time of 0 means not applicable, not a zero-duration sample.
		if responseTimeMs > 0 {
			if info.AvgResponseTimeMs == 0 {
				info.AvgResponseTimeMs = responseTimeMs
			} else {
				info.AvgResponseTimeMs = int64(float64(info.AvgResponseTimeMs)*0.8 + float64(responseTimeMs)*0.2)
			}
		}
	} else if recordFailure {
		info.InteractionAttempts++
		info.InteractionFailures++
		info.LastInteractionAttempt = now
		info.LastInteractionFailure = now
	}

	r.calculateAndUpdateReputation(info)
}

// Remove deletes a peer entry from the registry. The peer's ban-score entry is
// intentionally preserved — bans must outlive peer disconnects, otherwise an
// offending peer could clear its own ban simply by reconnecting. ClearBannedPeers
// is the explicit knob for wiping ban state.
func (r *CentralizedPeerRegistry) Remove(peerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.peers, peerID)
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
// descending. Passing nil for transportFilter disables that filter. When
// sortByStorage is true, "full" peers sort ahead of "pruned" peers ahead of
// unknown — applied as the primary key, with reputation as the secondary key.
//
// When excludeBanned is true, List acquires the write lock so it can run ban
// expiry inline (auto-unban + sync to PeerInfo). Without that pass, a list
// filtered for excludeBanned could include peers whose ban window has ended
// but whose entry.Banned flag had not yet been refreshed by IsBannedPeer.
func (r *CentralizedPeerRegistry) List(
	transportFilter *blockchain_api.TransportType,
	minReputation float64,
	minHeight uint32,
	excludeBanned bool,
	sortByStorage bool,
) []*PeerInfo {
	if excludeBanned {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.expireBansLocked()
	} else {
		r.mu.RLock()
		defer r.mu.RUnlock()
	}

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
		if sortByStorage {
			si, sj := storagePreference(result[i].Storage), storagePreference(result[j].Storage)
			if si != sj {
				return si > sj
			}
		}
		if result[i].ReputationScore != result[j].ReputationScore {
			return result[i].ReputationScore > result[j].ReputationScore
		}
		// Stable secondary ordering by most recent successful interaction.
		return result[i].LastInteractionSuccess.After(result[j].LastInteractionSuccess)
	})

	return result
}

// storagePreference returns a relative ordering for storage modes used by
// catchup peer selection: full > pruned > unknown.
func storagePreference(mode string) int {
	switch mode {
	case "full":
		return 3
	case "pruned":
		return 2
	default:
		return 1
	}
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

// expireBansLocked sweeps banScores for entries whose BanUntil has passed,
// clears their Banned flag and score, and syncs the result onto PeerInfo so
// downstream readers see consistent state. Must be called with the write lock.
func (r *CentralizedPeerRegistry) expireBansLocked() {
	now := time.Now()
	for peerID, entry := range r.banScores {
		if !entry.Banned || now.Before(entry.BanUntil) {
			continue
		}
		entry.Banned = false
		entry.Score = 0
		entry.Reasons = nil
		if peer, exists := r.peers[peerID]; exists {
			peer.IsBanned = false
			peer.BanScore = 0
		}
	}
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

	// Cap reason history to a small window — operators only need recent context
	// for diagnosing a ban; the full history isn't useful and would scale to
	// noticeable memory at fleet scale (peers × maxReasonHistory × ~20 bytes).
	const maxReasonHistory = 20
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

// Close signals every background goroutine started by the registry (currently
// just the ban-decay loop in StartBanDecay) to exit and waits for them. Safe
// to call multiple times. Pairs with the service's Stop() so a clean shutdown
// blocks until decay traffic has actually drained, instead of relying on the
// service-manager's context cancellation racing with Stop's return.
func (r *CentralizedPeerRegistry) Close() {
	r.stopOnce.Do(func() { close(r.stopCh) })
	r.wg.Wait()
}

// StartBanDecay starts a background goroutine that decays ban scores periodically
// and removes zero-score entries. Call this once during service startup. The
// goroutine exits on the first of: ctx cancellation OR the registry's Close().
func (r *CentralizedPeerRegistry) StartBanDecay(ctx context.Context) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ticker := time.NewTicker(r.banConfig.DecayInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-r.stopCh:
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

// ReconsiderBadPeers resets reputation for peers that have been bad for a while.
// Returns the number of peers that had their reputation recovered. Mirrors the
// semantics of the original P2P registry: only peers with reputation < 20 are
// candidates, the configured cooldown applies after the most recent failure,
// and previously-reset peers must wait an exponentially longer cooldown
// (3× per prior reset) before being eligible again.
func (r *CentralizedPeerRegistry) ReconsiderBadPeers(cooldown time.Duration) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	peersRecovered := 0
	now := time.Now()

	for _, info := range r.peers {
		if info.ReputationScore >= 20 {
			continue
		}
		if info.LastInteractionFailure.IsZero() || now.Sub(info.LastInteractionFailure) < cooldown {
			continue
		}

		if !info.LastReputationReset.IsZero() {
			requiredCooldown := cooldown
			for i := 0; i < int(info.ReputationResetCount); i++ {
				requiredCooldown *= 3
			}
			if now.Sub(info.LastReputationReset) < requiredCooldown {
				continue
			}
		}

		info.ReputationScore = 30
		info.MaliciousCount = 0
		info.LastReputationReset = now
		info.ReputationResetCount++

		peersRecovered++
	}

	return peersRecovered
}

// ResetReputation resets reputation metrics for a specific peer or all peers.
// When peerID is empty, every peer is reset. Returns the number of peers reset.
func (r *CentralizedPeerRegistry) ResetReputation(peerID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	resetOne := func(info *PeerInfo) {
		info.InteractionAttempts = 0
		info.InteractionSuccesses = 0
		info.InteractionFailures = 0
		info.LastInteractionAttempt = time.Time{}
		info.LastInteractionSuccess = time.Time{}
		info.LastInteractionFailure = time.Time{}

		info.ReputationScore = 50.0
		info.MaliciousCount = 0
		info.AvgResponseTimeMs = 0

		info.LastCatchupError = ""
		info.LastCatchupErrorTime = time.Time{}

		info.LastReputationReset = time.Time{}
		info.ReputationResetCount = 0

		info.LastSyncAttempt = time.Time{}
		info.SyncAttemptCount = 0
	}

	if peerID == "" {
		for _, info := range r.peers {
			resetOne(info)
		}
		return len(r.peers)
	}

	info, ok := r.peers[peerID]
	if !ok {
		return 0
	}
	resetOne(info)
	return 1
}

// UpdateConnectionState flips the IsConnected flag for an existing peer entry.
// No-op when the peer is unknown.
func (r *CentralizedPeerRegistry) UpdateConnectionState(peerID string, connected bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if info, ok := r.peers[peerID]; ok {
		info.IsConnected = connected
	}
}

// UpdateLastMessageTime sets the peer's last-message timestamp to now.
func (r *CentralizedPeerRegistry) UpdateLastMessageTime(peerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if info, ok := r.peers[peerID]; ok {
		info.LastMessageTime = time.Now()
	}
}

// UpdateStorage records the peer's storage mode (e.g. "full", "pruned").
func (r *CentralizedPeerRegistry) UpdateStorage(peerID string, storage string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if info, ok := r.peers[peerID]; ok {
		info.Storage = storage
	}
}

// RecordSyncAttempt notes that we attempted to sync with the peer for backoff tracking.
func (r *CentralizedPeerRegistry) RecordSyncAttempt(peerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if info, ok := r.peers[peerID]; ok {
		info.LastSyncAttempt = time.Now()
		info.SyncAttemptCount++
	}
}

// ClearAllSyncAttempts resets sync-attempt tracking on every peer that has any
// and returns the number cleared. Used when every peer has been tried and we
// want to retry from scratch.
func (r *CentralizedPeerRegistry) ClearAllSyncAttempts() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	cleared := 0
	for _, info := range r.peers {
		if !info.LastSyncAttempt.IsZero() {
			info.LastSyncAttempt = time.Time{}
			cleared++
		}
	}
	return cleared
}

// recordReceivedSuccessLocked is the shared body of RecordBlockReceived /
// RecordSubtreeReceived: increment the supplied counter, mark a successful
// interaction, blend the response time into the rolling average, recompute
// reputation. Caller must hold the write lock.
func (r *CentralizedPeerRegistry) recordReceivedSuccessLocked(info *PeerInfo, counter *int64, responseTimeMs int64) {
	*counter++
	info.InteractionSuccesses++
	info.LastInteractionSuccess = time.Now()
	info.LastSeen = info.LastInteractionSuccess

	if responseTimeMs > 0 {
		if info.AvgResponseTimeMs == 0 {
			info.AvgResponseTimeMs = responseTimeMs
		} else {
			info.AvgResponseTimeMs = int64(float64(info.AvgResponseTimeMs)*0.8 + float64(responseTimeMs)*0.2)
		}
	}

	r.calculateAndUpdateReputation(info)
}

// RecordBlockReceived increments BlocksReceived, sets LastBlockTime, blends the
// response time into the running average, and updates reputation.
func (r *CentralizedPeerRegistry) RecordBlockReceived(peerID string, responseTimeMs int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if info, ok := r.peers[peerID]; ok {
		info.LastBlockTime = time.Now()
		r.recordReceivedSuccessLocked(info, &info.BlocksReceived, responseTimeMs)
	}
}

// RecordSubtreeReceived increments SubtreesReceived and records a successful interaction.
func (r *CentralizedPeerRegistry) RecordSubtreeReceived(peerID string, responseTimeMs int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if info, ok := r.peers[peerID]; ok {
		r.recordReceivedSuccessLocked(info, &info.SubtreesReceived, responseTimeMs)
	}
}

// RecordTransactionReceived increments TransactionsReceived and records a successful interaction.
// Transactions are broadcast so we do not track per-tx response time.
func (r *CentralizedPeerRegistry) RecordTransactionReceived(peerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if info, ok := r.peers[peerID]; ok {
		info.TransactionsReceived++
		info.InteractionSuccesses++
		info.LastInteractionSuccess = time.Now()
		info.LastSeen = info.LastInteractionSuccess
		r.calculateAndUpdateReputation(info)
	}
}

// RecordCatchupError stores the most recent catchup error reported against the peer.
func (r *CentralizedPeerRegistry) RecordCatchupError(peerID string, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if info, ok := r.peers[peerID]; ok {
		info.LastCatchupError = errMsg
		info.LastCatchupErrorTime = time.Now()
	}
}

// Cleanup evicts stale peers to bound memory and lookup cost. Phase 1 (TTL)
// drops peers whose LastMessageTime is older than ttl. Phase 2 (LRU) then drops
// oldest-first until the non-exempt portion of the registry fits under maxSize.
// Connected peers and banned peers are exempt from both phases. A maxSize of 0
// disables the LRU phase. Returns (expired, lru) counts.
func (r *CentralizedPeerRegistry) Cleanup(maxSize int, ttl time.Duration) (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	expired := 0

	for id, info := range r.peers {
		if isCleanupExempt(info) {
			continue
		}
		if !info.LastMessageTime.IsZero() && now.Sub(info.LastMessageTime) <= ttl {
			continue
		}
		delete(r.peers, id)
		expired++
	}

	if maxSize <= 0 {
		return expired, 0
	}

	type candidate struct {
		id   string
		last time.Time
	}
	candidates := make([]candidate, 0, len(r.peers))
	exemptCount := 0
	for id, info := range r.peers {
		if isCleanupExempt(info) {
			exemptCount++
			continue
		}
		candidates = append(candidates, candidate{id: id, last: info.LastMessageTime})
	}

	target := maxSize - exemptCount
	if target < 0 {
		target = 0
	}
	toEvict := len(candidates) - target
	if toEvict <= 0 {
		return expired, 0
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].last.Before(candidates[j].last)
	})

	for i := 0; i < toEvict; i++ {
		delete(r.peers, candidates[i].id)
	}

	return expired, toEvict
}

// isCleanupExempt reports whether a peer must be retained regardless of TTL or
// size pressure. Caller must hold the registry lock.
func isCleanupExempt(info *PeerInfo) bool {
	return info.IsConnected || info.IsBanned
}
