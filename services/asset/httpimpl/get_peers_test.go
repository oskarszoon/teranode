package httpimpl

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/p2p"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
)

// TestGetPeers_NewPeerRegistryClientFails covers the registry-creation error
// branch in getPeersFromCentralRegistry. The mock blockchain client is non-nil
// but the gRPC address is bogus, so blockchain.NewPeerRegistryClient fails and
// the handler should return 503 with an empty peer list.
func TestGetPeers_NewPeerRegistryClientFails(t *testing.T) {
	httpServer, mockRepo, c, rec := GetMockHTTP(t, nil)

	// Non-nil typed client so the nil-check passes; subsequent
	// NewPeerRegistryClient call will fail because settings has no real
	// blockchain gRPC address backing it.
	mockBC := &blockchain.Mock{}
	mockRepo.On("GetBlockchainClient").Return(mockBC)

	err := httpServer.GetPeers(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var resp PeersResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 0, resp.Count)
	require.Empty(t, resp.Peers)
}

// TestPeerInfoResponse_JSON covers the JSON shape the dashboard expects so any
// future field-rename refactor breaks visibly rather than silently.
func TestPeerInfoResponse_JSON(t *testing.T) {
	r := PeerInfoResponse{
		ID:                     "12D3KooWTest",
		ClientName:             "tn",
		Height:                 99,
		BlockHash:              "deadbeef",
		DataHubURL:             "http://example.com",
		BanScore:               5,
		IsBanned:               false,
		IsConnected:            true,
		ConnectedAt:            1700000000,
		BytesReceived:          2048,
		LastBlockTime:          1700000005,
		LastMessageTime:        1700000010,
		CatchupAttempts:        12,
		CatchupSuccesses:       11,
		CatchupFailures:        1,
		CatchupReputationScore: 80.5,
		CatchupMaliciousCount:  0,
		CatchupAvgResponseTime: 150,
		LastCatchupError:       "",
		LastCatchupErrorTime:   0,
	}
	data, err := json.Marshal(r)
	require.NoError(t, err)

	// Spot-check key fields are emitted with the expected JSON tags.
	require.Contains(t, string(data), `"id":"12D3KooWTest"`)
	require.Contains(t, string(data), `"client_name":"tn"`)
	require.Contains(t, string(data), `"data_hub_url":"http://example.com"`)
	require.Contains(t, string(data), `"is_connected":true`)
	require.Contains(t, string(data), `"catchup_avg_response_ms":150`)
}

// TestPeersToResponse_Empty covers the no-peers branch.
func TestPeersToResponse_Empty(t *testing.T) {
	got := peersToResponse(nil)
	require.Equal(t, 0, got.Count)
	require.Empty(t, got.Peers)

	got = peersToResponse([]*p2p.PeerInfo{})
	require.Equal(t, 0, got.Count)
	require.Empty(t, got.Peers)
}

// TestPeersToResponse_FullConversion covers the conversion loop including the
// BlockHash branch and all interaction/catchup metric fields.
func TestPeersToResponse_FullConversion(t *testing.T) {
	pid, err := peer.Decode("12D3KooWNvSZnPi3RrhrTwEY4LuHBeB6K6idsA5DZtEt3yBHk5CQ")
	require.NoError(t, err)

	hash := chainhash.HashH([]byte("block"))
	now := time.Unix(1_700_000_000, 0).UTC()

	peers := []*p2p.PeerInfo{
		{
			ID:                     pid,
			ClientName:             "tn",
			Height:                 99,
			BlockHash:              &hash,
			DataHubURL:             "http://example.com",
			BanScore:               5,
			IsBanned:               false,
			IsConnected:            true,
			ConnectedAt:            now,
			BytesReceived:          2048,
			LastBlockTime:          now.Add(time.Second),
			LastMessageTime:        now.Add(2 * time.Second),
			InteractionAttempts:    10,
			InteractionSuccesses:   8,
			InteractionFailures:    2,
			LastInteractionAttempt: now.Add(3 * time.Second),
			LastInteractionSuccess: now.Add(4 * time.Second),
			LastInteractionFailure: now.Add(5 * time.Second),
			ReputationScore:        75.5,
			MaliciousCount:         1,
			AvgResponseTime:        150 * time.Millisecond,
			LastCatchupError:       "timeout",
			LastCatchupErrorTime:   now.Add(6 * time.Second),
		},
	}

	got := peersToResponse(peers)
	require.Equal(t, 1, got.Count)
	require.Len(t, got.Peers, 1)

	p0 := got.Peers[0]
	require.Equal(t, pid.String(), p0.ID)
	require.Equal(t, "tn", p0.ClientName)
	require.Equal(t, uint32(99), p0.Height)
	require.Equal(t, hash.String(), p0.BlockHash)
	require.Equal(t, "http://example.com", p0.DataHubURL)
	require.Equal(t, 5, p0.BanScore)
	require.False(t, p0.IsBanned)
	require.True(t, p0.IsConnected)
	require.Equal(t, now.Unix(), p0.ConnectedAt)
	require.Equal(t, uint64(2048), p0.BytesReceived)
	require.Equal(t, int64(10), p0.CatchupAttempts)
	require.Equal(t, int64(8), p0.CatchupSuccesses)
	require.Equal(t, int64(2), p0.CatchupFailures)
	require.Equal(t, 75.5, p0.CatchupReputationScore)
	require.Equal(t, int64(1), p0.CatchupMaliciousCount)
	require.Equal(t, int64(150), p0.CatchupAvgResponseTime)
	require.Equal(t, "timeout", p0.LastCatchupError)
}

// TestPeersToResponse_NilBlockHash covers the nil-BlockHash branch where the
// response gets an empty string rather than panicking.
func TestPeersToResponse_NilBlockHash(t *testing.T) {
	pid, err := peer.Decode("12D3KooWNvSZnPi3RrhrTwEY4LuHBeB6K6idsA5DZtEt3yBHk5CQ")
	require.NoError(t, err)

	peers := []*p2p.PeerInfo{
		{ID: pid, BlockHash: nil},
	}

	got := peersToResponse(peers)
	require.Len(t, got.Peers, 1)
	require.Equal(t, "", got.Peers[0].BlockHash)
}

// TestPeersResponse_JSON sanity-checks the wrapper struct's JSON shape.
func TestPeersResponse_JSON(t *testing.T) {
	r := PeersResponse{
		Peers: []PeerInfoResponse{{ID: "x"}},
		Count: 1,
	}
	data, err := json.Marshal(r)
	require.NoError(t, err)
	require.Contains(t, string(data), `"peers":[`)
	require.Contains(t, string(data), `"count":1`)
}
