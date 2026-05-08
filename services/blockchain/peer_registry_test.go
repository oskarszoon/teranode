package blockchain

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/stretchr/testify/require"
)

func TestCentralizedPeerRegistry_RegisterAndGet(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	info := &PeerInfo{
		ID:              "peer-1",
		TransportType:   blockchain_api.TransportType_TRANSPORT_HTTP,
		ClientName:      "test-client",
		Height:          100,
		DataHubURL:      "http://peer1.example.com",
		ReputationScore: 75.0,
	}

	r.Register(info)

	got, ok := r.Get("peer-1")
	require.True(t, ok)
	require.Equal(t, "peer-1", got.ID)
	require.Equal(t, blockchain_api.TransportType_TRANSPORT_HTTP, got.TransportType)
	require.Equal(t, "test-client", got.ClientName)
	require.Equal(t, uint32(100), got.Height)
	// Reputation is reset to 50.0 for new entries.
	require.Equal(t, 50.0, got.ReputationScore)
	require.False(t, got.ConnectedAt.IsZero())
	require.False(t, got.LastSeen.IsZero())
}

func TestCentralizedPeerRegistry_RegisterUpdatesExisting(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	r.Register(&PeerInfo{ID: "peer-1", Height: 10, DataHubURL: "http://old.example.com"})
	r.Register(&PeerInfo{ID: "peer-1", Height: 20, DataHubURL: "http://new.example.com"})

	got, ok := r.Get("peer-1")
	require.True(t, ok)
	require.Equal(t, uint32(20), got.Height)
	require.Equal(t, "http://new.example.com", got.DataHubURL)
}

func TestCentralizedPeerRegistry_Remove(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	r.Register(&PeerInfo{ID: "peer-1"})
	require.Equal(t, 1, r.Count())

	r.Remove("peer-1")
	require.Equal(t, 0, r.Count())

	_, ok := r.Get("peer-1")
	require.False(t, ok)
}

func TestCentralizedPeerRegistry_ListFilters(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	r.Register(&PeerInfo{ID: "http-peer", TransportType: blockchain_api.TransportType_TRANSPORT_HTTP, Height: 100})
	r.Register(&PeerInfo{ID: "wire-peer", TransportType: blockchain_api.TransportType_TRANSPORT_WIRE_PROTOCOL, Height: 50})
	r.Register(&PeerInfo{ID: "banned-peer", TransportType: blockchain_api.TransportType_TRANSPORT_WIRE_PROTOCOL, Height: 200})
	// Actually ban the peer via AddBanScore so isPeerBannedLocked finds the ban entry.
	r.AddBanScore("banned-peer", "spam", 100)
	r.AddBanScore("banned-peer", "spam", 100)

	// No filters — returns all.
	all := r.List(nil, 0, 0, false)
	require.Len(t, all, 3)

	// Filter by transport.
	httpFilter := blockchain_api.TransportType_TRANSPORT_HTTP
	httpOnly := r.List(&httpFilter, 0, 0, false)
	require.Len(t, httpOnly, 1)
	require.Equal(t, "http-peer", httpOnly[0].ID)

	// Exclude banned.
	noBanned := r.List(nil, 0, 0, true)
	require.Len(t, noBanned, 2)

	// Min height.
	highOnly := r.List(nil, 0, 100, false)
	require.Len(t, highOnly, 2) // http-peer (100) and banned-peer (200)
}

func TestCentralizedPeerRegistry_ListSortedByReputation(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	r.Register(&PeerInfo{ID: "low", Height: 50})
	r.Register(&PeerInfo{ID: "high", Height: 50})

	// Manually set reputation via UpdateMetrics to simulate different scores.
	// Record a success for "high" to push it above 50.
	r.UpdateMetrics("high", 0, 0, 0, true, false, false, 100)

	peers := r.List(nil, 0, 0, false)
	require.Len(t, peers, 2)
	// Higher reputation should come first.
	require.Equal(t, "high", peers[0].ID)
}

func TestCentralizedPeerRegistry_UpdateMetrics(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	r.Register(&PeerInfo{ID: "peer-1"})

	r.UpdateMetrics("peer-1", 200, 1024, 512, true, false, false, 150)

	got, ok := r.Get("peer-1")
	require.True(t, ok)
	require.Equal(t, uint32(200), got.Height)
	require.Equal(t, uint64(1024), got.BytesSent)
	require.Equal(t, uint64(512), got.BytesReceived)
	require.Equal(t, int64(1), got.InteractionSuccesses)
	require.Equal(t, int64(150), got.AvgResponseTimeMs)
}

func TestCentralizedPeerRegistry_UpdateMetrics_IgnoresUntimedSuccessForAverage(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	r.Register(&PeerInfo{ID: "peer-1"})

	r.UpdateMetrics("peer-1", 100, 0, 0, true, false, false, 0)

	got, ok := r.Get("peer-1")
	require.True(t, ok)
	require.Equal(t, int64(1), got.InteractionSuccesses)
	require.Equal(t, int64(0), got.AvgResponseTimeMs)

	r.UpdateMetrics("peer-1", 100, 0, 0, true, false, false, 200)
	r.UpdateMetrics("peer-1", 100, 0, 0, true, false, false, 0)

	got, ok = r.Get("peer-1")
	require.True(t, ok)
	require.Equal(t, int64(3), got.InteractionSuccesses)
	require.Equal(t, int64(200), got.AvgResponseTimeMs)
}

func TestCentralizedPeerRegistry_UpdateMetrics_Malicious(t *testing.T) {
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	r.Register(&PeerInfo{ID: "bad-peer"})
	r.UpdateMetrics("bad-peer", 0, 0, 0, false, false, true, 0)

	got, ok := r.Get("bad-peer")
	require.True(t, ok)
	require.Equal(t, 5.0, got.ReputationScore)
	require.Equal(t, int64(1), got.MaliciousCount)
}

func TestCentralizedPeerRegistry_Persistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peers.json")

	r := NewCentralizedPeerRegistry(DefaultBanConfig())
	r.Register(&PeerInfo{ID: "peer-1", Height: 42, TransportType: blockchain_api.TransportType_TRANSPORT_WIRE_PROTOCOL})
	r.Register(&PeerInfo{ID: "peer-2", Height: 99, TransportType: blockchain_api.TransportType_TRANSPORT_HTTP})

	require.NoError(t, r.Save(path))

	r2 := NewCentralizedPeerRegistry(DefaultBanConfig())
	require.NoError(t, r2.Load(path, 24*time.Hour))

	require.Equal(t, 2, r2.Count())

	p1, ok := r2.Get("peer-1")
	require.True(t, ok)
	require.Equal(t, uint32(42), p1.Height)
	require.Equal(t, blockchain_api.TransportType_TRANSPORT_WIRE_PROTOCOL, p1.TransportType)
}

func TestCentralizedPeerRegistry_Persistence_MissingFile(t *testing.T) {
	dir := t.TempDir()
	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	// Loading from a non-existent path should succeed and leave the registry empty.
	require.NoError(t, r.Load(filepath.Join(dir, "nonexistent.json"), 24*time.Hour))
	require.Equal(t, 0, r.Count())
}

func TestCentralizedPeerRegistry_Persistence_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peers.json")

	require.NoError(t, os.WriteFile(path, []byte("not valid json {{{{"), 0o600))

	r := NewCentralizedPeerRegistry(DefaultBanConfig())
	// Should not return an error — corrupt file is renamed and registry starts empty.
	require.NoError(t, r.Load(path, 24*time.Hour))
	require.Equal(t, 0, r.Count())

	// The corrupt file should have been renamed.
	_, err := os.Stat(path + ".corrupted")
	require.NoError(t, err)
}

func TestCentralizedPeerRegistry_Persistence_TTLCleanup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peers.json")

	r := NewCentralizedPeerRegistry(DefaultBanConfig())
	r.Register(&PeerInfo{ID: "fresh"})
	r.Register(&PeerInfo{ID: "stale"})

	// Backdate the stale peer's LastSeen so it falls outside TTL.
	r.mu.Lock()
	r.peers["stale"].LastSeen = time.Now().Add(-48 * time.Hour)
	r.mu.Unlock()

	require.NoError(t, r.Save(path))

	r2 := NewCentralizedPeerRegistry(DefaultBanConfig())
	require.NoError(t, r2.Load(path, 24*time.Hour))

	require.Equal(t, 1, r2.Count())
	_, ok := r2.Get("fresh")
	require.True(t, ok)
	_, ok = r2.Get("stale")
	require.False(t, ok)
}
