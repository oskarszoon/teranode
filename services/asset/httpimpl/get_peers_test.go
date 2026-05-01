package httpimpl

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/bsv-blockchain/teranode/services/blockchain"
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
