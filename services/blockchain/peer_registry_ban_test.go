package blockchain

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// shortBanConfig returns a BanConfig with very short durations for fast tests.
func shortBanConfig() BanConfig {
	return BanConfig{
		Threshold:     100,
		Duration:      time.Millisecond,
		DecayInterval: time.Millisecond,
		DecayAmount:   1,
		ReasonPoints: map[string]int32{
			"invalid_block": 10,
			"spam":          50,
		},
	}
}

// ---------------------------------------------------------------------------
// AddBanScore
// ---------------------------------------------------------------------------

func TestBanAddBanScore_BasicScoring(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	score, banned := r.AddBanScore("peer-1", "test", 25)
	require.Equal(t, int32(25), score)
	require.False(t, banned)

	score, banned = r.AddBanScore("peer-1", "test", 30)
	require.Equal(t, int32(55), score)
	require.False(t, banned)
}

func TestBanAddBanScore_ThresholdBan(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	// Add points up to just below threshold.
	score, banned := r.AddBanScore("peer-1", "test", 99)
	require.Equal(t, int32(99), score)
	require.False(t, banned)

	// Crossing the threshold should trigger a ban.
	score, banned = r.AddBanScore("peer-1", "test", 1)
	require.Equal(t, int32(100), score)
	require.True(t, banned)

	// Adding more points to an already-banned peer should NOT return banned=true
	// again (the return value signals a NEW ban only).
	score, banned = r.AddBanScore("peer-1", "test", 10)
	require.Equal(t, int32(110), score)
	require.False(t, banned, "banned should be false because peer was already banned")
}

func TestBanAddBanScore_DecayBeforeAdding(t *testing.T) {
	cfg := shortBanConfig()
	cfg.Threshold = 100
	cfg.DecayInterval = time.Millisecond
	cfg.DecayAmount = 5
	r := NewCentralizedPeerRegistry(cfg)

	// Register an initial score.
	score, _ := r.AddBanScore("peer-1", "test", 20)
	require.Equal(t, int32(20), score)

	// Wait long enough for at least one decay step.
	time.Sleep(5 * time.Millisecond)

	// Add more points; the previous score should have decayed.
	score, _ = r.AddBanScore("peer-1", "test", 10)
	require.Less(t, score, int32(30), "score should be less than 30 because decay was applied")
}

func TestBanAddBanScore_PeerInfoSync(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	// Register a peer so the PeerInfo map is populated.
	r.Register(&PeerInfo{ID: "peer-1"})

	r.AddBanScore("peer-1", "test", 40)

	got, ok := r.Get("peer-1")
	require.True(t, ok)
	require.Equal(t, int32(40), got.BanScore)
	require.False(t, got.IsBanned)

	// Push past threshold.
	r.AddBanScore("peer-1", "test", 70)

	got, ok = r.Get("peer-1")
	require.True(t, ok)
	require.Equal(t, int32(110), got.BanScore)
	require.True(t, got.IsBanned)
}

func TestBanAddBanScore_ConfigReasonLookup(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	// "invalid_block" is mapped to 10 points in DefaultBanConfig.
	// Even though we pass 9999, the config value should be used.
	score, _ := r.AddBanScore("peer-1", "invalid_block", 9999)
	require.Equal(t, int32(10), score)

	// Unknown reason should use the provided points.
	r2 := NewCentralizedPeerRegistry(DefaultBanConfig())
	score, _ = r2.AddBanScore("peer-2", "unknown_reason", 42)
	require.Equal(t, int32(42), score)
}

func TestBanAddBanScore_ReasonHistoryCappedAt20(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	// 25 separate reason additions; only the last 20 should remain.
	for i := 0; i < 25; i++ {
		r.AddBanScore("peer-1", "test", 1)
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	require.Len(t, r.banScores["peer-1"].Reasons, 20)
}

// ---------------------------------------------------------------------------
// IsBannedPeer
// ---------------------------------------------------------------------------

func TestBanIsBannedPeer_NotBanned(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	require.False(t, r.IsBannedPeer("nonexistent-peer"))
}

func TestBanIsBannedPeer_Banned(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	r.AddBanScore("peer-1", "test", 100)
	require.True(t, r.IsBannedPeer("peer-1"))
}

func TestBanIsBannedPeer_AutoUnban(t *testing.T) {
	cfg := shortBanConfig()
	cfg.Duration = time.Millisecond
	r := NewCentralizedPeerRegistry(cfg)

	// Register so PeerInfo sync can be tested too.
	r.Register(&PeerInfo{ID: "peer-1"})

	r.AddBanScore("peer-1", "test", 100)
	require.True(t, r.IsBannedPeer("peer-1"))

	// Wait for the ban to expire.
	time.Sleep(5 * time.Millisecond)

	require.False(t, r.IsBannedPeer("peer-1"), "ban should have expired")

	// PeerInfo should be updated by the auto-unban.
	got, ok := r.Get("peer-1")
	require.True(t, ok)
	require.False(t, got.IsBanned)
	require.Equal(t, int32(0), got.BanScore)
}

// ---------------------------------------------------------------------------
// ListBannedPeers
// ---------------------------------------------------------------------------

func TestBanListBannedPeers_Empty(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	banned := r.ListBannedPeers()
	require.Empty(t, banned)
}

func TestBanListBannedPeers_ReturnsBannedOnly(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	// Ban two peers, leave one with a score below threshold.
	r.AddBanScore("bad-1", "test", 100)
	r.AddBanScore("bad-2", "test", 150)
	r.AddBanScore("good-1", "test", 10)

	banned := r.ListBannedPeers()
	require.Len(t, banned, 2)
	require.ElementsMatch(t, []string{"bad-1", "bad-2"}, banned)
}

// ---------------------------------------------------------------------------
// ClearBannedPeers
// ---------------------------------------------------------------------------

func TestBanClearBannedPeers(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	r.Register(&PeerInfo{ID: "peer-1"})
	r.Register(&PeerInfo{ID: "peer-2"})

	r.AddBanScore("peer-1", "test", 100)
	r.AddBanScore("peer-2", "test", 100)

	require.Len(t, r.ListBannedPeers(), 2)

	r.ClearBannedPeers()

	require.Empty(t, r.ListBannedPeers())

	// Verify PeerInfo ban status is also reset.
	p1, ok := r.Get("peer-1")
	require.True(t, ok)
	require.False(t, p1.IsBanned)
	require.Equal(t, int32(0), p1.BanScore)

	p2, ok := r.Get("peer-2")
	require.True(t, ok)
	require.False(t, p2.IsBanned)
	require.Equal(t, int32(0), p2.BanScore)
}

// ---------------------------------------------------------------------------
// ReconsiderBadPeers
// ---------------------------------------------------------------------------

func TestBanReconsiderBadPeers_ResetsOldFailures(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	r.Register(&PeerInfo{ID: "old-bad"})
	// Drive reputation below the 20 threshold by recording a malicious event.
	r.UpdateMetrics("old-bad", 0, 0, 0, false, false, true, 0)

	got, _ := r.Get("old-bad")
	require.Less(t, got.ReputationScore, 20.0)

	// Backdate the last failure so it falls before the cooldown.
	r.mu.Lock()
	r.peers["old-bad"].LastInteractionFailure = time.Now().Add(-2 * time.Hour)
	r.mu.Unlock()

	count := r.ReconsiderBadPeers(1 * time.Hour)
	require.Equal(t, 1, count)

	got, _ = r.Get("old-bad")
	// Recovery sets reputation to 30 (below neutral 50, above threshold 20),
	// clears MaliciousCount, and stamps LastReputationReset.
	require.Equal(t, 30.0, got.ReputationScore)
	require.Equal(t, int64(0), got.MaliciousCount)
	require.False(t, got.LastReputationReset.IsZero())
	require.Equal(t, int32(1), got.ReputationResetCount)
}

func TestBanReconsiderBadPeers_ExponentialCooldownBlocksRepeatedReset(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	r.Register(&PeerInfo{ID: "repeat-bad"})
	r.UpdateMetrics("repeat-bad", 0, 0, 0, false, false, true, 0)

	r.mu.Lock()
	r.peers["repeat-bad"].LastInteractionFailure = time.Now().Add(-2 * time.Hour)
	r.mu.Unlock()

	// First reset succeeds.
	require.Equal(t, 1, r.ReconsiderBadPeers(1*time.Hour))

	// Drive reputation back below 20 and backdate the failure again.
	r.mu.Lock()
	r.peers["repeat-bad"].ReputationScore = 5
	r.peers["repeat-bad"].LastInteractionFailure = time.Now().Add(-2 * time.Hour)
	r.mu.Unlock()

	// Second attempt within the same cooldown window is blocked because
	// LastReputationReset is recent and the required cooldown is now 3× the base.
	require.Equal(t, 0, r.ReconsiderBadPeers(1*time.Hour))
}

func TestBanReconsiderBadPeers_IgnoresRecentFailures(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	r.Register(&PeerInfo{ID: "recent-bad"})
	r.UpdateMetrics("recent-bad", 0, 0, 0, false, false, true, 0)

	got, _ := r.Get("recent-bad")
	require.Less(t, got.ReputationScore, 30.0)

	// Do NOT backdate — the failure is recent.
	count := r.ReconsiderBadPeers(1 * time.Hour)
	require.Equal(t, 0, count)

	got, _ = r.Get("recent-bad")
	require.Less(t, got.ReputationScore, 30.0, "reputation should not have been reset")
}

func TestBanReconsiderBadPeers_ReturnsCount(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	// Create multiple bad peers with old failures.
	for _, id := range []string{"a", "b", "c"} {
		r.Register(&PeerInfo{ID: id})
		r.UpdateMetrics(id, 0, 0, 0, false, false, true, 0)
		r.mu.Lock()
		r.peers[id].LastInteractionFailure = time.Now().Add(-2 * time.Hour)
		r.mu.Unlock()
	}

	// One peer with a recent failure should not be reconsidered.
	r.Register(&PeerInfo{ID: "recent"})
	r.UpdateMetrics("recent", 0, 0, 0, false, false, true, 0)

	count := r.ReconsiderBadPeers(1 * time.Hour)
	require.Equal(t, 3, count)
}

// ---------------------------------------------------------------------------
// decayBanScores
// ---------------------------------------------------------------------------

func TestBanDecayBanScores_ScoresDecay(t *testing.T) {
	cfg := shortBanConfig()
	cfg.DecayInterval = time.Millisecond
	cfg.DecayAmount = 10
	r := NewCentralizedPeerRegistry(cfg)

	// Register a peer and give it a score.
	r.Register(&PeerInfo{ID: "peer-1"})
	r.AddBanScore("peer-1", "test", 50)

	// Wait for several decay intervals.
	time.Sleep(5 * time.Millisecond)

	r.decayBanScores()

	got, ok := r.Get("peer-1")
	require.True(t, ok)
	require.Less(t, got.BanScore, int32(50), "score should have decayed")
}

func TestBanDecayBanScores_ZeroScoreCleanup(t *testing.T) {
	cfg := shortBanConfig()
	cfg.DecayInterval = time.Millisecond
	cfg.DecayAmount = 100 // aggressive decay so it hits zero quickly
	r := NewCentralizedPeerRegistry(cfg)

	r.AddBanScore("peer-1", "test", 5)

	// Wait for decay to exceed the score.
	time.Sleep(5 * time.Millisecond)

	r.decayBanScores()

	// The entry should be cleaned up because score is 0 and peer is not banned.
	r.mu.RLock()
	_, exists := r.banScores["peer-1"]
	r.mu.RUnlock()
	require.False(t, exists, "zero-score unbanned entry should be removed")
}

func TestBanDecayBanScores_BannedEntryNotCleaned(t *testing.T) {
	cfg := shortBanConfig()
	cfg.DecayInterval = time.Millisecond
	cfg.DecayAmount = 100
	cfg.Duration = time.Hour // long ban so it doesn't expire during the test
	r := NewCentralizedPeerRegistry(cfg)

	// Ban the peer.
	r.AddBanScore("peer-1", "test", 100)
	require.True(t, r.IsBannedPeer("peer-1"))

	time.Sleep(5 * time.Millisecond)
	r.decayBanScores()

	// Even though score decayed to 0, the entry should remain because peer is still banned.
	r.mu.RLock()
	entry, exists := r.banScores["peer-1"]
	r.mu.RUnlock()
	require.True(t, exists, "banned entry should not be cleaned up")
	require.True(t, entry.Banned)
}

func TestBanAddBanScore_ExpiresStaleBanBeforeScoring(t *testing.T) {
	cfg := DefaultBanConfig()
	cfg.Duration = 5 * time.Millisecond
	r := NewCentralizedPeerRegistry(cfg)

	r.Register(&PeerInfo{ID: "p"})
	// Two strikes ban (50 + 50 = 100 = threshold).
	r.AddBanScore("p", "spam", 0)
	r.AddBanScore("p", "spam", 0)
	require.True(t, r.IsBannedPeer("p"))

	// Walk past the ban window.
	time.Sleep(20 * time.Millisecond)

	// Without an intervening IsBannedPeer call, the entry still has Banned=true
	// and Score>=Threshold. Adding a fresh score with the stale-expiry guard
	// must reset Banned/Score first, then re-arm the ban after the new strike.
	score, banned := r.AddBanScore("p", "spam", 0)
	require.Equal(t, int32(50), score, "stale ban must be expired before counting the new strike")
	require.False(t, banned, "single fresh strike below threshold must not re-ban")

	got, _ := r.Get("p")
	require.False(t, got.IsBanned)
	require.Equal(t, int32(50), got.BanScore)
}

func TestBanClose_StopsDecayGoroutineWithoutContextCancel(t *testing.T) {
	cfg := shortBanConfig()
	cfg.DecayInterval = time.Millisecond
	cfg.DecayAmount = 50
	r := NewCentralizedPeerRegistry(cfg)

	r.AddBanScore("p", "test", 80)
	r.StartBanDecay(context.Background())

	// Close must drive the goroutine to exit even though the supplied ctx is
	// never cancelled (this is the shutdown-race fix from the review feedback).
	doneCh := make(chan struct{})
	go func() {
		r.Close()
		close(doneCh)
	}()

	select {
	case <-doneCh:
	case <-time.After(time.Second):
		t.Fatal("Close did not return within 1s — goroutine likely leaked")
	}
}

func TestBanClose_IsIdempotent(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())
	r.StartBanDecay(context.Background())
	r.Close()
	require.NotPanics(t, func() { r.Close() }, "second Close must not panic")
}

func TestBanStartBanDecay_ContextCancellation(t *testing.T) {
	cfg := shortBanConfig()
	cfg.DecayInterval = time.Millisecond
	cfg.DecayAmount = 50
	r := NewCentralizedPeerRegistry(cfg)

	r.Register(&PeerInfo{ID: "peer-1"})
	r.AddBanScore("peer-1", "test", 80)

	ctx, cancel := context.WithCancel(context.Background())
	r.StartBanDecay(ctx)

	// Let the goroutine run a few ticks.
	time.Sleep(10 * time.Millisecond)

	// Score should have decayed by now.
	got, ok := r.Get("peer-1")
	require.True(t, ok)
	require.Less(t, got.BanScore, int32(80))

	// Cancel and verify the goroutine doesn't keep decaying.
	cancel()
	time.Sleep(5 * time.Millisecond)

	scoreAfterCancel, _ := r.Get("peer-1")
	time.Sleep(10 * time.Millisecond)
	scoreAfterWait, _ := r.Get("peer-1")
	require.Equal(t, scoreAfterCancel.BanScore, scoreAfterWait.BanScore,
		"score should stop decaying after context cancellation")
}
