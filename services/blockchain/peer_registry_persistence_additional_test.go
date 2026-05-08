package blockchain

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/stretchr/testify/require"
)

// TestCentralizedPeerRegistry_Persistence_AllFields verifies that Save then Load
// preserves every PeerInfo field, not just Height and TransportType.
func TestCentralizedPeerRegistry_Persistence_AllFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peers.json")

	blockHash, err := chainhash.NewHashFromStr("000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f")
	require.NoError(t, err)

	r := NewCentralizedPeerRegistry(DefaultBanConfig())

	// Register a peer with minimal fields, then manually set all the counters
	// to known values so we can verify round-tripping.
	r.Register(&PeerInfo{
		ID:             "full-peer",
		TransportType:  blockchain_api.TransportType_TRANSPORT_WIRE_PROTOCOL,
		ClientName:     "test-node/0.1",
		Height:         12345,
		DataHubURL:     "http://datahub.example.com:8090",
		NetworkAddress: "192.168.1.100:8333",
		Storage:        "aerospike",
		BlockHash:      blockHash,
	})

	// Override internal fields that Register sets or that accumulate over time.
	r.mu.Lock()
	p := r.peers["full-peer"]
	p.BytesSent = 999888
	p.BytesReceived = 777666
	p.InteractionAttempts = 50
	p.InteractionSuccesses = 45
	p.InteractionFailures = 5
	p.MaliciousCount = 1
	p.ReputationScore = 42.5
	p.AvgResponseTimeMs = 250
	p.IsBanned = true
	p.BanScore = 80
	// Set time fields to known values.
	fixedTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	p.ConnectedAt = fixedTime
	p.LastMessageTime = fixedTime.Add(1 * time.Minute)
	p.LastInteractionAttempt = fixedTime.Add(2 * time.Minute)
	p.LastInteractionSuccess = fixedTime.Add(3 * time.Minute)
	p.LastInteractionFailure = fixedTime.Add(4 * time.Minute)
	p.LastSeen = fixedTime.Add(5 * time.Minute)
	r.mu.Unlock()

	require.NoError(t, r.Save(path))

	// Load into a fresh registry with a generous TTL so the peer survives.
	r2 := NewCentralizedPeerRegistry(DefaultBanConfig())
	require.NoError(t, r2.Load(path, 365*24*time.Hour))
	require.Equal(t, 1, r2.Count())

	got, ok := r2.Get("full-peer")
	require.True(t, ok)

	// Core identity
	require.Equal(t, "full-peer", got.ID)
	require.Equal(t, blockchain_api.TransportType_TRANSPORT_WIRE_PROTOCOL, got.TransportType)
	require.Equal(t, "test-node/0.1", got.ClientName)
	require.Equal(t, uint32(12345), got.Height)
	require.Equal(t, "http://datahub.example.com:8090", got.DataHubURL)
	require.Equal(t, "192.168.1.100:8333", got.NetworkAddress)
	require.Equal(t, "aerospike", got.Storage)

	// BlockHash
	require.NotNil(t, got.BlockHash)
	require.Equal(t, blockHash.String(), got.BlockHash.String())

	// Counters
	require.Equal(t, uint64(999888), got.BytesSent)
	require.Equal(t, uint64(777666), got.BytesReceived)
	require.Equal(t, int64(50), got.InteractionAttempts)
	require.Equal(t, int64(45), got.InteractionSuccesses)
	require.Equal(t, int64(5), got.InteractionFailures)
	require.Equal(t, int64(1), got.MaliciousCount)
	require.Equal(t, 42.5, got.ReputationScore)
	require.Equal(t, int64(250), got.AvgResponseTimeMs)

	// Ban fields
	require.True(t, got.IsBanned)
	require.Equal(t, int32(80), got.BanScore)

	// Time fields
	require.True(t, got.ConnectedAt.Equal(fixedTime))
	require.True(t, got.LastMessageTime.Equal(fixedTime.Add(1*time.Minute)))
	require.True(t, got.LastInteractionAttempt.Equal(fixedTime.Add(2*time.Minute)))
	require.True(t, got.LastInteractionSuccess.Equal(fixedTime.Add(3*time.Minute)))
	require.True(t, got.LastInteractionFailure.Equal(fixedTime.Add(4*time.Minute)))
	require.True(t, got.LastSeen.Equal(fixedTime.Add(5*time.Minute)))
}

// TestCentralizedPeerRegistry_Persistence_MultiplePeersRoundTrip verifies that
// saving and loading multiple peers with different transport types preserves all
// entries and their identity.
func TestCentralizedPeerRegistry_Persistence_MultiplePeersRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "multi.json")

	r := NewCentralizedPeerRegistry(DefaultBanConfig())
	r.Register(&PeerInfo{ID: "http-peer", TransportType: blockchain_api.TransportType_TRANSPORT_HTTP, Height: 100, Storage: "s3"})
	r.Register(&PeerInfo{ID: "wire-peer", TransportType: blockchain_api.TransportType_TRANSPORT_WIRE_PROTOCOL, Height: 200, Storage: "aerospike"})
	r.Register(&PeerInfo{ID: "unknown-peer", Height: 50})

	require.NoError(t, r.Save(path))

	r2 := NewCentralizedPeerRegistry(DefaultBanConfig())
	require.NoError(t, r2.Load(path, 24*time.Hour))
	require.Equal(t, 3, r2.Count())

	for _, id := range []string{"http-peer", "wire-peer", "unknown-peer"} {
		_, ok := r2.Get(id)
		require.True(t, ok, "peer %s should exist after round-trip", id)
	}

	got, _ := r2.Get("http-peer")
	require.Equal(t, "s3", got.Storage)

	got, _ = r2.Get("wire-peer")
	require.Equal(t, "aerospike", got.Storage)
}

// TestCentralizedPeerRegistry_Persistence_EmptyRegistrySave verifies that saving
// an empty registry produces a valid file that loads cleanly.
func TestCentralizedPeerRegistry_Persistence_EmptyRegistrySave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.json")

	r := NewCentralizedPeerRegistry(DefaultBanConfig())
	require.NoError(t, r.Save(path))

	r2 := NewCentralizedPeerRegistry(DefaultBanConfig())
	require.NoError(t, r2.Load(path, 24*time.Hour))
	require.Equal(t, 0, r2.Count())
}

// TestCentralizedPeerRegistry_Persistence_LoadReplacesExistingState verifies that
// Load fully replaces the in-memory state, not merges with it.
func TestCentralizedPeerRegistry_Persistence_LoadReplacesExistingState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "replace.json")

	// Save a registry with one peer.
	r := NewCentralizedPeerRegistry(DefaultBanConfig())
	r.Register(&PeerInfo{ID: "persisted-peer", Height: 10})
	require.NoError(t, r.Save(path))

	// Create a new registry with a different peer.
	r2 := NewCentralizedPeerRegistry(DefaultBanConfig())
	r2.Register(&PeerInfo{ID: "in-memory-peer", Height: 20})
	require.Equal(t, 1, r2.Count())

	// Load should replace, not merge.
	require.NoError(t, r2.Load(path, 24*time.Hour))
	require.Equal(t, 1, r2.Count())
	_, ok := r2.Get("persisted-peer")
	require.True(t, ok)
	_, ok = r2.Get("in-memory-peer")
	require.False(t, ok)
}

// TestCentralizedPeerRegistry_Persistence_FilePermissions verifies that the saved
// file has restrictive permissions (0600).
func TestCentralizedPeerRegistry_Persistence_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "perms.json")

	r := NewCentralizedPeerRegistry(DefaultBanConfig())
	r.Register(&PeerInfo{ID: "peer-1"})
	require.NoError(t, r.Save(path))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// TestCentralizedPeerRegistry_Persistence_SaveToReadOnlyDir verifies that Save
// returns an error when writing to a read-only directory.
func TestCentralizedPeerRegistry_Persistence_SaveToReadOnlyDir(t *testing.T) {
	dir := t.TempDir()
	// Make the directory read-only so the temp file write fails.
	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o755) // restore so TempDir cleanup works
	})

	path := filepath.Join(dir, "peers.json")

	r := NewCentralizedPeerRegistry(DefaultBanConfig())
	r.Register(&PeerInfo{ID: "peer-1"})

	err := r.Save(path)
	require.Error(t, err)
	require.ErrorContains(t, err, "write peer registry tmp file")
}

// TestSavePeerRegistry_AtomicRename verifies that the final file contains valid
// JSON after a successful save (i.e. the rename happened correctly).
func TestSavePeerRegistry_AtomicRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "atomic.json")

	peers := []*PeerInfo{
		{ID: "a", Height: 1},
		{ID: "b", Height: 2},
	}
	require.NoError(t, savePeerRegistry(path, peers, nil))

	// The temp file should not exist after a successful save.
	_, err := os.Stat(path + ".tmp")
	require.True(t, os.IsNotExist(err))

	// The final file should contain valid JSON in the persisted-envelope shape.
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var loaded persistedRegistry
	require.NoError(t, json.Unmarshal(data, &loaded))
	require.Len(t, loaded.Peers, 2)
	require.Equal(t, persistedRegistryVersion, loaded.Version)
}

// TestLoadPeerRegistry_AllExpired verifies that if every peer is past the TTL
// cutoff the result is an empty slice (not nil).
func TestLoadPeerRegistry_AllExpired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "expired.json")

	stale := time.Now().Add(-72 * time.Hour)
	envelope := persistedRegistry{
		Version: persistedRegistryVersion,
		Peers: []*PeerInfo{
			{ID: "old-1", LastSeen: stale},
			{ID: "old-2", LastSeen: stale},
		},
	}
	data, err := json.Marshal(&envelope)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	loaded, _, err := loadPeerRegistry(path, 24*time.Hour)
	require.NoError(t, err)
	require.Empty(t, loaded)
}

// TestPersistence_BansSurviveRestart verifies the major review-feedback fix:
// banScores are written and restored across Save/Load, so an in-flight ban
// keeps enforcing after a process restart.
func TestPersistence_BansSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ban-state.json")

	r := NewCentralizedPeerRegistry(DefaultBanConfig())
	r.Register(&PeerInfo{ID: "p"})
	// spam = 50, threshold = 100. Two strikes => banned.
	r.AddBanScore("p", "spam", 0)
	r.AddBanScore("p", "spam", 0)
	require.True(t, r.IsBannedPeer("p"))

	require.NoError(t, r.Save(path))

	r2 := NewCentralizedPeerRegistry(DefaultBanConfig())
	require.NoError(t, r2.Load(path, 24*time.Hour))

	require.True(t, r2.IsBannedPeer("p"), "ban must persist across Save/Load")
	require.Equal(t, []string{"p"}, r2.ListBannedPeers())
}

func TestPersistence_BannedPeersExemptFromTTL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ban-ttl.json")

	r := NewCentralizedPeerRegistry(DefaultBanConfig())
	r.Register(&PeerInfo{ID: "p"})
	r.AddBanScore("p", "spam", 0)
	r.AddBanScore("p", "spam", 0)

	// Backdate LastSeen so it falls outside TTL — without the ban exemption it
	// would be evicted on Load and the ban entry would be orphaned.
	r.mu.Lock()
	r.peers["p"].LastSeen = time.Now().Add(-48 * time.Hour)
	r.mu.Unlock()

	require.NoError(t, r.Save(path))

	r2 := NewCentralizedPeerRegistry(DefaultBanConfig())
	require.NoError(t, r2.Load(path, 24*time.Hour))

	require.True(t, r2.IsBannedPeer("p"))
	_, ok := r2.Get("p")
	require.True(t, ok, "banned peer must not be evicted by TTL on Load")
}

func TestPersistence_ExpiredBanDroppedOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "expired-ban.json")

	envelope := persistedRegistry{
		Version: persistedRegistryVersion,
		BanScores: map[string]persistedBanEntry{
			"already-expired": {
				Score:    150,
				Banned:   true,
				BanUntil: time.Now().Add(-1 * time.Hour),
			},
			"still-banned": {
				Score:    150,
				Banned:   true,
				BanUntil: time.Now().Add(1 * time.Hour),
			},
		},
	}
	data, err := json.Marshal(&envelope)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	r := NewCentralizedPeerRegistry(DefaultBanConfig())
	require.NoError(t, r.Load(path, 24*time.Hour))

	require.False(t, r.IsBannedPeer("already-expired"))
	require.True(t, r.IsBannedPeer("still-banned"))
}
